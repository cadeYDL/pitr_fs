# pitr-fs 性能测试方案

**目标**:在开始 Go 编码前,用同一套纯命令环境(与 `demo/` 共用触发器 + revert SQL)量化四个维度,给出"这套设计能承载什么规模"的定量答案。

## 四个维度

| 维度 | 想回答的问题 | 对应脚本 | 关键指标 |
|---|---|---|---:|
| 1. 规模承载 | 元数据规模到多大会开始退化? | `bench-scale.py`   | create/stat ms/op 随 inode 数变化曲线 |
| 2. 性能损耗 | pitr 相对 JuiceFS、JuiceFS 相对裸盘各慢多少? | `bench-throughput.sh` | 顺序/随机 读/写 的 BW / IOPS / p99 延迟 |
| 3. 体积膨胀 | 由于 blob 只增不减 + history 表,空间开销是多少? | `bench-space.sh`   | 数据目录字节数、`pitr_*` 表大小、jfs 表行数随写入递增 |
| 4. 版本切换 | 对大文件/多小文件/高频 churn 的 revert 各要多久? | `bench-revert.sh`  | revert 耗时 vs history 行数 vs 文件大小 |

**核心假设**(需要被数据验证):
- pitr 触发器给**数据 IO** 增加的开销应该 **<10%**(触发器只在元数据表触发,数据 IO 走 JuiceFS 原路径)。
- pitr 触发器给**元数据操作** 增加的开销预期 **30%–100%**(每次 UPDATE/DELETE 额外做一次 INSERT 到 history)。
- revert 耗时应**与文件内容大小解耦**、只与 history 行数相关(设计的核心承诺)。
- 100 万 inode 内,元数据操作延迟应保持 <10 ms/op。

数据跑出来若显著偏离,说明设计需要调整——这是 P0 之前的**头号止损点**。

## 三挡对比

| 挡位 | 挂载点 | 描述 | 意义 |
|---|---|---|---|
| native | `$MNT_NATIVE` | 宿主机本地目录 | 天花板 |
| plain  | `$MNT_PLAIN`  | JuiceFS + PG 元数据,**无** pitr 触发器 | JuiceFS 本身开销 |
| pitr   | `$MNT_PITR`   | JuiceFS + PG + **pitr 触发器** | 完整开销 |

两个 JuiceFS 卷跑在**同一个 PG 实例的两个 database** 上,天然隔离触发器。

## 前置条件

- Linux(macOS 有 macFUSE 依赖 + Docker Desktop VM 层,数字可能失真)
- `docker` / `juicefs` / `psql` / `fio` / `python3` / `dd` / `du`
- 建议物理机或独占 VM,避免其他负载干扰

## 全流程

`setup.sh` 会自动清理上一次残留(挂载/容器/data 目录),tmpfs 环境下 `BENCH_ROOT` 自动切到 `$HOME/pitr-bench`,以下用 `$BENCH_ROOT` 引用真实路径。

```bash
cd bench

# 1. 起环境(3 挡)
./setup.sh
source "$BENCH_ROOT/env.sh"       # 路径以 setup.sh 结束时提示为准

# 2. 维度 1:规模承载
python3 bench-scale.py \
    --mnts native="$MNT_NATIVE" plain="$MNT_PLAIN" pitr="$MNT_PITR" \
    --out "$BENCH_ROOT/results/scale.json"
python3 summarize.py scale "$BENCH_ROOT/results/scale.json"

# 3. 维度 2:性能损耗
./bench-throughput.sh
# 已内嵌 summarize 调用,输出表 + 判定

# 4. 维度 3:体积膨胀
./bench-space.sh
# 输出 $BENCH_ROOT/results/space/{rewrite,churn}.csv + 判定

# 5. 维度 4:版本切换
./bench-revert.sh
# 输出 $BENCH_ROOT/results/revert/summary.csv + 判定

# 6. 综合结论(读上面四份产物,产 verdict.md)
python3 summarize.py verdict "$BENCH_ROOT/results"

# 7. 清理
./teardown.sh            # 保留数据方便复查
./teardown.sh --purge    # 彻底清
```

## 如何判读

四个 bench 每步跑完都会自动打印 `[PASS] / [WARN] / [FAIL]` 判定,阈值来自本文档"核心假设"章节;最后 `summarize.py verdict` 会把四个维度合成一份 `verdict.md`,给出总体结论:

- `[PASS]` = 所有关键假设成立,可直接进入 Go 编码 P0
- `[WARN]` = 有指标偏离预期但未越红线,可继续,需在最终报告中记录
- `[FAIL]` = 至少一项核心假设被推翻,回设计文档改机制(高概率:history 表加分区/触发器改异步/加 GC)

判定的具体阈值:

### 维度 1:规模承载 (`bench-scale.py`)

| 指标 | PASS | WARN | FAIL |
| --- | --- | --- | --- |
| pitr/plain create @ 100k inode | <2x | 2–5x | >5x |
| pitr create @ 100k inode | <5 ms/op | 5–10 ms/op | >10 ms/op |
| create 斜率(100k / 1k) | <2x | 2–5x | >5x |

- **plain vs native** 反映 JuiceFS 本身开销(通常 20–50x,因为走网络协议栈到 PG),不参与判定。
- 斜率越大越说明 PG 索引扩展性有问题,需要加索引/分区。

### 维度 2:性能损耗 (`bench-throughput.sh`)

| 指标 | PASS | WARN | FAIL |
| --- | --- | --- | --- |
| 顺序 pitr/plain 带宽损失 | <5% | 5–30% | >30% |
| 随机写 pitr/plain 带宽损失 | <30% | 30–50% | >50% |
| 各 job p99 pitr/plain | <2x | 2–3x | >3x |

顺序 IO 若 pitr 明显慢,说明打点位置放错了(数据 IO 不应经触发器)。p99 长尾若到秒级,考虑加 `fillfactor` 和 partition。

### 维度 3:体积膨胀 (`bench-space.sh`)

| 指标 | PASS | WARN | FAIL |
| --- | --- | --- | --- |
| pitr rewrite data_bytes 末/首 | >1.5x | 1–1.5x | ≤1x(未增长) |
| plain rewrite data_bytes 漂移 | <50% | 50–200% | >200% |
| churn pitr_hist_rows / ops | ~3 (<5) | 5–10 | >10 |
| 平均 pitr_bytes / op | 200–2000 B | 2000–5000 B | >5000 B |

pitr 侧未增长说明 `--trash-days 36500` 没生效;plain 侧漂移大说明 JuiceFS 的 slice compaction 出问题。**换算生产成本**:每天 100 万次操作,按 200–1000 B/op 计,每年 pitr 表新增约 70–300 GB,若不可接受 GC 策略要提前设计。

### 维度 4:版本切换 (`bench-revert.sh`)

| 指标 | PASS | WARN | FAIL |
| --- | --- | --- | --- |
| Case A revert(hist ~30 行) | <500 ms | 500–2000 ms | >2000 ms |
| A vs B 时间比 | <0.5(A 明显更快) | 0.5–1 | ≥1(A 反而更慢) |
| B→C 非线性系数 | <1.5(接近线性) | 1.5–3 | >3 |

- Case A / B 的对比是**核心承诺**:文件大小差 1000× 而 A 更快,证明 revert 与文件大小解耦。
- 非线性系数 = (time_ratio / hist_ratio),1 是完美线性;>1.5 说明反向 replay SQL 需要优化(index、批量、并行)。
- 最终 `expect: v1` / `expect: 0` 校验必须通过,否则任何耗时都无意义(校验目前脚本内 echo,后续可补进判定)。

## 如何形成结论

跑完四个维度,填以下表格(bench 之后手工整理成一份 md 报告):

```markdown
## pitr-fs 可行性结论(vX.Y)

- 数据 IO 损耗:__ %(基于顺序读写 BW)
- 元数据操作损耗:__ %(基于 create ms/op @ 100k inode)
- 100 万 inode 时元数据延迟:__ ms/op
- 每 100 万次写操作产生 pitr 元数据:__ MB
- Revert 1000 行 history 耗时:__ ms
- Revert 1GB 文件(3 行 history):__ ms

结论:□ 满足设计目标  □ 需调整方案
```

结论若 ✅ → 进入 Go 编码 P0;若 ❌ → 回设计文档改机制(高概率改动:history 表加分区、触发器改异步、加 GC)。

## 已知偏差

- **触发器归属**:demo/bench 都是"无条件记录 + 时间戳关联",而生产版会用 `pitr.current_txn` GUC 精确归属。理论上生产版会**略慢**(每次操作前多一次 `SET LOCAL`),差异约 <5%,可在 Go P2 阶段复测。
- **FUSE 拦截层**:本 bench **不包含 FUSE 开销**——一层用户态 loopback 会再增加 30%–100% 元数据延迟,数据吞吐通常 <10% 损失。
- **gRPC 控制面**:本 bench 不包含 CLI/SDK → daemon 的通信开销(每次事务操作多一次 unix socket 往返,~0.1 ms 量级,可忽略)。

Go 阶段完成后需重跑一次完整 bench(P6),那次的报告作为生产版性能承诺。

## Phase 6 生产版复跑

生产版不再使用上面的 demo 触发器挂载。先用 `install.sh` 启动真实
`pitrd + JuiceFS + FUSE proxy`,再运行:

```bash
PITR_CONTAINER=pitrfs ./bench/bench-prod.sh
```

脚本在同一容器和同一 JuiceFS 卷中比较底层挂载与用户可见挂载,验证元数据
延迟、顺序读写、history 空间以及 1 GiB sparse 文件 revert。原始结果默认写入
`/tmp/pitr-prod-bench`,并确定性生成 `bench/PROD.md`。可通过
`PITR_BENCH_META_COUNT`、`PITR_BENCH_IO_MIB` 调整样本规模。
