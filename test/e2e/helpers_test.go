package e2e

import (
	"context"
	"testing"

	"pitr_fs/sdk/go/pitr"
)

func latestAutoHash(
	t testing.TB,
	client *pitr.Client,
	ctx context.Context,
	scope string,
) string {
	t.Helper()
	logs, err := client.Logs(ctx, scope, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range logs {
		if entry.State == "auto" {
			return entry.VersionHash
		}
	}
	t.Fatalf("scope %s 没有自动版本: %+v", scope, logs)
	return ""
}
