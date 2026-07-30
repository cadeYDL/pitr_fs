# Phase 6 生产版基准报告

本报告由 `bench/bench-prod.sh` 在生产 daemon、JuiceFS 和双层 FUSE 真实挂载上生成。plain 与 pitr 使用同一容器、同一 PostgreSQL 和同一 JuiceFS 卷；plain 直接访问底层挂载，pitr 访问用户可见的版本化挂载。

## 环境

- 时间：2026-07-30T11:09:29+08:00
- 内核：Linux 7.0.11-orbstack-00360-gc9bc4d96ac70
- 镜像：pitr-fs:phase6
- 独立轮数：3（结果取中位数）
- 元数据样本：2000 个文件
- 顺序 I/O 样本：256 MiB

## 结果

| 指标 | plain | pitr | 退化/耗时 | 阈值 | 判定 |
|---|---:|---:|---:|---:|:---:|
| metadata_create_ms_op | 2.0749 ms/op | 10.1335 ms/op | 388.38% | ≤ 30% | FAIL |
| sequential_write_mib_s | 1622.79 MiB/s | 1350.52 MiB/s | 16.78% | ≤ 10% | FAIL |
| sequential_read_mib_s | 1444.85 MiB/s | 1389.17 MiB/s | 3.85% | ≤ 10% | PASS |
| revert_1gib_ms | 0.00 ms | 270.76 ms | 270.76 ms | ≤ 500 ms | PASS |

## 空间快照

- history 行数：36680
- history 表总大小：11075584 bytes

## 结论与优化路径

以下指标未达到目标；功能正确性不受影响，但不能把这些目标作为当前版本的性能承诺：

- `metadata_create_ms_op`：实际 388.38%，目标 ≤ 30%。
- `sequential_write_mib_s`：实际 16.78%，目标 ≤ 10%。

建议按以下顺序优化并用同一脚本复测：

1. 元数据：把连续操作的 auto window 合并为显式批次，减少每个 FUSE 调用的 PostgreSQL 往返；保留失败补偿边界。
2. 写吞吐：确认内核 `CAP_PASSTHROUGH`/stacking depth 实际启用，用 FUSE profile 定位剩余用户态拷贝；保留 fd auto 与 direct-I/O 一致性边界。
3. 读吞吐：用 FUSE/JuiceFS profile 区分缓存失效和用户态拷贝成本，只针对读路径恢复可证明安全的缓存。
4. revert：对 scope 闭包和 history 回放执行 `EXPLAIN ANALYZE`，按结果补组合索引或批量回放。

原始 CSV、环境和空间快照位于运行时的 `PITR_PROD_RESULTS` 目录。
