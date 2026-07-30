package proxy

import (
	"context"
	"errors"
	"log/slog"
	"syscall"
	"time"

	"pitr_fs/internal/txn"
)

type fdVersion struct {
	autoID   int64
	parentID int64
	long     bool
	bypass   bool
}

func (l *Loopback) loadFD(fd uint64) (fdVersion, bool) {
	l.fdMu.Lock()
	defer l.fdMu.Unlock()
	value, ok := l.fds[fd]
	return value, ok
}

func (l *Loopback) storeFD(fd uint64, value fdVersion) {
	if fd == 0 || (value.autoID == 0 && !value.bypass) {
		return
	}
	l.fdMu.Lock()
	l.fds[fd] = value
	l.fdMu.Unlock()
}

func (l *Loopback) deleteFD(fd uint64) {
	if fd == 0 {
		return
	}
	l.fdMu.Lock()
	delete(l.fds, fd)
	l.fdMu.Unlock()
}

// takeFD 原子转移窗口的终结权。Flush 与 Release 可被 FUSE 并发调度,
// 只有成功 take 的一方允许 CloseAuto/Unlock。
func (l *Loopback) takeFD(fd uint64) (fdVersion, bool) {
	l.fdMu.Lock()
	defer l.fdMu.Unlock()
	value, ok := l.fds[fd]
	if ok {
		delete(l.fds, fd)
	}
	return value, ok
}

// versionedHook 执行退化但确定性的开放窗口方案。window 覆盖全部写类操作,
// 包括没有 active txn 的写,确保任何时刻都不会有 scope 外操作落进另一条
// JuiceFS 连接正在使用的开放 auto。
func (n *Node) versionedHook(
	ctx context.Context,
	absPath, command string,
	fd uint64,
	action func() syscall.Errno,
) syscall.Errno {
	_, errno := n.versionedHookWindow(ctx, absPath, command, fd, action)
	return errno
}

func (n *Node) versionedHookWindow(
	ctx context.Context,
	absPath, command string,
	fd uint64,
	action func() syscall.Errno,
) (fdVersion, syscall.Errno) {
	root := n.root
	// O_RDWR/writable-mmap 句柄在 Open 到 Release 之间持有唯一全局窗口。
	// 同一 fd 的操作已经处在该窗口内,不能再次获取 window mutex。
	if fd != 0 {
		if existing, ok := root.loadFD(fd); ok && (existing.long || existing.bypass) {
			return existing, action()
		}
	}
	if root.isLongPath(absPath) {
		return fdVersion{long: true}, action()
	}
	root.window.Lock()
	defer root.window.Unlock()

	if root.manager == nil {
		return fdVersion{}, action()
	}

	active, err := root.manager.FindActiveByPath(ctx, absPath)
	if err != nil {
		slog.Error("查找 active txn", "path", absPath, "error", err)
		return fdVersion{}, syscall.EIO
	}
	if active == nil {
		root.deleteFD(fd)
		return fdVersion{}, action()
	}

	window, reused, err := root.openWindow(ctx, fd, active.ID, command)
	if err != nil {
		slog.Error("打开 auto 窗口", "path", absPath, "command", command, "error", err)
		return fdVersion{}, syscall.EIO
	}

	errno := action()
	if errno != 0 {
		if abortErr := root.manager.AbortAutoVersion(ctx, window.autoID); abortErr != nil {
			slog.Error("补偿失败的 auto", "auto_id", window.autoID, "error", abortErr)
		}
		if fd != 0 {
			root.deleteFD(fd)
		}
		return fdVersion{}, errno
	}
	if err := root.manager.CloseAutoVersion(ctx, window.autoID); err != nil {
		slog.Error("关闭 auto 窗口", "auto_id", window.autoID, "error", err)
		if abortErr := root.manager.AbortAutoVersion(ctx, window.autoID); abortErr != nil {
			slog.Error("关闭失败后的 auto 补偿", "auto_id", window.autoID, "error", abortErr)
		}
		if fd != 0 {
			root.deleteFD(fd)
		}
		return fdVersion{}, syscall.EIO
	}
	if fd != 0 && !reused {
		root.storeFD(fd, window)
	}
	return window, 0
}

// keepWritableWindow 为可写句柄保留一个跨完整 fd 生命周期的 auto。这样
// 顺序 Write 不再为每个 FUSE request 往返 PostgreSQL;同时覆盖 Linux 可能
// 不把 writable mmap 脏页作为单独 Write 通知的情况。无 active 时记录 fd
// 级负判定,后续写直接旁路;事务必须先 begin 再打开写 fd。
func (n *Node) keepWritableWindow(
	ctx context.Context,
	absPath, command string,
	file *trackedFile,
) syscall.Errno {
	root := n.root
	if root.manager == nil || file == nil || !file.writable {
		return 0
	}
	root.window.Lock()
	active, err := root.manager.FindActiveByPath(ctx, absPath)
	if err != nil {
		root.window.Unlock()
		slog.Error("查找 buffered active txn", "path", absPath, "error", err)
		return syscall.EIO
	}
	if active == nil {
		root.storeFD(file.id, fdVersion{bypass: true})
		root.window.Unlock()
		return 0
	}
	autoID, err := root.manager.OpenAutoVersion(ctx, active.ID, command)
	if err != nil {
		root.window.Unlock()
		slog.Error("打开 buffered auto 窗口", "path", absPath, "error", err)
		return syscall.EIO
	}
	root.storeFD(file.id, fdVersion{
		autoID: autoID, parentID: active.ID, long: true,
	})
	root.setLongPath(absPath)
	return 0
}

func (n *Node) closeWritableWindow(
	ctx context.Context,
	file *trackedFile,
) syscall.Errno {
	if file == nil {
		return 0
	}
	window, ok := n.root.takeFD(file.id)
	if !ok {
		return file.LoopbackFile.Release(ctx)
	}
	if window.bypass {
		return file.LoopbackFile.Release(ctx)
	}

	releaseErrno := file.LoopbackFile.Release(ctx)
	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	closeErr := n.root.manager.CloseAutoVersion(finalCtx, window.autoID)
	cancel()
	n.root.setLongPath("")
	n.root.window.Unlock()
	if closeErr != nil {
		slog.Error("关闭 writable auto 窗口",
			"auto_id", window.autoID, "error", closeErr)
		return syscall.EIO
	}
	return releaseErrno
}

func (l *Loopback) openWindow(
	ctx context.Context,
	fd uint64,
	parentID int64,
	command string,
) (fdVersion, bool, error) {
	if fd != 0 {
		if existing, ok := l.loadFD(fd); ok {
			if existing.parentID == parentID {
				if err := l.manager.ReopenAutoVersion(ctx, existing.autoID, parentID); err == nil {
					return existing, true, nil
				} else if !errors.Is(err, txn.ErrIllegalTransit) {
					return fdVersion{}, false, err
				}
			}
			l.deleteFD(fd)
		}
	}
	autoID, err := l.manager.OpenAutoVersion(ctx, parentID, command)
	if err != nil {
		return fdVersion{}, false, err
	}
	return fdVersion{autoID: autoID, parentID: parentID}, false, nil
}
