#!/usr/bin/env bash
set -euo pipefail
BENCH_ROOT="${BENCH_ROOT:-/tmp/pitr-bench}"
# 与 setup.sh 保持一致:tmpfs 时自动切到 $HOME(否则找不到真实数据/挂载)
if [[ "$BENCH_ROOT" == "/tmp/pitr-bench" ]] \
   && [[ "$(df -T /tmp 2>/dev/null | awk 'NR==2{print $2}')" == "tmpfs" ]]; then
    BENCH_ROOT="$HOME/pitr-bench"
fi

for m in "$BENCH_ROOT/mnt-plain" "$BENCH_ROOT/mnt-pitr"; do
    mountpoint -q "$m" 2>/dev/null && { juicefs umount "$m" 2>/dev/null || umount -f "$m" 2>/dev/null || true; }
done
docker rm -f pitr-bench-pg >/dev/null 2>&1 || true

if [ "${1:-}" = "--purge" ]; then
    rm -rf "$BENCH_ROOT"
    echo "  数据已清理: $BENCH_ROOT"
else
    echo "  容器和挂载已清理,数据保留在 $BENCH_ROOT"
    echo "  彻底清理: $0 --purge"
fi
