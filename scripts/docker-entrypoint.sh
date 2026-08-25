#!/usr/bin/env bash
# ============================================================
# docker-entrypoint.sh: 参数透传，空卷时触发配置生成并兼容旧版 :8080
# 新版二进制已为 :18080，此分支为 no-op；旧版 v0.1.0 会自动迁移
# ============================================================
set -e

CONFIG_PATH="${CONFIG_PATH:-/app/data/config.toml}"

HAS_CONFIG_FLAG=0
for arg in "$@"; do
  if [ "$arg" = "-config" ] || [[ "$arg" == -config=* ]]; then
    HAS_CONFIG_FLAG=1
    break
  fi
done

if [ "$HAS_CONFIG_FLAG" -eq 0 ]; then
  mkdir -p "$(dirname "$CONFIG_PATH")"

  # 空卷场景：配置尚不存在，触发一次生成（EnsureDataConfig）后迁移
  if [ ! -f "$CONFIG_PATH" ]; then
    # 后台短暂启动以生成 data/config.toml（旧版为 :8080，新版直接 :18080）
    timeout 3 /app/ztag >/dev/null 2>&1 || true
    for i in 1 2 3 4 5; do
      [ -f "$CONFIG_PATH" ] && break
      sleep 0.5
    done
  fi

  # 兼容旧版 v0.1.0：若仍为 :8080 则迁移到 :18080；新版已是 :18080 则 no-op
  if [ -f "$CONFIG_PATH" ] && grep -q 'addr *= *":8080"' "$CONFIG_PATH" 2>/dev/null; then
    echo "[entrypoint] migrate $CONFIG_PATH :8080 -> :18080"
    sed -i 's/addr *= *":8080"/addr = ":18080"/g' "$CONFIG_PATH"
  fi
fi

if [ $# -eq 0 ]; then
  exec /app/ztag
elif [[ "$1" == -* ]]; then
  exec /app/ztag "$@"
else
  exec "$@"
fi
