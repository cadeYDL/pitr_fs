#!/usr/bin/env bash
# 容器 PID 1 编排:PG → 幂等 apply SQL → juicefs format(首次)→ 再 apply SQL → exec pitrd
# 设计文档 §13.3
set -euo pipefail

log() { echo "[pitr] $*"; }

# 1. 后台拉起 PG (复用官方 entrypoint, 它会处理 initdb + docker-entrypoint-initdb.d)
log "启动 PostgreSQL 后台..."
docker-entrypoint.sh postgres &
PG_PID=$!

# 2. 等 PG 就绪
log "等待 PostgreSQL 就绪..."
until pg_isready -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" >/dev/null 2>&1; do
    sleep 1
done
log "PostgreSQL 就绪 (pid=$PG_PID)"

PG_DSN="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:5432/${POSTGRES_DB}?sslmode=disable"

# 3. 幂等再跑一次建表 (补丁生效, 不依赖 initdb.d 是否执行过)
log "校准 MVCC schema..."
PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
    -v ON_ERROR_STOP=1 -f /etc/pitr/init_pitr.sql >/dev/null

# 4. 格式化 JuiceFS 卷 (仅首次)
if ! juicefs status "$PG_DSN" >/dev/null 2>&1; then
    log "首次格式化 JuiceFS 卷 (storage=${PITR_STORAGE:-file}, volume=$PITR_VOLUME)..."
    juicefs format \
        --storage "${PITR_STORAGE:-file}" \
        --bucket  "${PITR_BUCKET:-$PITR_DATA_DIR}" \
        ${AWS_ACCESS_KEY_ID:+--access-key "$AWS_ACCESS_KEY_ID"} \
        ${AWS_SECRET_ACCESS_KEY:+--secret-key "$AWS_SECRET_ACCESS_KEY"} \
        ${PITR_SESSION_TOKEN:+--session-token "$PITR_SESSION_TOKEN"} \
        --trash-days 0 \
        "$PG_DSN" "$PITR_VOLUME" >/dev/null
    log "juicefs format 完成; 再跑一次 init SQL 装 jfs_* 触发器..."
    PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
        -v ON_ERROR_STOP=1 -f /etc/pitr/init_pitr.sql >/dev/null
else
    log "JuiceFS 卷已存在, 跳过 format (recover 场景)"
fi

# PITR 已在 PostgreSQL 中为保留版本建立 slice pin。关闭 JuiceFS 自身的
# 长期 trash,让被淘汰版本释放 pin 后可由原生 gc 真正回收对象。
log "校准 JuiceFS 对象生命周期策略 (trash-days=0)..."
juicefs config --yes --trash-days 0 "$PG_DSN" >/dev/null

# 5. 建目录
mkdir -p "$(dirname "$PITR_SOCKET")" "$PITR_MOUNT_ROOT" /var/lib/pitr/jfs

# 6. 启动 pitrd。entrypoint 保持为编排进程,确保容器停止时先让 gRPC 优雅退出,
# 再关闭 PostgreSQL,避免下次启动触发 WAL crash recovery。
log "启动 pitrd..."
pitrd \
    --pg-dsn "$PG_DSN" \
    --volume "$PITR_VOLUME" \
    --jfs-mount /var/lib/pitr/jfs \
    --mount-root "$PITR_MOUNT_ROOT" \
    --gc-interval "${PITR_GC_INTERVAL:-10m}" \
    --gc-threads "${PITR_GC_THREADS:-4}" \
    --socket   "$PITR_SOCKET" &
PITRD_PID=$!

stop_postgres() {
    if kill -0 "$PG_PID" >/dev/null 2>&1; then
        log "关闭 PostgreSQL..."
        if ! gosu postgres pg_ctl -D "$PGDATA" -m fast -w stop; then
            kill -TERM "$PG_PID" >/dev/null 2>&1 || true
        fi
        wait "$PG_PID" 2>/dev/null || true
    fi
}

shutdown() {
    log "收到停止信号,关闭 pitrd..."
    kill -TERM "$PITRD_PID" >/dev/null 2>&1 || true
    wait "$PITRD_PID" 2>/dev/null || true
    stop_postgres
    exit 0
}

trap shutdown TERM INT

set +e
wait "$PITRD_PID"
PITRD_RC=$?
set -e
trap - TERM INT

# pitrd 非预期退出时也关闭 PG,并把 pitrd exit code 交给容器运行时。
stop_postgres
exit "$PITRD_RC"
