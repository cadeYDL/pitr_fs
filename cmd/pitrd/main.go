// pitrd — pitr filesystem daemon.
//
// P1 阶段:cobra root 骨架,可编译且能 `--help`。真实逻辑在 P2 起填充。
package main

import (
	"fmt"
	"os"

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
	root := &cobra.Command{
		Use:   "pitrd",
		Short: "pitr filesystem daemon (元数据事务 + FUSE 拦截)",
		Long: `pitrd 是 pitr 文件系统的守护进程,持有两层挂载(JuiceFS + FUSE loopback)
和 gRPC 控制面 socket。P1 阶段仅提供骨架。`,
		RunE: runDaemon,
	}
	root.Flags().StringVar(&flagPGDSN, "pg-dsn", "", "PostgreSQL DSN(必填,可用 $PITR_PG_DSN)")
	root.Flags().StringVar(&flagVolume, "volume", "default", "JuiceFS 卷名")
	root.Flags().StringVar(&flagJFSMount, "jfs-mount", "/var/lib/pitr/jfs", "JuiceFS 底层挂载目录")
	root.Flags().StringVar(&flagFUSEMount, "fuse-mount", "/workspace", "FUSE loopback 对用户暴露的挂载点")
	root.Flags().StringVar(&flagSocket, "socket", "/var/run/pitrd.sock", "pitrd unix 控制 socket")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDaemon(_ *cobra.Command, _ []string) error {
	fmt.Fprintln(os.Stderr, "pitrd: skeleton — wire-up 在 Phase 2/3 之后")
	return nil
}
