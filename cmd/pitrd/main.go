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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/buildinfo"
	juicefsabi "pitr_fs/internal/juicefsabi/v1"
	pitrmount "pitr_fs/internal/mount"
	"pitr_fs/internal/pg"
	"pitr_fs/internal/proxy"
	pitrserver "pitr_fs/internal/server"
	"pitr_fs/internal/txn"
)

var (
	flagPGDSN      string
	flagVolume     string
	flagJFSMount   string
	flagMountRoot  string
	flagSocket     string
	flagLogLevel   string
	flagGCInterval time.Duration
	flagGCThreads  int
	flagJFSCache   int
	flagCheckABI   bool
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
	root.Flags().StringVar(&flagMountRoot, "mount-root", "/pitr", "允许 init 使用的宿主机挂载根目录")
	root.Flags().StringVar(&flagSocket, "socket", "/var/run/pitrd.sock", "pitrd unix 控制 socket")
	root.Flags().StringVar(&flagLogLevel, "log-level", "info", "日志级别:debug|info|warn|error")
	root.Flags().DurationVar(&flagGCInterval, "gc-interval", 10*time.Minute, "对象 GC 批处理间隔；0 表示停用")
	root.Flags().IntVar(&flagGCThreads, "gc-threads", 4, "JuiceFS GC 对象删除并发数")
	root.Flags().IntVar(&flagJFSCache, "jfs-cache-size", 1024,
		"JuiceFS 本地缓存上限(MiB)")
	root.Flags().BoolVar(&flagCheckABI, "check-compatibility", false,
		"只读校验固定 JuiceFS/PostgreSQL ABI 后退出")
	root.SetContext(ctx)

	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDaemon(cmd *cobra.Command, _ []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("pitrd 仅支持 Linux，当前系统为 %s", runtime.GOOS)
	}
	flagMountRoot = filepath.Clean(flagMountRoot)
	if !filepath.IsAbs(flagMountRoot) || flagMountRoot == "/" {
		return errors.New("--mount-root 必须是非根目录的绝对路径")
	}
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

	jfs := &pitrmount.JuiceFS{
		MetaURL:      dsn,
		MountPoint:   flagJFSMount,
		CacheSizeMiB: flagJFSCache,
	}
	if err := juicefsabi.ValidateBinary(cmd.Context(), jfs.Binary); err != nil {
		return fmt.Errorf("校验 JuiceFS 运行时: %w", err)
	}
	if err := juicefsabi.ValidateMetadata(cmd.Context(), db); err != nil {
		return fmt.Errorf("校验 JuiceFS 元数据 ABI: %w", err)
	}
	slog.Info("JuiceFS compatibility contract verified",
		"contract", juicefsabi.Contract())
	if flagCheckABI {
		return nil
	}
	if err := jfs.Start(cmd.Context()); err != nil {
		return fmt.Errorf("挂载 JuiceFS: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := jfs.Stop(stopCtx); err != nil {
			slog.Error("stop JuiceFS", "error", err)
		}
	}()

	mgr := txn.NewManager(db)
	if closed, err := mgr.CloseDanglingAutoVersions(cmd.Context()); err != nil {
		return err
	} else if closed != 0 {
		slog.Warn("closed dangling auto windows", "count", closed)
	}
	if flagGCInterval < 0 {
		return errors.New("--gc-interval 不能为负数")
	}
	if flagGCThreads <= 0 {
		return errors.New("--gc-threads 必须大于 0")
	}
	if flagGCInterval > 0 {
		go runGCWorker(cmd.Context(), mgr, jfs, flagGCInterval, flagGCThreads)
	}
	persisted, err := mgr.LoadVolumeMountConfig(cmd.Context(), flagVolume)
	if err != nil {
		return err
	}
	var fuseProxy *proxy.Loopback
	mountProxy := func(_ context.Context, mountPath string) error {
		if fuseProxy != nil {
			if fuseProxy.Mount != filepath.Clean(mountPath) {
				return fmt.Errorf("FUSE 已配置到 %s", fuseProxy.Mount)
			}
			return fuseProxy.Start()
		}
		created, createErr := proxy.NewLoopback(
			flagJFSMount,
			mountPath,
			proxy.WithManager(mgr),
			proxy.WithAllowOther(true),
		)
		if createErr != nil {
			return createErr
		}
		if startErr := created.Start(); startErr != nil {
			return startErr
		}
		fuseProxy = created
		return nil
	}
	umountProxy := func(context.Context) error {
		if fuseProxy == nil {
			return nil
		}
		return fuseProxy.Unmount()
	}
	forceUmountProxy := func(context.Context) error {
		if fuseProxy == nil {
			return nil
		}
		return fuseProxy.UnmountLazy()
	}
	fuseMount := ""
	if persisted != nil {
		fuseMount = persisted.FUSEMount
		if !pathInsideMountRoot(fuseMount, flagMountRoot) {
			return fmt.Errorf("持久化挂载点 %q 不在 mount root %q 下",
				fuseMount, flagMountRoot)
		}
		if err := mountProxy(cmd.Context(), fuseMount); err != nil {
			return fmt.Errorf("恢复 FUSE 代理: %w", err)
		}
	}
	defer func() {
		if err := umountProxy(context.Background()); err != nil {
			slog.Error("stop FUSE proxy", "error", err)
		}
	}()

	handler := pitrserver.New(db, mgr, pitrserver.Config{
		DaemonVersion:   buildinfo.Full(),
		Volume:          flagVolume,
		JFSMount:        flagJFSMount,
		FUSEMount:       fuseMount,
		MountRoot:       flagMountRoot,
		JFSMounted:      true,
		FUSEMounted:     fuseProxy != nil && fuseProxy.Mounted(),
		MountFunc:       mountProxy,
		UmountFunc:      umountProxy,
		ForceUmountFunc: forceUmountProxy,
		QuiesceFunc: func(enabled bool) {
			if fuseProxy != nil {
				fuseProxy.SetQuiescing(enabled)
			}
		},
		DiscardWritesFunc: func(ctx context.Context) (int, error) {
			if fuseProxy == nil {
				return 0, nil
			}
			return fuseProxy.DiscardOpenWrites(ctx)
		},
		UpgradeDiscardRequested: func() bool {
			_, err := os.Stat("/run/pitr/discard-open-writes")
			return err == nil
		},
	})
	grpcServer := grpc.NewServer()
	pb.RegisterPitrdServer(grpcServer, handler)
	reflection.Register(grpcServer)

	slog.Info("pitrd starting",
		"socket", flagSocket,
		"volume", flagVolume,
		"jfs_mount", flagJFSMount,
		"mount_root", flagMountRoot,
		"fuse_mount", fuseMount,
	)
	return serveSocket(cmd.Context(), flagSocket, grpcServer)
}

func runGCWorker(
	ctx context.Context,
	mgr *txn.Manager,
	jfs *pitrmount.JuiceFS,
	interval time.Duration,
	threads int,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := interval
			if timeout < time.Hour {
				timeout = time.Hour
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			ran, err := mgr.RunPendingGC(runCtx, func(gcCtx context.Context) error {
				return jfs.GC(gcCtx, threads)
			})
			cancel()
			switch {
			case err == nil && ran:
				slog.Info("JuiceFS lifecycle GC completed")
			case errors.Is(err, txn.ErrMaintenanceBusy):
				slog.Debug("JuiceFS lifecycle GC deferred", "reason", err)
			case err != nil && !errors.Is(err, context.Canceled):
				slog.Error("JuiceFS lifecycle GC failed", "error", err)
			}
		}
	}
}

func pathInsideMountRoot(candidate, root string) bool {
	candidate = filepath.Clean(candidate)
	root = filepath.Clean(root)
	if !filepath.IsAbs(candidate) || !filepath.IsAbs(root) || root == "/" {
		return false
	}
	return candidate != root && strings.HasPrefix(candidate, root+string(os.PathSeparator))
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
