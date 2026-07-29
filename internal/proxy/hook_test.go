package proxy

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"

	"pitr_fs/internal/txn"
)

type mockManager struct {
	mu sync.Mutex

	active      *txn.Txn
	activeFor   func(string) *txn.Txn
	findErr     error
	openErr     error
	reopenErr   error
	closeErr    error
	abortErr    error
	nextAuto    int64
	openCalls   int
	reopenCalls int
	closeCalls  int
	abortCalls  int
	commands    []string
	parentIDs   []int64
}

func (m *mockManager) FindActiveByPath(_ context.Context, path string) (*txn.Txn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeFor != nil {
		return m.activeFor(path), m.findErr
	}
	return m.active, m.findErr
}

func (m *mockManager) OpenAutoVersion(
	_ context.Context,
	parentID int64,
	command string,
) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openCalls++
	m.commands = append(m.commands, command)
	m.parentIDs = append(m.parentIDs, parentID)
	if m.openErr != nil {
		return 0, m.openErr
	}
	m.nextAuto++
	return m.nextAuto, nil
}

func (m *mockManager) ReopenAutoVersion(context.Context, int64, int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reopenCalls++
	return m.reopenErr
}

func (m *mockManager) CloseAutoVersion(context.Context, int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	return m.closeErr
}

func (m *mockManager) AbortAutoVersion(context.Context, int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abortCalls++
	return m.abortErr
}

func newHookNode(manager VersionManager) *Node {
	return &Node{root: &Loopback{
		manager: manager,
		fds:     make(map[uint64]fdVersion),
	}}
}

func TestHook_NoActiveTxn_PassThrough(t *testing.T) {
	manager := new(mockManager)
	node := newHookNode(manager)
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a", 1,
		func() syscall.Errno {
			called = true
			return 0
		})
	if errno != 0 || !called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
	if manager.openCalls != 0 || manager.closeCalls != 0 {
		t.Fatalf("无 active 时不应创建 auto: %+v", manager)
	}
}

func TestHook_ActiveTxn_CreatesAutoVersion(t *testing.T) {
	manager := &mockManager{active: &txn.Txn{ID: 42}}
	node := newHookNode(manager)
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a", 7,
		func() syscall.Errno { return 0 })
	if errno != 0 {
		t.Fatalf("errno=%v", errno)
	}
	if manager.openCalls != 1 || manager.closeCalls != 1 ||
		len(manager.commands) != 1 || manager.commands[0] != "write:/workspace/a" {
		t.Fatalf("auto 生命周期不正确: %+v", manager)
	}
}

func TestHook_FdDedup(t *testing.T) {
	manager := &mockManager{active: &txn.Txn{ID: 42}}
	node := newHookNode(manager)
	for i := 0; i < 100; i++ {
		errno := node.versionedHook(
			context.Background(), "/workspace/a", "write:/workspace/a", 7,
			func() syscall.Errno { return 0 })
		if errno != 0 {
			t.Fatalf("第 %d 次 errno=%v", i, errno)
		}
	}
	if manager.openCalls != 1 || manager.reopenCalls != 99 ||
		manager.closeCalls != 100 {
		t.Fatalf("fd 去重失败: open=%d reopen=%d close=%d",
			manager.openCalls, manager.reopenCalls, manager.closeCalls)
	}
}

func TestHook_PGFail_ReturnsEIO(t *testing.T) {
	manager := &mockManager{
		active:  &txn.Txn{ID: 42},
		openErr: errors.New("pg unavailable"),
	}
	node := newHookNode(manager)
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a", 7,
		func() syscall.Errno {
			called = true
			return 0
		})
	if errno != syscall.EIO || called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
}

func TestHook_ActionFail_RollsBackVersion(t *testing.T) {
	manager := &mockManager{active: &txn.Txn{ID: 42}}
	node := newHookNode(manager)
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a", 7,
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
		active:   &txn.Txn{ID: 42},
		closeErr: errors.New("close failed"),
	}
	node := newHookNode(manager)
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "write:/workspace/a", 7,
		func() syscall.Errno { return 0 })
	if errno != syscall.EIO || manager.abortCalls != 1 {
		t.Fatalf("errno=%v abort=%d", errno, manager.abortCalls)
	}
}

func TestHook_FindFail_DoesNotMutate(t *testing.T) {
	manager := &mockManager{findErr: errors.New("pg unavailable")}
	node := newHookNode(manager)
	called := false
	errno := node.versionedHook(
		context.Background(), "/workspace/a", "unlink:/workspace/a", 0,
		func() syscall.Errno {
			called = true
			return 0
		})
	if errno != syscall.EIO || called {
		t.Fatalf("errno=%v called=%v", errno, called)
	}
}
