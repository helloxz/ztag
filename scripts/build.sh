#!/usr/bin/env bash
# ============================================================
# Build script: compile the service to bin/ztag
#
# 图片管线使用 libvips（cgo），构建依赖（Debian/Ubuntu）：
#   apt-get install -y build-essential pkg-config libvips-dev
#
# Usage: ./scripts/build.sh
# ============================================================
set -euo pipefail

# Switch to project root (ensure correct path regardless of cwd)
cd "$(dirname "$0")/.."

echo ">>> Building ztag (libvips image pipeline) ..."
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o bin/ztag ./cmd/server
echo ">>> Build succeeded: $(pwd)/bin/ztag"
