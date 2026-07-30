package txn

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"pitr_fs/internal/pg"
)

const defaultHistoryLimit = 100

type ClearStats struct {
	VersionsDeleted int64
	HistoryDeleted  int64
}

// HistoryLimit 按最长 scope 前缀读取配置。当前控制面只写入根配置，但这里
// 已具备未来目录级“子目录优先、否则继承父目录”的解析语义。
func (m *Manager) HistoryLimit(ctx context.Context, scope string) (int, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	var limit int
	err = m.db.QueryRow(ctx, `
		SELECT history_limit
		  FROM pitr_config
		 WHERE scope_path='/'
		    OR scope_path=$1
		    OR $1 LIKE rtrim(scope_path, '/') || '/%'
		 ORDER BY length(scope_path) DESC
		 LIMIT 1`, normalized).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultHistoryLimit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取 history limit(%s): %w", normalized, err)
	}
	return limit, nil
}

func historyLimitTx(ctx context.Context, tx pg.Tx, scope string) (int, error) {
	var limit int
	err := tx.QueryRow(ctx, `
		SELECT history_limit
		  FROM pitr_config
		 WHERE scope_path='/'
		    OR scope_path=$1
		    OR $1 LIKE rtrim(scope_path, '/') || '/%'
		 ORDER BY length(scope_path) DESC
		 LIMIT 1`, scope).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultHistoryLimit, nil
	}
	return limit, err
}

// SetHistoryLimit 持久化指定 scope 的版本上限并立即裁剪。服务层当前只允许
// scope='/'，保留 scope 参数是为了未来目录级配置不改变存储接口。
func (m *Manager) SetHistoryLimit(
	ctx context.Context,
	scope string,
	limit int,
) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, fmt.Errorf("history limit 必须大于 0: %d", limit)
	}
	var pruned int64
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pitr_config(scope_path,history_limit,updated_at)
			VALUES ($1,$2,now())
			ON CONFLICT (scope_path) DO UPDATE
			    SET history_limit=EXCLUDED.history_limit,updated_at=now()`,
			normalized, limit); err != nil {
			return err
		}
		pruned, err = pruneClosedVersions(ctx, tx, normalized, limit)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("设置 history limit(%s=%d): %w",
			normalized, limit, err)
	}
	return pruned, nil
}

// OpenStandaloneVersion 为一次自动写操作创建一等版本。parent 指向当前最新
// 已关闭版本；开放窗口期间 trigger 通过唯一 auto 行捕获 JuiceFS 变更。
func (m *Manager) OpenStandaloneVersion(
	ctx context.Context,
	scope string,
	command string,
	metadata VersionMetadata,
) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	var versionID int64
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		var parentID int64
		if err := tx.QueryRow(ctx, `
			SELECT id
			  FROM pitr_txn
			 WHERE state='root' OR closed_at IS NOT NULL
			 ORDER BY id DESC
			 LIMIT 1`).Scan(&parentID); err != nil {
			return fmt.Errorf("查找自动版本 parent: %w", err)
		}
		for attempt := 0; attempt < 3; attempt++ {
			hash, err := NewVersionHash()
			if err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO pitr_txn
					(version_hash,parent_id,scope_path,state,command,posix_op,
					 process_command,actor_uid,actor_gid,actor_pid,actor_name)
				VALUES ($1,$2,$3,'auto',$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (version_hash) DO NOTHING
				RETURNING id`,
				hash, parentID, normalized, command,
				metadata.PosixOp, metadata.ProcessCommand,
				metadata.ActorUID, metadata.ActorGID, metadata.ActorPID,
				metadata.ActorName).Scan(&versionID)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return errors.New("生成唯一自动 version hash 失败:连续冲突 3 次")
	})
	if err != nil {
		return 0, fmt.Errorf("打开自动版本(%s): %w", normalized, err)
	}
	return versionID, nil
}

func (m *Manager) CloseStandaloneVersion(
	ctx context.Context,
	versionID int64,
	posixOp string,
	changeSummary string,
) error {
	err := m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		var scope string
		if err := tx.QueryRow(ctx, `
			UPDATE pitr_txn
			   SET closed_at=now(),
			       posix_op=COALESCE(NULLIF($2,''),posix_op),
			       change_summary=NULLIF($3,'')
			 WHERE id=$1 AND state='auto' AND closed_at IS NULL
			 RETURNING scope_path`,
			versionID, posixOp, changeSummary).Scan(&scope); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w:auto %d 不存在或已关闭",
					ErrIllegalTransit, versionID)
			}
			return err
		}
		limit, err := historyLimitTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		_, err = pruneClosedVersions(ctx, tx, scope, limit)
		return err
	})
	if err != nil {
		return fmt.Errorf("关闭自动版本 %d: %w", versionID, err)
	}
	return nil
}

// pruneClosedVersions 当前按全卷时间线裁剪。scope 用于解析有效配置；未来
// 开放目录级配置时可在此将候选集限定为继承该配置的 scope。
func pruneClosedVersions(
	ctx context.Context,
	tx pg.Tx,
	scope string,
	limit int,
) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT id
		  FROM pitr_txn
		 WHERE state<>'root' AND closed_at IS NOT NULL
		 ORDER BY id DESC
		 OFFSET $1`, limit)
	if err != nil {
		return 0, err
	}
	var doomed []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		doomed = append(doomed, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(doomed) == 0 {
		return 0, nil
	}
	var rootID int64
	if err := tx.QueryRow(ctx,
		"SELECT id FROM pitr_txn WHERE state='root' ORDER BY id LIMIT 1").
		Scan(&rootID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pitr_txn SET parent_id=$1 WHERE parent_id=ANY($2)`,
		rootID, doomed); err != nil {
		return 0, err
	}
	affected, err := tx.Exec(ctx,
		"DELETE FROM pitr_txn WHERE id=ANY($1)", doomed)
	if err != nil {
		return 0, err
	}
	return affected, nil
}

func PruneClosedVersions(
	ctx context.Context,
	tx pg.Tx,
	scope string,
) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	limit, err := historyLimitTx(ctx, tx, normalized)
	if err != nil {
		return 0, err
	}
	return pruneClosedVersions(ctx, tx, normalized, limit)
}

func (m *Manager) Prune(ctx context.Context, scope string) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	var pruned int64
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		limit, err := historyLimitTx(ctx, tx, normalized)
		if err != nil {
			return err
		}
		pruned, err = pruneClosedVersions(ctx, tx, normalized, limit)
		return err
	})
	return pruned, err
}

// ClearHistory 保留当前 JuiceFS 数据并把它作为新 root 基线，只删除版本、
// shadow history 和 blob retention 记录。调用方当前只允许传 '/'。
func (m *Manager) ClearHistory(ctx context.Context, scope string) (ClearStats, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return ClearStats{}, err
	}
	if normalized != "/" {
		return ClearStats{}, errors.New("当前仅支持全局 clear")
	}
	var stats ClearStats
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		var open int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pitr_txn
			 WHERE state='active' OR (state='auto' AND closed_at IS NULL)`).
			Scan(&open); err != nil {
			return err
		}
		if open != 0 {
			return fmt.Errorf("%w:仍有 %d 个开放写窗口", ErrIllegalTransit, open)
		}
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM pitr_node_history) +
			  (SELECT count(*) FROM pitr_edge_history) +
			  (SELECT count(*) FROM pitr_chunk_history) +
			  (SELECT count(*) FROM pitr_chunk_ref_history)`).
			Scan(&stats.HistoryDeleted); err != nil {
			return err
		}
		affected, err := tx.Exec(ctx, "DELETE FROM pitr_txn WHERE state<>'root'")
		if err != nil {
			return err
		}
		stats.VersionsDeleted = affected
		if _, err := tx.Exec(ctx, "TRUNCATE pitr_blob_retention"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE pitr_txn
			   SET parent_id=NULL,scope_path='/',command='baseline',
			       message='',created_at=now(),closed_at=NULL
			 WHERE state='root'`)
		return err
	})
	if err != nil {
		return ClearStats{}, fmt.Errorf("clear history: %w", err)
	}
	return stats, nil
}
