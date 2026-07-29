// pitrd — pitr filesystem daemon.
//
// P1 阶段:cobra root 骨架,建 unix socket + 阻塞,让部署脚本能拿到就绪信号。
// P2 起把 serve() 内部的 <-ctx.Done() 之前替换为 gRPC Serve 主循环。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

var (
	flagPGDSN     string
	flagVolume    string
	flagJFSMount  string
	flagFUSEMount string
	flagSocket    string
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:   "pitrd",
		Short: "pitr filesystem daemon (元数据事务 + FUSE 拦截)",
		Long: `pitrd 是 pitr 文件系统的守护进程,持有两层挂载(JuiceFS + FUSE loopback)
和 gRPC 控制面 socket。P1 阶段仅提供骨架(建 socket 后阻塞)。`,
		RunE: runDaemon,
	}
	root.Flags().StringVar(&flagPGDSN, "pg-dsn", "", "PostgreSQL DSN(必填,可用 $PITR_PG_DSN)")
	root.Flags().StringVar(&flagVolume, "volume", "default", "JuiceFS 卷名")
	root.Flags().StringVar(&flagJFSMount, "jfs-mount", "/var/lib/pitr/jfs", "JuiceFS 底层挂载目录")
	root.Flags().StringVar(&flagFUSEMount, "fuse-mount", "/workspace", "FUSE loopback 对用户暴露的挂载点")
	root.Flags().StringVar(&flagSocket, "socket", "/var/run/pitrd.sock", "pitrd unix 控制 socket")
	root.SetContext(ctx)

	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDaemon(cmd *cobra.Command, _ []string) error {
	return serve(cmd.Context(), flagSocket)
}

// serve — P1 阶段: 建 socket 后阻塞在 ctx.Done()。P2 起在这里 gRPC Serve。
func serve(ctx context.Context, socket string) error {
	if socket == "" {
		return errors.New("--socket 不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	_ = os.Remove(socket) // 上次残留

	l, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("bind socket %s: %w", socket, err)
	}
	defer func() { _ = l.Close() }()

	_, _ = fmt.Fprintf(os.Stderr, "pitrd: skeleton ready — pg=%s socket=%s (等待 ctx.Done)\n",
		flagPGDSN, socket)
	<-ctx.Done()
	_, _ = fmt.Fprintln(os.Stderr, "pitrd: ctx 已 done, 优雅退出")
	return nil
}
