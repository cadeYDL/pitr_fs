package revert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/schema"
)

var revertTestDSN string

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PITR_TEST_PG_DSN"); dsn != "" {
		revertTestDSN = dsn
		os.Exit(m.Run())
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "跳过 internal/revert 集成测试:未找到 docker")
		os.Exit(0)
	}
	name := fmt.Sprintf("pitr-revert-test-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=x", "-e", "POSTGRES_DB=pitr_revert_test",
		"postgres:16").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 PostgreSQL: %v: %s\n", err, out)
		os.Exit(1)
	}
	ipOut, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		fmt.Fprintln(os.Stderr, "读取 PostgreSQL 容器 IP:", err)
		os.Exit(1)
	}
	revertTestDSN = fmt.Sprintf(
		"postgres://postgres:x@%s:5432/pitr_revert_test?sslmode=disable",
		strings.TrimSpace(string(ipOut)))
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		db, connectErr := pg.Connect(ctx, revertTestDSN)
		cancel()
		if connectErr == nil {
			_ = db.Close()
			break
		}
		if time.Now().After(deadline) {
			_ = exec.Command("docker", "rm", "-f", name).Run()
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

func setupEngine(t *testing.T) (*Engine, *pg.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := pg.Connect(ctx, revertTestDSN, pg.WithMaxConns(8))
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
		)`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return NewEngine(db), db
}

func insertCommitted(
	t *testing.T,
	db *pg.DB,
	hash, scope string,
) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(), `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ($1,1,$2,'committed','commit',now()) RETURNING id`,
		hash, scope).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func updateLength(
	t *testing.T,
	db *pg.DB,
	txnID, inode, length int64,
) {
	t.Helper()
	ctx := context.Background()
	if err := db.InTx(ctx, func(tx pg.Tx) error {
		if err := tx.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(txnID)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			"UPDATE jfs_node SET length=$2 WHERE inode=$1", inode, length)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRevert_InvalidHash(t *testing.T) {
	engine := new(Engine)
	_, _, err := engine.Revert(context.Background(), Options{TargetHash: "bad"})
	if !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("err=%v", err)
	}
}

func TestRevert_DryRunDoesNotMutate(t *testing.T) {
	engine, db := setupEngine(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_node (inode,mode,nlink,length) VALUES (100,33188,1,10)"); err != nil {
		t.Fatal(err)
	}
	v1 := insertCommitted(t, db, "111111111111", "/workspace/proj")
	_ = v1
	v2 := insertCommitted(t, db, "222222222222", "/workspace/proj")
	updateLength(t, db, v2, 100, 20)

	applied, hash, err := engine.Revert(ctx, Options{
		TargetHash: "111111111111",
		DryRun:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var length, revertVersions int
	if err := db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=100").
		Scan(&length); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE command LIKE 'revert:%'").
		Scan(&revertVersions); err != nil {
		t.Fatal(err)
	}
	if applied != 1 || hash != "" || length != 20 || revertVersions != 0 {
		t.Fatalf("applied=%d hash=%q length=%d versions=%d",
			applied, hash, length, revertVersions)
	}
}

func TestRevert_ProducesUndoableVersion(t *testing.T) {
	engine, db := setupEngine(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_node (inode,mode,nlink,length) VALUES (100,33188,1,10)"); err != nil {
		t.Fatal(err)
	}
	insertCommitted(t, db, "111111111111", "/workspace/proj")
	v2 := insertCommitted(t, db, "222222222222", "/workspace/proj")
	updateLength(t, db, v2, 100, 20)

	applied, revertHash, err := engine.Revert(ctx, Options{
		TargetHash: "111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}
	var length int64
	if err := db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=100").
		Scan(&length); err != nil {
		t.Fatal(err)
	}
	var command string
	var inverseLength int64
	if err := db.QueryRow(ctx, `
		SELECT t.command,(h.snapshot->>'length')::bigint
		  FROM pitr_txn t JOIN pitr_node_history h ON h.txn_id=t.id
		 WHERE t.version_hash=$1`, revertHash).Scan(&command, &inverseLength); err != nil {
		t.Fatal(err)
	}
	if applied != 1 || length != 10 ||
		command != "revert:111111111111" || inverseLength != 20 {
		t.Fatalf("applied=%d length=%d command=%q inverse=%d hash=%s",
			applied, length, command, inverseLength, revertHash)
	}

	// 撤销刚才的 revert:回到 v2,验证 revert 事件自己的 undo history 可用。
	if _, _, err := engine.Revert(ctx, Options{TargetHash: "222222222222"}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=100").
		Scan(&length); err != nil {
		t.Fatal(err)
	}
	if length != 20 {
		t.Fatalf("revert 回 v2 后 length=%d", length)
	}
}

func TestRevert_SubtreeScope(t *testing.T) {
	engine, db := setupEngine(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_node (inode,mode,nlink,length)
		VALUES (100,33188,1,10),(200,33188,1,100)`); err != nil {
		t.Fatal(err)
	}
	proj := insertCommitted(t, db, "111111111111", "/workspace/proj")
	updateLength(t, db, proj, 100, 20)
	other := insertCommitted(t, db, "222222222222", "/workspace/other")
	updateLength(t, db, other, 200, 200)

	if _, _, err := engine.Revert(ctx, Options{
		TargetHash: "000000000000",
		ScopePath:  "/workspace/proj",
	}); err != nil {
		t.Fatal(err)
	}
	var projLength, otherLength int64
	if err := db.QueryRow(ctx,
		"SELECT max(length) FILTER (WHERE inode=100),max(length) FILTER (WHERE inode=200) FROM jfs_node").
		Scan(&projLength, &otherLength); err != nil {
		t.Fatal(err)
	}
	if projLength != 10 || otherLength != 200 {
		t.Fatalf("proj=%d other=%d", projLength, otherLength)
	}
}

func TestRevert_SubtreeInodeClosureWithinBroadTxn(t *testing.T) {
	_, db := setupEngine(t)
	engine := NewEngine(db, WithMountPath("/workspace"))
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_node(inode,mode,nlink,length,parent)
		VALUES
		  (10,16877,2,0,1),(20,16877,2,0,1),
		  (100,33188,1,10,10),(200,33188,1,100,20);
		INSERT INTO jfs_edge(parent,name,inode,type)
		VALUES
		  (1,convert_to('proj','UTF8'),10,2),
		  (1,convert_to('other','UTF8'),20,2),
		  (10,convert_to('file','UTF8'),100,1),
		  (20,convert_to('file','UTF8'),200,1)`); err != nil {
		t.Fatal(err)
	}
	insertCommitted(t, db, "111111111111", "/workspace")
	broad := insertCommitted(t, db, "222222222222", "/workspace")
	if err := db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(broad)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx,
			"UPDATE jfs_node SET length=length+1 WHERE inode IN (100,200)")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	applied, _, err := engine.Revert(ctx, Options{
		TargetHash: "111111111111",
		ScopePath:  "/workspace/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	var projLength, otherLength int64
	if err := db.QueryRow(ctx, `
		SELECT max(length) FILTER (WHERE inode=100),
		       max(length) FILTER (WHERE inode=200)
		  FROM jfs_node`).Scan(&projLength, &otherLength); err != nil {
		t.Fatal(err)
	}
	if applied != 1 || projLength != 10 || otherLength != 101 {
		t.Fatalf("applied=%d proj=%d other=%d",
			applied, projLength, otherLength)
	}
}

func TestRevert_RejectsActiveScope(t *testing.T) {
	engine, db := setupEngine(t)
	if _, err := db.Exec(context.Background(), `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command)
		VALUES ('111111111111',1,'/workspace/proj','active','begin')`); err != nil {
		t.Fatal(err)
	}
	_, _, err := engine.Revert(context.Background(), Options{
		TargetHash: "000000000000",
		ScopePath:  "/workspace/proj",
	})
	if !errors.Is(err, ErrActiveScope) {
		t.Fatalf("err=%v", err)
	}
}

func TestRevert_WaitsForAutomaticWriteRelease(t *testing.T) {
	engine, db := setupEngine(t)
	ctx := context.Background()
	insertCommitted(t, db, "111111111111", "/workspace/proj")
	var openID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command)
		VALUES ('222222222222',2,'/workspace/proj','auto','write')
		RETURNING id`).Scan(&openID); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(75 * time.Millisecond)
		_, err := db.Exec(ctx,
			"UPDATE pitr_txn SET closed_at=now() WHERE id=$1", openID)
		closed <- err
	}()
	started := time.Now()
	if _, _, err := engine.Revert(ctx, Options{
		TargetHash: "111111111111",
		ScopePath:  "/workspace/proj",
		DryRun:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("revert 未等待自动写窗口: %s", elapsed)
	}
}

func TestRevert_10kFilesDir(t *testing.T) {
	engine, db := setupEngine(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_node(inode,mode,nlink,length)
		SELECT value,33188,1,10 FROM generate_series(1000,10999) AS value`); err != nil {
		t.Fatal(err)
	}
	insertCommitted(t, db, "111111111111", "/workspace/tenk")
	v2 := insertCommitted(t, db, "222222222222", "/workspace/tenk")
	if err := db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(v2)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx, `
			UPDATE jfs_node SET length=20 WHERE inode BETWEEN 1000 AND 10999`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	applied, _, err := engine.Revert(ctx, Options{
		TargetHash: "111111111111",
		ScopePath:  "/workspace/tenk",
	})
	if err != nil {
		t.Fatal(err)
	}
	var restored int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM jfs_node
		 WHERE inode BETWEEN 1000 AND 10999 AND length=10`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if applied != 10_000 || restored != 10_000 {
		t.Fatalf("applied=%d restored=%d", applied, restored)
	}
	t.Logf("10k files metadata revert: %s", time.Since(started))
}

func TestRevert_ConcurrentWriteBlocksUntilCommit(t *testing.T) {
	engine, db := setupEngine(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_node(inode,mode,nlink,length) VALUES (100,33188,1,10)"); err != nil {
		t.Fatal(err)
	}
	insertCommitted(t, db, "111111111111", "/workspace/concurrent")
	v2 := insertCommitted(t, db, "222222222222", "/workspace/concurrent")
	updateLength(t, db, v2, 100, 20)
	// 只让 revert replay 的 UPDATE 放慢 250ms。此时 pitr_revert 已持有四张
	// JuiceFS 表的 EXCLUSIVE lock,并发写必须等它提交。
	if _, err := db.Exec(ctx, `
		CREATE OR REPLACE FUNCTION pitr_test_slow_revert() RETURNS trigger AS $$
		DECLARE command_value text;
		BEGIN
		  SELECT command INTO command_value
		    FROM pitr_txn
		   WHERE id=NULLIF(current_setting('pitr.current_txn',true),'')::bigint;
		  IF command_value LIKE 'revert:%' THEN
		    PERFORM pg_sleep(0.25);
		  END IF;
		  RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER tg_test_slow_revert
		  BEFORE UPDATE ON jfs_node
		  FOR EACH ROW EXECUTE FUNCTION pitr_test_slow_revert()`); err != nil {
		t.Fatal(err)
	}

	revertDone := make(chan error, 1)
	go func() {
		_, _, err := engine.Revert(ctx, Options{
			TargetHash: "111111111111",
			ScopePath:  "/workspace/concurrent",
		})
		revertDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	if _, err := db.Exec(ctx, "UPDATE jfs_node SET length=99 WHERE inode=100"); err != nil {
		t.Fatal(err)
	}
	blockedFor := time.Since(started)
	if err := <-revertDone; err != nil {
		t.Fatal(err)
	}
	var length int64
	if err := db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=100").
		Scan(&length); err != nil {
		t.Fatal(err)
	}
	if blockedFor < 150*time.Millisecond || length != 99 {
		t.Fatalf("blocked=%s length=%d", blockedFor, length)
	}
}
