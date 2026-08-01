package txn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/schema"
)

var txnTestDSN string

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PITR_TEST_PG_DSN"); dsn != "" {
		txnTestDSN = dsn
		os.Exit(m.Run())
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "跳过 internal/txn 集成测试:未找到 docker")
		os.Exit(0)
	}
	name := fmt.Sprintf("pitr-txn-test-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=x", "-e", "POSTGRES_DB=pitr_txn_test",
		"postgres:16").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 PostgreSQL: %v: %s\n", err, out)
		os.Exit(1)
	}
	ipOut, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 PostgreSQL 容器 IP:", err)
		os.Exit(1)
	}
	txnTestDSN = fmt.Sprintf("postgres://postgres:x@%s:5432/pitr_txn_test?sslmode=disable",
		strings.TrimSpace(string(ipOut)))
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		db, connectErr := pg.Connect(ctx, txnTestDSN)
		cancel()
		if connectErr == nil {
			_ = db.Close()
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "PostgreSQL 60 秒内未就绪:", connectErr)
			os.Exit(1)
		}
		time.Sleep(200 * time.Millisecond)
	}
	code := m.Run()
	if err := exec.Command("docker", "rm", "-f", name).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "清理 PostgreSQL 测试容器:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func setupManager(t *testing.T) (*Manager, *pg.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := pg.Connect(ctx, txnTestDSN, pg.WithMaxConns(8))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(ctx, `
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;
		CREATE TABLE jfs_node (
			inode bigint PRIMARY KEY, type smallint, flags smallint, mode int,
			uid int, gid int, atime bigint, mtime bigint, ctime bigint,
			atimensec int, mtimensec int, ctimensec int, nlink int,
			length bigint, rdev int, parent bigint, access_acl_id int,
			default_acl_id int
		);
		CREATE TABLE jfs_edge (
			parent bigint, name bytea, inode bigint, type smallint,
			PRIMARY KEY (parent, name)
		);
		CREATE TABLE jfs_chunk (
			inode bigint, indx int, slices bytea, PRIMARY KEY (inode, indx)
		);
		CREATE TABLE jfs_chunk_ref (
			chunkid bigint PRIMARY KEY, size int, refs int
		);
		CREATE TABLE jfs_delslices (
			chunkid bigint PRIMARY KEY, deleted bigint, slices bytea
		)`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return NewManager(db), db
}

func createClosedAuto(t *testing.T, mgr *Manager, db *pg.DB, parentID int64, command string) int64 {
	t.Helper()
	ctx := context.Background()
	var autoID int64
	if err := db.InTx(ctx, func(tx pg.Tx) error {
		id, _, err := mgr.CreateAutoVersion(ctx, tx, parentID, command)
		autoID = id
		return err
	}); err != nil {
		t.Fatalf("CreateAutoVersion: %v", err)
	}
	if err := mgr.CloseAutoVersion(ctx, autoID); err != nil {
		t.Fatalf("CloseAutoVersion: %v", err)
	}
	return autoID
}

func TestBegin_ReturnsTxn(t *testing.T) {
	mgr, _ := setupManager(t)
	got, err := mgr.Begin(context.Background(), "/a/../a", "开始")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || len(got.VersionHash) != 12 || got.ScopePath != "/a" ||
		got.State != StateActive || got.Command != "begin" || got.Message != "开始" {
		t.Fatalf("unexpected txn: %+v", got)
	}
}

func TestBegin_UniqueActivePerScope(t *testing.T) {
	mgr, _ := setupManager(t)
	if _, err := mgr.Begin(context.Background(), "/a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Begin(context.Background(), "/a/", ""); !errors.Is(err, ErrScopeActive) {
		t.Fatalf("二次 begin 应为 ErrScopeActive,实际 %v", err)
	}
}

func TestFindActive_LongestPrefix(t *testing.T) {
	mgr, _ := setupManager(t)
	if _, err := mgr.Begin(context.Background(), "/a", "outer"); err != nil {
		t.Fatal(err)
	}
	inner, err := mgr.Begin(context.Background(), "/a/b", "inner")
	if err != nil {
		t.Fatal(err)
	}
	got, err := mgr.FindActiveByPath(context.Background(), "/a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != inner.ID {
		t.Fatalf("最长前缀应命中 inner=%d,实际 %+v", inner.ID, got)
	}
}

func TestFindActive_NoMatch_ReturnsNil(t *testing.T) {
	mgr, _ := setupManager(t)
	if _, err := mgr.Begin(context.Background(), "/a", ""); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.FindActiveByPath(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("不应命中:%+v", got)
	}
}

func TestCommit_MovesState(t *testing.T) {
	mgr, _ := setupManager(t)
	active, err := mgr.Begin(context.Background(), "/a", "before")
	if err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Commit(context.Background(), active.ID, "done")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCommitted || got.ClosedAt == nil || got.Message != "done" {
		t.Fatalf("unexpected committed txn:%+v", got)
	}
}

func TestCommit_CollapsesAutoVersions(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	active, err := mgr.Begin(ctx, "/a", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, length := range []int{0, 10, 20} {
		autoID := createClosedAuto(t, mgr, db, active.ID, fmt.Sprintf("write:%d", i))
		if _, err := db.Exec(ctx, `
			INSERT INTO pitr_node_history (txn_id, inode, op, snapshot)
			VALUES ($1, 10, 'U', jsonb_build_object('length', $2::int))`,
			autoID, length); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mgr.Commit(ctx, active.ID, "collapsed"); err != nil {
		t.Fatal(err)
	}
	var historyCount, autoCount int
	var snapshotLength int
	if err := db.QueryRow(ctx, `
		SELECT count(*), (min(snapshot->>'length'))::int
		  FROM pitr_node_history WHERE txn_id=$1`, active.ID).
		Scan(&historyCount, &snapshotLength); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE parent_id=$1 AND state='auto'",
		active.ID).Scan(&autoCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 || snapshotLength != 0 || autoCount != 0 {
		t.Fatalf("collapse history=%d length=%d autos=%d",
			historyCount, snapshotLength, autoCount)
	}
}

func TestRollback_ReplaysHistory(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (100, 33188, 1, 100)"); err != nil {
		t.Fatal(err)
	}
	active, err := mgr.Begin(ctx, "/a", "")
	if err != nil {
		t.Fatal(err)
	}
	autoID := createClosedAuto(t, mgr, db, active.ID, "write:/a/f")
	// createClosedAuto 已关闭窗口,这里用同连接 GUC 精确造 history。
	if err := db.InTx(ctx, func(tx pg.Tx) error {
		if err := tx.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(autoID)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE jfs_node SET length=999 WHERE inode=100")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Rollback(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	var length, autos int
	if err := db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=100").Scan(&length); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE parent_id=$1", active.ID).Scan(&autos); err != nil {
		t.Fatal(err)
	}
	if got.State != StateRolledBack || got.ClosedAt == nil || length != 100 || autos != 0 {
		t.Fatalf("rollback txn=%+v length=%d autos=%d", got, length, autos)
	}
}

func TestAutoVersion_OpenReopenClose(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	active, err := mgr.Begin(ctx, "/a", "")
	if err != nil {
		t.Fatal(err)
	}
	autoID, err := mgr.OpenAutoVersion(ctx, active.ID, "write:/a/f")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.CloseAutoVersion(ctx, autoID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ReopenAutoVersion(ctx, autoID, active.ID); err != nil {
		t.Fatal(err)
	}
	var closed bool
	if err := db.QueryRow(ctx,
		"SELECT closed_at IS NOT NULL FROM pitr_txn WHERE id=$1", autoID).
		Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Fatal("reopen 后 closed_at 应为 NULL")
	}
	if err := mgr.CloseAutoVersion(ctx, autoID); err != nil {
		t.Fatal(err)
	}
}

func TestCloseDanglingAutoVersions(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	active, err := mgr.Begin(ctx, "/a", "")
	if err != nil {
		t.Fatal(err)
	}
	autoID, err := mgr.OpenAutoVersion(ctx, active.ID, "write:/a/f")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := mgr.CloseDanglingAutoVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("closed=%d", closed)
	}
	var isClosed bool
	if err := db.QueryRow(ctx,
		"SELECT closed_at IS NOT NULL FROM pitr_txn WHERE id=$1", autoID).
		Scan(&isClosed); err != nil {
		t.Fatal(err)
	}
	if !isClosed {
		t.Fatal("遗留 auto 应被关闭")
	}
}

func TestAbortAutoVersion_OnlyReplaysTarget(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (100, 33188, 1, 10)"); err != nil {
		t.Fatal(err)
	}
	active, err := mgr.Begin(ctx, "/a", "")
	if err != nil {
		t.Fatal(err)
	}
	siblingID := createClosedAuto(t, mgr, db, active.ID, "chmod:/a/f")
	targetID := createClosedAuto(t, mgr, db, active.ID, "write:/a/f")
	if err := db.InTx(ctx, func(tx pg.Tx) error {
		if err := tx.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(targetID)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE jfs_node SET length=99 WHERE inode=100")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.AbortAutoVersion(ctx, targetID); err != nil {
		t.Fatal(err)
	}
	var length int
	if err := db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=100").
		Scan(&length); err != nil {
		t.Fatal(err)
	}
	var parentState string
	if err := db.QueryRow(ctx, "SELECT state FROM pitr_txn WHERE id=$1", active.ID).
		Scan(&parentState); err != nil {
		t.Fatal(err)
	}
	var siblingParent int64
	if err := db.QueryRow(ctx, "SELECT parent_id FROM pitr_txn WHERE id=$1", siblingID).
		Scan(&siblingParent); err != nil {
		t.Fatal(err)
	}
	var targetCount int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM pitr_txn WHERE id=$1", targetID).
		Scan(&targetCount); err != nil {
		t.Fatal(err)
	}
	if length != 10 || parentState != StateActive ||
		siblingParent != active.ID || targetCount != 0 {
		t.Fatalf("length=%d state=%s sibling.parent=%d target.count=%d",
			length, parentState, siblingParent, targetCount)
	}
}

func TestLogs_OrderScopeAndLimit(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	for _, value := range []struct {
		hash, scope, command string
	}{
		{"111111111111", "/workspace/proj", "commit"},
		{"222222222222", "/workspace/other", "commit"},
		{"333333333333", "/workspace/proj/sub", "write:/workspace/proj/sub/a"},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO pitr_txn
				(version_hash,parent_id,scope_path,state,command,closed_at)
			VALUES ($1,1,$2,'committed',$3,now())`,
			value.hash, value.scope, value.command); err != nil {
			t.Fatal(err)
		}
	}
	items, err := mgr.List(ctx, "/workspace/proj", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].VersionHash != "333333333333" ||
		items[1].VersionHash != "111111111111" {
		t.Fatalf("logs=%+v", items)
	}
	for _, item := range items {
		if item.ScopePath == "/workspace/other" {
			t.Fatalf("scope filter 泄漏 other:%+v", item)
		}
	}
}

func TestDiff_CountsDistinctKeysAndScope(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	var v1, v2, other int64
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('111111111111',1,'/workspace/proj','committed','commit',now())
		RETURNING id`).Scan(&v1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('222222222222',$1,'/workspace/proj','committed','commit',now())
		RETURNING id`, v1).Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('333333333333',$1,'/workspace/other','committed','commit',now())
		RETURNING id`, v2).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_node_history(txn_id,inode,op,snapshot)
			VALUES ($1,10,'U','{}'),($2,20,'U','{}')`, v2, other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_edge_history(txn_id,parent,name,op,snapshot)
			VALUES ($1,1,'a','U','{}')`, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_chunk_history(txn_id,inode,indx,op,snapshot)
			VALUES ($1,10,0,'U','{}')`, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_chunk_ref_history(txn_id,chunkid,op,snapshot)
			VALUES ($1,99,'U','{}')`, v2); err != nil {
		t.Fatal(err)
	}
	stats, err := mgr.Diff(ctx,
		"111111111111", "333333333333", "/workspace/proj")
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodeChanges != 1 || stats.EdgeChanges != 1 || stats.ChunkChanges != 2 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestVersionHash_NoCollision(t *testing.T) {
	const count = 1000
	hashes := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := newVersionHash()
			if err != nil {
				errs <- err
				return
			}
			hashes <- hash
		}()
	}
	wg.Wait()
	close(hashes)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, count)
	for hash := range hashes {
		if len(hash) != 12 {
			t.Fatalf("hash 长度=%d:%q", len(hash), hash)
		}
		if _, exists := seen[hash]; exists {
			t.Fatalf("hash 冲突:%s", hash)
		}
		seen[hash] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("只生成 %d 个 hash", len(seen))
	}
}
