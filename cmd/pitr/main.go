// pitr — pitr CLI 客户端。
//
// cobra 命令覆盖设计文档 §8.1/§8.2 的控制面，并通过 unix gRPC
// 调用 pitrd；daemon 子命令用于前台启动 pitrd。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "pitr_fs/api/pitrd/v1"
	"pitr_fs/internal/buildinfo"
	"pitr_fs/internal/txn"
)

const (
	rpcTimeout             = 5 * time.Second
	defaultHistoryLimitCLI = 100
)

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

// resolveCLIPath 把用户路径按 pitr 进程的工作目录转成 daemon 使用的绝对
// scope。空值表示 diff/revert/recover 的全局范围，必须原样保留。
func resolveCLIPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析路径 %q: %w", value, err)
	}
	return filepath.Clean(absolute), nil
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
	root.CompletionOptions.DisableDefaultCmd = true

	// 全局 flag(未来放到 config)
	root.PersistentFlags().String("socket", "/var/run/pitrd.sock", "pitrd unix socket")

	// 生命周期 (§8.1)
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newRecoverCmd())
	root.AddCommand(newMountCmd())
	root.AddCommand(newUmountCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newUpgradeCmd())
	root.AddCommand(newSpaceCmd())
	root.AddCommand(newConfigCmd())

	// 事务与时间旅行 (§8.2)
	root.AddCommand(newBeginCmd())
	root.AddCommand(newCommitCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newDiffCmd())
	root.AddCommand(newRevertCmd())
	root.AddCommand(newClearCmd())

	return root
}

func newVersionCmd() *cobra.Command {
	var clientOnly bool
	command := &cobra.Command{
		Use:   "version",
		Short: "查看 pitr 客户端和 pitrd 服务端版本",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pitr %s\n", buildinfo.Full())
			if clientOnly {
				return nil
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			response, err := client.rpc.Status(ctx, &pb.StatusRequest{})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pitrd %s\n",
				response.GetDaemonVersion())
			return nil
		},
	}
	command.Flags().BoolVar(&clientOnly, "client-only", false,
		"只显示客户端版本，不连接 pitrd")
	return command
}

func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade [版本]",
		Short: "升级或回退 pitr/pitrd 逻辑版本",
		Long: `升级由 Linux 宿主机安装的 pitr wrapper 执行。
		省略版本时下载最新 GitHub Release，也可指定版本或使用本地升级包。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return errors.New("upgrade 只能通过 Linux 宿主机安装的 pitr wrapper 执行")
		},
	}
}

// ---------- 生命周期 ----------

func newDaemonCmd() *cobra.Command {
	var pgDSN, volume, jfsMount, mountRoot, retention, logLevel string
	var gcInterval time.Duration
	var gcThreads int
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
				"--mount-root", mountRoot,
				"--socket", socket,
				"--retention", retention,
				"--log-level", logLevel,
				"--gc-interval", gcInterval.String(),
				"--gc-threads", fmt.Sprint(gcThreads),
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
	c.Flags().StringVar(&mountRoot, "mount-root", "/pitr", "允许 init 使用的挂载根目录")
	c.Flags().StringVar(&retention, "retention", "compact", "保留策略")
	c.Flags().StringVar(&logLevel, "log-level", "info", "日志级别")
	c.Flags().DurationVar(&gcInterval, "gc-interval", 10*time.Minute, "对象 GC 批处理间隔")
	c.Flags().IntVar(&gcThreads, "gc-threads", 4, "对象 GC 删除并发数")
	return c
}

func newInitCmd() *cobra.Command {
	var volume, storage, bucket, accessKey, secretKey, retention, dataDir string
	var historyLimit, spaceReserve int
	var maxSpace string
	c := &cobra.Command{
		Use:   "init <path>",
		Short: "幂等校准已部署卷并恢复 FUSE 挂载",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
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
			request := &pb.InitRequest{
				Path: resolved, Volume: volume,
				Retention: retention, StorageArgs: storageArgs,
			}
			if cmd.Flags().Changed("history-limit") {
				value := int32(historyLimit)
				request.HistoryLimit = &value
			}
			if cmd.Flags().Changed("max-space") {
				value, parseErr := txn.ParseSpaceBytes(maxSpace)
				if parseErr != nil {
					return parseErr
				}
				request.MaxSpaceBytes = &value
			}
			if cmd.Flags().Changed("space-reserve") {
				value := int32(spaceReserve)
				request.SpaceReservePercent = &value
			}
			resp, err := client.rpc.Init(ctx, request)
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			item := resp.GetVolume()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"initialized %s @ %s (jfs=%s retention=%s history-limit=%d max-space=%s reserve=%d%%)\n",
				item.GetName(), item.GetFuseMount(),
				item.GetJfsMount(), item.GetRetention(), item.GetHistoryLimit(),
				txn.FormatSpaceLimit(item.GetMaxSpaceBytes()),
				item.GetSpaceReservePercent())
			return nil
		},
	}
	c.Flags().StringVar(&volume, "volume", "default", "JuiceFS 卷名")
	c.Flags().StringVar(&storage, "storage", "file", "兼容参数；存储后端在 install 时确定")
	c.Flags().StringVar(&bucket, "bucket", "", "兼容参数；bucket 在 install 时确定")
	c.Flags().StringVar(&accessKey, "access-key", "", "兼容参数；凭证在 install 时确定")
	c.Flags().StringVar(&secretKey, "secret-key", "", "兼容参数；凭证在 install 时确定")
	c.Flags().StringVar(&retention, "retention", "compact", "保留策略: verbose|compact|archive")
	c.Flags().StringVar(&dataDir, "data-dir", "", "兼容参数；file 数据目录在 install 时确定")
	c.Flags().IntVar(&historyLimit, "history-limit", defaultHistoryLimitCLI,
		"最多保留的版本数")
	c.Flags().StringVar(&maxSpace, "max-space", "unlimited",
		"文件数据空间上限，例如 100GiB；unlimited 表示不限")
	c.Flags().IntVar(&spaceReserve, "space-reserve", 20,
		"预留空间百分比；20 表示使用达到 80% 时淘汰最老版本")
	return c
}

func newRecoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recover [path]",
		Short: "数据仍在,重新拉起 daemon + 挂载",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var requested string
			var err error
			if len(args) == 1 {
				requested, err = resolveCLIPath(args[0])
				if err != nil {
					return err
				}
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
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
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Mount(ctx,
				&pb.MountRequest{Path: resolved, Volume: volume})
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
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			if _, err := client.rpc.Umount(ctx,
				&pb.UmountRequest{Path: resolved}); err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "unmounted %s\n", resolved)
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
				"connected to pitrd %s, %d volumes, %d open writes\n",
				resp.GetDaemonVersion(), len(resp.GetVolumes()), resp.GetOpenWrites())
			for _, volume := range resp.GetVolumes() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"%s\tjfs=%s\tfuse=%s\tretention=%s\thistory-limit=%d\tmax-space=%s\treserve=%d%%\tretained=%s\treclaimable=%s\n",
					volume.GetName(), volume.GetJfsMount(),
					volume.GetFuseMount(), volume.GetRetention(),
					volume.GetHistoryLimit(),
					txn.FormatSpaceLimit(volume.GetMaxSpaceBytes()),
					volume.GetSpaceReservePercent(),
					txn.FormatSpaceBytes(volume.GetRetainedSpaceBytes()),
					txn.FormatSpaceBytes(volume.GetReclaimableSpaceBytes()))
			}
			return nil
		},
	}
}

func newSpaceCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "space [path]",
		Short: "查看空间水位和按最老优先裁剪时的版本释放估算",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			requested := "."
			if len(args) == 1 {
				requested = args[0]
			}
			resolved, err := resolveCLIPath(requested)
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			response, err := client.rpc.Space(ctx, &pb.SpaceRequest{
				Path: resolved, Limit: int32(limit),
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"max=%s reserve=%d%% high=%s retained=%s reclaimable=%s\n",
				txn.FormatSpaceLimit(response.GetMaxSpaceBytes()),
				response.GetReservePercent(),
				txn.FormatSpaceBytes(response.GetHighWatermarkBytes()),
				txn.FormatSpaceBytes(response.GetRetainedBytes()),
				txn.FormatSpaceBytes(response.GetReclaimableBytes()))
			_, _ = fmt.Fprintln(cmd.OutOrStdout(),
				"VERSION\tRELEASE_IF_DELETED\tPINNED\tCLOSED_AT")
			for _, version := range response.GetVersions() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n",
					version.GetVersionHash(),
					txn.FormatSpaceBytes(version.GetEstimatedReleaseBytes()),
					txn.FormatSpaceBytes(version.GetPinnedBytes()),
					version.GetClosedAt())
			}
			return nil
		},
	}
	c.Flags().IntVarP(&limit, "limit", "n", 20, "最多显示的最老版本数")
	return c
}

func newConfigCmd() *cobra.Command {
	const configHelp = `查看或设置全局运行时配置。

支持的配置项：
  retention      verbose|compact|archive；archive 必须配合 --window
  history-limit  1..100000，最多保留的版本数
  max-space      unlimited、0 或容量值（如 100GiB）
  space-reserve  1..99%，达到可用空间高水位后优先淘汰老版本`
	listConfig := func(cmd *cobra.Command, _ []string) error {
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
		retention, historyLimit, maxSpace, spaceReserve := "-", "-", "-", "-"
		if len(resp.GetVolumes()) > 0 {
			volume := resp.GetVolumes()[0]
			retention = volume.GetRetention()
			if retention == "" {
				retention = "-"
			}
			historyLimit = fmt.Sprintf("%d", volume.GetHistoryLimit())
			maxSpace = txn.FormatSpaceLimit(volume.GetMaxSpaceBytes())
			spaceReserve = fmt.Sprintf("%d%%", volume.GetSpaceReservePercent())
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"配置项\t当前值\t默认值\t取值范围\t说明")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"retention\t%s\tcompact\tverbose|compact|archive\t版本保留策略；archive 需 --window\n",
			retention)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"history-limit\t%s\t100\t1..100000\t最多保留的版本数\n",
			historyLimit)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"max-space\t%s\tunlimited\tunlimited|0|容量值\t允许用户实际占用的最大空间\n",
			maxSpace)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"space-reserve\t%s\t20%%\t1..99%%\t临近空间比例，优先淘汰老版本\n",
			spaceReserve)
		return nil
	}
	c := &cobra.Command{
		Use:   "config",
		Short: "查看或设置运行时配置",
		Long:  configHelp,
		Args:  cobra.NoArgs,
		RunE:  listConfig,
	}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "列出支持的配置项和当前值",
		Args:    cobra.NoArgs,
		RunE:    listConfig,
	}
	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "设置配置，例如 history-limit、max-space、space-reserve",
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
	c.AddCommand(list, set)
	return c
}

// ---------- 事务 / 时间旅行 ----------

func newBeginCmd() *cobra.Command {
	var message string
	c := &cobra.Command{
		Use:    "begin <path>",
		Short:  "已废弃：自动快照模式无需 begin",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Begin(ctx, &pb.BeginRequest{
				Path: resolved, Message: message,
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
		Use:    "commit <path>",
		Short:  "已废弃：文件关闭后自动形成版本",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Commit(ctx, &pb.CommitRequest{
				Path: resolved, Message: message,
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
		Use:    "rollback <path>",
		Short:  "已废弃：请使用 revert",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Rollback(ctx,
				&pb.RollbackRequest{Path: resolved})
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
	var long bool
	c := &cobra.Command{
		Use:   "logs <path>",
		Short: "查看 <path> 的版本历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveCLIPath(args[0])
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Logs(ctx, &pb.LogsRequest{
				Path: resolved, Limit: int32(limit),
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			for _, entry := range resp.GetEntries() {
				transaction := entry.GetTransaction()
				if long {
					posixOp := transaction.GetPosixOperation()
					if posixOp == "" {
						posixOp = transaction.GetCommand()
					}
					processCommand := shortenForLog(
						transaction.GetProcessCommand(), 10)
					if processCommand == "" {
						processCommand = "<unknown>"
					}
					operationTime := transaction.GetCreatedAt()
					if transaction.GetClosedAt() != nil {
						operationTime = transaction.GetClosedAt()
					}
					timestamp := ""
					if operationTime != nil {
						timestamp = operationTime.AsTime().Format(time.RFC3339Nano)
					}
					actor := formatLogActor(transaction)
					change := transaction.GetChangeSummary()
					if change == "" {
						change = "-"
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"%s\t%s\t%s\t%s\t%s\t%s\n",
						transaction.GetVersionHash(), posixOp, processCommand,
						timestamp, actor, change)
					continue
				}
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
	c.Flags().BoolVarP(&long, "long", "l", false,
		"显示 POSIX 操作、原始命令、操作时间、操作人和内容摘要")
	return c
}

func shortenForLog(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func formatLogActor(transaction *pb.Transaction) string {
	if transaction.GetActorName() != "" {
		return fmt.Sprintf("%s(uid=%d,gid=%d,pid=%d)",
			transaction.GetActorName(), transaction.GetActorUid(),
			transaction.GetActorGid(), transaction.GetActorPid())
	}
	if transaction.GetActorUid() == 0 && transaction.GetActorGid() == 0 &&
		transaction.GetActorPid() == 0 {
		return "<unknown>"
	}
	return fmt.Sprintf("uid=%d,gid=%d,pid=%d",
		transaction.GetActorUid(), transaction.GetActorGid(),
		transaction.GetActorPid())
}

func newDiffCmd() *cobra.Command {
	var scope string
	command := &cobra.Command{
		Use:   "diff <version-a> <version-b>",
		Short: "对比两个版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveCLIPath(scope)
			if err != nil {
				return err
			}
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
				Path:     resolved,
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
	var global bool
	var targetTime string
	c := &cobra.Command{
		Use:   "revert [version-hash]",
		Short: "按版本号或时间回退",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 0) == (targetTime == "") {
				return errors.New("version-hash 与 --at 必须且只能指定一个")
			}
			if global && scope != "" {
				return errors.New("--global 与 --path/--scope 不能同时使用")
			}
			requested := scope
			if !global && requested == "" {
				requested = "."
			}
			resolved, err := resolveCLIPath(requested)
			if err != nil {
				return err
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			versionHash := ""
			if len(args) == 1 {
				versionHash = args[0]
			}
			resp, err := client.rpc.Revert(ctx, &pb.RevertRequest{
				VersionHash: versionHash,
				Path:        resolved,
				DryRun:      dryRun,
				TargetTime:  targetTime,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			if dryRun {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"dry-run: target %s at %s; would apply %d history rows\n",
					resp.GetResolvedVersionHash(), resp.GetResolvedVersionTime(),
					resp.GetApplied())
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"reverted to %s at %s; applied %d history rows; new version %s\n",
					resp.GetResolvedVersionHash(), resp.GetResolvedVersionTime(),
					resp.GetApplied(), resp.GetNewVersionHash())
			}
			return nil
		},
	}
	c.Flags().StringVar(&scope, "path", "", "目录级 revert 范围(默认当前目录)")
	c.Flags().StringVar(&scope, "scope", "", "已弃用:请使用 --path")
	c.Flags().BoolVar(&global, "global", false, "回退整个卷(默认只回退当前目录)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只统计将回放的 history")
	c.Flags().StringVar(&targetTime, "at", "",
		"回退到该 RFC3339 时间之前最近的完整版本")
	return c
}

func newClearCmd() *cobra.Command {
	var global, yes bool
	c := &cobra.Command{
		Use:   "clear",
		Short: "清空版本历史并保留当前文件作为新基线",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !global {
				return errors.New("当前仅支持全局 clear，请添加 --global")
			}
			if !yes {
				return errors.New("clear 会永久删除全部历史，请添加 --yes")
			}
			client, err := dialDaemon(cmd)
			if err != nil {
				return err
			}
			defer client.close()
			ctx, cancel := rpcContext(cmd)
			defer cancel()
			resp, err := client.rpc.Clear(ctx, &pb.ClearRequest{
				Global: true, Confirm: true,
			})
			if err != nil {
				return friendlyRPCError(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"cleared %d versions and %d history rows; current files kept as baseline\n",
				resp.GetVersionsDeleted(), resp.GetHistoryDeleted())
			return nil
		},
	}
	c.Flags().BoolVar(&global, "global", false, "清空整个卷(当前唯一支持的维度)")
	c.Flags().BoolVar(&yes, "yes", false, "确认永久删除历史")
	return c
}
