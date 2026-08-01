package server

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

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
		if err := s.mgr.SaveVolumeMountConfig(ctx, txn.VolumeMountConfig{
			VolumeName: volume.Name,
			FUSEMount:  cleaned,
			Retention:  volume.Retention,
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
		Retention:  volume.Retention,
	}); err != nil {
		if s.cfg.UmountFunc != nil {
			_ = s.cfg.UmountFunc(ctx)
		}
		volume.FUSEMounted = false
		return status.Error(codes.Internal, fmt.Sprintf("持久化挂载配置: %v", err))
	}
	s.rev.SetMountPath(cleaned)
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
	switch req.GetRetention() {
	case "", "verbose", "compact", "archive":
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"非法 retention %q", req.GetRetention())
	}
	if req.HistoryLimit != nil &&
		(req.GetHistoryLimit() < 1 || req.GetHistoryLimit() > 100000) {
		return nil, status.Errorf(codes.InvalidArgument,
			"history-limit 必须是 1..100000: %d", req.GetHistoryLimit())
	}
	if req.MaxSpaceBytes != nil && req.GetMaxSpaceBytes() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max-space 不能为负数")
	}
	if req.SpaceReservePercent != nil &&
		(req.GetSpaceReservePercent() < 1 || req.GetSpaceReservePercent() > 99) {
		return nil, status.Errorf(codes.InvalidArgument,
			"space-reserve 必须是 1..99%%: %d", req.GetSpaceReservePercent())
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	index, err := s.findVolumeLocked("", req.GetVolume())
	if err != nil {
		return nil, err
	}
	if req.GetRetention() != "" {
		s.volumes[index].Retention = req.GetRetention()
	}
	if req.HistoryLimit != nil {
		if _, err := s.mgr.SetHistoryLimit(ctx, "/", int(req.GetHistoryLimit())); err != nil {
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
	result.HistoryLimit = int32(historyLimit)
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
	if active, err := s.mgr.CountOpenWrites(ctx); err != nil {
		return nil, rpcError(err)
	} else if active != 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"仍有 %d 个开放写窗口，拒绝卸载", active)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	index, err := s.findVolumeLocked(req.GetPath(), "")
	if err != nil {
		return nil, err
	}
	if !s.volumes[index].FUSEMounted {
		return &emptypb.Empty{}, nil
	}
	if s.cfg.UmountFunc == nil {
		return nil, status.Error(codes.FailedPrecondition, "daemon 未配置动态 umount")
	}
	if err := s.cfg.UmountFunc(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "卸载 FUSE: %v", err)
	}
	s.volumes[index].FUSEMounted = false
	return &emptypb.Empty{}, nil
}

func (s *Server) ConfigSet(
	ctx context.Context,
	req *pb.ConfigSetRequest,
) (*pb.ConfigSetResponse, error) {
	key := strings.ToLower(strings.TrimSpace(req.GetKey()))
	value := strings.ToLower(strings.TrimSpace(req.GetValue()))
	if key == "history-limit" {
		if req.GetWindow() != "" {
			return nil, status.Error(codes.InvalidArgument,
				"history-limit 不接受 --window")
		}
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100000 {
			return nil, status.Errorf(codes.InvalidArgument,
				"history-limit 必须是 1..100000 的整数: %q", value)
		}
		if _, err := s.mgr.SetHistoryLimit(ctx, "/", limit); err != nil {
			return nil, rpcError(err)
		}
		return &pb.ConfigSetResponse{Key: key, Value: strconv.Itoa(limit)}, nil
	}
	if key == "max-space" {
		if req.GetWindow() != "" {
			return nil, status.Error(codes.InvalidArgument,
				"max-space 不接受 --window")
		}
		maxBytes, err := txn.ParseSpaceBytes(value)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		policy, err := s.mgr.SpacePolicy(ctx, "/")
		if err != nil {
			return nil, rpcError(err)
		}
		if _, err := s.mgr.SetSpacePolicy(
			ctx, "/", maxBytes, policy.ReservePercent); err != nil {
			return nil, rpcError(err)
		}
		return &pb.ConfigSetResponse{
			Key: key, Value: txn.FormatSpaceLimit(maxBytes)}, nil
	}
	if key == "space-reserve" {
		if req.GetWindow() != "" {
			return nil, status.Error(codes.InvalidArgument,
				"space-reserve 不接受 --window")
		}
		percentText := strings.TrimSuffix(value, "%")
		percent, err := strconv.Atoi(percentText)
		if err != nil || percent < 1 || percent > 99 {
			return nil, status.Errorf(codes.InvalidArgument,
				"space-reserve 必须是 1..99%%: %q", value)
		}
		policy, err := s.mgr.SpacePolicy(ctx, "/")
		if err != nil {
			return nil, rpcError(err)
		}
		if _, err := s.mgr.SetSpacePolicy(
			ctx, "/", policy.MaxBytes, percent); err != nil {
			return nil, rpcError(err)
		}
		return &pb.ConfigSetResponse{
			Key: key, Value: fmt.Sprintf("%d%%", percent)}, nil
	}
	if key != "retention" {
		return nil, status.Errorf(codes.InvalidArgument, "不支持配置项 %q", key)
	}
	switch value {
	case "verbose", "compact":
		if req.GetWindow() != "" {
			return nil, status.Error(codes.InvalidArgument,
				"只有 archive retention 接受 window")
		}
	case "archive":
		if strings.TrimSpace(req.GetWindow()) == "" {
			return nil, status.Error(codes.InvalidArgument,
				"archive retention 必须指定 --window")
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"非法 retention %q", value)
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	for index := range s.volumes {
		s.volumes[index].Retention = value
	}
	s.cfg.Retention = value
	if req.GetWindow() == "" {
		delete(s.windows, key)
	} else {
		s.windows[key] = req.GetWindow()
	}
	return &pb.ConfigSetResponse{
		Key: key, Value: value, Window: req.GetWindow(),
	}, nil
}
