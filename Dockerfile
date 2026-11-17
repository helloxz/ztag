# ============================================================
# ztag Dockerfile - 基于 debian:12-slim，下载官方 release 二进制
# 构建: docker build -t ztag:latest .
# 构建（锁定版本）: docker build --build-arg VERSION=v0.2.3 -t ztag:v0.2.3 .
# 运行: docker run -d -p 18080:18080 -v ztag-data:/app/data --name ztag ztag:latest
# ============================================================
FROM debian:12-slim

LABEL maintainer="helloxz/ztag" \
      description="ztag - Image content recognition API (pure Go)"

WORKDIR /app

# 1. 安装脚本：负责安装运行时工具（curl/tar/file）并下载 release 二进制
COPY scripts/docker-install.sh /tmp/docker-install.sh

ARG TARGETARCH

# 可选：锁定要下载的 release 版本（形如 v0.2.3）；为空时解析 latest
# CI 中必须传入，否则 latest 受 GitHub API 缓存影响，会出现版本漂移/不可复现
ARG VERSION=""

RUN chmod +x /tmp/docker-install.sh \
    && /tmp/docker-install.sh "${TARGETARCH}" "${VERSION}" \
    && rm -f /tmp/docker-install.sh

# 2. 入口脚本：处理空卷场景与参数透传
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

EXPOSE 18080

VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:18080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
