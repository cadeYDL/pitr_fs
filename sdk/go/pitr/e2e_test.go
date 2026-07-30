package pitr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGoSDK_RealPitrd_E2E(t *testing.T) {
	socket := os.Getenv("PITR_E2E_SOCKET")
	hostPath := os.Getenv("PITR_E2E_HOST_PATH")
	scope := os.Getenv("PITR_E2E_SCOPE")
	if socket == "" || hostPath == "" || scope == "" {
		t.Skip("未设置 PITR_E2E_SOCKET/PITR_E2E_HOST_PATH/PITR_E2E_SCOPE")
	}
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	filePath := filepath.Join(hostPath, "file.txt")

	v1, err := client.Begin(ctx, scope, WithMessage("go-sdk-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("go-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v1.Commit(ctx, "go-sdk-v1"); err != nil {
		t.Fatal(err)
	}

	v2, err := client.Begin(ctx, scope, WithMessage("go-sdk-v2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("go-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v2.Commit(ctx, "go-sdk-v2"); err != nil {
		t.Fatal(err)
	}
	diff, err := client.Diff(ctx, v1.VersionHash(), v2.VersionHash(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if diff.NodeChanges == 0 || diff.ChunkChanges == 0 {
		t.Fatalf("diff=%+v", diff)
	}
	if _, err := client.Revert(ctx, v1.VersionHash(), WithPath(scope)); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filePath); err != nil || string(content) != "go-v1" {
		t.Fatalf("revert content=%q err=%v", content, err)
	}

	rollback, err := client.Begin(ctx, scope, WithMessage("go-sdk-rollback"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("must-rollback"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rollback.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filePath); err != nil || string(content) != "go-v1" {
		t.Fatalf("rollback content=%q err=%v", content, err)
	}
	logs, err := client.Logs(ctx, scope, 20)
	if err != nil || len(logs) < 4 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
}
