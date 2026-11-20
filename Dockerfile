# syntax=docker/dockerfile:1
# ============================================================
# ztag 镜像（linux/amd64）：图片管线使用 libvips（cgo）
#
# 构建阶段与运行阶段统一使用 Debian 12：libvips 是动态库，
# 跨发行版构建会因 glibc / time_t（t64 迁移）不一致而无法运行。
#
# 完整源码构建（不依赖 release 资产）：
# 构建: docker build -t ztag:latest .
# 运行: docker run -d -p 18080:18080 -v ztag-data:/app/data --name ztag ztag:latest
# ============================================================

# ---------- 构建阶段 ----------
FROM golang:1.27-bookworm AS builder

# libvips 开发头文件 + pkg-config 是 govips 的编译前提
RUN apt-get update \
    && apt-get install -y --no-install-recommends libvips-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
# 先拷贝依赖清单，命中层缓存
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# 图片管线固定使用 libvips（internal/helper/image_vips.go）。
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/ztag ./cmd/server \
    && /out/ztag --version

# ---------- 运行阶段 ----------
FROM debian:12-slim

LABEL maintainer="helloxz/ztag" \
      description="ztag - Image content recognition API (libvips image pipeline)"

# libvips42 为运行时库（Debian 12 为 8.14.1，与构建阶段一致）
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libvips42 \
    && rm -rf /var/lib/apt/lists/*

# glib 多线程 malloc arena 会放大碎片，压到 2 个可显著降低 RSS 增长
ENV MALLOC_ARENA_MAX=2

WORKDIR /app

COPY --from=builder /out/ztag /app/ztag
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /app/ztag /usr/local/bin/docker-entrypoint.sh

EXPOSE 18080

VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:18080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
