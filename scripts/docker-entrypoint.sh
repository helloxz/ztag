#!/usr/bin/env bash
# ============================================================
# docker-entrypoint.sh: 空卷挂载时 seeding 配置后启动 ztag
# ============================================================
set -e

CONFIG_PATH="${CONFIG_PATH:-/app/data/config.toml}"
TEMPLATE_PATH="/usr/local/share/ztag/config.toml.template"

# 检测是否通过 -config 自定义路径
HAS_CONFIG_FLAG=0
for arg in "$@"; do
  if [ "$arg" = "-config" ] || [[ "$arg" == -config=* ]]; then
    HAS_CONFIG_FLAG=1
    break
  fi
done

if [ "$HAS_CONFIG_FLAG" -eq 0 ]; then
  mkdir -p "$(dirname "$CONFIG_PATH")"

  # 空卷挂载（bind mount 空目录）导致镜像内 /app/data 被遮盖，用内置模板补齐
  if [ ! -f "$CONFIG_PATH" ] && [ -f "$TEMPLATE_PATH" ]; then
    echo "[entrypoint] seeding config from template -> $CONFIG_PATH"
    cp "$TEMPLATE_PATH" "$CONFIG_PATH"
  fi

  if [ -f "$CONFIG_PATH" ]; then
    echo "[entrypoint] using config: $CONFIG_PATH"
    grep -E '^\s*addr\s*=' "$CONFIG_PATH" || true
  fi
fi

if [ $# -eq 0 ]; then
  exec /app/ztag
elif [[ "$1" == -* ]]; then
  exec /app/ztag "$@"
else
  exec "$@"
fi
