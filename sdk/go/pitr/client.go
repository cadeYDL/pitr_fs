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

var ErrTxnClosed = errors.New("transaction 已结束")

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
	if c == nil || c.rpc == nil {
		return nil, errors.New("pitr client 未连接")
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	config := beginOptions{}
	for _, option := range options {
		option(&config)
	}
	response, err := c.rpc.Begin(ctx, &pb.BeginRequest{
		Path: resolved, Message: config.message,
	})
	if err != nil {
		return nil, fmt.Errorf("begin %s: %w", resolved, err)
	}
	value := transactionFromPB(response.GetTransaction())
	return &Txn{
		client:      c,
		path:        value.ScopePath,
		txnID:       value.ID,
		versionHash: value.VersionHash,
		state:       value.State,
	}, nil
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
	path   string
	dryRun bool
}

func WithPath(path string) RevertOption {
	return func(options *revertOptions) {
		options.path = path
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
	config := revertOptions{}
	for _, option := range options {
		option(&config)
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
	Name        string
	JFSMount    string
	FUSEMount   string
	JFSMounted  bool
	FUSEMounted bool
	Retention   string
	Error       string
}

func volumeFromPB(value *pb.VolumeStatus) Volume {
	return Volume{
		Name:        value.GetName(),
		JFSMount:    value.GetJfsMount(),
		FUSEMount:   value.GetFuseMount(),
		JFSMounted:  value.GetJfsMounted(),
		FUSEMounted: value.GetFuseMounted(),
		Retention:   value.GetRetention(),
		Error:       value.GetError(),
	}
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
