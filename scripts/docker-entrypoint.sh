#!/usr/bin/env bash
# ============================================================
# docker-entrypoint.sh: 确保配置端口为 18080 后启动 ztag
# 兼容历史 release 默认 :8080 与当前要求 :18080 的差异
# ============================================================
set -e

CONFIG_PATH="${CONFIG_PATH:-/app/data/config.toml}"
DEFAULT_ADDR=":18080"

# 如果用户显式通过 -config 指定路径，则尊重用户配置
HAS_CONFIG_FLAG=0
for arg in "$@"; do
  if [ "$arg" = "-config" ] || [[ "$arg" == -config=* ]]; then
    HAS_CONFIG_FLAG=1
    break
  fi
done

# 当使用默认配置路径时，确保端口为 18080
# 逻辑：
# 1. 若 config 不存在：先让 ztag 生成一次（前台启动会阻塞，故用 --help 触发 EnsureDataConfig 无需改），
#    更可靠是直接等待 ztag 首次启动生成；这里我们预创建目录并在启动前 patch
# 2. 若 config 已存在但为旧版 :8080，则自动迁移到 :18080
ensure_port() {
  local cfg="$1"
  if [ -f "$cfg" ]; then
    if grep -q 'addr *= *":8080"' "$cfg"; then
      echo "[entrypoint] migrate $cfg :8080 -> ${DEFAULT_ADDR}"
      sed -i 's/addr *= *":8080"/addr = ":18080"/g' "$cfg"
    fi
  fi
}

# 如果是默认路径，先确保 data 目录存在
mkdir -p "$(dirname "$CONFIG_PATH")"

# 非 -config 覆盖场景才做迁移
if [ "$HAS_CONFIG_FLAG" -eq 0 ]; then
  # 分两种情况：
  # a) 文件已存在（VOLUME 持久化或镜像预置）-> 直接 patch
  # b) 文件不存在 -> 先启动一次的副作用会生成，但我们不能阻塞；改为预生成空配置后 patch
  #    简化：若不存在，创建一个最小占位，ztag 的 EnsureDataConfig 会视为不存在并覆盖？不，会视为存在。
  #    因此不存在时不预创建，改在后台让 ztag 生成后 patch 的方案不可靠。
  #    更稳妥：直接检测不存在时先运行一次 ztag --version 间接触发 EnsureDataConfig？但 --version 不走配置加载。
  #    最终：让 ztag 正常启动，若首次启动生成的是 :8080，下一秒的健康检查前我们无法拦截。
  #    所以采用包装：启动 ztag 前若文件不存在，先 touch 一个临时文件让后续 patch 生效，待 ztag 覆盖后再 patch？
  #    最简单可靠：启动前若文件不存在，等待 2 秒后由 entrypoint 异步 patch + SIGHUP? 过于复杂。
  #    折中：同步检查，若不存在则先执行一次短暂超时的启动来生成配置，再 patch，最后 exec 正式启动。
  if [ ! -f "$CONFIG_PATH" ]; then
    echo "[entrypoint] config not found, generating default config..."
    # 后台短暂启动，2 秒后杀掉，仅为触发 EnsureDataConfig 生成文件
    timeout 3 /app/ztag 2>/dev/null || true
    # 等待文件生成
    for i in 1 2 3 4 5; do
      if [ -f "$CONFIG_PATH" ]; then break; fi
      sleep 0.5
    done
  fi
  ensure_port "$CONFIG_PATH"
fi

# 最终展示配置监听地址（便于排障）
if [ -f "$CONFIG_PATH" ]; then
  echo "[entrypoint] using config: $CONFIG_PATH"
  grep -E '^\s*addr\s*=' "$CONFIG_PATH" || true
fi

# 透传所有参数给 ztag；若无参数则直接启动
if [ $# -eq 0 ]; then
  exec /app/ztag
else
  # 支持 docker run ztag --version / --help 等直接透传
  # 若第一个参数以 - 开头，视为 ztag 参数
  if [[ "$1" == -* ]]; then
    exec /app/ztag "$@"
  else
    exec "$@"
  fi
fi
