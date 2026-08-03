package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/txn"
	"pitr_fs/internal/workspace"
)

func (s *Server) workspaceManager(id int64) *txn.Manager {
	return s.mgr.ForWorkspace(id)
}

func (s *Server) managerForPath(
	ctx context.Context,
	value string,
) (*txn.Manager, string, workspace.Workspace, error) {
	resolved, err := s.catalog.ResolveMount(ctx, value)
	if err == nil {
		return s.workspaceManager(resolved.Workspace.ID), resolved.Scope,
			resolved.Workspace, nil
	}
	if !errors.Is(err, workspace.ErrNotFound) {
		return nil, "", workspace.Workspace{}, err
	}
	item, getErr := s.catalog.GetByName(ctx, workspace.DefaultName)
	if getErr != nil {
		return nil, "", workspace.Workspace{}, getErr
	}
	// 兼容尚未写入 pitr_workspace_mount 的旧安装：CLI 传入的是用户可见
	// 挂载路径，版本范围仍必须归一化为 workspace 内的 / 相对路径。
	scope := path.Clean(value)
	legacyMount := path.Clean(s.cfg.FUSEMount)
	if legacyMount != "." && legacyMount != "/" {
		switch {
		case scope == legacyMount:
			scope = "/"
		case strings.HasPrefix(scope, legacyMount+"/"):
			scope = strings.TrimPrefix(scope, legacyMount)
		}
	}
	return s.workspaceManager(item.ID), scope, item, nil
}

func (s *Server) managerForWorkspaceName(
	ctx context.Context,
	name string,
) (*txn.Manager, workspace.Workspace, error) {
	if name == "" {
		name = workspace.DefaultName
	}
	item, err := s.catalog.GetByName(ctx, name)
	if err != nil {
		return nil, workspace.Workspace{}, err
	}
	return s.workspaceManager(item.ID), item, nil
}

func (s *Server) managerForVersion(
	ctx context.Context,
	hash string,
) (*txn.Manager, workspace.Workspace, error) {
	var workspaceID int64
	if err := s.db.QueryRow(ctx, `
		SELECT workspace_id FROM pitr_txn WHERE version_hash=$1`, hash).
		Scan(&workspaceID); err != nil {
		return nil, workspace.Workspace{}, err
	}
	item, err := s.catalog.GetByID(ctx, workspaceID)
	if err != nil {
		return nil, workspace.Workspace{}, err
	}
	return s.workspaceManager(workspaceID), item, nil
}

func (s *Server) ensureWorkspace(
	ctx context.Context,
	name string,
	volumeName string,
) (workspace.Workspace, error) {
	if name == "" {
		name = workspace.DefaultName
	}
	if volumeName == "" {
		volumeName = s.cfg.Volume
	}
	item, err := s.catalog.Ensure(ctx, name, volumeName)
	if err != nil {
		if errors.Is(err, workspace.ErrInvalidName) {
			return workspace.Workspace{}, status.Error(codes.InvalidArgument, err.Error())
		}
		return workspace.Workspace{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	return item, nil
}

func (s *Server) mountWorkspaceLocked(
	ctx context.Context,
	item workspace.Workspace,
	mountPath string,
) error {
	cleaned, err := s.validateMountPath(mountPath)
	if err != nil {
		return err
	}
	mounts, err := s.catalog.Mounts(ctx, item.ID)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	configured := false
	for _, mounted := range mounts {
		if path.Clean(mounted.Path) == cleaned {
			configured = true
			break
		}
	}
	if s.mountedWorkspacePaths[cleaned] {
		return nil
	}
	if s.cfg.MountWorkspaceFunc != nil {
		if err := s.cfg.MountWorkspaceFunc(ctx, item, cleaned); err != nil {
			return status.Errorf(codes.Internal, "挂载 workspace %s: %v", item.Name, err)
		}
	} else if s.cfg.MountFunc != nil {
		if err := s.cfg.MountFunc(ctx, cleaned); err != nil {
			return status.Errorf(codes.Internal, "挂载 FUSE: %v", err)
		}
	} else {
		return status.Error(codes.FailedPrecondition, "daemon 未配置动态 mount")
	}
	if !configured {
		if err := s.catalog.AddMount(ctx, item.ID, cleaned); err != nil {
			if s.cfg.UmountWorkspaceFunc != nil {
				_ = s.cfg.UmountWorkspaceFunc(ctx, item, cleaned, false)
			}
			if errors.Is(err, workspace.ErrMountConflict) {
				return status.Error(codes.FailedPrecondition, err.Error())
			}
			return status.Error(codes.Internal, err.Error())
		}
	}
	s.mountedWorkspacePaths[cleaned] = true
	return nil
}

func (s *Server) initWorkspace(
	ctx context.Context,
	req *pb.InitRequest,
) (*pb.InitResponse, error) {
	item, err := s.ensureWorkspace(ctx, req.GetWorkspace(), req.GetVolume())
	if err != nil {
		return nil, err
	}
	manager := s.workspaceManager(item.ID)
	if req.HistoryLimit != nil {
		if _, err := manager.SetHistoryLimit(ctx, "/", req.GetHistoryLimit()); err != nil {
			return nil, rpcError(err)
		}
	}
	// 当前空间计数和对象 GC 仍属于共享 JuiceFS 卷；只允许 default workspace
	// 修改卷级空间策略，避免把全卷占用伪装成独立 workspace 配额。
	if item.Name != workspace.DefaultName &&
		(req.MaxSpaceBytes != nil || req.SpaceReservePercent != nil) {
		return nil, status.Error(codes.FailedPrecondition,
			"max-space 与 space-reserve 当前是 JuiceFS 卷级策略，只能由 default workspace 设置")
	}
	if req.MaxSpaceBytes != nil || req.SpaceReservePercent != nil {
		policy, err := manager.SpacePolicy(ctx, "/")
		if err != nil {
			return nil, rpcError(err)
		}
		if req.MaxSpaceBytes != nil {
			policy.MaxBytes = req.GetMaxSpaceBytes()
		}
		if req.SpaceReservePercent != nil {
			policy.ReservePercent = int(req.GetSpaceReservePercent())
		}
		if _, err := manager.SetSpacePolicy(
			ctx, "/", policy.MaxBytes, policy.ReservePercent); err != nil {
			return nil, rpcError(err)
		}
	}
	if err := s.mountWorkspaceLocked(ctx, item, req.GetPath()); err != nil {
		return nil, err
	}
	workspaceStatus, err := s.workspaceStatus(ctx, item)
	if err != nil {
		return nil, err
	}
	legacy := workspaceVolumeStatus(workspaceStatus, req.GetPath())
	return &pb.InitResponse{Volume: legacy, Workspace: workspaceStatus}, nil
}

func (s *Server) mountWorkspace(
	ctx context.Context,
	req *pb.MountRequest,
) (*pb.MountResponse, error) {
	item, err := s.ensureWorkspace(ctx, req.GetWorkspace(), req.GetVolume())
	if err != nil {
		return nil, err
	}
	if err := s.mountWorkspaceLocked(ctx, item, req.GetPath()); err != nil {
		return nil, err
	}
	workspaceStatus, err := s.workspaceStatus(ctx, item)
	if err != nil {
		return nil, err
	}
	return &pb.MountResponse{
		Volume:    workspaceVolumeStatus(workspaceStatus, req.GetPath()),
		Workspace: workspaceStatus,
	}, nil
}

func (s *Server) umountWorkspaceLocked(
	ctx context.Context,
	item workspace.Workspace,
	mountPath string,
) (*emptypb.Empty, error) {
	cleaned := path.Clean(mountPath)
	if !s.mountedWorkspacePaths[cleaned] {
		return &emptypb.Empty{}, nil
	}
	manager := s.workspaceManager(item.ID)
	if s.cfg.QuiesceWorkspaceFunc != nil {
		s.cfg.QuiesceWorkspaceFunc(item.ID, true)
	}
	resume := true
	defer func() {
		if resume && s.cfg.QuiesceWorkspaceFunc != nil {
			s.cfg.QuiesceWorkspaceFunc(item.ID, false)
		}
	}()
	upgradeDiscard := s.cfg.UpgradeDiscardRequested != nil &&
		s.cfg.UpgradeDiscardRequested()
	deadline := time.Now().Add(3 * time.Second)
	for {
		open, err := manager.CountWorkspaceOpenWrites(ctx)
		if err != nil {
			return nil, rpcError(err)
		}
		if open == 0 {
			break
		}
		if upgradeDiscard && s.cfg.DiscardWorkspaceWritesFunc != nil {
			if _, err := s.cfg.DiscardWorkspaceWritesFunc(ctx, item.ID); err != nil {
				resume = false
				return nil, status.Errorf(codes.Internal,
					"丢弃 workspace %s 开放写窗口: %v", item.Name, err)
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"workspace %s 仍有 %d 个开放写窗口", item.Name, open)
		}
		select {
		case <-ctx.Done():
			return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if s.cfg.UmountWorkspaceFunc == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"daemon 未配置 workspace umount")
	}
	if err := s.cfg.UmountWorkspaceFunc(ctx, item, cleaned, upgradeDiscard); err != nil {
		return nil, status.Errorf(codes.Internal, "卸载 workspace: %v", err)
	}
	s.mountedWorkspacePaths[cleaned] = false
	if s.cfg.QuiesceWorkspaceFunc != nil {
		s.cfg.QuiesceWorkspaceFunc(item.ID, false)
	}
	resume = false
	return &emptypb.Empty{}, nil
}

func (s *Server) workspaceStatus(
	ctx context.Context,
	item workspace.Workspace,
) (*pb.WorkspaceStatus, error) {
	mounts, err := s.catalog.Mounts(ctx, item.ID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	manager := s.workspaceManager(item.ID)
	limit, err := manager.HistoryLimit(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	policy, err := manager.SpacePolicy(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	out := &pb.WorkspaceStatus{
		Id: item.ID, Name: item.Name, Volume: item.VolumeName,
		BackendPath: item.BackendPath, JfsMount: s.cfg.JFSMount,
		HistoryLimit: limit, MaxSpaceBytes: policy.MaxBytes,
		SpaceReservePercent:   int32(policy.ReservePercent),
		RetainedSpaceBytes:    policy.RetainedBytes,
		ReclaimableSpaceBytes: policy.ReclaimableBytes,
	}
	for _, mount := range mounts {
		out.Mounts = append(out.Mounts, &pb.MountStatus{
			Path: mount.Path, Mounted: s.mountedWorkspacePaths[path.Clean(mount.Path)],
		})
	}
	return out, nil
}

func workspaceVolumeStatus(item *pb.WorkspaceStatus, mountPath string) *pb.VolumeStatus {
	mounted := false
	for _, mount := range item.GetMounts() {
		if path.Clean(mount.GetPath()) == path.Clean(mountPath) {
			mounted = mount.GetMounted()
			break
		}
	}
	return &pb.VolumeStatus{
		Name: item.GetVolume(), JfsMount: item.GetJfsMount(), FuseMount: mountPath,
		JfsMounted: true, FuseMounted: mounted,
		HistoryLimit: item.GetHistoryLimit(), MaxSpaceBytes: item.GetMaxSpaceBytes(),
		SpaceReservePercent:   item.GetSpaceReservePercent(),
		RetainedSpaceBytes:    item.GetRetainedSpaceBytes(),
		ReclaimableSpaceBytes: item.GetReclaimableSpaceBytes(),
		WorkspaceId:           item.GetId(), WorkspaceName: item.GetName(),
	}
}

func (s *Server) allWorkspaceStatuses(ctx context.Context) ([]*pb.WorkspaceStatus, error) {
	items, err := s.catalog.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出 workspace: %w", err)
	}
	out := make([]*pb.WorkspaceStatus, 0, len(items))
	for _, item := range items {
		statusItem, err := s.workspaceStatus(ctx, item)
		if err != nil {
			return nil, err
		}
		out = append(out, statusItem)
	}
	return out, nil
}
