#!/usr/bin/env bash
# 维度 2:性能损耗 —— 三挡对比 fio 顺序/随机 读/写
set -euo pipefail
source "${BENCH_ROOT:-/tmp/pitr-bench}/env.sh"

FIO_JOB="$(dirname "$0")/bench-throughput.fio"
OUT="$BENCH_ROOT/results/throughput"
mkdir -p "$OUT"

for name in native plain pitr; do
    var="MNT_${name^^}"
    mnt="${!var}"
    echo "==> fio on $name ($mnt)"
    DIR="$mnt" fio --output-format=json --output="$OUT/$name.json" "$FIO_JOB" >/dev/null
done

python3 "$(dirname "$0")/summarize.py" throughput "$OUT"
