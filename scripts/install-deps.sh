#!/usr/bin/env bash
# 安装 pitr-fs 在 Linux 宿主机所需的运行依赖。
set -euo pipefail

STATE_DIR="${PITR_STATE_DIR:-/var/lib/pitr-fs}"
STATE_FILE="$STATE_DIR/host-install.state"

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

ensure_state_file() {
    if [ ! -e "$STATE_FILE" ]; then
        run_root install -d -m 0755 "$STATE_DIR"
        printf 'format=1\n' | run_root tee "$STATE_FILE" >/dev/null
        run_root chmod 0644 "$STATE_FILE"
    fi
}

state_append() {
    local line=$1
    ensure_state_file
    if ! grep -Fqx -- "$line" "$STATE_FILE" 2>/dev/null; then
        printf '%s\n' "$line" | run_root tee -a "$STATE_FILE" >/dev/null
    fi
}

state_values() {
    local key=$1
    [ -r "$STATE_FILE" ] || return 0
    awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print }' "$STATE_FILE"
}

docker_daemon_available() {
    timeout 10 docker info >/dev/null 2>&1 && return 0
    [ "$(id -u)" -eq 0 ] && return 1
    if command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
        timeout 10 sudo docker info >/dev/null 2>&1
    elif command -v sudo >/dev/null 2>&1 && [ -t 0 ]; then
        timeout 10 sudo docker info >/dev/null 2>&1
    else
        return 1
    fi
}

run_docker() {
    if timeout 10 docker info >/dev/null 2>&1; then
        docker "$@"
    elif [ "$(id -u)" -eq 0 ]; then
        docker "$@"
    elif command -v sudo >/dev/null 2>&1 \
        && timeout 10 sudo -n docker info >/dev/null 2>&1; then
        sudo docker "$@"
    else
        echo "错误: 无法访问 Docker，不能维护安装对象清单" >&2
        return 1
    fi
}

docker_snapshot_before() {
    if state_values docker_snapshot_started | grep -qx 1; then
        return 0
    fi
    state_append "docker_snapshot_started=1"
    while read -r image_id; do
        [ -n "$image_id" ] && state_append "docker_baseline_image=$image_id"
    done < <(run_docker images -aq --no-trunc | sort -u)
}

docker_snapshot_after() {
    local image_name=${1:-pitr-fs:latest} image_id baseline=0
    if ! state_values docker_snapshot_completed | grep -qx 1; then
        while read -r image_id; do
            [ -n "$image_id" ] || continue
            baseline=0
            while read -r existing; do
                [ "$existing" = "$image_id" ] && baseline=1
            done < <(state_values docker_baseline_image)
            [ "$baseline" -ne 0 ] || state_append "docker_image=$image_id"
        done < <(run_docker images -aq --no-trunc | sort -u)
        state_append "docker_snapshot_completed=1"
        return 0
    fi
    image_id=$(run_docker image inspect -f '{{.Id}}' "$image_name")
    if ! state_values docker_baseline_image | grep -Fqx "$image_id"; then
        state_append "docker_image=$image_id"
    fi
}

check_dependencies() {
    local failed=0 command_name
    for command_name in docker fusermount3 findmnt mountpoint realpath curl git python3 awk tar sha256sum; do
        if ! command -v "$command_name" >/dev/null 2>&1; then
            echo "缺少: $command_name" >&2
            failed=1
        fi
    done
    if [ ! -e /dev/fuse ]; then
        echo "缺少: /dev/fuse" >&2
        failed=1
    fi
    if command -v docker >/dev/null 2>&1 && ! docker_daemon_available; then
        echo "不可用: Docker daemon 或当前用户无访问权限" >&2
        failed=1
    fi
    [ "$failed" -eq 0 ] || return 1
    echo "pitr-fs Linux 宿主机依赖检查通过"
}

if [ "${1:-}" = "--check" ]; then
    check_dependencies
    exit
fi

uninstall_managed_dependencies() {
    if [ ! -r "$STATE_FILE" ]; then
        echo "没有由 pitr-fs 安装的宿主机依赖"
        return 0
    fi

    local manager simulated package removed external=0 image_id object
    local docker_owned=0
    local -a owned_packages group_users managed_images baseline_images current_images
    manager=$(state_values manager | tail -1)
    mapfile -t owned_packages < <(state_values package)
    mapfile -t group_users < <(state_values docker_group_user)
    mapfile -t managed_images < <(state_values docker_image)
    mapfile -t baseline_images < <(state_values docker_baseline_image)

    for package in "${owned_packages[@]}"; do
        case "$package" in docker|docker.io|docker-ce) docker_owned=1 ;; esac
    done

    if [ "${#managed_images[@]}" -gt 0 ] && command -v docker >/dev/null 2>&1; then
        if [ "$docker_owned" -ne 0 ]; then
            mapfile -t current_images < <(run_docker images -aq --no-trunc | sort -u)
            for image_id in "${current_images[@]}"; do
                external=1
                for object in "${managed_images[@]}" "${baseline_images[@]}"; do
                    if [ "$object" = "$image_id" ]; then external=0; break; fi
                done
                if [ "$external" -ne 0 ]; then
                    echo "错误: Docker 中存在非 pitr-fs 管理的镜像 $image_id；保留 Docker 与安装清单" >&2
                    return 1
                fi
            done
            if [ -n "$(run_docker ps -aq)" ]; then
                echo "错误: Docker 中存在非 pitr-fs 容器；保留 Docker 与安装清单" >&2
                return 1
            fi
            if [ -n "$(run_docker volume ls -q)" ]; then
                echo "错误: Docker 中存在非 pitr-fs 数据卷；保留 Docker 与安装清单" >&2
                return 1
            fi
            if run_docker network ls --filter type=custom -q | grep -q .; then
                echo "错误: Docker 中存在非 pitr-fs 自定义网络；保留 Docker 与安装清单" >&2
                return 1
            fi
        else
            echo "==> 删除 pitr-fs 新增的 Docker 镜像"
            run_docker image rm "${managed_images[@]}" >/dev/null || {
                echo "错误: pitr-fs 镜像仍被其他 Docker 对象引用；保留安装清单" >&2
                return 1
            }
        fi
    fi

    if [ "${#owned_packages[@]}" -gt 0 ]; then
        echo "==> 卸载 pitr-fs 曾安装的软件包: ${owned_packages[*]}"
        if [ "$docker_owned" -ne 0 ] && command -v systemctl >/dev/null 2>&1; then
            # 先停止 socket unit，再卸载 Docker 包。否则 systemd 可能继续持有旧的
            # /run/docker.sock；同机重新安装时 dockerd 会因没有有效 socket activation
            # fd 而启动失败。
            run_root systemctl stop docker.service docker.socket 2>/dev/null || true
            if ! systemctl is-active --quiet docker.socket; then
                run_root rm -f /run/docker.sock
            fi
        fi
        case "$manager" in
            apt-get)
                simulated=$(run_root apt-get -s purge "${owned_packages[@]}")
                while read -r removed; do
                    [ -n "$removed" ] || continue
                    external=1
                    for package in "${owned_packages[@]}"; do
                        if [ "$package" = "$removed" ]; then
                            external=0
                            break
                        fi
                    done
                    if [ "$external" -ne 0 ]; then
                        echo "错误: 卸载依赖还会删除后来安装的外部软件包 $removed，已停止清理" >&2
                        return 1
                    fi
                done < <(printf '%s\n' "$simulated" | awk '/^Remv / { print $2 }')
                run_root env DEBIAN_FRONTEND=noninteractive apt-get purge -y \
                    "${owned_packages[@]}"
                ;;
            dnf|yum|zypper)
                # rpm 会在仍有外部软件依赖这些包时拒绝删除，不会级联误删。
                run_root rpm -e "${owned_packages[@]}"
                ;;
            pacman)
                # 不使用 -c/-s，依赖冲突时让 pacman 拒绝删除。
                run_root pacman -R --noconfirm "${owned_packages[@]}"
                ;;
            *)
                echo "错误: 安装记录中的包管理器不可识别: ${manager:-空}" >&2
                return 1
                ;;
        esac
    fi

    if [ "$docker_owned" -ne 0 ]; then
        echo "==> 清理由 pitr-fs 安装的 Docker/containerd 运行数据"
        run_root rm -rf -- /var/lib/docker /var/lib/containerd
    fi

    for package in "${group_users[@]}"; do
        if getent group docker >/dev/null 2>&1 \
            && id -nG "$package" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
            run_root gpasswd -d "$package" docker >/dev/null
            echo "已撤销由 pitr-fs 添加的 docker 组成员: $package"
        fi
    done
    if state_values docker_group_created | grep -qx 1 \
        && getent group docker >/dev/null 2>&1; then
        if run_root groupdel docker; then
            echo "已删除由 pitr-fs 创建的空 docker 组"
        else
            echo "错误: 由 pitr-fs 创建的 docker 组仍被使用；保留安装清单" >&2
            return 1
        fi
    fi

    if state_values docker_service_changed | grep -qx 1 \
        && command -v systemctl >/dev/null 2>&1 \
        && command -v docker >/dev/null 2>&1; then
        case "$(state_values docker_service_enabled | tail -1)" in
            enabled) run_root systemctl enable docker >/dev/null ;;
            *) run_root systemctl disable docker >/dev/null ;;
        esac
        case "$(state_values docker_service_active | tail -1)" in
            active) run_root systemctl start docker ;;
            *) run_root systemctl stop docker ;;
        esac
        echo "已恢复安装前的 Docker 服务状态"
    fi

    run_root rm -f "$STATE_FILE"
    run_root rmdir "$STATE_DIR" 2>/dev/null || true
    echo "pitr-fs 安装的宿主机依赖已清理；安装前已有的软件未改动"
}

if [ "${1:-}" = "--uninstall" ]; then
    [ "$#" -eq 1 ] || { echo "用法: $0 [--check|--uninstall]" >&2; exit 1; }
    uninstall_managed_dependencies
    exit
fi
if [ "${1:-}" = "--docker-snapshot-before" ]; then
    [ "$#" -eq 1 ] || { echo "用法: $0 --docker-snapshot-before" >&2; exit 1; }
    docker_snapshot_before
    exit
fi
if [ "${1:-}" = "--docker-snapshot-after" ]; then
    [ "$#" -le 2 ] || { echo "用法: $0 --docker-snapshot-after [镜像名]" >&2; exit 1; }
    docker_snapshot_after "${2:-pitr-fs:latest}"
    exit
fi
if [ "$#" -ne 0 ]; then
    echo "用法: $0 [--check|--uninstall]" >&2
    exit 1
fi

# 只安装真正缺失的依赖。尤其不能在已有可执行 docker 的机器上安装发行版的
# docker 包，否则 apt/dnf 可能卸载用户已有的 Docker CE 或其他实现。
packages=()
package_install_rc=0
docker_missing_before=0
command -v docker >/dev/null 2>&1 || docker_missing_before=1
docker_group_missing_before=0
getent group docker >/dev/null 2>&1 || docker_group_missing_before=1
add_package() {
    local candidate existing
    candidate=$1
    for existing in "${packages[@]:-}"; do
        [ "$existing" = "$candidate" ] && return 0
    done
    packages+=("$candidate")
}

manager=""
if command -v apt-get >/dev/null 2>&1; then
    manager=apt-get
    command -v docker >/dev/null 2>&1 || add_package docker.io
    command -v fusermount3 >/dev/null 2>&1 || add_package fuse3
    command -v findmnt >/dev/null 2>&1 || add_package util-linux
    command -v mountpoint >/dev/null 2>&1 || add_package util-linux
    command -v realpath >/dev/null 2>&1 || add_package coreutils
    command -v sha256sum >/dev/null 2>&1 || add_package coreutils
    command -v tar >/dev/null 2>&1 || add_package tar
    command -v curl >/dev/null 2>&1 || add_package curl
    command -v git >/dev/null 2>&1 || add_package git
    command -v python3 >/dev/null 2>&1 || add_package python3
    command -v awk >/dev/null 2>&1 || add_package gawk
    [ -e /etc/ssl/certs/ca-certificates.crt ] || add_package ca-certificates
    if [ "${#packages[@]}" -gt 0 ]; then
        before_packages=$(mktemp)
        after_packages=$(mktemp)
        trap 'rm -f "$before_packages" "$after_packages"' EXIT
        dpkg-query -W -f='${binary:Package}\n' | sort -u >"$before_packages"
        run_root apt-get update
        run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade \
            "${packages[@]}" || package_install_rc=$?
        dpkg-query -W -f='${binary:Package}\n' | sort -u >"$after_packages"
    fi
elif command -v dnf >/dev/null 2>&1; then
    manager=dnf
    command -v docker >/dev/null 2>&1 || add_package docker
    command -v fusermount3 >/dev/null 2>&1 || add_package fuse3
    command -v findmnt >/dev/null 2>&1 || add_package util-linux
    command -v mountpoint >/dev/null 2>&1 || add_package util-linux
    command -v realpath >/dev/null 2>&1 || add_package coreutils
    command -v sha256sum >/dev/null 2>&1 || add_package coreutils
    command -v tar >/dev/null 2>&1 || add_package tar
    command -v curl >/dev/null 2>&1 || add_package curl
    command -v git >/dev/null 2>&1 || add_package git
    command -v python3 >/dev/null 2>&1 || add_package python3
    command -v awk >/dev/null 2>&1 || add_package gawk
    [ -e /etc/ssl/certs/ca-bundle.crt ] || add_package ca-certificates
    if [ "${#packages[@]}" -gt 0 ]; then
        before_packages=$(mktemp); after_packages=$(mktemp)
        trap 'rm -f "$before_packages" "$after_packages"' EXIT
        rpm -qa --qf '%{NAME}\n' | sort -u >"$before_packages"
        run_root dnf install -y "${packages[@]}" || package_install_rc=$?
        rpm -qa --qf '%{NAME}\n' | sort -u >"$after_packages"
    fi
elif command -v yum >/dev/null 2>&1; then
    manager=yum
    command -v docker >/dev/null 2>&1 || add_package docker
    command -v fusermount3 >/dev/null 2>&1 || add_package fuse3
    command -v findmnt >/dev/null 2>&1 || add_package util-linux
    command -v mountpoint >/dev/null 2>&1 || add_package util-linux
    command -v realpath >/dev/null 2>&1 || add_package coreutils
    command -v sha256sum >/dev/null 2>&1 || add_package coreutils
    command -v tar >/dev/null 2>&1 || add_package tar
    command -v curl >/dev/null 2>&1 || add_package curl
    command -v git >/dev/null 2>&1 || add_package git
    command -v python3 >/dev/null 2>&1 || add_package python3
    command -v awk >/dev/null 2>&1 || add_package gawk
    [ -e /etc/ssl/certs/ca-bundle.crt ] || add_package ca-certificates
    if [ "${#packages[@]}" -gt 0 ]; then
        before_packages=$(mktemp); after_packages=$(mktemp)
        trap 'rm -f "$before_packages" "$after_packages"' EXIT
        rpm -qa --qf '%{NAME}\n' | sort -u >"$before_packages"
        run_root yum install -y "${packages[@]}" || package_install_rc=$?
        rpm -qa --qf '%{NAME}\n' | sort -u >"$after_packages"
    fi
elif command -v pacman >/dev/null 2>&1; then
    manager=pacman
    command -v docker >/dev/null 2>&1 || add_package docker
    command -v fusermount3 >/dev/null 2>&1 || add_package fuse3
    command -v findmnt >/dev/null 2>&1 || add_package util-linux
    command -v mountpoint >/dev/null 2>&1 || add_package util-linux
    command -v realpath >/dev/null 2>&1 || add_package coreutils
    command -v sha256sum >/dev/null 2>&1 || add_package coreutils
    command -v tar >/dev/null 2>&1 || add_package tar
    command -v curl >/dev/null 2>&1 || add_package curl
    command -v git >/dev/null 2>&1 || add_package git
    command -v python3 >/dev/null 2>&1 || add_package python
    command -v awk >/dev/null 2>&1 || add_package gawk
    [ -e /etc/ssl/certs/ca-certificates.crt ] || add_package ca-certificates
    if [ "${#packages[@]}" -gt 0 ]; then
        before_packages=$(mktemp); after_packages=$(mktemp)
        trap 'rm -f "$before_packages" "$after_packages"' EXIT
        pacman -Qq | sort -u >"$before_packages"
        run_root pacman -Sy --needed --noconfirm "${packages[@]}" || package_install_rc=$?
        pacman -Qq | sort -u >"$after_packages"
    fi
elif command -v zypper >/dev/null 2>&1; then
    manager=zypper
    command -v docker >/dev/null 2>&1 || add_package docker
    command -v fusermount3 >/dev/null 2>&1 || add_package fuse3
    command -v findmnt >/dev/null 2>&1 || add_package util-linux
    command -v mountpoint >/dev/null 2>&1 || add_package util-linux
    command -v realpath >/dev/null 2>&1 || add_package coreutils
    command -v sha256sum >/dev/null 2>&1 || add_package coreutils
    command -v tar >/dev/null 2>&1 || add_package tar
    command -v curl >/dev/null 2>&1 || add_package curl
    command -v git >/dev/null 2>&1 || add_package git
    command -v python3 >/dev/null 2>&1 || add_package python3
    command -v awk >/dev/null 2>&1 || add_package gawk
    [ -e /etc/ssl/ca-bundle.pem ] || add_package ca-certificates
    if [ "${#packages[@]}" -gt 0 ]; then
        before_packages=$(mktemp); after_packages=$(mktemp)
        trap 'rm -f "$before_packages" "$after_packages"' EXIT
        rpm -qa --qf '%{NAME}\n' | sort -u >"$before_packages"
        run_root zypper --non-interactive install "${packages[@]}" || package_install_rc=$?
        rpm -qa --qf '%{NAME}\n' | sort -u >"$after_packages"
    fi
else
    echo "错误: 不支持当前 Linux 发行版的包管理器，请手动安装 Docker、FUSE3、util-linux、curl、git、Python 3 和 awk" >&2
    exit 1
fi

if [ "${#packages[@]}" -gt 0 ]; then
    state_append "manager=$manager"
    while read -r package; do
        [ -n "$package" ] && state_append "package=$package"
    done < <(comm -13 "$before_packages" "$after_packages")
fi
if [ "$package_install_rc" -ne 0 ]; then
    echo "提示: 包管理器返回 $package_install_rc，安装器将修复服务并以最终依赖检查为准"
fi
if [ "$docker_group_missing_before" -ne 0 ] && getent group docker >/dev/null 2>&1; then
    state_append "docker_group_created=1"
fi

if [ "${#packages[@]}" -eq 0 ]; then
    echo "宿主机依赖均已存在，未替换或重装任何软件包"
fi

if command -v modprobe >/dev/null 2>&1; then
    run_root modprobe fuse || true
fi
if command -v docker >/dev/null 2>&1 && ! docker_daemon_available; then
    docker_managed=0
    state_values package | grep -Eq '^(docker|docker\.io|docker-ce)$' && docker_managed=1
    if [ "$docker_missing_before" -ne 0 ] || [ "$docker_managed" -ne 0 ]; then
        if command -v systemctl >/dev/null 2>&1; then
            # 修复中断安装或同机卸载重装留下的失效 socket activation 状态。
            run_root systemctl stop docker.service docker.socket 2>/dev/null || true
            if ! systemctl is-active --quiet docker.socket; then
                run_root rm -f /run/docker.sock
            fi
            run_root systemctl daemon-reload
            run_root systemctl reset-failed docker.service docker.socket 2>/dev/null || true
            run_root systemctl enable --now containerd.service
            run_root systemctl enable --now docker.socket
            run_root systemctl start docker.service
        else
            echo "错误: 本脚本安装了 Docker，但当前系统不使用 systemd，无法自动启动 daemon" >&2
            exit 1
        fi
    else
        echo "错误: 检测到安装前已有的 Docker，但 daemon 不可用；为避免修改用户环境，安装器不会启动或重配它" >&2
        exit 1
    fi
fi

# 让普通用户后续可以直接使用 docker；当前 shell 的组列表不会即时刷新，
# install.sh 和生成的 pitr wrapper 会在此期间自动使用 sudo。
install_user=${SUDO_USER:-$(id -un)}
if [ "$install_user" != "root" ] && command -v getent >/dev/null 2>&1 \
    && getent group docker >/dev/null 2>&1 \
    && ! id -nG "$install_user" | tr ' ' '\n' | grep -qx docker; then
    run_root usermod -aG docker "$install_user"
    state_append "docker_group_user=$install_user"
    echo "已将 $install_user 加入 docker 组；重新登录后可免 sudo 使用 Docker"
fi

check_dependencies
