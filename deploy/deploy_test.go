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

// TestScripts_BashSyntax — 所有 Shell 入口必须过 `bash -n`
func TestScripts_BashSyntax(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"install.sh", "uninstall.sh", "scripts/install-deps.sh", "deploy/entrypoint.sh",
		"scripts/build-upgrade-bundle.sh", "scripts/pitr-host-upgrade.sh",
	} {
		p := filepath.Join(root, rel)
		out, err := exec.Command("bash", "-n", p).CombinedOutput()
		if err != nil {
			t.Errorf("%s bash -n 失败: %v\n%s", rel, err, out)
		}
	}
}

func TestUserFacingScriptsAreExecutable(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"install.sh", "uninstall.sh", "scripts/install-deps.sh",
		"scripts/build-upgrade-bundle.sh", "scripts/pitr-host-upgrade.sh",
	} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s 缺少可执行权限", rel)
		}
	}
}

func TestLogicUpgrade_IsHostControlledAndKeepsContainer(t *testing.T) {
	root := repoRoot(t)
	install, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`if [ "\${1:-}" = "upgrade" ]`,
		`PITR_CALLER_PWD="\${PWD:-}" "\$host_upgrader" "\$@"`,
		`source=$RUNTIME_DIR,target=/opt/pitr`,
		`install_host_upgrader`,
		`SAVED_RUNTIME_DIR=%q`,
		`SAVED_UPDATE_REPOSITORY=%q`,
		`pitr-host-upgrade-builtin`,
		`current/pitr-host-upgrade`,
		`prepare_schema_marker`,
	} {
		if !bytes.Contains(install, []byte(required)) {
			t.Errorf("install.sh 缺少逻辑升级片段 %q", required)
		}
	}

	upgrader, err := os.ReadFile(filepath.Join(root, "scripts/pitr-host-upgrade.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"警告: 升级会先停止 pitr 文件系统服务",
		"请确保当前没有任何写入操作",
		"非交互升级必须显式指定 --yes",
		`/run/pitr/discard-open-writes`,
		`--bundle PATH`,
		`pitr upgrade [版本]`,
		`download_release_bundle`,
		`release_asset_from_json`,
		`asset.get("digest") or ""`,
		`--proto '=https'`,
		`--retry-all-errors`,
		`--retry-max-time 60`,
		`download_output=(--progress-bar)`,
		`download_output=(--silent)`,
		`if [ -t 2 ]; then`,
		`==> 校验升级包`,
		`==> 切换逻辑版本并恢复挂载`,
		`PITR_UPDATE_REPOSITORY`,
		`pitr-host-upgrade init_pitr.sql`,
		`upgrade-fallback`,
		`request_restart`,
		`文件系统未能安全卸载，逻辑版本未切换`,
		`current_schema_digest`,
		`target_schema_digest`,
		`schema 内容未变化`,
		`record_schema_digest`,
		`ensure_safe_upgrade_cwd`,
		`preflight_target_runtime`,
		`升级已在停止服务前取消`,
		`psql --single-transaction`,
		`client_min_messages=warning`,
		`数据库升级已原子取消`,
	} {
		if !bytes.Contains(upgrader, []byte(required)) {
			t.Errorf("宿主升级器缺少 %q", required)
		}
	}
	if bytes.Contains(upgrader, []byte("docker rm")) ||
		bytes.Contains(upgrader, []byte("docker run")) {
		t.Error("逻辑升级器不应删除或重建容器")
	}
}

func TestLogicUpgradeRejectsCallerInsideFuseMount(t *testing.T) {
	root := repoRoot(t)
	temp := t.TempDir()
	config := filepath.Join(temp, "install.conf")
	mountRoot := "/pitr/data"
	if err := os.WriteFile(config,
		[]byte("SAVED_MOUNT_ROOT="+mountRoot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "pitr-host-upgrade.sh")
	command := exec.Command("bash", "-c", `
source "$1"
CALLER_PWD="$2/project"
MOUNT_ROOT="$2"
ensure_safe_upgrade_cwd 0
`, "bash", script, mountRoot)
	command.Env = append(os.Environ(), "PITR_INSTALL_CONFIG="+config)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("挂载目录内升级应在停服务前失败: %s", output)
	}
	for _, expected := range []string{
		"当前终端位于 pitr 管理的挂载目录范围中", "请先执行 cd /", mountRoot,
	} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Errorf("升级提示缺少 %q:\n%s", expected, output)
		}
	}

	command = exec.Command("bash", "-c", `
source "$1"
CALLER_PWD="$2/project"
MOUNT_ROOT="$2"
ensure_safe_upgrade_cwd 1
`, "bash", script, mountRoot)
	command.Env = append(os.Environ(), "PITR_INSTALL_CONFIG="+config)
	if output, err = command.CombinedOutput(); err != nil {
		t.Fatalf("--check 不会重挂载，应允许在挂载目录执行: %v\n%s", err, output)
	}
}

func TestLogicUpgradeSummarizesSchemaFailureForUsers(t *testing.T) {
	root := repoRoot(t)
	temp := t.TempDir()
	config := filepath.Join(temp, "install.conf")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "pitr-host-upgrade.sh")
	command := exec.Command("bash", "-c", `
source "$1"
docker_cli() {
  printf '%s\n' 'psql: ERROR: 重建 slice 索引时无法安全释放 12289/67108864 的 1 个旧 pin' >&2
  return 1
}
apply_schema
`, "bash", script)
	command.Env = append(os.Environ(), "PITR_INSTALL_CONFIG="+config)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("模拟 schema 失败应返回非零: %s", output)
	}
	for _, expected := range []string{
		"历史版本的数据引用索引不一致",
		"当前逻辑版本和文件数据没有切换",
		"PITR_UPGRADE_DEBUG=1",
	} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Errorf("schema 失败提示缺少 %q:\n%s", expected, output)
		}
	}
	if bytes.Contains(output, []byte("12289/67108864")) {
		t.Fatalf("默认错误不应向普通用户暴露底层 slice/pin 细节:\n%s", output)
	}
}

func TestLogicUpgrade_SelectsNewestPublishedReleaseIncludingPrerelease(t *testing.T) {
	root := repoRoot(t)
	temp := t.TempDir()
	config := filepath.Join(temp, "install.conf")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	metadata := filepath.Join(temp, "releases.json")
	fixture := `[
  {"tag_name":"v1.0.0","draft":false,"prerelease":false,
   "published_at":"2026-01-01T00:00:00Z","assets":[]},
  {"tag_name":"dev-new","draft":false,"prerelease":true,
   "published_at":"2026-08-02T00:00:00Z","assets":[
     {"name":"pitr-fs_dev-new_linux_arm64.tar.gz","state":"uploaded",
      "browser_download_url":"https://github.com/example/release/dev-new.tar.gz",
      "digest":"sha256:` + digest + `"}]},
  {"tag_name":"ignored-draft","draft":true,"prerelease":false,
   "published_at":"2027-01-01T00:00:00Z","assets":[]}
]`
	if err := os.WriteFile(metadata, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "pitr-host-upgrade.sh")
	command := exec.Command("bash", "-c", `
source "$1"
release_asset_from_json "$2" "" arm64
`, "bash", script, metadata)
	command.Env = append(os.Environ(), "PITR_INSTALL_CONFIG="+config)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("解析最新 Release: %v\n%s", err, output)
	}
	want := "dev-new\nhttps://github.com/example/release/dev-new.tar.gz\nsha256:" + digest + "\n"
	if string(output) != want {
		t.Fatalf("release info=%q want=%q", output, want)
	}
}

func TestLogicUpgrade_RejectsReleaseWithoutGitHubDigest(t *testing.T) {
	root := repoRoot(t)
	temp := t.TempDir()
	config := filepath.Join(temp, "install.conf")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(temp, "release.json")
	fixture := `{"tag_name":"dev-unsafe","draft":false,
"published_at":"2026-08-02T00:00:00Z","assets":[
{"name":"pitr-fs_dev-unsafe_linux_amd64.tar.gz","state":"uploaded",
"browser_download_url":"https://github.com/example/unsafe.tar.gz","digest":null}]}`
	if err := os.WriteFile(metadata, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "pitr-host-upgrade.sh")
	command := exec.Command("bash", "-c", `
source "$1"
release_asset_from_json "$2" dev-unsafe amd64
`, "bash", script, metadata)
	command.Env = append(os.Environ(), "PITR_INSTALL_CONFIG="+config)
	if output, err := command.CombinedOutput(); err == nil ||
		!bytes.Contains(output, []byte("缺少 GitHub SHA256 摘要")) {
		t.Fatalf("缺少摘要应失败: err=%v output=%s", err, output)
	}
}

func TestBuildUpgradeBundleSupportsLinuxArchitecturesAndSelfUpdate(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root,
		"scripts", "build-upgrade-bundle.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`PITR_GOARCH`, `GOOS=linux GOARCH="$goarch"`,
		`pitr-host-upgrade`, `goarch=%s`,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("升级包构建器缺少 %q", required)
		}
	}
}

func TestInstall_PrepareSchemaMarkerSupportsStoppedContainer(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\ninstall_main() {\n")
	index := bytes.Index(content, marker)
	if index < 0 {
		t.Fatal("install.sh 未找到主命令 case")
	}
	temp := t.TempDir()
	functions := filepath.Join(temp, "install-functions.sh")
	if err := os.WriteFile(functions, content[:index], 0o600); err != nil {
		t.Fatal(err)
	}
	fakeDocker := filepath.Join(temp, "docker")
	fake := `#!/usr/bin/env bash
case "$1" in
  inspect) exit 0 ;;
  exec) exit 1 ;;
  cp) printf 'stopped-container-schema\n' >"$3" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeDocker, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(temp, "runtime")
	if err := os.Mkdir(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `
source "$1"
DOCKER_COMMAND=("$2")
RUNTIME_DIR=$3
CONTAINER=stopped-pitrfs
prepare_schema_marker
test -s "$RUNTIME_DIR/schema.applied.sha256"
`, "bash", functions, fakeDocker, runtimeDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("stopped 容器 schema marker 回退失败: %v\n%s", err, output)
	}
}

func TestInstall_HostUpgradeDispatcherFollowsCurrentRuntime(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\ninstall_main() {\n")
	index := bytes.Index(content, marker)
	if index < 0 {
		t.Fatal("install.sh 未找到主命令 case")
	}
	temp := t.TempDir()
	functions := filepath.Join(temp, "install-functions.sh")
	if err := os.WriteFile(functions, content[:index], 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(temp, "runtime with space")
	versionDir := filepath.Join(runtimeDir, "versions", "dev-test")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	currentHelper := filepath.Join(versionDir, "pitr-host-upgrade")
	if err := os.WriteFile(currentHelper,
		[]byte("#!/bin/sh\nprintf 'current:%s\\n' \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("versions/dev-test", filepath.Join(runtimeDir, "current")); err != nil {
		t.Fatal(err)
	}
	dispatcher := filepath.Join(temp, "bin", "pitr-host-upgrade")
	if err := os.Mkdir(filepath.Dir(dispatcher), 0o755); err != nil {
		t.Fatal(err)
	}
	generate := exec.Command("bash", "-c", `
source "$1"
RUNTIME_DIR=$2
HOST_UPGRADER=$3
SCRIPT_DIR=$4
install_host_upgrader
`, "bash", functions, runtimeDir, dispatcher, root)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("生成宿主升级分发器: %v\n%s", err, output)
	}
	output, err := exec.Command(dispatcher, "v1.2.3").CombinedOutput()
	if err != nil {
		t.Fatalf("执行宿主升级分发器: %v\n%s", err, output)
	}
	if string(output) != "current:v1.2.3\n" {
		t.Fatalf("分发器未使用 current 升级器: %q", output)
	}
}

func TestLogicUpgrade_UsesLazyUnmountOnlyForUpgradeDiscard(t *testing.T) {
	root := repoRoot(t)
	handler, err := os.ReadFile(filepath.Join(root,
		"internal/server/handlers_lifecycle.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"upgradeDiscard :=",
		"s.cfg.ForceUmountFunc != nil",
		"umountFunc = s.cfg.ForceUmountFunc",
	} {
		if !bytes.Contains(handler, []byte(required)) {
			t.Errorf("升级卸载路径缺少 %q", required)
		}
	}
}

func TestEntrypoint_SupervisesLogicRestartWithoutStoppingPostgres(t *testing.T) {
	root := repoRoot(t)
	entrypoint, err := os.ReadFile(filepath.Join(root, "deploy/entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`printf '%s\n' "$PITRD_PID" >/run/pitr/pitrd.pid`,
		`if [ -e /run/pitr/restart.request ]; then`,
		"按升级请求重启 pitrd，PostgreSQL 保持运行",
		`if [ -r /opt/pitr/upgrade-fallback ]; then`,
		"自动切回",
		`schema.applied.sha256`,
		`apply_schema 0`,
		`apply_schema 1`,
		`psql --single-transaction`,
		`client_min_messages=warning`,
		`reconcile_database_collation`,
		`REINDEX DATABASE %I`,
		`REFRESH COLLATION VERSION`,
	} {
		if !bytes.Contains(entrypoint, []byte(required)) {
			t.Errorf("entrypoint 缺少无容器重建升级片段 %q", required)
		}
	}
}

// TestInstall_UsageCoversAllSubcommands — --help 输出必须列出全部子命令
func TestInstall_UsageCoversAllSubcommands(t *testing.T) {
	root := repoRoot(t)
	sh := filepath.Join(root, "install.sh")
	var out bytes.Buffer
	cmd := exec.Command("bash", sh, "--help")
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() // --help 返回 0 也可能非 0, 只看输出
	got := out.String()
	for _, sub := range []string{"install", "recover", "status", "logs"} {
		if !strings.Contains(got, sub) {
			t.Errorf("--help 输出缺子命令 %q\n完整输出:\n%s", sub, got)
		}
	}
	if strings.Contains(got, "install.sh uninstall") {
		t.Errorf("install.sh 不应再暴露卸载子命令\n完整输出:\n%s", got)
	}
	if !strings.Contains(got, "source ./uninstall.sh [--purge]") {
		t.Errorf("install.sh --help 未指向独立卸载脚本\n完整输出:\n%s", got)
	}
}

func TestInstall_SuccessOutputDoesNotSuggestInitOrWrites(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"下一步:", "echo hi > a.txt", "已有挂载已恢复"} {
		if bytes.Contains(content, []byte(forbidden)) {
			t.Errorf("install.sh 安装成功输出不应包含 %q", forbidden)
		}
	}
	if !bytes.Contains(content, []byte(`echo "  ✓ 服务安装完成"`)) {
		t.Error("install.sh 缺少唯一的安装成功提示")
	}
}

func TestUninstall_HelpRequiresSourceAndDocumentsPurge(t *testing.T) {
	root := repoRoot(t)
	sh := filepath.Join(root, "uninstall.sh")
	out, err := exec.Command("bash", "-c", `source "$1" --help`, "bash", sh).CombinedOutput()
	if err != nil {
		t.Fatalf("source uninstall.sh --help: %v\n%s", err, out)
	}
	for _, required := range []string{"source ./uninstall.sh", "--purge", "命令缓存"} {
		if !bytes.Contains(out, []byte(required)) {
			t.Errorf("uninstall.sh --help 缺少 %q\n完整输出:\n%s", required, out)
		}
	}
	out, err = exec.Command("bash", sh, "--help").CombinedOutput()
	if err == nil {
		t.Fatal("直接执行 uninstall.sh 应失败，必须要求 source")
	}
	if !bytes.Contains(out, []byte("source ./uninstall.sh")) {
		t.Errorf("直接执行时未给出 source 指引:\n%s", out)
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
		`host_mount_root=$quoted_root`,
		`pitr_args=("\$@")`,
		`host_pwd=\${PWD:-}`,
		`requested_workdir="\$host_pwd"`,
		`docker_args=(exec --workdir "\$host_mount_root")`,
		`while ! cd "\$container_workdir" 2>/dev/null; do`,
		`if [ -t 0 ] && [ -t 1 ]; then`,
		"docker_args+=(-it)",
		`docker_command=(docker)`,
		`docker_command=(sudo docker)`,
		`[ ! -x /opt/pitr/current/pitr ] || cli=/opt/pitr/current/pitr`,
		`exec "\$cli" "\$@"`,
		`' sh "\$requested_workdir" "\$host_mount_root" "\${pitr_args[@]}"`,
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
	marker := []byte("\ninstall_main() {\n")
	index := bytes.Index(content, marker)
	if index < 0 {
		t.Fatal("install.sh 未找到主命令 case")
	}
	temp := t.TempDir()
	functions := filepath.Join(temp, "install-functions.sh")
	if err := os.WriteFile(functions, content[:index], 0o600); err != nil {
		t.Fatal(err)
	}
	mountRoot := filepath.Join(temp, "mount root")
	subdir := filepath.Join(mountRoot, "project")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(temp, "pitr")
	generate := exec.Command("bash", "-c", `source "$1"; install_wrapper`, "bash", functions)
	generate.Env = append(os.Environ(),
		"PITR_MOUNT_ROOT="+mountRoot,
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
		"<exec>", "<--workdir>", "<" + mountRoot + ">", "<" + subdir + ">",
		"<test-container>", "<sh>",
		"/opt/pitr/current/pitr", "<begin>", "<.>",
	} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Errorf("wrapper 输出缺少 %q:\n%s", expected, output)
		}
	}
	if bytes.Contains(output, []byte("<-it>")) {
		t.Errorf("非 TTY wrapper 不应传 -it:\n%s", output)
	}

	command = exec.Command(wrapper, "init", ".")
	command.Dir = subdir
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("运行 init wrapper: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("<"+subdir+">")) {
		t.Fatalf("init 相对路径没有解析为宿主绝对路径:\n%s", output)
	}
}

func TestInstall_WrapperFallsBackWhenCWDWasDeleted(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\ninstall_main() {\n")
	index := bytes.Index(content, marker)
	if index < 0 {
		t.Fatal("install.sh 未找到主命令 case")
	}
	temp := t.TempDir()
	functions := filepath.Join(temp, "install-functions.sh")
	if err := os.WriteFile(functions, content[:index], 0o600); err != nil {
		t.Fatal(err)
	}
	mountRoot := filepath.Join(temp, "mount")
	staleDir := filepath.Join(mountRoot, "deleted-cwd")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(temp, "pitr")
	generate := exec.Command("bash", "-c", `source "$1"; install_wrapper`, "bash", functions)
	generate.Env = append(os.Environ(),
		"PITR_MOUNT_ROOT="+mountRoot,
		"PITR_BIN="+wrapper,
		"PITR_CONTAINER=test-container",
	)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("生成 wrapper: %v\n%s", err, output)
	}
	fakeCLI := filepath.Join(temp, "fake-pitr")
	fakeCLIContent := "#!/bin/sh\nprintf 'cwd=%s\\n' \"$(pwd -P)\"\nprintf 'arg=<%s>\\n' \"$@\"\n"
	if err := os.WriteFile(fakeCLI, []byte(fakeCLIContent), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapperContent, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	wrapperContent = bytes.ReplaceAll(wrapperContent,
		[]byte("/usr/local/bin/pitr"), []byte(fakeCLI))
	if err := os.WriteFile(wrapper, wrapperContent, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(temp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDocker := filepath.Join(bin, "docker")
	fakeDockerScript := `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  exec)
    shift
    test "$1" = --workdir
    initial_workdir=$2
    shift 2
    shift
    cd "$initial_workdir"
    exec "$@"
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `
cd "$1"
rmdir "$1"
exec "$2" version
`, "bash", staleDir, wrapper)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("已删除 cwd 中运行 wrapper: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"当前目录已不存在", "cwd=" + mountRoot, "arg=<version>",
	} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Errorf("wrapper 输出缺少 %q:\n%s", expected, output)
		}
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
		`docker_cli exec "$CONTAINER" test -S /var/run/pitrd.sock`,
		`docker_cli exec "$CONTAINER" pitr status >/dev/null 2>&1`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("wait_ready 缺少端到端就绪检查 %q", required)
		}
	}
}

func TestInstall_AuditCanResolveHostProcessAndUser(t *testing.T) {
	root := repoRoot(t)
	installContent, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--pid host",
		"source=/etc/passwd,target=/host/etc/passwd,readonly",
	} {
		if !bytes.Contains(installContent, []byte(required)) {
			t.Errorf("install.sh 缺少主机审计支持 %q", required)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "deploy/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dockerfile,
		[]byte(`ENTRYPOINT ["/usr/bin/tini", "-s", "--"`)) {
		t.Error("共享 PID namespace 后 tini 必须注册为 child subreaper")
	}
}

func TestInstall_UsesBoundedDedicatedJuiceFSCache(t *testing.T) {
	root := repoRoot(t)
	installContent, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`CACHE_VOLUME="${PITR_CACHE_VOLUME:-${SAVED_CACHE_VOLUME:-pitr_cache}}"`,
		`JFS_CACHE_SIZE="${PITR_JFS_CACHE_SIZE:-${SAVED_JFS_CACHE_SIZE:-1024}}"`,
		`CACHE_VOLUME_MANAGED="${SAVED_CACHE_VOLUME_MANAGED:-}"`,
		`docker_cli volume create "$CACHE_VOLUME"`,
		`SAVED_CACHE_VOLUME_MANAGED=%q`,
		`-e "PITR_JFS_CACHE_SIZE=$JFS_CACHE_SIZE"`,
		`-v "$CACHE_VOLUME:/var/jfsCache"`,
	} {
		if !bytes.Contains(installContent, []byte(required)) {
			t.Errorf("install.sh 缺少有界 JuiceFS 缓存配置 %q", required)
		}
	}
	entrypoint, err := os.ReadFile(filepath.Join(root, "deploy", "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entrypoint,
		[]byte(`--jfs-cache-size "${PITR_JFS_CACHE_SIZE:-1024}"`)) {
		t.Error("entrypoint 未把缓存上限传给 pitrd")
	}
	uninstall, err := os.ReadFile(filepath.Join(root, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(uninstall, []byte(`if [ "$CACHE_VOLUME_MANAGED" = "1" ]`)) ||
		!bytes.Contains(uninstall, []byte(`elif docker_cli volume inspect "$CACHE_VOLUME"`)) ||
		!bytes.Contains(uninstall, []byte(`docker_cli volume rm "$CACHE_VOLUME"`)) {
		t.Error("卸载脚本未清理临时缓存卷")
	}
}

func TestInstall_DetachesOnlyPitrFuseBeforeRecover(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, required := range []string{
		`for attempt in $(seq 1 8); do`,
		`findmnt -rn -t fuse.pitrfs -o TARGET`,
		`docker_cli_timeout 10 exec "$CONTAINER" fusermount3 -uz "$target"`,
		`fusermount3 -uz "$target"`,
		`pitr FUSE 层超过安全清理上限 8`,
		"detach_stale_fuse\n            docker_cli start",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh 缺少安全的失联 FUSE 恢复片段 %q", required)
		}
	}
	uninstallContent, err := os.ReadFile(filepath.Join(root, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	uninstall := string(uninstallContent)
	detach := strings.Index(uninstall, "detach_stale_fuse")
	remove := strings.Index(uninstall, `docker_cli_timeout 30 rm -f "$CONTAINER"`)
	if detach < 0 || remove < 0 || detach > remove {
		t.Error("uninstall.sh 必须先解除 FUSE，再删除容器")
	}
}

func TestInstall_DockerOperationsAreBounded(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, required := range []string{`timeout 10 docker info`, `sudo -n true`} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh 缺少有界 Docker/sudo 处理 %q", required)
		}
	}
	uninstall, err := os.ReadFile(filepath.Join(root, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(uninstall, []byte(`docker_cli_timeout 30 rm -f "$CONTAINER"`)) {
		t.Error("uninstall.sh 缺少有界容器停止")
	}
}

func TestUninstall_RefreshesParentShellCommandCache(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"pitr_uninstall_main() (", "hash -r", `source "$_pitr_uninstall_dir/install.sh"`} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("uninstall.sh 缺少父 Shell 缓存处理 %q", required)
		}
	}
}

func TestInstall_IsLinuxOnlyAndUsesGenericMountRoot(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`[ "$(uname -s)" = "Linux" ]`,
		`MOUNT_ROOT="${PITR_MOUNT_ROOT:-${SAVED_MOUNT_ROOT:-/pitr}}"`,
		`source=$MOUNT_ROOT,target=$MOUNT_ROOT,bind-propagation=rshared`,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("install.sh 缺少 Linux/动态挂载约束 %q", required)
		}
	}
}

func TestInstall_SupportsUserMountedBlockStorage(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`BLOCK_PATH="${PITR_BLOCK_PATH:-${SAVED_BLOCK_PATH:-}}"`,
		`type=bind,source=$BLOCK_PATH,target=/data`,
		`block_mount=(-v "$DATA_VOLUME:/data")`,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("install.sh 缺少块存储挂载语义 %q", required)
		}
	}
	uninstall, err := os.ReadFile(filepath.Join(root, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(uninstall, []byte(`用户块存储目录未删除`)) {
		t.Error("uninstall.sh 未说明用户块存储不会被删除")
	}
}

func TestEntrypoint_EnablesPinnedSliceLifecycleGC(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "deploy/entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--trash-days 0",
		`juicefs config --yes --trash-days 0 "$PG_DSN"`,
		`--gc-interval "${PITR_GC_INTERVAL:-10m}"`,
		`--gc-threads "${PITR_GC_THREADS:-4}"`,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("entrypoint 缺少生命周期配置 %q", required)
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
	if !strings.Contains(out.String(), "服务未安装或未运行") {
		t.Errorf("status 应输出'服务未安装或未运行', 实际:\n%s", out.String())
	}
}

func TestInstall_DoesNotReplaceExistingDocker(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "scripts/install-deps.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, required := range []string{
		`command -v docker >/dev/null 2>&1 || add_package docker.io`,
		`宿主机依赖均已存在，未替换或重装任何软件包`,
		`usermod -aG docker "$install_user"`,
		`systemctl enable --now docker.socket`,
		`安装前已有的 Docker，但 daemon 不可用`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("依赖脚本缺少已有 Docker 保护片段 %q", required)
		}
	}
}

func TestInstall_TracksAndRemovesOnlyManagedDependencies(t *testing.T) {
	root := repoRoot(t)
	deps, err := os.ReadFile(filepath.Join(root, "scripts/install-deps.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`STATE_FILE="$STATE_DIR/host-install.state"`,
		`state_append "package=$package"`,
		`state_append "docker_group_user=$install_user"`,
		`state_append "docker_group_created=1"`,
		`apt-get -s purge "${owned_packages[@]}"`,
		`run_root rpm -e "${owned_packages[@]}"`,
		`if [ "${1:-}" = "--uninstall" ]`,
		`docker_snapshot_before`,
		`docker_snapshot_after`,
		`Docker 中存在非 pitr-fs 管理的镜像`,
		`Docker 中存在非 pitr-fs 容器`,
		`systemctl stop docker.service docker.socket`,
		`run_root rm -f /run/docker.sock`,
		`run_root rm -rf -- /var/lib/docker /var/lib/containerd`,
	} {
		if !bytes.Contains(deps, []byte(required)) {
			t.Errorf("依赖脚本缺少可追踪清理片段 %q", required)
		}
	}

	install, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`bash "$SCRIPT_DIR/scripts/install-deps.sh"`,
		`--docker-snapshot-before`,
		`--docker-snapshot-after "$IMAGE"`,
	} {
		if !bytes.Contains(install, []byte(required)) {
			t.Errorf("install.sh 缺少一键环境管理片段 %q", required)
		}
	}
	uninstall, err := os.ReadFile(filepath.Join(root, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(uninstall, []byte(`bash "$SCRIPT_DIR/scripts/install-deps.sh" --uninstall`)) {
		t.Error("uninstall.sh 缺少托管依赖清理调用")
	}
}

func TestInstall_PersistsNonSecretInstallConfiguration(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`INSTALL_CONFIG="${PITR_INSTALL_CONFIG:-/etc/pitr-fs/install.conf}"`,
		`MOUNT_ROOT="${PITR_MOUNT_ROOT:-${SAVED_MOUNT_ROOT:-/pitr}}"`,
		`printf 'SAVED_MOUNT_ROOT=%q\n' "$MOUNT_ROOT"`,
		`write_install_config`,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("install.sh 缺少安装参数持久化片段 %q", required)
		}
	}
	if bytes.Contains(content, []byte("SAVED_AWS_SECRET_ACCESS_KEY")) ||
		bytes.Contains(content, []byte("SAVED_POSTGRES_PASSWORD")) {
		t.Error("安装配置不得持久化访问凭证或数据库密码")
	}
}

func TestDockerfileDoesNotBakeDatabasePassword(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "deploy/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("POSTGRES_PASSWORD=")) {
		t.Error("Dockerfile 不应把数据库密码写进镜像 ENV")
	}
}

func TestDockerfilePinsJuiceFSAndPostgreSQLABI(t *testing.T) {
	root := repoRoot(t)
	dockerfile, err := os.ReadFile(filepath.Join(root, "deploy/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(dockerfile)
	for _, required := range []string{
		"JUICEFS_VERSION=v1.3.0",
		"30190ca1094d26e85f19a979ca51b0ea19af1eaa",
		"git apply --check /tmp/juicefs.patch",
		"JUICEFS_PATCH_REVISION=pitrfs.1",
		"version.revision=${JUICEFS_PATCH_REVISION}-30190ca",
		"postgres:16.14-bookworm@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55",
		"golang:1.25@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Dockerfile 缺少固定运行时约束 %q", required)
		}
	}
	if strings.Contains(content, "d.juicefs.com/install") {
		t.Error("Dockerfile 不得通过安装脚本获取浮动 JuiceFS 版本")
	}

	entrypoint, err := os.ReadFile(filepath.Join(root, "deploy/entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entrypoint, []byte("mkdir -p /opt/pitr")) {
		t.Error("全新容器启动前必须创建运行时状态目录")
	}
	for _, required := range []string{
		`if juicefs status "$PG_DSN"`,
		`/usr/local/bin/pitrd`,
		`--check-compatibility`,
		`log "校准 MVCC schema..."`,
	} {
		if !bytes.Contains(entrypoint, []byte(required)) {
			t.Errorf("entrypoint 缺少 schema 变更前 ABI 校验 %q", required)
		}
	}
	if bytes.Index(entrypoint, []byte("--check-compatibility")) >
		bytes.Index(entrypoint, []byte(`log "校准 MVCC schema..."`)) {
		t.Error("已有卷 ABI 校验必须发生在 schema 校准之前")
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
