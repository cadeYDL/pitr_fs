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
		);
		CREATE TABLE jfs_setting (
			name text PRIMARY KEY, value text
		);
		INSERT INTO jfs_setting(name,value) VALUES ('name','test-volume')
		`); err != nil {
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
		JFSMounted:    true,
		FUSEMounted:   true,
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

func TestServer_RevertDiffRecover_E2E(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	if _, err := f.db.Exec(ctx, `
		INSERT INTO jfs_node (inode,mode,nlink,length,parent)
		VALUES (10,16877,2,0,1),(700,33188,1,10,10);
		INSERT INTO jfs_edge(parent,name,inode,type)
		VALUES
		  (1,convert_to('proj','UTF8'),10,2),
		  (10,convert_to('file','UTF8'),700,1)`); err != nil {
		t.Fatal(err)
	}
	var v1, v2 int64
	if err := f.db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('111111111111',1,'/workspace/proj','committed','commit',now())
		RETURNING id`).Scan(&v1); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('222222222222',$1,'/workspace/proj','committed','commit',now())
		RETURNING id`, v1).Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if err := f.db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(v2)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx, "UPDATE jfs_node SET length=20 WHERE inode=700")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	diff, err := f.client.Diff(ctx, &pb.DiffRequest{
		VersionA: "111111111111",
		VersionB: "222222222222",
		Path:     "/workspace/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff.GetNodeChanges() != 1 || diff.GetEdgeChanges() != 0 {
		t.Fatalf("diff=%+v", diff)
	}
	reverted, err := f.client.Revert(ctx, &pb.RevertRequest{
		VersionHash: "111111111111",
		Path:        "/workspace/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reverted.GetApplied() != 1 || len(reverted.GetNewVersionHash()) != 12 {
		t.Fatalf("revert=%+v", reverted)
	}
	var length int64
	if err := f.db.QueryRow(ctx, "SELECT length FROM jfs_node WHERE inode=700").
		Scan(&length); err != nil {
		t.Fatal(err)
	}
	if length != 10 {
		t.Fatalf("length=%d", length)
	}
	recovered, err := f.client.Recover(ctx,
		&pb.RecoverRequest{Path: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.GetVolumes()) != 1 ||
		!recovered.GetVolumes()[0].GetFuseMounted() {
		t.Fatalf("recover=%+v", recovered)
	}
}

func TestServer_RecoverVolumeMissingDoesNotFormat(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	if _, err := f.db.Exec(ctx, "DROP TABLE jfs_setting"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Recover(ctx, &pb.RecoverRequest{}); err == nil {
		t.Fatal("缺少 jfs_setting 时 recover 应失败")
	}
	var exists bool
	if err := f.db.QueryRow(ctx,
		"SELECT to_regclass('public.jfs_setting') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("recover 禁止重新创建/format JuiceFS 元数据")
	}
}

func TestServer_RecoverMultiVolume_OneFailureDoesNotHideSuccess(t *testing.T) {
	f := setupServer(t)
	handler := New(f.db, f.mgr, Config{
		Volumes: []VolumeConfig{
			{
				Name: "healthy", JFSMount: "/jfs-a", FUSEMount: "/workspace-a",
				Retention: "compact", JFSMounted: true, FUSEMounted: true, DB: f.db,
			},
			{
				Name: "missing", JFSMount: "/jfs-b", FUSEMount: "/workspace-b",
				Retention: "compact", JFSMounted: false, FUSEMounted: false,
			},
		},
	})
	response, err := handler.Recover(context.Background(), &pb.RecoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetVolumes()) != 2 {
		t.Fatalf("volumes=%+v", response.GetVolumes())
	}
	if response.GetVolumes()[0].GetError() != "" ||
		response.GetVolumes()[1].GetError() == "" {
		t.Fatalf("healthy=%+v missing=%+v",
			response.GetVolumes()[0], response.GetVolumes()[1])
	}
}
