#!/usr/bin/env bash
# ============================================================
# 编译脚本：编译服务并输出到 bin/ztag
# 用法：./scripts/build.sh
# ============================================================
set -euo pipefail

# 切换到项目根目录（保证从任意位置执行脚本路径都正确）
cd "$(dirname "$0")/.."

echo ">>> 开始编译 ztag ..."
go build -o bin/ztag ./cmd/server
echo ">>> 编译完成: $(pwd)/bin/ztag"