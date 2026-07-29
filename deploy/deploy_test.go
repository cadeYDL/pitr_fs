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
