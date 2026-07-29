package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/pg"
	"pitr_fs/internal/schema"
	"pitr_fs/internal/txn"
)

var serverTestDSN string

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PITR_TEST_PG_DSN"); dsn != "" {
		serverTestDSN = dsn
		os.Exit(m.Run())
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "跳过 internal/server E2E:未找到 docker")
		os.Exit(0)
	}
	name := fmt.Sprintf("pitr-server-test-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=x", "-e", "POSTGRES_DB=pitr_server_test",
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
	serverTestDSN = fmt.Sprintf(
		"postgres://postgres:x@%s:5432/pitr_server_test?sslmode=disable",
		strings.TrimSpace(string(ipOut)))
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		db, connectErr := pg.Connect(ctx, serverTestDSN)
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

type serverFixture struct {
	db     *pg.DB
	mgr    *txn.Manager
	client pb.PitrdClient
}

func setupServer(t *testing.T) *serverFixture {
	t.Helper()
	ctx := context.Background()
	db, err := pg.Connect(ctx, serverTestDSN, pg.WithMaxConns(8))
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

	mgr := txn.NewManager(db)
	grpcServer := grpc.NewServer()
	pb.RegisterPitrdServer(grpcServer, New(db, mgr, Config{
		DaemonVersion: "test",
		Volume:        "test-volume",
		JFSMount:      "/jfs",
		FUSEMount:     "/workspace",
		Retention:     "compact",
	}))
	listener := bufconn.Listen(1024 * 1024)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &serverFixture{db: db, mgr: mgr, client: pb.NewPitrdClient(conn)}
}

func TestServer_BeginCommit_E2E(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	statusResp, err := f.client.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !statusResp.GetPostgresHealthy() || len(statusResp.GetVolumes()) != 1 {
		t.Fatalf("unexpected status:%+v", statusResp)
	}
	beginResp, err := f.client.Begin(ctx, &pb.BeginRequest{
		Path: "/workspace/proj", Message: "edit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if beginResp.GetTransaction().GetState() != txn.StateActive {
		t.Fatalf("unexpected begin:%+v", beginResp)
	}
	commitResp, err := f.client.Commit(ctx, &pb.CommitRequest{
		Path: "/workspace/proj", Message: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if commitResp.GetTransaction().GetState() != txn.StateCommitted ||
		commitResp.GetTransaction().GetMessage() != "done" {
		t.Fatalf("unexpected commit:%+v", commitResp)
	}
	logs, err := f.client.Logs(ctx,
		&pb.LogsRequest{Path: "/workspace/proj", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs.GetEntries()) < 1 ||
		logs.GetEntries()[0].GetTransaction().GetState() != txn.StateCommitted {
		t.Fatalf("unexpected logs:%+v", logs)
	}
}

func TestServer_Rollback_E2E(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	if _, err := f.db.Exec(ctx,
		"INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (500, 33188, 1, 5)"); err != nil {
		t.Fatal(err)
	}
	beginResp, err := f.client.Begin(ctx,
		&pb.BeginRequest{Path: "/workspace/proj"})
	if err != nil {
		t.Fatal(err)
	}
	activeID := beginResp.GetTransaction().GetTxnId()
	var autoID int64
	if err := f.db.InTx(ctx, func(txDB pg.Tx) error {
		id, _, err := f.mgr.CreateAutoVersion(ctx, txDB, activeID, "write:/workspace/proj/a")
		autoID = id
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.CloseAutoVersion(ctx, autoID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(autoID)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx, "UPDATE jfs_node SET length=99 WHERE inode=500")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rollbackResp, err := f.client.Rollback(ctx,
		&pb.RollbackRequest{TxnId: activeID})
	if err != nil {
		t.Fatal(err)
	}
	if rollbackResp.GetTransaction().GetState() != txn.StateRolledBack {
		t.Fatalf("unexpected rollback:%+v", rollbackResp)
	}
	var length int64
	if err := f.db.QueryRow(ctx,
		"SELECT length FROM jfs_node WHERE inode=500").Scan(&length); err != nil {
		t.Fatal(err)
	}
	if length != 5 {
		t.Fatalf("rollback 后 length=%d", length)
	}
}
