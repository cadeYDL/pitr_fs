#!/usr/bin/env bash
# 容器 PID 1 编排:PG → 幂等 apply SQL → juicefs format(首次)→ 再 apply SQL → exec pitrd
# 设计文档 §13.3
set -euo pipefail

log() { echo "[pitr] $*"; }

# 逻辑升级运行时和仅供诊断的宿主 schema 摘要缓存保存在这里；数据库内账本才是
# schema 状态的权威来源。全新安装没有宿主机升级脚本，entrypoint 必须自行创建目录。
mkdir -p /opt/pitr

runtime_file() {
    local name=$1
    if [ -e "/opt/pitr/current/$name" ]; then
        printf '/opt/pitr/current/%s\n' "$name"
    else
        case "$name" in
            init_pitr.sql|SCHEMA-COMPAT) printf '/etc/pitr/%s\n' "$name" ;;
            *) printf '/usr/local/bin/%s\n' "$name" ;;
        esac
    fi
}

contract_value() {
    local file=$1 key=$2 value
    value=$(awk -F= -v key="$key" '
        $1==key && $2 ~ /^[0-9]+$/ { value=$2; count++ }
        END { if (count==1) print value; else exit 1 }
    ' "$file") || return 1
    printf '%s\n' "$value"
}

apply_schema() {
    local force=${1:-0} digest applied temporary contract revision min_logic
    local logic_version schema_table
    digest=$(sha256sum "$SCHEMA_PATH" | awk '{print $1}')
    contract=$(runtime_file SCHEMA-COMPAT)
    revision=$(contract_value "$contract" schema_revision) || {
        log "schema 兼容契约损坏: $contract"
        return 1
    }
    min_logic=$(contract_value "$contract" min_logic_revision) || {
        log "schema 兼容契约损坏: $contract"
        return 1
    }
    [ "$min_logic" -le "$revision" ] || {
        log "schema 兼容契约非法: min_logic_revision > schema_revision"
        return 1
    }
    logic_version=$($(runtime_file pitr) version --client-only |
        awk '$1=="pitr" { print $2; exit }')
    [ -n "$logic_version" ] || return 1

    # 外部摘要只保留作运维提示；是否跳过完全由数据库内账本决定。
    schema_table=$(PGPASSWORD="$POSTGRES_PASSWORD" psql -At \
        -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
        -v ON_ERROR_STOP=1 -c "SELECT to_regclass('public.pitr_schema_state')")
    applied=""
    if [ "$schema_table" = pitr_schema_state ]; then
        applied=$(PGPASSWORD="$POSTGRES_PASSWORD" psql -At \
            -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
            -v ON_ERROR_STOP=1 -c \
            "SELECT digest FROM pitr_schema_state WHERE singleton" || true)
    fi
    if [ "$force" -eq 0 ] && [ "$digest" = "$applied" ]; then
        log "MVCC schema 内容未变化,跳过重复校准"
        return 0
    fi
    {
        cat "$SCHEMA_PATH"
        cat <<'SQL'
INSERT INTO pitr_schema_state(
    singleton,schema_revision,min_logic_revision,digest,logic_version,applied_at
) VALUES (
    true,:schema_revision,:min_logic_revision,:'schema_digest',:'logic_version',
    clock_timestamp()
)
ON CONFLICT (singleton) DO UPDATE
   SET schema_revision=EXCLUDED.schema_revision,
       min_logic_revision=EXCLUDED.min_logic_revision,
       digest=EXCLUDED.digest,
       logic_version=EXCLUDED.logic_version,
       applied_at=EXCLUDED.applied_at;
SQL
    } | PGOPTIONS="-c client_min_messages=warning" \
        PGPASSWORD="$POSTGRES_PASSWORD" psql --single-transaction \
        -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
        -v ON_ERROR_STOP=1 -v schema_revision="$revision" \
        -v min_logic_revision="$min_logic" -v schema_digest="$digest" \
        -v logic_version="$logic_version" >/dev/null
    temporary=/opt/pitr/.schema.applied.$$
    printf '%s\n' "$digest" >"$temporary"
    mv -f "$temporary" /opt/pitr/schema.applied.sha256
}

reconcile_database_collation() {
    local versions stored actual reindex_sql refresh_sql
    versions=$(PGOPTIONS="-c client_min_messages=error" \
        PGPASSWORD="$POSTGRES_PASSWORD" psql -At -F '|' -h 127.0.0.1 \
        -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
        -c "SELECT COALESCE(datcollversion,''),COALESCE(pg_database_collation_actual_version(oid),'') FROM pg_database WHERE datname=current_database()")
    stored=${versions%%|*}
    actual=${versions#*|}
    [ -n "$actual" ] || return 0
    [ "$stored" != "$actual" ] || return 0

    log "检测到数据库排序规则版本变化 ($stored -> $actual)，服务启动前重建相关索引..."
    reindex_sql=$(PGOPTIONS="-c client_min_messages=error" \
        PGPASSWORD="$POSTGRES_PASSWORD" psql -At -h 127.0.0.1 \
        -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
        -c "SELECT format('REINDEX DATABASE %I',current_database())")
    refresh_sql=$(PGOPTIONS="-c client_min_messages=error" \
        PGPASSWORD="$POSTGRES_PASSWORD" psql -At -h 127.0.0.1 \
        -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
        -c "SELECT format('ALTER DATABASE %I REFRESH COLLATION VERSION',current_database())")
    PGOPTIONS="-c client_min_messages=error" \
        PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 \
        -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
        -c "$reindex_sql" >/dev/null
    PGOPTIONS="-c client_min_messages=error" \
        PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 \
        -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
        -c "$refresh_sql" >/dev/null
    log "数据库排序规则索引校准完成"
}

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

# 从旧基础镜像迁移到固定 PostgreSQL 镜像时，glibc/ICU 排序规则版本可能
# 变化。此时 PostgreSQL 会在每次连接输出底层 WARNING；趁 pitrd 尚未启动，
# 原子重建依赖索引并刷新版本，避免把维护细节暴露给普通命令用户。
reconcile_database_collation

PG_DSN="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:5432/${POSTGRES_DB}?sslmode=disable"

# 已有卷必须在任何 pitr schema/trigger 变更前完成只读 ABI 校验。使用镜像
# 内置 pitrd 而不是可热切换的逻辑版本，保证校验器与固定 JuiceFS 二进制匹配。
if juicefs status "$PG_DSN" >/dev/null 2>&1; then
    log "校验已有 JuiceFS/PostgreSQL 兼容契约..."
    /usr/local/bin/pitrd \
        --pg-dsn "$PG_DSN" \
        --check-compatibility \
        --log-level warn
fi

# 3. 幂等再跑一次建表 (补丁生效, 不依赖 initdb.d 是否执行过)
log "校准 MVCC schema..."
SCHEMA_PATH=$(runtime_file init_pitr.sql)
apply_schema 0

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
    apply_schema 1
else
    log "JuiceFS 卷已存在, 跳过 format (recover 场景)"
fi

# PITR 已在 PostgreSQL 中为保留版本建立 slice pin。关闭 JuiceFS 自身的
# 长期 trash,让被淘汰版本释放 pin 后可由原生 gc 真正回收对象。
log "校准 JuiceFS 对象生命周期策略 (trash-days=0)..."
juicefs config --yes --trash-days 0 "$PG_DSN" >/dev/null

# 5. 建目录
mkdir -p "$(dirname "$PITR_SOCKET")" "$PITR_MOUNT_ROOT" /var/lib/pitr/jfs /run/pitr

# 6. 启动 pitrd。entrypoint 保持为编排进程,确保容器停止时先让 gRPC 优雅退出,
# 再关闭 PostgreSQL,避免下次启动触发 WAL crash recovery。
PITRD_PID=""

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
    if [ -n "$PITRD_PID" ]; then
        kill -TERM "$PITRD_PID" >/dev/null 2>&1 || true
        wait "$PITRD_PID" 2>/dev/null || true
    fi
    stop_postgres
    exit 0
}

trap shutdown TERM INT

activate_runtime() {
    local target=$1 temporary=/opt/pitr/.current.$$
    if [ "$target" = builtin ]; then
        rm -f /opt/pitr/current
        return 0
    fi
    case "$target" in
        ''|*[!A-Za-z0-9._+-]*) return 1 ;;
    esac
    [ -x "/opt/pitr/versions/$target/pitrd" ] || return 1
    rm -f "$temporary"
    ln -s "versions/$target" "$temporary"
    mv -Tf "$temporary" /opt/pitr/current
}

while true; do
    PITRD_BIN=$(runtime_file pitrd)
    log "启动 pitrd ($("$PITRD_BIN" --help 2>/dev/null | head -1 || basename "$PITRD_BIN"))..."
    "$PITRD_BIN" \
        --pg-dsn "$PG_DSN" \
        --volume "$PITR_VOLUME" \
        --jfs-mount /var/lib/pitr/jfs \
        --mount-root "$PITR_MOUNT_ROOT" \
        --gc-interval "${PITR_GC_INTERVAL:-10m}" \
        --gc-threads "${PITR_GC_THREADS:-4}" \
        --jfs-cache-size "${PITR_JFS_CACHE_SIZE:-1024}" \
        --socket "$PITR_SOCKET" &
    PITRD_PID=$!
    printf '%s\n' "$PITRD_PID" >/run/pitr/pitrd.pid

    set +e
    wait "$PITRD_PID"
    PITRD_RC=$?
    set -e
    PITRD_PID=""
    rm -f /run/pitr/pitrd.pid

    if [ -e /run/pitr/restart.request ]; then
        rm -f /run/pitr/restart.request
        log "按升级请求重启 pitrd，PostgreSQL 保持运行"
        continue
    fi
    if [ -r /opt/pitr/upgrade-fallback ]; then
        fallback=$(cat /opt/pitr/upgrade-fallback)
        if activate_runtime "$fallback"; then
            rm -f /opt/pitr/upgrade-fallback
            log "新版 pitrd 异常退出，自动切回 $fallback"
            continue
        fi
        log "自动回退目标无效: $fallback"
    fi

    # 非升级场景的异常退出仍传给容器运行时，保持原有故障可见性。
    trap - TERM INT
    stop_postgres
    exit "$PITRD_RC"
done
