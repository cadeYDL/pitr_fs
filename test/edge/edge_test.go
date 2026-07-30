package edge

import (
	"errors"
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

func TestEdge_MmapPolicy(t *testing.T) {
	env := testutil.Load(t)
	host, _ := env.Scenario(t, "mmap-policy")
	file := filepath.Join(host, "mapped.txt")
	testutil.WriteString(t, file, "read-only-mmap")

	readFile, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	readMapping, err := unix.Mmap(
		int(readFile.Fd()), 0, len("read-only-mmap"), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = readFile.Close()
		t.Fatalf("只读 mmap 应受支持: %v", err)
	}
	if string(readMapping) != "read-only-mmap" {
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
	defer writeFile.Close()
	writeMapping, err := unix.Mmap(
		int(writeFile.Fd()), 0, len("read-only-mmap"),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err == nil {
		_ = unix.Munmap(writeMapping)
		t.Fatal("direct-I/O 可写句柄不应允许 writable mmap")
	}
	if !errors.Is(err, unix.ENODEV) &&
		!errors.Is(err, unix.EINVAL) &&
		!errors.Is(err, unix.EOPNOTSUPP) {
		t.Fatalf("writable mmap 应以明确的不支持错误失败,实际: %v", err)
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

	sourceTxn, err := client.Begin(ctx, path.Join(scope, "src"))
	if err != nil {
		t.Fatal(err)
	}
	destinationTxn, err := client.Begin(ctx, path.Join(scope, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	if err := destinationTxn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, source); got != "cross-scope" {
		t.Fatalf("dst rollback 未恢复源文件: %q", got)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("dst rollback 后目标仍存在: %v", err)
	}
	if err := sourceTxn.Rollback(ctx); err != nil {
		t.Fatalf("src 事务不应捕获 rename: %v", err)
	}
}

func TestEdge_DaemonCrashRestart(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "daemon-crash-restart")
	ctx := testutil.Context(t)
	client := env.Client(t)
	file := filepath.Join(host, "survive.txt")
	testutil.WriteString(t, file, "baseline")

	transaction, err := client.Begin(ctx, scope, pitr.WithMessage("crash window"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteString(t, file, "written-before-crash")
	env.Docker(t, "kill", "--signal=KILL", env.Container)
	env.Docker(t, "start", env.Container)
	env.WaitReady(t, 2*time.Minute)

	if got := testutil.ReadString(t, file); got != "written-before-crash" {
		t.Fatalf("重启后数据不可读或内容错误: %q", got)
	}
	// grpc-go 会对重建后的 unix socket 自动重连;rollback 同时证明 dangling
	// auto 已在 daemon 启动阶段关闭,active 事务仍可安全收口。
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, file); got != "baseline" {
		t.Fatalf("崩溃恢复后的 rollback 内容=%q", got)
	}
	volumes, err := client.Recover(ctx, scope[:len(env.ScopeRoot)])
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

	relative, ok := strings.CutPrefix(scope, env.ScopeRoot)
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

	baseline, err := client.Begin(ctx, scope, pitr.WithMessage("large baseline"))
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Commit(ctx, "large baseline"); err != nil {
		t.Fatal(err)
	}
	rename, err := client.Begin(ctx, scope, pitr.WithMessage("large rename"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(host, "payload"),
		filepath.Join(host, "payload-renamed"),
	); err != nil {
		t.Fatal(err)
	}
	if err := rename.Commit(ctx, "large rename"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	result, err := client.Revert(
		ctx, baseline.VersionHash(), pitr.WithPath(scope))
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

	transaction, err := client.Begin(ctx, scope, pitr.WithMessage("unicode"))
	if err != nil {
		t.Fatal(err)
	}
	for index, name := range names {
		testutil.WriteString(t, filepath.Join(host, name), fmt.Sprintf("内容-%d", index))
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(host, name)); !os.IsNotExist(err) {
			t.Fatalf("rollback 后 Unicode 文件 %q 仍存在: %v", name, err)
		}
	}
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
