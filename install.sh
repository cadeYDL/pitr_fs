#!/usr/bin/env bash
# pitr-fs 一键安装 / 卸载 / 恢复
# 设计文档 §13.5
set -euo pipefail

IMAGE="${PITR_IMAGE:-pitr-fs:latest}"
CONTAINER="${PITR_CONTAINER:-pitrfs}"
WORKSPACE="${PITR_WORKSPACE:-$HOME/pitr-workspace}"
BIN_LINK="${PITR_BIN:-/usr/local/bin/pitr}"
PG_VOLUME="${PITR_PG_VOLUME:-pitr_pgdata}"
DATA_VOLUME="${PITR_DATA_VOLUME:-pitr_data}"
READY_TIMEOUT="${PITR_READY_TIMEOUT:-120}"   # 秒
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
用法: $0 [install|recover|uninstall|status]
  install               构建镜像 + 启动容器 + 安装宿主机 pitr 命令(默认)
  recover               数据卷已存在时: 重启容器 + 恢复挂载 (不 format)
  uninstall [--purge]   停止并删除容器/wrapper; --purge 一并清理数据卷
  status                查看容器状态

环境变量:
  PITR_WORKSPACE   宿主机工作目录 (默认 \$HOME/pitr-workspace)
  PITR_CONTAINER   容器名 (默认 pitrfs)
  PITR_IMAGE       镜像名 (默认 pitr-fs:latest)
  PITR_PG_VOLUME   PostgreSQL Docker volume (默认 pitr_pgdata)
  PITR_DATA_VOLUME 对象数据 Docker volume (默认 pitr_data)
  PITR_STORAGE     juicefs 存储后端 (默认 file); s3/minio/oss/cos/...
  PITR_BUCKET      存储 bucket URL / 本地路径 (默认容器内 /data)
  AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY   云对象存储凭证 (透传)
EOF
}

sudo_if_needed() {
    if [ -w "$(dirname "$1")" ]; then echo ""; else echo "sudo"; fi
}

need_docker() {
    command -v docker >/dev/null 2>&1 || { echo "错误: 未安装 Docker" >&2; exit 1; }
    docker info >/dev/null 2>&1        || { echo "错误: Docker daemon 未运行" >&2; exit 1; }
}

detach_stale_fuse() {
    case "$WORKSPACE" in
        /*) ;;
        *) echo "错误: PITR_WORKSPACE 必须是绝对路径: $WORKSPACE" >&2; return 1 ;;
    esac
    [ "$WORKSPACE" != "/" ] ||
        { echo "错误: PITR_WORKSPACE 不能是根目录" >&2; return 1; }

    local attempt
    for attempt in $(seq 1 8); do
        if ! findmnt -n -o FSTYPE --target "$WORKSPACE" 2>/dev/null |
            grep -qx "fuse.pitrfs"; then
            return 0
        fi
        echo "==> 卸载失联的 pitr FUSE: $WORKSPACE (第 $attempt 层)"
        if fusermount3 -uz "$WORKSPACE" >/dev/null 2>&1; then
            continue
        fi
        if command -v sudo >/dev/null 2>&1; then
            sudo fusermount3 -uz "$WORKSPACE" >/dev/null 2>&1 ||
                sudo umount -l "$WORKSPACE"
            continue
        fi
        umount -l "$WORKSPACE"
    done
    if findmnt -n -o FSTYPE --target "$WORKSPACE" 2>/dev/null |
        grep -qx "fuse.pitrfs"; then
        echo "错误: $WORKSPACE 的 pitr FUSE 层超过安全清理上限 8" >&2
        return 1
    fi
}

# 端到端 ready 信号:socket 建立且一次真实 gRPC Status 成功。只检查 socket
# 文件会在 SIGKILL 后误认容器可用，因为 writable layer 可能短暂保留
# 上一进程留下的 unix socket。
wait_ready() {
    echo "==> 等待 pitrd 就绪 (最多 ${READY_TIMEOUT} 秒)"
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
    local sudo quoted_workspace
    sudo=$(sudo_if_needed "$BIN_LINK")
    printf -v quoted_workspace '%q' "$WORKSPACE"
    $sudo tee "$BIN_LINK" >/dev/null <<EOF2
#!/usr/bin/env bash
# pitr 宿主机 wrapper, 转发到容器内 CLI
set -euo pipefail
host_workspace=$quoted_workspace
container_workdir=/workspace
case "\$PWD" in
    "\$host_workspace") ;;
    "\$host_workspace"/*)
        container_workdir="/workspace/\${PWD#"\$host_workspace"/}"
        ;;
esac
docker_args=(exec --workdir "\$container_workdir")
if [ -t 0 ] && [ -t 1 ]; then
    docker_args+=(-it)
fi
exec docker "\${docker_args[@]}" "$CONTAINER" pitr "\$@"
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
        ${PITR_STORAGE:+-e "PITR_STORAGE=$PITR_STORAGE"} \
        ${PITR_BUCKET:+-e "PITR_BUCKET=$PITR_BUCKET"} \
        ${AWS_ACCESS_KEY_ID:+-e "AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID"} \
        ${AWS_SECRET_ACCESS_KEY:+-e "AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY"} \
        -v "$PG_VOLUME:/var/lib/postgresql/data" \
        -v "$DATA_VOLUME:/data" \
        --mount "type=bind,source=/etc/passwd,target=/host/etc/passwd,readonly" \
        --mount "type=bind,source=$WORKSPACE,target=/workspace,bind-propagation=rshared" \
        "$IMAGE" >/dev/null
}

do_install() {
    need_docker
    echo "==> 构建镜像 $IMAGE (首次约 3 分钟)"
    docker build -t "$IMAGE" -f "$SCRIPT_DIR/deploy/Dockerfile" "$SCRIPT_DIR"

    echo "==> 准备工作目录 $WORKSPACE"
    mkdir -p "$WORKSPACE"

    echo "==> 启动容器 $CONTAINER"
    run_container
    wait_ready

    echo "==> 安装宿主机命令 $BIN_LINK"
    install_wrapper

    cat <<EOF

  ✓ 安装完成
  宿主机命令: pitr <cmd>
  工作目录:   $WORKSPACE   (容器内 /workspace)

  快速试用:
    mkdir -p $WORKSPACE/demo
    cd $WORKSPACE/demo
    echo hi > $WORKSPACE/demo/a.txt
    pitr logs   . -n 5
    pitr config set history-limit 100
EOF
}

do_uninstall() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    detach_stale_fuse
    if [ "${1:-}" = "--purge" ]; then
        docker volume rm "$PG_VOLUME" "$DATA_VOLUME" >/dev/null 2>&1 || true
        echo "  数据卷已清理"
    fi
    local sudo; sudo=$(sudo_if_needed "$BIN_LINK")
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
    need_docker

    if docker inspect "$CONTAINER" >/dev/null 2>&1; then
        state=$(docker inspect -f '{{.State.Status}}' "$CONTAINER")
        if [ "$state" != "running" ]; then
            echo "==> 容器状态 $state, 启动..."
            detach_stale_fuse
            docker start "$CONTAINER" >/dev/null
        else
            echo "==> 容器已在运行"
        fi
    else
        docker volume inspect "$PG_VOLUME" >/dev/null 2>&1 \
            || { echo "错误: $PG_VOLUME volume 不存在, 无法 recover; 请用 $0 install" >&2; exit 1; }
        mkdir -p "$WORKSPACE"
        echo "==> 容器不存在, 复用 volume 重建..."
        run_container
    fi

    wait_ready

    if [ ! -x "$BIN_LINK" ]; then
        install_wrapper
    fi

    # daemon 层只校验既有 JuiceFS 元数据和双层 mount,绝不 format。
    docker exec "$CONTAINER" pitr recover
    echo "  ✓ recover 完成"
}

case "${1:-install}" in
    install)   do_install ;;
    recover)   do_recover ;;
    uninstall) do_uninstall "${2:-}" ;;
    status)    do_status ;;
    -h|--help) usage ;;
    *)         usage; exit 1 ;;
esac
