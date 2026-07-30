#!/usr/bin/env bash
# 从仓库权威 proto 重新生成 Python stub。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
PYTHON="${PYTHON:-python3}"

"$PYTHON" -m grpc_tools.protoc \
    -I "$REPO_ROOT/api/pitrd/v1" \
    --python_out="$SCRIPT_DIR/pitr/pb" \
    --grpc_python_out="$SCRIPT_DIR/pitr/pb" \
    "$REPO_ROOT/api/pitrd/v1/pitrd.proto"

# grpc_tools 对无 package 目录的输出使用绝对 import；SDK 包内需相对 import。
sed -i 's/^import pitrd_pb2 as pitrd__pb2$/from . import pitrd_pb2 as pitrd__pb2/' \
    "$SCRIPT_DIR/pitr/pb/pitrd_pb2_grpc.py"
