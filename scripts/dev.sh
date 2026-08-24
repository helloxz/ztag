#!/usr/bin/env bash
# ============================================================
# 开发环境运行脚本：不编译，直接用 go run 启动服务
# 用法：
#   ./scripts/dev.sh               # 使用默认配置 data/config.toml
#   ./scripts/dev.sh -config xxx   # 转发参数指定配置文件（如临时换端口）
# 说明：
#   - 首次启动会自动生成 data/config.toml（见 internal/config）
#   - 本机 8080 若被占用，可在 data/config.toml 修改 server.addr，
#     或通过 -config 指定一份自定义配置
# ============================================================
set -euo pipefail

# 切换到项目根目录
cd "$(dirname "$0")/.."

echo ">>> 以 dev 模式启动 ztag (go run) ..."
# 透传全部参数（如 -config），便于开发时灵活指定配置
go run ./cmd/server "$@"