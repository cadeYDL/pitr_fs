package txn

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"pitr_fs/internal/pg"
)

func TestStandaloneVersionPersistsAuditMetadata(t *testing.T) {
	mgr, _ := setupManager(t)
	ctx := context.Background()
	id, err := mgr.OpenStandaloneVersion(
		ctx,
		"/workspace/project/file",
		"write:/workspace/project/file",
		VersionMetadata{
			PosixOp:        `open("/workspace/project/file", O_WRONLY)`,
			ProcessCommand: "echo hi...",
			ActorUID:       1000,
			ActorGID:       1001,
			ActorPID:       42,
			ActorName:      "tester",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.CloseStandaloneVersion(
		ctx, id,
		`write("/workspace/project/file", offset=0, total=2, calls=1)`,
		`"v1" -> "v2"`,
	); err != nil {
		t.Fatal(err)
	}
	found, err := mgr.FindByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	byHash, err := mgr.FindByHash(ctx, found.VersionHash)
	if err != nil {
		t.Fatal(err)
	}
	if byHash.PosixOp !=
		`write("/workspace/project/file", offset=0, total=2, calls=1)` ||
		byHash.ProcessCommand != "echo hi..." ||
		byHash.ActorUID != 1000 || byHash.ActorGID != 1001 ||
		byHash.ActorPID != 42 || byHash.ActorName != "tester" ||
		byHash.ChangeSummary != `"v1" -> "v2"` ||
		byHash.ClosedAt == nil {
		t.Fatalf("audit metadata=%+v", byHash)
	}
}

func TestFindClosedAtOrBeforeUsesNearestCompleteVersion(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	ids := make([]int64, 3)
	for index := range ids {
		id, err := mgr.OpenStandaloneVersion(
			ctx, fmt.Sprintf("/workspace/file%d", index),
			fmt.Sprintf("write:%d", index), VersionMetadata{})
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.CloseStandaloneVersion(ctx, id, "", ""); err != nil {
			t.Fatal(err)
		}
		ids[index] = id
	}
	t1 := time.Date(2020, 1, 1, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	if _, err := db.Exec(ctx, `
		UPDATE pitr_txn
		   SET closed_at=CASE
		     WHEN id=ANY($1::bigint[]) THEN $2::timestamptz
		     ELSE $3::timestamptz
		   END
		 WHERE id=ANY($4::bigint[])`,
		ids[:2], t1, t2, ids); err != nil {
		t.Fatal(err)
	}
	found, err := mgr.FindClosedAtOrBefore(ctx, t1.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != ids[1] {
		t.Fatalf("相同 closed_at 应选择较新 id: got=%d want=%d",
			found.ID, ids[1])
	}
	found, err = mgr.FindClosedAtOrBefore(ctx, t2)
	if err != nil || found.ID != ids[2] {
		t.Fatalf("精确时间 got=%+v err=%v", found, err)
	}
	if _, err := mgr.FindClosedAtOrBefore(
		ctx, t1.Add(-time.Nanosecond)); !errors.Is(err, ErrTimeBeforeHistory) {
		t.Fatalf("过早时间 err=%v", err)
	}
}

func TestFindClosedAtOrBeforeRootOnly(t *testing.T) {
	mgr, _ := setupManager(t)
	found, err := mgr.FindClosedAtOrBefore(
		context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if found.State != StateRoot {
		t.Fatalf("root-only result=%+v", found)
	}
}

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
			ctx, "/workspace/project/file", fmt.Sprintf("write:%d", index),
			VersionMetadata{})
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
		if err := mgr.CloseStandaloneVersion(ctx, versionID, "", ""); err != nil {
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
		ctx, "/workspace/project/file", "write", VersionMetadata{})
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
