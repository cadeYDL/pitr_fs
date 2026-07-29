package proxy

import (
	"context"
	"errors"
	"log/slog"
	"syscall"

	"pitr_fs/internal/txn"
)

type fdVersion struct {
	autoID   int64
	parentID int64
}

func (l *Loopback) loadFD(fd uint64) (fdVersion, bool) {
	l.fdMu.Lock()
	defer l.fdMu.Unlock()
	value, ok := l.fds[fd]
	return value, ok
}

func (l *Loopback) storeFD(fd uint64, value fdVersion) {
	if fd == 0 || value.autoID == 0 {
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
