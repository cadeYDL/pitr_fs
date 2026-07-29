# P0 可行性验证报告 —— Task 0.3: PG 连接共享

**状态**: DONE — PG 层机制 4/4 PASS;JuiceFS 1.4.0 无法跨进程复用
pitrd 的 PG 事务,最终选择“单 daemon 开放 auto 窗口”退化方案
**关联文档**: 设计文档 §5.3、执行计划 Task 0.3

---

## 1. 问题陈述

Task 0.3 要回答一个问题:

> **能否让 pitrd 与 JuiceFS 客户端复用同一个 PG 连接,让 trigger 里的 `current_setting('pitr.current_txn')` 读到 pitrd 事前 `SET LOCAL` 的值?**

如果可以 → 走**生产版本方案**:精确归属,revert 精准。
如果不可以 → 退化到**时间戳方案**:归属靠时间窗关联,并发下有精度损失(见 §4)。

这是设计文档 §5.3 明确标注的**头号风险**,直接决定 `internal/pg` 和 `internal/txn` 的实现路径。

---

## 2. 验证分两半

### 2.1 PG 层机制验证 (脚本自动化)

`bench/verify-conn-share.sh` 起隔离 PG、装生产版触发器,跑 4 个断言:

| 场景 | 目的 | 预期 |
| --- | --- | --- |
| A. 同连接 + SET LOCAL + UPDATE | 核心正路 | 触发器 `txn_id = 42` |
| B. 跨连接: X 里 SET LOCAL commit,Y 里 UPDATE | 头号风险量化 | 触发器 `txn_id = NULL` |
| C. SET SESSION 泄漏到后续事务 | 连接池陷阱 | 归属方案必须 LOCAL 不能 SESSION |
| D. 跨连接 FK 引用未 commit `pitr_txn` | MVCC 可见性 | FK 违反 → 报错 |

**跑法**:

```bash
cd bench
./verify-conn-share.sh
```

**结论**(2026-07-29 跑通,4/4 PASS):

- [x] 场景 A: **PASS** — 同连接 + SET LOCAL + UPDATE,触发器读到 `txn_id=42`
- [x] 场景 B: **PASS** — 跨连接,触发器读到 `txn_id=NULL`(GUC 定量丢失,证实必须共享连接)
- [x] 场景 C: **PASS** — SET SESSION 泄漏,inode=4 被错误归属为 99(证实归属必须用 LOCAL 不能 SESSION)
- [x] 场景 D: **PASS** — 跨连接 FK 引用未 commit 的 pitr_txn 被拒(证实归属和主写必须在同一事务)

结论:**生产版方案在 PG 层是自洽的**,剩下只看 JuiceFS SDK 能否复用连接(见 §2.2)。

### 2.2 JuiceFS SDK 复用连接调研

JuiceFS 元数据 SQL 后端的核心入口在 `pkg/meta/sql.go`,底层是 `xorm.Engine`。需要回答:

1. **能否传入外部 `*sql.DB`?** 目前公开 API 只接受 DSN 字符串(`meta.NewClient(dsn, ...)`),内部自建 `xorm.Engine`。
2. **能否传入 `*sql.Conn` 或类似 pin-connection 语义?** xorm 有 `Session` 可以在同一连接上跑,但 `NewClient` 的入口封死了。
3. **fork 成本**?

**调研指令**:

```bash
# 定位 SQL 客户端构造
git clone https://github.com/juicedata/juicefs /tmp/juicefs-src
cd /tmp/juicefs-src && grep -rn "NewClient\|xorm.NewEngine\|sql.Open" pkg/meta/sql.go

# 看 Meta 接口是否暴露复用点
grep -rn "type Meta interface" pkg/meta/interface.go
grep -rn "meta.RegisterMeta\|EngineArgs" pkg/meta/
```

**调研环境**:

- 运行环境:OrbStack `calw`
- JuiceFS:`1.4.0+2026-07-06.62bedf3`
- 对应源码提交:`62bedf3c`

**调研结论**:

- 官方 `meta.NewClient(uri, conf)` 只接受 DSN,不接受外部 `*sql.DB`、
  `*sql.Conn` 或 `pgx.Conn`。
- SQL meta 在 `newSQLMeta` 内部调用 `xorm.NewEngine`,连接池由 JuiceFS
  自己创建。虽然包内有未导出的 `engineCreator`,它也只能替换 engine 创建,
  不能把 pitrd 已开启的 PG transaction 传给一次元数据操作。
- `dbMeta.txn` 每次调用 `xorm.Engine.Transaction`,自行从 JuiceFS 连接池获取连接
  并开启事务。
- 当前部署拓扑中 `juicefs mount` 是独立进程,pitrd 的 FUSE loopback 通过
  POSIX syscall 进入该进程。即使给 JuiceFS 增加 engine 注入 API,也无法把
  pitrd 进程中的 pgx transaction 跨进程传递给某一次 syscall。
- 因此严格方案不是“少于 100 行的 fork”;它需要给 JuiceFS 增加跨进程事务/
  请求上下文协议并贯穿 FUSE 与 meta 层,超出一期范围。

### 2.3 综合判定

| 条件 | 走哪条路径 |
| --- | --- |
| SDK 有官方复用连接 API | ✅ 生产方案 |
| SDK 无 API 但 fork ≤ 100 行 | ✅ fork + 生产方案 |
| SDK 无 API 且 fork 成本高 | ❌ 走时间戳退化 |

**最终判定**:❌ 不可共享,采用退化方案。

---

## 3. 若走生产方案,pitrd 需保证

- 每个 versionedHook 请求**独占一条 pgx.Conn**(不是 `pgxpool.Pool.Acquire` 后随意释放),整个事务里所有操作都跑在这条 Conn 上
- `SET LOCAL pitr.current_txn = ...` 必须放在 `BEGIN` 之后、任何 JuiceFS 元数据操作之前
- JuiceFS 客户端把这条 Conn 直接拿去用,**不能**再走它内部的 sql.DB 连接池分配
- Conn 上的 GUC 用 LOCAL 不用 SESSION(场景 C 已定量证明),事务结束自然清理

对应实现约束落在:
- Task 2.2 `internal/pg.InTx`: 必须签名接受 `func(context.Context, *pgx.Conn) error` 而非 `*pgxpool.Pool`
- Task 3.2 `versionedHook`: 打点事务和 JuiceFS 元数据操作绑到同一个 Conn

---

## 4. 若走退化方案,妥协的地方

设计文档 §5.3 已给出思路:"打点在独立事务里,记录 begin/end 时间戳,一致性稍弱但可用"。

**具体退化**:

- Phase 3 的 proxy 在一次 JuiceFS 写操作前,先提交一条
  `state='auto', closed_at IS NULL` 的开放版本。
- JuiceFS 独立连接上的 trigger 在 GUC 为空时,选择唯一开放的 auto,
  直接把其 id 写入 history 的 `txn_id`。
- 写操作结束后 proxy 设置 `closed_at`;失败时反向补偿并删除 auto。
- daemon 用互斥锁保证同一时刻至多一个开放窗口。相比事后用时间戳关联,
  该方案仍保留 FK 和明确 txn_id,但本质上仍是独立事务窗口。

**精度损失**:

- **并发写重叠**: version_A `[t1, t3]`、version_B `[t2, t4]`,重叠区 `[t2, t3]` 里的 history 行按启发式(fd/path 前缀)归属 → 存在归错的可能
- **补偿而非 rollback**:pitrd 事务失败要主动 replay/删除开放窗口里的
  history,不能靠 PG 自动 rollback。
- **性能持平**: 触发器少一次 `current_setting` 调用,反而略快

**这个损失可接受吗?**

- 单 daemon 场景:开放窗口不重叠,history 归属是确定的。
- 多 daemon / 高并发: 归错的概率见 §4 补测(暂未做,等 SDK 结论后视情况)

---

## 5. 已知偏差记录

- 本报告的 PG 层验证不覆盖 pgbouncer 事务级 pooling 的复用行为;若生产部署会经过 pgbouncer,需要额外验证 `SET LOCAL` 与 pooling 的兼容性(简单结论:pgbouncer transaction mode 会破坏所有 session-level GUC 依赖,pitrd 若走生产方案必须直连 PG 或使用 session-mode pooling)
- 本报告不评估 JuiceFS 内部 compaction / cleanup 线程的连接归属;这些操作在 GUC 为空时应被忽略,触发器已用 `NULLIF(current_setting(..., true), '')` 兼容
