package server

import (
	"context"
	"errors"
	"path"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/revert"
	"pitr_fs/internal/txn"
)

func (s *Server) Revert(
	ctx context.Context,
	req *pb.RevertRequest,
) (*pb.RevertResponse, error) {
	if req.GetVersionHash() == "" {
		return nil, status.Error(codes.InvalidArgument, "version_hash 不能为空")
	}
	applied, hash, err := s.rev.Revert(ctx, revert.Options{
		TargetHash: req.GetVersionHash(),
		ScopePath:  req.GetPath(),
		DryRun:     req.GetDryRun(),
	})
	if err != nil {
		switch {
		case errors.Is(err, revert.ErrInvalidHash),
			errors.Is(err, txn.ErrInvalidScope):
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case errors.Is(err, revert.ErrTargetMissing):
			return nil, status.Error(codes.NotFound, err.Error())
		case errors.Is(err, revert.ErrTargetState),
			errors.Is(err, revert.ErrActiveScope):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return &pb.RevertResponse{
		Applied:        applied,
		NewVersionHash: hash,
	}, nil
}

func (s *Server) Diff(
	ctx context.Context,
	req *pb.DiffRequest,
) (*pb.DiffResponse, error) {
	if req.GetVersionA() == "" || req.GetVersionB() == "" {
		return nil, status.Error(codes.InvalidArgument,
			"version_a 和 version_b 均不能为空")
	}
	stats, err := s.mgr.Diff(
		ctx, req.GetVersionA(), req.GetVersionB(), req.GetPath())
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.DiffResponse{
		NodeChanges:  stats.NodeChanges,
		EdgeChanges:  stats.EdgeChanges,
		ChunkChanges: stats.ChunkChanges,
	}, nil
}

// Recover 是 daemon 层的无损校验。pitrd 启动顺序已经恢复两层 mount;RPC
// 只确认目标卷元数据和挂载状态,绝不调用 juicefs format。
func (s *Server) Recover(
	ctx context.Context,
	req *pb.RecoverRequest,
) (*pb.RecoverResponse, error) {
	requested := path.Clean(req.GetPath())
	if req.GetPath() != "" &&
		requested != path.Clean(s.cfg.FUSEMount) &&
		requested != "/" {
		return nil, status.Errorf(codes.NotFound,
			"未找到挂载点 %s", req.GetPath())
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `
		SELECT to_regclass('public.jfs_setting') IS NOT NULL
		   AND EXISTS (SELECT 1 FROM jfs_setting)`).Scan(&exists); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return nil, status.Error(codes.FailedPrecondition,
				"JuiceFS 卷元数据不存在;recover 禁止 format,请显式 init")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !exists {
		return nil, status.Error(codes.FailedPrecondition,
			"JuiceFS 卷元数据不存在;recover 禁止 format,请显式 init")
	}
	if !s.cfg.JFSMounted || !s.cfg.FUSEMounted {
		return nil, status.Error(codes.Unavailable, "卷存在但挂载未就绪")
	}
	return &pb.RecoverResponse{Volumes: []*pb.VolumeStatus{{
		Name:        s.cfg.Volume,
		JfsMount:    s.cfg.JFSMount,
		FuseMount:   s.cfg.FUSEMount,
		JfsMounted:  true,
		FuseMounted: true,
		Retention:   s.cfg.Retention,
	}}}, nil
}
