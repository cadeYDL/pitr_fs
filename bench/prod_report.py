#!/usr/bin/env python3
"""把 Phase 6 生产基准原始数据生成可审计 Markdown 报告。"""

import csv
import sys
from pathlib import Path


def read_kv(path: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for line in path.read_text().splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            result[key] = value
    return result


def main() -> None:
    if len(sys.argv) != 5:
        raise SystemExit("用法: prod_report.py DATA.csv ENV.txt SPACE.txt OUT.md")
    data_path, env_path, space_path, output_path = map(Path, sys.argv[1:])
    with data_path.open(newline="") as source:
        rows = list(csv.DictReader(source))
    environment = read_kv(env_path)
    space = read_kv(space_path)
    failures = [row for row in rows if row["passed"] != "true"]

    lines = [
        "# Phase 6 生产版基准报告",
        "",
        "本报告由 `bench/bench-prod.sh` 在生产 daemon、JuiceFS 和双层 FUSE "
        "真实挂载上生成。plain 与 pitr 使用同一容器、同一 PostgreSQL 和同一 "
        "JuiceFS 卷；plain 直接访问底层挂载，pitr 访问用户可见的版本化挂载。",
        "",
        "## 环境",
        "",
        f"- 时间：{environment.get('date', '-')}",
        f"- 内核：{environment.get('kernel', '-')}",
        f"- 镜像：{environment.get('image', '-')}",
        f"- 独立轮数：{environment.get('rounds', '1')}（结果取中位数）",
        f"- 元数据样本：{environment.get('meta_count', '-')} 个文件",
        f"- 顺序 I/O 样本：{environment.get('io_mib', '-')} MiB",
        "",
        "## 结果",
        "",
        "| 指标 | plain | pitr | 退化/耗时 | 阈值 | 判定 |",
        "|---|---:|---:|---:|---:|:---:|",
    ]
    for row in rows:
        verdict = "PASS" if row["passed"] == "true" else "FAIL"
        if row["metric"] == "revert_1gib_ms":
            comparison = f"{row['pitr']} ms"
            threshold = f"≤ {row['threshold_pct']} ms"
        else:
            comparison = f"{row['regression_pct']}%"
            threshold = f"≤ {row['threshold_pct']}%"
        lines.append(
            f"| {row['metric']} | {row['plain']} {row['unit']} | "
            f"{row['pitr']} {row['unit']} | {comparison} | "
            f"{threshold} | {verdict} |"
        )

    lines += [
        "",
        "## 空间快照",
        "",
        f"- history 行数：{space.get('history_rows', '-')}",
        f"- history 表总大小：{space.get('history_bytes', '-')} bytes",
        "",
        "## 结论与优化路径",
        "",
    ]
    if not failures:
        lines.append("全部生产阈值通过。")
    else:
        lines.append(
            "以下指标未达到目标；功能正确性不受影响，但不能把这些目标作为当前版本"
            "的性能承诺："
        )
        lines.append("")
        for row in failures:
            if row["metric"] == "revert_1gib_ms":
                lines.append(
                    f"- `{row['metric']}`：实际 {row['pitr']} ms，"
                    f"目标 ≤ {row['threshold_pct']} ms。"
                )
            else:
                lines.append(
                    f"- `{row['metric']}`：实际 {row['regression_pct']}%，"
                    f"目标 ≤ {row['threshold_pct']}%。"
                )
        lines += [
            "",
            "建议按以下顺序优化并用同一脚本复测：",
            "",
            "1. 元数据：把连续操作的 auto window 合并为显式批次，减少每个 FUSE "
            "调用的 PostgreSQL 往返；保留失败补偿边界。",
            "2. 写吞吐：确认内核 `CAP_PASSTHROUGH`/stacking depth 实际启用，"
            "用 FUSE profile 定位剩余用户态拷贝；保留 fd auto 与 direct-I/O "
            "一致性边界。",
            "3. 读吞吐：用 FUSE/JuiceFS profile 区分缓存失效和用户态拷贝成本，"
            "只针对读路径恢复可证明安全的缓存。",
            "4. revert：对 scope 闭包和 history 回放执行 `EXPLAIN ANALYZE`，按结果"
            "补组合索引或批量回放。",
        ]
    lines += [
        "",
        "原始 CSV、环境和空间快照位于运行时的 `PITR_PROD_RESULTS` 目录。",
        "",
    ]
    output_path.write_text("\n".join(lines))


if __name__ == "__main__":
    main()
