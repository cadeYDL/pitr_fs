#!/usr/bin/env bash
# 起 3 挡对比环境:
#   native  = 宿主机本地目录            (性能天花板)
#   plain   = JuiceFS(PG 元数据,无 pitr) (JuiceFS 本身开销)
#   pitr    = JuiceFS + pitr 触发器       (完整开销)
# 用两个独立 PG database 隔离,避免触发器互相污染。
set -euo pipefail

for c in docker juicefs psql fio python3 dd; do
    command -v "$c" >/dev/null || { echo "缺少 $c"; exit 1; }
done

BENCH_ROOT="${BENCH_ROOT:-/tmp/pitr-bench}"
# 若默认落在 tmpfs(内存)上会让 fio 直接测内存带宽,自动切到 $HOME 下的真实磁盘
if [[ "$BENCH_ROOT" == "/tmp/pitr-bench" ]] \
   && [[ "$(df -T /tmp 2>/dev/null | awk 'NR==2{print $2}')" == "tmpfs" ]]; then
    BENCH_ROOT="$HOME/pitr-bench"
    echo "!! /tmp 是 tmpfs,自动切到 $BENCH_ROOT(真实磁盘)"
fi

echo "==> 0/5 清理上一次残留(幂等)"
for m in "$BENCH_ROOT/mnt-plain" "$BENCH_ROOT/mnt-pitr"; do
    mountpoint -q "$m" 2>/dev/null && { juicefs umount "$m" 2>/dev/null || umount -f "$m" 2>/dev/null || true; }
done
docker rm -f pitr-bench-pg >/dev/null 2>&1 || true
rm -rf "$BENCH_ROOT"/{mnt-plain,mnt-pitr,data-plain,data-pitr,results}
mkdir -p "$BENCH_ROOT"/{mnt-native,mnt-plain,mnt-pitr,data-plain,data-pitr,results}

echo "==> 1/5 启动 PG"
docker run -d --name pitr-bench-pg \
    -e POSTGRES_USER=pitr -e POSTGRES_PASSWORD=pitr -e POSTGRES_DB=postgres \
    -p 127.0.0.1:55433:5432 \
    postgres:16 >/dev/null

until docker exec pitr-bench-pg pg_isready -U pitr >/dev/null 2>&1; do sleep 1; done

echo "==> 2/5 建两个 database"
docker exec pitr-bench-pg psql -U pitr -d postgres -c "CREATE DATABASE plain;" >/dev/null
docker exec pitr-bench-pg psql -U pitr -d postgres -c "CREATE DATABASE pitr;"  >/dev/null

DSN_PLAIN="postgres://pitr:pitr@127.0.0.1:55433/plain?sslmode=disable"
DSN_PITR="postgres://pitr:pitr@127.0.0.1:55433/pitr?sslmode=disable"

echo "==> 3/5 Format JuiceFS 两个卷"
juicefs format --storage file --bucket "$BENCH_ROOT/data-plain" \
    --trash-days 36500 "$DSN_PLAIN" plain >/dev/null
juicefs format --storage file --bucket "$BENCH_ROOT/data-pitr"  \
    --trash-days 36500 "$DSN_PITR"  pitr  >/dev/null

echo "==> 4/5 装 pitr 触发器 (仅 pitr database)"
docker exec -i pitr-bench-pg psql -U pitr -d pitr < "$(dirname "$0")/../demo/init.sql"   >/dev/null
docker exec -i pitr-bench-pg psql -U pitr -d pitr < "$(dirname "$0")/../demo/revert.sql" >/dev/null

echo "==> 5/5 挂载"
juicefs mount -d --no-bgjob "$DSN_PLAIN" "$BENCH_ROOT/mnt-plain" >/dev/null
juicefs mount -d --no-bgjob "$DSN_PITR"  "$BENCH_ROOT/mnt-pitr"  >/dev/null

cat > "$BENCH_ROOT/env.sh" <<EOF
export BENCH_ROOT="$BENCH_ROOT"
export MNT_NATIVE="$BENCH_ROOT/mnt-native"
export MNT_PLAIN="$BENCH_ROOT/mnt-plain"
export MNT_PITR="$BENCH_ROOT/mnt-pitr"
export DSN_PLAIN="$DSN_PLAIN"
export DSN_PITR="$DSN_PITR"
export PG_CONTAINER="pitr-bench-pg"
EOF

echo
echo "  ✓ 就绪"
echo "  运行前先: source $BENCH_ROOT/env.sh"
echo "  三挡:"
echo "    native = \$MNT_NATIVE"
echo "    plain  = \$MNT_PLAIN"
echo "    pitr   = \$MNT_PITR"
