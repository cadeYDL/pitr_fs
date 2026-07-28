// pitr — pitr CLI 客户端。
//
// P1 阶段:cobra 骨架覆盖设计文档 §8.1 的所有子命令(逻辑留空,
// 每个子命令 RunE 返回 "not implemented" 让集成侧能提前接线)。
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var errNotImpl = errors.New("尚未实现;将在 Phase 2/4 落地")

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "pitr",
		Short: "pitr — 元数据 MVCC 文件系统 CLI",
		Long:  `pitr 通过 unix socket 连接本地 pitrd,提供版本管理、revert、logs、diff 等命令。`,
	}

	// 全局 flag(未来放到 config)
	root.PersistentFlags().String("socket", "/var/run/pitrd.sock", "pitrd unix socket")

	// 生命周期 (§8.1)
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newRecoverCmd())
	root.AddCommand(newMountCmd())
	root.AddCommand(newUmountCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newConfigCmd())

	// 事务与时间旅行 (§8.2)
	root.AddCommand(newBeginCmd())
	root.AddCommand(newCommitCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newDiffCmd())
	root.AddCommand(newRevertCmd())

	return root
}

// ---------- 生命周期 ----------

func newDaemonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "daemon",
		Short: "前台启动 pitrd(通常由 install/systemd 拉起)",
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	c.Flags().String("pg-dsn", "", "PostgreSQL DSN(可用 $PITR_PG_DSN)")
	return c
}

func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init <path>",
		Short: "首次初始化:格式化 JuiceFS + 装触发器 + 挂载 FUSE 到 <path>",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	c.Flags().String("volume", "default", "JuiceFS 卷名")
	c.Flags().String("storage", "file", "juicefs 存储后端 (透传)")
	c.Flags().String("bucket", "", "juicefs bucket URL/路径 (透传)")
	c.Flags().String("access-key", "", "云对象存储 access key (透传)")
	c.Flags().String("secret-key", "", "云对象存储 secret key (透传)")
	c.Flags().String("retention", "compact", "保留策略: verbose|compact|archive")
	c.Flags().String("data-dir", "", "file 后端本地数据目录")
	return c
}

func newRecoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recover [path]",
		Short: "数据仍在,重新拉起 daemon + 挂载",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
}

func newMountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mount <path>",
		Short: "已格式化的卷单独恢复 FUSE 挂载",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
}

func newUmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "umount <path>",
		Short: "卸载 FUSE",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "列出所有已知卷 + 挂载 + 事务状态",
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
}

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "运行时配置(retention 等)",
	}
	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "设置配置,例如: pitr config set retention compact --window 30d",
		Args:  cobra.MinimumNArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	set.Flags().String("window", "", "archive 策略的保留窗口,如 30d")
	c.AddCommand(set)
	return c
}

// ---------- 事务 / 时间旅行 ----------

func newBeginCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "begin <path>",
		Short: "在 <path> 上开一个 active 事务",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	c.Flags().StringP("message", "m", "", "事务说明")
	return c
}

func newCommitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "commit <path>",
		Short: "提交 <path> 上的 active 事务(触发坍缩)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	c.Flags().StringP("message", "m", "", "commit 说明")
	return c
}

func newRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <path>",
		Short: "回滚 <path> 上的 active 事务",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
}

func newLogsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logs <path>",
		Short: "查看 <path> 的版本历史",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	c.Flags().IntP("number", "n", 20, "最多返回多少条")
	return c
}

func newDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <version-a> <version-b>",
		Short: "对比两个版本",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
}

func newRevertCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revert <version-hash>",
		Short: "回退到指定版本",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return errNotImpl },
	}
	c.Flags().String("scope", "", "目录级 revert 范围(默认全局)")
	return c
}
