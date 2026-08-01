#!/usr/bin/env bash
# pitr-fs 卸载入口。必须 source，使脚本能清理当前 Bash 的命令路径缓存。

if [ -z "${BASH_VERSION:-}" ]; then
    echo "错误: uninstall.sh 当前仅支持在 Bash 中 source" >&2
    return 2 2>/dev/null || exit 2
fi

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    echo "错误: 请使用 source ./uninstall.sh [--purge]，以便自动清理当前 Bash 的命令缓存" >&2
    exit 2
fi

_pitr_uninstall_source=${BASH_SOURCE[0]}
_pitr_uninstall_dir=$(cd "$(dirname "$_pitr_uninstall_source")" && pwd)

pitr_uninstall_main() (
    set -euo pipefail
    # 在子 Shell 中复用安装器的宿主环境、Docker、FUSE 与持久化配置函数，
    # 避免改变用户当前 Shell 的 errexit/nounset/pipefail 选项。
    # shellcheck disable=SC1090
    source "$_pitr_uninstall_dir/install.sh"

    case "${1:-}" in
        "") purge=0 ;;
        --purge) purge=1 ;;
        -h|--help)
            cat <<'EOF'
用法: source ./uninstall.sh [--purge]
  无参数    删除服务、挂载和 pitr 命令，保留数据、依赖和安装配置
  --purge   永久删除数据，并清理由 pitr-fs 安装的宿主依赖

必须使用 source，让卸载脚本在成功后自动清理当前 Bash 的命令缓存。
EOF
            return 0
            ;;
        *)
            echo "错误: 用法: source ./uninstall.sh [--purge]" >&2
            return 2
            ;;
    esac
    [ "$#" -le 1 ] || {
        echo "错误: 用法: source ./uninstall.sh [--purge]" >&2
        return 2
    }

    require_linux
    validate_mount_root
    configure_docker
    detach_stale_fuse
    if docker_cli_timeout 10 inspect "$CONTAINER" >/dev/null 2>&1; then
        docker_cli_timeout 30 rm -f "$CONTAINER" >/dev/null 2>&1 || {
            echo "错误: 服务未能在 30 秒内停止；数据卷和 pitr 命令均未删除" >&2
            return 1
        }
    fi
    if [ "$purge" -ne 0 ]; then
        docker_cli volume rm "$PG_VOLUME" "$DATA_VOLUME" >/dev/null 2>&1 || true
        echo "  数据卷已清理"
        if [ -n "$BLOCK_PATH" ]; then
            echo "  用户块存储目录未删除: $BLOCK_PATH"
        fi
    fi
    if [ "$CACHE_VOLUME_MANAGED" = "1" ]; then
        docker_cli volume rm "$CACHE_VOLUME" >/dev/null 2>&1 || true
    elif docker_cli volume inspect "$CACHE_VOLUME" >/dev/null 2>&1; then
        echo "  用户已有缓存卷未删除: $CACHE_VOLUME"
    fi
    local sudo
    sudo=$(sudo_if_needed "$BIN_LINK")
    $sudo rm -f "$BIN_LINK"
    if [ "$purge" -ne 0 ]; then
        bash "$SCRIPT_DIR/scripts/install-deps.sh" --uninstall
        sudo=$(sudo_if_needed "$INSTALL_CONFIG")
        $sudo rm -f "$INSTALL_CONFIG"
        $sudo rmdir "$(dirname "$INSTALL_CONFIG")" 2>/dev/null || true
    fi
    echo "  ✓ 已卸载"
)

_pitr_uninstall_rc=0
pitr_uninstall_main "$@" || _pitr_uninstall_rc=$?

# 即使彻底卸载因外部 Docker 资源保护而未完成，pitr wrapper 也可能已经删除；
# 无条件刷新是安全的，并能避免 Bash 继续命中已不存在的旧路径。
hash -r
if [ "$_pitr_uninstall_rc" -eq 0 ]; then
    unset -f pitr_uninstall_main
    unset _pitr_uninstall_source _pitr_uninstall_dir _pitr_uninstall_rc
    return 0
fi

echo "卸载未完成；已刷新当前 Bash 的命令缓存，请处理上述错误后重试" >&2
unset -f pitr_uninstall_main
unset _pitr_uninstall_source _pitr_uninstall_dir
unset _pitr_uninstall_rc
return 1
