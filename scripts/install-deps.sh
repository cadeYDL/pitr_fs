#!/usr/bin/env bash
# 安装 pitr-fs 在 Linux 宿主机所需的运行依赖。
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
    echo "错误: pitr-fs 及本依赖脚本仅支持 Linux" >&2
    exit 1
fi

run_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif command -v sudo >/dev/null 2>&1; then
        sudo "$@"
    else
        echo "错误: 安装依赖需要 root 或 sudo" >&2
        exit 1
    fi
}

check_dependencies() {
    local failed=0 command_name
    for command_name in docker fusermount3 findmnt realpath curl git; do
        if ! command -v "$command_name" >/dev/null 2>&1; then
            echo "缺少: $command_name" >&2
            failed=1
        fi
    done
    if [ ! -e /dev/fuse ]; then
        echo "缺少: /dev/fuse" >&2
        failed=1
    fi
    if command -v docker >/dev/null 2>&1 && ! docker info >/dev/null 2>&1; then
        echo "不可用: Docker daemon" >&2
        failed=1
    fi
    [ "$failed" -eq 0 ] || return 1
    echo "pitr-fs Linux 宿主机依赖检查通过"
}

if [ "${1:-}" = "--check" ]; then
    check_dependencies
    exit
fi
if [ "$#" -ne 0 ]; then
    echo "用法: $0 [--check]" >&2
    exit 1
fi

if command -v apt-get >/dev/null 2>&1; then
    run_root apt-get update
    run_root apt-get install -y docker.io fuse3 util-linux ca-certificates curl git
elif command -v dnf >/dev/null 2>&1; then
    run_root dnf install -y docker fuse3 util-linux ca-certificates curl git
elif command -v yum >/dev/null 2>&1; then
    run_root yum install -y docker fuse3 util-linux ca-certificates curl git
elif command -v pacman >/dev/null 2>&1; then
    run_root pacman -Sy --needed --noconfirm docker fuse3 util-linux ca-certificates curl git
elif command -v zypper >/dev/null 2>&1; then
    run_root zypper --non-interactive install docker fuse3 util-linux ca-certificates curl git
else
    echo "错误: 不支持当前 Linux 发行版的包管理器，请手动安装 Docker、FUSE3、util-linux、curl、git" >&2
    exit 1
fi

if command -v modprobe >/dev/null 2>&1; then
    run_root modprobe fuse || true
fi
if command -v systemctl >/dev/null 2>&1; then
    run_root systemctl enable --now docker
else
    echo "提示: 当前系统不使用 systemd，请自行启动 Docker daemon"
fi

check_dependencies
