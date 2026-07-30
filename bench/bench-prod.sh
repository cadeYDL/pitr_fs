#!/usr/bin/env bash
# Phase 6 生产基准:同一生产容器、同一 JuiceFS 卷内对比底层 JuiceFS 与
# 上层 PITR FUSE,避免设备和 PostgreSQL 差异污染结果。
set -euo pipefail

CONTAINER="${PITR_CONTAINER:-pitrfs}"
RESULTS="${PITR_PROD_RESULTS:-/tmp/pitr-prod-bench}"
META_COUNT="${PITR_BENCH_META_COUNT:-2000}"
IO_MIB="${PITR_BENCH_IO_MIB:-256}"
SCOPE="/workspace/__pitr_prod_bench"
PLAIN="/var/lib/pitr/jfs/__pitr_prod_bench"
PITR="$SCOPE"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "$META_COUNT:$IO_MIB" in
    *[!0-9:]*|0:*|*:0) echo "PITR_BENCH_META_COUNT/PITR_BENCH_IO_MIB 必须是正整数" >&2; exit 2 ;;
esac
docker inspect "$CONTAINER" >/dev/null 2>&1 ||
    { echo "生产容器 $CONTAINER 不存在" >&2; exit 1; }
docker exec "$CONTAINER" sh -c \
    'test -S /var/run/pitrd.sock && mountpoint -q /workspace' ||
    { echo "pitrd 或生产 FUSE 未就绪" >&2; exit 1; }

mkdir -p "$RESULTS"
RAW="$RESULTS/prod.csv"
ENV_FILE="$RESULTS/environment.txt"
echo "metric,plain,pitr,unit,regression_pct,threshold_pct,passed" >"$RAW"

docker exec "$CONTAINER" rm -rf "$PLAIN"
docker exec "$CONTAINER" mkdir -p "$PLAIN"
cleanup() {
    docker exec "$CONTAINER" pitr rollback "$SCOPE" >/dev/null 2>&1 || true
    docker exec "$CONTAINER" rm -rf "$PLAIN" >/dev/null 2>&1 || true
}
trap cleanup EXIT

elapsed_ns() {
    docker exec "$CONTAINER" sh -c '
set -e
start=$(date +%s%N)
"$@" >/dev/null
end=$(date +%s%N)
echo $((end-start))
' timer "$@"
}

metadata_ns() {
    docker exec "$CONTAINER" sh -c '
root=$1
count=$2
rm -rf "$root"
mkdir -p "$root"
start=$(date +%s%N)
i=1
while [ "$i" -le "$count" ]; do
  : >"$root/f_$i"
  i=$((i+1))
done
end=$(date +%s%N)
echo $((end-start))
' metadata "$1" "$2"
}

dd_write_ns() {
    docker exec "$CONTAINER" sh -c '
file=$1
count=$2
rm -f "$file"
start=$(date +%s%N)
dd if=/dev/zero of="$file" bs=1M count="$count" conv=fsync status=none
end=$(date +%s%N)
echo $((end-start))
' write "$1" "$2"
}

dd_read_ns() {
    docker exec "$CONTAINER" sh -c '
file=$1
start=$(date +%s%N)
dd if="$file" of=/dev/null bs=1M status=none
end=$(date +%s%N)
echo $((end-start))
' read "$1"
}

ratio_loss() {
    awk -v plain="$1" -v pitr="$2" 'BEGIN {
      if (plain == 0) { print 0 } else { printf "%.2f", (plain-pitr)/plain*100 }
    }'
}

ratio_latency() {
    awk -v plain="$1" -v pitr="$2" 'BEGIN {
      if (plain == 0) { print 0 } else { printf "%.2f", (pitr-plain)/plain*100 }
    }'
}

pass_le() {
    awk -v value="$1" -v limit="$2" 'BEGIN {
      if (value <= limit) print "true"; else print "false"
    }'
}

echo "==> 元数据 create: $META_COUNT files"
plain_meta_ns=$(metadata_ns "$PLAIN/plain-meta" "$META_COUNT")
docker exec "$CONTAINER" rm -rf "$PITR/pitr-meta"
docker exec "$CONTAINER" mkdir -p "$PITR/pitr-meta"
docker exec "$CONTAINER" pitr begin "$PITR/pitr-meta" -m "prod bench metadata" >/dev/null
pitr_meta_ns=$(metadata_ns "$PITR/pitr-meta" "$META_COUNT")
docker exec "$CONTAINER" pitr commit "$PITR/pitr-meta" -m "prod bench metadata" >/dev/null
plain_meta=$(awk -v ns="$plain_meta_ns" -v n="$META_COUNT" 'BEGIN {printf "%.4f",ns/n/1000000}')
pitr_meta=$(awk -v ns="$pitr_meta_ns" -v n="$META_COUNT" 'BEGIN {printf "%.4f",ns/n/1000000}')
meta_reg=$(ratio_latency "$plain_meta" "$pitr_meta")
echo "metadata_create_ms_op,$plain_meta,$pitr_meta,ms/op,$meta_reg,30,$(pass_le "$meta_reg" 30)" >>"$RAW"

echo "==> 顺序写: ${IO_MIB} MiB"
plain_write_ns=$(dd_write_ns "$PLAIN/plain-io.bin" "$IO_MIB")
docker exec "$CONTAINER" pitr begin "$PITR" -m "prod bench write" >/dev/null
pitr_write_ns=$(dd_write_ns "$PITR/pitr-io.bin" "$IO_MIB")
docker exec "$CONTAINER" pitr commit "$PITR" -m "prod bench write" >/dev/null
plain_write=$(awk -v mib="$IO_MIB" -v ns="$plain_write_ns" 'BEGIN {printf "%.2f",mib/(ns/1000000000)}')
pitr_write=$(awk -v mib="$IO_MIB" -v ns="$pitr_write_ns" 'BEGIN {printf "%.2f",mib/(ns/1000000000)}')
write_reg=$(ratio_loss "$plain_write" "$pitr_write")
echo "sequential_write_mib_s,$plain_write,$pitr_write,MiB/s,$write_reg,10,$(pass_le "$write_reg" 10)" >>"$RAW"

echo "==> 顺序读: ${IO_MIB} MiB"
plain_read_ns=$(dd_read_ns "$PLAIN/plain-io.bin")
pitr_read_ns=$(dd_read_ns "$PITR/pitr-io.bin")
plain_read=$(awk -v mib="$IO_MIB" -v ns="$plain_read_ns" 'BEGIN {printf "%.2f",mib/(ns/1000000000)}')
pitr_read=$(awk -v mib="$IO_MIB" -v ns="$pitr_read_ns" 'BEGIN {printf "%.2f",mib/(ns/1000000000)}')
read_reg=$(ratio_loss "$plain_read" "$pitr_read")
echo "sequential_read_mib_s,$plain_read,$pitr_read,MiB/s,$read_reg,10,$(pass_le "$read_reg" 10)" >>"$RAW"

echo "==> 1 GiB sparse file revert"
docker exec "$CONTAINER" rm -rf "$PITR/revert"
docker exec "$CONTAINER" mkdir -p "$PITR/revert"
baseline_output=$(docker exec "$CONTAINER" pitr begin "$PITR/revert" -m "1GiB baseline")
baseline_hash=$(printf '%s\n' "$baseline_output" | awk '{print $3}')
docker exec "$CONTAINER" truncate -s 1G "$PITR/revert/big.bin"
docker exec "$CONTAINER" pitr commit "$PITR/revert" -m "1GiB baseline" >/dev/null
docker exec "$CONTAINER" pitr begin "$PITR/revert" -m "1GiB mutate" >/dev/null
docker exec "$CONTAINER" sh -c \
    'printf changed | dd of="$1" bs=1 conv=notrunc status=none' write "$PITR/revert/big.bin"
docker exec "$CONTAINER" pitr commit "$PITR/revert" -m "1GiB mutate" >/dev/null
revert_ns=$(elapsed_ns pitr revert "$baseline_hash" --path "$PITR/revert")
revert_ms=$(awk -v ns="$revert_ns" 'BEGIN {printf "%.2f",ns/1000000}')
size=$(docker exec "$CONTAINER" stat -c %s "$PITR/revert/big.bin")
[ "$size" = "1073741824" ] || { echo "revert 后大文件尺寸错误: $size" >&2; exit 1; }
prefix=$(docker exec "$CONTAINER" sh -c \
    'od -An -tu1 -N7 "$1" | tr -d " \n"' read "$PITR/revert/big.bin")
[ "$prefix" = "0000000" ] || { echo "revert 后大文件内容错误: $prefix" >&2; exit 1; }
echo "revert_1gib_ms,0,$revert_ms,ms,$revert_ms,500,$(pass_le "$revert_ms" 500)" >>"$RAW"

docker exec "$CONTAINER" psql -U pitr -d pitr_fs -Atc "
SELECT 'history_rows=' ||
 ((SELECT count(*) FROM pitr_node_history) +
  (SELECT count(*) FROM pitr_edge_history) +
  (SELECT count(*) FROM pitr_chunk_history) +
  (SELECT count(*) FROM pitr_chunk_ref_history));
SELECT 'history_bytes=' ||
 (pg_total_relation_size('pitr_node_history') +
  pg_total_relation_size('pitr_edge_history') +
  pg_total_relation_size('pitr_chunk_history') +
  pg_total_relation_size('pitr_chunk_ref_history'));" >"$RESULTS/space.txt"

{
    echo "date=$(date -Iseconds)"
    echo "kernel=$(uname -sr)"
    echo "container=$CONTAINER"
    echo "image=$(docker inspect -f '{{.Config.Image}}' "$CONTAINER")"
    echo "meta_count=$META_COUNT"
    echo "io_mib=$IO_MIB"
} >"$ENV_FILE"

python3 "$SCRIPT_DIR/prod_report.py" \
    "$RAW" "$ENV_FILE" "$RESULTS/space.txt" "$SCRIPT_DIR/PROD.md"
echo "==> 原始数据: $RAW"
echo "==> 生产报告: $SCRIPT_DIR/PROD.md"
