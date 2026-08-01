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
	"google.golang.org/protobuf/types/known/timestamppb"

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
		"logs", "diff", "revert", "clear",
	} {
		if !bytes.Contains([]byte(got), []byte(sub)) {
			t.Errorf("--help 输出未包含子命令 %q", sub)
		}
	}
}

func TestPitrCLI_CompletionHelpIsChinese(t *testing.T) {
	for _, args := range [][]string{
		{"completion", "--help"},
		{"completion", "bash", "--help"},
	} {
		root := newRoot()
		buf := new(bytes.Buffer)
		root.SetOut(buf)
		root.SetErr(buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		output := buf.String()
		if !strings.Contains(output, "生成") ||
			!strings.Contains(output, "自动补全脚本") {
			t.Fatalf("%v 未输出中文说明：\n%s", args, output)
		}
		if strings.Contains(output, "Generate the autocompletion") {
			t.Fatalf("%v 仍包含英文说明：\n%s", args, output)
		}
	}
}

func TestPitrCLI_CompletionWritesToConfiguredOutput(t *testing.T) {
	root := newRoot()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "bash completion V2 for pitr") {
		t.Fatal("completion 脚本未写入命令配置的输出流")
	}
}

// TestPitrCLI_DaemonUnavailableReturnsErr — daemon 不可用时返回明确错误
func TestPitrCLI_DaemonUnavailableReturnsErr(t *testing.T) {
	cases := [][]string{
		{"init", "/tmp/x"},
		{"logs", "/tmp/x"},
		{"revert", "abc123"},
		{"clear", "--global", "--yes"},
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
	changedAt := time.Date(2026, 7, 31, 6, 23, 17, 842000000, time.UTC)
	return &pb.Transaction{
		TxnId:          9,
		VersionHash:    "012345abcdef",
		ScopePath:      "/workspace/proj",
		State:          state,
		Command:        command,
		Message:        message,
		PosixOperation: `write("/workspace/proj/a", offset=0, total=2, calls=1)`,
		ProcessCommand: "echo 123456789",
		ActorUid:       501,
		ActorGid:       20,
		ActorPid:       1234,
		ActorName:      "ydl",
		ChangeSummary:  `"v1" -> "v2"`,
		CreatedAt:      timestamppb.New(changedAt.Add(-time.Millisecond)),
		ClosedAt:       timestamppb.New(changedAt),
	}
}

func (fakePitrd) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{
		DaemonVersion:   "test",
		PostgresHealthy: true,
		OpenWrites:      0,
		Volumes: []*pb.VolumeStatus{{
			Name: "default", JfsMount: "/jfs", FuseMount: "/workspace",
			Retention: "compact", HistoryLimit: 100,
			MaxSpaceBytes: 100 << 30, SpaceReservePercent: 20,
			RetainedSpaceBytes: 60 << 30, ReclaimableSpaceBytes: 4 << 30,
		}},
	}, nil
}

func (fakePitrd) Begin(
	_ context.Context,
	request *pb.BeginRequest,
) (*pb.BeginResponse, error) {
	transaction := fakeTxn("active", "begin", "edit")
	transaction.ScopePath = request.GetPath()
	return &pb.BeginResponse{Transaction: transaction}, nil
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
	return &pb.RevertResponse{
		Applied:             9,
		NewVersionHash:      "fedcba654321",
		ResolvedVersionHash: "111111111111",
		ResolvedVersionTime: "2026-07-31T06:23:17.842Z",
	}, nil
}

func (fakePitrd) Clear(context.Context, *pb.ClearRequest) (*pb.ClearResponse, error) {
	return &pb.ClearResponse{VersionsDeleted: 8, HistoryDeleted: 21}, nil
}

func (fakePitrd) Space(context.Context, *pb.SpaceRequest) (*pb.SpaceResponse, error) {
	return &pb.SpaceResponse{
		MaxSpaceBytes: 100 << 30, ReservePercent: 20,
		HighWatermarkBytes: 80 << 30, RetainedBytes: 60 << 30,
		ReclaimableBytes: 4 << 30,
		Versions: []*pb.SpaceVersion{{
			VersionHash: "012345abcdef", ClosedAt: "2026-08-01T10:00:00Z",
			PinnedBytes: 2 << 30, EstimatedReleaseBytes: 1 << 30,
		}},
	}, nil
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
		{[]string{"--socket", socket, "status"}, "history-limit=100"},
		{[]string{"--socket", socket, "space", "/workspace"}, "max=100.00 GiB reserve=20% high=80.00 GiB"},
		{[]string{"--socket", socket, "space", "/workspace"}, "012345abcdef\t1.00 GiB\t2.00 GiB"},
		{[]string{"--socket", socket, "logs", "/workspace/proj", "-n", "2"}, "012345abcdef   commit   # done"},
		{[]string{"--socket", socket, "logs", "/workspace/proj", "-l", "-n", "1"},
			"012345abcdef\twrite(\"/workspace/proj/a\", offset=0, total=2, calls=1)\techo 12345..."},
		{[]string{"--socket", socket, "diff", "111111111111", "222222222222", "--path", "/workspace/proj"}, "nodes=2 edges=3 chunks=4"},
		{[]string{"--socket", socket, "revert", "111111111111", "--path", "/workspace/proj"}, "reverted to 111111111111 at 2026-07-31T06:23:17.842Z; applied 9 history rows; new version fedcba654321"},
		{[]string{"--socket", socket, "revert", "111111111111", "--dry-run"}, "dry-run: target 111111111111 at 2026-07-31T06:23:17.842Z; would apply 9 history rows"},
		{[]string{"--socket", socket, "revert", "--at", "2026-07-31T14:23:17+08:00"}, "reverted to 111111111111"},
		{[]string{"--socket", socket, "recover", "/workspace"}, "recovered default @ /workspace"},
		{[]string{"--socket", socket, "init", "/workspace"}, "initialized default @ /workspace"},
		{[]string{"--socket", socket, "umount", "/workspace"}, "unmounted /workspace"},
		{[]string{"--socket", socket, "mount", "/workspace"}, "mounted default @ /workspace"},
		{[]string{"--socket", socket, "config", "set", "retention", "archive", "--window", "30d"}, "set retention=archive window=30d"},
		{[]string{"--socket", socket, "config", "set", "history-limit", "42"}, "set history-limit=42"},
		{[]string{"--socket", socket, "config", "set", "max-space", "100GiB"}, "set max-space=100GiB"},
		{[]string{"--socket", socket, "config", "set", "space-reserve", "15%"}, "set space-reserve=15%"},
		{[]string{"--socket", socket, "clear", "--global", "--yes"}, "cleared 8 versions and 21 history rows"},
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

func TestCLI_RelativePathAndGlobalValidation(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	want := filepath.Join(working, "project")
	got, err := resolveCLIPath("project/../project")
	if err != nil || got != want {
		t.Fatalf("相对路径未按 cwd 解析，want=%q got=%q err=%v", want, got, err)
	}
	if global, err := resolveCLIPath(""); err != nil || global != "" {
		t.Fatalf("空 path 应保留全局语义，got=%q err=%v", global, err)
	}
	if _, err := executeCLI(t,
		"revert", "abc123", "--global", "--path", "project"); err == nil {
		t.Fatal("--global 与 --path 同时使用应失败")
	}
	if _, err := executeCLI(t, "revert"); err == nil {
		t.Fatal("revert 缺少版本号或 --at 应失败")
	}
	if _, err := executeCLI(t,
		"revert", "111111111111", "--at", "2026-07-31T10:00:00Z"); err == nil {
		t.Fatal("revert 同时指定版本号和 --at 应失败")
	}
	if _, err := executeCLI(t, "clear", "--global"); err == nil {
		t.Fatal("clear 缺少 --yes 应失败")
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
