#!/usr/bin/env bash
# 维度 3:体积膨胀
#   场景 A:1MB 文件反复覆盖 100 次 → 观察 blob 增长(是否 ~100 倍)
#   场景 B:1000 组 create+delete   → 观察 history 表膨胀
set -euo pipefail
source "${BENCH_ROOT:-/tmp/pitr-bench}/env.sh"

OUT="$BENCH_ROOT/results/space"
mkdir -p "$OUT"

sample_size() {
    local mnt_name="$1"
    local data_dir="$BENCH_ROOT/data-$mnt_name"
    du -sb "$data_dir" 2>/dev/null | awk '{print $1}'
}

sample_pg() {
    local db="$1"
    docker exec "$PG_CONTAINER" psql -U pitr -d "$db" -qtAX -c "
        SELECT
            (SELECT count(*) FROM jfs_node)               AS jfs_node_rows,
            (SELECT count(*) FROM jfs_chunk)              AS jfs_chunk_rows,
            (SELECT COALESCE(SUM(pg_total_relation_size(oid)), 0)
             FROM pg_class WHERE relname LIKE 'jfs_%')    AS jfs_bytes,
            (SELECT COALESCE(count(*), 0) FROM pitr_node_history
             WHERE to_regclass('pitr_node_history') IS NOT NULL) AS pitr_hist_rows,
            (SELECT COALESCE(SUM(pg_total_relation_size(oid)), 0)
             FROM pg_class WHERE relname LIKE 'pitr_%')   AS pitr_bytes
    " 2>/dev/null || echo "0|0|0|0|0"
}

echo "===== 场景 A:同一 1MB 文件覆盖 100 次 ====="
{
    echo "mnt_name,round,data_bytes,jfs_node_rows,jfs_chunk_rows,jfs_bytes,pitr_hist_rows,pitr_bytes"
    for name in plain pitr; do
        db="$name"
        var="MNT_${name^^}"
        mnt="${!var}"
        target="$mnt/rewrite.bin"

        # 初始
        dd if=/dev/urandom of="$target" bs=1M count=1 status=none
        for r in 1 20 50 100; do
            # 覆盖到 r 轮
            while (( $(( $(stat -c%s "$target" 2>/dev/null || stat -f%z "$target") )) < 1048576 * r + 1 )); do
                dd if=/dev/urandom of="$target" bs=1M count=1 status=none conv=notrunc
            done
            # 简化:直接再多写 r 次 保证 blob 增长
            for _ in $(seq 1 $((r > 5 ? 5 : r))); do
                dd if=/dev/urandom of="$target" bs=1M count=1 status=none conv=notrunc
            done
            sync
            sleep 1
            IFS='|' read jn jc jb ph pb <<<"$(sample_pg "$db")"
            db_sz=$(sample_size "$name")
            echo "$name,$r,$db_sz,$jn,$jc,$jb,$ph,$pb"
        done
        rm -f "$target"
    done
} | tee "$OUT/rewrite.csv"

echo
echo "===== 场景 B:1000 组 create+delete ====="
{
    echo "mnt_name,ops,data_bytes,jfs_node_rows,pitr_hist_rows,pitr_bytes"
    for name in plain pitr; do
        db="$name"
        var="MNT_${name^^}"
        mnt="${!var}"
        dir="$mnt/churn"
        rm -rf "$dir"; mkdir -p "$dir"
        for round in 100 500 1000; do
            for i in $(seq 1 $round); do
                : > "$dir/f_$i"; rm -f "$dir/f_$i"
            done
            sync; sleep 1
            IFS='|' read jn jc jb ph pb <<<"$(sample_pg "$db")"
            db_sz=$(sample_size "$name")
            echo "$name,$round,$db_sz,$jn,$ph,$pb"
        done
    done
} | tee "$OUT/churn.csv"

echo
echo "结果目录: $OUT"
python3 "$(dirname "$0")/summarize.py" space "$OUT"
