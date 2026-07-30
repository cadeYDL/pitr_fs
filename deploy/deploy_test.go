// Package deploy — 部署脚本的静态与端到端测试。
//
// 这里的测试不需要 docker/网络, 仅用 `bash -n` 校验语法 + 运行 install.sh 的
// 只读子命令(usage / 未知子命令 exit 1)。
// 真正跑 install.sh 拉起容器的端到端验证见 e2e_test.go(需要 docker,
// -tags=e2e 才编译, 默认跳过)。
package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot — 定位项目根,任何测试都能拿到 install.sh / deploy/*
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestScripts_BashSyntax — install.sh 与 entrypoint.sh 必须过 `bash -n`
func TestScripts_BashSyntax(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"install.sh", "deploy/entrypoint.sh"} {
		p := filepath.Join(root, rel)
		out, err := exec.Command("bash", "-n", p).CombinedOutput()
		if err != nil {
			t.Errorf("%s bash -n 失败: %v\n%s", rel, err, out)
		}
	}
}

// TestInstall_UsageCoversAllSubcommands — --help 输出必须列 4 个子命令
func TestInstall_UsageCoversAllSubcommands(t *testing.T) {
	root := repoRoot(t)
	sh := filepath.Join(root, "install.sh")
	var out bytes.Buffer
	cmd := exec.Command("bash", sh, "--help")
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() // --help 返回 0 也可能非 0, 只看输出
	got := out.String()
	for _, sub := range []string{"install", "recover", "uninstall", "status"} {
		if !strings.Contains(got, sub) {
			t.Errorf("--help 输出缺子命令 %q\n完整输出:\n%s", sub, got)
		}
	}
}

// TestInstall_UnknownSubcommandFails — 未知子命令应 exit 非 0
func TestInstall_UnknownSubcommandFails(t *testing.T) {
	root := repoRoot(t)
	sh := filepath.Join(root, "install.sh")
	cmd := exec.Command("bash", sh, "not-a-real-subcommand")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("未知子命令应报错并 exit 非 0, 实际 exit 0")
	}
}

// TestInstall_WrapperSupportsNonTTY — wrapper 在终端中保留交互体验，在脚本和
// CI 的非 TTY 环境中不得强制 docker exec -it。
func TestInstall_WrapperSupportsNonTTY(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, required := range []string{
		`host_workspace=$quoted_workspace`,
		`container_workdir="/workspace/\${PWD#"\$host_workspace"/}"`,
		`docker_args=(exec --workdir "\$container_workdir")`,
		`if [ -t 0 ] && [ -t 1 ]; then`,
		"docker_args+=(-it)",
		`exec docker "\${docker_args[@]}" "$CONTAINER" pitr "\$@"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh wrapper 缺少非 TTY 兼容片段 %q", required)
		}
	}
	if strings.Contains(script, `exec docker exec -it "$CONTAINER"`) {
		t.Error("install.sh wrapper 仍无条件强制 docker exec -it")
	}
}

func TestInstall_WrapperMapsHostCWD(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\ncase \"${1:-install}\" in\n")
	index := bytes.Index(content, marker)
	if index < 0 {
		t.Fatal("install.sh 未找到主命令 case")
	}
	temp := t.TempDir()
	functions := filepath.Join(temp, "install-functions.sh")
	if err := os.WriteFile(functions, content[:index], 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(temp, "workspace with space")
	subdir := filepath.Join(workspace, "project")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(temp, "pitr")
	generate := exec.Command("bash", "-c", `source "$1"; install_wrapper`, "bash", functions)
	generate.Env = append(os.Environ(),
		"PITR_WORKSPACE="+workspace,
		"PITR_BIN="+wrapper,
		"PITR_CONTAINER=test-container",
	)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("生成 wrapper: %v\n%s", err, output)
	}
	if output, err := exec.Command("bash", "-n", wrapper).CombinedOutput(); err != nil {
		t.Fatalf("生成的 wrapper 语法错误: %v\n%s", err, output)
	}

	bin := filepath.Join(temp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDocker := filepath.Join(bin, "docker")
	if err := os.WriteFile(fakeDocker,
		[]byte("#!/bin/sh\nprintf '<%s>\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper, "begin", ".")
	command.Dir = subdir
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("运行 wrapper: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"<exec>", "<--workdir>", "</workspace/project>",
		"<test-container>", "<pitr>", "<begin>", "<.>",
	} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Errorf("wrapper 输出缺少 %q:\n%s", expected, output)
		}
	}
	if bytes.Contains(output, []byte("<-it>")) {
		t.Errorf("非 TTY wrapper 不应传 -it:\n%s", output)
	}
}

func TestInstall_ReadyRequiresSuccessfulRPC(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, required := range []string{
		`docker exec "$CONTAINER" test -S /var/run/pitrd.sock`,
		`docker exec "$CONTAINER" pitr status >/dev/null 2>&1`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("wait_ready 缺少端到端就绪检查 %q", required)
		}
	}
}

// TestInstall_StatusOnMissingContainer — status 在容器不存在时输出"容器未运行"
// 需要 docker 才能跑; 无 docker 直接跳过
func TestInstall_StatusOnMissingContainer(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("无 docker, 跳过")
	}
	root := repoRoot(t)
	sh := filepath.Join(root, "install.sh")
	// 用一个几乎不可能存在的容器名, 避免污染真环境
	env := append(os.Environ(), "PITR_CONTAINER=pitrfs-test-nonexistent-name-xyz")
	var out bytes.Buffer
	cmd := exec.Command("bash", sh, "status")
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	if !strings.Contains(out.String(), "容器未运行") {
		t.Errorf("status 应输出'容器未运行', 实际:\n%s", out.String())
	}
}

func TestEntrypoint_GracefullyStopsPostgres(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "deploy/entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, required := range []string{
		"trap shutdown TERM INT",
		"kill -TERM \"$PITRD_PID\"",
		"pg_ctl -D \"$PGDATA\" -m fast -w stop",
		"exit \"$PITRD_RC\"",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("entrypoint 缺少优雅关闭片段 %q", required)
		}
	}
}
