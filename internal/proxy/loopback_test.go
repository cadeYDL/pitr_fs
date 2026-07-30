//go:build linux

package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"pitr_fs/internal/txn"
)

func mountedLoopback(
	t *testing.T,
	options ...Option,
) (backend, mount string, loopback *Loopback) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("当前环境没有 /dev/fuse: %v", err)
	}
	root := t.TempDir()
	backend = filepath.Join(root, "backend")
	mount = filepath.Join(root, "mount")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	loopback, err := NewLoopback(backend, mount, options...)
	if err != nil {
		t.Fatal(err)
	}
	if err := loopback.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("当前环境不允许 FUSE mount: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := loopback.Unmount(); err != nil {
			t.Errorf("Unmount: %v", err)
		}
	})
	return backend, mount, loopback
}

func TestLoopback_WriteRead(t *testing.T) {
	backend, mount, _ := mountedLoopback(t)
	content := []byte("pitr loopback\n")
	if err := os.WriteFile(filepath.Join(mount, "a.txt"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(backend, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("backend content=%q", got)
	}
}

func TestDirectWritePolicy(t *testing.T) {
	if !directWrite(syscall.O_WRONLY) {
		t.Fatal("O_WRONLY 应启用 direct-I/O")
	}
	if directWrite(syscall.O_RDWR) {
		t.Fatal("O_RDWR 必须保留缓存以支持可捕获的 writable mmap")
	}
	if directWrite(syscall.O_RDONLY) {
		t.Fatal("O_RDONLY 不应启用 direct-I/O")
	}
}

func TestLoopback_Mkdir_Ls(t *testing.T) {
	backend, mount, _ := mountedLoopback(t)
	if err := os.Mkdir(filepath.Join(mount, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(mount)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "dir" || !entries[0].IsDir() {
		t.Fatalf("entries=%v", entries)
	}
	info, err := os.Stat(filepath.Join(backend, "dir"))
	if err != nil || !info.IsDir() {
		t.Fatalf("backend dir info=%v err=%v", info, err)
	}
}

func TestLoopback_Rename_CrossDir(t *testing.T) {
	backend, mount, _ := mountedLoopback(t)
	if err := os.MkdirAll(filepath.Join(mount, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mount, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "a", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(mount, "a", "x"),
		filepath.Join(mount, "b", "y"),
	); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(backend, "b", "y")); err != nil ||
		string(got) != "x" {
		t.Fatalf("renamed content=%q err=%v", got, err)
	}
}

func TestLoopback_Symlink_Readlink(t *testing.T) {
	backend, mount, _ := mountedLoopback(t)
	if err := os.WriteFile(filepath.Join(mount, "target"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(mount, "link")); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(mount, "link"))
	if err != nil {
		t.Fatal(err)
	}
	backendTarget, err := os.Readlink(filepath.Join(backend, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "target" || backendTarget != "target" {
		t.Fatalf("mount target=%q backend target=%q", target, backendTarget)
	}
}

func TestLoopback_Unmount(t *testing.T) {
	backend, mount, loopback := mountedLoopback(t)
	if err := os.WriteFile(filepath.Join(mount, "visible"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loopback.Unmount(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mount, "visible")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("卸载后 mount 不应再暴露 backend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backend, "visible")); err != nil {
		t.Fatalf("backend 数据应保留: %v", err)
	}
}

func TestLoopback_AllMutationOperationsAreVersioned(t *testing.T) {
	manager := &mockManager{active: &txn.Txn{ID: 42}}
	backend, mount, _ := mountedLoopback(t, WithManager(manager))
	dir := filepath.Join(mount, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(dir, "file")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(2); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fallocate(int(file.Fd()), 0, 0, 4096); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Linux 可在 close(2) 返回后异步发送 FUSE Release；若立刻对同一路径
	// 设置 xattr，它会正确归入尚未关闭的 fd auto，但 command 不会另起一条。
	// 用一个从 backend 预置、从未打开写 fd 的文件独立验证 xattr handlers。
	xattrPath := filepath.Join(dir, "xattr")
	if err := os.WriteFile(
		filepath.Join(backend, "dir", "xattr"), []byte("x"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(xattrPath, "user.pitr", []byte("yes"), 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Removexattr(xattrPath, "user.pitr"); err != nil {
		t.Fatal(err)
	}
	truncated, err := os.OpenFile(filePath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := truncated.Close(); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(dir, "hardlink")
	if err := os.Link(filePath, hardlink); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink("file", symlink); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(dir, "renamed")
	if err := os.Rename(hardlink, renamed); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{renamed, symlink, xattrPath, filePath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	seen := make(map[string]bool)
	for _, value := range manager.commands {
		op, _, _ := strings.Cut(value, ":")
		seen[op] = true
	}
	for _, op := range []string{
		"mkdir", "create", "open-write", "setxattr", "removexattr", "link",
		"symlink", "rename", "unlink", "rmdir",
	} {
		if !seen[op] {
			t.Errorf("操作 %s 未打点; commands=%v", op, manager.commands)
		}
	}
}

func TestLoopback_FileMutationHandlers_FirstOperation(t *testing.T) {
	manager := &mockManager{active: &txn.Txn{ID: 42}}
	backend, mount, _ := mountedLoopback(t, WithManager(manager))

	reset := func() {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		manager.commands = nil
		manager.parentIDs = nil
		manager.openCalls = 0
		manager.reopenCalls = 0
		manager.closeCalls = 0
	}
	assertFirst := func(want ...string) {
		t.Helper()
		manager.mu.Lock()
		defer manager.mu.Unlock()
		if manager.openCalls < 1 || len(manager.commands) < 1 {
			t.Fatalf("open=%d commands=%v", manager.openCalls, manager.commands)
		}
		got, _, _ := strings.Cut(manager.commands[0], ":")
		for _, candidate := range want {
			if got == candidate {
				return
			}
		}
		t.Fatalf("首操作 command=%q,期望之一 %v", got, want)
	}
	createBackend := func(name string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(backend, name), []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(mount, name)
	}

	t.Run("write", func(t *testing.T) {
		reset()
		file, err := os.OpenFile(createBackend("write"), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("changed")); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		assertFirst("open-write")
	})

	t.Run("setattr", func(t *testing.T) {
		reset()
		path := createBackend("setattr")
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		assertFirst("setattr")
	})

	t.Run("truncate", func(t *testing.T) {
		reset()
		path := createBackend("truncate")
		if err := os.Truncate(path, 2); err != nil {
			t.Fatal(err)
		}
		assertFirst("truncate")
	})

	t.Run("fallocate", func(t *testing.T) {
		reset()
		file, err := os.OpenFile(createBackend("fallocate"), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Fallocate(int(file.Fd()), 0, 0, 4096); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		assertFirst("open-write")
	})

	t.Run("open_trunc", func(t *testing.T) {
		reset()
		file, err := os.OpenFile(createBackend("open_trunc"), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		// Linux FUSE 可把 O_TRUNC 下沉成 SETATTR(size=0),两种路径都满足语义。
		assertFirst("open", "truncate", "open-write")
	})
}

func TestLoopback_WriteSequential_FdDedup(t *testing.T) {
	manager := &mockManager{active: &txn.Txn{ID: 42}}
	backend, mount, _ := mountedLoopback(t, WithManager(manager))
	if err := os.WriteFile(filepath.Join(backend, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(mount, "file"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := file.WriteAt([]byte{byte(i)}, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.openCalls != 1 {
		t.Fatalf("100 次顺序 write 应只创建 1 个 auto,实际 %d; commands=%v",
			manager.openCalls, manager.commands)
	}
	if manager.reopenCalls != 0 {
		t.Fatalf("fd 长窗口不应逐次 reopen; reopen=%d", manager.reopenCalls)
	}
}

func TestLoopback_RenameCrossScope_BelongsToDestination(t *testing.T) {
	manager := &mockManager{
		activeFor: func(path string) *txn.Txn {
			if strings.Contains(path, string(filepath.Separator)+"dst"+string(filepath.Separator)) {
				return &txn.Txn{ID: 2}
			}
			return &txn.Txn{ID: 1}
		},
	}
	_, mount, _ := mountedLoopback(t, WithManager(manager))
	if err := os.Mkdir(filepath.Join(mount, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(mount, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "src", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	manager.parentIDs = nil
	manager.commands = nil
	manager.openCalls = 0
	manager.mu.Unlock()
	if err := os.Rename(
		filepath.Join(mount, "src", "file"),
		filepath.Join(mount, "dst", "file"),
	); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.openCalls != 1 || len(manager.parentIDs) != 1 || manager.parentIDs[0] != 2 {
		t.Fatalf("跨 scope rename 应归属 dst txn=2: parents=%v commands=%v",
			manager.parentIDs, manager.commands)
	}
}

func TestLoopback_ServeStopsOnContext(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("当前环境没有 /dev/fuse: %v", err)
	}
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	mount := filepath.Join(root, "mount")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	loopback, err := NewLoopback(backend, mount)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loopback.Serve(ctx) }()
	for i := 0; i < 100; i++ {
		if loopback.Mounted() {
			break
		}
		syscall.Nanosleep(&syscall.Timespec{Nsec: 1_000_000}, nil)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
