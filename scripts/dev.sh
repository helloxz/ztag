#!/usr/bin/env bash
# ============================================================
# Development run script: start the service directly with go run (no build)
# Usage:
#   ./scripts/dev.sh               # use default config data/config.toml
#   ./scripts/dev.sh -config xxx   # forward args to specify a custom config (e.g. temp port)
# Notes:
#   - On first run, data/config.toml is auto-generated from the embedded template (see internal/config)
#   - If local port 8080 is in use, change server.addr in data/config.toml
#     or specify a custom config via -config
# ============================================================
set -euo pipefail

# Switch to project root
cd "$(dirname "$0")/.."

echo ">>> Starting ztag in dev mode (go run) ..."
# Forward all args (e.g. -config) for flexible config override in development
go run ./cmd/server "$@"
