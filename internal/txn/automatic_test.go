package txn

import (
	"context"
	"fmt"
	"testing"

	"pitr_fs/internal/pg"
)

func TestAutomaticHistoryLimitPersistenceAndInheritance(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()

	limit, err := mgr.HistoryLimit(ctx, "/workspace/project")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 100 {
		t.Fatalf("default history limit=%d", limit)
	}
	if _, err := mgr.SetHistoryLimit(ctx, "/", 7); err != nil {
		t.Fatal(err)
	}
	// 新 Manager 证明配置来自数据库，而不是进程内状态。
	persisted, err := NewManager(db).HistoryLimit(ctx, "/workspace/project")
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 7 {
		t.Fatalf("persisted history limit=%d", persisted)
	}

	// 控制面当前只开放全局配置；直接插入目录配置验证未来继承数据模型。
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_config(scope_path,history_limit)
		VALUES ('/workspace/team',3)`); err != nil {
		t.Fatal(err)
	}
	child, err := mgr.HistoryLimit(ctx, "/workspace/team/repo/sub")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := mgr.HistoryLimit(ctx, "/workspace/other")
	if err != nil {
		t.Fatal(err)
	}
	if child != 3 || sibling != 7 {
		t.Fatalf("inheritance child=%d sibling=%d", child, sibling)
	}
}

func TestAutomaticVersionsPruneAndClear(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_node (inode,mode,nlink,length)
		VALUES (700,33188,1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SetHistoryLimit(ctx, "/", 3); err != nil {
		t.Fatal(err)
	}

	for index := int64(1); index <= 5; index++ {
		versionID, err := mgr.OpenStandaloneVersion(
			ctx, "/workspace/project/file", fmt.Sprintf("write:%d", index))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.InTx(ctx, func(txDB pg.Tx) error {
			if err := txDB.SetLocal(
				ctx, "pitr.current_txn", fmt.Sprint(versionID)); err != nil {
				return err
			}
			_, err := txDB.Exec(ctx,
				"UPDATE jfs_node SET length=$1 WHERE inode=700", index)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := mgr.CloseStandaloneVersion(ctx, versionID); err != nil {
			t.Fatal(err)
		}
	}

	var versions, history int64
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE state<>'root'").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_node_history").Scan(&history); err != nil {
		t.Fatal(err)
	}
	if versions != 3 || history != 3 {
		t.Fatalf("pruned versions=%d history=%d", versions, history)
	}

	stats, err := mgr.ClearHistory(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if stats.VersionsDeleted != 3 || stats.HistoryDeleted != 3 {
		t.Fatalf("clear stats=%+v", stats)
	}
	var length, rootCount, configLimit int64
	if err := db.QueryRow(ctx,
		"SELECT length FROM jfs_node WHERE inode=700").Scan(&length); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE state='root'").Scan(&rootCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT history_limit FROM pitr_config WHERE scope_path='/'").
		Scan(&configLimit); err != nil {
		t.Fatal(err)
	}
	if length != 5 || rootCount != 1 || configLimit != 3 {
		t.Fatalf("clear length=%d roots=%d limit=%d",
			length, rootCount, configLimit)
	}
}

func TestAutomaticClearRejectsOpenWriteAndAbortRestores(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_node (inode,mode,nlink,length)
		VALUES (800,33188,1,10)`); err != nil {
		t.Fatal(err)
	}
	versionID, err := mgr.OpenStandaloneVersion(
		ctx, "/workspace/project/file", "write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "UPDATE jfs_node SET length=20 WHERE inode=800"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ClearHistory(ctx, "/"); err == nil {
		t.Fatal("开放写窗口存在时 clear 应失败")
	}
	if err := mgr.AbortAutoVersion(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	var length, versions int64
	if err := db.QueryRow(ctx,
		"SELECT length FROM jfs_node WHERE inode=800").Scan(&length); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE state<>'root'").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if length != 10 || versions != 0 {
		t.Fatalf("abort length=%d versions=%d", length, versions)
	}
}
