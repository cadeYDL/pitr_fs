#!/usr/bin/env bash
# Linux 宿主机逻辑升级控制器。只切换 pitr/pitrd/schema，不重建容器或数据卷。
set -euo pipefail

INSTALL_CONFIG=${PITR_INSTALL_CONFIG:-/etc/pitr-fs/install.conf}
if [ ! -r "$INSTALL_CONFIG" ]; then
    echo "错误: 未找到安装配置 $INSTALL_CONFIG；请先安装 pitr-fs" >&2
    exit 1
fi
# install.conf 由安装器以 root:root 0644 写入，只含 %q 转义的非敏感参数。
# shellcheck disable=SC1090
source "$INSTALL_CONFIG"

CONTAINER=${PITR_CONTAINER:-${SAVED_CONTAINER:-pitrfs}}
RUNTIME_DIR=${PITR_RUNTIME_DIR:-${SAVED_RUNTIME_DIR:-/var/lib/pitr-fs/runtime}}
HOST_UPGRADER=${PITR_HOST_UPGRADER:-${SAVED_HOST_UPGRADER:-/usr/local/lib/pitr-fs/pitr-host-upgrade}}
UPDATE_REPOSITORY=${PITR_UPDATE_REPOSITORY:-${SAVED_UPDATE_REPOSITORY:-cadeYDL/pitr_fs}}
UPDATE_API_URL=${PITR_UPDATE_API_URL:-${SAVED_UPDATE_API_URL:-https://api.github.com}}
READY_TIMEOUT=${PITR_READY_TIMEOUT:-120}
DOCKER_COMMAND=(docker)

usage() {
    cat <<'EOF'
用法:
  pitr upgrade [版本] [--check] [--yes]
  pitr upgrade --bundle <升级包.tar.gz> [--check] [--yes]
  pitr upgrade --rollback [--yes]

选项:
  版本           下载指定 GitHub Release；省略时下载最新已发布版本
  --bundle PATH  使用本地、带 SHA256 校验的逻辑升级包
  --check        只校验升级包，不停止服务
  --rollback     回退到上一个逻辑版本
  --yes          非交互确认服务中断
  -h, --help     显示帮助

dev/test Pre-release 与正式版本使用不同版本号，但都属于“已发布版本”。
EOF
}

run_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif command -v sudo >/dev/null 2>&1; then
        sudo "$@"
    else
        echo "错误: 逻辑升级需要 root 或 sudo 写入 $RUNTIME_DIR" >&2
        return 1
    fi
}

configure_docker() {
    if timeout 10 docker info >/dev/null 2>&1; then
        DOCKER_COMMAND=(docker)
    elif command -v sudo >/dev/null 2>&1 \
        && timeout 10 sudo docker info >/dev/null 2>&1; then
        DOCKER_COMMAND=(sudo docker)
    else
        echo "错误: Docker 服务未运行或当前用户无访问权限" >&2
        return 1
    fi
}

docker_cli() {
    "${DOCKER_COMMAND[@]}" "$@"
}

validate_bundle() {
    local bundle=$1 work=$2 listed expected version binary_version
    [ -f "$bundle" ] || { echo "错误: 升级包不存在: $bundle" >&2; return 1; }
    listed=$(tar -tzf "$bundle" | LC_ALL=C sort)
    expected=$(printf '%s\n' BUILD-INFO SHA256SUMS VERSION init_pitr.sql \
        pitr pitr-host-upgrade pitrd |
        LC_ALL=C sort)
    [ "$listed" = "$expected" ] || {
        echo "错误: 升级包只能包含 pitr、pitrd、宿主升级器、schema 和校验清单" >&2
        return 1
    }
    if tar -tvzf "$bundle" | awk '$1 !~ /^-/ { exit 1 }'; then
        :
    else
        echo "错误: 升级包包含非普通文件" >&2
        return 1
    fi
    tar -xzf "$bundle" -C "$work" --no-same-owner --no-same-permissions -- \
        pitr pitrd pitr-host-upgrade init_pitr.sql VERSION BUILD-INFO SHA256SUMS
    (cd "$work" && sha256sum -c SHA256SUMS >/dev/null) || {
        echo "错误: 升级包 SHA256 校验失败" >&2
        return 1
    }
    [ "$(wc -l <"$work/VERSION")" -eq 1 ] || {
        echo "错误: VERSION 格式无效" >&2
        return 1
    }
    version=$(cat "$work/VERSION")
    case "$version" in
        ''|*[!A-Za-z0-9._+-]*) echo "错误: VERSION 包含非法字符" >&2; return 1 ;;
    esac
    chmod 0755 "$work/pitr" "$work/pitrd" "$work/pitr-host-upgrade"
    binary_version=$("$work/pitr" version --client-only |
        awk '$1=="pitr" { print $2; exit }')
    [ "$binary_version" = "$version" ] || {
        echo "错误: VERSION=$version，但 pitr 二进制版本为 $binary_version" >&2
        return 1
    }
    printf '%s\n' "$version"
}

release_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *) echo "错误: 暂不支持当前 CPU 架构: $(uname -m)" >&2; return 1 ;;
    esac
}

# 输出三行: tag、asset URL、GitHub 计算的 sha256 摘要。
release_asset_from_json() {
    local metadata=$1 requested=$2 arch=$3
    python3 - "$metadata" "$requested" "$arch" <<'PY'
import json
import re
import sys

path, requested, arch = sys.argv[1:]
with open(path, "r", encoding="utf-8") as handle:
    payload = json.load(handle)

if isinstance(payload, list):
    releases = [release for release in payload if not release.get("draft")]
    if not releases:
        raise SystemExit("错误: 仓库尚无已发布版本")
    release = max(releases, key=lambda item: item.get("published_at") or "")
elif isinstance(payload, dict) and payload.get("tag_name"):
    release = payload
else:
    message = payload.get("message", "响应格式无效") if isinstance(payload, dict) else "响应格式无效"
    raise SystemExit(f"错误: 无法解析 GitHub Release: {message}")

tag = release.get("tag_name", "")
if requested and tag != requested:
    raise SystemExit(f"错误: 请求版本 {requested}，GitHub 返回 {tag or '<空>'}")
if not re.fullmatch(r"[A-Za-z0-9._+-]+", tag):
    raise SystemExit(f"错误: Release 版本号包含非法字符: {tag!r}")

expected = f"pitr-fs_{tag}_linux_{arch}.tar.gz"
asset = next((item for item in release.get("assets", [])
              if item.get("name") == expected and item.get("state") == "uploaded"), None)
if asset is None:
    raise SystemExit(f"错误: Release {tag} 缺少 {expected}")
url = asset.get("browser_download_url", "")
digest = asset.get("digest") or ""
if not url.startswith("https://"):
    raise SystemExit("错误: Release 资产下载地址不是 HTTPS")
if not re.fullmatch(r"sha256:[0-9a-fA-F]{64}", digest):
    raise SystemExit(f"错误: Release 资产 {expected} 缺少 GitHub SHA256 摘要")
print(tag)
print(url)
print(digest.lower())
PY
}

download_release_bundle() {
    local requested=$1 work=$2 arch metadata endpoint asset_url digest tag bundle actual
    local auth_config
    local -a curl_headers curl_retry release_info
    arch=$(release_arch) || return 1
    metadata="$work/release.json"
    if [[ ! "$UPDATE_REPOSITORY" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
        echo "错误: PITR_UPDATE_REPOSITORY 必须是 owner/repo: $UPDATE_REPOSITORY" >&2
        return 1
    fi
    case "$UPDATE_API_URL" in
        https://*) ;;
        *) echo "错误: PITR_UPDATE_API_URL 必须使用 HTTPS: $UPDATE_API_URL" >&2; return 1 ;;
    esac
    if [ -n "$requested" ]; then
        endpoint="$UPDATE_API_URL/repos/$UPDATE_REPOSITORY/releases/tags/$requested"
    else
        # /releases/latest 会排除 Pre-release；这里按 published_at 选最新已发布项。
        endpoint="$UPDATE_API_URL/repos/$UPDATE_REPOSITORY/releases?per_page=100"
    fi
    curl_headers=(-H 'Accept: application/vnd.github+json' \
        -H 'X-GitHub-Api-Version: 2022-11-28')
    # Release 下载经过公网，连接重置、TLS 提前断开等瞬时错误不应让升级直接失败。
    # 不设置总传输超时，避免大包或慢链路被误杀；重试次数和等待时间保持有界。
    curl_retry=(--retry 4 --retry-delay 1 --retry-max-time 60 \
        --retry-all-errors --connect-timeout 15)
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        if [[ ! "$GITHUB_TOKEN" =~ ^[A-Za-z0-9_]+$ ]]; then
            echo "错误: GITHUB_TOKEN 格式无效" >&2
            return 1
        fi
        auth_config="$work/curl-auth.conf"
        (umask 077; printf 'header = "Authorization: Bearer %s"\n' \
            "$GITHUB_TOKEN" >"$auth_config")
        curl_headers+=(--config "$auth_config")
    fi
    if ! curl --fail --silent --show-error --location \
        --proto '=https' --tlsv1.2 "${curl_retry[@]}" "${curl_headers[@]}" \
        -o "$metadata" "$endpoint"; then
        echo "错误: 获取 GitHub Release 失败: ${requested:-latest}" >&2
        return 1
    fi
    mapfile -t release_info < <(
        release_asset_from_json "$metadata" "$requested" "$arch"
    )
    [ "${#release_info[@]}" -eq 3 ] || return 1
    tag=${release_info[0]}
    asset_url=${release_info[1]}
    digest=${release_info[2]}
    bundle="$work/pitr-fs_${tag}_linux_${arch}.tar.gz"
    if ! curl --fail --silent --show-error --location \
        --proto '=https' --tlsv1.2 "${curl_retry[@]}" "${curl_headers[@]}" \
        -o "$bundle" "$asset_url"; then
        echo "错误: 下载 Release 资产失败: $asset_url" >&2
        return 1
    fi
    actual=$(sha256sum "$bundle" | awk '{print $1}')
    if [ "$actual" != "${digest#sha256:}" ]; then
        echo "错误: GitHub Release 资产 SHA256 校验失败" >&2
        return 1
    fi
    echo "已下载 $UPDATE_REPOSITORY 的 ${tag} (linux/$arch)" >&2
    printf '%s\n' "$bundle"
}

confirm_downtime() {
    cat >&2 <<'EOF'
警告: 升级会先停止 pitr 文件系统服务并短暂中断挂载。
请确保当前没有任何写入操作；升级时仍未关闭的写入会整个撤销，
已写入的半成品将被丢弃。PostgreSQL、对象存储和容器不会停止。
EOF
    if [ "$assume_yes" -eq 1 ]; then
        return 0
    fi
    [ -t 0 ] || {
        echo "错误: 非交互升级必须显式指定 --yes" >&2
        return 1
    }
    read -r -p "确认停止文件系统服务并继续？[y/N] " answer
    case "$answer" in y|Y|yes|YES) ;; *) echo "已取消升级"; return 1 ;; esac
}

current_cli() {
    docker_cli exec "$CONTAINER" sh -c \
        'if [ -x /opt/pitr/current/pitr ]; then echo /opt/pitr/current/pitr; else echo /usr/local/bin/pitr; fi'
}

ensure_upgrade_capable() {
    docker_cli inspect "$CONTAINER" >/dev/null 2>&1 || {
        echo "错误: 服务容器 $CONTAINER 不存在" >&2
        return 1
    }
    docker_cli inspect -f '{{range .Mounts}}{{if eq .Destination "/opt/pitr"}}{{.Source}}{{end}}{{end}}' \
        "$CONTAINER" | grep -Fx "$RUNTIME_DIR" >/dev/null || {
        echo "错误: 当前安装尚未启用逻辑运行目录；请先用新版 install.sh install 迁移一次" >&2
        return 1
    }
    docker_cli exec "$CONTAINER" test -r /run/pitr/pitrd.pid || {
        echo "错误: 当前容器 entrypoint 不支持无重建逻辑升级" >&2
        return 1
    }
}

status_output() {
    local cli
    cli=$(current_cli)
    docker_cli exec "$CONTAINER" "$cli" status
}

daemon_version() {
    local cli
    cli=$(current_cli)
    docker_cli exec "$CONTAINER" "$cli" version 2>/dev/null |
        awk '$1=="pitrd" { print $2; exit }'
}

target_binary_version() {
    local target=$1 cli
    if [ "$target" = builtin ]; then
        cli=/usr/local/bin/pitr
    else
        cli="/opt/pitr/versions/$target/pitr"
    fi
    docker_cli exec "$CONTAINER" "$cli" version --client-only 2>/dev/null |
        awk '$1=="pitr" { print $2; exit }'
}

unmount_filesystem() {
    local status fuse cli
    status=$(status_output)
    fuse=$(printf '%s\n' "$status" | awk '
        /fuse=/ { for (i=1;i<=NF;i++) if ($i ~ /^fuse=/) { sub(/^fuse=/,"",$i); print $i; exit } }')
    [ -n "$fuse" ] || return 0
    cli=$(current_cli)
    if ! docker_cli exec "$CONTAINER" sh -c ': > /run/pitr/discard-open-writes'; then
        echo "错误: 无法创建升级写入丢弃标记" >&2
        return 1
    fi
    if ! docker_cli exec "$CONTAINER" "$cli" umount "$fuse" >/dev/null; then
        docker_cli exec "$CONTAINER" rm -f /run/pitr/discard-open-writes || true
        return 1
    fi
    docker_cli exec "$CONTAINER" rm -f /run/pitr/discard-open-writes
}

runtime_name() {
    local target
    target=$(readlink "$RUNTIME_DIR/current" 2>/dev/null || true)
    if [ -z "$target" ]; then
        echo builtin
    else
        basename "$target"
    fi
}

switch_runtime() {
    local target=$1 old=$2 temporary="$RUNTIME_DIR/.current.$$"
    if ! printf '%s\n' "$old" | run_root tee "$RUNTIME_DIR/previous" >/dev/null; then
        return 1
    fi
    if [ "$target" = builtin ]; then
        run_root rm -f "$RUNTIME_DIR/current"
    else
        run_root rm -f "$temporary"
        if ! run_root ln -s "versions/$target" "$temporary"; then
            return 1
        fi
        run_root mv -Tf "$temporary" "$RUNTIME_DIR/current"
    fi
}

apply_schema() {
    docker_cli exec "$CONTAINER" sh -c '
        schema=/etc/pitr/init_pitr.sql
        [ ! -r /opt/pitr/current/init_pitr.sql ] || schema=/opt/pitr/current/init_pitr.sql
        PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 \
          -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -f "$schema" >/dev/null
    '
}

current_schema_digest() {
    docker_cli exec "$CONTAINER" sh -c '
        schema=/etc/pitr/init_pitr.sql
        [ ! -r /opt/pitr/current/init_pitr.sql ] || schema=/opt/pitr/current/init_pitr.sql
        sha256sum "$schema"
    ' | awk '{print $1}'
}

target_schema_digest() {
    local target=$1
    if [ "$target" = builtin ]; then
        docker_cli exec "$CONTAINER" sha256sum /etc/pitr/init_pitr.sql |
            awk '{print $1}'
    else
        sha256sum "$RUNTIME_DIR/versions/$target/init_pitr.sql" |
            awk '{print $1}'
    fi
}

record_schema_digest() {
    local digest=$1
    printf '%s\n' "$digest" |
        run_root tee "$RUNTIME_DIR/schema.applied.sha256" >/dev/null
}

request_restart() {
    docker_cli exec "$CONTAINER" sh -c '
        test -r /run/pitr/pitrd.pid
        : > /run/pitr/restart.request
        kill -TERM "$(cat /run/pitr/pitrd.pid)"
    '
}

wait_version() {
    local expected=$1 cli output
    for _ in $(seq 1 "$READY_TIMEOUT"); do
        cli=$(current_cli 2>/dev/null || true)
        if [ -n "$cli" ]; then
            output=$(docker_cli exec "$CONTAINER" "$cli" version 2>/dev/null || true)
            if printf '%s\n' "$output" | grep -Fq "pitrd $expected "; then
                return 0
            fi
        fi
        sleep 1
    done
    return 1
}

recover_mount() {
    local cli
    cli=$(current_cli)
    docker_cli exec "$CONTAINER" "$cli" recover >/dev/null 2>&1 || true
}

install_version() {
    local work=$1 version=$2 destination
    destination="$RUNTIME_DIR/versions/$version"
    if [ -d "$destination" ]; then
        if [ "$(sha256sum "$work/SHA256SUMS" | awk '{print $1}')" != \
            "$(sha256sum "$destination/SHA256SUMS" | awk '{print $1}')" ]; then
            echo "错误: 逻辑版本 $version 已存在，但内容校验清单不同；版本号必须不可变" >&2
            return 1
        fi
    else
        local staging="$RUNTIME_DIR/versions/.${version}.$$"
        run_root install -d -m 0755 "$staging"
        run_root install -m 0755 "$work/pitr" "$work/pitrd" \
            "$work/pitr-host-upgrade" "$staging/"
        run_root install -m 0644 "$work/init_pitr.sql" "$work/VERSION" \
            "$work/BUILD-INFO" "$work/SHA256SUMS" "$staging/"
        run_root mv "$staging" "$destination"
    fi
    (cd "$destination" && sha256sum -c SHA256SUMS >/dev/null) || {
        echo "错误: 已安装版本 $version 的校验失败" >&2
        return 1
    }
}

perform_switch() {
    local target=$1 old=$2 expected=$3 old_expected=$4 old_schema target_schema
    old_schema=$(current_schema_digest) || {
        echo "错误: 无法计算当前 schema 摘要" >&2
        return 1
    }
    target_schema=$(target_schema_digest "$target") || {
        echo "错误: 无法计算目标 schema 摘要" >&2
        return 1
    }
    if ! unmount_filesystem; then
        echo "错误: 文件系统未能安全卸载，逻辑版本未切换" >&2
        return 1
    fi
    if ! printf '%s\n' "$old" |
        run_root tee "$RUNTIME_DIR/upgrade-fallback" >/dev/null; then
        recover_mount
        return 1
    fi
    if ! switch_runtime "$target" "$old"; then
        run_root rm -f "$RUNTIME_DIR/upgrade-fallback"
        recover_mount
        echo "错误: 无法切换逻辑运行目录，已恢复挂载" >&2
        return 1
    fi
    if [ "$old_schema" = "$target_schema" ]; then
        echo "schema 内容未变化，跳过索引/引用生命周期重校准"
    elif ! apply_schema; then
        if ! switch_runtime "$old" "$target"; then
            echo "严重错误: schema 失败且无法恢复旧逻辑目录" >&2
            return 1
        fi
        run_root rm -f "$RUNTIME_DIR/upgrade-fallback"
        recover_mount
        echo "错误: schema 校准失败，已恢复旧逻辑" >&2
        return 1
    fi
    if ! record_schema_digest "$target_schema"; then
        switch_runtime "$old" "$target" || true
        run_root rm -f "$RUNTIME_DIR/upgrade-fallback"
        recover_mount
        echo "错误: 无法持久化 schema 版本，已恢复旧逻辑" >&2
        return 1
    fi
    if ! request_restart; then
        switch_runtime "$old" "$target" || true
        run_root rm -f "$RUNTIME_DIR/upgrade-fallback"
        recover_mount
        echo "错误: 无法请求 pitrd 重启，已恢复旧逻辑" >&2
        return 1
    fi
    if wait_version "$expected"; then
        run_root rm -f "$RUNTIME_DIR/upgrade-fallback"
        recover_mount
        return 0
    fi

    echo "错误: 新版 pitrd 未通过健康检查，正在回退到 $old" >&2
    switch_runtime "$old" "$target"
    request_restart || true
    if ! wait_version "$old_expected"; then
        echo "严重错误: 自动回退后 pitrd 仍未恢复，请执行 ./install.sh recover" >&2
        return 1
    fi
    run_root rm -f "$RUNTIME_DIR/upgrade-fallback"
    recover_mount
    return 1
}

upgrade_main() (
    local bundle="" requested_version="" rollback=0 check_only=0 assume_yes=0
    local work version old old_expected expected
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --bundle)
                [ "$#" -ge 2 ] || { echo "错误: --bundle 缺少路径" >&2; return 2; }
                bundle=$2; shift 2 ;;
            --rollback) rollback=1; shift ;;
            --check) check_only=1; shift ;;
            --yes) assume_yes=1; shift ;;
            -h|--help) usage; return 0 ;;
            -*) echo "错误: 未知参数 $1" >&2; usage >&2; return 2 ;;
            *)
                [ -z "$requested_version" ] || {
                    echo "错误: 只能指定一个版本" >&2
                    return 2
                }
                requested_version=$1
                shift
                ;;
        esac
    done

    if [ "$rollback" -eq 1 ] && {
        [ -n "$bundle" ] || [ -n "$requested_version" ] || [ "$check_only" -eq 1 ];
    }; then
        echo "错误: --rollback 不能与版本、--bundle/--check 同时使用" >&2
        return 2
    fi
    if [ -n "$bundle" ] && [ -n "$requested_version" ]; then
        echo "错误: 指定版本不能与 --bundle 同时使用" >&2
        return 2
    fi
    if [ -n "$requested_version" ]; then
        case "$requested_version" in
            *[!A-Za-z0-9._+-]*)
                echo "错误: 版本号包含非法字符: $requested_version" >&2
                return 2
                ;;
        esac
    fi

    work=$(mktemp -d)
    trap 'rm -rf -- "$work"' EXIT
    if [ "$rollback" -eq 0 ]; then
        if [ -z "$bundle" ]; then
            bundle=$(download_release_bundle "$requested_version" "$work") || return 1
        fi
        version=$(validate_bundle "$bundle" "$work") || return 1
        if [ "$check_only" -eq 1 ]; then
            echo "升级包校验通过: $version"
            return 0
        fi
    fi

    configure_docker
    ensure_upgrade_capable
    run_root install -d -m 0755 "$RUNTIME_DIR" "$RUNTIME_DIR/versions"
    old=$(runtime_name)
    old_expected=$(daemon_version)
    [ -n "$old_expected" ] || {
        echo "错误: 无法读取当前 pitrd 版本" >&2
        return 1
    }

    if [ "$rollback" -eq 1 ]; then
        [ -r "$RUNTIME_DIR/previous" ] || {
            echo "错误: 没有可回退的上一个逻辑版本" >&2
            return 1
        }
        version=$(cat "$RUNTIME_DIR/previous")
        case "$version" in
            builtin) ;;
            ''|*[!A-Za-z0-9._+-]*|"$old")
                echo "错误: 回退目标无效: $version" >&2; return 1 ;;
        esac
    else
        [ "$version" != "$old" ] || {
            echo "当前已经是逻辑版本 $version"
            return 0
        }
        install_version "$work" "$version"
    fi

    expected=$(target_binary_version "$version")
    [ -n "$expected" ] || {
        echo "错误: 无法读取目标逻辑版本" >&2
        return 1
    }

    confirm_downtime
    perform_switch "$version" "$old" "$expected" "$old_expected"
    echo "逻辑版本已从 $old 切换到 $version；容器、PostgreSQL 和数据卷未重建"
)

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    upgrade_main "$@"
fi
