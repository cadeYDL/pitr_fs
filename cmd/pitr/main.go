// pitr — pitr CLI 客户端。
//
// P1 阶段:cobra 骨架覆盖设计文档 §8.1 的所有子命令(逻辑留空,
// 每个子命令 RunE 返回 "not implemented" 让集成侧能提前接线)。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "pitr_fs/api/pitrd/v1"
)

const rpcTimeout = 5 * time.Second

type daemonClient struct {
	conn *grpc.ClientConn
	rpc  pb.PitrdClient
}

func dialDaemon(cmd *cobra.Command) (*daemonClient, error) {
	socket, err := cmd.Root().PersistentFlags().GetString("socket")
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(
		"unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("连接 pitrd(%s): %w", socket, err)
	}
	return &daemonClient{conn: conn, rpc: pb.NewPitrdClient(conn)}, nil
}

func (c *daemonClient) close() {
	_ = c.conn.Close()
}

func rpcContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), rpcTimeout)
}

func friendlyRPCError(cmd *cobra.Command, err error) error {
	socket, _ := cmd.Root().PersistentFlags().GetString("socket")
	return fmt.Errorf("pitrd 请求失败(%s): %w", socket, err)
}

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
	var pgDSN, volume, jfsMount, fuseMount, retention, logLevel string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "前台启动 pitrd(通常由 install/systemd 拉起)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			binary, err := exec.LookPath("pitrd")
			if err != nil {
				return fmt.Errorf("查找 pitrd: %w", err)
			}
			socket, _ := cmd.Root().PersistentFlags().GetString("socket")
			arguments := []string{
				"--volume", volume,
				"--jfs-mount", jfsMount,
				"--fuse-mount", fuseMount,
				"--socket", socket,
				"--retention", retention,
				"--log-level", logLevel,
			}
			if pgDSN != "" {
				arguments = append(arguments, "--pg-dsn", pgDSN)
			}
			process := exec.CommandContext(cmd.Context(), binary, arguments...)
			process.Stdin = cmd.InOrStdin()
			process.Stdout = cmd.OutOrStdout()
			process.Stderr = cmd.ErrOrStderr()
			if err := process.Run(); err != nil {
				return fmt.Errorf("pitrd 退出: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&pgDSN, "pg-dsn", "", "PostgreSQL DSN(可用 $PITR_PG_DSN)")
	c.Flags().StringVar(&volume, "volume", "default", "JuiceFS 卷名")
	c.Flags().StringVar(&jfsMount, "jfs-mount", "/var/lib/pitr/jfs", "底层 JuiceFS 挂载点")
	c.Flags().StringVar(&fuseMount, "fuse-mount", "/workspace", "用户可见 FUSE 挂载点")
	c.Flags().StringVar(&retention, "retention", "compact", "保留策略")
	c.Flags().StringVar(&logLevel, "log-level", "info", "日志级别")
	return c
}

func newInitCmd() *cobra.Command {
	var volume, storage, bucket, accessKey, secretKey, retention, dataDir string
	c := &cobra.Command{
		Use:   "init <path>",
		Short: "首次初始化:格式化 JuiceFS + 装触发器 + 挂载 FUSE 到 <path>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			storageArgs := []string{"storage=" + storage}
			for key, value := range map[string]string{
				"bucket": bucket, "access-key": accessKey,
				"secret-key": secretKey, "data-dir": dataDir,
			} {
				if value != "" {
					storageArgs = append(storageArgs, key+"="+value)
				}
			}
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Init(ctx, &pb.InitRequest{
				Path: args[0], Volume: volume,
				Retention: retention, StorageArgs: storageArgs,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			item := resp.GetVolume()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"initialized %s @ %s (jfs=%s retention=%s)\n",
				item.GetName(), item.GetFuseMount(),
				item.GetJfsMount(), item.GetRetention())
			return nil
		},
	}
	c.Flags().StringVar(&volume, "volume", "default", "JuiceFS 卷名")
	c.Flags().StringVar(&storage, "storage", "file", "juicefs 存储后端 (透传)")
	c.Flags().StringVar(&bucket, "bucket", "", "juicefs bucket URL/路径 (透传)")
	c.Flags().StringVar(&accessKey, "access-key", "", "云对象存储 access key (透传)")
	c.Flags().StringVar(&secretKey, "secret-key", "", "云对象存储 secret key (透传)")
	c.Flags().StringVar(&retention, "retention", "compact", "保留策略: verbose|compact|archive")
	c.Flags().StringVar(&dataDir, "data-dir", "", "file 后端本地数据目录")
	return c
}

func newRecoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recover [path]",
		Short: "数据仍在,重新拉起 daemon + 挂载",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			var requested string
			if len(args) == 1 {
				requested = args[0]
			}
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Recover(ctx,
				&pb.RecoverRequest{Path: requested})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			for _, volume := range resp.GetVolumes() {
				if volume.GetError() != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"failed %s @ %s: %s\n",
						volume.GetName(), volume.GetFuseMount(), volume.GetError())
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"recovered %s @ %s (jfs=%s)\n",
						volume.GetName(), volume.GetFuseMount(), volume.GetJfsMount())
				}
			}
			return nil
		},
	}
}

func newMountCmd() *cobra.Command {
	var volume string
	c := &cobra.Command{
		Use:   "mount <path>",
		Short: "已格式化的卷单独恢复 FUSE 挂载",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Mount(ctx,
				&pb.MountRequest{Path: args[0], Volume: volume})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			item := resp.GetVolume()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"mounted %s @ %s\n", item.GetName(), item.GetFuseMount())
			return nil
		},
	}
	c.Flags().StringVar(&volume, "volume", "", "卷名(默认按 path 匹配)")
	return c
}

func newUmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "umount <path>",
		Short: "卸载 FUSE",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			if _, err := client.rpc.Umount(ctx,
				&pb.UmountRequest{Path: args[0]}); err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "unmounted %s\n", args[0])
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "列出所有已知卷 + 挂载 + 事务状态",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Status(ctx, &pb.StatusRequest{})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"connected to pitrd %s, %d volumes, %d active transactions\n",
				resp.GetDaemonVersion(), len(resp.GetVolumes()), resp.GetActiveTransactions())
			for _, volume := range resp.GetVolumes() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"%s\tjfs=%s\tfuse=%s\tretention=%s\n",
					volume.GetName(), volume.GetJfsMount(),
					volume.GetFuseMount(), volume.GetRetention())
			}
			return nil
		},
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
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			window, _ := cmd.Flags().GetString("window")
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.ConfigSet(ctx, &pb.ConfigSetRequest{
				Key: args[0], Value: args[1], Window: window,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "set %s=%s",
				resp.GetKey(), resp.GetValue())
			if resp.GetWindow() != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), " window=%s", resp.GetWindow())
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout())
			return nil
		},
	}
	set.Flags().String("window", "", "archive 策略的保留窗口,如 30d")
	c.AddCommand(set)
	return c
}

// ---------- 事务 / 时间旅行 ----------

func newBeginCmd() *cobra.Command {
	var message string
	c := &cobra.Command{
		Use:   "begin <path>",
		Short: "在 <path> 上开一个 active 事务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Begin(ctx, &pb.BeginRequest{
				Path: args[0], Message: message,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			transaction := resp.GetTransaction()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started txn %s @ %s\n",
				transaction.GetVersionHash(), transaction.GetScopePath())
			return nil
		},
	}
	c.Flags().StringVarP(&message, "message", "m", "", "事务说明")
	return c
}

func newCommitCmd() *cobra.Command {
	var message string
	c := &cobra.Command{
		Use:   "commit <path>",
		Short: "提交 <path> 上的 active 事务(触发坍缩)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Commit(ctx, &pb.CommitRequest{
				Path: args[0], Message: message,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			transaction := resp.GetTransaction()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "committed txn %s @ %s\n",
				transaction.GetVersionHash(), transaction.GetScopePath())
			return nil
		},
	}
	c.Flags().StringVarP(&message, "message", "m", "", "commit 说明")
	return c
}

func newRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <path>",
		Short: "回滚 <path> 上的 active 事务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Rollback(ctx,
				&pb.RollbackRequest{Path: args[0]})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			transaction := resp.GetTransaction()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rolled back txn %s @ %s\n",
				transaction.GetVersionHash(), transaction.GetScopePath())
			return nil
		},
	}
}

func newLogsCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "logs <path>",
		Short: "查看 <path> 的版本历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Logs(ctx, &pb.LogsRequest{
				Path: args[0], Limit: int32(limit),
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			for _, entry := range resp.GetEntries() {
				transaction := entry.GetTransaction()
				if transaction.GetMessage() == "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s   %s\n",
						transaction.GetVersionHash(), transaction.GetCommand())
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s   %s   # %s\n",
						transaction.GetVersionHash(), transaction.GetCommand(),
						transaction.GetMessage())
				}
			}
			return nil
		},
	}
	c.Flags().IntVarP(&limit, "number", "n", 20, "最多返回多少条")
	return c
}

func newDiffCmd() *cobra.Command {
	var scope string
	command := &cobra.Command{
		Use:   "diff <version-a> <version-b>",
		Short: "对比两个版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Diff(ctx, &pb.DiffRequest{
				VersionA: args[0],
				VersionB: args[1],
				Path:     scope,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"nodes=%d edges=%d chunks=%d\n",
				resp.GetNodeChanges(), resp.GetEdgeChanges(), resp.GetChunkChanges())
			return nil
		},
	}
	command.Flags().StringVar(&scope, "path", "", "只统计指定目录范围")
	return command
}

func newRevertCmd() *cobra.Command {
	var scope string
	var dryRun bool
	c := &cobra.Command{
		Use:   "revert <version-hash>",
		Short: "回退到指定版本",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Revert(ctx, &pb.RevertRequest{
				VersionHash: args[0],
				Path:        scope,
				DryRun:      dryRun,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			if dryRun {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"dry-run: would apply %d history rows\n", resp.GetApplied())
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"reverted %d history rows; new version %s\n",
					resp.GetApplied(), resp.GetNewVersionHash())
			}
			return nil
		},
	}
	c.Flags().StringVar(&scope, "path", "", "目录级 revert 范围(默认全局)")
	c.Flags().StringVar(&scope, "scope", "", "已弃用:请使用 --path")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只统计将回放的 history")
	return c
}
