package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type fdVersion struct {
	autoID int64
	long   bool
}

// writableWindow 表示当前唯一开放的自动版本。多个同时存活的可写 fd
// 共享该窗口，避免编辑器持有 swap fd 时再次 open 导致自锁。
type writableWindow struct {
	autoID    int64
	scope     string
	files     map[uint64]*trackedFile
	posixOps  []string
	summaries []string
}

func commonScope(left, right string) string {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	for {
		if right == left || strings.HasPrefix(right, left+string(filepath.Separator)) {
			return left
		}
		parent := filepath.Dir(left)
		if parent == left {
			return left
		}
		left = parent
	}
}

func appendNonEmpty(values []string, value string) []string {
	if value != "" {
		return append(values, value)
	}
	return values
}

func joinAudit(values []string) string {
	return strings.Join(values, "; ")
}

func (l *Loopback) ensureWindowScopeLocked(
	ctx context.Context,
	absPath string,
) error {
	if l.active == nil {
		return nil
	}
	scope := commonScope(l.active.scope, absPath)
	if scope == l.active.scope {
		return nil
	}
	if err := l.manager.UpdateStandaloneVersionScope(
		ctx, l.active.autoID, scope); err != nil {
		return err
	}
	l.active.scope = scope
	return nil
}

// versionedHook 为元数据写创建独立自动版本；存在可写 fd 窗口时，写操作
// 加入该唯一窗口。window mutex 只覆盖本次调用，不跨 fd 生命周期持有。
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
	if root.Quiescing() {
		return fdVersion{}, syscall.EBUSY
	}
	root.window.Lock()
	defer root.window.Unlock()
	if root.Quiescing() {
		return fdVersion{}, syscall.EBUSY
	}

	if root.manager == nil {
		return fdVersion{}, action()
	}

	if root.active != nil {
		if err := root.ensureWindowScopeLocked(ctx, absPath); err != nil {
			slog.Error("扩展 writable auto 范围",
				"auto_id", root.active.autoID, "path", absPath, "error", err)
			return fdVersion{}, syscall.EIO
		}
		current := fdVersion{autoID: root.active.autoID, long: true}
		if _, ok := root.active.files[fd]; fd != 0 && ok {
			return current, action()
		}

		before := root.captureContent(absPath)
		errno := action()
		if errno == 0 {
			after := root.captureContent(absPath)
			root.active.posixOps = appendNonEmpty(root.active.posixOps, posixOp)
			root.active.summaries = appendNonEmpty(
				root.active.summaries,
				summarizeContent(before, after, nil),
			)
		}
		return current, errno
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
		return fdVersion{}, syscall.EIO
	}
	return window, 0
}

// keepWritableWindow 将可写句柄加入当前唯一自动版本。这里不再把 mutex
// 留给 Release 解锁，因此 Vim 等同时持有多个 swap fd 的程序可以继续 open。
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
	defer root.window.Unlock()
	if root.Quiescing() {
		return syscall.EBUSY
	}

	before := root.captureContent(absPath)
	if root.active == nil {
		autoID, err := root.manager.OpenStandaloneVersion(
			ctx, absPath, command, root.versionMetadata(ctx, posixOp))
		if err != nil {
			slog.Error("打开 buffered auto 窗口", "path", absPath, "error", err)
			return syscall.EIO
		}
		root.active = &writableWindow{
			autoID: autoID,
			scope:  absPath,
			files:  make(map[uint64]*trackedFile),
		}
	} else if err := root.ensureWindowScopeLocked(ctx, absPath); err != nil {
		slog.Error("扩展 buffered auto 范围",
			"auto_id", root.active.autoID, "path", absPath, "error", err)
		return syscall.EIO
	}
	root.active.files[file.id] = file
	file.setAuditState(absPath, posixOp, before)
	return 0
}

func (n *Node) closeWritableWindow(
	ctx context.Context,
	file *trackedFile,
) syscall.Errno {
	if file == nil {
		return 0
	}
	if !file.released.CompareAndSwap(false, true) {
		if file.discarded.Load() {
			return syscall.EBUSY
		}
		return 0
	}
	root := n.root
	if root.manager == nil {
		return file.LoopbackFile.Release(ctx)
	}

	root.window.Lock()
	defer root.window.Unlock()

	window := root.active
	if window == nil || window.files[file.id] != file {
		return file.LoopbackFile.Release(ctx)
	}

	path, posixOp, before, samples := file.auditState()
	releaseErrno := file.LoopbackFile.Release(ctx)
	after := root.captureContent(path)
	window.posixOps = appendNonEmpty(
		window.posixOps,
		summarizeWriteOp(path, posixOp, samples),
	)
	window.summaries = appendNonEmpty(
		window.summaries,
		summarizeContent(before, after, samples),
	)
	delete(window.files, file.id)
	if len(window.files) != 0 {
		return releaseErrno
	}

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	closeErr := root.manager.CloseStandaloneVersion(
		finalCtx,
		window.autoID,
		joinAudit(window.posixOps),
		joinAudit(window.summaries),
	)
	cancel()
	root.active = nil
	if closeErr != nil {
		slog.Error("关闭 writable auto 窗口",
			"auto_id", window.autoID, "error", closeErr)
		if abortErr := root.manager.AbortAutoVersion(
			context.Background(), window.autoID); abortErr != nil {
			slog.Error("关闭失败后的 writable auto 补偿",
				"auto_id", window.autoID, "error", abortErr)
		}
		return syscall.EIO
	}
	return releaseErrno
}

// DiscardOpenWrites 在升级冻结后丢弃尚未关闭的完整写窗口。先关闭所有底层
// fd，让已到达 JuiceFS 的元数据归入当前 auto，再用 undo 回放整个窗口。
// 之后旧 FUSE fd 的 Write/Release 都不会再次触达底层文件。
func (l *Loopback) DiscardOpenWrites(ctx context.Context) (int, error) {
	l.SetQuiescing(true)
	l.window.Lock()
	defer l.window.Unlock()
	if l.active == nil {
		return 0, nil
	}
	window := l.active
	count := len(window.files)
	for _, file := range window.files {
		file.discarded.Store(true)
		if file.released.CompareAndSwap(false, true) {
			if errno := file.LoopbackFile.Release(ctx); errno != 0 {
				slog.Warn("丢弃写窗口时关闭底层 fd 失败",
					"auto_id", window.autoID, "fd", file.id, "errno", errno)
			}
		}
	}
	if err := l.manager.AbortAutoVersion(ctx, window.autoID); err != nil {
		return 0, fmt.Errorf("丢弃 writable auto %d: %w", window.autoID, err)
	}
	l.active = nil
	return count, nil
}
