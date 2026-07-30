package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pitr_fs/sdk/go/pitr"
	"pitr_fs/test/testutil"
)

func TestE2E_AgentEditFlow(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "agent-edit-flow")
	ctx := testutil.Context(t)
	client := env.Client(t)

	testutil.WriteString(t, filepath.Join(host, "obsolete.txt"), "baseline")
	for index := 1; index <= 5; index++ {
		testutil.WriteString(t, filepath.Join(host, fmt.Sprintf("file-%d.txt", index)), "v1")
	}
	testutil.WriteString(t, filepath.Join(host, "file-2.txt"), "v2")
	testutil.WriteString(t, filepath.Join(host, "file-4.txt"), "v2")
	if err := os.Remove(filepath.Join(host, "obsolete.txt")); err != nil {
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
	var automatic int
	for _, entry := range logs {
		if entry.State == "auto" {
			automatic++
		}
	}
	if automatic < 8 {
		t.Fatalf("自动版本数=%d，期望至少 8: %+v", automatic, logs)
	}
}

func TestE2E_RevertAtCompletedVersion(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "revert-at")
	ctx := testutil.Context(t)
	client := env.Client(t)
	file := filepath.Join(host, "timeline.txt")

	testutil.WriteString(t, file, "v1")
	var target time.Time
	for attempt := 0; attempt < 20; attempt++ {
		logs, err := client.Logs(ctx, scope, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range logs {
			if entry.State == "auto" && entry.ClosedAt != nil {
				target = *entry.ClosedAt
				break
			}
		}
		if !target.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if target.IsZero() {
		t.Fatal("未找到已完成 baseline 版本")
	}

	testutil.WriteString(t, file, "v2")
	if _, err := client.RevertAt(
		ctx, target, pitr.WithPath(scope)); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadString(t, file); got != "v1" {
		t.Fatalf("按时间回滚后=%q", got)
	}
}

func TestE2E_AgentRevert(t *testing.T) {
	env := testutil.Load(t)
	host, scope := env.Scenario(t, "agent-rollback")
	ctx := testutil.Context(t)
	client := env.Client(t)

	keep := filepath.Join(host, "keep.txt")
	deleted := filepath.Join(host, "deleted.txt")
	testutil.WriteString(t, keep, "before")
	testutil.WriteString(t, deleted, "restore-me")
	baseline := latestAutoHash(t, client, ctx, scope)

	testutil.WriteString(t, keep, "must-rollback")
	testutil.WriteString(t, filepath.Join(host, "new.txt"), "must-disappear")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Revert(ctx, baseline, pitr.WithPath(scope)); err != nil {
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

	versions := make([]string, 0, 3)
	for _, content := range []string{"v1", "v2", "v3"} {
		testutil.WriteString(t, file, content)
		versions = append(versions, latestAutoHash(t, client, ctx, scope))
	}

	for _, target := range []struct {
		version string
		content string
	}{
		{versions[0], "v1"},
		{versions[2], "v3"},
		{versions[1], "v2"},
	} {
		if _, err := client.Revert(
			ctx, target.version, pitr.WithPath(scope)); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ReadString(t, file); got != target.content {
			t.Fatalf("revert %s 后内容=%q,期望 %q",
				target.version, got, target.content)
		}
	}
}
