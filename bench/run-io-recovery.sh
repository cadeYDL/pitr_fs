#!/usr/bin/env bash
# 在任意已安装并完成 pitr init 的 Linux 主机上运行 I/O 与恢复基准。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PITR_CLI="${PITR_BIN:-pitr}"
ROUNDS="${PITR_BENCH_ROUNDS:-3}"
IMAGE="${PITR_BENCH_FIO_IMAGE:-pitrfs-bench-fio:local}"
KEEP_DATA="${PITR_BENCH_KEEP_DATA:-0}"
SKIP_IO="${PITR_BENCH_SKIP_IO:-0}"
OUTPUT="${PITR_BENCH_OUTPUT:-${TMPDIR:-/tmp}/pitrfs-bench-results/$(date +%Y%m%d-%H%M%S)}"
NATIVE_ROOT="${PITR_BENCH_NATIVE_ROOT:-/var/tmp}"
MOUNT="${1:-${PITR_BENCH_MOUNT:-}}"
DOCKER_COMMAND=(docker)
CONTAINER=""
NATIVE_DIR=""
BENCH_DIR=""

usage() {
    cat <<EOF
用法: $0 <已完成 pitr init 的挂载点>

环境变量:
  PITR_BIN                  pitr 命令路径，默认 pitr
  PITR_BENCH_ROUNDS         测试轮数，默认 3
  PITR_BENCH_OUTPUT         结果目录，默认 /tmp/pitrfs-bench-results/<时间>
  PITR_BENCH_NATIVE_ROOT    普通文件系统测试目录的父目录，默认 /var/tmp
  PITR_BENCH_FIO_IMAGE      自动构建的 fio 镜像名
  PITR_BENCH_KEEP_DATA=1    保留挂载点和普通盘中的测试文件
  PITR_BENCH_SKIP_IO=1      复用输出目录的 io-median.json，只续跑恢复测试

脚本只支持 Linux。它会自动构建 fio 容器，不会安装或升级宿主机软件。
EOF
}

die() {
    echo "错误: $*" >&2
    exit 1
}

docker_cli() {
    "${DOCKER_COMMAND[@]}" "$@"
}

cleanup() {
    local exit_code=$?
    trap - EXIT INT TERM
    if [ -n "$CONTAINER" ]; then
        docker_cli rm -f "$CONTAINER" >/dev/null 2>&1 || true
    fi
    if [ "$KEEP_DATA" != "1" ]; then
        if [ -n "$BENCH_DIR" ] && [ -d "$BENCH_DIR" ]; then
            rm -rf -- "$BENCH_DIR" >/dev/null 2>&1 ||
                echo "警告: 测试目录未完全删除: $BENCH_DIR" >&2
        fi
        if [ -n "$NATIVE_DIR" ] && [ -d "$NATIVE_DIR" ]; then
            rm -rf -- "$NATIVE_DIR" || true
        fi
    elif [ -n "$BENCH_DIR" ]; then
        echo "测试数据已保留: $BENCH_DIR"
        echo "普通盘数据已保留: $NATIVE_DIR"
    fi
    exit "$exit_code"
}
trap cleanup EXIT INT TERM

[ "${1:-}" != "--help" ] && [ "${1:-}" != "-h" ] || { usage; exit 0; }
[ "$(uname -s)" = "Linux" ] || die "基准测试仅支持 Linux"
[ -n "$MOUNT" ] || { usage >&2; exit 2; }
case "$ROUNDS" in
    ''|*[!0-9]*|0) die "PITR_BENCH_ROUNDS 必须是正整数" ;;
esac
case "$SKIP_IO" in
    0|1) ;;
    *) die "PITR_BENCH_SKIP_IO 只能是 0 或 1" ;;
esac
command -v "$PITR_CLI" >/dev/null 2>&1 || die "找不到 pitr 命令: $PITR_CLI"
command -v python3 >/dev/null 2>&1 || die "缺少 python3，请先运行项目 install.sh"
command -v findmnt >/dev/null 2>&1 || die "缺少 findmnt，请先运行项目 install.sh"
command -v realpath >/dev/null 2>&1 || die "缺少 realpath，请先运行项目 install.sh"

if timeout 10 docker info >/dev/null 2>&1; then
    DOCKER_COMMAND=(docker)
elif command -v sudo >/dev/null 2>&1 && timeout 10 sudo docker info >/dev/null 2>&1; then
    DOCKER_COMMAND=(sudo docker)
else
    die "Docker 未运行或当前用户无权限，请先运行项目 install.sh"
fi

MOUNT="$(realpath -e -- "$MOUNT")"
[ -d "$MOUNT" ] || die "挂载点不存在: $MOUNT"
FSTYPE="$(findmnt -T "$MOUNT" -n -o FSTYPE 2>/dev/null || true)"
[ "$FSTYPE" = "fuse.pitrfs" ] || die "$MOUNT 不是 pitrfs 挂载点（检测到 ${FSTYPE:-未知文件系统}）"
"$PITR_CLI" status >/dev/null || die "pitr 服务未就绪"

[ -d "$NATIVE_ROOT" ] && [ -w "$NATIVE_ROOT" ] || die "普通盘父目录不可写: $NATIVE_ROOT"
NATIVE_ROOT="$(realpath -e -- "$NATIVE_ROOT")"
NATIVE_DIR="$(mktemp -d "$NATIVE_ROOT/pitrfs-native.XXXXXX")"
RUN_ID="$(date +%Y%m%d%H%M%S)-$$"
BENCH_DIR="$MOUNT/__pitr_bench_$RUN_ID"
mkdir -p "$BENCH_DIR"
OUTPUT="$(realpath -m -- "$OUTPUT")"
mkdir -p "$OUTPUT"

echo "==> 构建/复用 fio 镜像: $IMAGE"
docker_cli build -q -f "$SCRIPT_DIR/Dockerfile.fio" -t "$IMAGE" "$SCRIPT_DIR" >/dev/null

CONTAINER="pitrfs-fio-${RUN_ID//[^a-zA-Z0-9_.-]/-}"
echo "==> 启动临时 fio 容器: $CONTAINER"
docker_cli run -d --name "$CONTAINER" --privileged --pid=host \
    -v "$NATIVE_DIR:/native" \
    -v "$BENCH_DIR:/pitr" \
    "$IMAGE" >/dev/null

printf -v DOCKER_COMMAND_STRING '%q ' "${DOCKER_COMMAND[@]}"
DOCKER_COMMAND_STRING="${DOCKER_COMMAND_STRING% }"
echo "==> 运行 ${ROUNDS} 轮 I/O 与恢复基准"
PYTHON_ARGS=(
    "$SCRIPT_DIR/bench-io-recovery.py"
    --container "$CONTAINER"
    --docker-command "$DOCKER_COMMAND_STRING"
    --rounds "$ROUNDS"
    --output "$OUTPUT"
    --native-path "$NATIVE_DIR"
    --recovery-root "$BENCH_DIR/recovery"
    --pitr-cli "$PITR_CLI"
    --pitr-mount "$MOUNT"
)
if [ "$KEEP_DATA" != "1" ]; then
    PYTHON_ARGS+=(--cleanup-root "$BENCH_DIR")
fi
if [ "$SKIP_IO" = "1" ]; then
    PYTHON_ARGS+=(--skip-io)
fi
python3 "${PYTHON_ARGS[@]}"

echo "==> 测试通过"
echo "结果目录: $OUTPUT"
