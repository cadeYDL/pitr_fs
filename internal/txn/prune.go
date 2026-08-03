package txn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"pitr_fs/internal/pg"
)

// PendingPruneWorkspaceIDs 返回当前有持久化裁剪任务的 workspace。daemon
// 使用它轮询全部版本线，避免只处理 default workspace。
func (m *Manager) PendingPruneWorkspaceIDs(ctx context.Context) ([]int64, error) {
	rows, err := m.db.Query(ctx, `
		SELECT workspace_id FROM pitr_prune_queue
		 WHERE singleton ORDER BY requested_at,workspace_id`)
	if err != nil {
		return nil, fmt.Errorf("列出 workspace 裁剪队列: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取 workspace 裁剪队列: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 workspace 裁剪队列: %w", err)
	}
	return ids, nil
}

// RunPendingPrune 处理一个有上限的版本裁剪批次。队列只保存“需要重算”这一
// 事实，所以 daemon 重启后会按当时的持久化配置继续，而不会执行过期计划。
func (m *Manager) RunPendingPrune(
	ctx context.Context,
	scope string,
	batch int64,
) (int64, bool, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return 0, false, err
	}
	if batch <= 0 {
		return 0, false, fmt.Errorf("裁剪批次必须是正整数: %d", batch)
	}
	var queued bool
	if err := m.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pitr_prune_queue
		 WHERE workspace_id=$1 AND singleton)`, m.workspaceID).
		Scan(&queued); err != nil {
		return 0, false, fmt.Errorf("读取裁剪队列: %w", err)
	}
	if !queued {
		return 0, false, nil
	}

	var result pruneBatchResult
	err = m.db.InTx(ctx, func(tx pg.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `
			SELECT pg_try_advisory_xact_lock(hashtext('pitr-fs:versions'))`).
			Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return ErrMaintenanceBusy
		}
		var open int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pitr_txn
			 WHERE state='active' OR (state='auto' AND closed_at IS NULL)`).
			Scan(&open); err != nil {
			return err
		}
		if open != 0 {
			return ErrMaintenanceBusy
		}
		limit, err := historyLimitTx(ctx, tx, m.workspaceID, normalized)
		if err != nil {
			return err
		}
		result, err = pruneClosedVersionsBatch(
			ctx, tx, m.workspaceID, normalized, limit, batch)
		return err
	})
	if err == nil {
		return result.Pruned, result.Pending, nil
	}
	if errors.Is(err, ErrMaintenanceBusy) {
		return result.Pruned, true, err
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 2000 {
		message = message[:2000]
	}
	recordCtx, cancel := queueErrorContext(ctx)
	defer cancel()
	_, updateErr := m.db.Exec(recordCtx, `
		UPDATE pitr_prune_queue
		   SET attempts=attempts+1,last_error=$1
		 WHERE workspace_id=$2 AND singleton`, message, m.workspaceID)
	if updateErr != nil {
		return result.Pruned, true, errors.Join(err,
			fmt.Errorf("记录裁剪失败: %w", updateErr))
	}
	return result.Pruned, true, err
}
