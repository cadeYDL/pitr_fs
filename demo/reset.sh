#!/usr/bin/env bash
# 把 demo 环境彻底清理, 回到跑 README §1 之前的状态.
# 覆盖: JuiceFS umount / PG 容器 / 数据目录 / 挂载点.
# 与 README §10 的清理步骤等价, 但可直接执行.
#
# 用法:
#   ./reset.sh              # 用 demo README 默认路径
#   MNT=/x DATA_DIR=/y PG_CONTAINER=zzz ./reset.sh
set -euo pipefail

DATA_DIR="${DATA_DIR:-/tmp/pitr-demo-data}"
MNT="${MNT:-/tmp/pitr-demo-mnt}"
PG_CONTAINER="${PG_CONTAINER:-pitr-demo-pg}"

echo "== umount JuiceFS ($MNT) =="
if mountpoint -q "$MNT" 2>/dev/null; then
    juicefs umount "$MNT" || fusermount -u "$MNT" 2>/dev/null || umount "$MNT"
else
    echo "  (未挂载, 跳过)"
fi

echo "== 删 PG 容器 ($PG_CONTAINER) =="
if docker ps -a --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
    docker rm -f "$PG_CONTAINER" >/dev/null
    echo "  已删"
else
    echo "  (无同名容器, 跳过)"
fi

echo "== 删数据目录 & 挂载点 =="
rm -rf "$DATA_DIR" "$MNT"

echo "== done, 可以从 README §1 重跑 =="
