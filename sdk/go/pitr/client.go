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
	ID          int64
	VersionHash string
	ParentID    *int64
	ScopePath   string
	State       string
	Command     string
	Message     string
	CreatedAt   time.Time
	ClosedAt    *time.Time
}

func transactionFromPB(value *pb.Transaction) Transaction {
	if value == nil {
		return Transaction{}
	}
	result := Transaction{
		ID:          value.GetTxnId(),
		VersionHash: value.GetVersionHash(),
		ScopePath:   value.GetScopePath(),
		State:       value.GetState(),
		Command:     value.GetCommand(),
		Message:     value.GetMessage(),
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
	Applied        int64
	NewVersionHash string
}

func (c *Client) Revert(
	ctx context.Context,
	versionHash string,
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
	})
	if err != nil {
		return RevertResult{}, fmt.Errorf("revert %s: %w", versionHash, err)
	}
	return RevertResult{
		Applied:        response.GetApplied(),
		NewVersionHash: response.GetNewVersionHash(),
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
	Name         string
	JFSMount     string
	FUSEMount    string
	JFSMounted   bool
	FUSEMounted  bool
	Retention    string
	HistoryLimit int
	Error        string
}

func volumeFromPB(value *pb.VolumeStatus) Volume {
	return Volume{
		Name:         value.GetName(),
		JFSMount:     value.GetJfsMount(),
		FUSEMount:    value.GetFuseMount(),
		JFSMounted:   value.GetJfsMounted(),
		FUSEMounted:  value.GetFuseMounted(),
		Retention:    value.GetRetention(),
		HistoryLimit: int(value.GetHistoryLimit()),
		Error:        value.GetError(),
	}
}

func (c *Client) SetHistoryLimit(ctx context.Context, limit int) error {
	if limit < 1 || limit > 100000 {
		return fmt.Errorf("history limit 必须在 1..100000 之间: %d", limit)
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
