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
	blockBegin bool
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
		},
	}}}, nil
}

func (*fakeServer) Revert(
	context.Context,
	*pb.RevertRequest,
) (*pb.RevertResponse, error) {
	return &pb.RevertResponse{
		Applied: 3, NewVersionHash: "aabbccddeeff",
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

type cancelServer struct {
	pb.UnimplementedPitrdServer
}

func (*cancelServer) Begin(
	ctx context.Context,
	_ *pb.BeginRequest,
) (*pb.BeginResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
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
	transaction, err := client.Begin(
		context.Background(), "/workspace/proj", WithMessage("edit"))
	if err != nil {
		t.Fatal(err)
	}
	if transaction.ID() != 7 || transaction.VersionHash() != "012345abcdef" ||
		transaction.Path() != "/workspace/proj" {
		t.Fatalf("txn id=%d hash=%s path=%s",
			transaction.ID(), transaction.VersionHash(), transaction.Path())
	}
}

func TestGoSDK_BeginCommitRollbackAndClosedState(t *testing.T) {
	client, err := Dial(startUnixServer(t, &fakeServer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	transaction, err := client.Begin(context.Background(), "/workspace/proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(context.Background(), "done"); err != nil {
		t.Fatal(err)
	}
	if transaction.State() != "committed" {
		t.Fatalf("state=%s", transaction.State())
	}
	if err := transaction.Rollback(context.Background()); !errors.Is(err, ErrTxnClosed) {
		t.Fatalf("重复结束 err=%v", err)
	}
}

func TestGoSDK_LogsDiffRevert(t *testing.T) {
	client, err := Dial(startUnixServer(t, &fakeServer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	logs, err := client.Logs(context.Background(), "/workspace/proj", 5)
	if err != nil || len(logs) != 1 || logs[0].Command != "write:a" {
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
		reverted.NewVersionHash != "aabbccddeeff" {
		t.Fatalf("revert=%+v err=%v", reverted, err)
	}
}

func TestGoSDK_ContextCancel(t *testing.T) {
	client, err := Dial(startUnixServer(t, &cancelServer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err = client.Begin(ctx, "/workspace/proj")
	if err == nil {
		t.Fatal("cancel 后 Begin 应失败")
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatalf("context cancel 返回过慢:%s", time.Since(started))
	}
}
