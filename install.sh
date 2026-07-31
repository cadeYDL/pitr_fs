#!/usr/bin/env bash
# pitr-fs Linux 一键安装 / 卸载 / 恢复
set -euo pipefail

IMAGE="${PITR_IMAGE:-pitr-fs:latest}"
CONTAINER="${PITR_CONTAINER:-pitrfs}"
MOUNT_ROOT="${PITR_MOUNT_ROOT:-/pitr}"
BIN_LINK="${PITR_BIN:-/usr/local/bin/pitr}"
PG_VOLUME="${PITR_PG_VOLUME:-pitr_pgdata}"
DATA_VOLUME="${PITR_DATA_VOLUME:-pitr_data}"
READY_TIMEOUT="${PITR_READY_TIMEOUT:-120}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
用法: $0 [install|recover|uninstall|status]
  install               构建镜像、启动服务并安装 pitr 命令；不自动挂载目录
  recover               数据卷已存在时重启服务并恢复已 init 的挂载
  uninstall [--purge]   删除容器和 wrapper；--purge 一并清理数据卷
  status                查看容器状态

仅支持 Linux。首次安装依赖可执行: ./scripts/install-deps.sh

环境变量:
  PITR_MOUNT_ROOT  允许 pitr init 使用的挂载根目录 (默认 /pitr)
  PITR_CONTAINER   容器名 (默认 pitrfs)
  PITR_IMAGE       镜像名 (默认 pitr-fs:latest)
  PITR_PG_VOLUME   PostgreSQL Docker volume (默认 pitr_pgdata)
  PITR_DATA_VOLUME 对象数据 Docker volume (默认 pitr_data)
  PITR_STORAGE     JuiceFS 存储后端 (默认 file); s3/minio/oss/cos/...
  PITR_BUCKET      存储 bucket URL / 本地路径 (默认容器内 /data)
  AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY   云对象存储凭证 (透传)
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
}

sudo_if_needed() {
    if [ -w "$(dirname "$1")" ]; then echo ""; else echo "sudo"; fi
}

need_host_tools() {
    local command_name
    for command_name in docker findmnt fusermount3 realpath; do
        command -v "$command_name" >/dev/null 2>&1 || {
            echo "错误: 缺少 $command_name；请先运行 ./scripts/install-deps.sh" >&2
            exit 1
        }
    done
    docker info >/dev/null 2>&1 || {
        echo "错误: Docker daemon 未运行" >&2
        exit 1
    }
    [ -e /dev/fuse ] || {
        echo "错误: /dev/fuse 不存在；请加载 Linux fuse 模块" >&2
        exit 1
    }
}

prepare_mount_root() {
    if [ ! -d "$MOUNT_ROOT" ]; then
        local sudo
        sudo=$(sudo_if_needed "$MOUNT_ROOT")
        $sudo install -d -m 0755 -o "$(id -u)" -g "$(id -g)" "$MOUNT_ROOT"
    fi
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
        if docker exec "$CONTAINER" test -S /var/run/pitrd.sock 2>/dev/null \
            && docker exec "$CONTAINER" pitr status >/dev/null 2>&1; then
            echo "    就绪"
            return 0
        fi
        sleep 1
    done
    echo "错误: pitrd 未在 ${READY_TIMEOUT} 秒内就绪" >&2
    echo "  docker logs $CONTAINER" >&2
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
exec docker "\${docker_args[@]}" "$CONTAINER" pitr "\${pitr_args[@]}"
EOF2
    $sudo chmod +x "$BIN_LINK"
}

run_container() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    detach_stale_fuse
    docker run -d --name "$CONTAINER" \
        --restart unless-stopped \
        --privileged \
        --pid host \
        --device /dev/fuse \
        --cap-add SYS_ADMIN \
        --security-opt apparmor:unconfined \
        -e "PITR_MOUNT_ROOT=$MOUNT_ROOT" \
        ${PITR_STORAGE:+-e "PITR_STORAGE=$PITR_STORAGE"} \
        ${PITR_BUCKET:+-e "PITR_BUCKET=$PITR_BUCKET"} \
        ${AWS_ACCESS_KEY_ID:+-e "AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID"} \
        ${AWS_SECRET_ACCESS_KEY:+-e "AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY"} \
        -v "$PG_VOLUME:/var/lib/postgresql/data" \
        -v "$DATA_VOLUME:/data" \
        --mount "type=bind,source=/etc/passwd,target=/host/etc/passwd,readonly" \
        --mount "type=bind,source=$MOUNT_ROOT,target=$MOUNT_ROOT,bind-propagation=rshared" \
        "$IMAGE" >/dev/null
}

do_install() {
    need_host_tools
    prepare_mount_root
    echo "==> 构建镜像 $IMAGE"
    docker build -t "$IMAGE" -f "$SCRIPT_DIR/deploy/Dockerfile" "$SCRIPT_DIR"
    echo "==> 启动容器 $CONTAINER"
    run_container
    wait_ready
    echo "==> 安装命令 $BIN_LINK"
    install_wrapper
    cat <<EOF

  ✓ 服务安装完成，尚未挂载用户目录
  允许的挂载根目录: $MOUNT_ROOT

  下一步:
    mkdir -p $MOUNT_ROOT/data
    pitr init $MOUNT_ROOT/data
    cd $MOUNT_ROOT/data
    echo hi > a.txt
    pitr logs . -n 5
EOF
}

do_uninstall() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    detach_stale_fuse
    if [ "${1:-}" = "--purge" ]; then
        docker volume rm "$PG_VOLUME" "$DATA_VOLUME" >/dev/null 2>&1 || true
        echo "  数据卷已清理"
    fi
    local sudo
    sudo=$(sudo_if_needed "$BIN_LINK")
    $sudo rm -f "$BIN_LINK"
    echo "  ✓ 已卸载"
}

do_status() {
    if docker inspect "$CONTAINER" >/dev/null 2>&1; then
        docker inspect -f 'container: {{.Name}}
status:    {{.State.Status}}
started:   {{.State.StartedAt}}' "$CONTAINER"
    else
        echo "容器未运行"
    fi
}

do_recover() {
    need_host_tools
    prepare_mount_root
    if docker inspect "$CONTAINER" >/dev/null 2>&1; then
        local state
        state=$(docker inspect -f '{{.State.Status}}' "$CONTAINER")
        if [ "$state" != "running" ]; then
            detach_stale_fuse
            docker start "$CONTAINER" >/dev/null
        fi
    else
        docker volume inspect "$PG_VOLUME" >/dev/null 2>&1 || {
            echo "错误: $PG_VOLUME 不存在；请先执行 $0 install" >&2
            exit 1
        }
        run_container
    fi
    wait_ready
    [ -x "$BIN_LINK" ] || install_wrapper
    if docker exec "$CONTAINER" sh -lc \
        "psql -U pitr -d pitr_fs -Atqc 'SELECT EXISTS (SELECT 1 FROM pitr_volume_config)'" |
        grep -qx t; then
        docker exec "$CONTAINER" pitr recover
        echo "  ✓ 服务与挂载恢复完成"
    else
        echo "  ✓ 服务恢复完成；尚无已 init 的挂载"
    fi
}

ACTION="${1:-install}"
case "$ACTION" in
    -h|--help) usage ;;
    install|recover|uninstall|status)
        require_linux
        validate_mount_root
        case "$ACTION" in
            install) do_install ;;
            recover) do_recover ;;
            uninstall) do_uninstall "${2:-}" ;;
            status) do_status ;;
        esac
        ;;
    *) usage; exit 1 ;;
esac
