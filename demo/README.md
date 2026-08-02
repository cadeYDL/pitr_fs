# pitr-fs 可行性 Demo (纯命令行)

**目标**:不写任何 Go 代码,只用 `docker` + `juicefs` + `psql` 三个工具,验证设计文档的核心机制:

1. JuiceFS 用 PG 做元数据引擎能跑起来。
2. 在 `jfs_node` / `jfs_edge` / `jfs_chunk` / `jfs_chunk_ref` 上挂触发器,能捕获所有变更(**元数据 + 内容层**)到 shadow 表。
3. 反向 replay shadow 表,能把文件系统状态**恢复到任意历史时间点**(含文件内容)。

**跑通 = 设计地基验证通过**,可以进入 Go 编码阶段。

**限制**(与生产版本的差异,demo 简化处理):

- 触发器**无条件**捕获所有变更(生产版会用 `pitr.current_txn` GUC 精确归属事务)。
- 版本归属靠 `recorded_at` 时间戳与 `pitr_txn.created_at` 关联(生产版按 txn_id 精确关联)。
- 没有 FUSE 拦截,直接在 JuiceFS 挂载点上用标准 POSIX 命令读写。
- Revert 期间 `DISABLE TRIGGER` 简化处理(生产版用 `session_replication_role`)。

---

## 0. 前置条件

- 本地已装 Docker
- 本地已装 `curl`
- Linux（pitr-fs 服务端不支持 macOS 或 Windows）

## 1. 起 PostgreSQL 容器

```bash
docker run -d --name pitr-demo-pg \
  -e POSTGRES_USER=pitr \
  -e POSTGRES_PASSWORD=pitr \
  -e POSTGRES_DB=pitr_fs \
  -p 127.0.0.1:55432:5432 \
  postgres:16.14-bookworm

# 等就绪
until docker exec pitr-demo-pg pg_isready -U pitr -d pitr_fs >/dev/null 2>&1; do
    sleep 1
done
echo "PG ready"
```

## 2. 准备固定 JuiceFS 客户端

该目录是早期机制 Demo，不是生产安装入口。为避免元数据结构漂移，运行时必须
使用主项目镜像中固定的 JuiceFS v1.3.0 `pitrfs.1` 构建，不能通过官方在线
安装脚本下载 `latest`。普通用户应直接按根目录 README 使用 `install.sh`，无需
执行本 Demo。

开发者确需运行 Demo 时，可从已经构建的项目镜像复制固定客户端：

```bash
container_id="$(docker create pitr-fs:latest)"
docker cp "$container_id:/usr/local/bin/juicefs" /tmp/juicefs
docker rm "$container_id"
sudo install -m 0755 /tmp/juicefs /usr/local/bin/juicefs
juicefs version
```

## 3. 格式化 JuiceFS 卷 + 挂载

```bash
export DSN="postgres://pitr:pitr@127.0.0.1:55432/pitr_fs?sslmode=disable"
export DATA_DIR="/tmp/pitr-demo-data"
export MNT="/tmp/pitr-demo-mnt"
mkdir -p "$DATA_DIR" "$MNT"

# format(数据放本地目录,元数据放 PG)
juicefs format \
  --storage file \
  --bucket "$DATA_DIR" \
  --trash-days 36500 \
  "$DSN" demo

# mount
juicefs mount -d --no-bgjob "$DSN" "$MNT"

# 确认
ls "$MNT"  # 应为空
```

## 4. 观察 JuiceFS 建的表

```bash
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "\dt"
```

**预期输出**(节选,验证 jfs_node/jfs_edge 存在):

```
              List of relations
 Schema |       Name        | Type  | Owner
--------+-------------------+-------+-------
 public | jfs_chunk         | table | pitr
 public | jfs_chunk_ref     | table | pitr
 public | jfs_delfile       | table | pitr
 public | jfs_edge          | table | pitr
 public | jfs_node          | table | pitr
 public | jfs_setting       | table | pitr
 public | jfs_symlink       | table | pitr
 ...
```

## 5. 安装 pitr MVCC schema + 触发器

在**当前 demo 目录**下执行(仓库里 `demo/init.sql` 和 `demo/revert.sql`):

```bash
cat init.sql   | docker exec -i pitr-demo-pg psql -U pitr -d pitr_fs
cat revert.sql | docker exec -i pitr-demo-pg psql -U pitr -d pitr_fs
```

验证触发器已装:

```bash
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "\dS jfs_node"
```

**预期输出**末尾能看到:

```
Triggers:
    tg_pitr_node AFTER INSERT OR DELETE OR UPDATE ON jfs_node ...
```

## 6. 场景 A:单文件的三个版本 → 回滚到 v1

### 6.1 建目录并创建 v1

```bash
mkdir "$MNT/proj"

# 打 v1
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
INSERT INTO pitr_txn (version_hash, scope_path, state, command, message)
VALUES ('v10000000000', '/proj', 'committed', 'create', 'v1: initial')
RETURNING id, version_hash, created_at;"

echo "hello v1" > "$MNT/proj/a.txt"
sync
```

### 6.2 修改到 v2

```bash
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
INSERT INTO pitr_txn (version_hash, scope_path, state, command, message)
VALUES ('v20000000000', '/proj', 'committed', 'edit',   'v2: modify a.txt');"

echo "hello v2 rewritten" > "$MNT/proj/a.txt"
sync
```

### 6.3 修改到 v3

```bash
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
INSERT INTO pitr_txn (version_hash, scope_path, state, command, message)
VALUES ('v30000000000', '/proj', 'committed', 'delete', 'v3: rm a.txt, add b.txt');"

rm "$MNT/proj/a.txt"
echo "brand new" > "$MNT/proj/b.txt"
sync
```

### 6.4 查看当前 FS 状态

```bash
ls -la "$MNT/proj"
# 期望: 只有 b.txt
cat "$MNT/proj/b.txt"
# 期望: brand new
```

### 6.5 查看版本树与 history

```bash
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
SELECT id, version_hash, command, message, created_at FROM pitr_txn ORDER BY id;"

docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
SELECT id, inode, op, recorded_at FROM pitr_node_history ORDER BY id;"

docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
SELECT id, parent, convert_from(name,'UTF8') AS name, op, recorded_at
FROM pitr_edge_history ORDER BY id;"
```

**预期观察**:

- `pitr_txn` 里能看到 root / v1 / v2 / v3 四条。
- `pitr_node_history` 里能看到 mkdir、create、write、delete 触发的 I/U/D 事件,`recorded_at` 递增。
- `pitr_edge_history` 里能看到目录项的增删。

**这一步跑通就已经证明:PG 触发器能捕获 JuiceFS 的元数据变更**。

### 6.6 回滚到 v1

```bash
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "SELECT pitr_revert('v10000000000');"
```

**预期输出**:

```
              pitr_revert
------------------------------------------
 reverted to v10000000000, applied N history rows
```

### 6.7 让 JuiceFS 刷新元数据缓存后验证

JuiceFS 客户端会缓存元数据,必须 remount 才能读到新状态:

```bash
juicefs umount "$MNT"
juicefs mount -d --no-bgjob "$DSN" "$MNT"

ls -la "$MNT/proj"
cat "$MNT/proj/a.txt"
```

**期望输出**:

```
-rw-r--r-- 1 xxx xxx 9 ... a.txt
hello v1
```

**`b.txt` 消失,`a.txt` 内容恢复到 "hello v1"**。至此,从"触发器捕获"到"反向 replay 恢复"的整条链路验证通过。

## 7. 场景 B:目录级快照 & 回滚

在 v1 的基础上再演一次场景 B(验证多目录、跨版本)。

**注意版本标签与编辑的顺序**:`pitr_txn` INSERT 必须**先于**该版本的所有文件编辑——版本 N 的编辑区间是 `[vN.created_at, v(N+1).created_at)`。写在 INSERT 前面的操作会被归到上一版本(或"孤儿区间"),revert 时不会被回退。

```bash
# 打 v4 (先立版本标签)
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
INSERT INTO pitr_txn (version_hash, scope_path, state, command)
VALUES ('v40000000000', '/', 'committed', 'add /other');"

# v4 的编辑
mkdir "$MNT/other"
echo "other file" > "$MNT/other/x.txt"
sync

# 打 v5
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "
INSERT INTO pitr_txn (version_hash, scope_path, state, command)
VALUES ('v50000000000', '/proj', 'committed', 'edit /proj/a.txt');"

# v5 的编辑
echo "proj v5" > "$MNT/proj/a.txt"
sync

# 回到 v4 (proj/a.txt 恢复 hello v1, /other 保留)
docker exec -it pitr-demo-pg psql -U pitr -d pitr_fs -c "SELECT pitr_revert('v40000000000');"

juicefs umount "$MNT" && juicefs mount -d --no-bgjob "$DSN" "$MNT"

cat "$MNT/proj/a.txt"    # 期望: hello v1
cat "$MNT/other/x.txt"   # 期望: other file
```

## 8. 关键机制验证点(逐条 check)

跑完上面步骤后,对照下表确认每个机制都工作:

| 机制 | 如何验证 | 通过标志 |
|---|---|---|
| JuiceFS + PG 元数据引擎可用 | 步骤 3 `juicefs mount` 成功 | `ls $MNT` 无报错 |
| 触发器捕获 INSERT/UPDATE/DELETE | 步骤 6.5 查询 shadow 表 | 4 张 history 表都有对应行 |
| 时间戳可用于版本归属 | 步骤 6.5 观察 `recorded_at` 在 v1 与 v2 之间 | 时间戳落在版本区间 |
| 反向 replay 可恢复 jfs_node | 步骤 6.7 `stat a.txt` size/mtime | 一致于 v1 |
| 反向 replay 可恢复 jfs_edge | 步骤 6.7 `ls proj` | b.txt 消失,a.txt 存在 |
| **反向 replay 可恢复 jfs_chunk** | 步骤 6.7 `cat a.txt` | **内容恢复 "hello v1"**(核心承诺) |
| **反向 replay 可恢复 jfs_chunk_ref** | 步骤 6.7 能读到 v1 blob | 引用计数恢复,`--no-bgjob` + `--trash-days` 保住 blob |
| 目录级回滚不影响其他子树 | 步骤 7 `cat /other/x.txt` | 内容保留 |

## 9. 已知限制与下一步

跑通 demo **不代表**下面几点已经解决,这些是 Go 阶段(P0/P2)要继续验证的:

1. **精确事务归属**(共享连接 or GUC 变量):demo 走时间戳关联,并发写会归属错乱。Go 阶段必须验证"pitrd 与 JuiceFS 客户端能否共享 PG 连接",让触发器直接读 `SET LOCAL pitr.current_txn`。
2. **JuiceFS 内部 compaction / trash 清理污染 history**:demo 里没跑压力测试。生产版触发器需按 GUC 变量过滤。
3. **JuiceFS 客户端元数据缓存**:demo 用 umount/remount 简单粗暴刷新。生产用 pitrd 主动通知客户端 invalidate。
4. **触发器覆盖面**:已接 `jfs_node` / `jfs_edge` / `jfs_chunk` / `jfs_chunk_ref` 四张核心表(内容层 revert 已验证)。**尚未覆盖**:`jfs_symlink`(符号链接 target)、`jfs_xattr`(扩展属性)、`jfs_delfile`(延迟删除队列)。生产版按需补齐。
5. **回滚期间的读操作**:demo `LOCK TABLE` 简单粗暴。生产要评估是否用 SERIALIZABLE + 短事务。

## 10. 清理

一键清理(等价于下方手工步骤):

```bash
./reset.sh
```

或手工:

```bash
juicefs umount "$MNT" 2>/dev/null || true
docker rm -f pitr-demo-pg
rm -rf "$DATA_DIR" "$MNT"
```

---

## 附:一键跑通(可选)

想省事的话,把 §1–§7 的命令串到一个脚本里,大约 60 行 bash。demo 目录里 **不预置**这个脚本——demo 的意义就是**逐步观察每一步的产物**(pg 表变化、history 行、文件系统状态),看到中间过程比看到最终结果更有价值。
