// Package proxy 提供面向用户的 FUSE loopback 与 PITR 写操作拦截。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"pitr_fs/internal/txn"
)

// VersionManager 是 FUSE 层需要的最小事务接口,便于对失败路径做隔离测试。
type VersionManager interface {
	OpenStandaloneVersion(
		context.Context, string, string, txn.VersionMetadata,
	) (int64, error)
	CloseStandaloneVersion(context.Context, int64, string, string) error
	UpdateStandaloneVersionScope(context.Context, int64, string) error
	AbortAutoVersion(context.Context, int64) error
}

type Option func(*Loopback)

func WithManager(manager VersionManager) Option {
	return func(loopback *Loopback) {
		loopback.manager = manager
	}
}

func WithAllowOther(allow bool) Option {
	return func(loopback *Loopback) {
		loopback.allowOther = allow
	}
}

// Loopback 把 Backend 透明暴露到 Mount。所有变更操作由自定义 Node 拦截,
// 读取操作继续使用 go-fuse 的 LoopbackNode 实现。
type Loopback struct {
	Backend string
	Mount   string
	Server  *fuse.Server

	manager    VersionManager
	allowOther bool
	rootData   *fs.LoopbackRoot
	rootNode   fs.InodeEmbedder

	mountMu sync.Mutex
	// window 只保护一次 FUSE 写操作或窗口成员变更，绝不能跨 fd 生命周期持有。
	// active 允许同一时刻打开的多个可写 fd 共用唯一 auto，适配 JuiceFS trigger
	// 只能归属到唯一开放版本的约束。
	window sync.Mutex
	active *writableWindow
	nextFD atomic.Uint64
	// writeGate 按原因维护写屏障。外部升级屏障与失败恢复可以重叠，任一原因
	// 仍存在时都必须拒绝新写，不能由另一方错误地提前解除。
	writeGate atomic.Uint32
	// managerTimeout 限制持有 window mutex 时的数据库调用和失败补偿。
	managerTimeout time.Duration

	auditMu      sync.Mutex
	processCache map[uint32]processCacheEntry
	userCache    map[uint32]string
}

const (
	writeGateExternal uint32 = 1 << iota
	writeGateRecovery
	defaultManagerTimeout = 5 * time.Second
)

func (l *Loopback) setWriteGate(reason uint32, enabled bool) {
	for {
		current := l.writeGate.Load()
		next := current | reason
		if !enabled {
			next = current &^ reason
		}
		if current == next || l.writeGate.CompareAndSwap(current, next) {
			return
		}
	}
}

func (l *Loopback) managerContext(parent context.Context) (
	context.Context,
	context.CancelFunc,
) {
	timeout := l.managerTimeout
	if timeout <= 0 {
		timeout = defaultManagerTimeout
	}
	return context.WithTimeout(parent, timeout)
}

// SetQuiescing 原子关闭/开放写入口。启用时通过 window mutex 做一次屏障，
// 返回后所有更早进入的单次写操作都已完整结束。
func (l *Loopback) SetQuiescing(enabled bool) {
	l.setWriteGate(writeGateExternal, enabled)
	if enabled {
		l.window.Lock()
		l.window.Unlock()
	}
}

func (l *Loopback) Quiescing() bool {
	return l.writeGate.Load() != 0
}

func NewLoopback(backend, mount string, options ...Option) (*Loopback, error) {
	if backend == "" || mount == "" {
		return nil, errors.New("backend 和 mount 均不能为空")
	}
	backend = filepath.Clean(backend)
	mount = filepath.Clean(mount)
	if !filepath.IsAbs(backend) || !filepath.IsAbs(mount) {
		return nil, errors.New("backend 和 mount 必须是绝对路径")
	}
	if backend == mount {
		return nil, errors.New("backend 与 mount 不能相同")
	}

	var st syscall.Stat_t
	if err := syscall.Stat(backend, &st); err != nil {
		return nil, fmt.Errorf("检查 backend %s: %w", backend, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return nil, fmt.Errorf("backend %s 不是目录", backend)
	}

	loopback := &Loopback{
		Backend:        backend,
		Mount:          mount,
		managerTimeout: defaultManagerTimeout,
		processCache:   make(map[uint32]processCacheEntry),
		userCache:      make(map[uint32]string),
	}
	for _, option := range options {
		option(loopback)
	}

	rootData := &fs.LoopbackRoot{
		Path: backend,
		Dev:  uint64(st.Dev),
	}
	loopback.rootData = rootData
	rootData.NewNode = func(
		data *fs.LoopbackRoot,
		_ *fs.Inode,
		_ string,
		_ *syscall.Stat_t,
	) fs.InodeEmbedder {
		return &Node{
			LoopbackNode: fs.LoopbackNode{RootData: data},
			root:         loopback,
		}
	}
	rootNode := rootData.NewNode(rootData, nil, "", &st)
	rootData.RootNode = rootNode
	loopback.rootNode = rootNode
	return loopback, nil
}

// Start 挂载并启动 go-fuse 服务。服务本身由 go-fuse 后台 goroutine 处理。
func (l *Loopback) Start() error {
	l.mountMu.Lock()
	defer l.mountMu.Unlock()
	if l.Server != nil {
		return nil
	}
	if err := os.MkdirAll(l.Mount, 0o755); err != nil {
		return fmt.Errorf("创建 FUSE 挂载点 %s: %w", l.Mount, err)
	}
	zeroCache := time.Duration(0)
	server, err := fs.Mount(l.Mount, l.rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:            "pitrfs",
			Name:              "pitrfs",
			AllowOther:        l.allowOther,
			ExtraCapabilities: fuse.CAP_DIRECT_IO_ALLOW_MMAP,
		},
		EntryTimeout:    &zeroCache,
		AttrTimeout:     &zeroCache,
		NegativeTimeout: &zeroCache,
	})
	if err != nil {
		return fmt.Errorf("挂载 FUSE %s: %w", l.Mount, err)
	}
	if server.KernelSettings().Flags64()&fuse.CAP_DIRECT_IO_ALLOW_MMAP == 0 {
		_ = server.Unmount()
		return errors.New(
			"Linux 内核不支持 FUSE_CAP_DIRECT_IO_ALLOW_MMAP，" +
				"无法同时保证可写 mmap 与升级写入原子性",
		)
	}
	l.Server = server
	return nil
}

// Serve 挂载后阻塞,直到 ctx 取消或 FUSE 服务自行退出。
func (l *Loopback) Serve(ctx context.Context) error {
	if err := l.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func(server *fuse.Server) {
		server.Wait()
		close(done)
	}(l.Server)

	select {
	case <-ctx.Done():
		if err := l.Unmount(); err != nil {
			return err
		}
		<-done
		return nil
	case <-done:
		return nil
	}
}

func (l *Loopback) Unmount() error {
	l.mountMu.Lock()
	defer l.mountMu.Unlock()
	if l.Server == nil {
		return nil
	}
	if err := l.Server.Unmount(); err != nil {
		return fmt.Errorf("卸载 FUSE %s: %w", l.Mount, err)
	}
	l.Server = nil
	return nil
}

// UnmountLazy 用于升级已经丢弃开放写窗口后切断旧挂载。
// MNT_DETACH 会立即从当前命名空间移除挂载；仍持有旧 fd 的进程
// 只能看到已被冻结的旧 FUSE 实例，不会阻塞新版本重新挂载。
func (l *Loopback) UnmountLazy() error {
	l.mountMu.Lock()
	defer l.mountMu.Unlock()
	if l.Server == nil {
		return nil
	}
	if err := unix.Unmount(l.Mount, unix.MNT_DETACH); err != nil {
		if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("惰性卸载 FUSE %s: %w", l.Mount, err)
		}
		// 非特权 FUSE owner 没有 CAP_SYS_ADMIN，直接 umount(2) 会返回
		// EPERM；fusermount3 通过受控 helper 执行同一个 lazy detach。
		output, helperErr := exec.Command("fusermount3", "-uz", l.Mount).
			CombinedOutput()
		if helperErr != nil {
			return fmt.Errorf("惰性卸载 FUSE %s: syscall=%v; fusermount3: %w: %s",
				l.Mount, err, helperErr, string(output))
		}
	}
	l.Server = nil
	return nil
}

func (l *Loopback) Mounted() bool {
	l.mountMu.Lock()
	defer l.mountMu.Unlock()
	return l.Server != nil
}

func (l *Loopback) newFile(base fs.FileHandle, flags uint32) *trackedFile {
	loopbackFile, ok := base.(*fs.LoopbackFile)
	if !ok {
		return nil
	}
	return &trackedFile{
		LoopbackFile: loopbackFile,
		id:           l.nextFD.Add(1),
		writable:     flags&syscall.O_ACCMODE != syscall.O_RDONLY,
	}
}

// 所有可写 fd 使用 direct-I/O，保证应用的 write(2) 在返回前已进入
// FUSE 版本窗口；这是升级冻结能让后续写立即失败、并完整丢弃半成品的
// 前提。挂载时要求内核支持 DIRECT_IO_ALLOW_MMAP，因此 O_RDWR 的可写
// mmap 仍由同一 fd 长窗口覆盖。
func directWrite(flags uint32) bool {
	return flags&syscall.O_ACCMODE != syscall.O_RDONLY
}

func (l *Loopback) visiblePath(node *Node, names ...string) string {
	relative := node.Path(l.rootData.RootNode.EmbeddedInode())
	parts := append([]string{l.Mount, relative}, names...)
	return filepath.Join(parts...)
}
