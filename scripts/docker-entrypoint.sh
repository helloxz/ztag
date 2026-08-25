#!/usr/bin/env bash
# ============================================================
# docker-entrypoint.sh: 确保配置端口为 18080 后启动 ztag
# 处理 v0.1.0 默认 :8080 与当前 :18080 的兼容，以及空卷挂载场景
# ============================================================
set -e

CONFIG_PATH="${CONFIG_PATH:-/app/data/config.toml}"
TEMPLATE_PATH="/app/data/.config.template.toml"
DEFAULT_ADDR=":18080"

# 检测是否通过 -config 自定义路径
HAS_CONFIG_FLAG=0
for arg in "$@"; do
  if [ "$arg" = "-config" ] || [[ "$arg" == -config=* ]]; then
    HAS_CONFIG_FLAG=1
    break
  fi
done

# 仅对默认路径做端口兼容处理
if [ "$HAS_CONFIG_FLAG" -eq 0 ]; then
  mkdir -p "$(dirname "$CONFIG_PATH")"

  # 空卷挂载（bind mount 空目录）导致模板丢失：用内置模板补齐
  if [ ! -f "$CONFIG_PATH" ] && [ -f "$TEMPLATE_PATH" ]; then
    echo "[entrypoint] seeding config from template -> $CONFIG_PATH"
    cp "$TEMPLATE_PATH" "$CONFIG_PATH"
  fi

  # 若文件已存在且为旧版 :8080，自动迁移到 :18080
  if [ -f "$CONFIG_PATH" ] && grep -q 'addr *= *":8080"' "$CONFIG_PATH"; then
    echo "[entrypoint] migrate $CONFIG_PATH :8080 -> ${DEFAULT_ADDR}"
    sed -i 's/addr *= *":8080"/addr = ":18080"/g' "$CONFIG_PATH"
  fi

  # 若仍不存在（首次启动且无模板），让 ztag 正常生成后，下次启动会被迁移；
  # 为避免首次监听错误端口，后台短暂触发一次生成再迁移
  if [ ! -f "$CONFIG_PATH" ]; then
    echo "[entrypoint] config not found, triggering generation..."
    timeout 3 /app/ztag 2>/dev/null || true
    for i in 1 2 3 4 5; do
      [ -f "$CONFIG_PATH" ] && break
      sleep 0.5
    done
    if [ -f "$CONFIG_PATH" ] && grep -q 'addr *= *":8080"' "$CONFIG_PATH"; then
      echo "[entrypoint] post-generate migrate :8080 -> ${DEFAULT_ADDR}"
      sed -i 's/addr *= *":8080"/addr = ":18080"/g' "$CONFIG_PATH"
    fi
  fi

  if [ -f "$CONFIG_PATH" ]; then
    echo "[entrypoint] using config: $CONFIG_PATH"
    grep -E '^\s*addr\s*=' "$CONFIG_PATH" || true
  fi
fi

# 启动：无参或 - 开头参数透传给 ztag，否则 exec 任意命令
if [ $# -eq 0 ]; then
  exec /app/ztag
elif [[ "$1" == -* ]]; then
  exec /app/ztag "$@"
else
  exec "$@"
fi
