# Phase 0 Demo 生产栈运行记录

## 结论

2026-07-30 在 OrbStack `calw`（`calw.orb.local`）中用最终生产栈重跑
`demo/README.md` §1–§7 的等价场景。生产栈包含 PostgreSQL 16、JuiceFS
1.4.0、生产 schema、pitrd 和用户可见 FUSE，不再使用 demo 的时间戳归属
和手工 SQL revert。八个关键机制全部通过，尤其是 v3 回到 v1 后：

```text
a.txt
hello v1
```

所有宿主命令均由 `orb -m calw sh -lc '…'` 发起。测试容器是
`pitrfs-phase6`（镜像 `pitr-fs:final`），宿主测试路径是
`/tmp/pitr-phase6-workspace/p0-proof`，容器路径是
`/workspace/p0-proof`。

## §1 PostgreSQL 已启动并就绪

生产镜像由 entrypoint 启动 PostgreSQL，并在 socket ready 后接受 CLI 请求：

```bash
docker inspect -f 'image={{.Config.Image}} status={{.State.Status}}' pitrfs-phase6
docker exec pitrfs-phase6 psql --version
docker exec pitrfs-phase6 pitr status
```

```text
image=pitr-fs:final status=running
psql (PostgreSQL) 16.14 (Debian 16.14-1.pgdg13+1)
connected to pitrd dev, 1 volumes, 0 active transactions
default  jfs=/var/lib/pitr/jfs  fuse=/workspace  history-limit=100
```

## §2 JuiceFS 客户端可用

```bash
docker exec pitrfs-phase6 juicefs version
```

```text
juicefs version 1.4.0+2026-07-06.62bedf3
```

## §3 JuiceFS 卷与双层 FUSE 已挂载

```bash
mountpoint -q /tmp/pitr-phase6-workspace
docker exec pitrfs-phase6 pitr status
```

`mountpoint` 返回 0；status 同时报告底层 `/var/lib/pitr/jfs` 和用户层
`/workspace`。随后从 calw 宿主路径写文件、从容器内 CLI 管理版本，证明
`rshared` 挂载传播有效。

## §4 核心 JuiceFS 表存在

```bash
docker exec pitrfs-phase6 psql -U pitr -d pitr_fs -Atc \
  "SELECT tablename FROM pg_tables
   WHERE tablename IN
   ('jfs_node','jfs_edge','jfs_chunk','jfs_chunk_ref')
   ORDER BY tablename"
```

```text
jfs_chunk
jfs_chunk_ref
jfs_edge
jfs_node
```

## §5 生产 schema 与触发器已安装

```bash
docker exec pitrfs-phase6 psql -U pitr -d pitr_fs -Atc \
  "SELECT DISTINCT event_object_table || '=' || trigger_name
   FROM information_schema.triggers
   WHERE trigger_name LIKE 'tg_pitr_%'
   ORDER BY 1"
```

```text
jfs_chunk=tg_pitr_chunk_capture
jfs_chunk_ref=tg_pitr_chunk_ref_capture
jfs_edge=tg_pitr_edge_capture
jfs_node=tg_pitr_node_capture
```

entrypoint 对 schema 安装和 format 都是幂等的：已有 JuiceFS 元数据时跳过
format，只重新校准 schema。

## §6 单文件 v1 → v2 → v3 → revert v1

### §6.1 创建 v1

```bash
docker exec pitrfs-phase6 pitr begin /workspace/p0-proof -m 'P0 v1'
echo 'hello v1' > /tmp/pitr-phase6-workspace/p0-proof/a.txt
docker exec pitrfs-phase6 pitr commit /workspace/p0-proof \
  -m 'P0 v1 committed'
```

```text
started txn 92591b05f233 @ /workspace/p0-proof
committed txn 92591b05f233 @ /workspace/p0-proof
```

### §6.2 修改到 v2

```bash
docker exec pitrfs-phase6 pitr begin /workspace/p0-proof -m 'P0 v2'
echo 'hello v2 rewritten' > /tmp/pitr-phase6-workspace/p0-proof/a.txt
docker exec pitrfs-phase6 pitr commit /workspace/p0-proof \
  -m 'P0 v2 committed'
```

```text
started txn 6f1d7bce3d12 @ /workspace/p0-proof
committed txn 6f1d7bce3d12 @ /workspace/p0-proof
```

### §6.3 修改到 v3

```bash
docker exec pitrfs-phase6 pitr begin /workspace/p0-proof -m 'P0 v3'
rm /tmp/pitr-phase6-workspace/p0-proof/a.txt
echo 'brand new' > /tmp/pitr-phase6-workspace/p0-proof/b.txt
docker exec pitrfs-phase6 pitr commit /workspace/p0-proof \
  -m 'P0 v3 committed'
```

```text
started txn 2aae99f48927 @ /workspace/p0-proof
committed txn 2aae99f48927 @ /workspace/p0-proof
```

### §6.4 当前状态

```bash
find /tmp/pitr-phase6-workspace/p0-proof -maxdepth 1 -type f -printf '%f\n'
cat /tmp/pitr-phase6-workspace/p0-proof/b.txt
```

```text
b.txt
brand new
```

### §6.5 版本与四类 history

生产版直接以 `txn_id` 归属，不依赖 demo 的时间戳区间。按三个版本聚合：

```text
2aae99f48927|chunk|1
2aae99f48927|chunk_ref|1
2aae99f48927|edge|3
2aae99f48927|node|3
6f1d7bce3d12|chunk|1
6f1d7bce3d12|chunk_ref|1
6f1d7bce3d12|node|1
92591b05f233|chunk|1
92591b05f233|chunk_ref|1
92591b05f233|edge|1
92591b05f233|node|1
```

这覆盖创建、内容更新、目录项删除以及新文件创建；四张 history 表均有记录。

### §6.6–§6.7 回到 v1 并验证内容

```bash
docker exec pitrfs-phase6 pitr revert 92591b05f233 \
  --path /workspace/p0-proof
find /tmp/pitr-phase6-workspace/p0-proof -maxdepth 1 -type f -printf '%f\n'
cat /tmp/pitr-phase6-workspace/p0-proof/a.txt
test ! -e /tmp/pitr-phase6-workspace/p0-proof/b.txt
```

```text
reverted 11 history rows; new version da1563602a38
a.txt
hello v1
```

无需手工 remount：生产 daemon 在 revert 后完成缓存一致性处理。

## §7 目录级回滚不影响其他子树

v4 同时创建 `proj/a.txt=project v4` 和 `other/x.txt=other file`；v5 把二者
分别改为 `project v5`、`other v5`，随后只回退 `proj`：

```bash
docker exec pitrfs-phase6 pitr revert 46cfc18d33cc \
  --path /workspace/p0-proof/proj
cat /tmp/pitr-phase6-workspace/p0-proof/proj/a.txt
cat /tmp/pitr-phase6-workspace/p0-proof/other/x.txt
```

```text
reverted 2 history rows; new version 424866aafffc
project v4
other v5
```

`proj` 回到 v4，范围外的 `other` 保持 v5，精确子树边界通过。

## §8 关键机制检查

| 机制 | 结果 | 生产证据 |
|---|:---:|---|
| JuiceFS + PG 元数据引擎 | ✅ | status、核心表和实际 POSIX I/O |
| 捕获 INSERT/UPDATE/DELETE | ✅ | v1/v2/v3 的 node/edge history |
| 版本归属 | ✅ | 生产 `txn_id` 精确归属，比时间戳关联更强 |
| 恢复 `jfs_node` | ✅ | v1 的 size/内容状态恢复 |
| 恢复 `jfs_edge` | ✅ | `b.txt` 消失、`a.txt` 恢复 |
| 恢复 `jfs_chunk` | ✅ | `cat a.txt` 为 `hello v1` |
| 恢复 `jfs_chunk_ref` | ✅ | v1 blob 可读且相应 history 存在 |
| 目录回滚隔离其他子树 | ✅ | `proj=project v4`，`other=other v5` |
