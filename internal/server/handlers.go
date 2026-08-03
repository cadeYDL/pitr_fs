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
		TxnId:          in.ID,
		WorkspaceId:    in.WorkspaceID,
		VersionHash:    in.VersionHash,
		ScopePath:      in.ScopePath,
		State:          in.State,
		Command:        in.Command,
		Message:        in.Message,
		PosixOperation: in.PosixOp,
		ProcessCommand: in.ProcessCommand,
		ActorUid:       in.ActorUID,
		ActorGid:       in.ActorGID,
		ActorPid:       in.ActorPID,
		ActorName:      in.ActorName,
		ChangeSummary:  in.ChangeSummary,
		CreatedAt:      timestamppb.New(in.CreatedAt),
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
	case errors.Is(err, txn.ErrInvalidSquashRange):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, txn.ErrSquashNonLinear),
		errors.Is(err, txn.ErrOpenWrites):
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
	openWrites, err := s.mgr.CountOpenWrites(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	s.lifecycleMu.Lock()
	volumes := s.volumeStatusesLocked()
	s.lifecycleMu.Unlock()
	historyLimit, err := s.mgr.HistoryLimit(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	spacePolicy, err := s.mgr.SpacePolicy(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	for _, volume := range volumes {
		volume.HistoryLimit = historyLimit
		volume.MaxSpaceBytes = spacePolicy.MaxBytes
		volume.SpaceReservePercent = int32(spacePolicy.ReservePercent)
		volume.RetainedSpaceBytes = spacePolicy.RetainedBytes
		volume.ReclaimableSpaceBytes = spacePolicy.ReclaimableBytes
	}
	workspaces, err := s.allWorkspaceStatuses(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.StatusResponse{
		DaemonVersion:   s.cfg.DaemonVersion,
		PostgresHealthy: true,
		Volumes:         volumes,
		OpenWrites:      openWrites,
		Workspaces:      workspaces,
	}, nil
}

func (s *Server) Space(
	ctx context.Context,
	req *pb.SpaceRequest,
) (*pb.SpaceResponse, error) {
	scope := req.GetPath()
	if scope == "" {
		scope = "/"
	}
	manager, scope, _, err := s.managerForPath(ctx, scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	policy, err := manager.SpacePolicy(ctx, scope)
	if err != nil {
		return nil, rpcError(err)
	}
	estimates, err := manager.SpaceVersions(ctx, scope, int(req.GetLimit()))
	if err != nil {
		return nil, rpcError(err)
	}
	response := &pb.SpaceResponse{
		MaxSpaceBytes:      policy.MaxBytes,
		ReservePercent:     int32(policy.ReservePercent),
		HighWatermarkBytes: policy.HighWatermarkBytes(),
		RetainedBytes:      policy.RetainedBytes,
		ReclaimableBytes:   policy.ReclaimableBytes,
	}
	for _, estimate := range estimates {
		response.Versions = append(response.Versions, &pb.SpaceVersion{
			VersionHash:           estimate.VersionHash,
			ClosedAt:              estimate.ClosedAt.Format(time.RFC3339Nano),
			PinnedBytes:           estimate.PinnedBytes,
			EstimatedReleaseBytes: estimate.ReleasableBytes,
		})
	}
	return response, nil
}

func (s *Server) Begin(
	_ context.Context,
	_ *pb.BeginRequest,
) (*pb.BeginResponse, error) {
	return nil, status.Error(codes.FailedPrecondition,
		"自动快照模式无需 begin；每次写操作会自动形成版本")
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
	_ context.Context,
	_ *pb.CommitRequest,
) (*pb.CommitResponse, error) {
	return nil, status.Error(codes.FailedPrecondition,
		"自动快照模式无需 commit；文件关闭后版本立即可用")
}

func (s *Server) Rollback(
	_ context.Context,
	_ *pb.RollbackRequest,
) (*pb.RollbackResponse, error) {
	return nil, status.Error(codes.FailedPrecondition,
		"自动快照模式没有 rollback；请用 pitr revert <version>")
}

func (s *Server) Logs(
	ctx context.Context,
	req *pb.LogsRequest,
) (*pb.LogsResponse, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path 不能为空")
	}
	manager, scope, _, err := s.managerForPath(ctx, req.GetPath())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	items, err := manager.List(ctx, scope, int(req.GetLimit()))
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
