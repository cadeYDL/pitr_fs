#!/usr/bin/env bash
# 在 calw 内直接执行(前提: 已用 orb shell calw 进入 orb).
# 编译 -N -l 无优化二进制, 启动 dlv headless 供 GoLand 的 Go Remote 连接.
#
# 用法:
#   ./remote-debug.sh ./cmd/xxx [-- args...]
#   ./remote-debug.sh -p 4000 ./cmd/xxx -- --config demo.yaml
#
# GoLand 侧:
#   Run > Edit Configurations > + > Go Remote
#     Host = calw.orb.local   Port = 2345
#   打断点 → 本脚本启动后, 点 Debug 连过来.
set -euo pipefail

[[ "$(uname)" == "Linux" ]] || {
    echo "此脚本应在 calw (linux) 里跑, 先: orb shell calw" >&2
    exit 1
}

PORT="${DLV_PORT:-2345}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--port) PORT="$2"; shift 2 ;;
        --) shift; break ;;
        -*) echo "未知参数: $1" >&2; exit 1 ;;
        *) break ;;
    esac
done

PKG="${1:?用法: $0 [-p PORT] <package-path> [-- args...]}"
shift || true
[[ "${1:-}" == "--" ]] && shift

OUT="/tmp/pitr-debug-bin"

echo ">> 编译 $PKG (-N -l 关优化)"
go build -o "$OUT" -gcflags='all=-N -l' "$PKG"
echo ">> dlv listen on :$PORT (accept-multiclient)"
exec dlv --listen=:$PORT --headless=true --api-version=2 --accept-multiclient \
     exec "$OUT" -- "$@"
