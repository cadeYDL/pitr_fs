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
	"time"

	"golang.org/x/sys/unix"
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
	if !directWrite(syscall.O_RDWR) {
		t.Fatal("O_RDWR 应启用支持 mmap 的 direct-I/O")
	}
	if directWrite(syscall.O_RDONLY) {
		t.Fatal("O_RDONLY 不应启用 direct-I/O")
	}
}

func TestLoopback_DirectWritableMmap(t *testing.T) {
	backend, mount, _ := mountedLoopback(t, WithManager(new(mockManager)))
	backendPath := filepath.Join(backend, "mapped")
	if err := os.WriteFile(backendPath, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(mount, "mapped"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := unix.Mmap(
		int(file.Fd()), 0, len("baseline"),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED,
	)
	if err != nil {
		_ = file.Close()
		t.Fatalf("direct-I/O writable mmap: %v", err)
	}
	copy(mapping, "changed!")
	if err := unix.Msync(mapping, unix.MS_SYNC); err != nil {
		t.Fatal(err)
	}
	if err := unix.Munmap(mapping); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(backendPath); err != nil || string(got) != "changed!" {
		t.Fatalf("direct mmap content=%q err=%v", got, err)
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
	manager := new(mockManager)
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
	manager := new(mockManager)
	backend, mount, _ := mountedLoopback(t, WithManager(manager))

	reset := func() {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		manager.commands = nil
		manager.scopes = nil
		manager.openCalls = 0
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
	manager := new(mockManager)
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
}

func TestLoopback_MultipleWritableFDsKeepDirectoryResponsive(t *testing.T) {
	manager := new(mockManager)
	_, mount, loopback := mountedLoopback(t, WithManager(manager))
	firstPath := filepath.Join(mount, ".file.swp")
	first, err := os.OpenFile(
		firstPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	type openResult struct {
		file *os.File
		err  error
	}
	secondResult := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(
			filepath.Join(mount, ".file.swpx"),
			os.O_CREATE|os.O_EXCL|os.O_RDWR,
			0o600,
		)
		secondResult <- openResult{file: file, err: err}
	}()

	var second *os.File
	select {
	case result := <-secondResult:
		if result.err != nil {
			_ = first.Close()
			t.Fatal(result.err)
		}
		second = result.file
	case <-time.After(2 * time.Second):
		_ = first.Close()
		select {
		case result := <-secondResult:
			if result.file != nil {
				_ = result.file.Close()
			}
		case <-time.After(2 * time.Second):
		}
		t.Fatal("第二个 Vim swap fd open 超时")
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := os.ReadDir(mount)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("可写 fd 存活期间目录读取超时")
	}

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var active *writableWindow
	for {
		loopback.window.Lock()
		active = loopback.active
		loopback.window.Unlock()
		if active == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if active != nil {
		t.Fatalf("所有可写 fd 关闭后仍残留 active auto: %+v", active)
	}
}

func TestLoopback_QuiescingRejectsWritesWithoutPartialData(t *testing.T) {
	backend, mount, loopback := mountedLoopback(t, WithManager(new(mockManager)))
	path := filepath.Join(mount, "file")
	if err := os.WriteFile(filepath.Join(backend, "file"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	loopback.SetQuiescing(true)
	if _, err := file.WriteAt([]byte("after"), 0); err == nil {
		t.Fatal("冻结后已有可写 fd 的新写入应直接失败")
	}
	if err := file.Close(); err == nil {
		t.Fatal("冻结后旧可写 fd 的 close/flush 应明确失败")
	}
	if err := os.WriteFile(filepath.Join(mount, "new"), []byte("x"), 0o644); err == nil {
		t.Fatal("冻结后创建文件应直接失败")
	}
	if got, err := os.ReadFile(filepath.Join(backend, "file")); err != nil ||
		string(got) != "before" {
		t.Fatalf("冻结失败后出现中间态 content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(backend, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("冻结失败后不应创建底层文件: %v", err)
	}
	loopback.SetQuiescing(false)
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatalf("解除冻结后写入失败: %v", err)
	}
}

type restoringManager struct {
	*mockManager
	path     string
	baseline []byte
}

func (m *restoringManager) AbortAutoVersion(ctx context.Context, id int64) error {
	if err := m.mockManager.AbortAutoVersion(ctx, id); err != nil {
		return err
	}
	return os.WriteFile(m.path, m.baseline, 0o644)
}

func TestLoopback_DiscardOpenWritesRestoresPartialFile(t *testing.T) {
	manager := &restoringManager{mockManager: new(mockManager)}
	backend, mount, loopback := mountedLoopback(t, WithManager(manager))
	manager.path = filepath.Join(backend, "large")
	manager.baseline = []byte("baseline")
	if err := os.WriteFile(manager.path, manager.baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(mount, "large"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("partial!"), 0); err != nil {
		t.Fatal(err)
	}
	discarded, err := loopback.DiscardOpenWrites(context.Background())
	if err != nil || discarded != 1 {
		t.Fatalf("discarded=%d err=%v", discarded, err)
	}
	if err := loopback.UnmountLazy(); err != nil {
		t.Fatalf("开放 fd 存在时惰性卸载: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mount, "large")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("惰性卸载后旧路径仍可见: %v", err)
	}
	if err := file.Sync(); err == nil {
		t.Fatal("丢弃后旧 fd 不应继续 fsync")
	}
	if _, err := file.WriteAt([]byte("later"), 0); err == nil {
		t.Fatal("丢弃后旧 fd 不应继续写入")
	}
	_ = file.Close()
	if got, err := os.ReadFile(manager.path); err != nil ||
		string(got) != string(manager.baseline) {
		t.Fatalf("丢弃后 content=%q err=%v", got, err)
	}
	manager.mu.Lock()
	aborts := manager.abortCalls
	manager.mu.Unlock()
	if aborts != 1 {
		t.Fatalf("abort calls=%d", aborts)
	}
}

func TestLoopback_RenameCrossScope_BelongsToDestination(t *testing.T) {
	manager := new(mockManager)
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
	manager.scopes = nil
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
	wantScope := filepath.Join(mount, "dst", "file")
	if manager.openCalls != 1 || len(manager.scopes) != 1 ||
		manager.scopes[0] != wantScope {
		t.Fatalf("跨 scope rename 应归属 dst path=%s: scopes=%v commands=%v",
			wantScope, manager.scopes, manager.commands)
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
