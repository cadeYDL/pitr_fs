package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

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
	versionHash := strings.TrimSpace(req.GetVersionHash())
	targetTime := strings.TrimSpace(req.GetTargetTime())
	if (versionHash == "") == (targetTime == "") {
		return nil, status.Error(codes.InvalidArgument,
			"version_hash 与 target_time 必须且只能指定一个")
	}
	var resolved *txn.Txn
	if targetTime != "" {
		parsed, err := time.Parse(time.RFC3339Nano, targetTime)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"target_time 必须是带时区的 RFC3339 时间: %v", err)
		}
		if parsed.After(time.Now()) {
			return nil, status.Error(codes.InvalidArgument,
				"target_time 不能晚于当前时间")
		}
		resolved, err = s.mgr.FindClosedAtOrBefore(ctx, parsed)
		if err != nil {
			if errors.Is(err, txn.ErrTimeBeforeHistory) {
				return nil, status.Error(codes.OutOfRange, err.Error())
			}
			return nil, rpcError(err)
		}
		versionHash = resolved.VersionHash
	} else {
		if !revert.ValidVersionHash(versionHash) {
			return nil, status.Errorf(codes.InvalidArgument,
				"%v: %q", revert.ErrInvalidHash, versionHash)
		}
		var err error
		resolved, err = s.mgr.FindByHash(ctx, versionHash)
		if err != nil {
			return nil, rpcError(err)
		}
	}
	applied, hash, err := s.rev.Revert(ctx, revert.Options{
		TargetHash: versionHash,
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
	resolvedTime := resolved.CreatedAt
	if resolved.ClosedAt != nil {
		resolvedTime = *resolved.ClosedAt
	}
	return &pb.RevertResponse{
		Applied:             applied,
		NewVersionHash:      hash,
		ResolvedVersionHash: resolved.VersionHash,
		ResolvedVersionTime: resolvedTime.Format(time.RFC3339Nano),
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

func (s *Server) Clear(
	ctx context.Context,
	req *pb.ClearRequest,
) (*pb.ClearResponse, error) {
	if !req.GetGlobal() {
		return nil, status.Error(codes.FailedPrecondition,
			"当前仅支持全局 clear；请显式使用 --global")
	}
	if !req.GetConfirm() {
		return nil, status.Error(codes.InvalidArgument,
			"clear 会永久删除全部版本历史；请添加 --yes")
	}
	stats, err := s.mgr.ClearHistory(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.ClearResponse{
		VersionsDeleted: stats.VersionsDeleted,
		HistoryDeleted:  stats.HistoryDeleted,
	}, nil
}

func (s *Server) Squash(
	ctx context.Context,
	req *pb.SquashRequest,
) (*pb.SquashResponse, error) {
	baseVersion := strings.TrimSpace(req.GetBaseVersion())
	endVersion := strings.TrimSpace(req.GetEndVersion())
	message := strings.TrimSpace(req.GetMessage())
	if !revert.ValidVersionHash(baseVersion) ||
		!revert.ValidVersionHash(endVersion) {
		return nil, status.Error(codes.InvalidArgument,
			"base_version 和 end_version 必须是 12 位十六进制版本号")
	}
	if message == "" {
		return nil, status.Error(codes.InvalidArgument, "message 不能为空；请使用 -m")
	}
	if req.GetDryRun() && req.GetConfirm() {
		return nil, status.Error(codes.InvalidArgument,
			"--dry-run 与 --yes 不能同时使用")
	}
	if !req.GetDryRun() && !req.GetConfirm() {
		return nil, status.Error(codes.InvalidArgument,
			"squash 会永久删除中间版本；请先使用 --dry-run 预览，再添加 --yes 执行")
	}

	if !req.GetDryRun() {
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		if s.cfg.QuiesceFunc != nil {
			s.cfg.QuiesceFunc(true)
			defer s.cfg.QuiesceFunc(false)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			open, err := s.mgr.CountOpenWrites(ctx)
			if err != nil {
				return nil, rpcError(err)
			}
			if open == 0 {
				break
			}
			if time.Now().After(deadline) {
				return nil, status.Errorf(codes.FailedPrecondition,
					"冻结新写入后仍有 %d 个写操作尚未关闭；squash 未执行", open)
			}
			select {
			case <-ctx.Done():
				return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
			case <-time.After(25 * time.Millisecond):
			}
		}
	}

	stats, err := s.mgr.Squash(ctx, txn.SquashOptions{
		BaseHash:  baseVersion,
		EndHash:   endVersion,
		Message:   message,
		DryRun:    req.GetDryRun(),
		ActorUID:  req.GetActorUid(),
		ActorGID:  req.GetActorGid(),
		ActorPID:  req.GetActorPid(),
		ActorName: req.GetActorName(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.SquashResponse{
		BaseVersion:      stats.BaseVersionHash,
		EndVersion:       stats.EndVersionHash,
		VersionsMerged:   stats.VersionsMerged,
		VersionsDeleted:  stats.VersionsDeleted,
		HistoryBefore:    stats.HistoryBefore,
		HistoryAfter:     stats.HistoryAfter,
		HistoryDeleted:   stats.HistoryDeleted,
		FirstOperationAt: stats.FirstOperationAt.Format(time.RFC3339Nano),
		EndClosedAt:      stats.EndClosedAt.Format(time.RFC3339Nano),
		DryRun:           stats.DryRun,
		Transaction:      transactionPB(stats.Transaction),
	}, nil
}

// Recover 是 daemon 层的无损校验。pitrd 启动顺序已经恢复两层 mount;RPC
// 只确认目标卷元数据和挂载状态,绝不调用 juicefs format。
func (s *Server) Recover(
	ctx context.Context,
	req *pb.RecoverRequest,
) (*pb.RecoverResponse, error) {
	requested := path.Clean(req.GetPath())
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	var results []*pb.VolumeStatus
	successes := 0
	for index := range s.volumes {
		volume := s.volumes[index]
		if req.GetPath() != "" && requested != "/" &&
			requested != path.Clean(volume.FUSEMount) {
			continue
		}
		if !volume.FUSEMounted && s.cfg.MountFunc != nil && volume.FUSEMount != "" {
			if err := s.mountLocked(ctx, index, volume.FUSEMount); err != nil {
				item := volumeStatusPB(volume)
				item.Error = err.Error()
				results = append(results, item)
				continue
			}
			volume = s.volumes[index]
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
	}
}

func (s *Server) volumeStatusesLocked() []*pb.VolumeStatus {
	out := make([]*pb.VolumeStatus, 0, len(s.volumes))
	for _, volume := range s.volumes {
		out = append(out, volumeStatusPB(volume))
	}
	return out
}
