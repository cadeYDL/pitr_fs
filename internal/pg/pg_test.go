package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pitr_fs/internal/schema"
)

var testDSN string

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PITR_TEST_PG_DSN"); dsn != "" {
		testDSN = dsn
		os.Exit(m.Run())
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "跳过 internal/pg 集成测试:未找到 docker")
		os.Exit(0)
	}

	name := fmt.Sprintf("pitr-pg-test-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=x", "-e", "POSTGRES_DB=pitr_pg_test",
		"postgres:16.14-bookworm").CombinedOutput()
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
	testDSN = fmt.Sprintf("postgres://postgres:x@%s:5432/pitr_pg_test?sslmode=disable",
		strings.TrimSpace(string(ipOut)))

	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		db, connectErr := Connect(ctx, testDSN)
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

func openTestDB(t *testing.T) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := Connect(ctx, testDSN, WithMaxConns(4))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func resetItems(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(ctx, "DROP TABLE IF EXISTS pg_test_items"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "CREATE TABLE pg_test_items (id int PRIMARY KEY, value text)"); err != nil {
		t.Fatal(err)
	}
}

func TestConnect_BadDSN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Connect(ctx, "://not-a-dsn"); err == nil {
		t.Fatal("错误 DSN 应返回 error")
	}
}

func TestInTx_Commit(t *testing.T) {
	db := openTestDB(t)
	resetItems(t, db)
	ctx := context.Background()
	if err := db.InTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO pg_test_items VALUES (1, 'committed')")
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	var value string
	if err := db.QueryRow(ctx, "SELECT value FROM pg_test_items WHERE id=1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "committed" {
		t.Fatalf("value=%q", value)
	}
}

func TestInTx_Rollback(t *testing.T) {
	db := openTestDB(t)
	resetItems(t, db)
	ctx := context.Background()
	sentinel := errors.New("fn failed")
	err := db.InTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO pg_test_items VALUES (1, 'no')"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应保留原始错误,实际 %v", err)
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM pg_test_items").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rollback 后仍有 %d 行", count)
	}
}

func TestInTx_PanicRollsBack(t *testing.T) {
	db := openTestDB(t)
	resetItems(t, db)
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic 应继续向上传播")
			}
		}()
		_ = db.InTx(ctx, func(tx Tx) error {
			_, _ = tx.Exec(ctx, "INSERT INTO pg_test_items VALUES (1, 'no')")
			panic("boom")
		})
	}()
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM pg_test_items").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("panic rollback 后仍有 %d 行", count)
	}
}

func TestSetLocal_Isolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.InTx(ctx, func(tx Tx) error {
		if err := tx.SetLocal(ctx, "pitr.current_txn", "123"); err != nil {
			return err
		}
		var got string
		if err := tx.QueryRow(ctx,
			"SELECT current_setting('pitr.current_txn', true)").Scan(&got); err != nil {
			return err
		}
		if got != "123" {
			return fmt.Errorf("事务 A GUC=%q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(tx Tx) error {
		var got *string
		if err := tx.QueryRow(ctx,
			"SELECT NULLIF(current_setting('pitr.current_txn', true), '')").Scan(&got); err != nil {
			return err
		}
		if got != nil {
			return fmt.Errorf("事务 B 泄漏 GUC=%q", *got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSetLocal_ReadInTrigger(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.Exec(ctx, `
		DROP TABLE IF EXISTS jfs_chunk_ref, jfs_chunk, jfs_edge, jfs_node;
		CREATE TABLE jfs_node (
			inode bigint PRIMARY KEY, type smallint, flags smallint, mode int,
			uid int, gid int, atime bigint, mtime bigint, ctime bigint,
			atimensec int, mtimensec int, ctimensec int, nlink int,
			length bigint, rdev int, parent bigint, access_acl_id int,
			default_acl_id int
		)`); err != nil {
		t.Fatalf("mock jfs_node: %v", err)
	}
	if _, err := db.Exec(ctx, schema.InitSQL); err != nil {
		t.Fatalf("re-apply schema: %v", err)
	}
	var txnID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash, parent_id, scope_path, state, command, closed_at)
		VALUES ('pgtrigger001', 1, '/', 'auto', 'test', now())
		RETURNING id`).Scan(&txnID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (700, 33188, 1, 0)"); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(tx Tx) error {
		if err := tx.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(txnID)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE jfs_node SET length=7 WHERE inode=700")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var owner int64
	if err := db.QueryRow(ctx,
		"SELECT txn_id FROM pitr_node_history WHERE inode=700").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != txnID {
		t.Fatalf("trigger txn_id=%d, want %d", owner, txnID)
	}
}

func TestPgErrorCodePreserved(t *testing.T) {
	db := openTestDB(t)
	resetItems(t, db)
	ctx := context.Background()
	_, _ = db.Exec(ctx, "INSERT INTO pg_test_items VALUES (1, 'first')")
	_, err := db.Exec(ctx, "INSERT INTO pg_test_items VALUES (1, 'duplicate')")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("应保留 23505 PgError,实际 %v", err)
	}
}
