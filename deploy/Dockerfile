# ============================================================================
# pitr-fs: all-in-one 镜像
#   阶段 1 golang 构建 pitrd + pitr 静态二进制
#   阶段 2 基于 postgres:16, 装 juicefs + fuse3 + tini, 铺 SQL 与 entrypoint
# 见设计文档 §13.2
# ============================================================================

# ---- 阶段 1: 构建 pitrd / pitr ----
FROM golang:1.25 AS builder
WORKDIR /src

# 依赖单独一层, 让源码改动不打破 mod cache
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pitrd ./cmd/pitrd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pitr  ./cmd/pitr

# ---- 阶段 2: 运行镜像 ----
FROM postgres:16

# JuiceFS + FUSE + tini
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl fuse3 tini \
    && curl -sSL https://d.juicefs.com/install | sh - \
    && rm -rf /var/lib/apt/lists/*

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

ENV POSTGRES_USER=pitr \
    POSTGRES_PASSWORD=pitr \
    POSTGRES_DB=pitr_fs \
    PITR_VOLUME=default \
    PITR_MOUNT=/workspace \
    PITR_SOCKET=/var/run/pitrd.sock \
    PITR_DATA_DIR=/data

VOLUME ["/var/lib/postgresql/data", "/data"]
EXPOSE 5432

# tini 收养 PG 变僵尸, exec pitrd 让 daemon 挂了容器就退不掩盖故障
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/pitr-entrypoint.sh"]
