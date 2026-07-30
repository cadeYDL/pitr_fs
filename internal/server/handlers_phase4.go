package server

import (
	"context"
	"errors"
	"fmt"
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
	var results []*pb.VolumeStatus
	successes := 0
	for _, volume := range s.volumes {
		if req.GetPath() != "" && requested != "/" &&
			requested != path.Clean(volume.FUSEMount) {
			continue
		}
		item := volumeStatusPB(volume)
		if err := recoverVolume(ctx, volume); err != nil {
			item.Error = err.Error()
		} else {
			successes++
		}
		results = append(results, item)
	}
	if len(results) == 0 {
		return nil, status.Errorf(codes.NotFound,
			"未找到挂载点 %s", req.GetPath())
	}
	if successes == 0 {
		return nil, status.Error(codes.FailedPrecondition,
			results[0].GetError())
	}
	return &pb.RecoverResponse{Volumes: results}, nil
}

func recoverVolume(ctx context.Context, volume VolumeConfig) error {
	if volume.DB == nil {
		return errors.New("卷数据库未配置")
	}
	var exists bool
	if err := volume.DB.QueryRow(ctx, `
		SELECT to_regclass('public.jfs_setting') IS NOT NULL
		   AND EXISTS (SELECT 1 FROM jfs_setting)`).Scan(&exists); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return errors.New("JuiceFS 卷元数据不存在;recover 禁止 format,请显式 init")
		}
		return fmt.Errorf("校验 JuiceFS 卷元数据: %w", err)
	}
	if !exists {
		return errors.New("JuiceFS 卷元数据不存在;recover 禁止 format,请显式 init")
	}
	if !volume.JFSMounted || !volume.FUSEMounted {
		return errors.New("卷存在但挂载未就绪")
	}
	return nil
}

func volumeStatusPB(volume VolumeConfig) *pb.VolumeStatus {
	return &pb.VolumeStatus{
		Name:        volume.Name,
		JfsMount:    volume.JFSMount,
		FuseMount:   volume.FUSEMount,
		JfsMounted:  volume.JFSMounted,
		FuseMounted: volume.FUSEMounted,
		Retention:   volume.Retention,
	}
}

func (s *Server) volumeStatuses() []*pb.VolumeStatus {
	out := make([]*pb.VolumeStatus, 0, len(s.volumes))
	for _, volume := range s.volumes {
		out = append(out, volumeStatusPB(volume))
	}
	return out
}
