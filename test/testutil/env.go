// Package testutil 提供真实 pitrd/FUSE 端到端测试共用的环境约束。
package testutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pitr_fs/sdk/go/pitr"
)

type Env struct {
	Socket          string
	HostRoot        string
	ScopeRoot       string
	MountRoot       string
	Container       string
	ContainerSocket string
}

func Load(t testing.TB) Env {
	t.Helper()
	env := Env{
		Socket:          os.Getenv("PITR_E2E_SOCKET"),
		HostRoot:        os.Getenv("PITR_E2E_HOST_PATH"),
		ScopeRoot:       os.Getenv("PITR_E2E_SCOPE"),
		MountRoot:       os.Getenv("PITR_E2E_MOUNT_ROOT"),
		Container:       os.Getenv("PITR_E2E_CONTAINER"),
		ContainerSocket: os.Getenv("PITR_E2E_CONTAINER_SOCKET"),
	}
	if env.ContainerSocket == "" {
		env.ContainerSocket = env.Socket
	}
	if env.Socket == "" || env.HostRoot == "" || env.ScopeRoot == "" {
		t.Skip("未设置 PITR_E2E_SOCKET/PITR_E2E_HOST_PATH/PITR_E2E_SCOPE")
	}
	if !filepath.IsAbs(env.Socket) || !filepath.IsAbs(env.HostRoot) ||
		!path.IsAbs(env.ScopeRoot) {
		t.Fatalf("E2E 路径必须为绝对路径: %+v", env)
	}
	if env.Container != "" {
		if output, err := exec.Command(
			"docker", "exec", env.Container,
			"chmod", "666", env.ContainerSocket,
		).CombinedOutput(); err != nil {
			t.Fatalf("设置 E2E socket 权限: %v\n%s", err, output)
		}
	}
	return env
}

// DetachHostMount 清理 daemon 非优雅退出后由 rshared 传播到测试 VM 的失联
// FUSE 挂载。只允许操作显式传入的绝对测试挂载点。
func (e Env) DetachHostMount(t testing.TB) {
	t.Helper()
	if !filepath.IsAbs(e.MountRoot) || e.MountRoot == "/" {
		t.Fatalf("PITR_E2E_MOUNT_ROOT 必须是非根绝对路径,实际 %q", e.MountRoot)
	}
	if err := exec.Command("fusermount3", "-uz", e.MountRoot).Run(); err == nil {
		return
	}
	output, err := exec.Command("sudo", "umount", "-l", e.MountRoot).CombinedOutput()
	if err != nil {
		t.Fatalf("卸载失联传播挂载 %s: %v\n%s", e.MountRoot, err, output)
	}
}

func (e Env) Client(t testing.TB) *pitr.Client {
	t.Helper()
	client, err := pitr.Dial(e.Socket)
	if err != nil {
		t.Fatalf("连接 pitrd: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Scenario 返回测试专属的宿主机路径和 daemon 可见 scope。测试名只用于
// 生成隔离目录，不允许路径分隔符进入测试数据。
func (e Env) Scenario(t testing.TB, name string) (string, string) {
	t.Helper()
	name = strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(name))
	host := filepath.Join(e.HostRoot, name)
	scope := path.Join(e.ScopeRoot, name)
	if err := os.RemoveAll(host); err != nil {
		t.Fatalf("清理场景目录 %s: %v", host, err)
	}
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = os.MkdirAll(host, 0o755)
		if err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("等待传播挂载可写并创建场景目录 %s: %v", host, err)
	}
	t.Cleanup(func() {
		var cleanupErr error
		for attempt := 0; attempt < 20; attempt++ {
			cleanupErr = os.RemoveAll(host)
			if cleanupErr == nil {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		if cleanupErr != nil {
			t.Logf("清理场景目录 %s: %v", host, cleanupErr)
		}
	})
	return host, scope
}

func Context(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func ReadString(t testing.TB, file string) string {
	t.Helper()
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取 %s: %v", file, err)
	}
	return string(content)
}

func WriteString(t testing.TB, file, content string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s: %v", file, err)
	}
}

func (e Env) Docker(t testing.TB, args ...string) string {
	t.Helper()
	if e.Container == "" {
		t.Skip("该测试需要 PITR_E2E_CONTAINER")
	}
	command := exec.Command("docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func (e Env) WaitReady(t testing.TB, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		command := exec.Command(
			"docker", "exec", e.Container, "sh", "-c",
			"test -S \"$1\" && pitr --socket \"$1\" status >/dev/null && mountpoint -q /workspace",
			"wait-ready", e.ContainerSocket,
		)
		if output, err := command.CombinedOutput(); err == nil {
			_ = exec.Command(
				"docker", "exec", e.Container,
				"chmod", "666", e.ContainerSocket,
			).Run()
			hostMounted := e.MountRoot == ""
			if !hostMounted {
				hostMounted = exec.Command("mountpoint", "-q", e.MountRoot).Run() == nil
			}
			if _, err := os.Stat(e.Socket); err == nil && hostMounted {
				probe := filepath.Join(e.HostRoot, ".pitr-ready")
				if err := os.MkdirAll(e.HostRoot, 0o755); err == nil {
					if err := os.WriteFile(probe, []byte("ready"), 0o600); err == nil {
						_ = os.Remove(probe)
						return
					}
				}
			}
		} else {
			last = fmt.Sprintf("%v: %s", err, output)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("pitrd/FUSE 在 %s 内未恢复就绪: %s", timeout, last)
}
