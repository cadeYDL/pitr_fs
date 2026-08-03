package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"pitr_fs/internal/workspace"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/schema"
)

func encodedSlice(id uint64, size uint32) []byte {
	value := make([]byte, 24)
	binary.BigEndian.PutUint64(value[4:12], id)
	binary.BigEndian.PutUint32(value[12:16], size)
	binary.BigEndian.PutUint32(value[20:24], size)
	return value
}

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

func TestWorkspaceManagersKeepVersionLinesAndPoliciesIndependent(t *testing.T) {
	defaultManager, db := setupManager(t)
	ctx := context.Background()
	alpha, err := workspace.NewCatalog(db).Ensure(ctx, "alpha", "default")
	if err != nil {
		t.Fatal(err)
	}
	alphaManager := defaultManager.ForWorkspace(alpha.ID)

	defaultID, err := defaultManager.OpenStandaloneVersion(
		ctx, "/same/file", "write:default", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := defaultManager.CloseStandaloneVersion(ctx, defaultID, "write", ""); err != nil {
		t.Fatal(err)
	}
	alphaID, err := alphaManager.OpenStandaloneVersion(
		ctx, "/same/file", "write:alpha", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := alphaManager.CloseStandaloneVersion(ctx, alphaID, "write", ""); err != nil {
		t.Fatal(err)
	}

	defaultLogs, err := defaultManager.List(ctx, "/", 10)
	if err != nil {
		t.Fatal(err)
	}
	alphaLogs, err := alphaManager.List(ctx, "/", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultLogs) != 1 || defaultLogs[0].Command != "write:default" {
		t.Fatalf("default logs=%+v", defaultLogs)
	}
	if len(alphaLogs) != 1 || alphaLogs[0].Command != "write:alpha" {
		t.Fatalf("alpha logs=%+v", alphaLogs)
	}
	if defaultLogs[0].WorkspaceID == alphaLogs[0].WorkspaceID {
		t.Fatalf("workspace id 未隔离: default=%d alpha=%d",
			defaultLogs[0].WorkspaceID, alphaLogs[0].WorkspaceID)
	}
	if _, err := alphaManager.SetHistoryLimit(ctx, "/", 7); err != nil {
		t.Fatal(err)
	}
	defaultLimit, err := defaultManager.HistoryLimit(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	alphaLimit, err := alphaManager.HistoryLimit(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if defaultLimit != 100 || alphaLimit != 7 {
		t.Fatalf("limits default=%d alpha=%d", defaultLimit, alphaLimit)
	}
}

func TestWorkspaceConcurrentWindowFailsBusyWithoutCrossAttribution(t *testing.T) {
	defaultManager, db := setupManager(t)
	ctx := context.Background()
	alpha, err := workspace.NewCatalog(db).Ensure(ctx, "alpha", "default")
	if err != nil {
		t.Fatal(err)
	}
	defaultID, err := defaultManager.OpenStandaloneVersion(
		ctx, "/file-a", "write:default", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaultManager.ForWorkspace(alpha.ID).OpenStandaloneVersion(
		ctx, "/file-b", "write:alpha", VersionMetadata{}); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("并发 workspace 写应快速返回 busy，实际 err=%v", err)
	}
	if err := defaultManager.CloseStandaloneVersion(ctx, defaultID, "write", ""); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateStandaloneVersionScopeOnlyWidens(t *testing.T) {
	mgr, _ := setupManager(t)
	ctx := context.Background()
	id, err := mgr.OpenStandaloneVersion(
		ctx, "/workspace/project/.file.swp", "open-write", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateStandaloneVersionScope(
		ctx, id, "/workspace/project"); err != nil {
		t.Fatal(err)
	}
	found, err := mgr.FindByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if found.ScopePath != "/workspace/project" {
		t.Fatalf("scope=%q", found.ScopePath)
	}
	if err := mgr.UpdateStandaloneVersionScope(
		ctx, id, "/workspace/project/narrow"); !errors.Is(err, ErrIllegalTransit) {
		t.Fatalf("缩小 scope 应失败,实际 %v", err)
	}
	if err := mgr.UpdateStandaloneVersionScope(ctx, id, "/"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CloseStandaloneVersion(ctx, id, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateStandaloneVersionScope(
		ctx, id, "/"); !errors.Is(err, ErrIllegalTransit) {
		t.Fatalf("关闭后更新 scope 应失败,实际 %v", err)
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

func TestAutomaticHistoryLimitUnlimitedAndNoArtificialUpperBound(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()

	if _, err := mgr.SetHistoryLimit(ctx, "/", 100001); err != nil {
		t.Fatalf("大于旧上限的正整数应被接受: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_txn(version_hash,scope_path,state,command,closed_at)
		VALUES ('limit0000001','/','auto','test',now()),
		       ('limit0000002','/','auto','test',now()),
		       ('limit0000003','/','auto','test',now())`); err != nil {
		t.Fatal(err)
	}
	if pruned, err := mgr.SetHistoryLimit(ctx, "/", -1); err != nil {
		t.Fatal(err)
	} else if pruned != 0 {
		t.Fatalf("unlimited pruned=%d", pruned)
	}
	limit, err := mgr.HistoryLimit(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	var versions int64
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE state<>'root'").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if limit != -1 || versions != 3 {
		t.Fatalf("limit=%d versions=%d", limit, versions)
	}
	if _, err := mgr.SetHistoryLimit(ctx, "/", 0); err == nil {
		t.Fatal("0 应被拒绝")
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

func TestSlicePinsReleasedAndGCQueueIsDurable(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (101,4096,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_chunk(inode,indx,slices) VALUES (900,0,$1)",
		encodedSlice(101, 4096)); err != nil {
		t.Fatal(err)
	}
	versionID, err := mgr.OpenStandaloneVersion(
		ctx, "/workspace/file", "write", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(versionID)); err != nil {
			return err
		}
		if _, err := txDB.Exec(ctx,
			"UPDATE jfs_chunk SET slices=$1 WHERE inode=900 AND indx=0",
			encodedSlice(0, 0)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx,
			"UPDATE jfs_chunk_ref SET refs=refs-1 WHERE chunkid=101")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CloseStandaloneVersion(ctx, versionID, "", ""); err != nil {
		t.Fatal(err)
	}
	var refs, pins, delayed, logical int64
	if err := db.QueryRow(ctx,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=101").Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT pins FROM pitr_slice_ref WHERE chunkid=101").Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM jfs_delslices WHERE chunkid>=8000000000000000000").
		Scan(&delayed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		SELECT (snapshot->>'refs')::bigint
		  FROM pitr_chunk_ref_history
		 WHERE txn_id=$1 AND chunkid=101`, versionID).Scan(&logical); err != nil {
		t.Fatal(err)
	}
	if refs != 1 || pins != 1 || delayed != 1 || logical != 1 {
		t.Fatalf("pinned refs=%d pins=%d delayed=%d logical=%d",
			refs, pins, delayed, logical)
	}

	if _, err := mgr.ClearHistory(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=101").Scan(&refs); err != nil {
		t.Fatal(err)
	}
	var pinRows, queueRows, dueRows int64
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_slice_ref").Scan(&pinRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_gc_queue").Scan(&queueRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM jfs_delslices
		 WHERE chunkid>=8000000000000000000
		   AND deleted<253402300799`).Scan(&dueRows); err != nil {
		t.Fatal(err)
	}
	if refs != 0 || pinRows != 0 || queueRows != 1 || dueRows != 1 {
		t.Fatalf("released refs=%d pins=%d queue=%d due=%d",
			refs, pinRows, queueRows, dueRows)
	}

	called := 0
	ran, err := mgr.RunPendingGC(ctx, func(context.Context) error {
		called++
		return nil
	})
	if err != nil || !ran || called != 1 {
		t.Fatalf("RunPendingGC ran=%v called=%d err=%v", ran, called, err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_gc_queue").Scan(&queueRows); err != nil {
		t.Fatal(err)
	}
	if queueRows != 0 {
		t.Fatalf("成功 GC 后 queue=%d", queueRows)
	}
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM jfs_delslices
		 WHERE chunkid>=8000000000000000000`).Scan(&dueRows); err != nil {
		t.Fatal(err)
	}
	if dueRows != 0 {
		t.Fatalf("成功 GC 后合成 delslices=%d", dueRows)
	}
}

func TestPendingGCDefersWhileWriteIsOpen(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	versionID, err := mgr.OpenStandaloneVersion(
		ctx, "/workspace/file", "write", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO pitr_gc_queue(singleton) VALUES (true)"); err != nil {
		t.Fatal(err)
	}
	called := false
	ran, err := mgr.RunPendingGC(ctx, func(context.Context) error {
		called = true
		return nil
	})
	if ran || called || !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("ran=%v called=%v err=%v", ran, called, err)
	}
	if err := mgr.CloseStandaloneVersion(ctx, versionID, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStandaloneVersionFailsFastWhileMaintenanceOwnsLock(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	locked := make(chan struct{})
	release := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- db.WithAdvisoryLock(
			ctx, "pitr-fs:versions", func() error {
				close(locked)
				<-release
				return nil
			})
	}()
	<-locked

	writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := mgr.OpenStandaloneVersion(
		writeCtx, "/workspace/maintenance", "write", VersionMetadata{})
	elapsed := time.Since(started)
	close(release)
	if maintenanceErr := <-maintenanceDone; maintenanceErr != nil {
		t.Fatal(maintenanceErr)
	}
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("维护期间错误=%v,期望 ErrMaintenanceBusy", err)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("维护期间新写等待过久:%s", elapsed)
	}
}

func TestPruningContinuesFromPersistentQueueInBoundedBatches(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		id, err := mgr.OpenStandaloneVersion(
			ctx, fmt.Sprintf("/workspace/%d", i), "write", VersionMetadata{})
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.CloseStandaloneVersion(ctx, id, "write", "change"); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := mgr.SetHistoryLimit(ctx, "/", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("前台裁剪=%d,期望固定为 1 个版本", pruned)
	}
	versions, err := mgr.List(ctx, "/", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) <= 1 {
		t.Fatalf("前台不应同步清空全部积压版本:len=%d", len(versions))
	}

	// 模拟 daemon 重启：队列必须完全由数据库状态驱动。
	restarted := NewManager(db)
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, runErr := restarted.RunPendingPrune(ctx, "/", 2)
		if runErr != nil {
			t.Fatal(runErr)
		}
		if !pending {
			break
		}
	}
	versions, err = restarted.List(ctx, "/", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("后台裁剪后版本数=%d,期望 1", len(versions))
	}
}

func TestSpacePressureWithoutHistoryDoesNotSpinPruneQueue(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (199,100,1)`); err != nil {
		t.Fatal(err)
	}
	pruned, err := mgr.SetSpacePolicy(ctx, "/", 100, 20)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Fatalf("没有历史版本时不应裁剪,pruned=%d", pruned)
	}
	var queued bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pitr_prune_queue WHERE singleton)`).
		Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued {
		t.Fatal("当前数据本身超限时不应留下无法完成的裁剪任务")
	}
	if _, pending, err := mgr.RunPendingPrune(ctx, "/", 2); err != nil || pending {
		t.Fatalf("空裁剪队列结果 pending=%v err=%v", pending, err)
	}
}

func TestSliceIndexUpgradeRebuildsLegacyAndCleanupState(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (201,8192,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_chunk(inode,indx,slices) VALUES (901,0,$1)",
		encodedSlice(201, 8192)); err != nil {
		t.Fatal(err)
	}
	versionID, err := mgr.OpenStandaloneVersion(
		ctx, "/workspace/legacy", "write", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(versionID)); err != nil {
			return err
		}
		if _, err := txDB.Exec(ctx,
			"UPDATE jfs_chunk SET slices=$1 WHERE inode=901 AND indx=0",
			encodedSlice(0, 0)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx,
			"UPDATE jfs_chunk_ref SET refs=refs-1 WHERE chunkid=201")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CloseStandaloneVersion(ctx, versionID, "", ""); err != nil {
		t.Fatal(err)
	}

	// 模拟旧版本留下了 history，但还没有 slice 索引。先去掉当前测试 schema
	// 自动建立的 pin，使 jfs refs 恢复为纯逻辑值。
	if _, err := db.Exec(ctx, `
		UPDATE jfs_chunk_ref SET refs=refs-1 WHERE chunkid=201;
		DELETE FROM jfs_delslices WHERE chunkid>=8000000000000000000;
		DELETE FROM pitr_slice_pin;
		DELETE FROM pitr_slice_ref;
		DELETE FROM pitr_slice_index_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("升级旧卷重建 slice 索引: %v", err)
	}
	var refs, pins, through int64
	var indexedAt, cleanupAt *time.Time
	if err := db.QueryRow(ctx, `
		SELECT r.refs,p.pins,s.indexed_through_txn_id,
		       s.indexed_at,s.last_version_cleanup_at
		  FROM jfs_chunk_ref r
		  JOIN pitr_slice_ref p ON p.chunkid=r.chunkid
		  CROSS JOIN pitr_slice_index_state s
		 WHERE r.chunkid=201`).Scan(
		&refs, &pins, &through, &indexedAt, &cleanupAt); err != nil {
		t.Fatal(err)
	}
	if refs != 1 || pins != 1 || through < versionID || indexedAt == nil || cleanupAt != nil {
		t.Fatalf("legacy rebuild refs=%d pins=%d through=%d indexed=%v cleanup=%v",
			refs, pins, through, indexedAt, cleanupAt)
	}

	// 幂等重放不能重复增加引用。
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("幂等重放 schema: %v", err)
	}
	if err := db.QueryRow(ctx,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=201").Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 1 {
		t.Fatalf("幂等重放后 refs=%d，期望 1", refs)
	}

	// 索引后删除版本必须记录 cleanup 水位；下次升级只能全量重建，并把
	// 新索引时间推进到 cleanup 之后。
	if _, err := db.Exec(ctx, "DELETE FROM pitr_txn WHERE id=$1", versionID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		SELECT indexed_at,last_version_cleanup_at
		  FROM pitr_slice_index_state WHERE singleton`).Scan(
		&indexedAt, &cleanupAt); err != nil {
		t.Fatal(err)
	}
	if indexedAt == nil || cleanupAt == nil || !cleanupAt.After(*indexedAt) {
		t.Fatalf("cleanup 水位未晚于索引: indexed=%v cleanup=%v", indexedAt, cleanupAt)
	}
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("cleanup 后全量重建: %v", err)
	}
	if err := db.QueryRow(ctx, `
		SELECT r.refs,s.indexed_at,s.last_version_cleanup_at
		  FROM jfs_chunk_ref r CROSS JOIN pitr_slice_index_state s
		 WHERE r.chunkid=201`).Scan(&refs, &indexedAt, &cleanupAt); err != nil {
		t.Fatal(err)
	}
	if refs != 0 || indexedAt == nil || cleanupAt == nil || indexedAt.Before(*cleanupAt) {
		t.Fatalf("cleanup rebuild refs=%d indexed=%v cleanup=%v",
			refs, indexedAt, cleanupAt)
	}
}

func TestSpacePolicyPrunesOldestByMarginalSliceBytes(t *testing.T) {
	_, db := setupManager(t)
	mgr := NewManager(db)
	ctx := context.Background()
	var rootID, oldestID, newestID int64
	if err := db.QueryRow(ctx,
		"SELECT id FROM pitr_txn WHERE state='root'").Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn(
			version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('spaceold001', $1, '/workspace/file', 'auto', 'write', now())
		RETURNING id`, rootID).Scan(&oldestID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn(
			version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('spacenew001', $1, '/workspace/file', 'auto', 'write', now())
		RETURNING id`, oldestID).Scan(&newestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_chunk_ref(chunkid,size,refs)
		VALUES (301,60,0),(302,30,0)`); err != nil {
		t.Fatal(err)
	}
	oldSlices := append(encodedSlice(301, 60), encodedSlice(302, 30)...)
	if _, err := db.Exec(ctx,
		"SELECT pitr_pin_chunk_slices($1,$2)", oldestID, oldSlices); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		"SELECT pitr_pin_chunk_slices($1,$2)", newestID,
		encodedSlice(301, 60)); err != nil {
		t.Fatal(err)
	}

	policy, err := mgr.SpacePolicy(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if policy.RetainedBytes != 90 || policy.ReclaimableBytes != 0 {
		t.Fatalf("pin 后空间 retained=%d reclaimable=%d",
			policy.RetainedBytes, policy.ReclaimableBytes)
	}
	pruned, err := mgr.SetSpacePolicy(ctx, "/", 100, 20)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned=%d，期望只删除最老版本", pruned)
	}
	var oldestRows, newestRows, sharedRefs, exclusiveRefs int64
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE id=$1", oldestID).Scan(&oldestRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE id=$1", newestID).Scan(&newestRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=301").Scan(&sharedRefs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=302").Scan(&exclusiveRefs); err != nil {
		t.Fatal(err)
	}
	policy, err = mgr.SpacePolicy(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	var queuedBytes int64
	if err := db.QueryRow(ctx,
		"SELECT estimated_bytes FROM pitr_gc_queue WHERE singleton").
		Scan(&queuedBytes); err != nil {
		t.Fatal(err)
	}
	if oldestRows != 0 || newestRows != 1 || sharedRefs != 1 || exclusiveRefs != 0 ||
		policy.RetainedBytes != 60 || policy.ReclaimableBytes != 30 || queuedBytes != 30 {
		t.Fatalf("old=%d new=%d shared=%d exclusive=%d retained=%d reclaimable=%d queued=%d",
			oldestRows, newestRows, sharedRefs, exclusiveRefs,
			policy.RetainedBytes, policy.ReclaimableBytes, queuedBytes)
	}
}
