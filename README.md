# pitr-fs

pitr-fs 是运行在 JuiceFS 之上的时间回溯文件系统。它拦截用户可见挂载点的
写操作，把每次成功写操作自动记录为一个版本，并通过 PostgreSQL 元数据
undo-log 在不复制完整文件的前提下恢复目录或整个卷。

当前安装方式面向 Linux。本文所有示例均假设项目与运行命令位于 OrbStack
`calw` 环境，用户可见挂载目录为 `/pitr_fs/data`。

## 功能描述

- 默认自动版本模式：不需要 `begin` 或 `commit`，成功的创建、写入、截断、
  删除、重命名、链接和扩展属性变更都会形成版本。
- 默认全局保留最近 100 个版本；`history-limit` 可调整并持久化。
- 支持相对路径。CLI 和 SDK 会按调用者当前工作目录解析路径。
- 支持按 12 位版本号回滚，也支持按带时区的 RFC3339 时间回滚。按时间回滚
  选择操作完成时间不晚于目标时间的最近完整版本。
- `logs -l` 可展示版本号、POSIX 操作、调用进程命令、操作时间、操作人和
  内容变化摘要。
- `clear --global --yes` 可永久清空历史，但保留当前文件作为新基线。
- 支持目录范围 `diff`、挂载恢复和 Go/Python SDK。

内容摘要是诊断信息而不是完整 diff：实现最多读取文件前 4 KiB，并为一个
写窗口保留至多 3 个 64 B 写入样本。二进制文件只显示类型和大小。调用者
命令从 `/proc/<pid>/cmdline` 尽力获取，用户名按 UID 从 calw 的只读
`/etc/passwd` 回退解析。安装容器为此与 calw 共享 PID namespace；shell
内建命令仍可能只能显示 shell 进程。命令超过 10 个 Unicode 字符后显示
省略号。

## 使用指南

查看运行状态和版本历史：

```bash
pitr status
pitr logs . -n 20
pitr logs . -l -n 20
```

长日志列顺序固定为：

```text
版本号  POSIX操作  原始命令  操作时间  操作人  内容变化
```

设置全局历史上限。允许范围是 `1..100000`，配置保存在 PostgreSQL 中，
容器重启后仍然有效：

```bash
pitr config set history-limit 100
```

按版本号回滚当前目录，或显式指定范围：

```bash
pitr revert 2c45c99418e8
pitr revert 2c45c99418e8 --path ./project
pitr revert 2c45c99418e8 --global
```

按时间回滚。时间必须包含时区，且不能晚于当前时间：

```bash
pitr revert --at '2026-07-31T18:30:00+08:00'
pitr revert --at '2026-07-31T10:30:00Z' --path ./project
```

先预览预计回放的 history 行数：

```bash
pitr revert 2c45c99418e8 --dry-run
pitr revert --at '2026-07-31T18:30:00+08:00' --dry-run
```

比较两个版本对指定目录造成的元数据变化：

```bash
pitr diff 2c45c99418e8 7a09ce104d31 --path ./project
```

清空全部历史：

```bash
pitr clear --global --yes
```

`clear` 不删除当前文件和持久配置，但历史无法恢复。当前控制面只允许全局
配置与全局清理。

## QuickStart

以下命令必须在 calw 内执行，不要在 macOS 宿主机直接运行：

```bash
orb -m calw
cd /Users/ydl/workspace/repo/pitr_fs

sudo mkdir -p /pitr_fs/data
sudo chown "$(id -u):$(id -g)" /pitr_fs/data

PITR_WORKSPACE=/pitr_fs/data ./install.sh install
pitr status
```

写入两个版本并查看审计日志：

```bash
cd /pitr_fs/data
mkdir -p quickstart
echo 'v1sdadsadas' > quickstart/test.txt
echo 'v2dasdas' > quickstart/test.txt
pitr logs quickstart/test.txt -l -n 10
```

复制第一列中的目标版本号，然后回滚并检查内容：

```bash
pitr revert <版本号> --path quickstart
cat quickstart/test.txt
```

也可以记录一个时间点后再回滚：

```bash
target_time="$(date --iso-8601=seconds)"
sleep 1
echo 'later' > quickstart/test.txt
pitr revert --at "$target_time" --path quickstart
```

生产路径性能测试使用真实 `pitrd + JuiceFS + FUSE proxy`：

```bash
cd /Users/ydl/workspace/repo/pitr_fs
PITR_CONTAINER=pitrfs ./bench/bench-prod.sh
```

缩小样本的快速检查：

```bash
PITR_BENCH_META_COUNT=200 PITR_BENCH_IO_MIB=64 \
  PITR_CONTAINER=pitrfs ./bench/bench-prod.sh
```

正式报告建议执行三轮并取中位数：

```bash
for run in 1 2 3; do
  PITR_PROD_RESULTS="/tmp/pitr-prod-$run" \
    PITR_CONTAINER=pitrfs ./bench/bench-prod.sh
done

python3 ./bench/prod_aggregate.py /tmp/pitr-prod-median.csv \
  /tmp/pitr-prod-{1,2,3}/prod.csv
```

基准原始数据位于 `PITR_PROD_RESULTS`，默认是
`/tmp/pitr-prod-bench`；可用 `PITR_PROD_REPORT` 指定报告路径，默认更新
`bench/PROD.md`。历史基线和详细口径见
[`bench/README.md`](bench/README.md)、[`bench/BASELINE.md`](bench/BASELINE.md)
和 [`bench/PROD.md`](bench/PROD.md)。

卸载但保留数据：

```bash
PITR_WORKSPACE=/pitr_fs/data ./install.sh uninstall
```

彻底删除容器、wrapper 和两个数据卷：

```bash
PITR_WORKSPACE=/pitr_fs/data ./install.sh uninstall --purge
```

## 例子

典型的 `logs -l` 输出如下。实际路径、用户和版本号会不同：

```text
2c45c99418e8  open("/pitr_fs/data/test.txt", O_WRONLY|O_CREAT|O_TRUNC, 0644)  bash  2026-07-31T18:20:10+08:00  ydl(uid=1000,gid=1000,pid=321)  ∅ -> ""
7a09ce104d31  write("/pitr_fs/data/test.txt", offset=0, total=11, calls=1)  bash  2026-07-31T18:20:10+08:00  ydl(uid=1000,gid=1000,pid=321)  "" -> "v1sdadsadas"
```

一个 `echo` 可能对应“创建/截断”和“写入”两个 POSIX 操作，因此形成两个
版本；它们会尽力记录相同的调用进程命令。版本只在操作成功后关闭，按时间
回滚不会选择仍在写入中的开放版本。

Go SDK：

```go
client, err := pitr.Dial("/var/run/pitrd.sock")
if err != nil {
    log.Fatal(err)
}
defer client.Close()

target := time.Date(2026, 7, 31, 18, 30, 0, 0, time.FixedZone("CST", 8*3600))
result, err := client.RevertAt(
    context.Background(), target, pitr.WithPath("./project"))
```

Python SDK：

```python
from datetime import datetime
from pitr import Client

with Client("/var/run/pitrd.sock") as client:
    result = client.revert_at(
        datetime.fromisoformat("2026-07-31T18:30:00+08:00"),
        path="./project",
    )
```

## 设计概述

写路径由四层组成：

```text
应用 POSIX 操作
    -> 用户可见 FUSE proxy（打开自动版本并采集有界审计信息）
    -> JuiceFS 挂载（文件数据写入对象存储）
    -> PostgreSQL 元数据触发器（记录 jfs_* 旧值到 pitr_*_history）
    -> 操作完成后关闭版本并按 history-limit 裁剪
```

`pitrd` 通过 Unix socket 提供 gRPC 控制面。CLI 和 SDK 只负责路径解析与
请求封装；版本选择、完整性检查、目录范围解析和 history 回放都在 daemon
内完成。

回滚不会复制整份文件。daemon 在一个 PostgreSQL 事务中锁定版本时间线，
按目标版本之后的 history 逆序恢复 JuiceFS 元数据，并把这次回滚本身记录为
新版本。目录级回滚使用当前目录图与历史 edge 合成 inode 闭包，避免目录被
重命名或删除后无法定位。

自动版本默认只保留 100 条。裁剪会同步删除对应 history；JuiceFS 的对象
回收由其 trash/retention 策略负责。`clear` 删除所有非根版本并把当前状态
作为新的逻辑基线。

## 后续演进设想

1. 开放目录级 `history-limit` 配置。数据模型已经支持父目录继承和子目录
   覆盖，后续需要补控制面、裁剪分区与配置冲突规则。
2. 在明确授权和安全边界下增强原始 shell 命令追踪；当前共享 PID namespace
   的 `/proc` 方案仍无法保证还原 shell 内建命令、管道和重定向原文。
3. 为高频元数据写入加入可证明原子性的批量自动版本，减少每个 POSIX 操作的
   PostgreSQL 往返。
4. 为大目录 scope 闭包和 history 回放补组合索引、分区与批处理，并持续用
   `bench/bench-prod.sh` 验证。
5. 增加更丰富但仍有界的文本 diff、二进制指纹和可配置审计保留策略。
6. 支持远端对象存储凭证管理、多卷编排、指标导出和可观测性告警。
