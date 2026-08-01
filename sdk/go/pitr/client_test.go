package pitr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "pitr_fs/api/pitrd/v1"
)

type fakeServer struct {
	pb.UnimplementedPitrdServer
	lastRevert *pb.RevertRequest
}

func (*fakeServer) Begin(
	_ context.Context,
	request *pb.BeginRequest,
) (*pb.BeginResponse, error) {
	transaction := &pb.Transaction{
		TxnId: 7, VersionHash: "012345abcdef", ScopePath: request.GetPath(),
		State: "active", Command: "begin", Message: request.GetMessage(),
	}
	return &pb.BeginResponse{Transaction: transaction}, nil
}

func (*fakeServer) Commit(
	_ context.Context,
	_ *pb.CommitRequest,
) (*pb.CommitResponse, error) {
	return &pb.CommitResponse{Transaction: &pb.Transaction{
		TxnId: 7, VersionHash: "012345abcdef",
		ScopePath: "/workspace/proj", State: "committed",
	}}, nil
}

func (*fakeServer) Rollback(
	_ context.Context,
	_ *pb.RollbackRequest,
) (*pb.RollbackResponse, error) {
	return &pb.RollbackResponse{Transaction: &pb.Transaction{
		TxnId: 7, VersionHash: "012345abcdef",
		ScopePath: "/workspace/proj", State: "rolled_back",
	}}, nil
}

func (*fakeServer) Logs(
	context.Context,
	*pb.LogsRequest,
) (*pb.LogsResponse, error) {
	return &pb.LogsResponse{Entries: []*pb.LogEntry{{
		Transaction: &pb.Transaction{
			TxnId: 8, VersionHash: "fedcba654321",
			ScopePath: "/workspace/proj", State: "auto", Command: "write:a",
			PosixOperation: "write(a, 2)", ProcessCommand: "echo hi > a",
			ActorUid: 1000, ActorGid: 1000, ActorPid: 22,
			ActorName: "tester", ChangeSummary: "\"v1\" -> \"v2\"",
		},
	}}}, nil
}

func (s *fakeServer) Revert(
	_ context.Context,
	request *pb.RevertRequest,
) (*pb.RevertResponse, error) {
	s.lastRevert = request
	return &pb.RevertResponse{
		Applied: 3, NewVersionHash: "aabbccddeeff",
		ResolvedVersionHash: "111111111111",
		ResolvedVersionTime: "2026-07-31T10:00:00Z",
	}, nil
}

func (*fakeServer) Diff(
	context.Context,
	*pb.DiffRequest,
) (*pb.DiffResponse, error) {
	return &pb.DiffResponse{
		NodeChanges: 1, EdgeChanges: 2, ChunkChanges: 3,
	}, nil
}

func (*fakeServer) ConfigSet(
	_ context.Context,
	request *pb.ConfigSetRequest,
) (*pb.ConfigSetResponse, error) {
	return &pb.ConfigSetResponse{
		Key: request.GetKey(), Value: request.GetValue(),
	}, nil
}

func (*fakeServer) Clear(
	context.Context,
	*pb.ClearRequest,
) (*pb.ClearResponse, error) {
	return &pb.ClearResponse{
		VersionsDeleted: 4, HistoryDeleted: 12,
	}, nil
}

func (*fakeServer) Space(
	context.Context,
	*pb.SpaceRequest,
) (*pb.SpaceResponse, error) {
	return &pb.SpaceResponse{
		MaxSpaceBytes: 100 << 30, ReservePercent: 20,
		HighWatermarkBytes: 80 << 30, RetainedBytes: 60 << 30,
		Versions: []*pb.SpaceVersion{{
			VersionHash: "012345abcdef", PinnedBytes: 2 << 30,
			EstimatedReleaseBytes: 1 << 30,
		}},
	}, nil
}

func startUnixServer(t *testing.T, server pb.PitrdServer) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "pitrd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterPitrdServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	return socket
}

func TestGoSDK_Dial_UnixSocket(t *testing.T) {
	client, err := Dial(startUnixServer(t, &fakeServer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Begin(
		context.Background(), "/workspace/proj", WithMessage("edit")); !errors.Is(
		err, ErrManualTransactionsDisabled) {
		t.Fatalf("Begin err=%v", err)
	}
}

func TestGoSDK_ResolveRelativePath(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	resolved, err := resolvePath("project/../project")
	if want := filepath.Join(working, "project"); err != nil || resolved != want {
		t.Fatalf("path=%q want=%q err=%v", resolved, want, err)
	}
	if global, err := resolvePath(""); err != nil || global != "" {
		t.Fatalf("空 path 应保留全局语义，got=%q err=%v", global, err)
	}
}

func TestGoSDK_LogsDiffRevert(t *testing.T) {
	implementation := &fakeServer{}
	client, err := Dial(startUnixServer(t, implementation))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	logs, err := client.Logs(context.Background(), "/workspace/proj", 5)
	if err != nil || len(logs) != 1 || logs[0].Command != "write:a" ||
		logs[0].POSIXOperation != "write(a, 2)" ||
		logs[0].ProcessCommand != "echo hi > a" ||
		logs[0].ActorName != "tester" ||
		logs[0].ChangeSummary != "\"v1\" -> \"v2\"" {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	diff, err := client.Diff(
		context.Background(), "111111111111", "222222222222", "/workspace/proj")
	if err != nil || diff.NodeChanges != 1 || diff.EdgeChanges != 2 ||
		diff.ChunkChanges != 3 {
		t.Fatalf("diff=%+v err=%v", diff, err)
	}
	reverted, err := client.Revert(
		context.Background(), "111111111111", WithPath("/workspace/proj"))
	if err != nil || reverted.Applied != 3 ||
		reverted.NewVersionHash != "aabbccddeeff" ||
		reverted.ResolvedVersionHash != "111111111111" {
		t.Fatalf("revert=%+v err=%v", reverted, err)
	}
	working := t.TempDir()
	t.Chdir(working)
	if _, err := client.Revert(context.Background(), "111111111111"); err != nil {
		t.Fatal(err)
	}
	if implementation.lastRevert.GetPath() != working {
		t.Fatalf("default revert path=%q want=%q",
			implementation.lastRevert.GetPath(), working)
	}
	if _, err := client.Revert(
		context.Background(), "111111111111", WithGlobal()); err != nil {
		t.Fatal(err)
	}
	if implementation.lastRevert.GetPath() != "" {
		t.Fatalf("global revert path=%q", implementation.lastRevert.GetPath())
	}
	target := time.Date(2026, 7, 31, 18, 0, 0, 123, time.FixedZone("CST", 8*60*60))
	if _, err := client.RevertAt(
		context.Background(), target, WithPath("/workspace/proj")); err != nil {
		t.Fatal(err)
	}
	if implementation.lastRevert.GetVersionHash() != "" ||
		implementation.lastRevert.GetTargetTime() != target.Format(time.RFC3339Nano) {
		t.Fatalf("revert at request=%+v", implementation.lastRevert)
	}
	if _, err := client.Revert(context.Background(), ""); err == nil {
		t.Fatal("empty version hash should fail")
	}
	if _, err := client.RevertAt(context.Background(), time.Time{}); err == nil {
		t.Fatal("zero target time should fail")
	}
}

func TestGoSDK_ConfigAndClear(t *testing.T) {
	client, err := Dial(startUnixServer(t, &fakeServer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetHistoryLimit(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
	if err := client.SetMaxSpaceBytes(context.Background(), 100<<30); err != nil {
		t.Fatal(err)
	}
	if err := client.SetSpaceReserve(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	space, err := client.Space(context.Background(), "/workspace", 10)
	if err != nil {
		t.Fatal(err)
	}
	if space.HighWatermarkBytes != 80<<30 || len(space.Versions) != 1 ||
		space.Versions[0].ReleasableBytes != 1<<30 {
		t.Fatalf("space=%+v", space)
	}
	if _, err := client.Clear(context.Background(), false); err == nil {
		t.Fatal("clear without confirmation should fail")
	}
	cleared, err := client.Clear(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.VersionsDeleted != 4 || cleared.HistoryDeleted != 12 {
		t.Fatalf("clear=%+v", cleared)
	}
}
