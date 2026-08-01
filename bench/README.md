# pitr-fs 基准测试

本目录提供可在任意受支持 Linux 主机运行的生产基准。推荐入口是
`run-io-recovery.sh`：它比较普通文件系统与已挂载 pitrfs 的顺序/随机读写，
并验证单文件、多文件、混合新增删除以及多目录恢复。

脚本不依赖机器名、固定容器名或固定挂载路径，同时支持 x86-64 和 ARM64。
macOS、Windows 以及未提供 `/dev/fuse` 的环境不在支持范围内。

## 前置条件

先按项目根目录 [README](../README.md) 完成安装，并使用 `pitr init` 挂载一个
**专用的空目录**。项目安装脚本会准备运行基准所需的 Docker、Python 3、
`findmnt` 和 `realpath`；fio 由基准脚本在临时 Docker 容器中运行，不会向宿主机
安装或升级软件。

完整三轮测试建议至少预留 30 GiB 可用空间。基准会临时把全局
`history-limit` 调高到 5000，结束或出错时分批恢复原值，避免一次淘汰大量版本
超过 CLI 超时。请勿在生产数据的
挂载点运行，因为测试会产生大量写入和历史版本。

## QuickStart

以下命令均从项目根目录执行。`/pitr/bench` 只是示例，可以替换为安装时
`PITR_MOUNT_ROOT` 下的任意专用空目录：

```bash
mkdir -p /pitr/bench
test -z "$(find /pitr/bench -mindepth 1 -maxdepth 1 -print -quit)"
pitr init /pitr/bench

./bench/run-io-recovery.sh /pitr/bench
```

脚本会自动完成以下工作：

1. 检查 Linux、pitr 服务、pitrfs 挂载、Docker 和基础命令。
2. 构建或复用多架构 fio 镜像，并创建名称唯一的临时容器。
3. 在 `/var/tmp` 创建普通文件系统样本，在挂载点下创建名称唯一的测试目录。
4. 运行三轮 I/O 与四类恢复测试，逐项校验恢复内容。
5. 在较高历史窗口下删除当前测试文件，再分批恢复原来的 `history-limit` 并删除
   临时容器。

成功后会打印结果目录，其中包含：

| 文件 | 内容 |
|---|---|
| `REPORT.md` | I/O 中位数、性能比例和恢复耗时汇总 |
| `io-raw.json` | 每轮 fio 原始指标 |
| `io-median.json` | 按文件规模、操作和文件系统聚合的中位数 |
| `recovery-raw.json` | 每轮恢复耗时、校验状态和 CLI 输出 |
| `environment.json` | 内核、文件系统、挂载和脚本校验和 |

## 可配置项

所有环境差异都通过参数或环境变量传入，不需要修改脚本：

| 配置 | 默认值 | 说明 |
|---|---|---|
| 命令第一个参数 / `PITR_BENCH_MOUNT` | 无 | 已完成 `pitr init` 的挂载点，必填 |
| `PITR_BIN` | `pitr` | pitr 命令名或绝对路径 |
| `PITR_BENCH_ROUNDS` | `3` | 测试轮数，必须为正整数 |
| `PITR_BENCH_OUTPUT` | `/tmp/pitrfs-bench-results/<时间>` | 报告和原始数据目录 |
| `PITR_BENCH_NATIVE_ROOT` | `/var/tmp` | 普通文件系统临时样本的父目录 |
| `PITR_BENCH_FIO_IMAGE` | `pitrfs-bench-fio:local` | 自动构建/复用的 fio 镜像名 |
| `PITR_BENCH_KEEP_DATA` | `0` | 设为 `1` 时保留当前测试文件 |
| `PITR_BENCH_SKIP_IO` | `0` | 设为 `1` 时复用输出目录的 `io-median.json`，只续跑恢复 |

例如，做一轮快速验证、把结果保存在当前用户目录：

```bash
PITR_BENCH_ROUNDS=1 \
PITR_BENCH_OUTPUT="$HOME/pitr-bench-result" \
./bench/run-io-recovery.sh /your/pitr/mount
```

如果当前用户尚未获得 Docker 组权限，脚本会自动尝试 `sudo docker`。第一次运行
需要联网获取 Debian 基础镜像和 fio 软件包；后续构建会使用 Docker 缓存。

## 测试口径

I/O 覆盖四档文件和四类操作：

| 规模 | 文件大小 | 顺序读写 | 随机读写 |
|---|---:|---|---|
| 小 | 4 KiB | 完整文件 | 4 KiB 块 |
| 中 | 1 MiB | 完整文件 | 4 KiB 块 |
| 大 | 256 MiB | 完整文件 | 4 KiB 块 |
| 超大 | 2 GiB | 完整文件 | 4 KiB 块 |

fio 使用 `psync`、`iodepth=1`、`direct=1`。轮次会交替普通文件系统与 pitrfs
的执行顺序，最终使用中位数减少缓存和瞬时负载影响。恢复测试覆盖：

- 单个 256 MiB 文件的局部修改恢复；
- 1000 个文件全部修改后恢复；
- 600 个文件混合修改、删除和新建后恢复；
- 多层目录树中 1000 个文件及目录结构恢复。

## 清理与注意事项

默认会删除临时 fio 容器、普通盘样本和挂载点中的当前测试文件，但这些删除本身
也会形成版本。恢复原 `history-limit` 后，超出保留数量的版本会进入后台 GC，
对象空间不保证在脚本退出瞬间全部返还。需要完全隔离性能数据时，请为基准使用
独立的 pitr-fs 安装实例，测试后按项目根 README 的卸载说明清理。

若测试中断，可安全地重新执行脚本。遗留容器名包含时间和进程号；可用下面命令
检查，确认后删除：

```bash
docker ps -a --filter 'name=pitrfs-fio-'
```

## 其他脚本

- `bench-prod.sh` 是 Phase 6 的容器内部回归基准，用于开发者比较底层 JuiceFS
  与上层 FUSE；参数见脚本开头的 `PITR_*` 环境变量。
- `setup.sh`、`bench-throughput.sh`、`bench-space.sh`、`bench-revert.sh` 和
  `bench-scale.py` 是 Go 实现前的设计验证工具，需要宿主机自行提供 JuiceFS、
  PostgreSQL 客户端和 fio，不作为最终用户 QuickStart 的组成部分。
- 已完成的标准环境实测结果见 [IO_RECOVERY.md](./IO_RECOVERY.md)。
