# JuiceFS 通用 Hook 上游可行性

> 调研日期：2026-08-04
> 调研范围：JuiceFS 官方源码、文档、CONTRIBUTING，以及官方 GitHub Issue/PR。本文是
> 《演进推演》的补充，不代表当前产品已经升级 JuiceFS 或完成单层 FUSE。

## 1. 结论

JuiceFS 维护团队对“可消费的文件系统变更流”有明确动力，但对“任意插件/RPC
介入所有文件操作”没有表现出接受意愿。更准确地说：

- **通用变更事件有上游机会**。维护者在 2025 年主动提出过为所有关键文件操作增加
  Hook，以满足权限、配额、统计和审计；虽然该方案最终被关闭为“不计划”，但团队
  随后在 2026 年合入了更收敛的元数据 changelog，并把审计、问题排查、外部消费和
  增量同步列为官方场景。
- **宽泛的同步 Hook 框架机会较低**。此前议题明确包含 plugin/RPC、blocking 和
  non-blocking 两种调用，最终没有实现且被关闭为“不计划”。Issue 没有写关闭理由，
  因而不能把性能或安全当作维护者的原话；但官方 changelog 默认关闭，并明确警告
  额外写入和存储开销，说明关键写路径的成本是上游必须控制的约束。
- **最可行的方向不是重新提交同一个 Hook 提案，而是增强现有 changelog**：提供稳定、
  有版本的结构化事件，明确 `user / compaction / gc / recovery` 等来源，并允许从
  FUSE/VFS 向 Meta 事务携带有界的 operation context。
- **即使通用能力合入，pitr-fs 仍需要很薄的定制层**。上游能力可以消除大量追踪补丁，
  但版本 pin、旧元数据保存、回滚和 slice 生命周期属于 pitr-fs 产品语义，不应要求
  JuiceFS 主干承担。

因此，推动上游是值得做的，但目标应是“让 JuiceFS 成为可扩展、可观察的变更源”，
而不是“让 JuiceFS 主干内置 pitr-fs”。

## 2. 一手证据

### 2.1 维护者确实提出过通用 Hook，但该形态被关闭

JuiceFS 维护者 `jiefenghuang` 创建的
[`support hook for all file operation` #6184](https://github.com/juicedata/juicefs/issues/6184)
要求在 create/read/write 等关键操作上增加 Hook，通过插件或 RPC 调用用户自定义服务，
同时支持 blocking 和 non-blocking；列出的动机包括自定义权限、配额、统计和审计。
该 Issue 被列入 Release 1.4，后来以 **Closed as not planned** 关闭，没有关联实现 PR，
也没有公开写明关闭理由。

这条证据同时说明两件事：团队认可问题的通用性，但不能据此认为“任意插件 + RPC +
同步阻塞”会被接受。原样重提 #6184 的成功率低。

### 2.2 上游已经选择元数据 changelog 作为通用扩展面

2026 年 4 月，JuiceFS 合入
[`meta: changelog of meta operations` #6777](https://github.com/juicedata/juicefs/pull/6777)。
该改动由 Davies Liu 提交，覆盖 Redis、SQL 和 TKV 等多类元数据引擎，经过维护者评审，
35 项检查通过后合入主干。官方文档明确将 changelog 用于：

- 操作审计；
- 问题排查；
- 跟随元数据变化的外部消费程序；
- 文件系统之间的增量同步。

来源：JuiceFS
[`Metadata Changelog` 官方文档](https://github.com/juicedata/juicefs/blob/v1.4.1/docs/en/administration/changelog.md#L7-L16)
和[增量同步流程](https://github.com/juicedata/juicefs/blob/v1.4.1/docs/en/administration/changelog.md#L54-L72)。

这不是内存回调，而是持久化在元数据引擎中的有序记录。消费者保存已经处理的游标，
重启后从游标恢复；TKV 允许回退窗口并要求消费者去重。官方因此已经给出了 CDC/审计
类需求的首选扩展边界。

这个能力也在持续维护：PostgreSQL 用户报告 changelog 文本列过短会使事务回滚后，
上游通过关联的 #7242 修复并关闭了
[#7239](https://github.com/juicedata/juicefs/issues/7239)，而不是撤回该能力。这进一步说明
维护团队对“可靠变更流”本身有投入意愿，但仍在收敛 beta 阶段的边界和实现质量。

### 2.3 changelog 与 Meta 修改处于同一后端事务

SQL Meta 的 `genLog` 接收当前 `xorm.Session`，并通过该 session 插入 changelog；
WRITE 在更新 chunk、slice ref 和 node 后、事务函数返回前调用它：

- [`genLog` 与 `ScanChangelog`](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/sql.go#L1078-L1118)
- [`WRITE` 事务中的事件写入](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/sql.go#L3385-L3401)

这比进程外 webhook 更接近可靠变更源：主元数据事务不提交，就不会得到可消费的成功
变更。其输出包含游标、时间、操作参数、client session ID 和 transaction ID，见
[官方格式定义](https://github.com/juicedata/juicefs/blob/v1.4.1/docs/en/administration/changelog.md#L74-L94)。

### 2.4 用户写与 Compaction 已经可以明确区分

JuiceFS v1.4.1 分别写入 `WRITE(...)` 和 `COMPACTCHUNK(...)`：

- [`WRITE`](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/sql.go#L3391-L3401)
- [`COMPACTCHUNK`](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/sql.go#L3888-L3902)

Compaction 使用 `Background()` context，而用户写使用请求 context。这说明“后台整理和
用户变更需要分开表达”已经符合上游现有模型，不再需要靠 slice 数量变化反推原因。
不过当前文本协议只有操作名，没有统一的 `cause` 枚举；把来源做成稳定字段仍有上游
增强价值。

### 2.5 现有 OnMsg 不是通用 mutation Hook

`meta.Meta` 已有 `OnMsg`，但它是内部控制消息回调。公开的消息常量包括
`DeleteSlice`、`CompactChunk`、`Rmr`、`Clone` 等；回调签名还是
`func(...interface{}) error`，没有统一的前后状态、事务游标或兼容版本：

- [内部消息类型](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/interface.go#L35-L60)
- [`MsgCallback` 和 `OnMsg`](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/interface.go#L147-L152)
- [`Meta` 接口中的 `OnMsg`](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/interface.go#L520-L529)

它适合 JuiceFS 客户端内部执行对象删除和 Compaction，不适合作为外部版本控制的稳定
插件协议。

### 2.6 VFS accesslog 已覆盖操作观察，但允许丢事件

VFS 的 `logit` 已记录 method、结果、耗时和 uid/gid/pid；事件写入读者缓冲区时使用
非阻塞 `select`，缓冲满会直接走 `default`，因此它适合 profiling 和临时观察，不是
持久、完整、事务一致的审计/版本源：

- [`logit` 实现](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/vfs/accesslog.go#L66-L101)

这也解释了 #6184 为什么认为 accesslog 无法满足更细粒度和实时的需求，而最终落地的
方案是在 Meta 层提供可恢复 changelog。

### 2.7 上游要求重大功能先达成设计共识

JuiceFS 的
[`CONTRIBUTING.md`](https://github.com/juicedata/juicefs/blob/main/CONTRIBUTING.md#L3-L8)
要求开发功能前先搜索或联系社区，用 Issue 讨论并达成一致；重大更新强烈建议先写设计
文档。直接提交跨 VFS/Meta 的大 PR，不先讨论 API 和性能预算，不符合项目协作路径。

## 3. 现有 changelog 对 pitr-fs 够不够

不够，但它显著缩小了需要自行维护的 JuiceFS 补丁。

| pitr-fs 需要的语义 | JuiceFS v1.4.1 changelog | 结论 |
|---|---|---|
| 区分用户写和 Compaction | 有 `WRITE` / `COMPACTCHUNK` | 基本满足 |
| 事务内有序事件 | 有 session/txn/游标，事件写在 Meta 事务中 | 基本满足 |
| 外部消费者断点续读 | 有 `ScanChangelog` 和保留策略 | 满足观察/复制 |
| FUSE 请求的 uid/gid/pid、fd 生命周期、原始意图 | accesslog 有部分身份；changelog 不统一携带 | 不满足 |
| 修改前 node/edge/chunk/chunk_ref 状态 | 主要记录操作及参数，不是 before image | 不满足 |
| 在旧 slice 失去原生引用前原子 pin | 外部消费发生在提交后 | 不满足 |
| 一个 open/write/flush/release 窗口归为同一 pitr 版本 | Meta txn ID 不等于 POSIX 写窗口 | 不满足 |
| 回滚、版本裁剪和 slice 生命周期 | 官方明确 changelog 不是备份且不含文件数据 | 不满足 |

官方也明确说明 changelog 不能单独恢复文件、旧记录被清理后无法补回，并会增加元数据
写入，见[限制说明](https://github.com/juicedata/juicefs/blob/v1.4.1/docs/en/administration/changelog.md#L96-L101)。

另外，pitr-fs 当前固定的 JuiceFS v1.3.0 没有 `ScanChangelog` 或 `juicefs changelog`
命令；该能力从 v1.4.0 起提供。评估单层 FUSE 时，应把“升级到固定的 v1.4.1 基线”
作为一次独立 ABI/迁移决策，不能把主干代码直接套在当前 v1.3.0 上。

## 4. 哪些 API 形态更可能合入

以下概率是基于已合入代码、被关闭 Issue 和官方性能说明的工程判断，不是维护者承诺。

### 4.1 较高：增强 changelog 的稳定结构和来源标记

建议把首个上游提案限制为：

```text
MutationEvent {
  schema_version
  cursor
  session_id
  txn_id
  operation
  cause          // user | compaction | gc | recovery | admin
  inode/chunk/slice 等现有操作参数
  bounded_context // 可选、受长度和字段白名单约束
}
```

兼容上保留现有文本输出，同时在内部使用有版本的 typed event，并允许消费者选择稳定
编码。理由完全来自 JuiceFS 官方已有场景，不需要提 pitr-fs 特例：审计、增量同步、
安全分析、备份索引和故障诊断都会受益。

这一方向的优势是沿用 #6777 已确认的 Meta 事务边界、游标、备份基线和多后端实现，
不会引入任意外部代码进入关键路径。

### 4.2 中等：有界 operation context 从 VFS 传到 Meta/changelog

JuiceFS 的 Meta `Context` 已经携带 pid/uid/gids，并支持 `WithValue`：

- [`Context` 定义](https://github.com/juicedata/juicefs/blob/v1.4.1/pkg/meta/context.go#L26-L42)

可以提议增加官方定义的、不可变且有严格大小上限的 `operation_id`、`source` 或
`trace_id`，由 FUSE/VFS 创建并传到 Meta 事务和 changelog。它能关联一次用户意图
展开出的多个 Meta 操作，也能服务分布式 tracing，不直接改变 POSIX 结果。

风险在于身份伪造、敏感数据和存储膨胀，因此不能开放任意 map 或任意字符串落库；
需要字段白名单、长度上限和默认关闭。

### 4.3 中低：只读、进程内、不可阻塞的 OperationObserver

如果 changelog 无法覆盖 VFS 的 open/flush/release 生命周期，可以在现有 `logit`
旁边提出 typed observer，但范围应严格限制：

- 只观察已完成操作；
- 不能修改参数、不能 veto POSIX 操作；
- 不允许网络 I/O；
- 队列有界，慢消费者可丢观察事件但必须暴露 dropped 指标；
- 默认关闭，不改变未启用时的热路径分配；
- 先服务 tracing/profiling，再由定制构建把 operation context 与持久 changelog 关联。

这种 API 比 #6184 小，但仍和 accesslog/changelog 有重叠，因此必须用基准证明它解决了
二者无法解决的问题。

### 4.4 较低：事务内同步 before/after callback

pitr-fs 最想要的能力是：Meta 事务修改前读取旧状态、持有 slice、和原修改原子提交。
但若把它设计成通用 callback，会要求所有元数据引擎定义回调事务权限、重试语义、
幂等性和失败传播；消费者稍慢就会直接放大文件系统延迟。

更现实的边界是：上游维护稳定事件和 operation context；pitr-fs 针对 PostgreSQL 的
before image、pin 和 undo 仍由自己的 schema/trigger 或小补丁完成。

## 5. 哪些形态大概率不应提交

- **Go `.so` 或任意动态插件系统**：带来 Go 版本/ABI、跨平台、崩溃隔离和供应链问题，
  而 #6184 的 plugin 方向已经被关闭。
- **写路径中的同步 RPC/webhook**：外部服务抖动会变成文件系统延迟或不可用；阻塞、
  超时、重试后能否重复提交都会污染 JuiceFS 的一致性边界。
- **允许 Hook 修改参数或拒绝任意文件操作**：这会让同一 JuiceFS 版本因插件产生不同
  POSIX 语义，也会把权限、配额和锁的正确性转移到不可控代码。
- **直接暴露 SQL 表、chunk 编码或内部 Go struct 作为稳定 ABI**：会锁死多元数据后端
  和未来升级。
- **把 pitr 的 version/pin/revert/refcount 表直接交给上游**：它只服务一种产品语义，
  会让 JuiceFS 为外部版本系统承担 GC 和兼容责任。
- **给每次 read/write 强制同步生成外部事件**：高频数据路径开销无法按需关闭，也与
  官方“changelog 默认关闭、需评估元数据写放大”的取向冲突。

## 6. 推荐推进方式

### 第一步：不要先写 PR，先提交设计 Issue

按照 CONTRIBUTING，先在 JuiceFS 社区提出设计讨论，并明确引用 #6184 和 #6777：

1. 承认“all operations plugin/RPC”已经被关闭，不重复该设计。
2. 基于现有 changelog，展示两个以上非 pitr 场景：结构化审计消费者、增量同步、
   tracing 关联、用户写与后台 Compaction 诊断。
3. 给出关闭状态下的零/近零性能增量，以及开启后的 p50/p99、WAL/元数据写放大。
4. 先询问维护者愿意接受的是 changelog v2、operation context，还是只接受内部重构。

### 第二步：分成可独立合入的小改动

推荐顺序：

1. 给 changelog operation/source 定义稳定枚举和版本号，不增加插件。
2. 补齐跨 Redis/SQL/TKV 的一致性测试和兼容读取测试。
3. 增加有界 `operation_id/source` 传递，并记录到 changelog。
4. 用现有 `WRITE` 与 `COMPACTCHUNK` 做真实消费原型和性能报告。
5. 只有维护者确认 VFS 生命周期仍需要扩展时，再讨论只读 observer。

### 第三步：pitr-fs 保留薄定制层

单层 FUSE 的合理终态不是“完全不构建定制 JuiceFS”，而是：

```text
JuiceFS 主干能力
  = 稳定 MutationEvent + cause + operation context + cursor

pitr-fs 定制层
  = 写窗口 + before image + version/squash/revert + slice pin/refcount/GC
```

如果通用能力上游合入，pitr-fs 仍需链接或构建一个包含自身实现的客户端，但与 JuiceFS
的长期差异可以从“修改所有写路径和 Meta 细节”缩小为“注册一个薄适配器 + PG 专属
版本层”。这才是上游 Hook 对维护成本的主要价值。

## 7. 最终判断

维护团队**有动力接受通用的、低侵入的变更能力**，而且 v1.4.x changelog 已经是最强
证据；但没有证据表明他们愿意接受任意 blocking plugin/RPC，现有 #6184 反而给出了
负面信号。

因此上游成功率取决于提案措辞和边界：

- 提“给 pitr-fs 一个 Hook”——成功率低。
- 提“让现有 changelog 成为稳定、可区分变更来源、可关联 VFS 意图的通用事件流”——
  成功率明显更高。
- 提“允许外部服务同步参与事务并改变操作结果”——不建议投入。

在得到维护者对设计 Issue 的正面反馈前，pitr-fs 不应把单层 FUSE 路线押在上游合入
上；应先以固定版本的小补丁验证正确性和收益，再把真正通用的最小部分逐项上游化。
