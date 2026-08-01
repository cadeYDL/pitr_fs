package mount

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestJuiceFS_GCUsesNativeCompactDelete(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "juicefs")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	m := &JuiceFS{
		Binary: binary, MetaURL: "postgres://example",
		MountPoint: "/tmp/jfs", LogOutput: &output,
	}
	if err := m.GC(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(output.String())
	if got != "gc --delete --threads 7 postgres://example" {
		t.Fatalf("gc args=%q", got)
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
