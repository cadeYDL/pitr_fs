// Package pitr 提供 pitrd gRPC 控制面的 Go 语义封装。
package pitr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "pitr_fs/api/pitrd/v1"
)

var (
	ErrTxnClosed                  = errors.New("transaction 已结束")
	ErrManualTransactionsDisabled = errors.New(
		"手工 transaction 已停用：写操作会自动形成版本")
)

func resolvePath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析路径 %q: %w", value, err)
	}
	return filepath.Clean(absolute), nil
}

type Client struct {
	conn *grpc.ClientConn
	rpc  pb.PitrdClient
}

type DialOption func(*dialOptions)

type dialOptions struct {
	grpcOptions []grpc.DialOption
}

func WithGRPCDialOptions(options ...grpc.DialOption) DialOption {
	return func(config *dialOptions) {
		config.grpcOptions = append(config.grpcOptions, options...)
	}
}

func Dial(socket string, options ...DialOption) (*Client, error) {
	if strings.TrimSpace(socket) == "" {
		return nil, errors.New("pitrd socket 不能为空")
	}
	config := dialOptions{
		grpcOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}
	for _, option := range options {
		option(&config)
	}
	target := socket
	if !strings.Contains(target, "://") {
		target = "unix://" + target
	}
	conn, err := grpc.NewClient(target, config.grpcOptions...)
	if err != nil {
		return nil, fmt.Errorf("连接 pitrd(%s): %w", socket, err)
	}
	return &Client{conn: conn, rpc: pb.NewPitrdClient(conn)}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

type BeginOption func(*beginOptions)

type beginOptions struct {
	message string
}

func WithMessage(message string) BeginOption {
	return func(options *beginOptions) {
		options.message = message
	}
}

func (c *Client) Begin(
	ctx context.Context,
	path string,
	options ...BeginOption,
) (*Txn, error) {
	return nil, ErrManualTransactionsDisabled
}

type Transaction struct {
	ID             int64
	VersionHash    string
	ParentID       *int64
	ScopePath      string
	State          string
	Command        string
	Message        string
	CreatedAt      time.Time
	ClosedAt       *time.Time
	POSIXOperation string
	ProcessCommand string
	ActorUID       int64
	ActorGID       int64
	ActorPID       int64
	ActorName      string
	ChangeSummary  string
}

func transactionFromPB(value *pb.Transaction) Transaction {
	if value == nil {
		return Transaction{}
	}
	result := Transaction{
		ID:             value.GetTxnId(),
		VersionHash:    value.GetVersionHash(),
		ScopePath:      value.GetScopePath(),
		State:          value.GetState(),
		Command:        value.GetCommand(),
		Message:        value.GetMessage(),
		POSIXOperation: value.GetPosixOperation(),
		ProcessCommand: value.GetProcessCommand(),
		ActorUID:       value.GetActorUid(),
		ActorGID:       value.GetActorGid(),
		ActorPID:       value.GetActorPid(),
		ActorName:      value.GetActorName(),
		ChangeSummary:  value.GetChangeSummary(),
	}
	if value.ParentId != nil {
		parent := value.GetParentId()
		result.ParentID = &parent
	}
	if value.GetCreatedAt() != nil {
		result.CreatedAt = value.GetCreatedAt().AsTime()
	}
	if value.GetClosedAt() != nil {
		closed := value.GetClosedAt().AsTime()
		result.ClosedAt = &closed
	}
	return result
}

type LogEntry struct {
	Transaction
}

func (c *Client) Logs(
	ctx context.Context,
	path string,
	limit int,
) ([]LogEntry, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	response, err := c.rpc.Logs(ctx, &pb.LogsRequest{
		Path: resolved, Limit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", resolved, err)
	}
	result := make([]LogEntry, 0, len(response.GetEntries()))
	for _, entry := range response.GetEntries() {
		result = append(result, LogEntry{
			Transaction: transactionFromPB(entry.GetTransaction()),
		})
	}
	return result, nil
}

type RevertOption func(*revertOptions)

type revertOptions struct {
	path         string
	pathExplicit bool
	global       bool
	dryRun       bool
}

func WithPath(path string) RevertOption {
	return func(options *revertOptions) {
		options.path = path
		options.pathExplicit = true
	}
}

func WithGlobal() RevertOption {
	return func(options *revertOptions) {
		options.global = true
	}
}

func WithDryRun() RevertOption {
	return func(options *revertOptions) {
		options.dryRun = true
	}
}

type RevertResult struct {
	Applied             int64
	NewVersionHash      string
	ResolvedVersionHash string
	ResolvedVersionTime string
}

func (c *Client) Revert(
	ctx context.Context,
	versionHash string,
	options ...RevertOption,
) (RevertResult, error) {
	if strings.TrimSpace(versionHash) == "" {
		return RevertResult{}, errors.New("version hash 不能为空")
	}
	return c.revert(ctx, versionHash, "", options...)
}

// RevertAt 回滚到不晚于 target 的最近一个已完成版本。
func (c *Client) RevertAt(
	ctx context.Context,
	target time.Time,
	options ...RevertOption,
) (RevertResult, error) {
	if target.IsZero() {
		return RevertResult{}, errors.New("target time 不能为空")
	}
	return c.revert(
		ctx, "", target.Format(time.RFC3339Nano), options...)
}

func (c *Client) revert(
	ctx context.Context,
	versionHash string,
	targetTime string,
	options ...RevertOption,
) (RevertResult, error) {
	config := revertOptions{path: "."}
	for _, option := range options {
		option(&config)
	}
	if config.global && config.pathExplicit {
		return RevertResult{}, errors.New("WithGlobal 与 WithPath 不能同时使用")
	}
	if config.global {
		config.path = ""
	}
	resolved, err := resolvePath(config.path)
	if err != nil {
		return RevertResult{}, err
	}
	response, err := c.rpc.Revert(ctx, &pb.RevertRequest{
		VersionHash: versionHash,
		Path:        resolved,
		DryRun:      config.dryRun,
		TargetTime:  targetTime,
	})
	if err != nil {
		target := versionHash
		if target == "" {
			target = targetTime
		}
		return RevertResult{}, fmt.Errorf("revert %s: %w", target, err)
	}
	return RevertResult{
		Applied:             response.GetApplied(),
		NewVersionHash:      response.GetNewVersionHash(),
		ResolvedVersionHash: response.GetResolvedVersionHash(),
		ResolvedVersionTime: response.GetResolvedVersionTime(),
	}, nil
}

type DiffStats struct {
	NodeChanges  int64
	EdgeChanges  int64
	ChunkChanges int64
}

func (c *Client) Diff(
	ctx context.Context,
	versionA, versionB, path string,
) (DiffStats, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return DiffStats{}, err
	}
	response, err := c.rpc.Diff(ctx, &pb.DiffRequest{
		VersionA: versionA, VersionB: versionB, Path: resolved,
	})
	if err != nil {
		return DiffStats{}, fmt.Errorf("diff %s..%s: %w", versionA, versionB, err)
	}
	return DiffStats{
		NodeChanges:  response.GetNodeChanges(),
		EdgeChanges:  response.GetEdgeChanges(),
		ChunkChanges: response.GetChunkChanges(),
	}, nil
}

type Volume struct {
	Name                  string
	JFSMount              string
	FUSEMount             string
	JFSMounted            bool
	FUSEMounted           bool
	HistoryLimit          int
	MaxSpaceBytes         int64
	SpaceReservePercent   int
	RetainedSpaceBytes    int64
	ReclaimableSpaceBytes int64
	Error                 string
}

func volumeFromPB(value *pb.VolumeStatus) Volume {
	return Volume{
		Name:                  value.GetName(),
		JFSMount:              value.GetJfsMount(),
		FUSEMount:             value.GetFuseMount(),
		JFSMounted:            value.GetJfsMounted(),
		FUSEMounted:           value.GetFuseMounted(),
		HistoryLimit:          int(value.GetHistoryLimit()),
		MaxSpaceBytes:         value.GetMaxSpaceBytes(),
		SpaceReservePercent:   int(value.GetSpaceReservePercent()),
		RetainedSpaceBytes:    value.GetRetainedSpaceBytes(),
		ReclaimableSpaceBytes: value.GetReclaimableSpaceBytes(),
		Error:                 value.GetError(),
	}
}

func (c *Client) SetMaxSpaceBytes(ctx context.Context, maxBytes int64) error {
	if maxBytes < 0 {
		return fmt.Errorf("max space 不能为负数: %d", maxBytes)
	}
	_, err := c.rpc.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "max-space", Value: fmt.Sprint(maxBytes) + "B",
	})
	if err != nil {
		return fmt.Errorf("设置 max space: %w", err)
	}
	return nil
}

func (c *Client) SetSpaceReserve(ctx context.Context, percent int) error {
	if percent < 1 || percent > 99 {
		return fmt.Errorf("space reserve 必须在 1..99 之间: %d", percent)
	}
	_, err := c.rpc.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "space-reserve", Value: fmt.Sprintf("%d%%", percent),
	})
	if err != nil {
		return fmt.Errorf("设置 space reserve: %w", err)
	}
	return nil
}

type VersionSpace struct {
	VersionHash     string
	ClosedAt        string
	PinnedBytes     int64
	ReleasableBytes int64
}

type SpaceInfo struct {
	MaxBytes           int64
	ReservePercent     int
	HighWatermarkBytes int64
	RetainedBytes      int64
	ReclaimableBytes   int64
	Versions           []VersionSpace
}

func (c *Client) Space(
	ctx context.Context,
	path string,
	limit int,
) (SpaceInfo, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return SpaceInfo{}, err
	}
	response, err := c.rpc.Space(ctx, &pb.SpaceRequest{
		Path: resolved, Limit: int32(limit),
	})
	if err != nil {
		return SpaceInfo{}, fmt.Errorf("查询空间: %w", err)
	}
	result := SpaceInfo{
		MaxBytes:           response.GetMaxSpaceBytes(),
		ReservePercent:     int(response.GetReservePercent()),
		HighWatermarkBytes: response.GetHighWatermarkBytes(),
		RetainedBytes:      response.GetRetainedBytes(),
		ReclaimableBytes:   response.GetReclaimableBytes(),
	}
	for _, version := range response.GetVersions() {
		result.Versions = append(result.Versions, VersionSpace{
			VersionHash:     version.GetVersionHash(),
			ClosedAt:        version.GetClosedAt(),
			PinnedBytes:     version.GetPinnedBytes(),
			ReleasableBytes: version.GetEstimatedReleaseBytes(),
		})
	}
	return result, nil
}

func (c *Client) SetHistoryLimit(ctx context.Context, limit int) error {
	if limit != -1 && limit < 1 {
		return fmt.Errorf("history limit 必须是 -1 或正整数: %d", limit)
	}
	_, err := c.rpc.ConfigSet(ctx, &pb.ConfigSetRequest{
		Key: "history-limit", Value: fmt.Sprint(limit),
	})
	if err != nil {
		return fmt.Errorf("设置 history limit: %w", err)
	}
	return nil
}

type ClearResult struct {
	VersionsDeleted int64
	HistoryDeleted  int64
}

type SquashResult struct {
	BaseVersion      string
	EndVersion       string
	VersionsMerged   int64
	VersionsDeleted  int64
	HistoryBefore    int64
	HistoryAfter     int64
	HistoryDeleted   int64
	FirstOperationAt time.Time
	EndClosedAt      time.Time
	DryRun           bool
	Transaction      Transaction
}

// Squash 将 (baseVersion,endVersion] 压缩到保留的 endVersion。
// dryRun=true 时 confirm 必须为 false；实际执行时必须显式 confirm=true。
func (c *Client) Squash(
	ctx context.Context,
	baseVersion, endVersion, message string,
	dryRun, confirm bool,
) (SquashResult, error) {
	if strings.TrimSpace(baseVersion) == "" ||
		strings.TrimSpace(endVersion) == "" || strings.TrimSpace(message) == "" {
		return SquashResult{}, errors.New("squash 的 base、end 和 message 均不能为空")
	}
	if dryRun == confirm {
		return SquashResult{}, errors.New("dryRun 与 confirm 必须且只能有一个为 true")
	}
	response, err := c.rpc.Squash(ctx, &pb.SquashRequest{
		BaseVersion: baseVersion,
		EndVersion:  endVersion,
		Message:     message,
		DryRun:      dryRun,
		Confirm:     confirm,
	})
	if err != nil {
		return SquashResult{}, fmt.Errorf("squash %s..%s: %w",
			baseVersion, endVersion, err)
	}
	result := SquashResult{
		BaseVersion:     response.GetBaseVersion(),
		EndVersion:      response.GetEndVersion(),
		VersionsMerged:  response.GetVersionsMerged(),
		VersionsDeleted: response.GetVersionsDeleted(),
		HistoryBefore:   response.GetHistoryBefore(),
		HistoryAfter:    response.GetHistoryAfter(),
		HistoryDeleted:  response.GetHistoryDeleted(),
		DryRun:          response.GetDryRun(),
		Transaction:     transactionFromPB(response.GetTransaction()),
	}
	if value, parseErr := time.Parse(time.RFC3339Nano,
		response.GetFirstOperationAt()); parseErr == nil {
		result.FirstOperationAt = value
	}
	if value, parseErr := time.Parse(time.RFC3339Nano,
		response.GetEndClosedAt()); parseErr == nil {
		result.EndClosedAt = value
	}
	return result, nil
}

// Clear permanently removes global version history while preserving current files.
func (c *Client) Clear(ctx context.Context, confirm bool) (ClearResult, error) {
	if !confirm {
		return ClearResult{}, errors.New("clear 必须显式 confirm=true")
	}
	response, err := c.rpc.Clear(ctx, &pb.ClearRequest{
		Global: true, Confirm: true,
	})
	if err != nil {
		return ClearResult{}, fmt.Errorf("clear history: %w", err)
	}
	return ClearResult{
		VersionsDeleted: response.GetVersionsDeleted(),
		HistoryDeleted:  response.GetHistoryDeleted(),
	}, nil
}

func (c *Client) Recover(ctx context.Context, path string) ([]Volume, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	response, err := c.rpc.Recover(ctx, &pb.RecoverRequest{Path: resolved})
	if err != nil {
		return nil, fmt.Errorf("recover %s: %w", resolved, err)
	}
	result := make([]Volume, 0, len(response.GetVolumes()))
	for _, volume := range response.GetVolumes() {
		result = append(result, volumeFromPB(volume))
	}
	return result, nil
}
