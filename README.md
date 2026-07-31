# pitr-fs

> **平台限制：pitr-fs 当前仅支持 Linux。** macOS 和 Windows 不能直接安装或
> 运行服务端；安装脚本会检测操作系统并拒绝在非 Linux 环境继续执行。

pitr-fs 是运行在 JuiceFS 之上的时间回溯文件系统。它透明拦截挂载目录中的
写操作，把每次成功的 POSIX 写操作记录为一个版本，并通过 PostgreSQL
元数据 undo-log 恢复目录或整个卷，无需为每个版本复制完整文件。

## 功能描述

- 默认自动版本模式，无需 `begin` 或 `commit`。创建、写入、截断、删除、
  重命名、链接和扩展属性变更都会自动形成版本。
- 默认全局保留最近 100 个版本；`history-limit` 可配置并持久化。
- CLI 支持绝对路径和相对路径，相对路径按调用命令时的工作目录解析。
- 支持按 12 位版本号回滚，也支持按带时区的 RFC3339 时间回滚；后者选择
  完成时间不晚于目标时间的最近版本。
- `logs -l` 展示版本号、POSIX 操作、调用进程命令、操作时间、操作人和
  内容变化摘要。
- `clear --global --yes` 永久清空历史，同时保留当前文件作为新基线。
- 支持目录范围 `diff`、服务恢复以及 Go/Python SDK。

内容摘要是有界诊断信息，不是完整 diff：每个文件最多读取前 4 KiB，一个
写窗口最多保留 3 个 64 B 样本；二进制文件只显示类型和大小。进程命令从
Linux `/proc/<pid>/cmdline` 尽力获取，用户名按 UID 从宿主机只读
`/etc/passwd` 解析。命令超过 10 个 Unicode 字符后会缩略。

## QuickStart

以下命令都应在 **Linux 主机**中执行。

### 1. 一键安装宿主机依赖

```bash
git clone <仓库地址> pitr-fs
cd pitr-fs
./scripts/install-deps.sh
./scripts/install-deps.sh --check
```

依赖脚本支持使用 `apt`、`dnf`、`yum`、`pacman` 或 `zypper` 的常见 Linux
发行版，会安装 Docker、FUSE3、util-linux、CA 证书、curl 和 git，并尽力
启动 Docker。最小宿主机要求是可用的 `/dev/fuse`、Docker daemon 和支持
shared bind propagation 的 Linux 内核。

### 2. 安装服务

```bash
./install.sh install
pitr status
```

安装只启动服务，不会擅自占用用户目录。默认允许挂载到 `/pitr` 的子目录；
可在首次安装时通过 `PITR_MOUNT_ROOT=/自定义根目录` 修改。挂载根不能是 `/`。

### 3. 初始化并挂载目录

绝对路径：

```bash
mkdir -p /pitr/data
pitr init /pitr/data
```

相对路径：

```bash
cd /pitr
mkdir -p data
pitr init ./data
```

当前版本一个服务只管理一个全局卷和一个挂载路径。`init` 幂等执行，并把路径
与保留策略写入 PostgreSQL；容器重启后会自动恢复这个挂载。

### 4. 快速验证

```bash
cd /pitr/data
mkdir -p quickstart
echo 'v1sdadsadas' > quickstart/test.txt
echo 'v2dasdas' > quickstart/test.txt
pitr logs quickstart/test.txt -l -n 10
```

复制第一列中的目标版本号，然后回滚：

```bash
pitr revert <版本号> --path quickstart
cat quickstart/test.txt
```

恢复服务、卸载或彻底清理：

```bash
./install.sh recover
./install.sh uninstall
./install.sh uninstall --purge  # 同时永久删除数据库和对象数据卷
```

## 使用指南

查看状态和历史：

```bash
pitr status
pitr logs . -n 20
pitr logs . -l -n 20
```

长日志列顺序固定为：

```text
版本号  POSIX操作  原始命令  操作时间  操作人  内容变化
```

设置持久化的全局历史上限，允许范围为 `1..100000`：

```bash
pitr config set history-limit 100
```

按版本或时间回滚：

```bash
pitr revert 2c45c99418e8
pitr revert 2c45c99418e8 --path ./project
pitr revert 2c45c99418e8 --global
pitr revert --at '2026-07-31T18:30:00+08:00' --path ./project
pitr revert --at '2026-07-31T10:30:00Z' --dry-run
```

比较版本、清空历史和手动卸载/重新挂载：

```bash
pitr diff 2c45c99418e8 7a09ce104d31 --path ./project
pitr clear --global --yes
pitr umount /pitr/data
pitr mount /pitr/data
```

`clear` 不删除当前文件和持久配置，但被清除的历史无法恢复。当前控制面只
开放全局配置与全局清理。

### 性能测试

对已经 `init` 的真实生产路径执行基准：

```bash
PITR_BENCH_MOUNT=/pitr/data ./bench/bench-prod.sh
```

快速小样本：

```bash
PITR_BENCH_META_COUNT=200 PITR_BENCH_IO_MIB=64 \
PITR_BENCH_MOUNT=/pitr/data ./bench/bench-prod.sh
```

正式报告建议执行三轮并取中位数：

```bash
for run in 1 2 3; do
  PITR_PROD_RESULTS="/tmp/pitr-prod-$run" \
  PITR_BENCH_MOUNT=/pitr/data ./bench/bench-prod.sh
done

python3 ./bench/prod_aggregate.py /tmp/pitr-prod-median.csv \
  /tmp/pitr-prod-{1,2,3}/prod.csv
```

详细口径见 [`bench/README.md`](bench/README.md)、
[`bench/BASELINE.md`](bench/BASELINE.md) 和 [`bench/PROD.md`](bench/PROD.md)。

## 例子

`logs -l` 的典型输出如下，实际路径、用户和版本号会不同：

```text
2c45c99418e8  open("/pitr/data/test.txt", O_WRONLY|O_CREAT|O_TRUNC, 0644)  bash  2026-07-31T18:20:10+08:00  user(uid=1000,gid=1000,pid=321)  ∅ -> ""
7a09ce104d31  write("/pitr/data/test.txt", offset=0, total=11, calls=1)  bash  2026-07-31T18:20:10+08:00  user(uid=1000,gid=1000,pid=321)  "" -> "v1sdadsadas"
```

一个 `echo` 可能对应创建/截断和写入两个 POSIX 操作，因此形成两个版本；
这些版本会尽力记录相同的调用进程命令。版本只在操作成功后关闭，按时间
回滚不会选择仍在写入中的开放版本。

按时间回滚的 Shell 例子：

```bash
target_time="$(date --iso-8601=seconds)"
sleep 1
echo later > test.txt
pitr revert --at "$target_time" --path .
```

## 设计概述

```text
应用 POSIX 操作
    -> 用户挂载点上的 FUSE proxy（打开自动版本并采集有界审计信息）
    -> 私有 JuiceFS 挂载（文件数据写入对象存储）
    -> PostgreSQL 触发器（把 jfs_* 旧值写入 pitr_*_history）
    -> 操作完成后关闭版本并按 history-limit 裁剪
```

安装时，宿主机的 `PITR_MOUNT_ROOT` 以相同绝对路径、`rshared` 方式绑定进
容器。`pitr init <path>` 只能选择该根目录下的子目录，daemon 随后在同一路径
创建 FUSE 挂载并持久化配置。这个边界避免把整个宿主根文件系统暴露给容器。

`pitrd` 通过 Unix socket 提供 gRPC 控制面。CLI 和 SDK 负责路径解析与请求
封装；版本选择、完整性检查、目录范围解析和 history 回放在 daemon 内完成。

回滚不会复制整份文件。daemon 在一个 PostgreSQL 事务中锁定版本时间线，
逆序恢复目标版本之后的 JuiceFS 元数据，并把回滚本身记录为新版本。目录级
回滚结合当前目录图与历史 edge 计算 inode 闭包，因此目录被改名或删除后
仍可定位。

历史上限会裁剪版本和对应 history，但对象块的实际回收仍由 JuiceFS 的
trash/retention 策略负责；版本数量与底层对象空间上限不是同一个概念。

## 后续演进设想

1. 开放目录级 `history-limit`：子目录继承父目录，子目录显式配置优先。
2. 增加按时间和空间预算管理 JuiceFS trash/blob 的可观测 GC，给出明确的
   最早可恢复时间，而不只限制版本条数。
3. 在明确授权和安全边界下增强原始 Shell 命令追踪；当前 `/proc` 方案无法
   保证还原所有 Shell 内建、管道和重定向原文。
4. 为高频元数据写入增加具备原子性证明的批量自动版本，减少数据库往返。
5. 为大目录 scope 闭包和 history 回放加入分区、组合索引与批处理。
6. 支持多卷、远端对象存储凭证管理、指标导出和告警。
