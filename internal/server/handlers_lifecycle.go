package server

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/txn"
)

func (s *Server) findVolumeLocked(requestedPath, requestedName string) (int, error) {
	cleaned := path.Clean(requestedPath)
	for index := range s.volumes {
		volume := &s.volumes[index]
		if requestedName != "" && requestedName != volume.Name {
			continue
		}
		if requestedPath != "" && cleaned != path.Clean(volume.FUSEMount) {
			continue
		}
		return index, nil
	}
	return -1, status.Errorf(codes.NotFound,
		"未找到卷 name=%q path=%q", requestedName, requestedPath)
}

func (s *Server) validateMountPath(requestedPath string) (string, error) {
	if !path.IsAbs(requestedPath) {
		return "", status.Error(codes.InvalidArgument, "path 必须是绝对路径")
	}
	cleaned := path.Clean(requestedPath)
	root := path.Clean(s.cfg.MountRoot)
	if root == "." || !path.IsAbs(root) {
		return "", status.Error(codes.FailedPrecondition, "daemon mount root 配置无效")
	}
	inside := strings.HasPrefix(cleaned, root+"/")
	if root == "/" {
		inside = cleaned != "/"
	}
	if cleaned == root || !inside {
		return "", status.Errorf(codes.InvalidArgument,
			"挂载路径 %q 必须位于 %q 下且不能等于根目录", cleaned, root)
	}
	return cleaned, nil
}

func (s *Server) mountLocked(ctx context.Context, index int, requestedPath string) error {
	volume := &s.volumes[index]
	cleaned, err := s.validateMountPath(requestedPath)
	if err != nil {
		return err
	}
	if volume.FUSEMount != "" && path.Clean(volume.FUSEMount) != cleaned {
		return status.Errorf(codes.FailedPrecondition,
			"当前卷已初始化到 %q；当前版本仅支持一个挂载路径", volume.FUSEMount)
	}
	if volume.FUSEMounted {
		if s.cfg.QuiesceFunc != nil {
			s.cfg.QuiesceFunc(false)
		}
		if err := s.mgr.SaveVolumeMountConfig(ctx, txn.VolumeMountConfig{
			VolumeName: volume.Name,
			FUSEMount:  cleaned,
		}); err != nil {
			return status.Error(codes.Internal,
				fmt.Sprintf("持久化挂载配置: %v", err))
		}
		return nil
	}
	if s.cfg.MountFunc == nil {
		return status.Error(codes.FailedPrecondition, "daemon 未配置动态 mount")
	}
	if err := s.cfg.MountFunc(ctx, cleaned); err != nil {
		return status.Errorf(codes.Internal, "挂载 FUSE: %v", err)
	}
	volume.FUSEMount = cleaned
	volume.JFSMounted = true
	volume.FUSEMounted = true
	if err := s.mgr.SaveVolumeMountConfig(ctx, txn.VolumeMountConfig{
		VolumeName: volume.Name,
		FUSEMount:  cleaned,
	}); err != nil {
		if s.cfg.UmountFunc != nil {
			_ = s.cfg.UmountFunc(ctx)
		}
		volume.FUSEMounted = false
		return status.Error(codes.Internal, fmt.Sprintf("持久化挂载配置: %v", err))
	}
	s.rev.SetMountPath(cleaned)
	if s.cfg.QuiesceFunc != nil {
		s.cfg.QuiesceFunc(false)
	}
	return nil
}

// Init 在已运行 daemon 上是幂等校准操作。首次 format/schema 安装由
// install.sh/entrypoint 在 daemon 启动前完成;这里确认元数据存在并恢复挂载。
func (s *Server) Init(
	ctx context.Context,
	req *pb.InitRequest,
) (*pb.InitResponse, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path 不能为空")
	}
	if req.GetRetention() != "" {
		return nil, status.Error(codes.InvalidArgument,
			"retention 已移除；请使用 history-limit 和 max-space 控制历史保留")
	}
	if req.HistoryLimit != nil &&
		req.GetHistoryLimit() != -1 && req.GetHistoryLimit() < 1 {
		return nil, status.Errorf(codes.InvalidArgument,
			"history-limit 必须是 -1 或正整数: %d", req.GetHistoryLimit())
	}
	if req.MaxSpaceBytes != nil && req.GetMaxSpaceBytes() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max-space 不能为负数")
	}
	if req.SpaceReservePercent != nil &&
		(req.GetSpaceReservePercent() < 1 || req.GetSpaceReservePercent() > 99) {
		return nil, status.Errorf(codes.InvalidArgument,
			"space-reserve 必须是 1..99%%: %d", req.GetSpaceReservePercent())
	}
	// 新 daemon 即使收到旧 CLI（尚未携带 workspace 字段）的请求，也把它
	// 解释为 default workspace；仅保留注入旧生命周期回调的单元测试/兼容
	// 嵌入场景继续走 legacy volume 分支。
	if req.GetWorkspace() != "" || s.cfg.MountWorkspaceFunc != nil {
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		return s.initWorkspace(ctx, req)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	index, err := s.findVolumeLocked("", req.GetVolume())
	if err != nil {
		return nil, err
	}
	if req.HistoryLimit != nil {
		if _, err := s.mgr.SetHistoryLimit(ctx, "/", req.GetHistoryLimit()); err != nil {
			return nil, rpcError(err)
		}
	}
	if req.MaxSpaceBytes != nil || req.SpaceReservePercent != nil {
		policy, err := s.mgr.SpacePolicy(ctx, "/")
		if err != nil {
			return nil, rpcError(err)
		}
		if req.MaxSpaceBytes != nil {
			policy.MaxBytes = req.GetMaxSpaceBytes()
		}
		if req.SpaceReservePercent != nil {
			policy.ReservePercent = int(req.GetSpaceReservePercent())
		}
		if _, err := s.mgr.SetSpacePolicy(
			ctx, "/", policy.MaxBytes, policy.ReservePercent); err != nil {
			return nil, rpcError(err)
		}
	}
	if err := s.mountLocked(ctx, index, req.GetPath()); err != nil {
		return nil, err
	}
	if err := recoverVolume(ctx, s.volumes[index]); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	result := volumeStatusPB(s.volumes[index])
	historyLimit, err := s.mgr.HistoryLimit(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	policy, err := s.mgr.SpacePolicy(ctx, "/")
	if err != nil {
		return nil, rpcError(err)
	}
	result.HistoryLimit = historyLimit
	result.MaxSpaceBytes = policy.MaxBytes
	result.SpaceReservePercent = int32(policy.ReservePercent)
	result.RetainedSpaceBytes = policy.RetainedBytes
	result.ReclaimableSpaceBytes = policy.ReclaimableBytes
	return &pb.InitResponse{Volume: result}, nil
}

func (s *Server) Mount(
	ctx context.Context,
	req *pb.MountRequest,
) (*pb.MountResponse, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path 不能为空")
	}
	if req.GetWorkspace() != "" || s.cfg.MountWorkspaceFunc != nil {
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		return s.mountWorkspace(ctx, req)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	index, err := s.findVolumeLocked(req.GetPath(), req.GetVolume())
	if err != nil {
		return nil, err
	}
	if err := s.mountLocked(ctx, index, req.GetPath()); err != nil {
		return nil, err
	}
	return &pb.MountResponse{Volume: volumeStatusPB(s.volumes[index])}, nil
}

func (s *Server) Umount(
	ctx context.Context,
	req *pb.UmountRequest,
) (*emptypb.Empty, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "path 不能为空")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if resolved, resolveErr := s.catalog.ResolveMount(ctx, req.GetPath()); resolveErr == nil {
		return s.umountWorkspaceLocked(ctx, resolved.Workspace, req.GetPath())
	}
	index, err := s.findVolumeLocked(req.GetPath(), "")
	if err != nil {
		return nil, err
	}
	if !s.volumes[index].FUSEMounted {
		return &emptypb.Empty{}, nil
	}
	if s.cfg.QuiesceFunc != nil {
		s.cfg.QuiesceFunc(true)
	}
	upgradeDiscard := s.cfg.UpgradeDiscardRequested != nil &&
		s.cfg.UpgradeDiscardRequested()
	resumeWrites := true
	defer func() {
		if resumeWrites && s.cfg.QuiesceFunc != nil {
			s.cfg.QuiesceFunc(false)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	var active int64
	for {
		active, err = s.mgr.CountOpenWrites(ctx)
		if err != nil {
			return nil, rpcError(err)
		}
		if active == 0 {
			break
		}
		if upgradeDiscard {
			if s.cfg.DiscardWritesFunc == nil {
				return nil, status.Error(codes.FailedPrecondition,
					"daemon 未配置升级写窗口丢弃能力")
			}
			discarded, discardErr := s.cfg.DiscardWritesFunc(ctx)
			if discardErr != nil {
				// 底层 fd 可能已经关闭，此时不能假装恢复可写。
				// 保持冻结，由管理员恢复/重启后处理遗留窗口。
				resumeWrites = false
				return nil, status.Errorf(codes.Internal,
					"丢弃 %d 个开放写窗口: %v；写入保持冻结，请恢复服务",
					active, discardErr)
			}
			if discarded == 0 {
				return nil, status.Error(codes.Internal,
					"检测到开放写窗口但代理没有可丢弃窗口")
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"写入冻结后仍有 %d 个开放写窗口，已取消卸载", active)
		}
		select {
		case <-ctx.Done():
			return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
		case <-time.After(25 * time.Millisecond):
		}
	}
	umountFunc := s.cfg.UmountFunc
	if upgradeDiscard && s.cfg.ForceUmountFunc != nil {
		umountFunc = s.cfg.ForceUmountFunc
	}
	if umountFunc == nil {
		return nil, status.Error(codes.FailedPrecondition, "daemon 未配置动态 umount")
	}
	if err := umountFunc(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "卸载 FUSE: %v", err)
	}
	s.volumes[index].FUSEMounted = false
	resumeWrites = false
	return &emptypb.Empty{}, nil
}

func (s *Server) ConfigSet(
	ctx context.Context,
	req *pb.ConfigSetRequest,
) (*pb.ConfigSetResponse, error) {
	key := strings.ToLower(strings.TrimSpace(req.GetKey()))
	value := strings.ToLower(strings.TrimSpace(req.GetValue()))
	manager, item, err := s.managerForWorkspaceName(ctx, req.GetWorkspace())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if req.GetWindow() != "" {
		return nil, status.Error(codes.InvalidArgument,
			"--window 已随 retention 配置移除")
	}
	if key == "history-limit" {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || (limit != -1 && limit < 1) {
			return nil, status.Errorf(codes.InvalidArgument,
				"history-limit 必须是 -1 或正整数: %q", value)
		}
		if _, err := manager.SetHistoryLimit(ctx, "/", limit); err != nil {
			return nil, rpcError(err)
		}
		return &pb.ConfigSetResponse{
			Key: key, Value: strconv.FormatInt(limit, 10)}, nil
	}
	if key == "max-space" {
		if item.Name != "default" {
			return nil, status.Error(codes.FailedPrecondition,
				"max-space 当前是 JuiceFS 卷级策略，只能由 default workspace 设置")
		}
		maxBytes, err := txn.ParseSpaceBytes(value)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		policy, err := manager.SpacePolicy(ctx, "/")
		if err != nil {
			return nil, rpcError(err)
		}
		if _, err := manager.SetSpacePolicy(
			ctx, "/", maxBytes, policy.ReservePercent); err != nil {
			return nil, rpcError(err)
		}
		return &pb.ConfigSetResponse{
			Key: key, Value: txn.FormatSpaceLimit(maxBytes)}, nil
	}
	if key == "space-reserve" {
		if item.Name != "default" {
			return nil, status.Error(codes.FailedPrecondition,
				"space-reserve 当前是 JuiceFS 卷级策略，只能由 default workspace 设置")
		}
		percentText := strings.TrimSuffix(value, "%")
		percent, err := strconv.Atoi(percentText)
		if err != nil || percent < 1 || percent > 99 {
			return nil, status.Errorf(codes.InvalidArgument,
				"space-reserve 必须是 1..99%%: %q", value)
		}
		policy, err := manager.SpacePolicy(ctx, "/")
		if err != nil {
			return nil, rpcError(err)
		}
		if _, err := manager.SetSpacePolicy(
			ctx, "/", policy.MaxBytes, percent); err != nil {
			return nil, rpcError(err)
		}
		return &pb.ConfigSetResponse{
			Key: key, Value: fmt.Sprintf("%d%%", percent)}, nil
	}
	if key == "retention" {
		return nil, status.Error(codes.InvalidArgument,
			"retention 已移除；请使用 history-limit 和 max-space 控制历史保留")
	}
	return nil, status.Errorf(codes.InvalidArgument, "不支持配置项 %q", key)
}
