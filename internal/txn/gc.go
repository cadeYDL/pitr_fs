package txn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrMaintenanceBusy = errors.New("存在开放写窗口，维护任务延期")

const queueErrorRecordTimeout = 5 * time.Second

func queueErrorContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), queueErrorRecordTimeout)
}

// RunPendingGC 把多次版本淘汰合并成一次外部 GC。run 必须执行 JuiceFS
// 原生 GC；维护锁阻止新版本开始，有已开放窗口时立即延期，避免阻塞 close。
func (m *Manager) RunPendingGC(
	ctx context.Context,
	run func(context.Context) error,
) (bool, error) {
	var pending bool
	if err := m.db.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pitr_gc_queue WHERE singleton)").
		Scan(&pending); err != nil {
		return false, fmt.Errorf("读取 GC 队列: %w", err)
	}
	if !pending {
		return false, nil
	}
	err := m.db.WithAdvisoryLock(ctx, "pitr-fs:versions", func() error {
		open, err := m.CountOpenWrites(ctx)
		if err != nil {
			return err
		}
		if open != 0 {
			return ErrMaintenanceBusy
		}
		if err := run(ctx); err != nil {
			return err
		}
		// 原生 GC 成功后，删除已释放版本留下的合成 delslices 索引，并与
		// 队列确认合成一个 SQL 原子动作。仍有 pitr_slice_pin 的远期记录
		// 不匹配 deleted 条件，也通过 NOT EXISTS 双重保护。
		_, err = m.db.Exec(ctx, `
			WITH cleaned AS (
			    DELETE FROM jfs_delslices d
			     WHERE d.chunkid>=8000000000000000000
			       AND d.chunkid<=9000000000000000000
			       AND d.deleted<253402300799
			       AND NOT EXISTS (
			           SELECT 1 FROM pitr_slice_pin p
			            WHERE p.delayed_id=d.chunkid)
			)
			DELETE FROM pitr_gc_queue WHERE singleton`)
		return err
	})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrMaintenanceBusy) {
		return false, err
	}
	message := err.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	recordCtx, cancel := queueErrorContext(ctx)
	defer cancel()
	_, updateErr := m.db.Exec(recordCtx, `
		UPDATE pitr_gc_queue
		   SET attempts=attempts+1,last_error=$1
		 WHERE singleton`, strings.TrimSpace(message))
	if updateErr != nil {
		return false, errors.Join(err,
			fmt.Errorf("记录 GC 失败: %w", updateErr))
	}
	return false, err
}
