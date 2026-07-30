package txn

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type DiffStats struct {
	NodeChanges  int64
	EdgeChanges  int64
	ChunkChanges int64
}

// Diff 统计两个版本之间被触及的元数据 key 数。相同 key 在多个中间版本中
// 只计一次;chunk_ref 是 chunk 内部引用细节,合并到 ChunkChanges。
func (m *Manager) Diff(
	ctx context.Context,
	versionA, versionB, scope string,
) (DiffStats, error) {
	var idA, idB int64
	if err := m.db.QueryRow(ctx,
		"SELECT id FROM pitr_txn WHERE version_hash=$1", versionA).Scan(&idA); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DiffStats{}, ErrTxnNotFound
		}
		return DiffStats{}, fmt.Errorf("查找 version_a: %w", err)
	}
	if err := m.db.QueryRow(ctx,
		"SELECT id FROM pitr_txn WHERE version_hash=$1", versionB).Scan(&idB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DiffStats{}, ErrTxnNotFound
		}
		return DiffStats{}, fmt.Errorf("查找 version_b: %w", err)
	}
	if idA > idB {
		idA, idB = idB, idA
	}
	var scopeArg any
	if scope != "" {
		normalized, err := NormalizeScope(scope)
		if err != nil {
			return DiffStats{}, err
		}
		scopeArg = normalized
	}

	var stats DiffStats
	if err := m.db.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM (
		     SELECT DISTINCT h.inode
		       FROM pitr_node_history h JOIN pitr_txn t ON t.id=h.txn_id
		      WHERE t.id>$1 AND t.id<=$2
		        AND ($3::text IS NULL OR pitr_scopes_overlap(t.scope_path,$3))
		  ) n),
		  (SELECT count(*) FROM (
		     SELECT DISTINCT h.parent,h.name
		       FROM pitr_edge_history h JOIN pitr_txn t ON t.id=h.txn_id
		      WHERE t.id>$1 AND t.id<=$2
		        AND ($3::text IS NULL OR pitr_scopes_overlap(t.scope_path,$3))
		  ) e),
		  (SELECT
		     (SELECT count(*) FROM (
		        SELECT DISTINCT h.inode,h.indx
		          FROM pitr_chunk_history h JOIN pitr_txn t ON t.id=h.txn_id
		         WHERE t.id>$1 AND t.id<=$2
		           AND ($3::text IS NULL OR pitr_scopes_overlap(t.scope_path,$3))
		     ) c)
		     +
		     (SELECT count(*) FROM (
		        SELECT DISTINCT h.chunkid
		          FROM pitr_chunk_ref_history h JOIN pitr_txn t ON t.id=h.txn_id
		         WHERE t.id>$1 AND t.id<=$2
		           AND ($3::text IS NULL OR pitr_scopes_overlap(t.scope_path,$3))
		     ) r)
		  )`,
		idA, idB, scopeArg).
		Scan(&stats.NodeChanges, &stats.EdgeChanges, &stats.ChunkChanges); err != nil {
		return DiffStats{}, fmt.Errorf("diff %s..%s: %w", versionA, versionB, err)
	}
	return stats, nil
}
