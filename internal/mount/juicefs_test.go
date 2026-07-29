package mount

import (
	"context"
	"path/filepath"
	"testing"
)

func TestIsMountPoint(t *testing.T) {
	mounted, err := IsMountPoint("/")
	if err != nil {
		t.Fatal(err)
	}
	if !mounted {
		t.Fatal("/ 应是 mountpoint")
	}
	mounted, err = IsMountPoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if mounted {
		t.Fatal("普通临时目录不应是 mountpoint")
	}
}

func TestJuiceFS_Validation(t *testing.T) {
	m := &JuiceFS{MetaURL: "", MountPoint: "/tmp/x"}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("空 MetaURL 应失败")
	}
	m = &JuiceFS{MetaURL: "postgres://example", MountPoint: "relative"}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("相对 mountpoint 应失败")
	}
}

func TestJuiceFS_StopBeforeStart(t *testing.T) {
	m := &JuiceFS{MountPoint: filepath.Join(t.TempDir(), "jfs")}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
