package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"pitr_fs/internal/txn"
	"pitr_fs/internal/workspace"
)

func TestPrepareWorkspaceMountTargetRecoversDisconnectedFuse(t *testing.T) {
	mkdirCalls := 0
	unmountCalls := 0
	err := prepareWorkspaceMountTarget("/pitr/data", func(path string, mode os.FileMode) error {
		mkdirCalls++
		if path != "/pitr/data" || mode != 0o755 {
			t.Fatalf("mkdir(%q, %o)", path, mode)
		}
		if mkdirCalls == 1 {
			return &os.PathError{Op: "mkdir", Path: path, Err: unix.ENOTCONN}
		}
		return nil
	}, func(path string, flags int) error {
		unmountCalls++
		if path != "/pitr/data" || flags != unix.MNT_DETACH {
			t.Fatalf("unmount(%q, %d)", path, flags)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mkdirCalls != 2 || unmountCalls != 1 {
		t.Fatalf("mkdir=%d unmount=%d", mkdirCalls, unmountCalls)
	}
}

func TestPrepareWorkspaceMountTargetDoesNotUnmountOrdinaryFailure(t *testing.T) {
	unmountCalls := 0
	want := errors.New("permission denied")
	err := prepareWorkspaceMountTarget("/pitr/data", func(string, os.FileMode) error {
		return want
	}, func(string, int) error {
		unmountCalls++
		return nil
	})
	if !errors.Is(err, want) || unmountCalls != 0 {
		t.Fatalf("err=%v unmount=%d", err, unmountCalls)
	}
}

type fakeWorkspaceProxy struct {
	started bool
	stopped bool
	gate    bool
}

func (f *fakeWorkspaceProxy) Start() error            { f.started = true; return nil }
func (f *fakeWorkspaceProxy) Unmount() error          { f.stopped = true; return nil }
func (f *fakeWorkspaceProxy) UnmountLazy() error      { f.stopped = true; return nil }
func (f *fakeWorkspaceProxy) SetQuiescing(value bool) { f.gate = value }
func (f *fakeWorkspaceProxy) DiscardOpenWrites(context.Context) (int, error) {
	return 0, nil
}

type fakeBinder struct {
	bindings map[string]string
}

func (f *fakeBinder) Bind(source, target string) error {
	if f.bindings == nil {
		f.bindings = map[string]string{}
	}
	f.bindings[target] = source
	return nil
}
func (f *fakeBinder) Unmount(target string, _ bool) error {
	delete(f.bindings, target)
	return nil
}

func TestWorkspaceRuntimeSharesOneProxyAcrossMountAliases(t *testing.T) {
	temp := t.TempDir()
	jfsRoot := filepath.Join(temp, "jfs")
	runtimeRoot := filepath.Join(temp, "runtime")
	if err := os.MkdirAll(jfsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	binder := &fakeBinder{}
	created := 0
	var proxyInstance *fakeWorkspaceProxy
	runtime := NewWorkspaceRuntime(
		jfsRoot, runtimeRoot, (*txn.Manager)(nil),
		WithWorkspaceBinder(binder),
		WithWorkspaceProxyFactory(func(_, _ string, _ *txn.Manager) (WorkspaceProxy, error) {
			created++
			proxyInstance = &fakeWorkspaceProxy{}
			return proxyInstance, nil
		}),
	)
	item := workspace.Workspace{
		ID: 7, Name: "alpha", BackendPath: "/.pitr/workspaces/alpha",
	}
	for _, alias := range []string{
		filepath.Join(temp, "mount-a"), filepath.Join(temp, "mount-b"),
	} {
		if err := runtime.Mount(context.Background(), item, alias); err != nil {
			t.Fatal(err)
		}
	}
	if created != 1 || len(binder.bindings) != 2 || !proxyInstance.started {
		t.Fatalf("created=%d bindings=%v proxy=%+v", created, binder.bindings, proxyInstance)
	}
	if err := runtime.Umount(context.Background(), item, filepath.Join(temp, "mount-a"), false); err != nil {
		t.Fatal(err)
	}
	if proxyInstance.stopped || len(binder.bindings) != 1 {
		t.Fatalf("第一个别名卸载不应停止共享 proxy: %+v bindings=%v",
			proxyInstance, binder.bindings)
	}
	if err := runtime.Umount(context.Background(), item, filepath.Join(temp, "mount-b"), false); err != nil {
		t.Fatal(err)
	}
	if !proxyInstance.stopped || len(binder.bindings) != 0 {
		t.Fatalf("最后别名卸载应停止 proxy: %+v bindings=%v",
			proxyInstance, binder.bindings)
	}
}
