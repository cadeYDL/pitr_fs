package proxy

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
)

type mockManager struct {
	mu sync.Mutex

	openErr    error
	closeErr   error
	abortErr   error
	nextAuto   int64
	openCalls  int
	closeCalls int
	abortCalls int
	commands   []string
	scopes     []string
}

func (m *mockManager) OpenStandaloneVersion(
	_ context.Context,
	scope string,
	command string,
) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openCalls++
	m.commands = append(m.commands, command)
	m.scopes = append(m.scopes, scope)
	if m.openErr != nil {
		return 0, m.openErr
	}
	m.nextAuto++
	return m.nextAuto, nil
}

func (m *mockManager) CloseStandaloneVersion(context.Context, int64) error {
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

func TestHook_EveryMutationCreatesVersion(t *testing.T) {
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
			context.Background(), "/workspace/a", "write:/workspace/a", 7,
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
	manager := new(mockManager)
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

func TestTakeFD_ConcurrentSingleOwner(t *testing.T) {
	loopback := &Loopback{fds: make(map[uint64]fdVersion)}
	loopback.storeFD(7, fdVersion{autoID: 42, long: true})
	var owners int
	var mu sync.Mutex
	var wait sync.WaitGroup
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, ok := loopback.takeFD(7); ok {
				mu.Lock()
				owners++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if owners != 1 {
		t.Fatalf("Flush/Release 窗口终结权=%d,期望 1", owners)
	}
}
