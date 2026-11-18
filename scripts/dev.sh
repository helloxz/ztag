#!/usr/bin/env bash
# ============================================================
# Development run script: start the service directly with go run (no build)
# Usage:
#   ./scripts/dev.sh               # use default config data/config.toml
#   ./scripts/dev.sh -config xxx   # forward args to specify a custom config (e.g. temp port)
#   ZTAG_WEB_DIR= ./scripts/dev.sh # disable on-disk admin assets (use embedded assets)
# Notes:
#   - On first run, data/config.toml is auto-generated from the embedded template (see internal/config)
#   - Admin pages/styles are served from internal/webui/web by default here, so editing
#     HTML/CSS/JS only needs a browser refresh (no restart). Go changes still need a restart.
#   - If local port 8080 is in use, change server.addr in data/config.toml
#     or specify a custom config via -config
# ============================================================
set -euo pipefail

# Switch to project root
cd "$(dirname "$0")/.."

# 开发默认读磁盘资源（显式置空则退回内嵌资源）
export ZTAG_WEB_DIR="${ZTAG_WEB_DIR-internal/webui/web}"
if [ -n "$ZTAG_WEB_DIR" ]; then
  echo ">>> admin assets: from disk ($ZTAG_WEB_DIR)"
else
  echo ">>> admin assets: embedded in binary"
fi

echo ">>> Starting ztag in dev mode (go run) ..."
# Forward all args (e.g. -config) for flexible config override in development
go run ./cmd/server "$@"
