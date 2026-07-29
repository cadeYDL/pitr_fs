package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/txn"
)

func transactionPB(in *txn.Txn) *pb.Transaction {
	if in == nil {
		return nil
	}
	out := &pb.Transaction{
		TxnId:       in.ID,
		VersionHash: in.VersionHash,
		ScopePath:   in.ScopePath,
		State:       in.State,
		Command:     in.Command,
		Message:     in.Message,
		CreatedAt:   timestamppb.New(in.CreatedAt),
	}
	if in.ParentID != nil {
		parent := *in.ParentID
		out.ParentId = &parent
	}
	if in.ClosedAt != nil {
		out.ClosedAt = timestamppb.New(*in.ClosedAt)
	}
	return out
}

func rpcError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, txn.ErrInvalidScope):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, txn.ErrScopeActive):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, txn.ErrTxnNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, txn.ErrIllegalTransit):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *Server) Status(
	ctx context.Context,
	_ *pb.StatusRequest,
) (*pb.StatusResponse, error) {
	if err := s.db.Ping(ctx); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	active, err := s.mgr.CountActive(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.StatusResponse{
		DaemonVersion:      s.cfg.DaemonVersion,
		PostgresHealthy:    true,
		ActiveTransactions: active,
		Volumes: []*pb.VolumeStatus{{
			Name:        s.cfg.Volume,
			JfsMount:    s.cfg.JFSMount,
			FuseMount:   s.cfg.FUSEMount,
			JfsMounted:  s.cfg.JFSMounted,
			FuseMounted: s.cfg.FUSEMounted,
			Retention:   s.cfg.Retention,
		}},
	}, nil
}

func (s *Server) Begin(
	ctx context.Context,
	req *pb.BeginRequest,
) (*pb.BeginResponse, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path 不能为空")
	}
	created, err := s.mgr.Begin(ctx, req.GetPath(), req.GetMessage())
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.BeginResponse{
		VersionHash: created.VersionHash,
		TxnId:       created.ID,
		Transaction: transactionPB(created),
	}, nil
}

func (s *Server) resolveActive(
	ctx context.Context,
	txnID int64,
	path string,
) (*txn.Txn, error) {
	if txnID != 0 {
		found, err := s.mgr.FindByID(ctx, txnID)
		if err != nil {
			return nil, err
		}
		if found.State != txn.StateActive {
			return nil, fmt.Errorf("%w:txn %d state=%s",
				txn.ErrIllegalTransit, txnID, found.State)
		}
		return found, nil
	}
	if path == "" {
		return nil, fmt.Errorf("%w:path 和 txn_id 不能同时为空", txn.ErrInvalidScope)
	}
	found, err := s.mgr.FindActiveByPath(ctx, path)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, txn.ErrTxnNotFound
	}
	return found, nil
}

func (s *Server) Commit(
	ctx context.Context,
	req *pb.CommitRequest,
) (*pb.CommitResponse, error) {
	active, err := s.resolveActive(ctx, req.GetTxnId(), req.GetPath())
	if err != nil {
		return nil, rpcError(err)
	}
	committed, err := s.mgr.Commit(ctx, active.ID, req.GetMessage())
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.CommitResponse{
		VersionHash: committed.VersionHash,
		TxnId:       committed.ID,
		Transaction: transactionPB(committed),
	}, nil
}

func (s *Server) Rollback(
	ctx context.Context,
	req *pb.RollbackRequest,
) (*pb.RollbackResponse, error) {
	active, err := s.resolveActive(ctx, req.GetTxnId(), req.GetPath())
	if err != nil {
		return nil, rpcError(err)
	}
	rolledBack, err := s.mgr.Rollback(ctx, active.ID)
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.RollbackResponse{
		VersionHash: rolledBack.VersionHash,
		TxnId:       rolledBack.ID,
		Transaction: transactionPB(rolledBack),
	}, nil
}

func (s *Server) Logs(
	ctx context.Context,
	req *pb.LogsRequest,
) (*pb.LogsResponse, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path 不能为空")
	}
	items, err := s.mgr.List(ctx, req.GetPath(), int(req.GetLimit()))
	if err != nil {
		return nil, rpcError(err)
	}
	out := &pb.LogsResponse{Entries: make([]*pb.LogEntry, 0, len(items))}
	for _, item := range items {
		out.Entries = append(out.Entries, &pb.LogEntry{
			VersionHash: item.VersionHash,
			Command:     item.Command,
			Message:     item.Message,
			State:       item.State,
			CreatedAt:   item.CreatedAt.Format(time.RFC3339Nano),
			Transaction: transactionPB(item),
		})
	}
	return out, nil
}
