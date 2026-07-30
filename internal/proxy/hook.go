package proxy

import (
	"context"
	"log/slog"
	"syscall"
	"time"
)

type fdVersion struct {
	autoID int64
	long   bool
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

// versionedHook 为每个元数据写操作创建独立自动版本。window 覆盖全部写类
// 操作，确保 JuiceFS 独立连接的 trigger 能把变化归属到唯一开放版本。
func (n *Node) versionedHook(
	ctx context.Context,
	absPath, command, posixOp string,
	fd uint64,
	action func() syscall.Errno,
) syscall.Errno {
	_, errno := n.versionedHookWindow(
		ctx, absPath, command, posixOp, fd, action)
	return errno
}

func (n *Node) versionedHookWindow(
	ctx context.Context,
	absPath, command, posixOp string,
	fd uint64,
	action func() syscall.Errno,
) (fdVersion, syscall.Errno) {
	root := n.root
	// O_RDWR/writable-mmap 句柄在 Open 到 Release 之间持有唯一全局窗口。
	// 同一 fd 的操作已经处在该窗口内,不能再次获取 window mutex。
	if fd != 0 {
		if existing, ok := root.loadFD(fd); ok && existing.long {
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

	before := root.captureContent(absPath)
	autoID, err := root.manager.OpenStandaloneVersion(
		ctx, absPath, command, root.versionMetadata(ctx, posixOp))
	if err != nil {
		slog.Error("打开自动版本", "path", absPath, "command", command, "error", err)
		return fdVersion{}, syscall.EIO
	}
	window := fdVersion{autoID: autoID}

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
	after := root.captureContent(absPath)
	summary := summarizeContent(before, after, nil)
	if err := root.manager.CloseStandaloneVersion(
		ctx, window.autoID, posixOp, summary); err != nil {
		slog.Error("关闭自动版本", "auto_id", window.autoID, "error", err)
		if abortErr := root.manager.AbortAutoVersion(ctx, window.autoID); abortErr != nil {
			slog.Error("关闭失败后的 auto 补偿", "auto_id", window.autoID, "error", abortErr)
		}
		if fd != 0 {
			root.deleteFD(fd)
		}
		return fdVersion{}, syscall.EIO
	}
	return window, 0
}

// keepWritableWindow 为可写句柄保留一个跨完整 fd 生命周期的自动版本。这样
// 顺序 Write 不再为每个 FUSE request 往返 PostgreSQL;同时覆盖 Linux 可能
// 不把 writable mmap 脏页作为单独 Write 通知的情况。
func (n *Node) keepWritableWindow(
	ctx context.Context,
	absPath, command, posixOp string,
	file *trackedFile,
) syscall.Errno {
	root := n.root
	if root.manager == nil || file == nil || !file.writable {
		return 0
	}
	root.window.Lock()
	before := root.captureContent(absPath)
	autoID, err := root.manager.OpenStandaloneVersion(
		ctx, absPath, command, root.versionMetadata(ctx, posixOp))
	if err != nil {
		root.window.Unlock()
		slog.Error("打开 buffered auto 窗口", "path", absPath, "error", err)
		return syscall.EIO
	}
	root.storeFD(file.id, fdVersion{
		autoID: autoID, long: true,
	})
	file.auditMu.Lock()
	file.path = absPath
	file.posixOp = posixOp
	file.before = before
	file.auditMu.Unlock()
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
	path, posixOp, before, samples := file.auditState()
	releaseErrno := file.LoopbackFile.Release(ctx)
	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	after := n.root.captureContent(path)
	changeSummary := summarizeContent(before, after, samples)
	posixOp = summarizeWriteOp(path, posixOp, samples)
	closeErr := n.root.manager.CloseStandaloneVersion(
		finalCtx, window.autoID, posixOp, changeSummary)
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
