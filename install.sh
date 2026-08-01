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
BLOCK_PATH="${PITR_BLOCK_PATH:-${SAVED_BLOCK_PATH:-}}"
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
  PITR_BLOCK_PATH  用户已挂载的块存储目录；为空时使用本地 Docker volume
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
    for command_name in findmnt fusermount3 realpath; do
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
    local sudo quoted_root
    sudo=$(sudo_if_needed "$BIN_LINK")
    printf -v quoted_root '%q' "$MOUNT_ROOT"
    $sudo tee "$BIN_LINK" >/dev/null <<EOF2
#!/usr/bin/env bash
# pitr Linux 宿主机 wrapper：把 CLI 转发到服务容器
set -euo pipefail
host_mount_root=$quoted_root
pitr_args=("\$@")
if [ "\${1:-}" = "init" ] && [ -n "\${2:-}" ]; then
    pitr_args[1]="\$(realpath -m -- "\$2")"
fi
case "\$PWD" in
    "\$host_mount_root"|"\$host_mount_root"/*) container_workdir="\$PWD" ;;
    *) container_workdir="\$host_mount_root" ;;
esac
docker_args=(exec --workdir "\$container_workdir")
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
exec "\${docker_command[@]}" "\${docker_args[@]}" "$CONTAINER" pitr "\${pitr_args[@]}"
EOF2
    $sudo chmod +x "$BIN_LINK"
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
        printf 'SAVED_BLOCK_PATH=%q\n' "$BLOCK_PATH"
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
        -v "$PG_VOLUME:/var/lib/postgresql/data" \
        "${block_mount[@]}" \
        --mount "type=bind,source=/etc/passwd,target=/host/etc/passwd,readonly" \
        --mount "type=bind,source=$MOUNT_ROOT,target=$MOUNT_ROOT,bind-propagation=rshared" \
        "$IMAGE" >/dev/null
}

do_install() {
    ensure_host_environment
    need_host_tools
    prepare_mount_root
    prepare_block_storage
    bash "$SCRIPT_DIR/scripts/install-deps.sh" --docker-snapshot-before
    echo "==> 构建镜像 $IMAGE"
    docker_cli build -t "$IMAGE" -f "$SCRIPT_DIR/deploy/Dockerfile" "$SCRIPT_DIR"
    echo "==> 启动容器 $CONTAINER"
    run_container
    wait_ready
    echo "==> 安装命令 $BIN_LINK"
    install_wrapper
    write_install_config
    bash "$SCRIPT_DIR/scripts/install-deps.sh" --docker-snapshot-after "$IMAGE"
    local restored_mount
    restored_mount=$(docker_cli exec "$CONTAINER" pitr status 2>/dev/null | awk -F '\t' '
        NR > 1 { for (i=1; i<=NF; i++) if ($i ~ /^fuse=/) { sub(/^fuse=/, "", $i); print $i; exit } }
    ')
    if [ -n "$restored_mount" ] && mountpoint -q "$restored_mount"; then
        cat <<EOF

  ✓ 服务安装完成，已有挂载已恢复: $restored_mount
  块存储: ${BLOCK_PATH:-本地 Docker volume $DATA_VOLUME}

  可以直接进入挂载目录使用 pitr；运行 pitr status 查看配置。
EOF
    else
        cat <<EOF

  ✓ 服务安装完成，尚未挂载用户目录
  允许的挂载根目录: $MOUNT_ROOT
  块存储: ${BLOCK_PATH:-本地 Docker volume $DATA_VOLUME}

  下一步:
    mkdir -p $MOUNT_ROOT/data
    pitr init $MOUNT_ROOT/data
    cd $MOUNT_ROOT/data
    echo hi > a.txt
    pitr logs . -n 5
EOF
    fi
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
    [ -x "$BIN_LINK" ] || install_wrapper
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
