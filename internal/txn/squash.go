package txn

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"pitr_fs/internal/pg"
)

// SquashOptions 描述一个闭区间末端压缩。BaseHash 是压缩前保留的基线，
// EndHash 是压缩后保留的版本；真正被合并的是 (base,end]。
type SquashOptions struct {
	BaseHash  string
	EndHash   string
	Message   string
	DryRun    bool
	ActorUID  int64
	ActorGID  int64
	ActorPID  int64
	ActorName string
}

// SquashStats 同时用于 dry-run 预览和实际执行结果。
type SquashStats struct {
	BaseVersionHash  string
	EndVersionHash   string
	VersionsMerged   int64
	VersionsDeleted  int64
	HistoryBefore    int64
	HistoryAfter     int64
	HistoryDeleted   int64
	FirstOperationAt time.Time
	EndClosedAt      time.Time
	DryRun           bool
	Transaction      *Txn
}

type squashLineageItem struct {
	ID          int64
	VersionHash string
	ParentID    *int64
	ScopePath   string
	State       string
	CreatedAt   time.Time
	ClosedAt    *time.Time
	Depth       int64
}

// Squash 将 (base,end] 的版本压缩为仍使用 end hash 的一个复合版本。
// 全部元数据改写、版本删除、slice pin 释放和重新 pin 都在同一个 PostgreSQL
// 事务内完成；任一步失败都会完整保留原版本链。
func (m *Manager) Squash(
	ctx context.Context,
	options SquashOptions,
) (SquashStats, error) {
	options.BaseHash = strings.TrimSpace(options.BaseHash)
	options.EndHash = strings.TrimSpace(options.EndHash)
	options.Message = strings.TrimSpace(options.Message)
	if options.BaseHash == "" || options.EndHash == "" || options.Message == "" {
		return SquashStats{}, fmt.Errorf(
			"%w: base、end 和 message 均不能为空", ErrInvalidSquashRange)
	}
	if options.BaseHash == options.EndHash {
		return SquashStats{}, fmt.Errorf(
			"%w: base 与 end 不能相同", ErrInvalidSquashRange)
	}

	stats := SquashStats{
		BaseVersionHash: options.BaseHash,
		EndVersionHash:  options.EndHash,
		DryRun:          options.DryRun,
	}
	err := m.db.InTx(ctx, func(tx pg.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); err != nil {
			return fmt.Errorf("锁定版本时间线: %w", err)
		}
		var open int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pitr_txn
			 WHERE workspace_id=$1
			   AND (state='active' OR (state='auto' AND closed_at IS NULL))`,
			m.workspaceID).Scan(&open); err != nil {
			return fmt.Errorf("检查开放写操作: %w", err)
		}
		if open != 0 {
			return fmt.Errorf("%w: %d", ErrOpenWrites, open)
		}

		lineage, err := loadSquashLineage(ctx, tx, m.workspaceID,
			options.BaseHash, options.EndHash)
		if err != nil {
			return err
		}
		base := lineage[0]
		rangeItems := lineage[1:]
		end := rangeItems[len(rangeItems)-1]
		if end.State == StateRoot || end.ClosedAt == nil {
			return fmt.Errorf("%w: end 必须是已关闭版本", ErrInvalidSquashRange)
		}
		for _, item := range rangeItems {
			if item.State == StateRoot || item.ClosedAt == nil {
				return fmt.Errorf("%w: 版本 %s 尚未关闭",
					ErrInvalidSquashRange, item.VersionHash)
			}
		}
		ids := make([]int64, 0, len(rangeItems))
		scopes := make([]string, 0, len(rangeItems))
		for _, item := range rangeItems {
			ids = append(ids, item.ID)
			scopes = append(scopes, item.ScopePath)
		}
		if err := ensureSquashLinear(ctx, tx, ids, end.ID); err != nil {
			return err
		}

		stats.VersionsMerged = int64(len(rangeItems))
		stats.VersionsDeleted = int64(len(rangeItems) - 1)
		stats.FirstOperationAt = rangeItems[0].CreatedAt
		stats.EndClosedAt = *end.ClosedAt
		stats.HistoryBefore, err = squashHistoryCount(ctx, tx, ids)
		if err != nil {
			return err
		}
		stats.HistoryAfter, err = squashHistoryUniqueCount(ctx, tx, ids)
		if err != nil {
			return err
		}
		stats.HistoryDeleted = stats.HistoryBefore - stats.HistoryAfter
		if options.DryRun {
			return nil
		}

		if err := materializeSquashState(ctx, tx, ids, end.ID); err != nil {
			return err
		}
		if err := replaceSquashRange(ctx, tx, base.ID, end.ID,
			commonScope(scopes), stats.FirstOperationAt, stats.EndClosedAt,
			options); err != nil {
			return err
		}
		stats.Transaction, err = scanTxn(tx.QueryRow(ctx,
			"SELECT "+txnColumns+" FROM pitr_txn WHERE id=$1 AND workspace_id=$2",
			end.ID, m.workspaceID))
		return err
	})
	if err != nil {
		return SquashStats{}, fmt.Errorf("squash %s..%s: %w",
			options.BaseHash, options.EndHash, err)
	}
	return stats, nil
}

func loadSquashLineage(
	ctx context.Context,
	tx pg.Tx,
	workspaceID int64,
	baseHash, endHash string,
) ([]squashLineageItem, error) {
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE lineage AS (
			SELECT id,trim(version_hash) AS version_hash,parent_id,scope_path,
			       state,created_at,closed_at,0::bigint AS depth
			  FROM pitr_txn WHERE version_hash=$1 AND workspace_id=$3
			UNION ALL
			SELECT p.id,trim(p.version_hash),p.parent_id,p.scope_path,
			       p.state,p.created_at,p.closed_at,l.depth+1
			  FROM pitr_txn p JOIN lineage l ON p.id=l.parent_id
			 WHERE l.version_hash<>$2 AND l.parent_id IS NOT NULL
			   AND p.workspace_id=$3
		)
		SELECT id,version_hash,parent_id,scope_path,state,created_at,closed_at,depth
		  FROM lineage ORDER BY depth DESC`, endHash, baseHash, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("读取版本链: %w", err)
	}
	defer rows.Close()
	var lineage []squashLineageItem
	for rows.Next() {
		var item squashLineageItem
		if err := rows.Scan(&item.ID, &item.VersionHash, &item.ParentID,
			&item.ScopePath, &item.State, &item.CreatedAt,
			&item.ClosedAt, &item.Depth); err != nil {
			return nil, fmt.Errorf("读取版本链行: %w", err)
		}
		lineage = append(lineage, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取版本链: %w", err)
	}
	if len(lineage) == 0 {
		return nil, ErrTxnNotFound
	}
	if lineage[0].VersionHash != baseHash || len(lineage) < 2 {
		return nil, fmt.Errorf("%w: %s 不是 %s 的祖先",
			ErrInvalidSquashRange, baseHash, endHash)
	}
	return lineage, nil
}

func ensureSquashLinear(ctx context.Context, tx pg.Tx, ids []int64, endID int64) error {
	var branches int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM pitr_txn child
		 WHERE child.parent_id=ANY($1::bigint[])
		   AND NOT (child.id=ANY($1::bigint[]))
		   AND child.parent_id<>$2`, ids, endID).Scan(&branches); err != nil {
		return fmt.Errorf("检查版本分支: %w", err)
	}
	if branches != 0 {
		return fmt.Errorf("%w: 中间版本存在 %d 个分支",
			ErrSquashNonLinear, branches)
	}
	return nil
}

func squashHistoryCount(ctx context.Context, tx pg.Tx, ids []int64) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pitr_node_history WHERE txn_id=ANY($1))+
		       (SELECT count(*) FROM pitr_edge_history WHERE txn_id=ANY($1))+
		       (SELECT count(*) FROM pitr_chunk_history WHERE txn_id=ANY($1))+
		       (SELECT count(*) FROM pitr_chunk_ref_history WHERE txn_id=ANY($1))`, ids).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("统计 squash 历史: %w", err)
	}
	return count, nil
}

func squashHistoryUniqueCount(ctx context.Context, tx pg.Tx, ids []int64) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `
		SELECT (SELECT count(DISTINCT inode) FROM pitr_node_history WHERE txn_id=ANY($1))+
		       (SELECT count(*) FROM (SELECT DISTINCT parent,name FROM pitr_edge_history WHERE txn_id=ANY($1)) e)+
		       (SELECT count(*) FROM (SELECT DISTINCT inode,indx FROM pitr_chunk_history WHERE txn_id=ANY($1)) c)+
		       (SELECT count(DISTINCT chunkid) FROM pitr_chunk_ref_history WHERE txn_id=ANY($1))`, ids).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("统计 squash 唯一历史: %w", err)
	}
	return count, nil
}

func materializeSquashState(ctx context.Context, tx pg.Tx, ids []int64, endID int64) error {
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE pitr_squash_range ON COMMIT DROP AS
		SELECT id,ord FROM unnest($1::bigint[]) WITH ORDINALITY r(id,ord)`, ids); err != nil {
		return fmt.Errorf("准备 squash 范围: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE pitr_squash_end ON COMMIT DROP AS
		SELECT * FROM pitr_txn WHERE id=$1`, endID); err != nil {
		return fmt.Errorf("准备 squash end: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE pitr_squash_children ON COMMIT DROP AS
		SELECT id FROM pitr_txn
		 WHERE parent_id=$2 AND NOT (id=ANY($1::bigint[]))`, ids, endID); err != nil {
		return fmt.Errorf("准备 squash 后继: %w", err)
	}
	statements := []string{
		`CREATE TEMP TABLE pitr_squash_node ON COMMIT DROP AS
		 SELECT DISTINCT ON (h.inode) h.inode,h.op,h.snapshot,h.recorded_at
		   FROM pitr_node_history h
		   JOIN unnest($1::bigint[]) WITH ORDINALITY r(id,ord) ON r.id=h.txn_id
		  ORDER BY h.inode,r.ord,h.recorded_at`,
		`CREATE TEMP TABLE pitr_squash_edge ON COMMIT DROP AS
		 SELECT DISTINCT ON (h.parent,h.name) h.parent,h.name,h.op,h.snapshot,h.recorded_at
		   FROM pitr_edge_history h
		   JOIN unnest($1::bigint[]) WITH ORDINALITY r(id,ord) ON r.id=h.txn_id
		  ORDER BY h.parent,h.name,r.ord,h.recorded_at`,
		`CREATE TEMP TABLE pitr_squash_chunk ON COMMIT DROP AS
		 SELECT DISTINCT ON (h.inode,h.indx) h.inode,h.indx,h.op,h.snapshot,h.recorded_at
		   FROM pitr_chunk_history h
		   JOIN unnest($1::bigint[]) WITH ORDINALITY r(id,ord) ON r.id=h.txn_id
		  ORDER BY h.inode,h.indx,r.ord,h.recorded_at`,
		`CREATE TEMP TABLE pitr_squash_chunk_ref ON COMMIT DROP AS
		 SELECT DISTINCT ON (h.chunkid) h.chunkid,h.op,h.snapshot,h.recorded_at
		   FROM pitr_chunk_ref_history h
		   JOIN unnest($1::bigint[]) WITH ORDINALITY r(id,ord) ON r.id=h.txn_id
		  ORDER BY h.chunkid,r.ord,h.recorded_at`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, ids); err != nil {
			return fmt.Errorf("准备 squash 状态: %w", err)
		}
	}
	return nil
}

func replaceSquashRange(
	ctx context.Context,
	tx pg.Tx,
	baseID, endID int64,
	scope string,
	firstOperationAt, endClosedAt time.Time,
	options SquashOptions,
) error {
	var releaseID int64
	if err := tx.QueryRow(ctx,
		"SELECT nextval('pitr_gc_bundle_id_seq')").Scan(&releaseID); err != nil {
		return fmt.Errorf("分配 squash 释放索引: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jfs_delslices
		   SET chunkid=$2
		 WHERE chunkid=(SELECT delayed_id FROM pitr_slice_pin WHERE txn_id=$1)`,
		endID, releaseID); err != nil {
		return fmt.Errorf("迁移 end slice 释放索引: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pitr_slice_pin SET delayed_id=$2 WHERE txn_id=$1`,
		endID, releaseID); err != nil {
		return fmt.Errorf("迁移 end slice pin: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pitr_txn SET parent_id=$1
		 WHERE id IN (SELECT id FROM pitr_squash_children)`, baseID); err != nil {
		return fmt.Errorf("暂存后继版本: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM pitr_txn
		 WHERE id IN (SELECT id FROM pitr_squash_range)`); err != nil {
		return fmt.Errorf("删除 squash 版本范围: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pitr_txn(
			id,workspace_id,version_hash,parent_id,scope_path,state,command,message,
			posix_op,process_command,actor_uid,actor_gid,actor_pid,actor_name,
			change_summary,created_at,closed_at
		)
		SELECT id,workspace_id,version_hash,$1,$2,'committed','squash',$3,
		       'squash','',$4,$5,$6,$7,$3,$8,$9
		  FROM pitr_squash_end`, baseID, scope, options.Message,
		options.ActorUID, options.ActorGID, options.ActorPID, options.ActorName,
		firstOperationAt, endClosedAt); err != nil {
		return fmt.Errorf("重建 squash end: %w", err)
	}
	historyStatements := []string{
		`INSERT INTO pitr_node_history(txn_id,inode,op,snapshot,recorded_at)
		 SELECT $1,inode,op,snapshot,recorded_at FROM pitr_squash_node`,
		`INSERT INTO pitr_edge_history(txn_id,parent,name,op,snapshot,recorded_at)
		 SELECT $1,parent,name,op,snapshot,recorded_at FROM pitr_squash_edge`,
		`INSERT INTO pitr_chunk_history(txn_id,inode,indx,op,snapshot,recorded_at)
		 SELECT $1,inode,indx,op,snapshot,recorded_at FROM pitr_squash_chunk`,
		`INSERT INTO pitr_chunk_ref_history(txn_id,chunkid,op,snapshot,recorded_at)
		 SELECT $1,chunkid,op,snapshot,recorded_at FROM pitr_squash_chunk_ref`,
	}
	for _, statement := range historyStatements {
		if _, err := tx.Exec(ctx, statement, endID); err != nil {
			return fmt.Errorf("重建 squash 历史: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		SELECT pitr_pin_chunk_slices($1,(jsonb_populate_record(NULL::jfs_chunk,snapshot)).slices)
		  FROM pitr_squash_chunk WHERE snapshot IS NOT NULL
		 ORDER BY inode,indx`, endID); err != nil {
		return fmt.Errorf("重建 squash slice pin: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pitr_txn SET parent_id=$1
		 WHERE id IN (SELECT id FROM pitr_squash_children)`, endID); err != nil {
		return fmt.Errorf("恢复 squash 后继版本: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pitr_slice_index_state(
			singleton,indexed_at,indexed_through_txn_id,last_version_cleanup_at
		) VALUES (true,clock_timestamp(),$1,clock_timestamp())
		ON CONFLICT (singleton) DO UPDATE
		   SET indexed_at=EXCLUDED.indexed_at,
		       indexed_through_txn_id=GREATEST(
		         pitr_slice_index_state.indexed_through_txn_id,EXCLUDED.indexed_through_txn_id),
		       last_version_cleanup_at=EXCLUDED.last_version_cleanup_at`, endID); err != nil {
		return fmt.Errorf("更新 squash slice 索引状态: %w", err)
	}
	return nil
}

func commonScope(scopes []string) string {
	if len(scopes) == 0 {
		return "/"
	}
	common := path.Clean(scopes[0])
	for _, candidate := range scopes[1:] {
		candidate = path.Clean(candidate)
		for common != "/" && candidate != common &&
			!strings.HasPrefix(candidate, strings.TrimSuffix(common, "/")+"/") {
			common = path.Dir(common)
		}
	}
	return common
}
