#!/usr/bin/env python3
"""聚合多轮 prod.csv，以中位数生成最终可审计数据。"""

import csv
import statistics
import sys
from pathlib import Path


def main() -> None:
    if len(sys.argv) < 4:
        raise SystemExit("用法: prod_aggregate.py OUT.csv RUN1.csv RUN2.csv [RUN3.csv ...]")
    output = Path(sys.argv[1])
    runs: list[dict[str, dict[str, str]]] = []
    for source_name in sys.argv[2:]:
        with Path(source_name).open(newline="") as source:
            rows = list(csv.DictReader(source))
        runs.append({row["metric"]: row for row in rows})

    metrics = list(runs[0])
    for run in runs[1:]:
        if list(run) != metrics:
            raise SystemExit("各轮 metric 集合或顺序不一致")

    fields = [
        "metric",
        "plain",
        "pitr",
        "unit",
        "regression_pct",
        "threshold_pct",
        "passed",
    ]
    with output.open("w", newline="") as target:
        writer = csv.DictWriter(target, fieldnames=fields, lineterminator="\n")
        writer.writeheader()
        for metric in metrics:
            sample = runs[0][metric]
            plain = statistics.median(float(run[metric]["plain"]) for run in runs)
            pitr = statistics.median(float(run[metric]["pitr"]) for run in runs)
            threshold = float(sample["threshold_pct"])
            if metric == "revert_1gib_ms":
                regression = pitr
            elif metric == "metadata_create_ms_op":
                regression = 0.0 if plain == 0 else (pitr - plain) / plain * 100
            else:
                regression = 0.0 if plain == 0 else (plain - pitr) / plain * 100
            writer.writerow(
                {
                    "metric": metric,
                    "plain": f"{plain:.4f}" if "metadata" in metric else f"{plain:.2f}",
                    "pitr": f"{pitr:.4f}" if "metadata" in metric else f"{pitr:.2f}",
                    "unit": sample["unit"],
                    "regression_pct": f"{regression:.2f}",
                    "threshold_pct": f"{threshold:g}",
                    "passed": str(regression <= threshold).lower(),
                }
            )


if __name__ == "__main__":
    main()
