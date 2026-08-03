# Workspace 与外部存储接入

## 1. 当前已实现语义

`workspace` 是版本、策略和恢复操作的归属边界，`mount` 只是访问入口。

- 一个 `pitrd` 可管理多个 workspace。
- 每个 workspace 维护独立版本线、`history-limit`、`logs`、`diff`、`revert`、
  `squash`、`clear` 和后台裁剪队列。
- 同一 workspace 只创建一个隐藏 pitr FUSE proxy；多个用户挂载点通过 bind mount
  指向它，因此共享文件视图和写窗口。
- 非 default workspace 的数据位于 JuiceFS 内部保留目录；default workspace
  无法列出、lookup 或修改该目录。
- 旧单挂载安装升级后无损迁移到 `default` workspace，旧绝对 scope 会转换为
  workspace 内以 `/` 开始的相对路径。

当前仍只有一个共享 JuiceFS 卷。空间计数、`max-space`、`space-reserve`、slice
引用和对象 GC 都是卷级语义，不宣称 workspace 独立物理配额。JuiceFS 元数据触发器
也只能可靠识别一个开放版本窗口，因此跨 workspace 同时写会返回 `EBUSY`；这是
防止错误归属的安全限制，不是最终并发模型。

## 2. 为什么 PostgreSQL 只能解决一部分分布式问题

中心化 PostgreSQL 能直接提供 workspace catalog、全局唯一 revision、事务提交、
租约表和持久化队列，是早期分布式控制面的合理权威源。但它不能自动解决：

- FUSE 客户端失联后旧 writer 的 fencing。
- 已上传对象与元数据事务之间的失败恢复。
- 客户端缓存失效、发布通知丢失和断线后对账。
- 多控制器同时 mount、revert、prune 或 GC 的所有权。
- 跨区域延迟、数据库热点、故障转移和凭证轮换。

因此可以先保持“一个 workspace 的元数据事务落在一个 PostgreSQL 主库”，不需要
立即引入分布式事务；但多节点前必须增加 lease/fencing、幂等 operation ID、
revision 对账和维护任务所有权。

## 3. 部署形态

### 3.1 Standalone（当前默认）

安装器管理 pitr-fs、固定版本 JuiceFS、PostgreSQL 和本地或远端对象后端。用户只需
执行安装脚本和 `pitr init`。这是单机开发、测试和小规模使用的默认形态。

### 3.2 External storage（下一阶段）

`pitrd` 仍单实例，但 PostgreSQL 和对象存储由用户或平台提供。安装器不安装、不升级、
不重启外部服务，只校验连接、权限、版本和必需能力。

建议通过 root-only 配置文件接入，而不是把密钥持久化在 Shell 历史中：

```yaml
deployment: external
metadata:
  driver: postgres
  dsn_file: /etc/pitr-fs/secrets/postgres-dsn
object_storage:
  type: s3
  endpoint: https://s3.example.com
  bucket: pitr-production
  access_key_file: /etc/pitr-fs/secrets/s3-access-key
  secret_key_file: /etc/pitr-fs/secrets/s3-secret-key
```

接入校验至少包括：

1. PostgreSQL 主版本和 JuiceFS schema/ABI 是否在支持矩阵中。
2. 账号是否只有所需数据库/对象前缀权限，TLS 和证书是否有效。
3. bucket 是否已经绑定其他不兼容 JuiceFS 卷；format 必须继续显式执行且幂等。
4. 数据库备份与对象数据的恢复点是否能对齐。
5. 网络断开时默认 fail closed，不能绕过版本捕获直接写对象或元数据。

彻底卸载只删除 pitr-fs 自己创建的 schema、账号或对象前缀，而且必须有安装状态账本
证明所有权；用户预先存在的 PostgreSQL、bucket、凭证和系统服务永远不自动删除。

### 3.3 Distributed clients（后续）

多个 pitr/JuiceFS 客户端共享外部 PostgreSQL 和对象存储。控制面需要把以下状态从
进程内存提升为数据库中的可恢复状态机：

- `workspace_id + session_id + operation_id` 的写入归属。
- 带 epoch 的 writer lease 和 fencing token。
- mount/controller 心跳、维护模式和所有权转移。
- revision 订阅游标、缓存失效与断线补偿。
- prune/GC 队列的抢占、续租、幂等重试和死信状态。

早期不支持跨 workspace 原子 rename/publish，也不支持一个 workspace 跨多个元数据
分片。这样可把强一致边界限制在单个 PostgreSQL 事务内，成本明显低于一开始引入
两阶段提交。

## 4. 接入接口建议

未来安装和运行时配置分为三层：

1. `install.sh` 只负责宿主依赖、镜像、systemd 和配置文件模板。
2. `pitr storage validate` 只读检查 PostgreSQL/对象存储，不执行 format。
3. `pitr storage init` 在明确确认后创建 pitr 自有 schema、JuiceFS 卷和所有权账本。

配置需要区分普通参数和秘密：普通参数可写入 `/etc/pitr-fs/config.yaml`；DSN、密钥
和 token 只接受权限为 `0600` 的文件、systemd credentials 或平台 secret provider。
状态命令只展示脱敏 endpoint、数据库名、bucket 和校验结果。

升级采用 expand/contract schema 迁移。数据库内 `pitr_schema_state` 是权威版本；
当新 schema 的 `min_logic_revision` 高于旧逻辑时，不允许自动回退旧二进制。外部服务
版本不由 `pitr upgrade` 改动，兼容性不满足时应在停服务前失败。

## 5. 建议实施顺序

1. 保持当前 all-in-one 安装为默认，稳定单机多 workspace 与多挂载。
2. 增加 external PostgreSQL + external S3/MinIO 的配置契约和只读预检。
3. 在全新 Linux、已有 PostgreSQL、已有对象存储三种环境验证干净安装/卸载。
4. 把版本窗口归属从“全卷唯一开放窗口”改为显式 operation/workspace context，解除
   跨 workspace 写串行限制。
5. 再加入多控制器 lease/fencing、通知对账和分布式维护队列。
6. 最后根据真实规模评估 PostgreSQL 分片和跨区域部署，不提前引入跨分片事务。
