#!/usr/bin/env bash
# 构建 pitr/pitrd/schema/宿主升级器的 Linux 离线逻辑升级包；不会发布 Release。
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

if [ "$(uname -s)" != "Linux" ]; then
    echo "错误: 升级包只能在 Linux 环境构建" >&2
    exit 1
fi
if [ "$#" -ne 1 ]; then
    echo "用法: PITR_VERSION=<版本> $0 <输出.tar.gz>" >&2
    exit 2
fi
for command_name in go git sha256sum tar realpath; do
    command -v "$command_name" >/dev/null 2>&1 || {
        echo "错误: 缺少 $command_name" >&2
        exit 1
    }
done

commit=$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
version=${PITR_VERSION:-dev-$commit}
case "$version" in
    ''|*[!A-Za-z0-9._+-]*)
        echo "错误: PITR_VERSION 只能包含字母、数字、点、下划线、加号和连字符" >&2
        exit 2
        ;;
esac
build_date=${PITR_BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
goarch=${PITR_GOARCH:-$(go env GOARCH)}
case "$goarch" in
    amd64|arm64) ;;
    *) echo "错误: PITR_GOARCH 仅支持 amd64/arm64: $goarch" >&2; exit 2 ;;
esac
output=$(realpath -m -- "$1")
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

ldflags="-s -w -X pitr_fs/internal/buildinfo.Version=$version -X pitr_fs/internal/buildinfo.Commit=$commit -X pitr_fs/internal/buildinfo.BuildDate=$build_date"
(
    cd "$REPO_ROOT"
    CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
        go build -trimpath -ldflags="$ldflags" -o "$work/pitr" ./cmd/pitr
    CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
        go build -trimpath -ldflags="$ldflags" -o "$work/pitrd" ./cmd/pitrd
)
install -m 0644 "$REPO_ROOT/internal/schema/init_pitr.sql" "$work/init_pitr.sql"
install -m 0755 "$REPO_ROOT/scripts/pitr-host-upgrade.sh" "$work/pitr-host-upgrade"
printf '%s\n' "$version" >"$work/VERSION"
printf 'commit=%s\nbuild_date=%s\ngoarch=%s\n' \
    "$commit" "$build_date" "$goarch" >"$work/BUILD-INFO"
(
    cd "$work"
    sha256sum pitr pitrd pitr-host-upgrade init_pitr.sql VERSION BUILD-INFO \
        >SHA256SUMS
    tar -czf "$output" pitr pitrd pitr-host-upgrade init_pitr.sql \
        VERSION BUILD-INFO SHA256SUMS
)
echo "已生成逻辑升级包: $output"
echo "版本: $version"
echo "架构: linux/$goarch"
