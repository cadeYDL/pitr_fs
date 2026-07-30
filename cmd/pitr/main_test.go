package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "pitr_fs/api/pitrd/v1"
)

// TestPitrCLI_Help — cobra 骨架能加载、`--help` 列出全部子命令
func TestPitrCLI_Help(t *testing.T) {
	root := newRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute --help: %v", err)
	}
	got := buf.String()
	for _, sub := range []string{
		"daemon", "init", "recover", "mount", "umount", "status", "config",
		"begin", "commit", "rollback", "logs", "diff", "revert",
	} {
		if !bytes.Contains([]byte(got), []byte(sub)) {
			t.Errorf("--help 输出未包含子命令 %q", sub)
		}
	}
}

// TestPitrCLI_DaemonUnavailableReturnsErr — daemon 不可用时返回明确错误
func TestPitrCLI_DaemonUnavailableReturnsErr(t *testing.T) {
	cases := [][]string{
		{"init", "/tmp/x"},
		{"begin", "/tmp/x"},
		{"logs", "/tmp/x"},
		{"revert", "abc123"},
	}
	for _, args := range cases {
		root := newRoot()
		root.SetOut(new(bytes.Buffer))
		root.SetErr(new(bytes.Buffer))
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Errorf("args=%v 在 daemon 不可用时应报错,但返回 nil", args)
		}
	}
}

// TestPitrCLI_ArgsValidation — 子命令的 Args 校验生效
func TestPitrCLI_ArgsValidation(t *testing.T) {
	root := newRoot()
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{"init"}) // 缺 <path>
	if err := root.Execute(); err == nil {
		t.Fatal("init 缺 path 应报错")
	}
}

type fakePitrd struct {
	pb.UnimplementedPitrdServer
}

func fakeTxn(state, command, message string) *pb.Transaction {
	return &pb.Transaction{
		TxnId:       9,
		VersionHash: "012345abcdef",
		ScopePath:   "/workspace/proj",
		State:       state,
		Command:     command,
		Message:     message,
	}
}

func (fakePitrd) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{
		DaemonVersion:      "test",
		PostgresHealthy:    true,
		ActiveTransactions: 1,
		Volumes: []*pb.VolumeStatus{{
			Name: "default", JfsMount: "/jfs", FuseMount: "/workspace", Retention: "compact",
		}},
	}, nil
}

func (fakePitrd) Begin(context.Context, *pb.BeginRequest) (*pb.BeginResponse, error) {
	return &pb.BeginResponse{Transaction: fakeTxn("active", "begin", "edit")}, nil
}

func (fakePitrd) Commit(context.Context, *pb.CommitRequest) (*pb.CommitResponse, error) {
	return &pb.CommitResponse{Transaction: fakeTxn("committed", "commit", "done")}, nil
}

func (fakePitrd) Rollback(context.Context, *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	return &pb.RollbackResponse{Transaction: fakeTxn("rolled_back", "rollback", "")}, nil
}

func (fakePitrd) Logs(context.Context, *pb.LogsRequest) (*pb.LogsResponse, error) {
	return &pb.LogsResponse{Entries: []*pb.LogEntry{
		{Transaction: fakeTxn("committed", "commit", "done")},
		{Transaction: fakeTxn("auto", "write:/workspace/proj/a", "")},
	}}, nil
}

func (fakePitrd) Diff(context.Context, *pb.DiffRequest) (*pb.DiffResponse, error) {
	return &pb.DiffResponse{NodeChanges: 2, EdgeChanges: 3, ChunkChanges: 4}, nil
}

func (fakePitrd) Revert(context.Context, *pb.RevertRequest) (*pb.RevertResponse, error) {
	return &pb.RevertResponse{Applied: 9, NewVersionHash: "fedcba654321"}, nil
}

func (fakePitrd) Recover(context.Context, *pb.RecoverRequest) (*pb.RecoverResponse, error) {
	return &pb.RecoverResponse{Volumes: []*pb.VolumeStatus{{
		Name: "default", JfsMount: "/jfs", FuseMount: "/workspace",
		JfsMounted: true, FuseMounted: true,
	}}}, nil
}

func (fakePitrd) Init(context.Context, *pb.InitRequest) (*pb.InitResponse, error) {
	return &pb.InitResponse{Volume: &pb.VolumeStatus{
		Name: "default", JfsMount: "/jfs", FuseMount: "/workspace",
		JfsMounted: true, FuseMounted: true, Retention: "compact",
	}}, nil
}

func (fakePitrd) Mount(context.Context, *pb.MountRequest) (*pb.MountResponse, error) {
	return &pb.MountResponse{Volume: &pb.VolumeStatus{
		Name: "default", JfsMount: "/jfs", FuseMount: "/workspace",
		JfsMounted: true, FuseMounted: true,
	}}, nil
}

func (fakePitrd) Umount(context.Context, *pb.UmountRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (fakePitrd) ConfigSet(
	_ context.Context,
	req *pb.ConfigSetRequest,
) (*pb.ConfigSetResponse, error) {
	return &pb.ConfigSetResponse{
		Key: req.GetKey(), Value: req.GetValue(), Window: req.GetWindow(),
	}, nil
}

func startFakePitrd(t *testing.T) string {
	t.Helper()
	socket := filepath.Join("/tmp",
		"pitr-cli-"+time.Now().Format("150405.000000000")+".sock")
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterPitrdServer(server, fakePitrd{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	return socket
}

func executeCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestCLI_ControlCommands_E2E(t *testing.T) {
	socket := startFakePitrd(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--socket", socket, "status"}, "connected to pitrd test, 1 volumes"},
		{[]string{"--socket", socket, "begin", "/workspace/proj", "-m", "edit"}, "started txn 012345abcdef"},
		{[]string{"--socket", socket, "commit", "/workspace/proj", "-m", "done"}, "committed txn 012345abcdef"},
		{[]string{"--socket", socket, "rollback", "/workspace/proj"}, "rolled back txn 012345abcdef"},
		{[]string{"--socket", socket, "logs", "/workspace/proj", "-n", "2"}, "012345abcdef   commit   # done"},
		{[]string{"--socket", socket, "diff", "111111111111", "222222222222", "--path", "/workspace/proj"}, "nodes=2 edges=3 chunks=4"},
		{[]string{"--socket", socket, "revert", "111111111111", "--path", "/workspace/proj"}, "reverted 9 history rows; new version fedcba654321"},
		{[]string{"--socket", socket, "revert", "111111111111", "--dry-run"}, "dry-run: would apply 9 history rows"},
		{[]string{"--socket", socket, "recover", "/workspace"}, "recovered default @ /workspace"},
		{[]string{"--socket", socket, "init", "/workspace"}, "initialized default @ /workspace"},
		{[]string{"--socket", socket, "umount", "/workspace"}, "unmounted /workspace"},
		{[]string{"--socket", socket, "mount", "/workspace"}, "mounted default @ /workspace"},
		{[]string{"--socket", socket, "config", "set", "retention", "archive", "--window", "30d"}, "set retention=archive window=30d"},
	}
	for _, tc := range cases {
		got, err := executeCLI(t, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%v 输出不包含 %q:\n%s", tc.args, tc.want, got)
		}
	}
}

func TestCLI_ErrorMessage(t *testing.T) {
	socket := filepath.Join("/tmp", "pitr-does-not-exist.sock")
	got, err := executeCLI(t, "--socket", socket, "status")
	if err == nil {
		t.Fatal("pitrd 不存在时应报错")
	}
	if !strings.Contains(err.Error(), "pitrd 请求失败") ||
		!strings.Contains(err.Error(), socket) {
		t.Fatalf("错误不友好:%v\n%s", err, got)
	}
}
