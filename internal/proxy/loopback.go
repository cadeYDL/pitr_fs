// Package proxy 提供面向用户的 FUSE loopback 与 PITR 写操作拦截。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"pitr_fs/internal/txn"
)

// VersionManager 是 FUSE 层需要的最小事务接口,便于对失败路径做隔离测试。
type VersionManager interface {
	FindActiveByPath(context.Context, string) (*txn.Txn, error)
	OpenAutoVersion(context.Context, int64, string) (int64, error)
	ReopenAutoVersion(context.Context, int64, int64) error
	CloseAutoVersion(context.Context, int64) error
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

	mountMu  sync.Mutex
	window   sync.Mutex
	fdMu     sync.Mutex
	fds      map[uint64]fdVersion
	longPath string
	nextFD   atomic.Uint64
}

func (l *Loopback) setLongPath(value string) {
	l.fdMu.Lock()
	l.longPath = value
	l.fdMu.Unlock()
}

func (l *Loopback) isLongPath(value string) bool {
	l.fdMu.Lock()
	defer l.fdMu.Unlock()
	return l.longPath != "" && l.longPath == value
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
		Backend: backend,
		Mount:   mount,
		fds:     make(map[uint64]fdVersion),
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
			FsName:     "pitrfs",
			Name:       "pitrfs",
			AllowOther: l.allowOther,
		},
		EntryTimeout:    &zeroCache,
		AttrTimeout:     &zeroCache,
		NegativeTimeout: &zeroCache,
	})
	if err != nil {
		return fmt.Errorf("挂载 FUSE %s: %w", l.Mount, err)
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

// O_WRONLY 是普通写入的主路径,用 direct-I/O 保证每次 write 落在 fd 长窗口
// 内。writable mmap 必须以 O_RDWR 打开;该路径保留页缓存,由同一个 fd 长窗口
// 覆盖 msync/close。调用方必须在 commit 前关闭可写 fd。
func directWrite(flags uint32) bool {
	return flags&syscall.O_ACCMODE == syscall.O_WRONLY
}

func (l *Loopback) visiblePath(node *Node, names ...string) string {
	relative := node.Path(l.rootData.RootNode.EmbeddedInode())
	parts := append([]string{l.Mount, relative}, names...)
	return filepath.Join(parts...)
}
