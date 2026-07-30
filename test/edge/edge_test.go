package edge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"pitr_fs/sdk/go/pitr"
	"pitr_fs/test/testutil"
)

func TestEdge_MmapWrite(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "mmap-write")
	ctx := testutil.Context(t)
	client := env.Client(t)
	file := filepath.Join(host, "mapped.txt")
	const baseline = "read-only-mmap"
	const changed = "write-via-mmap"
	testutil.WriteString(t, file, baseline)
	baselineVersion := latestAutoHash(t, client, ctx, scope)

	readFile, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	readMapping, err := unix.Mmap(
		int(readFile.Fd()), 0, len(baseline), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = readFile.Close()
		t.Fatalf("只读 mmap 应受支持: %v", err)
	}
	if string(readMapping) != baseline {
		t.Fatalf("只读 mmap 内容=%q", readMapping)
	}
	if err := unix.Munmap(readMapping); err != nil {
		t.Fatal(err)
	}
	if err := readFile.Close(); err != nil {
		t.Fatal(err)
	}

	writeFile, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeMapping, err := unix.Mmap(
		int(writeFile.Fd()), 0, len(baseline),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = writeFile.Close()
		t.Fatalf("当前 Linux/FUSE 应支持 writable mmap: %v", err)
	}
	copy(writeMapping, changed)
	if err := unix.Msync(writeMapping, unix.MS_SYNC); err != nil {
		t.Fatal(err)
	}
	if err := unix.Munmap(writeMapping); err != nil {
		t.Fatal(err)
	}
	if err := writeFile.Close(); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, file); got != changed {
		t.Fatalf("mmap 写入内容=%q", got)
	}
	if _, err := client.Revert(
		ctx, baselineVersion, pitr.WithPath(scope)); err != nil {
		t.Fatal(err)
	}
	relative := strings.TrimPrefix(scope, "/workspace")
	lowerContent := env.Docker(
		t, "exec", env.Container, "cat",
		path.Join("/var/lib/pitr/jfs", relative, "mapped.txt"))
	if lowerContent != baseline {
		t.Fatalf("mmap rollback 后底层内容=%q", lowerContent)
	}
	if got := testutil.ReadString(t, file); got != baseline {
		t.Fatalf("mmap 写入 rollback 后内容=%q", got)
	}
}

func TestEdge_RenameCrossScope(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "rename-cross-scope")
	ctx := testutil.Context(t)
	client := env.Client(t)
	sourceDir, destinationDir := filepath.Join(host, "src"), filepath.Join(host, "dst")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "value.txt")
	destination := filepath.Join(destinationDir, "value.txt")
	testutil.WriteString(t, source, "cross-scope")
	baselineVersion := latestAutoHash(t, client, ctx, scope)

	if err := os.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Revert(
		ctx, baselineVersion, pitr.WithPath(scope)); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, source); got != "cross-scope" {
		t.Fatalf("dst rollback 未恢复源文件: %q", got)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("dst rollback 后目标仍存在: %v", err)
	}
}

func TestEdge_DaemonCrashRestart(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "daemon-crash-restart")
	ctx := testutil.Context(t)
	client := env.Client(t)
	file := filepath.Join(host, "survive.txt")
	testutil.WriteString(t, file, "baseline")
	baselineVersion := latestAutoHash(t, client, ctx, scope)

	testutil.WriteString(t, file, "written-before-crash")
	env.Docker(t, "kill", "--signal=KILL", env.Container)
	env.DetachHostMount(t)
	env.Docker(t, "start", env.Container)
	env.WaitReady(t, 2*time.Minute)

	if got := waitReadString(t, file, 10*time.Second); got != "written-before-crash" {
		t.Fatalf("重启后数据不可读或内容错误: %q", got)
	}
	// grpc-go 会对重建后的 unix socket 自动重连；revert 证明关闭后的自动
	// 版本在 daemon 重启后仍然可用。
	if _, err := client.Revert(
		ctx, baselineVersion, pitr.WithPath(scope)); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, file); got != "baseline" {
		t.Fatalf("崩溃恢复后的 rollback 内容=%q", got)
	}
	mountPath := "/" + strings.Split(strings.Trim(env.ScopeRoot, "/"), "/")[0]
	volumes, err := client.Recover(ctx, mountPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 || !volumes[0].JFSMounted || !volumes[0].FUSEMounted {
		t.Fatalf("recover 状态=%+v", volumes)
	}
}

func TestEdge_LargeDirectoryRevert(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "large-directory")
	ctx := testutil.Context(t)
	client := env.Client(t)
	count := envInt(t, "PITR_E2E_LARGE_COUNT", 100000)
	maxSeconds := envInt(t, "PITR_E2E_LARGE_MAX_SECONDS", 30)

	relative, ok := strings.CutPrefix(scope, "/workspace")
	if !ok || strings.Contains(relative, "..") {
		t.Fatalf("非法测试 scope: %s", scope)
	}
	lower := path.Join("/var/lib/pitr/jfs", relative)
	seed := `set -eu
root=$1
count=$2
mkdir -p "$root/payload"
i=1
while [ "$i" -le "$count" ]; do
  : > "$root/payload/f_$i"
  i=$((i+1))
done`
	env.Docker(t, "exec", env.Container, "sh", "-c", seed, "seed", lower, strconv.Itoa(count))
	t.Cleanup(func() {
		_ = exec.Command(
			"docker", "exec", env.Container, "rm", "-rf", lower,
		).Run()
	})

	testutil.WriteString(t, filepath.Join(host, ".baseline"), "baseline")
	baselineVersion := latestAutoHash(t, client, ctx, scope)
	if err := os.Rename(
		filepath.Join(host, "payload"),
		filepath.Join(host, "payload-renamed"),
	); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	result, err := client.Revert(
		ctx, baselineVersion, pitr.WithPath(scope))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > time.Duration(maxSeconds)*time.Second {
		t.Fatalf("%d 文件目录 revert 耗时 %s,超过 %ds", count, elapsed, maxSeconds)
	}
	entries, err := os.ReadDir(filepath.Join(host, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != count {
		t.Fatalf("revert 后文件数=%d,期望 %d", len(entries), count)
	}
	if _, err := os.Stat(filepath.Join(host, "payload-renamed")); !os.IsNotExist(err) {
		t.Fatalf("revert 后 renamed 目录仍存在: %v", err)
	}
	t.Logf("large-directory count=%d applied=%d revert=%s", count, result.Applied, elapsed)
}

func TestEdge_UnicodeFilename(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "unicode-filename")
	ctx := testutil.Context(t)
	client := env.Client(t)
	names := []string{"你好，世界.txt", "résumé-Δ.md", "emoji-🧪.json"}
	testutil.WriteString(t, filepath.Join(host, ".baseline"), "baseline")
	baselineVersion := latestAutoHash(t, client, ctx, scope)

	for index, name := range names {
		testutil.WriteString(t, filepath.Join(host, name), fmt.Sprintf("内容-%d", index))
	}
	if _, err := client.Revert(
		ctx, baselineVersion, pitr.WithPath(scope)); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(host, name)); !os.IsNotExist(err) {
			t.Fatalf("rollback 后 Unicode 文件 %q 仍存在: %v", name, err)
		}
	}
}

func waitReadString(t testing.TB, file string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(file)
		if err == nil {
			return string(content)
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待读取 %s: %v", file, lastErr)
	return ""
}

func latestAutoHash(
	t testing.TB,
	client *pitr.Client,
	ctx context.Context,
	scope string,
) string {
	t.Helper()
	logs, err := client.Logs(ctx, scope, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range logs {
		if entry.State == "auto" {
			return entry.VersionHash
		}
	}
	t.Fatalf("scope %s 没有自动版本: %+v", scope, logs)
	return ""
}

func envInt(t testing.TB, name string, fallback int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		t.Fatalf("%s 必须是正整数,实际 %q", name, value)
	}
	return parsed
}
