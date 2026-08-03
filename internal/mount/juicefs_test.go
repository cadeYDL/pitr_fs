package mount

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	m = &JuiceFS{
		MetaURL: "postgres://example", MountPoint: "/tmp/x", CacheSizeMiB: -1,
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("负数 cache size 应失败")
	}
}

func TestJuiceFS_MountArgsLimitCache(t *testing.T) {
	m := &JuiceFS{
		MetaURL: "postgres://example", MountPoint: "/tmp/jfs",
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(m.mountArgs(), " ")
	if !strings.Contains(args, "--cache-size 1024") {
		t.Fatalf("默认挂载参数未限制缓存: %s", args)
	}
	m.CacheSizeMiB = 256
	args = strings.Join(m.mountArgs(), " ")
	if !strings.Contains(args, "--cache-size 256") {
		t.Fatalf("自定义缓存未生效: %s", args)
	}
}

func TestJuiceFS_StopBeforeStart(t *testing.T) {
	m := &JuiceFS{MountPoint: filepath.Join(t.TempDir(), "jfs")}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJuiceFS_StartFailureKillsReapsAndResetsProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("JuiceFS mount process cleanup 仅在 Linux 验证")
	}
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	binary := filepath.Join(root, "juicefs")
	script := "#!/bin/sh\ntrap '' TERM\nprintf '%s' $$ >\"$PITR_TEST_PID\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PITR_TEST_PID", pidFile)
	m := &JuiceFS{
		Binary: binary, MetaURL: "postgres://example",
		MountPoint: filepath.Join(root, "mount"), LogOutput: new(bytes.Buffer),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx); err == nil {
		t.Fatal("取消 Start 应返回错误")
	}
	content, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("失败 Start 遗留进程 pid=%d err=%v", pid, err)
	}
	if m.cmd != nil || m.done != nil || m.managed {
		t.Fatalf("失败 Start 未重置状态: cmd=%v done=%v managed=%v",
			m.cmd, m.done, m.managed)
	}
}
