#!/usr/bin/env bash
# ============================================================
# docker-install.sh: 安装运行时依赖并下载 ztag 二进制
# 由 Dockerfile 调用，工作目录为 /app
# 用法: docker-install.sh [arch] [version]
#   arch    架构（amd64/arm64），缺省取 TARGETARCH 或自动推断
#   version 锁定的 release 版本（如 v0.2.3），缺省解析 latest
# ============================================================
set -euxo pipefail

# 允许通过参数或环境变量传入架构，Dockerfile 会传递 TARGETARCH
ARCH="${1:-${TARGETARCH:-}}"
# 锁定的版本号（CI 传入，避免 latest 漂移）
PINNED="${2:-${VERSION:-}}"
if [ -z "$ARCH" ]; then
  # 回退：根据系统架构自动推断
  MACHINE="$(uname -m)"
  case "$MACHINE" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "unsupported machine: $MACHINE" >&2; exit 1 ;;
  esac
fi

# 归一化架构名（release 资产为 linux-amd64 / linux-arm64）
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "unsupported arch: $ARCH (only amd64/arm64)" >&2; exit 1 ;;
esac

echo ">>> Install runtime tools..."
export DEBIAN_FRONTEND=noninteractive
apt-get update
# --no-install-recommends 减小体积；纯 Go 静态二进制无动态库依赖，仅需下载/校验/探测工具
apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  tar \
  file
# 清理 apt 缓存减小镜像体积
rm -rf /var/lib/apt/lists/*

echo ">>> Resolving ztag version..."
VER=""
if [ -n "$PINNED" ]; then
  # 优先使用显式锁定版本：CI 构建与历史版本重建都走这里
  VER="$PINNED"
  echo ">>> Using pinned version: $VER"
else
  # 未锁定时：优先通过 GitHub API 获取最新 tag，其次通过重定向 Location 头
  if VER="$(curl -fsSL https://api.github.com/repos/helloxz/ztag/releases/latest | grep -oE '\"tag_name\"[[:space:]]*:[[:space:]]*\"[^\"]+\"' | head -n1 | sed -E 's/.*\"(v[^\"]+)\".*/\1/')"; then
    if [ -n "$VER" ]; then
      echo "resolved version via API: $VER"
    fi
  fi

  # API 失败时回退到解析重定向 URL
  if [ -z "$VER" ]; then
    echo "API failed, fallback to redirect parsing..."
    REDIRECT_URL="$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/helloxz/ztag/releases/latest || true)"
    # 期望 https://github.com/helloxz/ztag/releases/tag/v0.1.0
    VER="$(echo "$REDIRECT_URL" | grep -oE 'tag/v[0-9]+\.[0-9]+\.[0-9]+.*' | sed 's|tag/||' | cut -d'/' -f1 || true)"
    if [ -n "$VER" ]; then
      echo "resolved version via redirect: $VER"
    fi
  fi
fi

if [ -z "$VER" ]; then
  echo "failed to resolve ztag version (latest lookup failed; pass an explicit version, e.g. v0.2.3)" >&2
  exit 1
fi

# 拼接下载地址
PKG="ztag-${VER}-linux-${ARCH}.tar.gz"
BASE_URL="https://github.com/helloxz/ztag/releases/download/${VER}/${PKG}"
SHA_URL="${BASE_URL}.sha256"

echo ">>> Downloading ${PKG} from ${BASE_URL}..."
WORKDIR="/app"
mkdir -p "${WORKDIR}"
cd "${WORKDIR}"

# 下载主包与校验文件（校验文件可选，失败不阻断但尽可能校验）
curl -fL -o "/tmp/${PKG}" "${BASE_URL}"
if curl -fL -o "/tmp/${PKG}.sha256" "${SHA_URL}"; then
  echo ">>> Verifying sha256..."
  # 官方 sha256 文件格式为 "<hash>  <filename>"，需在 /tmp 目录下校验
  (cd /tmp && sha256sum -c "${PKG}.sha256")
else
  echo ">>> sha256 file not found, skip verification"
fi

echo ">>> Extracting..."
tar -xzf "/tmp/${PKG}" -C "${WORKDIR}"
# release 包内为单文件 ztag
chmod +x "${WORKDIR}/ztag"
ls -lh "${WORKDIR}/ztag"
file "${WORKDIR}/ztag" 2>&1 | head -n 2 || true
# 冒烟：验证二进制可执行（仅当前架构可运行）
if "${WORKDIR}/ztag" --version 2>&1 | head -n5; then
  echo ">>> Binary version check passed"
else
  echo ">>> Warning: binary version check failed (may be cross-arch build)" >&2
fi

# upx 压缩的二进制 ldd 会显示 not a dynamic executable，属正常，跳过错误提示
if file "${WORKDIR}/ztag" 2>&1 | grep -q "dynamically linked"; then
  if ldd "${WORKDIR}/ztag" 2>&1 | grep -q "not found"; then
    echo ">>> ERROR: missing shared libraries:" >&2
    ldd "${WORKDIR}/ztag" || true
    exit 1
  fi
fi

# 清理临时文件
rm -f "/tmp/${PKG}" "/tmp/${PKG}.sha256"

echo ">>> Install completed: ${WORKDIR}/ztag ${VER} ${ARCH}"
