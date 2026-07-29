// pitrd — pitr filesystem daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/pg"
	pitrserver "pitr_fs/internal/server"
	"pitr_fs/internal/txn"
)

var (
	flagPGDSN     string
	flagVolume    string
	flagJFSMount  string
	flagFUSEMount string
	flagSocket    string
	flagRetention string
	flagLogLevel  string
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:   "pitrd",
		Short: "pitr filesystem daemon (元数据事务 + FUSE 拦截)",
		Long: `pitrd 是 pitr 文件系统的守护进程,持有 JuiceFS/FUSE 挂载和
gRPC 控制面 unix socket。`,
		RunE: runDaemon,
	}
	root.Flags().StringVar(&flagPGDSN, "pg-dsn", "", "PostgreSQL DSN(必填,可用 $PITR_PG_DSN)")
	root.Flags().StringVar(&flagVolume, "volume", "default", "JuiceFS 卷名")
	root.Flags().StringVar(&flagJFSMount, "jfs-mount", "/var/lib/pitr/jfs", "JuiceFS 底层挂载目录")
	root.Flags().StringVar(&flagFUSEMount, "fuse-mount", "/workspace", "FUSE loopback 对用户暴露的挂载点")
	root.Flags().StringVar(&flagSocket, "socket", "/var/run/pitrd.sock", "pitrd unix 控制 socket")
	root.Flags().StringVar(&flagRetention, "retention", "compact", "保留策略")
	root.Flags().StringVar(&flagLogLevel, "log-level", "info", "日志级别:debug|info|warn|error")
	root.SetContext(ctx)

	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDaemon(cmd *cobra.Command, _ []string) error {
	dsn := flagPGDSN
	if dsn == "" {
		dsn = os.Getenv("PITR_PG_DSN")
	}
	if dsn == "" {
		return errors.New("--pg-dsn 不能为空,也未设置 PITR_PG_DSN")
	}
	if err := configureLogging(flagLogLevel); err != nil {
		return err
	}

	db, err := pg.Connect(cmd.Context(), dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := txn.NewManager(db)
	handler := pitrserver.New(db, mgr, pitrserver.Config{
		Volume:    flagVolume,
		JFSMount:  flagJFSMount,
		FUSEMount: flagFUSEMount,
		Retention: flagRetention,
	})
	grpcServer := grpc.NewServer()
	pb.RegisterPitrdServer(grpcServer, handler)
	reflection.Register(grpcServer)

	slog.Info("pitrd starting",
		"socket", flagSocket,
		"volume", flagVolume,
		"jfs_mount", flagJFSMount,
		"fuse_mount", flagFUSEMount,
	)
	return serveSocket(cmd.Context(), flagSocket, grpcServer)
}

func configureLogging(level string) error {
	var parsed slog.Level
	switch strings.ToLower(level) {
	case "debug":
		parsed = slog.LevelDebug
	case "info":
		parsed = slog.LevelInfo
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		return fmt.Errorf("非法 --log-level %q", level)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: parsed,
	})))
	return nil
}

func serveSocket(ctx context.Context, socket string, grpcServer *grpc.Server) error {
	if socket == "" {
		return errors.New("--socket 不能为空")
	}
	if grpcServer == nil {
		return errors.New("grpc server 不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return fmt.Errorf("创建 socket 目录: %w", err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清理旧 socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("监听 unix socket %s: %w", socket, err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0o660); err != nil {
		return fmt.Errorf("设置 socket 权限: %w", err)
	}

	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			gracefulDone := make(chan struct{})
			go func() {
				grpcServer.GracefulStop()
				close(gracefulDone)
			}()
			select {
			case <-gracefulDone:
			case <-time.After(5 * time.Second):
				grpcServer.Stop()
			}
		case <-shutdownDone:
		}
	}()

	err = grpcServer.Serve(listener)
	close(shutdownDone)
	if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("gRPC serve: %w", err)
	}
	return nil
}
