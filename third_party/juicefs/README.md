# JuiceFS 固定运行时

pitr-fs 当前固定使用 JuiceFS `v1.3.0`，上游 commit 为
`30190ca1094d26e85f19a979ca51b0ea19af1eaa`。

该版本是 JuiceFS 官方 LTS 版本。pitr-fs 必须访问 JuiceFS 的 SQL 元数据内部
结构，因此不会在镜像构建时下载 `latest`，也不允许在未经兼容验证的情况下
替换 JuiceFS 二进制。

## 本地补丁

`patches/0001-mark-postgres-compaction.patch` 在 PostgreSQL 的 chunk Compaction
事务中设置事务级 `pitr.internal_op=compact`。pitr-fs 的元数据触发器据此跳过
物理 Compaction，但仍持续维护当前数据和历史版本的 slice 引用计数。

补丁只增加一个事务内标记，不改变 JuiceFS 元数据表结构、slice 编码或对象
命名。镜像中的版本输出带有 `pitrfs.1-30190ca` 标记，pitrd 启动时会强制校验。

## 未来升级规则

升级 JuiceFS 前必须：

1. 新增独立的 `juicefsabi/vN` 兼容层，不得静默修改现有 ABI。
2. 审核依赖的表字段、唯一键、MetaVersion、24 字节 slice 编码和 12 字节
   delayed-slice 编码。
3. 重新移植并测试 Compaction 标记补丁。
4. 完成元数据导出、恢复、随机写、Compaction 和对象 GC 回归测试。
5. 更新固定 commit、构建标记和镜像摘要后才能启用新运行时。
