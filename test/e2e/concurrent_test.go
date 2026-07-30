package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"pitr_fs/sdk/go/pitr"
	"pitr_fs/test/testutil"
)

func TestE2E_DirectoryRevertDoesNotAffectSibling(t *testing.T) {
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
	versionA := latestAutoHash(t, client, ctx, scope)

	testutil.WriteString(t, filepath.Join(hostA, "value.txt"), "a1")
	testutil.WriteString(t, filepath.Join(hostB, "value.txt"), "b1")

	if _, err := client.Revert(
		ctx, versionA, pitr.WithPath(filepath.Join(scope, "a"))); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, filepath.Join(hostA, "value.txt")); got != "a0" {
		t.Fatalf("scope a rollback 后=%q", got)
	}
	if got := testutil.ReadString(t, filepath.Join(hostB, "value.txt")); got != "b1" {
		t.Fatalf("scope b 被 scope a rollback 污染=%q", got)
	}
	if got := testutil.ReadString(t, filepath.Join(hostB, "value.txt")); got != "b1" {
		t.Fatalf("scope b 被 scope a revert 污染=%q", got)
	}
}
