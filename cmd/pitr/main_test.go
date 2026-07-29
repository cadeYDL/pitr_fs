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

// TestPitrCLI_UnimplementedReturnsErr — 未实现的子命令返回明确错误
func TestPitrCLI_UnimplementedReturnsErr(t *testing.T) {
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
			t.Errorf("args=%v 应报错(未实现),但返回 nil", args)
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
