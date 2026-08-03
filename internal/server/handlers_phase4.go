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
	"pitr_fs/internal/workspace"
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
	var manager *txn.Manager
	var workspaceItem workspace.Workspace
	scope := req.GetPath()
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
		if scope != "" {
			manager, scope, workspaceItem, err = s.managerForPath(ctx, scope)
		} else {
			manager, workspaceItem, err = s.managerForWorkspaceName(ctx, workspace.DefaultName)
		}
		if err != nil {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		resolved, err = manager.FindClosedAtOrBefore(ctx, parsed)
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
		manager, workspaceItem, err = s.managerForVersion(ctx, versionHash)
		if err == nil && scope != "" {
			var pathManager *txn.Manager
			var pathWorkspace workspace.Workspace
			pathManager, scope, pathWorkspace, err = s.managerForPath(ctx, scope)
			if err == nil && pathWorkspace.ID != workspaceItem.ID {
				err = fmt.Errorf("版本属于 workspace %s，path 属于 workspace %s",
					workspaceItem.Name, pathWorkspace.Name)
			}
			_ = pathManager
		}
		if err != nil {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		resolved, err = manager.FindByHash(ctx, versionHash)
		if err != nil {
			return nil, rpcError(err)
		}
	}
	engine := revert.NewEngine(s.db,
		revert.WithWorkspace(workspaceItem.ID, workspaceItem.BackendPath))
	applied, hash, err := engine.Revert(ctx, revert.Options{
		TargetHash: versionHash,
		ScopePath:  scope,
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
	manager, workspaceA, err := s.managerForVersion(ctx, req.GetVersionA())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	_, workspaceB, err := s.managerForVersion(ctx, req.GetVersionB())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if workspaceA.ID != workspaceB.ID {
		return nil, status.Error(codes.InvalidArgument,
			"不能比较不同 workspace 的版本")
	}
	scope := req.GetPath()
	if scope != "" {
		_, resolvedScope, pathWorkspace, resolveErr := s.managerForPath(ctx, scope)
		if resolveErr != nil {
			return nil, status.Error(codes.NotFound, resolveErr.Error())
		}
		if pathWorkspace.ID != workspaceA.ID {
			return nil, status.Error(codes.InvalidArgument,
				"path 与版本不属于同一 workspace")
		}
		scope = resolvedScope
	}
	stats, err := manager.Diff(
		ctx, req.GetVersionA(), req.GetVersionB(), scope)
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
	manager, _, err := s.managerForWorkspaceName(ctx, req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	stats, err := manager.ClearHistory(ctx, "/")
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
	manager, baseWorkspace, err := s.managerForVersion(ctx, baseVersion)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	_, endWorkspace, err := s.managerForVersion(ctx, endVersion)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if baseWorkspace.ID != endWorkspace.ID {
		return nil, status.Error(codes.InvalidArgument,
			"不能 squash 不同 workspace 的版本")
	}

	if !req.GetDryRun() {
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		if s.cfg.QuiesceWorkspaceFunc != nil {
			s.cfg.QuiesceWorkspaceFunc(baseWorkspace.ID, true)
			defer s.cfg.QuiesceWorkspaceFunc(baseWorkspace.ID, false)
		} else if s.cfg.QuiesceFunc != nil {
			s.cfg.QuiesceFunc(true)
			defer s.cfg.QuiesceFunc(false)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			open, err := manager.CountWorkspaceOpenWrites(ctx)
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

	stats, err := manager.Squash(ctx, txn.SquashOptions{
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
	if req.GetWorkspace() != "" || s.cfg.MountWorkspaceFunc != nil {
		return s.recoverWorkspaces(ctx, req)
	}
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

func (s *Server) recoverWorkspaces(
	ctx context.Context,
	req *pb.RecoverRequest,
) (*pb.RecoverResponse, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	// workspace 模式没有单一的 legacy FUSEMount；各 workspace proxy 在下面
	// 分别恢复。这里仅校验共享 JuiceFS 元数据和底层挂载。
	physicalVolume := s.volumes[0]
	physicalVolume.FUSEMounted = true
	if err := recoverVolume(ctx, physicalVolume); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	var items []workspace.Workspace
	switch {
	case req.GetPath() != "":
		resolved, err := s.catalog.ResolveMount(ctx, req.GetPath())
		if err != nil {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		if req.GetWorkspace() != "" && req.GetWorkspace() != resolved.Workspace.Name {
			return nil, status.Errorf(codes.InvalidArgument,
				"挂载点属于 workspace %s，不是 %s",
				resolved.Workspace.Name, req.GetWorkspace())
		}
		items = []workspace.Workspace{resolved.Workspace}
	case req.GetWorkspace() != "":
		item, err := s.catalog.GetByName(ctx, req.GetWorkspace())
		if err != nil {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		items = []workspace.Workspace{item}
	default:
		var err error
		items, err = s.catalog.List(ctx)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	results := make([]*pb.WorkspaceStatus, 0, len(items))
	for _, item := range items {
		mounts, err := s.catalog.Mounts(ctx, item.ID)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		for _, mount := range mounts {
			if req.GetPath() != "" && path.Clean(mount.Path) != path.Clean(req.GetPath()) {
				continue
			}
			if err := s.mountWorkspaceLocked(ctx, item, mount.Path); err != nil {
				return nil, err
			}
		}
		itemStatus, err := s.workspaceStatus(ctx, item)
		if err != nil {
			return nil, err
		}
		results = append(results, itemStatus)
	}
	if len(results) == 0 {
		return nil, status.Error(codes.NotFound, "未找到可恢复的 workspace")
	}
	return &pb.RecoverResponse{Workspaces: results}, nil
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
