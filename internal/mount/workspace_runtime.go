package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"pitr_fs/internal/proxy"
	"pitr_fs/internal/txn"
	"pitr_fs/internal/workspace"
)

type WorkspaceProxy interface {
	Start() error
	Unmount() error
	UnmountLazy() error
	SetQuiescing(bool)
	DiscardOpenWrites(context.Context) (int, error)
}

type WorkspaceBinder interface {
	Bind(source, target string) error
	Unmount(target string, lazy bool) error
}

type WorkspaceProxyFactory func(
	backend string,
	mount string,
	manager *txn.Manager,
) (WorkspaceProxy, error)

type WorkspaceRuntimeOption func(*WorkspaceRuntime)

func WithWorkspaceBinder(value WorkspaceBinder) WorkspaceRuntimeOption {
	return func(runtime *WorkspaceRuntime) { runtime.binder = value }
}

func WithWorkspaceProxyFactory(value WorkspaceProxyFactory) WorkspaceRuntimeOption {
	return func(runtime *WorkspaceRuntime) { runtime.proxyFactory = value }
}

type workspaceRuntimeEntry struct {
	workspace workspace.Workspace
	proxy     WorkspaceProxy
	hidden    string
	aliases   map[string]struct{}
}

// WorkspaceRuntime 为每个 workspace 只创建一个 pitr FUSE proxy；多个用户
// 挂载点使用 bind mount 指向同一 proxy，从而共享写窗口和版本时间线。
type WorkspaceRuntime struct {
	jfsRoot      string
	runtimeRoot  string
	manager      *txn.Manager
	binder       WorkspaceBinder
	proxyFactory WorkspaceProxyFactory

	mu      sync.Mutex
	entries map[int64]*workspaceRuntimeEntry
}

func NewWorkspaceRuntime(
	jfsRoot string,
	runtimeRoot string,
	manager *txn.Manager,
	options ...WorkspaceRuntimeOption,
) *WorkspaceRuntime {
	runtime := &WorkspaceRuntime{
		jfsRoot: filepath.Clean(jfsRoot), runtimeRoot: filepath.Clean(runtimeRoot),
		manager: manager, binder: unixWorkspaceBinder{},
		entries: make(map[int64]*workspaceRuntimeEntry),
	}
	runtime.proxyFactory = func(backend, mount string, manager *txn.Manager) (
		WorkspaceProxy,
		error,
	) {
		options := []proxy.Option{
			proxy.WithManager(manager), proxy.WithAllowOther(true),
			proxy.WithDisplayRoot("/"),
		}
		if filepath.Clean(backend) == runtime.jfsRoot {
			options = append(options, proxy.WithHiddenRootName(".pitr"))
		}
		return proxy.NewLoopback(backend, mount, options...)
	}
	for _, option := range options {
		option(runtime)
	}
	return runtime
}

func (r *WorkspaceRuntime) workspaceBackend(item workspace.Workspace) (string, error) {
	backend := r.jfsRoot
	if item.BackendPath != "/" {
		backend = filepath.Join(r.jfsRoot, filepath.FromSlash(item.BackendPath))
	}
	backend = filepath.Clean(backend)
	rootPrefix := r.jfsRoot + string(os.PathSeparator)
	if backend != r.jfsRoot && !strings.HasPrefix(backend, rootPrefix) {
		return "", fmt.Errorf("workspace backend 越界: %s", item.BackendPath)
	}
	return backend, nil
}

func (r *WorkspaceRuntime) Mount(
	_ context.Context,
	item workspace.Workspace,
	alias string,
) error {
	alias = filepath.Clean(alias)
	if !filepath.IsAbs(alias) || alias == "/" {
		return fmt.Errorf("workspace 挂载点必须是非根绝对路径: %s", alias)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.entries[item.ID]; entry != nil {
		if _, exists := entry.aliases[alias]; exists {
			return nil
		}
		if err := r.binder.Bind(entry.hidden, alias); err != nil {
			return err
		}
		entry.aliases[alias] = struct{}{}
		return nil
	}

	backend, err := r.workspaceBackend(item)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(backend, 0o755); err != nil {
		return fmt.Errorf("创建 workspace 后端 %s: %w", backend, err)
	}
	hidden := filepath.Join(r.runtimeRoot, strconv.FormatInt(item.ID, 10))
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		return fmt.Errorf("创建 workspace 运行挂载点 %s: %w", hidden, err)
	}
	manager := r.manager
	if manager != nil {
		manager = manager.ForWorkspace(item.ID)
	}
	created, err := r.proxyFactory(backend, hidden, manager)
	if err != nil {
		return err
	}
	if err := created.Start(); err != nil {
		return err
	}
	if err := r.binder.Bind(hidden, alias); err != nil {
		_ = created.Unmount()
		return err
	}
	r.entries[item.ID] = &workspaceRuntimeEntry{
		workspace: item, proxy: created, hidden: hidden,
		aliases: map[string]struct{}{alias: {}},
	}
	return nil
}

func (r *WorkspaceRuntime) Umount(
	_ context.Context,
	item workspace.Workspace,
	alias string,
	lazy bool,
) error {
	alias = filepath.Clean(alias)
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[item.ID]
	if entry == nil {
		return nil
	}
	if _, exists := entry.aliases[alias]; !exists {
		return nil
	}
	if err := r.binder.Unmount(alias, lazy); err != nil {
		return err
	}
	delete(entry.aliases, alias)
	if len(entry.aliases) != 0 {
		return nil
	}
	var err error
	if lazy {
		err = entry.proxy.UnmountLazy()
	} else {
		err = entry.proxy.Unmount()
	}
	if err == nil {
		delete(r.entries, item.ID)
	}
	return err
}

func (r *WorkspaceRuntime) SetQuiescing(workspaceID int64, enabled bool) {
	r.mu.Lock()
	entry := r.entries[workspaceID]
	r.mu.Unlock()
	if entry != nil {
		entry.proxy.SetQuiescing(enabled)
	}
}

func (r *WorkspaceRuntime) DiscardOpenWrites(
	ctx context.Context,
	workspaceID int64,
) (int, error) {
	r.mu.Lock()
	entry := r.entries[workspaceID]
	r.mu.Unlock()
	if entry == nil {
		return 0, nil
	}
	return entry.proxy.DiscardOpenWrites(ctx)
}

func (r *WorkspaceRuntime) Mounted(alias string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	alias = filepath.Clean(alias)
	for _, entry := range r.entries {
		if _, exists := entry.aliases[alias]; exists {
			return true
		}
	}
	return false
}

func (r *WorkspaceRuntime) StopAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var errs []error
	for _, id := range ids {
		entry := r.entries[id]
		for alias := range entry.aliases {
			if err := r.binder.Unmount(alias, true); err != nil {
				errs = append(errs, err)
			}
		}
		if err := entry.proxy.UnmountLazy(); err != nil {
			errs = append(errs, err)
		}
		delete(r.entries, id)
	}
	return errors.Join(errs...)
}

type unixWorkspaceBinder struct{}

func (unixWorkspaceBinder) Bind(source, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount %s -> %s: %w", source, target, err)
	}
	return nil
}

func (unixWorkspaceBinder) Unmount(target string, lazy bool) error {
	flags := 0
	if lazy {
		flags = unix.MNT_DETACH
	}
	if err := unix.Unmount(target, flags); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("卸载 workspace 挂载 %s: %w", target, err)
	}
	return nil
}
