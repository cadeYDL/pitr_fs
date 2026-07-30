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
	root.AddCommand(newClearCmd())

	root.InitDefaultCompletionCmd()
	localizeCompletionCommand(root)
	return root
}

func localizeCompletionCommand(root *cobra.Command) {
	var completion *cobra.Command
	for _, command := range root.Commands() {
		if command.Name() == "completion" {
			completion = command
			break
		}
	}
	if completion == nil {
		return
	}
	completion.Short = "生成指定 shell 的自动补全脚本"
	completion.Long = `为 pitr 生成指定 shell 的自动补全脚本。
具体加载和安装方法请查看各 shell 子命令的帮助。`

	longDescriptions := map[string]string{
		"bash": `生成 bash 自动补全脚本。

该脚本依赖 bash-completion 软件包。当前会话可执行：

	source <(pitr completion bash)

永久启用可执行：

	pitr completion bash > /etc/bash_completion.d/pitr

macOS 使用 Homebrew 时可执行：

	pitr completion bash > $(brew --prefix)/etc/bash_completion.d/pitr

重新启动 shell 后生效。`,
		"zsh": `生成 zsh 自动补全脚本。

若尚未启用补全，请先执行：

	echo "autoload -U compinit; compinit" >> ~/.zshrc

当前会话可执行：

	source <(pitr completion zsh)

永久启用可将脚本写入 ${fpath[1]}/_pitr；macOS 使用 Homebrew 时写入
$(brew --prefix)/share/zsh/site-functions/_pitr。

重新启动 shell 后生效。`,
		"fish": `生成 fish 自动补全脚本。

当前会话可执行：

	pitr completion fish | source

永久启用可执行：

	pitr completion fish > ~/.config/fish/completions/pitr.fish

重新启动 shell 后生效。`,
		"powershell": `生成 PowerShell 自动补全脚本。

当前会话可执行：

	pitr completion powershell | Out-String | Invoke-Expression

永久启用时，请将上述命令加入 PowerShell 配置文件。`,
	}
	for _, command := range completion.Commands() {
		shell := command.Name()
		command.Short = fmt.Sprintf("生成 %s 自动补全脚本", shell)
		command.Long = longDescriptions[shell]
		if flag := command.Flags().Lookup("no-descriptions"); flag != nil {
			flag.Usage = "生成不含命令说明的补全脚本"
		}
		switch shell {
		case "bash":
			command.RunE = func(cmd *cobra.Command, _ []string) error {
				noDescriptions, _ := cmd.Flags().GetBool("no-descriptions")
				return cmd.Root().GenBashCompletionV2(
					cmd.OutOrStdout(), !noDescriptions)
			}
		case "zsh":
			command.RunE = func(cmd *cobra.Command, _ []string) error {
				noDescriptions, _ := cmd.Flags().GetBool("no-descriptions")
				if noDescriptions {
					return cmd.Root().GenZshCompletionNoDesc(cmd.OutOrStdout())
				}
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			}
		case "fish":
			command.RunE = func(cmd *cobra.Command, _ []string) error {
				noDescriptions, _ := cmd.Flags().GetBool("no-descriptions")
				return cmd.Root().GenFishCompletion(
					cmd.OutOrStdout(), !noDescriptions)
			}
		case "powershell":
			command.RunE = func(cmd *cobra.Command, _ []string) error {
				noDescriptions, _ := cmd.Flags().GetBool("no-descriptions")
				if noDescriptions {
					return cmd.Root().GenPowerShellCompletion(cmd.OutOrStdout())
				}
				return cmd.Root().GenPowerShellCompletionWithDesc(
					cmd.OutOrStdout())
			}
		}
	}
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
			resp, err := client.rpc.Init(ctx, &pb.InitRequest{
				Path: resolved, Volume: volume,
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
	c.Flags().StringVar(&storage, "storage", "file", "兼容参数；存储后端在 install 时确定")
	c.Flags().StringVar(&bucket, "bucket", "", "兼容参数；bucket 在 install 时确定")
	c.Flags().StringVar(&accessKey, "access-key", "", "兼容参数；凭证在 install 时确定")
	c.Flags().StringVar(&secretKey, "secret-key", "", "兼容参数；凭证在 install 时确定")
	c.Flags().StringVar(&retention, "retention", "compact", "保留策略: verbose|compact|archive")
	c.Flags().StringVar(&dataDir, "data-dir", "", "兼容参数；file 数据目录在 install 时确定")
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
					"%s\tjfs=%s\tfuse=%s\tretention=%s\thistory-limit=%d\n",
					volume.GetName(), volume.GetJfsMount(),
					volume.GetFuseMount(), volume.GetRetention(),
					volume.GetHistoryLimit())
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
		Short: "设置配置，例如: pitr config set history-limit 100",
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
	c := &cobra.Command{
		Use:   "revert <version-hash>",
		Short: "回退到指定版本",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			resp, err := client.rpc.Revert(ctx, &pb.RevertRequest{
				VersionHash: args[0],
				Path:        resolved,
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
	c.Flags().StringVar(&scope, "path", "", "目录级 revert 范围(默认当前目录)")
	c.Flags().StringVar(&scope, "scope", "", "已弃用:请使用 --path")
	c.Flags().BoolVar(&global, "global", false, "回退整个卷(默认只回退当前目录)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "只统计将回放的 history")
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
