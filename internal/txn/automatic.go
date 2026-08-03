package txn

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pitr_fs/internal/pg"
)

const (
	defaultHistoryLimit        = 100
	defaultSpaceReservePercent = 20
	foregroundPruneBatch       = 1
)

type SpacePolicy struct {
	MaxBytes         int64
	ReservePercent   int
	RetainedBytes    int64
	ReclaimableBytes int64
}

func (p SpacePolicy) HighWatermarkBytes() int64 {
	if p.MaxBytes <= 0 {
		return 0
	}
	usablePercent := int64(100 - p.ReservePercent)
	return (p.MaxBytes/100)*usablePercent +
		(p.MaxBytes%100)*usablePercent/100
}

type ClearStats struct {
	VersionsDeleted int64
	HistoryDeleted  int64
}

// HistoryLimit 按最长 scope 前缀读取配置。当前控制面只写入根配置，但这里
// 已具备未来目录级“子目录优先、否则继承父目录”的解析语义。
func (m *Manager) HistoryLimit(ctx context.Context, scope string) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	var limit int64
	err = m.db.QueryRow(ctx, `
		SELECT history_limit
		  FROM pitr_config
		 WHERE workspace_id=$2
		   AND (scope_path='/'
		    OR scope_path=$1
		    OR $1 LIKE rtrim(scope_path, '/') || '/%')
		 ORDER BY length(scope_path) DESC
		 LIMIT 1`, normalized, m.workspaceID).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultHistoryLimit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取 history limit(%s): %w", normalized, err)
	}
	return limit, nil
}

func historyLimitTx(
	ctx context.Context,
	tx pg.Tx,
	workspaceID int64,
	scope string,
) (int64, error) {
	var limit int64
	err := tx.QueryRow(ctx, `
		SELECT history_limit
		  FROM pitr_config
		 WHERE workspace_id=$2
		   AND (scope_path='/'
		    OR scope_path=$1
		    OR $1 LIKE rtrim(scope_path, '/') || '/%')
		 ORDER BY length(scope_path) DESC
		 LIMIT 1`, scope, workspaceID).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultHistoryLimit, nil
	}
	return limit, err
}

func (m *Manager) SpacePolicy(ctx context.Context, scope string) (SpacePolicy, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return SpacePolicy{}, err
	}
	var policy SpacePolicy
	err = m.db.QueryRow(ctx, `
		SELECT c.max_space_bytes,c.space_reserve_percent,
		       COALESCE(s.retained_bytes,0),COALESCE(s.reclaimable_bytes,0)
		  FROM LATERAL (
		        SELECT max_space_bytes,space_reserve_percent
		          FROM pitr_config
		         WHERE workspace_id=$2 AND (scope_path='/' OR scope_path=$1
		            OR $1 LIKE rtrim(scope_path,'/') || '/%')
		         ORDER BY length(scope_path) DESC LIMIT 1
		       ) c
		  LEFT JOIN pitr_space_state s ON s.singleton`, normalized, m.workspaceID).Scan(
		&policy.MaxBytes, &policy.ReservePercent,
		&policy.RetainedBytes, &policy.ReclaimableBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		policy.ReservePercent = defaultSpaceReservePercent
		return policy, nil
	}
	if err != nil {
		return SpacePolicy{}, fmt.Errorf("读取空间策略(%s): %w", normalized, err)
	}
	return policy, nil
}

func spacePolicyTx(
	ctx context.Context,
	tx pg.Tx,
	workspaceID int64,
	scope string,
) (SpacePolicy, error) {
	var policy SpacePolicy
	err := tx.QueryRow(ctx, `
		SELECT c.max_space_bytes,c.space_reserve_percent,
		       COALESCE(s.retained_bytes,0),COALESCE(s.reclaimable_bytes,0)
		  FROM LATERAL (
		        SELECT max_space_bytes,space_reserve_percent
		          FROM pitr_config
		         WHERE workspace_id=$2 AND (scope_path='/' OR scope_path=$1
		            OR $1 LIKE rtrim(scope_path,'/') || '/%')
		         ORDER BY length(scope_path) DESC LIMIT 1
		       ) c
		  LEFT JOIN pitr_space_state s ON s.singleton`, scope, workspaceID).Scan(
		&policy.MaxBytes, &policy.ReservePercent,
		&policy.RetainedBytes, &policy.ReclaimableBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		policy.ReservePercent = defaultSpaceReservePercent
		return policy, nil
	}
	return policy, err
}

// SetSpacePolicy 设置用户视角的文件数据额度。maxBytes=0 表示不按空间裁剪；
// reservePercent=20 表示预计保留数据达到额度的 80% 时淘汰最老版本。
func (m *Manager) SetSpacePolicy(
	ctx context.Context,
	scope string,
	maxBytes int64,
	reservePercent int,
) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	if maxBytes < 0 {
		return 0, fmt.Errorf("max space 不能为负数: %d", maxBytes)
	}
	if reservePercent < 1 || reservePercent > 99 {
		return 0, fmt.Errorf("space reserve 必须是 1..99: %d", reservePercent)
	}
	var pruned int64
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pitr_config(
				workspace_id,scope_path,history_limit,max_space_bytes,
				space_reserve_percent,updated_at)
			VALUES ($1,$2,$3,$4,$5,now())
			ON CONFLICT (workspace_id,scope_path) DO UPDATE
			   SET max_space_bytes=EXCLUDED.max_space_bytes,
			       space_reserve_percent=EXCLUDED.space_reserve_percent,
			       updated_at=now()`, m.workspaceID, normalized, defaultHistoryLimit,
			maxBytes, reservePercent); err != nil {
			return err
		}
		limit, err := historyLimitTx(ctx, tx, m.workspaceID, normalized)
		if err != nil {
			return err
		}
		pruned, err = pruneClosedVersions(
			ctx, tx, m.workspaceID, normalized, limit)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("设置空间策略(%s): %w", normalized, err)
	}
	return pruned, nil
}

// SetHistoryLimit 持久化指定 scope 的版本上限并立即裁剪。-1 表示不按
// 版本数量裁剪，但配置的空间水位仍然生效。服务层当前只允许
// scope='/'，保留 scope 参数是为了未来目录级配置不改变存储接口。
func (m *Manager) SetHistoryLimit(
	ctx context.Context,
	scope string,
	limit int64,
) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	if limit != -1 && limit <= 0 {
		return 0, fmt.Errorf("history limit 必须是 -1 或正整数: %d", limit)
	}
	var pruned int64
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pitr_config(workspace_id,scope_path,history_limit,updated_at)
			VALUES ($1,$2,$3,now())
			ON CONFLICT (workspace_id,scope_path) DO UPDATE
			    SET history_limit=EXCLUDED.history_limit,updated_at=now()`,
			m.workspaceID, normalized, limit); err != nil {
			return err
		}
		pruned, err = pruneClosedVersions(
			ctx, tx, m.workspaceID, normalized, limit)
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
		var locked bool
		if err := tx.QueryRow(ctx,
			"SELECT pg_try_advisory_xact_lock(hashtext('pitr-fs:versions'))").
			Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return ErrMaintenanceBusy
		}
		var parentID int64
		if err := tx.QueryRow(ctx, `
			SELECT id
			  FROM pitr_txn
			 WHERE workspace_id=$1 AND (state='root' OR closed_at IS NOT NULL)
			 ORDER BY id DESC
			 LIMIT 1`, m.workspaceID).Scan(&parentID); err != nil {
			return fmt.Errorf("查找自动版本 parent: %w", err)
		}
		for attempt := 0; attempt < 3; attempt++ {
			hash, err := NewVersionHash()
			if err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO pitr_txn
					(workspace_id,version_hash,parent_id,scope_path,state,command,posix_op,
					 process_command,actor_uid,actor_gid,actor_pid,actor_name)
				VALUES ($1,$2,$3,$4,'auto',$5,$6,$7,$8,$9,$10,$11)
				ON CONFLICT (version_hash) DO NOTHING
				RETURNING id`,
				m.workspaceID, hash, parentID, normalized, command,
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
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "uniq_open_auto_window" {
			return 0, fmt.Errorf("%w: 另一 workspace 正在关闭版本窗口",
				ErrMaintenanceBusy)
		}
		return 0, fmt.Errorf("打开自动版本(%s): %w", normalized, err)
	}
	return versionID, nil
}

// UpdateStandaloneVersionScope 只允许把开放自动版本扩展到祖先目录。
// 同一时刻的多个可写 fd 共用唯一版本时，所有变更路径都必须落在 scope 内。
func (m *Manager) UpdateStandaloneVersionScope(
	ctx context.Context,
	versionID int64,
	scope string,
) error {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return err
	}
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return err
		}
		var updated string
		err := tx.QueryRow(ctx, `
			UPDATE pitr_txn
			   SET scope_path=$2
			 WHERE id=$1
			   AND workspace_id=$3
			   AND state='auto'
			   AND closed_at IS NULL
			   AND (scope_path=$2
			        OR scope_path LIKE
			           CASE WHEN $2='/' THEN '/%'
			                ELSE rtrim($2, '/') || '/%' END)
			 RETURNING scope_path`,
			versionID, normalized, m.workspaceID).Scan(&updated)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w:auto %d 不存在、已关闭或 scope 不是祖先",
				ErrIllegalTransit, versionID)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("扩展自动版本 %d scope 到 %s: %w",
			versionID, normalized, err)
	}
	return nil
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
			 WHERE id=$1 AND workspace_id=$4
			   AND state='auto' AND closed_at IS NULL
			 RETURNING scope_path`,
			versionID, posixOp, changeSummary, m.workspaceID).Scan(&scope); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w:auto %d 不存在或已关闭",
					ErrIllegalTransit, versionID)
			}
			return err
		}
		limit, err := historyLimitTx(ctx, tx, m.workspaceID, scope)
		if err != nil {
			return err
		}
		_, err = pruneClosedVersions(ctx, tx, m.workspaceID, scope, limit)
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
	workspaceID int64,
	scope string,
	limit int64,
) (int64, error) {
	result, err := pruneClosedVersionsBatch(
		ctx, tx, workspaceID, scope, limit, foregroundPruneBatch)
	return result.Pruned, err
}

type pruneBatchResult struct {
	Pruned  int64
	Pending bool
}

func pruneClosedVersionsBatch(
	ctx context.Context,
	tx pg.Tx,
	workspaceID int64,
	scope string,
	limit int64,
	batch int64,
) (pruneBatchResult, error) {
	result := pruneBatchResult{}
	if limit != -1 && limit <= 0 {
		return result, fmt.Errorf("history limit 必须是 -1 或正整数: %d", limit)
	}
	if batch <= 0 {
		return result, fmt.Errorf("裁剪批次必须是正整数: %d", batch)
	}
	remaining := batch
	var rootID int64
	if err := tx.QueryRow(ctx,
		"SELECT id FROM pitr_txn WHERE workspace_id=$1 AND state='root' ORDER BY id LIMIT 1",
		workspaceID).Scan(&rootID); err != nil {
		return result, err
	}

	var doomed []int64
	if limit != -1 {
		var count int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pitr_txn
			 WHERE workspace_id=$1 AND state<>'root' AND closed_at IS NOT NULL`,
			workspaceID).Scan(&count); err != nil {
			return result, err
		}
		take := count - limit
		if take > remaining {
			take = remaining
		}
		if take > 0 {
			rows, err := tx.Query(ctx, `
				SELECT id FROM pitr_txn
				 WHERE workspace_id=$1 AND state<>'root' AND closed_at IS NOT NULL
				 ORDER BY id ASC LIMIT $2`, workspaceID, take)
			if err != nil {
				return result, err
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return result, err
				}
				doomed = append(doomed, id)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return result, err
			}
			rows.Close()
		}
	}
	if len(doomed) != 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE pitr_txn SET parent_id=$1 WHERE parent_id=ANY($2)`,
			rootID, doomed); err != nil {
			return result, err
		}
		affected, err := tx.Exec(ctx,
			"DELETE FROM pitr_txn WHERE id=ANY($1)", doomed)
		if err != nil {
			return result, err
		}
		result.Pruned += affected
		remaining -= affected
	}

	policy, err := spacePolicyTx(ctx, tx, workspaceID, scope)
	if err != nil {
		return result, err
	}
	highWatermark := policy.HighWatermarkBytes()
	for policy.MaxBytes != 0 && policy.RetainedBytes > highWatermark && remaining > 0 {
		var oldestID int64
		err := tx.QueryRow(ctx, `
			SELECT id FROM pitr_txn
			 WHERE workspace_id=$1 AND state<>'root' AND closed_at IS NOT NULL
			 ORDER BY id ASC LIMIT 1`, workspaceID).Scan(&oldestID)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return result, err
		}
		if _, err := tx.Exec(ctx,
			"UPDATE pitr_txn SET parent_id=$1 WHERE parent_id=$2",
			rootID, oldestID); err != nil {
			return result, err
		}
		affected, err := tx.Exec(ctx,
			"DELETE FROM pitr_txn WHERE id=$1", oldestID)
		if err != nil {
			return result, err
		}
		result.Pruned += affected
		remaining -= affected
		if err := tx.QueryRow(ctx, `
			SELECT retained_bytes FROM pitr_space_state WHERE singleton`).
			Scan(&policy.RetainedBytes); err != nil {
			return result, err
		}
	}

	if limit != -1 {
		var count int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pitr_txn
			 WHERE workspace_id=$1 AND state<>'root' AND closed_at IS NOT NULL`,
			workspaceID).Scan(&count); err != nil {
			return result, err
		}
		result.Pending = count > limit
	}
	if policy.MaxBytes != 0 && policy.RetainedBytes > highWatermark {
		var hasPrunable bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pitr_txn
				 WHERE workspace_id=$1 AND state<>'root' AND closed_at IS NOT NULL
			)`, workspaceID).Scan(&hasPrunable); err != nil {
			return result, err
		}
		// 当前数据本身超过空间阈值时，删除历史已无法释放更多空间。
		// 这种状态仍由 config/status 暴露，但不能让后台队列永久空转。
		result.Pending = result.Pending || hasPrunable
	}
	if result.Pending {
		_, err = tx.Exec(ctx, `
			INSERT INTO pitr_prune_queue(workspace_id,singleton,requested_at)
			VALUES ($1,true,now())
			ON CONFLICT (workspace_id,singleton) DO UPDATE
			   SET requested_at=EXCLUDED.requested_at`, workspaceID)
	} else {
		_, err = tx.Exec(ctx,
			"DELETE FROM pitr_prune_queue WHERE workspace_id=$1 AND singleton",
			workspaceID)
	}
	return result, err
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
	limit, err := historyLimitTx(ctx, tx, 1, normalized)
	if err != nil {
		return 0, err
	}
	return pruneClosedVersions(ctx, tx, 1, normalized, limit)
}

func PruneClosedVersionsForWorkspace(
	ctx context.Context,
	tx pg.Tx,
	workspaceID int64,
	scope string,
) (int64, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, err
	}
	limit, err := historyLimitTx(ctx, tx, workspaceID, normalized)
	if err != nil {
		return 0, err
	}
	return pruneClosedVersions(ctx, tx, workspaceID, normalized, limit)
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
		limit, err := historyLimitTx(ctx, tx, m.workspaceID, normalized)
		if err != nil {
			return err
		}
		pruned, err = pruneClosedVersions(
			ctx, tx, m.workspaceID, normalized, limit)
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
			 WHERE workspace_id=$1 AND
			       (state='active' OR (state='auto' AND closed_at IS NULL))`,
			m.workspaceID).Scan(&open); err != nil {
			return err
		}
		if open != 0 {
			return fmt.Errorf("%w:仍有 %d 个开放写窗口", ErrIllegalTransit, open)
		}
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM pitr_node_history h JOIN pitr_txn t ON t.id=h.txn_id WHERE t.workspace_id=$1) +
			  (SELECT count(*) FROM pitr_edge_history h JOIN pitr_txn t ON t.id=h.txn_id WHERE t.workspace_id=$1) +
			  (SELECT count(*) FROM pitr_chunk_history h JOIN pitr_txn t ON t.id=h.txn_id WHERE t.workspace_id=$1) +
			  (SELECT count(*) FROM pitr_chunk_ref_history h JOIN pitr_txn t ON t.id=h.txn_id WHERE t.workspace_id=$1)`,
			m.workspaceID).Scan(&stats.HistoryDeleted); err != nil {
			return err
		}
		affected, err := tx.Exec(ctx,
			"DELETE FROM pitr_txn WHERE workspace_id=$1 AND state<>'root'",
			m.workspaceID)
		if err != nil {
			return err
		}
		stats.VersionsDeleted = affected
		_, err = tx.Exec(ctx, `
			UPDATE pitr_txn
			   SET parent_id=NULL,scope_path='/',command='baseline',
			       message='',created_at=now(),closed_at=NULL
			 WHERE workspace_id=$1 AND state='root'`, m.workspaceID)
		return err
	})
	if err != nil {
		return ClearStats{}, fmt.Errorf("clear history: %w", err)
	}
	return stats, nil
}
