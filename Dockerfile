# ============================================================
# ztag Dockerfile - 基于 debian:12-slim，下载官方 release 二进制
# 构建: docker build -t ztag:latest .
# 运行: docker run -d -p 18080:18080 -v ztag-data:/app/data --name ztag ztag:latest
# ============================================================
FROM debian:12-slim

# 镜像元信息
LABEL maintainer="helloxz/ztag" \
      description="ztag - Image content recognition API (libvips + bimg)"

# 工作目录（要求 /app）
WORKDIR /app

# 复制安装脚本（安装 libvips 等运行时依赖 + 下载最新 release）
COPY scripts/docker-install.sh /tmp/docker-install.sh

# 多架构支持：Docker buildx 会自动注入 TARGETARCH (amd64/arm64)
ARG TARGETARCH

# 执行安装脚本并清理
RUN chmod +x /tmp/docker-install.sh \
    && /tmp/docker-install.sh "${TARGETARCH}" \
    && rm -f /tmp/docker-install.sh

# 暴露端口（与 data/config.toml 中 server.addr=:18080 一致）
EXPOSE 18080

# 数据持久化目录（配置 data/config.toml、日志 data/logs/）
VOLUME ["/app/data"]

# 健康检查：依赖 /healthz 探活接口（不鉴权）
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:18080/healthz || exit 1

# 默认启动命令；支持通过 docker run 追加参数透传（如 -config）
ENTRYPOINT ["/app/ztag"]
