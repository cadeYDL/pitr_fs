#!/usr/bin/env bash
# pitr-fs Linux 一键安装 / 恢复 / 状态检查
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_CONFIG="${PITR_INSTALL_CONFIG:-/etc/pitr-fs/install.conf}"
if [ -r "$INSTALL_CONFIG" ]; then
    # 文件由本脚本以 root:root 0644 生成，只包含经过 %q 转义的非敏感安装参数。
    # shellcheck disable=SC1090
    source "$INSTALL_CONFIG"
fi
IMAGE="${PITR_IMAGE:-${SAVED_IMAGE:-pitr-fs:latest}}"
CONTAINER="${PITR_CONTAINER:-${SAVED_CONTAINER:-pitrfs}}"
MOUNT_ROOT="${PITR_MOUNT_ROOT:-${SAVED_MOUNT_ROOT:-/pitr}}"
BIN_LINK="${PITR_BIN:-${SAVED_BIN_LINK:-/usr/local/bin/pitr}}"
PG_VOLUME="${PITR_PG_VOLUME:-${SAVED_PG_VOLUME:-pitr_pgdata}}"
DATA_VOLUME="${PITR_DATA_VOLUME:-${SAVED_DATA_VOLUME:-pitr_data}}"
CACHE_VOLUME="${PITR_CACHE_VOLUME:-${SAVED_CACHE_VOLUME:-pitr_cache}}"
JFS_CACHE_SIZE="${PITR_JFS_CACHE_SIZE:-${SAVED_JFS_CACHE_SIZE:-1024}}"
CACHE_VOLUME_MANAGED="${SAVED_CACHE_VOLUME_MANAGED:-}"
BLOCK_PATH="${PITR_BLOCK_PATH:-${SAVED_BLOCK_PATH:-}}"
RUNTIME_DIR="${PITR_RUNTIME_DIR:-${SAVED_RUNTIME_DIR:-/var/lib/pitr-fs/runtime}}"
HOST_UPGRADER="${PITR_HOST_UPGRADER:-${SAVED_HOST_UPGRADER:-/usr/local/lib/pitr-fs/pitr-host-upgrade}}"
UPDATE_REPOSITORY="${PITR_UPDATE_REPOSITORY:-${SAVED_UPDATE_REPOSITORY:-cadeYDL/pitr_fs}}"
UPDATE_API_URL="${PITR_UPDATE_API_URL:-${SAVED_UPDATE_API_URL:-https://api.github.com}}"
READY_TIMEOUT="${PITR_READY_TIMEOUT:-120}"
DOCKER_COMMAND=(docker)

usage() {
    cat <<EOF
用法: $0 [install|recover|status|logs]
  install               构建镜像、启动服务并安装 pitr 命令；不自动挂载目录
  recover               数据卷已存在时重启服务并恢复已 init 的挂载
  status                查看服务与挂载状态
  logs                  查看最近 200 行服务诊断日志

卸载请使用: source ./uninstall.sh [--purge]

仅支持 Linux。install 会自动检查并安装缺失的宿主依赖。

环境变量:
  PITR_MOUNT_ROOT  允许 pitr init 使用的挂载根目录 (默认 /pitr)
  PITR_CONTAINER   容器名 (默认 pitrfs)
  PITR_IMAGE       镜像名 (默认 pitr-fs:latest)
  PITR_PG_VOLUME   PostgreSQL Docker volume (默认 pitr_pgdata)
  PITR_DATA_VOLUME 对象数据 Docker volume (默认 pitr_data)
  PITR_CACHE_VOLUME JuiceFS 临时缓存 Docker volume (默认 pitr_cache)
  PITR_JFS_CACHE_SIZE JuiceFS 本地缓存上限 MiB (默认 1024)
  PITR_BLOCK_PATH  用户已挂载的块存储目录；为空时使用本地 Docker volume
  PITR_RUNTIME_DIR pitr/pitrd 版本化逻辑目录 (默认 /var/lib/pitr-fs/runtime)
  PITR_UPDATE_REPOSITORY 自动升级使用的 GitHub owner/repo (默认 cadeYDL/pitr_fs)
  PITR_UPDATE_API_URL GitHub Release API 地址 (默认 https://api.github.com)
  PITR_STORAGE     JuiceFS 存储后端 (默认 file); s3/minio/oss/cos/...
  PITR_BUCKET      存储 bucket URL / 本地路径 (默认容器内 /data)
  PITR_GC_INTERVAL 对象 GC 合并执行间隔 (默认 10m; 0 停用)
  PITR_GC_THREADS  对象 GC 删除并发数 (默认 4)
  AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY   云对象存储凭证 (透传)

非敏感安装参数会持久化到 $INSTALL_CONFIG，后续 recover 和 uninstall.sh 无需重复设置。
EOF
}

require_linux() {
    [ "$(uname -s)" = "Linux" ] || {
        echo "错误: pitr-fs 仅支持 Linux，不能在 $(uname -s) 上安装或运行" >&2
        exit 1
    }
}

validate_mount_root() {
    case "$MOUNT_ROOT" in
        /*) ;;
        *) echo "错误: PITR_MOUNT_ROOT 必须是绝对路径: $MOUNT_ROOT" >&2; exit 1 ;;
    esac
    [ "$MOUNT_ROOT" != "/" ] || {
        echo "错误: PITR_MOUNT_ROOT 不能是根目录" >&2
        exit 1
    }
    case "$JFS_CACHE_SIZE" in
        ''|*[!0-9]*)
            echo "错误: PITR_JFS_CACHE_SIZE 必须是正整数 MiB: $JFS_CACHE_SIZE" >&2
            exit 1
            ;;
    esac
    [ "$JFS_CACHE_SIZE" -ge 1 ] || {
        echo "错误: PITR_JFS_CACHE_SIZE 必须至少为 1 MiB" >&2
        exit 1
    }
    if [ -n "$BLOCK_PATH" ]; then
        case "$BLOCK_PATH" in
            /*) ;;
            *) echo "错误: PITR_BLOCK_PATH 必须是绝对路径: $BLOCK_PATH" >&2; exit 1 ;;
        esac
        [ "$BLOCK_PATH" != "/" ] || {
            echo "错误: PITR_BLOCK_PATH 不能是根目录" >&2
            exit 1
        }
        case "$BLOCK_PATH/" in
            "$MOUNT_ROOT/"* )
                echo "错误: PITR_BLOCK_PATH 不能位于 PITR_MOUNT_ROOT 内" >&2
                exit 1
                ;;
        esac
        case "$MOUNT_ROOT/" in
            "$BLOCK_PATH/"* )
                echo "错误: PITR_MOUNT_ROOT 不能位于 PITR_BLOCK_PATH 内" >&2
                exit 1
                ;;
        esac
    fi
    case "$RUNTIME_DIR" in
        /*) ;;
        *) echo "错误: PITR_RUNTIME_DIR 必须是绝对路径: $RUNTIME_DIR" >&2; exit 1 ;;
    esac
    [ "$RUNTIME_DIR" != "/" ] || {
        echo "错误: PITR_RUNTIME_DIR 不能是根目录" >&2
        exit 1
    }
    case "$HOST_UPGRADER" in
        /*) ;;
        *) echo "错误: PITR_HOST_UPGRADER 必须是绝对路径: $HOST_UPGRADER" >&2; exit 1 ;;
    esac
    if [[ ! "$UPDATE_REPOSITORY" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
        echo "错误: PITR_UPDATE_REPOSITORY 必须是 owner/repo: $UPDATE_REPOSITORY" >&2
        exit 1
    fi
    case "$UPDATE_API_URL" in
        https://*) ;;
        *) echo "错误: PITR_UPDATE_API_URL 必须使用 HTTPS: $UPDATE_API_URL" >&2; exit 1 ;;
    esac
}

sudo_if_needed() {
    if [ -w "$(dirname "$1")" ]; then
        echo ""
    elif ! command -v sudo >/dev/null 2>&1; then
        echo "错误: 写入 $1 需要 root 或 sudo" >&2
        return 1
    elif sudo -n true >/dev/null 2>&1; then
        echo "sudo"
    elif [ -t 0 ]; then
        sudo -v
        echo "sudo"
    else
        echo "错误: 写入 $1 需要 sudo；请在交互式终端运行本命令" >&2
        return 1
    fi
}

configure_docker() {
    command -v docker >/dev/null 2>&1 || {
        echo "错误: 缺少 docker；请先运行 ./scripts/install-deps.sh" >&2
        exit 1
    }
    if timeout 10 docker info >/dev/null 2>&1; then
        DOCKER_COMMAND=(docker)
        return 0
    fi
    if command -v sudo >/dev/null 2>&1 \
        && { sudo -n true >/dev/null 2>&1 || [ -t 0 ]; } \
        && timeout 10 sudo docker info >/dev/null 2>&1; then
        DOCKER_COMMAND=(sudo docker)
        echo "提示: 当前登录会话尚无 Docker 权限，本次自动使用 sudo；重新登录后可免 sudo"
        return 0
    fi
    echo "错误: Docker 服务未运行或当前用户无访问权限；请先运行 ./scripts/install-deps.sh" >&2
    exit 1
}

docker_cli() {
    "${DOCKER_COMMAND[@]}" "$@"
}

docker_cli_timeout() {
    local seconds=$1
    shift
    timeout "$seconds" "${DOCKER_COMMAND[@]}" "$@"
}

need_host_tools() {
    local command_name
    for command_name in findmnt fusermount3 realpath tar sha256sum; do
        command -v "$command_name" >/dev/null 2>&1 || {
            echo "错误: 缺少 $command_name；请先运行 ./scripts/install-deps.sh" >&2
            exit 1
        }
    done
    configure_docker
    [ -e /dev/fuse ] || {
        echo "错误: /dev/fuse 不存在；请加载 Linux fuse 模块" >&2
        exit 1
    }
}

prepare_runtime_dir() {
    local sudo marker="$RUNTIME_DIR/.pitr-runtime"
    if [ ! -d "$RUNTIME_DIR" ]; then
        sudo=$(sudo_if_needed "$RUNTIME_DIR")
        $sudo install -d -m 0755 "$RUNTIME_DIR"
    fi
    if [ ! -e "$marker" ]; then
        if [ -n "$(find "$RUNTIME_DIR" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
            echo "错误: PITR_RUNTIME_DIR 已存在且不是 pitr 运行目录: $RUNTIME_DIR" >&2
            exit 1
        fi
        sudo=$(sudo_if_needed "$marker")
        printf 'format=1\n' | $sudo tee "$marker" >/dev/null
        $sudo chmod 0644 "$marker"
    fi
    sudo=$(sudo_if_needed "$RUNTIME_DIR/versions")
    $sudo install -d -m 0755 "$RUNTIME_DIR/versions"
}

prepare_schema_marker() {
    local marker="$RUNTIME_DIR/schema.applied.sha256" digest sudo temporary
    [ ! -e "$marker" ] || return 0
    docker_cli inspect "$CONTAINER" >/dev/null 2>&1 || return 0
    digest=$(docker_cli exec "$CONTAINER" sh -c '
        schema=/etc/pitr/init_pitr.sql
        [ ! -r /opt/pitr/current/init_pitr.sql ] || schema=/opt/pitr/current/init_pitr.sql
        sha256sum "$schema"
    ' 2>/dev/null | awk '{print $1}' || true)
    if [ -z "$digest" ] && [ -r "$RUNTIME_DIR/current/init_pitr.sql" ]; then
        digest=$(sha256sum "$RUNTIME_DIR/current/init_pitr.sql" | awk '{print $1}')
    fi
    if [ -z "$digest" ]; then
        temporary=$(mktemp)
        if docker_cli cp "$CONTAINER:/etc/pitr/init_pitr.sql" "$temporary" \
            >/dev/null 2>&1; then
            digest=$(sha256sum "$temporary" | awk '{print $1}')
        fi
        rm -f "$temporary"
    fi
    [ -n "$digest" ] || return 0
    sudo=$(sudo_if_needed "$marker")
    printf '%s\n' "$digest" | $sudo tee "$marker" >/dev/null
    $sudo chmod 0644 "$marker"
}

ensure_host_environment() {
    echo "==> 检查 Linux 宿主机环境"
    bash "$SCRIPT_DIR/scripts/install-deps.sh"
}

prepare_mount_root() {
    if [ ! -d "$MOUNT_ROOT" ]; then
        local sudo
        sudo=$(sudo_if_needed "$MOUNT_ROOT")
        $sudo install -d -m 0755 -o "$(id -u)" -g "$(id -g)" "$MOUNT_ROOT"
    fi
}

prepare_block_storage() {
    [ -n "$BLOCK_PATH" ] || return 0
    if [ ! -d "$BLOCK_PATH" ]; then
        local sudo
        sudo=$(sudo_if_needed "$BLOCK_PATH")
        $sudo install -d -m 0755 -o "$(id -u)" -g "$(id -g)" "$BLOCK_PATH"
    fi
    [ -w "$BLOCK_PATH" ] || {
        echo "错误: 当前用户不能写入 PITR_BLOCK_PATH: $BLOCK_PATH" >&2
        exit 1
    }
}

prepare_cache_volume() {
    if docker_cli volume inspect "$CACHE_VOLUME" >/dev/null 2>&1; then
        CACHE_VOLUME_MANAGED="${CACHE_VOLUME_MANAGED:-0}"
        return 0
    fi
    docker_cli volume create "$CACHE_VOLUME" >/dev/null
    CACHE_VOLUME_MANAGED=1
}

detach_stale_fuse() {
    local attempt target index
    local -a targets relevant
    for attempt in $(seq 1 8); do
        mapfile -t targets < <(findmnt -rn -t fuse.pitrfs -o TARGET 2>/dev/null || true)
        relevant=()
        for target in "${targets[@]}"; do
            case "$target" in
                "$MOUNT_ROOT"|"$MOUNT_ROOT"/*) relevant+=("$target") ;;
            esac
        done
        [ "${#relevant[@]}" -ne 0 ] || return 0
        for ((index=${#relevant[@]}-1; index>=0; index--)); do
            target=${relevant[$index]}
            echo "==> 卸载失联的 pitr FUSE: $target (第 $attempt 轮)"
            if docker_cli_timeout 10 exec "$CONTAINER" fusermount3 -uz "$target" \
                >/dev/null 2>&1; then
                continue
            fi
            if fusermount3 -uz "$target" >/dev/null 2>&1; then
                continue
            fi
            if command -v sudo >/dev/null 2>&1; then
                sudo fusermount3 -uz "$target" >/dev/null 2>&1 ||
                    sudo umount -l "$target"
            else
                umount -l "$target"
            fi
        done
    done
    echo "错误: $MOUNT_ROOT 下的 pitr FUSE 层超过安全清理上限 8" >&2
    return 1
}

wait_ready() {
    echo "==> 等待 pitrd 就绪 (最多 ${READY_TIMEOUT} 秒)"
    local i
    for i in $(seq 1 "$READY_TIMEOUT"); do
        if docker_cli exec "$CONTAINER" test -S /var/run/pitrd.sock 2>/dev/null \
            && docker_cli exec "$CONTAINER" pitr status >/dev/null 2>&1; then
            echo "    就绪"
            return 0
        fi
        sleep 1
    done
    echo "错误: pitrd 未在 ${READY_TIMEOUT} 秒内就绪" >&2
    echo "  $0 logs" >&2
    return 1
}

install_wrapper() {
    local sudo quoted_root quoted_upgrader quoted_config
    sudo=$(sudo_if_needed "$BIN_LINK")
    printf -v quoted_root '%q' "$MOUNT_ROOT"
    printf -v quoted_upgrader '%q' "$HOST_UPGRADER"
    printf -v quoted_config '%q' "$INSTALL_CONFIG"
    $sudo tee "$BIN_LINK" >/dev/null <<EOF2
#!/usr/bin/env bash
# pitr Linux 宿主机 wrapper：把 CLI 转发到服务容器
set -euo pipefail
host_mount_root=$quoted_root
host_upgrader=$quoted_upgrader
install_config=$quoted_config
if [ "\${1:-}" = "upgrade" ]; then
    [ -x "\$host_upgrader" ] || {
        echo "错误: 宿主升级控制器不存在: \$host_upgrader" >&2
        exit 1
    }
    shift
    exec env PITR_INSTALL_CONFIG="\$install_config" \
        PITR_CALLER_PWD="\${PWD:-}" "\$host_upgrader" "\$@"
fi
pitr_args=("\$@")
if [ "\${1:-}" = "init" ] && [ -n "\${2:-}" ]; then
    pitr_args[1]="\$(realpath -m -- "\$2")"
fi
host_pwd=\${PWD:-}
case "\$host_pwd" in
    "\$host_mount_root"|"\$host_mount_root"/*) requested_workdir="\$host_pwd" ;;
    *) requested_workdir="\$host_mount_root" ;;
esac
# Docker 在创建 exec 进程前就会解析 --workdir。若回滚刚删除了调用者所在
# 目录，直接传入旧 PWD 会使 CLI 尚未启动便被 OCI runtime 拒绝。先从始终
# 存在的挂载根启动，再由容器内 shell 进入目标目录并处理删除竞态。
docker_args=(exec --workdir "\$host_mount_root")
if [ -t 0 ] && [ -t 1 ]; then
    docker_args+=(-it)
fi
docker_command=(docker)
if ! timeout 10 docker info >/dev/null 2>&1; then
    if command -v sudo >/dev/null 2>&1 \
        && { sudo -n true >/dev/null 2>&1 || [ -t 0 ]; } \
        && timeout 10 sudo docker info >/dev/null 2>&1; then
        docker_command=(sudo docker)
    else
        echo "错误: Docker 服务未运行或当前用户无访问权限" >&2
        exit 1
    fi
fi
exec "\${docker_command[@]}" "\${docker_args[@]}" "$CONTAINER" sh -c '
requested_workdir=\$1
host_mount_root=\$2
shift 2
container_workdir=\$requested_workdir
while ! cd "\$container_workdir" 2>/dev/null; do
    if [ "\$container_workdir" = "\$host_mount_root" ]; then
        echo "错误: pitr 挂载根目录不可访问: \$host_mount_root" >&2
        exit 1
    fi
    container_workdir=\${container_workdir%/*}
    [ -n "\$container_workdir" ] || container_workdir=\$host_mount_root
done
if [ "\$container_workdir" != "\$requested_workdir" ]; then
    echo "警告: 当前目录已不存在，改用最近的现存父目录: \$container_workdir" >&2
fi
cli=/usr/local/bin/pitr
[ ! -x /opt/pitr/current/pitr ] || cli=/opt/pitr/current/pitr
exec "\$cli" "\$@"
' sh "\$requested_workdir" "\$host_mount_root" "\${pitr_args[@]}"
EOF2
    $sudo chmod +x "$BIN_LINK"
}

install_host_upgrader() {
    local sudo quoted_runtime
    sudo=$(sudo_if_needed "$RUNTIME_DIR/pitr-host-upgrade-builtin")
    $sudo install -m 0755 "$SCRIPT_DIR/scripts/pitr-host-upgrade.sh" \
        "$RUNTIME_DIR/pitr-host-upgrade-builtin"
    sudo=$(sudo_if_needed "$HOST_UPGRADER")
    $sudo install -d -m 0755 "$(dirname "$HOST_UPGRADER")"
    printf -v quoted_runtime '%q' "$RUNTIME_DIR"
    $sudo tee "$HOST_UPGRADER" >/dev/null <<EOF2
#!/usr/bin/env bash
set -euo pipefail
runtime_dir=$quoted_runtime
caller_pwd=\${PITR_CALLER_PWD:-\${PWD:-}}
cd /
upgrader="\$runtime_dir/pitr-host-upgrade-builtin"
if [ -x "\$runtime_dir/current/pitr-host-upgrade" ]; then
    upgrader="\$runtime_dir/current/pitr-host-upgrade"
fi
exec env PITR_CALLER_PWD="\$caller_pwd" "\$upgrader" "\$@"
EOF2
    $sudo chmod 0755 "$HOST_UPGRADER"
}

write_install_config() {
    local sudo config_dir
    config_dir=$(dirname "$INSTALL_CONFIG")
    sudo=$(sudo_if_needed "$INSTALL_CONFIG")
    $sudo install -d -m 0755 "$config_dir"
    {
        printf 'SAVED_IMAGE=%q\n' "$IMAGE"
        printf 'SAVED_CONTAINER=%q\n' "$CONTAINER"
        printf 'SAVED_MOUNT_ROOT=%q\n' "$MOUNT_ROOT"
        printf 'SAVED_BIN_LINK=%q\n' "$BIN_LINK"
        printf 'SAVED_PG_VOLUME=%q\n' "$PG_VOLUME"
        printf 'SAVED_DATA_VOLUME=%q\n' "$DATA_VOLUME"
        printf 'SAVED_CACHE_VOLUME=%q\n' "$CACHE_VOLUME"
        printf 'SAVED_JFS_CACHE_SIZE=%q\n' "$JFS_CACHE_SIZE"
        printf 'SAVED_CACHE_VOLUME_MANAGED=%q\n' "$CACHE_VOLUME_MANAGED"
        printf 'SAVED_BLOCK_PATH=%q\n' "$BLOCK_PATH"
        printf 'SAVED_RUNTIME_DIR=%q\n' "$RUNTIME_DIR"
        printf 'SAVED_HOST_UPGRADER=%q\n' "$HOST_UPGRADER"
        printf 'SAVED_UPDATE_REPOSITORY=%q\n' "$UPDATE_REPOSITORY"
        printf 'SAVED_UPDATE_API_URL=%q\n' "$UPDATE_API_URL"
    } | $sudo tee "$INSTALL_CONFIG" >/dev/null
    $sudo chmod 0644 "$INSTALL_CONFIG"
}

run_container() {
    local -a block_mount
    if [ -n "$BLOCK_PATH" ]; then
        block_mount=(--mount "type=bind,source=$BLOCK_PATH,target=/data")
    else
        block_mount=(-v "$DATA_VOLUME:/data")
    fi
    detach_stale_fuse
    if docker_cli_timeout 10 inspect "$CONTAINER" >/dev/null 2>&1; then
        docker_cli_timeout 30 rm -f "$CONTAINER" >/dev/null 2>&1 || {
            echo "错误: 旧服务未能在 30 秒内停止；请运行 $0 logs 查看诊断信息" >&2
            return 1
        }
    fi
    docker_cli run -d --name "$CONTAINER" \
        --restart unless-stopped \
        --privileged \
        --pid host \
        --device /dev/fuse \
        --cap-add SYS_ADMIN \
        --security-opt apparmor:unconfined \
        -e "POSTGRES_USER=${PITR_POSTGRES_USER:-pitr}" \
        -e "POSTGRES_PASSWORD=${PITR_POSTGRES_PASSWORD:-pitr}" \
        -e "POSTGRES_DB=${PITR_POSTGRES_DB:-pitr_fs}" \
        -e "PITR_MOUNT_ROOT=$MOUNT_ROOT" \
        ${PITR_STORAGE:+-e "PITR_STORAGE=$PITR_STORAGE"} \
        ${PITR_BUCKET:+-e "PITR_BUCKET=$PITR_BUCKET"} \
        ${AWS_ACCESS_KEY_ID:+-e "AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID"} \
        ${AWS_SECRET_ACCESS_KEY:+-e "AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY"} \
        ${PITR_GC_INTERVAL:+-e "PITR_GC_INTERVAL=$PITR_GC_INTERVAL"} \
        ${PITR_GC_THREADS:+-e "PITR_GC_THREADS=$PITR_GC_THREADS"} \
        -e "PITR_JFS_CACHE_SIZE=$JFS_CACHE_SIZE" \
        -v "$PG_VOLUME:/var/lib/postgresql/data" \
        -v "$CACHE_VOLUME:/var/jfsCache" \
        "${block_mount[@]}" \
        --mount "type=bind,source=/etc/passwd,target=/host/etc/passwd,readonly" \
        --mount "type=bind,source=$MOUNT_ROOT,target=$MOUNT_ROOT,bind-propagation=rshared" \
        --mount "type=bind,source=$RUNTIME_DIR,target=/opt/pitr" \
        "$IMAGE" >/dev/null
}

do_install() {
    ensure_host_environment
    need_host_tools
    prepare_mount_root
    prepare_block_storage
    prepare_cache_volume
    prepare_runtime_dir
    prepare_schema_marker
    bash "$SCRIPT_DIR/scripts/install-deps.sh" --docker-snapshot-before
    echo "==> 构建镜像 $IMAGE"
    local build_commit build_date build_version
    build_commit=$(git -C "$SCRIPT_DIR" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
    build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    build_version=${PITR_VERSION:-dev-$build_commit}
    docker_cli build -t "$IMAGE" -f "$SCRIPT_DIR/deploy/Dockerfile" \
        --build-arg "PITR_VERSION=$build_version" \
        --build-arg "PITR_COMMIT=$build_commit" \
        --build-arg "PITR_BUILD_DATE=$build_date" \
        "$SCRIPT_DIR"
    echo "==> 启动容器 $CONTAINER"
    run_container
    wait_ready
    echo "==> 安装命令 $BIN_LINK"
    install_host_upgrader
    install_wrapper
    write_install_config
    bash "$SCRIPT_DIR/scripts/install-deps.sh" --docker-snapshot-after "$IMAGE"
    echo
    echo "  ✓ 服务安装完成"
}

do_status() {
    configure_docker
    if docker_cli inspect "$CONTAINER" >/dev/null 2>&1; then
        docker_cli inspect -f 'service:   {{.Name}}
status:    {{.State.Status}}
started:   {{.State.StartedAt}}' "$CONTAINER"
        if [ "$(docker_cli inspect -f '{{.State.Status}}' "$CONTAINER")" = "running" ]; then
            echo
            docker_cli exec "$CONTAINER" pitr status
        fi
    else
        echo "服务未安装或未运行"
    fi
}

do_logs() {
    configure_docker
    docker_cli inspect "$CONTAINER" >/dev/null 2>&1 || {
        echo "错误: 服务未安装；请先执行 $0 install" >&2
        exit 1
    }
    docker_cli logs --tail 200 "$CONTAINER"
}

do_recover() {
    need_host_tools
    prepare_mount_root
    prepare_block_storage
    prepare_cache_volume
    prepare_runtime_dir
    prepare_schema_marker
    if docker_cli inspect "$CONTAINER" >/dev/null 2>&1; then
        local state
        state=$(docker_cli inspect -f '{{.State.Status}}' "$CONTAINER")
        if [ "$state" != "running" ]; then
            detach_stale_fuse
            docker_cli start "$CONTAINER" >/dev/null
        fi
    else
        docker_cli volume inspect "$PG_VOLUME" >/dev/null 2>&1 || {
            echo "错误: $PG_VOLUME 不存在；请先执行 $0 install" >&2
            exit 1
        }
        run_container
    fi
    wait_ready
    install_host_upgrader
    install_wrapper
    if docker_cli exec "$CONTAINER" pitr status | grep -q 'fuse='; then
        docker_cli exec "$CONTAINER" pitr recover
        echo "  ✓ 服务与挂载恢复完成"
    else
        echo "  ✓ 服务恢复完成；尚无已 init 的挂载"
    fi
}

install_main() {
    ACTION="${1:-install}"
    case "$ACTION" in
        -h|--help) usage ;;
        install|recover|status|logs)
            require_linux
            validate_mount_root
            case "$ACTION" in
                install) do_install ;;
                recover) do_recover ;;
                status) do_status ;;
                logs) do_logs ;;
            esac
            ;;
        *) usage; return 1 ;;
    esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    install_main "$@"
fi
