package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// shortSock — 短路径 sock, 避开 macOS 103-byte sun_path 限制
func shortSock(t *testing.T, base string) string {
	t.Helper()
	// 用 /tmp 保证短, PID+纳秒足够唯一
	p := filepath.Join("/tmp", fmt.Sprintf("pitrd-%s-%d-%d.sock", base, os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

// TestServe_CreatesSocketAndBlocks — P1 骨架: 建 socket + 阻塞, ctx 取消后返回
func TestServe_CreatesSocketAndBlocks(t *testing.T) {
	sock := shortSock(t, "create")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- serveSocket(ctx, sock, grpc.NewServer()) }()

	waitCtx, wCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wCancel()
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("serve 提前返回: %v", err)
		case <-waitCtx.Done():
			t.Fatalf("2s 内未看到 socket 建立: %s", sock)
		case <-time.After(30 * time.Millisecond):
		}
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	_ = conn.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve 应返回 nil, 实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx cancel 后 3s 内 serve 未返回")
	}
}

// TestServe_EmptySocketRejected — 空 socket 应报错
func TestServe_EmptySocketRejected(t *testing.T) {
	if err := serveSocket(context.Background(), "", grpc.NewServer()); err == nil {
		t.Fatal("空 socket 应报错")
	}
}

// TestServe_StaleSocketReplaced — 之前残留的 socket 文件不阻碍启动
func TestServe_StaleSocketReplaced(t *testing.T) {
	sock := shortSock(t, "stale")
	if err := os.WriteFile(sock, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- serveSocket(ctx, sock, grpc.NewServer()) }()

	// 等到能 dial(即残留被替换、监听已就绪)
	deadline := time.Now().Add(2 * time.Second)
	dialed := false
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("serve 提前返回: %v", err)
		default:
		}
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			dialed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-errCh
	if !dialed {
		t.Fatal("残留 socket 场景下 dial 未成功")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("退出后 socket 应清理,stat err=%v", err)
	}
}

func TestPathInsideMountRoot(t *testing.T) {
	for _, item := range []struct {
		candidate string
		root      string
		want      bool
	}{
		{"/pitr/data", "/pitr", true},
		{"/pitr/team/data", "/pitr/", true},
		{"/pitr", "/pitr", false},
		{"/pitr-other/data", "/pitr", false},
		{"data", "/pitr", false},
		{"/pitr/data", "/", false},
	} {
		if got := pathInsideMountRoot(item.candidate, item.root); got != item.want {
			t.Errorf("pathInsideMountRoot(%q,%q)=%v, want %v",
				item.candidate, item.root, got, item.want)
		}
	}
}
