#!/usr/bin/env bash
# ============================================================
# Build script: compile the service to bin/ztag
# Usage: ./scripts/build.sh
# ============================================================
set -euo pipefail

# Switch to project root (ensure correct path regardless of cwd)
cd "$(dirname "$0")/.."

echo ">>> Building ztag ..."
go build -trimpath -ldflags="-s -w" -o bin/ztag ./cmd/server
echo ">>> Build succeeded: $(pwd)/bin/ztag"
