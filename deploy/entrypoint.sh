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
        --trash-days 36500 \
        "$PG_DSN" "$PITR_VOLUME" >/dev/null
    log "juicefs format 完成; 再跑一次 init SQL 装 jfs_* 触发器..."
    PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
        -v ON_ERROR_STOP=1 -f /etc/pitr/init_pitr.sql >/dev/null
else
    log "JuiceFS 卷已存在, 跳过 format (recover 场景)"
fi

# 5. 建目录
mkdir -p "$(dirname "$PITR_SOCKET")" "$PITR_MOUNT" /var/lib/pitr/jfs

# 6. exec pitrd 成为主进程 (P1 骨架, 立刻返回; P2 之后阻塞持有 socket)
log "启动 pitrd..."
exec pitrd \
    --pg-dsn "$PG_DSN" \
    --volume "$PITR_VOLUME" \
    --jfs-mount /var/lib/pitr/jfs \
    --fuse-mount "$PITR_MOUNT" \
    --socket   "$PITR_SOCKET"
