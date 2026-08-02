# ============================================================================
# pitr-fs: all-in-one 镜像
#   阶段 1 golang 构建 pitrd + pitr 静态二进制
#   阶段 2 从固定 LTS 源码构建带 pitr Compaction 标记的 JuiceFS
#   阶段 3 基于固定 PostgreSQL 16.14,装 fuse3 + tini,铺 SQL 与 entrypoint
# 见设计文档 §13.2
# ============================================================================

# ---- 阶段 1: 构建 pitrd / pitr ----
FROM golang:1.25@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980 AS builder
WORKDIR /src

ARG PITR_VERSION=dev
ARG PITR_COMMIT=unknown
ARG PITR_BUILD_DATE=unknown

# 依赖单独一层, 让源码改动不打破 mod cache
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN build_flags="-s -w \
      -X pitr_fs/internal/buildinfo.Version=${PITR_VERSION} \
      -X pitr_fs/internal/buildinfo.Commit=${PITR_COMMIT} \
      -X pitr_fs/internal/buildinfo.BuildDate=${PITR_BUILD_DATE}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$build_flags" -o /out/pitrd ./cmd/pitrd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$build_flags" -o /out/pitr  ./cmd/pitr

# ---- 阶段 2: 构建固定 JuiceFS LTS 与最小 pitr 补丁 ----
FROM golang:1.25@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980 AS juicefs-builder

ARG JUICEFS_VERSION=v1.3.0
ARG JUICEFS_COMMIT=30190ca1094d26e85f19a979ca51b0ea19af1eaa
ARG JUICEFS_PATCH_REVISION=pitrfs.1
WORKDIR /src
COPY third_party/juicefs/patches/0001-mark-postgres-compaction.patch /tmp/juicefs.patch
RUN git clone --depth 1 --branch "$JUICEFS_VERSION" \
        https://github.com/juicedata/juicefs.git juicefs \
 && cd juicefs \
 && test "$(git rev-parse HEAD)" = "$JUICEFS_COMMIT" \
 && git apply --check /tmp/juicefs.patch \
 && git apply /tmp/juicefs.patch \
 && CGO_ENABLED=1 go build -trimpath \
      -ldflags="-s -w \
        -X github.com/juicedata/juicefs/pkg/version.revision=${JUICEFS_PATCH_REVISION}-30190ca \
        -X github.com/juicedata/juicefs/pkg/version.revisionDate=stable" \
      -o /out/juicefs .

# ---- 阶段 3: 运行镜像 ----
FROM postgres:16.14-bookworm@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55

# FUSE3(fusermount3,供 JuiceFS 与 pitr proxy 双层挂载)+ tini。
# JuiceFS 由上一个阶段的固定源码构建，不允许安装脚本获取 latest。
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl fuse3 tini \
    && rm -rf /var/lib/apt/lists/*

COPY --from=juicefs-builder /out/juicefs /usr/local/bin/juicefs

# 版本 label 放在重依赖层之后，逻辑版本变化不会触发重新 apt/juicefs 下载。
ARG PITR_VERSION=dev
ARG PITR_COMMIT=unknown
ARG PITR_BUILD_DATE=unknown
LABEL org.opencontainers.image.version="$PITR_VERSION" \
      org.opencontainers.image.revision="$PITR_COMMIT" \
      org.opencontainers.image.created="$PITR_BUILD_DATE" \
      io.pitr-fs.juicefs.version="1.3.0" \
      io.pitr-fs.juicefs.commit="30190ca1094d26e85f19a979ca51b0ea19af1eaa" \
      io.pitr-fs.juicefs.patch="pitrfs.1" \
      io.pitr-fs.postgresql.version="16.14"

# 二进制
COPY --from=builder /out/pitrd /usr/local/bin/pitrd
COPY --from=builder /out/pitr  /usr/local/bin/pitr

# SQL 双拷贝:
#   /docker-entrypoint-initdb.d/ 由 PG 官方镜像在数据目录为空时自动执行
#   /etc/pitr/ 由 entrypoint 在 PG 就绪后再幂等跑一次(镜像升级补 DDL)
COPY internal/schema/init_pitr.sql /docker-entrypoint-initdb.d/10-pitr-schema.sql
COPY internal/schema/init_pitr.sql /etc/pitr/init_pitr.sql

# 容器编排脚本
COPY deploy/entrypoint.sh /usr/local/bin/pitr-entrypoint.sh
RUN chmod +x /usr/local/bin/pitr-entrypoint.sh

ENV PITR_VOLUME=default \
    PITR_MOUNT_ROOT=/pitr \
    PITR_GC_INTERVAL=10m \
    PITR_GC_THREADS=4 \
    PITR_JFS_CACHE_SIZE=1024 \
    PITR_SOCKET=/var/run/pitrd.sock \
    PITR_DATA_DIR=/data

VOLUME ["/var/lib/postgresql/data", "/data"]
EXPOSE 5432

# tini 转发信号;entrypoint 依次关闭 pitrd 与 PG,且透传 pitrd 异常退出码
ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/usr/local/bin/pitr-entrypoint.sh"]
