#!/usr/bin/env bash
# ============================================================
# 图片处理链路 A/B 基准（端到端）
#
# 用法:
#   ./scripts/bench-images.sh                      # 默认对比 bin/ztag-baseline（改造前）与 bin/ztag（当前）
#   ./scripts/bench-images.sh bin/ztag             # 只测指定二进制
#
# 说明:
#   - 使用 mock AI（ai.mock = true），只测图片预处理链路（缩放 / 转 WebP / 转 JPEG），
#     不含真实模型耗时；
#   - 每次请求记录接口返回的 elapsed_ms 与 curl 的 time_total；
#   - libvips 的原生内存不在 Go 堆里，进程峰值统一读 /proc/<pid>/status 的 VmHWM；
#   - 先串行跑各尺寸图片，再用同一张 24MP 大图并发 PARALLEL 次压峰值内存。
# ============================================================
set -euo pipefail

cd "$(dirname "$0")/.."

# 与 Dockerfile 保持一致：glib 多线程 malloc arena 会放大碎片，
# 压到 2 个可显著降低 libvips 进程的常驻内存（实测空闲 RSS 385MB → 195MB）。
export MALLOC_ARENA_MAX="${MALLOC_ARENA_MAX:-2}"

OUT="${ZTAG_BENCH_DIR:-/tmp/ztag-bench}"
IMG_DIR="$OUT/images"
SERIES="${ZTAG_BENCH_SERIES:-4}"      # 每个场景串行测量次数
PARALLEL="${ZTAG_BENCH_PARALLEL:-4}"  # 并发阶段请求数

mkdir -p "$IMG_DIR" "$OUT/run"

echo ">>> 生成基准图集 → $IMG_DIR"
ZTAG_BENCH_IMAGES="$IMG_DIR" go test ./internal/helper -run TestGenerateBenchImages -v >/dev/null

IMAGES=(photo-2mp.jpg photo-6mp.jpg photo-24mp.jpg screenshot.png alpha-2000x1500.png)

BINS=("$@")
if [ ${#BINS[@]} -eq 0 ]; then
  BINS=(bin/ztag-baseline bin/ztag)
fi

RESULTS="$OUT/results.tsv"
: > "$RESULTS"
printf "impl\timage\tmode\telapsed_ms\thttp_s\tbytes\tvmhwm_kb\n" >> "$RESULTS"

request_json() { # $1=port $2=image path → 输出 "elapsed_ms time_total"
  local port="$1" path="$2" resp body tt ms
  # 大图 base64 后可达 20MB+，必须走 stdin（命令行参数会超 ARG_MAX）
  resp="$( { printf '{"image_base64":"'; base64 -w0 "$path"; printf '"}'; } \
    | curl -sS -X POST "http://127.0.0.1:$port/api/v1/image/analyze" \
      -H 'Content-Type: application/json' \
      --data-binary @- \
      -w '\n%{time_total}')"
  body="${resp%$'\n'*}"
  tt="${resp##*$'\n'}"
  ms="$(printf '%s' "$body" | grep -o '"elapsed_ms":[0-9]*' | head -1 | cut -d: -f2 || true)"
  if [ -z "$ms" ]; then
    echo "请求失败: ${body:0:300}" >&2
    return 1
  fi
  echo "$ms $tt"
}

vmhwm() { awk '/VmHWM/{print $2; exit}' "/proc/$1/status" 2>/dev/null || echo 0; }

port_in_use() { ss -ltn 2>/dev/null | grep -q "127.0.0.1:$1 "; }

children=()
cleanup() {
  for p in "${children[@]:-}"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null || true
  done
}
trap cleanup EXIT

idx=0
for bin in "${BINS[@]}"; do
  idx=$((idx + 1))
  if [ ! -x "$bin" ]; then
    echo "!! 跳过（文件不存在）: $bin"
    continue
  fi
  name="$(basename "$bin")"
  bin_abs="$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")"

  # 选一个未被占用的端口，避免误测到上一轮残留的进程
  port=$((18100 + idx))
  while port_in_use "$port"; do port=$((port + 1)); done

  workdir="$OUT/run/$name"
  mkdir -p "$workdir"

  cat > "$workdir/config.toml" <<EOF
[server]
addr = "127.0.0.1:$port"
mode = "release"
workers = 4

[log]
level = "warn"

[auth]
enabled = false

[admin]
enabled = false

[ai]
mock = true
timeout = 30
image_timeout = 30
# 基准图集里有 16MB 的 24MP 样张，这里放宽上限以便测到该尺寸
max_image_bytes = 33554432

[[ai.channels]]
id = "default"
type = "openai-chat"
base_url = "http://127.0.0.1:1/v1"
models = ["dummy"]
EOF

  ( cd "$workdir" && exec "$bin_abs" -config "$workdir/config.toml" >server.log 2>&1 ) &
  pid=$!
  children+=("$pid")

  ready=0
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.1
  done
  # 端口必须是本进程在监听，且进程仍存活（防止误测到残留进程）
  if [ "$ready" != "1" ] || ! kill -0 "$pid" 2>/dev/null; then
    echo "!! $name 启动失败，日志末尾："
    tail -20 "$workdir/server.log"
    kill "$pid" 2>/dev/null || true
    continue
  fi

  echo ">>> $name 启动成功（port=$port，起始 VmHWM=$(vmhwm "$pid")KB，impl=$(grep -o 'image pipeline initialized.*' "$workdir/server.log" | head -1 || true)）"

  # 预热：不计入结果（触发首次编码器/管线初始化）
  request_json "$port" "$IMG_DIR/photo-2mp.jpg" >/dev/null

  for img in "${IMAGES[@]}"; do
    size="$(stat -c%s "$IMG_DIR/$img")"
    for _ in $(seq 1 "$SERIES"); do
      read -r ms tt < <(request_json "$port" "$IMG_DIR/$img")
      printf "%s\t%s\tserial\t%s\t%s\t%s\n" "$name" "$img" "$ms" "$tt" "$size" >> "$RESULTS"
    done
  done

  # 并发压峰值：同一张 24MP 大图并发 PARALLEL 次
  pids=()
  for _ in $(seq 1 "$PARALLEL"); do
    ( request_json "$port" "$IMG_DIR/photo-24mp.jpg" >/dev/null ) &
    pids+=($!)
  done
  for p in "${pids[@]}"; do wait "$p" || true; done

  hwm="$(vmhwm "$pid")"
  printf "%s\tphoto-24mp.jpg\tconcurrent-%s\t-\t-\t-\t%s\n" "$name" "$PARALLEL" "$hwm" >> "$RESULTS"
  echo ">>> $name 结束（VmHWM=${hwm}KB）"

  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
done

echo
echo "===== 串行场景（elapsed_ms，单位毫秒）====="
awk -F'\t' '
  NR>1 && $3=="serial" {
    k=$1 SUBSEP $2; n[k]++; sum[k]+=$4
    if ($4>mx[k]) mx[k]=$4
  }
  END {
    printf "%-18s %-22s %9s %9s %7s\n", "impl", "image", "avg", "max", "n"
    for (k in n) { split(k,p,SUBSEP); printf "%-18s %-22s %9.1f %9d %7d\n", p[1], p[2], sum[k]/n[k], mx[k], n[k] }
  }
' "$RESULTS" | sort -k1,1 -k2,2

echo
echo "===== 进程峰值内存（VmHWM，含 ${PARALLEL} 并发 24MP）====="
awk -F'\t' '
  NR>1 && $3 ~ /^concurrent/ { printf "%-18s %-16s %10s KB\n", $1, $3, $7 }
' "$RESULTS"

echo
echo "原始数据: $RESULTS"
