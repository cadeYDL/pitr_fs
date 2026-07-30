package e2e

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"pitr_fs/sdk/go/pitr"
	"pitr_fs/test/testutil"
)

func TestE2E_TwoActiveTxnsInDifferentScopes(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "two-active")
	ctx := testutil.Context(t)
	client := env.Client(t)
	hostA, hostB := filepath.Join(host, "a"), filepath.Join(host, "b")
	if err := os.MkdirAll(hostA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hostB, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.WriteString(t, filepath.Join(hostA, "value.txt"), "a0")
	testutil.WriteString(t, filepath.Join(hostB, "value.txt"), "b0")

	txnA, err := client.Begin(ctx, path.Join(scope, "a"), pitr.WithMessage("scope a"))
	if err != nil {
		t.Fatal(err)
	}
	txnB, err := client.Begin(ctx, path.Join(scope, "b"), pitr.WithMessage("scope b"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteString(t, filepath.Join(hostA, "value.txt"), "a1")
	testutil.WriteString(t, filepath.Join(hostB, "value.txt"), "b1")

	if err := txnA.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, filepath.Join(hostA, "value.txt")); got != "a0" {
		t.Fatalf("scope a rollback 后=%q", got)
	}
	if got := testutil.ReadString(t, filepath.Join(hostB, "value.txt")); got != "b1" {
		t.Fatalf("scope b 被 scope a rollback 污染=%q", got)
	}
	testutil.WriteString(t, filepath.Join(hostB, "second.txt"), "still-active")
	if err := txnB.Commit(ctx, "scope b committed"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, filepath.Join(hostB, "value.txt")); got != "b1" {
		t.Fatalf("scope b commit 后=%q", got)
	}
}
