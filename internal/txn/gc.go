package txn

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrMaintenanceBusy = errors.New("存在开放写窗口，维护任务延期")

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
		_, err = m.db.Exec(ctx,
			"DELETE FROM pitr_gc_queue WHERE singleton")
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
	_, updateErr := m.db.Exec(context.WithoutCancel(ctx), `
		UPDATE pitr_gc_queue
		   SET attempts=attempts+1,last_error=$1
		 WHERE singleton`, strings.TrimSpace(message))
	if updateErr != nil {
		return false, errors.Join(err,
			fmt.Errorf("记录 GC 失败: %w", updateErr))
	}
	return false, err
}
