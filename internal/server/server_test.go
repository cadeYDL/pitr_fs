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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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
		CREATE TABLE jfs_delslices (
			chunkid bigint PRIMARY KEY, deleted bigint, slices bytea
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
		MountRoot:     "/",
		Retention:     "compact",
		JFSMounted:    true,
		FUSEMounted:   true,
		MountFunc:     func(context.Context, string) error { return nil },
		UmountFunc:    func(context.Context) error { return nil },
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

func TestServer_ManualTransactionsDisabled(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	statusResp, err := f.client.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !statusResp.GetPostgresHealthy() || len(statusResp.GetVolumes()) != 1 ||
		statusResp.GetOpenWrites() != 0 ||
		statusResp.GetVolumes()[0].GetHistoryLimit() != 100 {
		t.Fatalf("unexpected status:%+v", statusResp)
	}
	calls := []func() error{
		func() error {
			_, err := f.client.Begin(ctx, &pb.BeginRequest{Path: "/workspace/proj"})
			return err
		},
		func() error {
			_, err := f.client.Commit(ctx, &pb.CommitRequest{Path: "/workspace/proj"})
			return err
		},
		func() error {
			_, err := f.client.Rollback(ctx, &pb.RollbackRequest{Path: "/workspace/proj"})
			return err
		},
	}
	for _, call := range calls {
		if code := status.Code(call()); code != codes.FailedPrecondition {
			t.Fatalf("manual transaction code=%s, want FailedPrecondition", code)
		}
	}
}

func TestServer_AutomaticVersion_E2E(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	if _, err := f.db.Exec(ctx,
		"INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (500, 33188, 1, 5)"); err != nil {
		t.Fatal(err)
	}
	autoID, err := f.mgr.OpenStandaloneVersion(
		ctx, "/workspace/proj/a", "write:/workspace/proj/a",
		txn.VersionMetadata{
			PosixOp:        `write("/workspace/proj/a", offset=0, len=2)`,
			ProcessCommand: "echo hi > ...",
			ActorUID:       1000, ActorGID: 1001, ActorPID: 42,
			ActorName: "tester",
		})
	if err != nil {
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
	if err := f.mgr.CloseStandaloneVersion(
		ctx, autoID, "", `"v1" -> "v2"`); err != nil {
		t.Fatal(err)
	}
	logs, err := f.client.Logs(ctx,
		&pb.LogsRequest{Path: "/workspace/proj", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs.GetEntries()) < 1 ||
		logs.GetEntries()[0].GetTransaction().GetState() != txn.StateAuto ||
		logs.GetEntries()[0].GetTransaction().GetCommand() !=
			"write:/workspace/proj/a" {
		t.Fatalf("unexpected logs:%+v", logs)
	}
	if logs.GetEntries()[0].GetTransaction().GetClosedAt() == nil {
		t.Fatal("自动版本必须已关闭")
	}
	audit := logs.GetEntries()[0].GetTransaction()
	if audit.GetPosixOperation() == "" ||
		audit.GetProcessCommand() != "echo hi > ..." ||
		audit.GetActorUid() != 1000 || audit.GetActorGid() != 1001 ||
		audit.GetActorPid() != 42 || audit.GetActorName() != "tester" ||
		audit.GetChangeSummary() != `"v1" -> "v2"` {
		t.Fatalf("audit=%+v", audit)
	}
}

func TestServer_RevertAtValidationAndResolution(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	var firstID int64
	if err := f.db.QueryRow(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('111111111111',1,'/workspace/proj','committed','commit',
		        '2020-01-01T01:00:00Z')
		RETURNING id`).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(ctx, `
		INSERT INTO pitr_txn
			(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('222222222222',$1,'/workspace/proj','committed','commit',
		        '2020-01-01T02:00:00Z')`, firstID); err != nil {
		t.Fatal(err)
	}
	response, err := f.client.Revert(ctx, &pb.RevertRequest{
		TargetTime: "2020-01-01T01:30:00+00:00",
		Path:       "/workspace/proj", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedTime, parseErr := time.Parse(
		time.RFC3339Nano, response.GetResolvedVersionTime())
	if response.GetResolvedVersionHash() != "111111111111" ||
		parseErr != nil ||
		!resolvedTime.Equal(time.Date(2020, 1, 1, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("resolved=%+v", response)
	}
	for _, request := range []*pb.RevertRequest{
		{},
		{VersionHash: "111111111111", TargetTime: "2020-01-01T01:00:00Z"},
		{TargetTime: "not-a-time"},
		{TargetTime: time.Now().Add(time.Hour).Format(time.RFC3339Nano)},
	} {
		if _, err := f.client.Revert(ctx, request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("request=%+v code=%s err=%v",
				request, status.Code(err), err)
		}
	}
	if _, err := f.client.Revert(ctx, &pb.RevertRequest{
		TargetTime: "2019-12-31T23:59:59Z",
		DryRun:     true,
	}); status.Code(err) != codes.OutOfRange {
		t.Fatalf("too early code=%s err=%v", status.Code(err), err)
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

func TestServer_LifecycleAndConfig_E2E(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	initialHistory := int32(9)
	initialMax := int64(100 << 20)
	initialReserve := int32(20)

	initialized, err := f.client.Init(ctx, &pb.InitRequest{
		Path: "/workspace", Volume: "test-volume", Retention: "verbose",
		HistoryLimit: &initialHistory, MaxSpaceBytes: &initialMax,
		SpaceReservePercent: &initialReserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !initialized.GetVolume().GetFuseMounted() ||
		initialized.GetVolume().GetRetention() != "verbose" ||
		initialized.GetVolume().GetHistoryLimit() != initialHistory ||
		initialized.GetVolume().GetMaxSpaceBytes() != initialMax {
		t.Fatalf("init=%+v", initialized)
	}
	if _, err := f.client.Umount(ctx,
		&pb.UmountRequest{Path: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	statusResponse, err := f.client.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if statusResponse.GetVolumes()[0].GetFuseMounted() {
		t.Fatal("umount 后 status 仍显示 mounted")
	}
	mounted, err := f.client.Mount(ctx, &pb.MountRequest{
		Path: "/workspace", Volume: "test-volume",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mounted.GetVolume().GetFuseMounted() {
		t.Fatalf("mount=%+v", mounted)
	}
	configured, err := f.client.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "retention", Value: "archive", Window: "30d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if configured.GetValue() != "archive" || configured.GetWindow() != "30d" {
		t.Fatalf("config=%+v", configured)
	}
	historyConfig, err := f.client.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "history-limit", Value: "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if historyConfig.GetValue() != "7" {
		t.Fatalf("history config=%+v", historyConfig)
	}
	spaceConfig, err := f.client.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "max-space", Value: "200MiB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spaceConfig.GetValue() != "200.00 MiB" {
		t.Fatalf("space config=%+v", spaceConfig)
	}
	reserveConfig, err := f.client.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "space-reserve", Value: "25%",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserveConfig.GetValue() != "25%" {
		t.Fatalf("reserve config=%+v", reserveConfig)
	}
	statusResponse, err = f.client.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusResponse.GetVolumes()[0].GetHistoryLimit(); got != 7 {
		t.Fatalf("persisted history limit=%d", got)
	}
	volume := statusResponse.GetVolumes()[0]
	if volume.GetMaxSpaceBytes() != 200<<20 || volume.GetSpaceReservePercent() != 25 {
		t.Fatalf("persisted space policy=%+v", volume)
	}
	space, err := f.client.Space(ctx, &pb.SpaceRequest{Path: "/workspace", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if space.GetMaxSpaceBytes() != 200<<20 ||
		space.GetHighWatermarkBytes() != 150<<20 {
		t.Fatalf("space response=%+v", space)
	}
}

func TestServer_InitSelectsAndPersistsMountPath(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	var mounted []string
	handler := New(f.db, f.mgr, Config{
		Volume:     "dynamic-volume",
		JFSMount:   "/jfs",
		MountRoot:  "/pitr",
		Retention:  "compact",
		JFSMounted: true,
		MountFunc: func(_ context.Context, mountPath string) error {
			mounted = append(mounted, mountPath)
			return nil
		},
		UmountFunc: func(context.Context) error { return nil },
	})
	for _, invalid := range []string{"relative", "/pitr", "/other/data"} {
		if _, err := handler.Init(ctx, &pb.InitRequest{
			Path: invalid, Volume: "dynamic-volume",
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("init(%q) code=%s err=%v", invalid, status.Code(err), err)
		}
	}
	initialized, err := handler.Init(ctx, &pb.InitRequest{
		Path: "/pitr/data", Volume: "dynamic-volume", Retention: "verbose",
	})
	if err != nil {
		t.Fatal(err)
	}
	if initialized.GetVolume().GetFuseMount() != "/pitr/data" ||
		!initialized.GetVolume().GetFuseMounted() || len(mounted) != 1 ||
		mounted[0] != "/pitr/data" {
		t.Fatalf("init=%+v mounted=%v", initialized, mounted)
	}
	persisted, err := f.mgr.LoadVolumeMountConfig(ctx, "dynamic-volume")
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.FUSEMount != "/pitr/data" ||
		persisted.Retention != "verbose" {
		t.Fatalf("persisted=%+v", persisted)
	}
	if _, err := handler.Init(ctx, &pb.InitRequest{
		Path: "/pitr/data", Volume: "dynamic-volume", Retention: "compact",
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err = f.mgr.LoadVolumeMountConfig(ctx, "dynamic-volume")
	if err != nil || persisted.Retention != "compact" || len(mounted) != 1 {
		t.Fatalf("idempotent persisted=%+v mounted=%v err=%v",
			persisted, mounted, err)
	}
	if _, err := handler.Init(ctx, &pb.InitRequest{
		Path: "/pitr/other", Volume: "dynamic-volume",
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("第二挂载路径 code=%s err=%v", status.Code(err), err)
	}
}

func TestServer_UmountRejectsOpenWrite(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	versionID, err := f.mgr.OpenStandaloneVersion(
		ctx, "/workspace/proj", "write:/workspace/proj/a",
		txn.VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Umount(ctx,
		&pb.UmountRequest{Path: "/workspace"}); err == nil {
		t.Fatal("存在开放写窗口时 umount 应失败")
	}
	if err := f.mgr.CloseStandaloneVersion(ctx, versionID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Umount(ctx,
		&pb.UmountRequest{Path: "/workspace"}); err != nil {
		t.Fatal(err)
	}
}

func TestServer_ClearRequiresConfirmationAndKeepsCurrentData(t *testing.T) {
	f := setupServer(t)
	ctx := context.Background()
	if _, err := f.db.Exec(ctx,
		"INSERT INTO jfs_node (inode,mode,nlink,length) VALUES (900,33188,1,17)"); err != nil {
		t.Fatal(err)
	}
	versionID, err := f.mgr.OpenStandaloneVersion(
		ctx, "/workspace/proj/file", "write:/workspace/proj/file",
		txn.VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.InTx(ctx, func(txDB pg.Tx) error {
		if err := txDB.SetLocal(ctx, "pitr.current_txn", fmt.Sprint(versionID)); err != nil {
			return err
		}
		_, err := txDB.Exec(ctx, "UPDATE jfs_node SET length=23 WHERE inode=900")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.CloseStandaloneVersion(ctx, versionID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Clear(ctx, &pb.ClearRequest{
		Global: true,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("未确认 clear code=%s", status.Code(err))
	}
	cleared, err := f.client.Clear(ctx, &pb.ClearRequest{
		Global: true, Confirm: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.GetVersionsDeleted() != 1 || cleared.GetHistoryDeleted() != 1 {
		t.Fatalf("clear=%+v", cleared)
	}
	var length, versions, configs int64
	if err := f.db.QueryRow(ctx,
		"SELECT length FROM jfs_node WHERE inode=900").Scan(&length); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_config").Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if length != 23 || versions != 1 || configs != 1 {
		t.Fatalf("clear 后 length=%d versions=%d configs=%d",
			length, versions, configs)
	}
}
