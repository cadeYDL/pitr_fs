#!/usr/bin/env bash
# 维度 4:版本切换开销
# 关键假设:revert 时间应与 history 行数相关,与文件内容大小无关
# 测四种 case:
#   A: 单个 1GB 大文件被覆盖 10 次    → history 行少,文件大 → 应快
#   B: 10000 个小文件全部改一次       → history 行多       → 应中等
#   C: 100000 次 create+delete 循环   → history 行极多     → 变慢
#   D: 目录级 revert(仅 /proj)vs 全局 → 前者应显著更快
set -euo pipefail
source "${BENCH_ROOT:-/tmp/pitr-bench}/env.sh"

MNT="$MNT_PITR"
DB="pitr"
OUT="$BENCH_ROOT/results/revert"
mkdir -p "$OUT"

psql_time_ms() {
    # 运行一段 SQL,返回耗时 (ms)
    local sql="$1"
    docker exec "$PG_CONTAINER" psql -U pitr -d "$DB" -qtAX \
        -c "\timing on" -c "$sql" 2>&1 \
        | grep -oP 'Time: \K[0-9.]+' | head -1
}

record_version() {
    local hash="$1" cmd="$2"
    docker exec "$PG_CONTAINER" psql -U pitr -d "$DB" -qtAX -c "
INSERT INTO pitr_txn (version_hash, scope_path, state, command)
VALUES ('$hash', '/', 'committed', '$cmd');" >/dev/null
}

history_rows() {
    docker exec "$PG_CONTAINER" psql -U pitr -d "$DB" -qtAX -c "
SELECT (SELECT count(*) FROM pitr_node_history)
     + (SELECT count(*) FROM pitr_edge_history);"
}

remount() {
    juicefs umount "$MNT" 2>/dev/null || true
    sleep 1
    juicefs mount -d --no-bgjob "$DSN_PITR" "$MNT" >/dev/null
}

reset_hist() {
    docker exec "$PG_CONTAINER" psql -U pitr -d "$DB" -qtAX -c "
TRUNCATE pitr_node_history, pitr_edge_history RESTART IDENTITY;
DELETE FROM pitr_txn WHERE state != 'root';" >/dev/null
}

RESULTS="$OUT/summary.csv"
echo "case,hist_rows_before_revert,revert_ms" > "$RESULTS"

# --------- CASE A ---------
echo "==> Case A: 1GB × 10 overwrites"
reset_hist
mkdir -p "$MNT/case_a"
big="$MNT/case_a/big.bin"
dd if=/dev/urandom of="$big" bs=1M count=1024 status=none 2>&1 | tail -1
sync; sleep 2
record_version "caseA0000v001" "case A: initial 1GB"

for i in $(seq 2 10); do
    dd if=/dev/urandom of="$big" bs=1M count=1024 status=none conv=notrunc 2>/dev/null
done
sync; sleep 2

rows=$(history_rows)
t=$(psql_time_ms "SELECT pitr_revert('caseA0000v001');")
remount
sz=$(stat -c%s "$big" 2>/dev/null || stat -f%z "$big")
echo "  hist=$rows, revert=$t ms, final file size=$sz"
echo "A_1GB_x10,$rows,$t" >> "$RESULTS"

# --------- CASE B ---------
echo "==> Case B: 10000 files × 1 edit"
reset_hist
rm -rf "$MNT/case_b"; mkdir -p "$MNT/case_b"
for i in $(seq 1 10000); do echo "v1" > "$MNT/case_b/f_$i"; done
sync; sleep 3
record_version "caseB0000v001" "case B: 10000 files"

for i in $(seq 1 10000); do echo "v2" > "$MNT/case_b/f_$i"; done
sync; sleep 3

rows=$(history_rows)
t=$(psql_time_ms "SELECT pitr_revert('caseB0000v001');")
remount
sample=$(cat "$MNT/case_b/f_1" | tr -d '\n')
echo "  hist=$rows, revert=$t ms, sample content='$sample' (expect: v1)"
echo "B_10k_files,$rows,$t" >> "$RESULTS"

# --------- CASE C ---------
echo "==> Case C: 100000 create+delete churn"
reset_hist
rm -rf "$MNT/case_c"; mkdir -p "$MNT/case_c"
record_version "caseC0000v001" "case C: empty"

for i in $(seq 1 100000); do
    : > "$MNT/case_c/f_$i"; rm -f "$MNT/case_c/f_$i"
    if (( i % 10000 == 0 )); then echo "    churn $i / 100000"; fi
done
sync; sleep 3

rows=$(history_rows)
t=$(psql_time_ms "SELECT pitr_revert('caseC0000v001');")
remount
count=$(ls "$MNT/case_c" | wc -l)
echo "  hist=$rows, revert=$t ms, final files=$count (expect: 0)"
echo "C_churn_100k,$rows,$t" >> "$RESULTS"

echo
echo "===== 汇总: $RESULTS ====="
cat "$RESULTS" | column -t -s,
python3 "$(dirname "$0")/summarize.py" revert "$RESULTS"
