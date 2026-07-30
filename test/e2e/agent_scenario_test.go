package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"pitr_fs/sdk/go/pitr"
	"pitr_fs/test/testutil"
)

func TestE2E_AgentEditFlow(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "agent-edit-flow")
	ctx := testutil.Context(t)
	client := env.Client(t)

	testutil.WriteString(t, filepath.Join(host, "obsolete.txt"), "baseline")
	transaction, err := client.Begin(ctx, scope, pitr.WithMessage("agent edit"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 5; index++ {
		testutil.WriteString(t, filepath.Join(host, fmt.Sprintf("file-%d.txt", index)), "v1")
	}
	testutil.WriteString(t, filepath.Join(host, "file-2.txt"), "v2")
	testutil.WriteString(t, filepath.Join(host, "file-4.txt"), "v2")
	if err := os.Remove(filepath.Join(host, "obsolete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(ctx, "agent edit committed"); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ReadString(t, filepath.Join(host, "file-2.txt")); got != "v2" {
		t.Fatalf("file-2=%q", got)
	}
	if _, err := os.Stat(filepath.Join(host, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("obsolete.txt 应已删除: %v", err)
	}
	logs, err := client.Logs(ctx, scope, 20)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range logs {
		if entry.VersionHash == transaction.VersionHash() &&
			entry.State == "committed" &&
			entry.Message == "agent edit committed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs 未展示已提交版本 %s: %+v", transaction.VersionHash(), logs)
	}
}

func TestE2E_AgentRollback(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "agent-rollback")
	ctx := testutil.Context(t)
	client := env.Client(t)

	keep := filepath.Join(host, "keep.txt")
	deleted := filepath.Join(host, "deleted.txt")
	testutil.WriteString(t, keep, "before")
	testutil.WriteString(t, deleted, "restore-me")

	transaction, err := client.Begin(ctx, scope, pitr.WithMessage("agent failure"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteString(t, keep, "must-rollback")
	testutil.WriteString(t, filepath.Join(host, "new.txt"), "must-disappear")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ReadString(t, keep); got != "before" {
		t.Fatalf("keep.txt=%q", got)
	}
	if got := testutil.ReadString(t, deleted); got != "restore-me" {
		t.Fatalf("deleted.txt=%q", got)
	}
	if _, err := os.Stat(filepath.Join(host, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt 应消失: %v", err)
	}
}

func TestE2E_TimeTravel(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "time-travel")
	ctx := testutil.Context(t)
	client := env.Client(t)
	file := filepath.Join(host, "timeline.txt")

	versions := make([]*pitr.Txn, 0, 3)
	for index, content := range []string{"v1", "v2", "v3"} {
		transaction, err := client.Begin(
			ctx, scope, pitr.WithMessage(fmt.Sprintf("version %d", index+1)))
		if err != nil {
			t.Fatal(err)
		}
		testutil.WriteString(t, file, content)
		if err := transaction.Commit(ctx, content); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, transaction)
	}

	for _, target := range []struct {
		version *pitr.Txn
		content string
	}{
		{versions[0], "v1"},
		{versions[2], "v3"},
		{versions[1], "v2"},
	} {
		if _, err := client.Revert(
			ctx, target.version.VersionHash(), pitr.WithPath(scope)); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ReadString(t, file); got != target.content {
			t.Fatalf("revert %s 后内容=%q,期望 %q",
				target.version.VersionHash(), got, target.content)
		}
	}
}
