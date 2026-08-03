package workspace

import (
	"context"
	"fmt"
	"os"
	"testing"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/schema"
)

func testCatalog(t *testing.T) (*Catalog, *pg.DB) {
	t.Helper()
	dsn := os.Getenv("PITR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 PITR_TEST_PG_DSN")
	}
	db, err := pg.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(context.Background(), `
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), schema.InitSQL); err != nil {
		t.Fatal(err)
	}
	return NewCatalog(db), db
}

func TestCatalog_WorkspaceOwnsOneRevisionLineAndManyMounts(t *testing.T) {
	catalog, db := testCatalog(t)
	ctx := context.Background()

	alpha, err := catalog.Ensure(ctx, "alpha", "default")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Name != "alpha" || alpha.BackendPath != "/.pitr/workspaces/alpha" {
		t.Fatalf("workspace=%+v", alpha)
	}
	if _, err := catalog.Ensure(ctx, "alpha", "default"); err != nil {
		t.Fatalf("Ensure 应幂等: %v", err)
	}
	for _, mount := range []string{"/pitr/alpha", "/pitr/alpha-copy"} {
		if err := catalog.AddMount(ctx, alpha.ID, mount); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := catalog.ResolveMount(ctx, "/pitr/alpha-copy/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Workspace.ID != alpha.ID || resolved.Scope != "/src/main.go" {
		t.Fatalf("resolved=%+v", resolved)
	}

	var roots, configs int64
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM pitr_txn
		 WHERE workspace_id=$1 AND state='root'`, alpha.ID).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM pitr_config
		 WHERE workspace_id=$1 AND scope_path='/'`, alpha.ID).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if roots != 1 || configs != 1 {
		t.Fatalf("roots=%d configs=%d", roots, configs)
	}
}

func TestCatalog_RejectsOverlappingMounts(t *testing.T) {
	catalog, _ := testCatalog(t)
	ctx := context.Background()
	alpha, err := catalog.Ensure(ctx, "alpha", "default")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := catalog.Ensure(ctx, "beta", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.AddMount(ctx, alpha.ID, "/pitr/team"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.AddMount(ctx, beta.ID, "/pitr/team/sub"); err == nil {
		t.Fatal("父子挂载点会使路径路由含糊，应拒绝")
	}
	if _, err := catalog.GetByName(ctx, "missing"); err == nil {
		t.Fatal("不存在的 workspace 应返回错误")
	}
	_ = fmt.Sprint(beta)
}
