#!/usr/bin/env python3
"""
汇总 bench 结果为 markdown 表 + 自动判定。

用法:
  python3 summarize.py scale      <json>        # 维度 1
  python3 summarize.py throughput <dir>         # 维度 2
  python3 summarize.py space      <dir>         # 维度 3
  python3 summarize.py revert     <csv>         # 维度 4
  python3 summarize.py verdict    <results_dir> # 四维综合报告 → verdict.md

判定级别: PASS / WARN / FAIL。阈值出自 README 的核心假设。
"""
import csv, json, sys
from pathlib import Path

# ============ 判定引擎 ============

PASS, WARN, FAIL = "PASS", "WARN", "FAIL"
BADGE = {PASS: "[PASS]", WARN: "[WARN]", FAIL: "[FAIL]"}

def worst(*levels):
    order = {PASS: 0, WARN: 1, FAIL: 2}
    return max(levels, key=lambda x: order[x])

def check(value, warn_at, fail_at, ge=True):
    """ge=True: 越大越坏; ge=False: 越大越好, 取倒过来的阈值"""
    if ge:
        if value >= fail_at: return FAIL
        if value >= warn_at: return WARN
        return PASS
    else:
        if value <= fail_at: return FAIL
        if value <= warn_at: return WARN
        return PASS

# ============ 维度 1: scale ============

def _scale_metrics(rows):
    """rows = [[tag, n, create_ms, stat_ms], ...] → {tag: {n: {create, stat}}}"""
    out = {}
    for tag, n, c, s in rows:
        out.setdefault(tag, {})[n] = {"create": c, "stat": s}
    return out

def _scale_verdicts(m):
    """README 假设:
       - pitr vs plain create @ 100k: <2x 正常, >5x 失控
       - pitr create @ 100k: <10 ms/op
       - 斜率 create@100k / create@1k: <2x 正常, >5x 说明 PG 索引扩展性有问题
    """
    v = []
    N = 100000
    if "pitr" in m and "plain" in m and N in m["pitr"] and N in m["plain"]:
        ratio = m["pitr"][N]["create"] / max(m["plain"][N]["create"], 1e-9)
        v.append((check(ratio, 2, 5), f"pitr/plain create ratio @ {N:,} inode: {ratio:.2f}x (期望 <2x, 红线 >5x)"))

    if "pitr" in m and N in m["pitr"]:
        lat = m["pitr"][N]["create"]
        v.append((check(lat, 5, 10), f"pitr create latency @ {N:,} inode: {lat:.3f} ms/op (期望 <10)"))

    for tag in ("plain", "pitr"):
        if tag in m and 1000 in m[tag] and N in m[tag]:
            slope = m[tag][N]["create"] / max(m[tag][1000]["create"], 1e-9)
            v.append((check(slope, 2, 5), f"{tag} create 斜率 (n={N:,} / n=1,000): {slope:.2f}x (期望 <2x)"))
    return v

def cmd_scale(json_path):
    rows = json.loads(Path(json_path).read_text())
    print("| mnt | inode 总数 | create ms/op | stat ms/op |")
    print("|---|---:|---:|---:|")
    for tag, n, c, s in rows:
        print(f"| {tag} | {n:,} | {c} | {s} |")

    m = _scale_metrics(rows)
    verdicts = _scale_verdicts(m)
    _print_verdicts("维度 1: 规模承载", verdicts)
    return verdicts

# ============ 维度 2: throughput ============

def _read_fio(path):
    d = json.loads(path.read_text())
    out = {}
    for job in d["jobs"]:
        r, w = job["read"], job["write"]
        active = w if w["io_bytes"] > 0 else r
        out[job["jobname"]] = {
            "iops": active["iops"],
            "bw_MBps": active["bw_bytes"] / 1024 / 1024,
            "lat_ms_p99": active["clat_ns"]["percentile"].get("99.000000", 0) / 1e6,
        }
    return out

def _throughput_verdicts(data):
    """README 假设:
       - 顺序读写 pitr vs plain: <5% 差异
       - 随机 4k 写 pitr vs plain: 慢 <30% 正常, >50% 警戒, >100% 红线
       - p99: pitr/plain <2x 正常, >3x 警戒, >10x 红线
    """
    v = []
    for job in sorted(data["plain"].keys()):
        plain_bw = data["plain"][job]["bw_MBps"]
        pitr_bw  = data["pitr"][job]["bw_MBps"]
        if plain_bw > 0:
            loss = (plain_bw - pitr_bw) / plain_bw
            is_seq = "seq" in job.lower() or "sequential" in job.lower()
            if is_seq:
                v.append((check(loss, 0.05, 0.30),
                          f"[{job}] 顺序 pitr/plain 带宽损失: {loss*100:.1f}% (期望 <5%)"))
            elif "rand" in job.lower() and ("write" in job.lower() or "wr" in job.lower()):
                v.append((check(loss, 0.30, 0.50),
                          f"[{job}] 随机写 pitr/plain 带宽损失: {loss*100:.1f}% (期望 <30%)"))

        plain_p99 = data["plain"][job]["lat_ms_p99"]
        pitr_p99  = data["pitr"][job]["lat_ms_p99"]
        if plain_p99 > 0:
            ratio = pitr_p99 / plain_p99
            v.append((check(ratio, 2, 3),
                      f"[{job}] p99 pitr/plain: {ratio:.2f}x (期望 <2x)"))
    return v

def cmd_throughput(dir_arg):
    d = Path(dir_arg)
    labels = ["native", "plain", "pitr"]
    data = {lbl: _read_fio(d / f"{lbl}.json") for lbl in labels}
    jobs = sorted(data["native"].keys())

    print("| job | metric | native | plain | pitr | plain vs native | pitr vs native |")
    print("|---|---|---:|---:|---:|---:|---:|")
    for j in jobs:
        for m, unit in [("bw_MBps", "MB/s"), ("iops", "IOPS"), ("lat_ms_p99", "p99 ms")]:
            row = []
            for lbl in labels:
                row.append(f"{data[lbl][j][m]:,.2f}")
            n_val = data["native"][j][m]
            def pct(x, n=n_val): return f"{x/n*100:.0f}%" if n > 0 else "-"
            row.append(pct(data["plain"][j][m]))
            row.append(pct(data["pitr"][j][m]))
            print(f"| {j} | {m} ({unit}) | " + " | ".join(row) + " |")

    verdicts = _throughput_verdicts(data)
    _print_verdicts("维度 2: 性能损耗", verdicts)
    return verdicts

# ============ 维度 3: space ============

def _read_csv(path):
    with open(path) as f:
        return list(csv.DictReader(f))

def _space_verdicts(rewrite, churn):
    """README 假设:
       - plain rewrite data_bytes 基本恒定
       - pitr rewrite data_bytes 线性增长 (每轮约 +1 MB)
       - churn pitr_hist_rows / ops ≈ 3 (≤5 正常)
       - 单 op 平均 pitr_bytes: 0.2–1 KB
    """
    v = []

    # rewrite: pitr 应增长, plain 应恒定
    pitr_rw = [r for r in rewrite if r["mnt_name"] == "pitr"]
    plain_rw = [r for r in rewrite if r["mnt_name"] == "plain"]
    if len(pitr_rw) >= 2:
        first, last = int(pitr_rw[0]["data_bytes"]), int(pitr_rw[-1]["data_bytes"])
        growth = last / max(first, 1)
        v.append((check(growth, 0, 1.5, ge=False),
                  f"pitr rewrite data_bytes 增长比 (末/首): {growth:.2f}x (期望 >1.5x, 否则说明 trash 未生效)"))
    if len(plain_rw) >= 2:
        first, last = int(plain_rw[0]["data_bytes"]), int(plain_rw[-1]["data_bytes"])
        if first > 0:
            drift = abs(last - first) / first
            v.append((check(drift, 0.5, 2.0),
                      f"plain rewrite data_bytes 漂移: {drift*100:.1f}% (期望 <50%, JuiceFS 应自动 compact)"))

    # churn: hist rows / ops ≈ 3
    for r in churn:
        if r["mnt_name"] == "pitr":
            ops = int(r["ops"])
            hist = int(r["pitr_hist_rows"])
            if ops > 0:
                per_op = hist / ops
                v.append((check(per_op, 5, 10),
                          f"churn @ {ops} ops: pitr_hist_rows/op = {per_op:.2f} (期望 ~3, 红线 >10)"))

    # 单 op 平均 pitr_bytes
    last_churn = [r for r in churn if r["mnt_name"] == "pitr"]
    if last_churn:
        r = last_churn[-1]
        ops = int(r["ops"])
        pb = int(r["pitr_bytes"])
        if ops > 0:
            per_op_bytes = pb / ops
            v.append((check(per_op_bytes, 2000, 5000),
                      f"平均 pitr_bytes/op: {per_op_bytes:.0f} B (期望 200–1000, 红线 >5000)"))
    return v

def cmd_space(dir_arg):
    d = Path(dir_arg)
    rewrite = _read_csv(d / "rewrite.csv")
    churn = _read_csv(d / "churn.csv")

    print("### 场景 A: rewrite\n")
    print("| mnt | round | data_bytes | jfs_bytes | pitr_hist_rows | pitr_bytes |")
    print("|---|---:|---:|---:|---:|---:|")
    for r in rewrite:
        print(f"| {r['mnt_name']} | {r['round']} | {int(r['data_bytes']):,} | {int(r['jfs_bytes']):,} | {r['pitr_hist_rows']} | {int(r['pitr_bytes']):,} |")

    print("\n### 场景 B: churn\n")
    print("| mnt | ops | data_bytes | pitr_hist_rows | pitr_bytes |")
    print("|---|---:|---:|---:|---:|")
    for r in churn:
        print(f"| {r['mnt_name']} | {r['ops']} | {int(r['data_bytes']):,} | {r['pitr_hist_rows']} | {int(r['pitr_bytes']):,} |")

    verdicts = _space_verdicts(rewrite, churn)
    _print_verdicts("维度 3: 体积膨胀", verdicts)
    return verdicts

# ============ 维度 4: revert ============

def _revert_verdicts(rows):
    """README 假设:
       - revert 与文件内容大小解耦 → CASE A (1GB×10, hist 少) 应比 B/C 显著快
       - B vs C: hist 差 20×, 时间差应接近线性 (≤25×)
       - CASE A revert 应 <500 ms (30 行 history)
    """
    v = []
    by_case = {r["case"]: r for r in rows}

    if "A_1GB_x10" in by_case:
        a_ms = float(by_case["A_1GB_x10"]["revert_ms"])
        v.append((check(a_ms, 500, 2000),
                  f"Case A (1GB × 10, hist ~30 行) revert: {a_ms:.1f} ms (期望 <500ms, 证实与文件大小解耦)"))

    if "A_1GB_x10" in by_case and "B_10k_files" in by_case:
        a = by_case["A_1GB_x10"]; b = by_case["B_10k_files"]
        a_hist, a_ms = float(a["hist_rows_before_revert"]), float(a["revert_ms"])
        b_hist, b_ms = float(b["hist_rows_before_revert"]), float(b["revert_ms"])
        # A 文件大 1000× 但 hist 少 → 应比 B 快
        v.append((check(a_ms / max(b_ms, 1e-9), 0.5, 1.0),
                  f"A vs B: {a_ms:.0f} ms vs {b_ms:.0f} ms — A 文件大 1000× 却更快, 证实与大小解耦"))

    if "B_10k_files" in by_case and "C_churn_100k" in by_case:
        b = by_case["B_10k_files"]; c = by_case["C_churn_100k"]
        b_hist = max(float(b["hist_rows_before_revert"]), 1)
        c_hist = max(float(c["hist_rows_before_revert"]), 1)
        b_ms = float(b["revert_ms"]); c_ms = float(c["revert_ms"])
        hist_ratio = c_hist / b_hist
        time_ratio = c_ms / max(b_ms, 1e-9)
        linearity = time_ratio / hist_ratio  # 1 = 完美线性, >1 = 非线性劣化
        v.append((check(linearity, 1.5, 3.0),
                  f"B→C 线性度: hist ×{hist_ratio:.1f}, time ×{time_ratio:.1f}, 非线性系数 {linearity:.2f} (期望 <1.5)"))
    return v

def cmd_revert(csv_path):
    rows = _read_csv(csv_path)
    print("| case | hist_rows_before_revert | revert_ms |")
    print("|---|---:|---:|")
    for r in rows:
        print(f"| {r['case']} | {r['hist_rows_before_revert']} | {r['revert_ms']} |")

    verdicts = _revert_verdicts(rows)
    _print_verdicts("维度 4: 版本切换", verdicts)
    return verdicts

# ============ 汇总 verdict.md ============

def _print_verdicts(title, verdicts):
    if not verdicts:
        print(f"\n### {title} — 判定\n\n(无可用数据)\n"); return
    levels = [v[0] for v in verdicts]
    overall = worst(*levels) if levels else PASS
    print(f"\n### {title} — 判定 {BADGE[overall]}\n")
    for lvl, msg in verdicts:
        print(f"- {BADGE[lvl]} {msg}")
    print()

def cmd_verdict(results_dir):
    d = Path(results_dir)
    lines = ["# pitr-fs bench 综合结论\n"]
    all_levels = []

    def section(title, fn, arg):
        nonlocal lines
        try:
            import io, contextlib
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                verdicts = fn(arg)
            lines.append(f"## {title}\n")
            lines.append(buf.getvalue())
            if verdicts:
                all_levels.extend(v[0] for v in verdicts)
        except FileNotFoundError as e:
            lines.append(f"## {title}\n\n(缺失数据: {e.filename})\n")

    section("维度 1: 规模承载",  cmd_scale,      d / "scale.json")
    section("维度 2: 性能损耗",  cmd_throughput, d / "throughput")
    section("维度 3: 体积膨胀",  cmd_space,      d / "space")
    section("维度 4: 版本切换",  cmd_revert,     d / "revert" / "summary.csv")

    overall = worst(*all_levels) if all_levels else PASS
    fails = sum(1 for l in all_levels if l == FAIL)
    warns = sum(1 for l in all_levels if l == WARN)
    passes = sum(1 for l in all_levels if l == PASS)

    lines.insert(1, f"""
**总体判定: {BADGE[overall]}** — PASS={passes} / WARN={warns} / FAIL={fails}

- `PASS` = 所有关键假设均成立,可进入 Go 编码 P0
- `WARN` = 部分指标偏离预期但未越红线,可继续但需在报告中记录
- `FAIL` = 至少一项核心假设被推翻,回设计文档改机制(高概率:history 表加分区/触发器改异步/加 GC)
""")

    out = d / "verdict.md"
    out.write_text("\n".join(lines))
    print("".join(lines))
    print(f"\n→ 已写入 {out}")

# ============ 入口 ============

def main():
    if len(sys.argv) < 3:
        print(__doc__); sys.exit(1)
    mode, target = sys.argv[1], sys.argv[2]
    dispatch = {
        "scale": cmd_scale,
        "throughput": cmd_throughput,
        "space": cmd_space,
        "revert": cmd_revert,
        "verdict": cmd_verdict,
    }
    if mode not in dispatch:
        print("unknown mode:", mode); sys.exit(1)
    dispatch[mode](target)

if __name__ == "__main__":
    main()
