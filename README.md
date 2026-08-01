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
- 可按用户熟悉的文件容量设置空间上限和预留比例；默认在预计占用达到上限的
  80% 时从最老版本开始裁剪，共享 slice 只在最后一个引用释放时计入空间。
- CLI 支持绝对路径和相对路径，相对路径按调用命令时的工作目录解析。
- 支持按 12 位版本号回滚，也支持按带时区的 RFC3339 时间回滚；后者选择
  完成时间不晚于目标时间的最近版本。
- `logs -l` 展示版本号、POSIX 操作、调用进程命令、操作时间、操作人和
  内容变化摘要。
- `clear --global --yes` 永久清空历史，同时保留当前文件作为新基线。
- 历史版本在 slice 维度持有对象引用；版本淘汰或 `clear` 后自动释放引用，
  后台批量调用 JuiceFS 原生 GC 回收不再可达的对象。
- 块数据默认保存在本地 Docker volume，也可直接使用用户已经挂载到 Linux
  主机的任意目录，或使用 JuiceFS 支持的远端对象存储。
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
发行版，会安装 Docker、FUSE3、util-linux、CA 证书、curl、git、Python 3
和 awk，并尽力启动 Docker。最小宿主机要求是可用的 `/dev/fuse`、Docker daemon 和支持
shared bind propagation 的 Linux 内核。

### 2. 安装服务

```bash
./install.sh install
pitr status
```

安装只启动服务，不会擅自占用用户目录。默认允许挂载到 `/pitr` 的子目录；
可在首次安装时通过 `PITR_MOUNT_ROOT=/自定义根目录` 修改。挂载根不能是 `/`。

默认块数据写入本地 Docker volume。若块存储已经由用户挂载到 Linux 主机，
可将其目录绑定给 pitr-fs：

```bash
PITR_BLOCK_PATH=/mnt/pitr-blocks ./install.sh install
```

`PITR_BLOCK_PATH` 必须是可写的绝对路径，且不能与 `PITR_MOUNT_ROOT` 重叠。
卸载时即使指定 `--purge`，安装脚本也不会删除这个用户目录。S3、MinIO、OSS
等后端仍可通过 `PITR_STORAGE`、`PITR_BUCKET` 和对应凭证配置。

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

初始化时同时设置版本数和空间水位：

```bash
pitr init ./data --history-limit 100 --max-space 100GiB --space-reserve 20
```

这里的 `100GiB` 表示当前文件和可恢复历史预计可占用的文件数据额度；预留
`20%` 表示达到约 `80GiB` 时开始淘汰最老版本。估算按去重 slice 大小计算，
不包含对象存储协议开销、压缩差异和异步 GC 尚未删除的临时占用。

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
pitr config set max-space 100GiB
pitr config set space-reserve 20%
```

查看当前空间水位，以及按最老优先顺序单独删除每个版本的预计释放量：

```bash
pitr space . -n 20
```

输出中的 `retained` 是当前文件和可恢复版本仍需保留的 slice；`reclaimable`
是引用已经归零、等待后台 GC 物理删除的空间。`RELEASE_IF_DELETED` 是以查询
时引用关系计算的边际值，前一个版本删除后，共享 slice 的释放量可能转移到
后续版本，因此裁剪器每删除一个版本都会重新判断。

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
[`bench/BASELINE.md`](bench/BASELINE.md)、[`bench/PROD.md`](bench/PROD.md)
以及 [`docs/性能瓶颈.md`](docs/性能瓶颈.md)。

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

每份首次捕获的旧 `jfs_chunk` 状态都会把其中 slice 以紧凑二进制列表挂到
对应版本，并增加 JuiceFS 物理引用计数。版本因 `history-limit` 淘汰或被
`clear` 删除时，同一数据库事务会逐 slice 减少引用并写入持久化 GC 请求。
原来的远期 pin 会被转换为 JuiceFS 立即到期的待删记录，而不是直接丢弃，确保
零引用 slice 仍能被原生 delete 扫描发现。
daemon 低频合并请求，在没有开放写窗口时执行 JuiceFS 原生 `gc --delete`；
进程崩溃不会丢请求，GC 失败也不会影响当前数据。例行生命周期 GC 不主动
compact 当前 chunk，减少对前台读取和写入的扰动。
这使“最多 100 个版本”同时约束元数据历史和这些历史独占的块对象，不过当前
版本本身、JuiceFS 合并过程的临时对象以及异步 GC 尚未运行时的对象仍会占用
空间，因此它不是严格的字节配额。

`PITR_GC_INTERVAL` 控制后台合并间隔（默认 `10m`，`0` 停用），
`PITR_GC_THREADS` 控制对象删除并发（默认 `4`）。停用 GC 只会延后物理删除，
不会破坏版本引用的正确性。

空间计数由 `jfs_chunk_ref` 的增量触发器维护，不在普通写入中遍历对象存储。
当引用从正数降为零时，slice 从 `retained` 转入 `reclaimable`，并把预计字节
累计到 GC 请求。空间水位与版本数上限同时生效，任何一项超限都会触发裁剪；
空间策略允许最终删除全部用户版本，但永远不会为了配额删除当前文件内容。
如果当前文件本身已经超过高水位，系统会报告超限，删除历史也无法继续降低。

## 后续演进设想

1. 开放目录级 `history-limit`：子目录继承父目录，子目录显式配置优先。
2. 在现有版本数与空间高水位上增加时间预算、严格物理字节采样、回收进度指标
   和明确的最早可恢复时间，并支持人工触发和限速。
3. 在明确授权和安全边界下增强原始 Shell 命令追踪；当前 `/proc` 方案无法
   保证还原所有 Shell 内建、管道和重定向原文。
4. 为高频元数据写入增加具备原子性证明的批量自动版本，减少数据库往返。
5. 为大目录 scope 闭包和 history 回放加入分区、组合索引与批处理。
6. 支持多卷、远端对象存储凭证管理、指标导出和告警。
