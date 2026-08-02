# Linux 安装与动态挂载验收

验收日期：2026-07-31

## 验收范围

- 服务端和安装脚本仅允许在 Linux 运行。
- 一键依赖脚本覆盖 Docker、FUSE3、util-linux、CA 证书、curl、git、
  Python 3 和 awk。
- `install.sh install` 启动服务但不自动占用用户目录。
- `pitr init <path>` 支持绝对路径与相对路径，动态创建 FUSE 挂载。
- 挂载路径和版本/空间配置在 PostgreSQL 中持久化，服务重启后自动恢复。
- 当前单卷只能选择一个挂载路径，路径不能逃逸配置的挂载根。
- 生产基准脚本可通过 `PITR_BENCH_MOUNT` 指定动态挂载点。
- 验收完成后不留下测试容器、挂载、wrapper、镜像或数据卷。

## 自动测试

以下检查全部通过：

```bash
bash -n install.sh scripts/install-deps.sh deploy/entrypoint.sh bench/bench-prod.sh
./scripts/install-deps.sh --check
go test ./...
go vet ./...
go test -race ./...
```

覆盖的新增单元/集成场景包括：

- 动态 `init` 选择挂载点并写入 `pitr_volume_config`。
- 同一路径重复 `init` 幂等，版本和空间配置更新后仍持久化。
- 相对路径由宿主 wrapper 解析为正确的绝对路径。
- 相对路径、挂载根本身、挂载根之外路径在 daemon 边界被拒绝。
- 已初始化卷不能切换到第二个路径。
- 持久化路径必须严格位于非根目录的 `mount-root` 下。
- 安装脚本 Linux 平台检查、同路径 shared bind 和安全失联 FUSE 清理。

## 真实 FUSE 验收

使用独立容器名、镜像名、临时 wrapper、挂载根和两个独立 Docker volume
完成了真实 `PostgreSQL + JuiceFS + FUSE proxy` 验收：

1. 安装完成后确认用户目录没有 `fuse.pitrfs` 挂载。
2. 从挂载根执行 `pitr init ./data`，确认宿主同一路径出现 `fuse.pitrfs`。
3. 依次写入 `v1`、`v2`，确认 `logs -l` 含 POSIX 操作、进程、时间、用户和
   `"v1" -> "v2"` 内容摘要。
4. 回滚到 `v1` 对应版本，文件内容恢复为 `v1`。
5. 重启容器，确认挂载自动恢复、内容仍为 `v1`、重复 `init` 成功。
6. 尝试 `init` 第二路径，确认返回 `FailedPrecondition`。
7. 执行 `install.sh recover`，确认服务和持久化挂载均恢复。
8. 以 10 个元数据操作、1 MiB I/O 的小样本运行 `bench/bench-prod.sh`，确认
   动态挂载参数贯穿全部生产基准步骤。

## 清理结果

隔离容器、两个数据卷、测试镜像、临时 wrapper、临时挂载目录和基准产物均
已删除；验收结束后没有正式安装 pitr-fs。
