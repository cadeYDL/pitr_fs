package proxy

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"pitr_fs/internal/txn"
)

type mockManager struct {
	mu sync.Mutex

	openErr      error
	closeErr     error
	abortErr     error
	nextAuto     int64
	openCalls    int
	closeCalls   int
	abortCalls   int
	commands     []string
	scopes       []string
	metadata     []txn.VersionMetadata
	posixOps     []string
	summaries    []string
	scopeUpdates []string
	abortStarted chan struct{}
	abortRelease chan struct{}
	abortOnce    sync.Once
}

func (m *mockManager) OpenStandaloneVersion(
	_ context.Context,
	scope string,
	command string,
	metadata txn.VersionMetadata,
) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openCalls++
	m.commands = append(m.commands, command)
	m.scopes = append(m.scopes, scope)
	m.metadata = append(m.metadata, metadata)
	if m.openErr != nil {
		return 0, m.openErr
	}
	m.nextAuto++
	return m.nextAuto, nil
}

func (m *mockManager) CloseStandaloneVersion(
	_ context.Context,
	_ int64,
	posixOp string,
	summary string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	m.posixOps = append(m.posixOps, posixOp)
	m.summaries = append(m.summaries, summary)
	return m.closeErr
}

func (m *mockManager) UpdateStandaloneVersionScope(
	_ context.Context,
	_ int64,
	scope string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scopeUpdates = append(m.scopeUpdates, scope)
	return nil
}

func (m *mockManager) AbortAutoVersion(ctx context.Context, _ int64) error {
	m.mu.Lock()
	m.abortCalls++
	err := m.abortErr
	started := m.abortStarted
	release := m.abortRelease
	m.mu.Unlock()
	if started != nil {
		m.abortOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func newHookNode(manager VersionManager) *Node {
	return &Node{root: &Loopback{manager: manager}}
}

func TestHook_EveryMutationCreatesVersion(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a",
		`write("/workspace/a")`, 1,
		func() syscall.Errno {
			called = true
			return 0
		})
	if errno != 0 || !called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
	if manager.openCalls != 1 || manager.closeCalls != 1 ||
		len(manager.commands) != 1 || manager.commands[0] != "write:/workspace/a" ||
		len(manager.scopes) != 1 || manager.scopes[0] != "/workspace/a" {
		t.Fatalf("auto 生命周期不正确: %+v", manager)
	}
}

func TestHook_EachMetadataCallCreatesVersion(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	for i := 0; i < 100; i++ {
		errno := node.versionedHook(
			context.Background(), "/workspace/a", "write:/workspace/a",
			`write("/workspace/a")`, 7,
			func() syscall.Errno { return 0 })
		if errno != 0 {
			t.Fatalf("第 %d 次 errno=%v", i, errno)
		}
	}
	if manager.openCalls != 100 || manager.closeCalls != 100 {
		t.Fatalf("每次独立操作应形成版本: open=%d close=%d",
			manager.openCalls, manager.closeCalls)
	}
}

func TestHook_PGFail_ReturnsEIO(t *testing.T) {
	manager := &mockManager{
		openErr: errors.New("pg unavailable"),
	}
	node := newHookNode(manager)
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a",
		`write("/workspace/a")`, 7,
		func() syscall.Errno {
			called = true
			return 0
		})
	if errno != syscall.EIO || called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
}

func TestHook_ActionFail_RollsBackVersion(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a",
		`write("/workspace/a")`, 7,
		func() syscall.Errno { return syscall.ENOSPC })
	if errno != syscall.ENOSPC {
		t.Fatalf("应保留 action errno,实际 %v", errno)
	}
	if manager.abortCalls != 1 || manager.closeCalls != 0 {
		t.Fatalf("失败 action 应补偿且不 close: %+v", manager)
	}
}

func TestHook_CloseFail_CompensatesAndReturnsEIO(t *testing.T) {
	manager := &mockManager{
		closeErr: errors.New("close failed"),
	}
	node := newHookNode(manager)
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a",
		`write("/workspace/a")`, 7,
		func() syscall.Errno { return 0 })
	if errno != syscall.EIO || manager.abortCalls != 1 {
		t.Fatalf("errno=%v abort=%d", errno, manager.abortCalls)
	}
}

func TestHook_QuiescingRejectsBeforeMutation(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	node.root.SetQuiescing(true)
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a",
		`write("/workspace/a")`, 0,
		func() syscall.Errno {
			called = true
			return 0
		})
	if errno != syscall.EBUSY || called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
	if manager.openCalls != 0 || manager.closeCalls != 0 {
		t.Fatalf("冻结写入不应创建中间版本: %+v", manager)
	}
}

func TestHook_NormalRecoveryGateWaitsThenContinues(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	node.root.setWriteGate(writeGateRecovery, true)
	go func() {
		time.Sleep(25 * time.Millisecond)
		node.root.setWriteGate(writeGateRecovery, false)
	}()
	started := time.Now()
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a",
		`write("/workspace/a")`, 0,
		func() syscall.Errno { called = true; return 0 })
	if errno != 0 || !called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond ||
		elapsed > time.Second {
		t.Fatalf("recovery gate 等待时间异常: %s", elapsed)
	}
}

func TestKeepWritableWindow_MultipleFDsDoNotHoldLifetimeLock(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	first := &trackedFile{id: 1, writable: true}
	second := &trackedFile{id: 2, writable: true}
	if errno := node.keepWritableWindow(
		context.Background(), "/workspace/.file.swp", "open-write",
		"open-swp", first); errno != 0 {
		t.Fatalf("first errno=%v", errno)
	}

	done := make(chan syscall.Errno, 1)
	go func() {
		done <- node.keepWritableWindow(
			context.Background(), "/workspace/.file.swpx", "open-write",
			"open-swpx", second)
	}()
	select {
	case errno := <-done:
		if errno != 0 {
			t.Fatalf("second errno=%v", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("第二个可写 fd 被第一个 fd 生命周期锁阻塞")
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.openCalls != 1 {
		t.Fatalf("并发可写 fd 应共享唯一 auto, open=%d", manager.openCalls)
	}
	if len(manager.scopeUpdates) != 1 ||
		manager.scopeUpdates[0] != "/workspace" {
		t.Fatalf("共享窗口应扩展到共同父目录: %v", manager.scopeUpdates)
	}
}
