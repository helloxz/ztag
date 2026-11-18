# ZTAG

基于大模型的图片内容识别服务：调用后端 AI 大模型识别图片内容，提取关键词与描述，并执行内容分类（色情、政治、血腥、暴力等），以统一 JSON API 的形式对外提供调用。

## 技术栈

- 语言 / Web 框架：Go + [gin](https://github.com/gin-gonic/gin)
  - 构建工具链锁定 `go1.27.4`（见 `go.mod` 的 `toolchain` 行），低于该版本会自动下载；不要降级，1.27.4 修复了 CVE-2026-111943（eMudhra 中间证书拒信）与 CVE-2026-97054（net/http MaxBytesReader 竞态崩溃）
- 配置管理：[viper](https://github.com/spf13/viper)，配置格式 TOML
- AI SDK：[github.com/zendev-sh/goai](https://github.com/zendev-sh/goai)（统一 SDK，一个接口对接 25+ 厂商）

## 目录结构

```
ztag/
├── cmd/server/main.go        # 程序入口：加载配置 → 构建依赖 → 启动服务
├── internal/
│   ├── config/               # 配置结构体 + viper 加载 + 校验（含内嵌默认模板）
│   ├── router/               # ★ 路由统一集中注册
│   ├── middleware/           # ★ 中间件：Recovery / AccessLog / Auth / RateLimit
│   ├── handler/              # 表现层：参数解析、统一响应输出
│   ├── service/              # 业务编排层：请求校验 → AI 网关调用
│   ├── ai/                   # ★ AI 多远端网关：渠道选择、协议适配（mock 骨架）
│   ├── model/                # 领域模型：请求体 / 响应体 / 业务错误码
│   ├── helper/               # ★ 助手函数：统一响应封装、错误转换
│   └── webui/                # 管理台前端资源（HTML/CSS/JS，go:embed 内嵌）
├── data/                     # 运行时数据目录（自动生成，docker 挂载点）
├── Makefile                  # build / run / tidy / vet / clean
└── .gitignore
```

分层依赖方向：`router → handler → service → ai`，单向向下，禁止跨层调用。

## 配置文件说明

运行时配置位于 `data/config.toml`：

- **首次启动自动生成**：若 `data/config.toml` 不存在，程序会自动把内嵌的默认模板拷贝到该路径；
- **docker 部署**：只需把宿主目录挂载为容器的 `/data`（`-v /host/data:/app/data`），配置即可持久化；
- 修改配置后需重启服务生效。

### 主备双渠道配置

固定两个渠道：`default`（默认/主渠道，必填）、`backup`（备用渠道，可选），每个渠道可配置多个模型（models 为模型标识字符串数组）：

```toml
[ai]
timeout = 60
max_retries = 2       # default 失败后切换/重试次数
mock = false          # 模拟 AI（无 API Key 联调用）
max_image_bytes = 10485760   # 图片大小上限（默认 10MB）
allow_private_urls = false   # 放行内网图片 URL（SSRF 防护，仅本地联调用）
temperature = 0.2            # 采样温度（越低输出越稳定，分类场景建议低值）
top_p = 0.9                  # 核采样（控制随机性，0 表示不设置）

[[ai.channels]]
id = "default"                # 渠道唯一标识：default | backup
type = "openai-chat"          # openai-chat | openai-response | anthropic
base_url = "https://api.openai.com/v1"
api_key = "sk-xxx"
models = [
  "gpt-4o-mini",
  "gpt-4o",
]

[[ai.channels]]
id = "backup"                 # 备用渠道（可整节删除）
type = "anthropic"            # Anthropic 兼容渠道示例（如阿里云）
base_url = "https://dashscope.aliyuncs.com/api/v2/apps/anthropic"
api_key = "sk-xxx"
models = [
  "claude-3-5-sonnet",
]
```

- 渠道 `id` 仅允许 `default` / `backup`（配置校验强制，default 必填、backup 可选）。
- `type` 支持三种协议：`openai-chat`（OpenAI Chat Completions 兼容）、`openai-response`（OpenAI Responses 兼容）、`anthropic`（Anthropic Messages 兼容，含阿里云等兼容网关）。
- 请求时通过 `model="channel/modelId"`（如 `default/gpt-4o-mini`、`backup/claude-3-5-sonnet`）指定，未传则走 `default` 第一个；`load_balance=true` 时随机选 `default` 一个（与 `model` 互斥，`model` 优先）；`default`失败随机切 `backup`，`backup`失败再随机重试。

### 场景决策模型配置（可选）

`POST /api/v1/image/decision` 使用 Gemini Decision 1（Decisions API），只做场景分类与风险评分、不生成文本，因此没有 keywords/description，也没有主备切换与 temperature/top_p：

```toml
[ai.decision]
enabled = false                                        # 未启用时该接口返回 decision model is not configured，不影响其它接口
base_url = "https://generativelanguage.googleapis.com" # 可带 /v1beta，也支持兼容网关
api_key = "your-gemini-api-key"                        # 请求头 x-goog-api-key
model = "gemini-decision-1"
resolution = "high"                                    # low | medium | high | ultra_high（影响 token 成本与细节识别）
timeout = 30                                           # 单次调用超时（秒），<=0 回退 30s
```

> 旧部署的 `data/config.toml` 不会被自动改写，需手工补上该段后重启；缺失时仅该接口不可用。

## Web 管理台（/admin）

浏览器访问 `http://<监听地址>/admin` 即可管理配置，无需手工编辑 `config.toml`：

- **首次访问**：提示初始化管理员账号与密码（密码以 bcrypt 哈希存储），保存后自动登录；
- **已初始化**：直接显示登录页，登录后进入管理页；
- **可管理内容**：基础配置（监听地址、运行模式、AI 超时/重试/温度等）、图像审核（主力/备用渠道 default、backup）、风险审核（决策模型）、对外 API 密钥（增删 + 鉴权开关）、管理员密码；
- **接口调试**：独立页面，可填图片 URL 或直接上传文件（前端转 base64），选择接口/模型/鉴权后一键发送并查看真实返回；下方实时生成等价的 curl 命令，可复制到终端复现；
- **写回方式**：按分区保存——「基础配置 / 图像审核 / 风险审核 / API 密钥」四个页面各自调用 `PUT /admin/api/config/{section}`，只提交并覆盖本分区的字段，其余分区不受影响；落盘时仍是整体重写 `data/config.toml`（覆盖式，不保留原文件注释），写前用同一套规则校验合并后的完整配置；文件权限收紧为 `0600`；
- **生效方式**：除管理员账号/密码外，其余配置需**重启服务**生效（页面会提示「有改动待重启生效」）。

页面资源（HTML/CSS/JS，含 [oat](https://oat.ink/) 的 `oat.min.css` / `oat.min.js`，MIT 协议）全部通过 `go:embed` 打进二进制：部署只需一个可执行文件，离线/内网环境同样可用（首次运行会在当前目录自建 `data/` 存放配置与日志）。

## 快速开始

```bash
make run          # 或 go run ./cmd/server
```

### 前端调试（改完刷新即可，无需重启）

管理台资源默认内嵌在二进制里；开发时用 `./scripts/dev.sh` 启动，脚本会自动设置 `ZTAG_WEB_DIR=internal/webui/web`，让服务直接读磁盘文件：

```bash
./scripts/dev.sh                       # 改 HTML/CSS/JS → 刷新浏览器即生效
ZTAG_WEB_DIR= ./scripts/dev.sh         # 显式关闭，退回内嵌资源
```

- 仅开发用：不设置 `ZTAG_WEB_DIR` 时，发布二进制行为不变（资源仍然内嵌）；
- 磁盘模式下响应带 `Cache-Control: no-store`，不需要 Ctrl+F5 清缓存；
- 目录不存在会自动回退内嵌并在 `data/logs/` 记录一条 WARN，不会启动失败；
- 改 `.go` 文件仍然需要重启（`./scripts/dev.sh` 会重新编译）。

服务默认监听 `:8080`，启动后：

```bash
# 健康检查
curl http://127.0.0.1:8080/healthz

# 图片分析（骨架阶段返回模拟结果）
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -d '{"image_url": "https://example.com/demo.jpg"}'

# 场景决策：只返回场景分类与 0~100 风险评分
curl -X POST http://127.0.0.1:8080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -d '{"image_url": "https://example.com/demo.jpg"}'
```

场景决策返回：

```json
{ "code": 200, "msg": "", "data": { "category": "porn", "score": 87, "elapsed_ms": 312 } }
```

- `category`：`normal | porn | suggestive | gore | violence | politics | gambling | drugs | terror | fraud | other_risk | blocked | prohibited`
- `score`：0~100 风险分（越大越危险）。自然判定与普通拦截上限都是 **99**；**100 只表示极危拦截**（`category = prohibited`，通常是 CSAM），可直接用来触发极危处置流程。详见 `docs/image-decision.md`
- `elapsed_ms`：本次请求处理总耗时（毫秒，含图片下载/压缩与模型调用；不含中间件）

开启鉴权后需携带 API Key：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <data/config.toml 中配置的 key>" \
  -d '{"image_url": "https://example.com/demo.jpg"}'
```

### 统一响应格式

所有接口返回统一格式 `{ code, msg, data }`，`code = 200` 表示成功，失败一律 `code = -1000`（msg 携带英文原因）。图片分析返回 `keywords` / `description`（SEO 用途，中文）与 `classification`（分类明细）：

```json
{
  "code": 200,
  "msg": "",
  "data": {
    "keywords": ["自然风景", "蓝天", "白云"],
    "description": "蓝天白云下的宁静自然风景。",
    "classification": {
      "category": "normal",
      "score": 0.98,
      "risk_level": "low",
      "risk_reason": "未检测到敏感内容。"
    },
    "model_id": "gpt-4o-mini",
    "elapsed_ms": 126
  }
}
```

- `keywords`：图片 SEO 关键词（3-5 个，中文）；`description`：SEO 描述（≤150 字，中文）
- `classification.category`：图片类型，固定枚举 `normal | porn | suggestive | gore | violence | politics | gambling | drugs | terror | other_risk`（英文枚举值，不做翻译）
- `classification.score`：该图片类型的匹配分数（0.0 ~ 1.0）
- `classification.risk_level`：综合风险 `low | medium | high`；`risk_reason`：判定依据概述（中文，人工复审用）
- `model_id`：实际处理该图的大模型标识（含主备切换后的真实模型）；`elapsed_ms`：本次请求处理总耗时（毫秒，含图片下载与 AI 调用）

### 图片校验与预处理策略（图片分析 / 场景决策共用）

请求经 `image_url`（或 `image_base64`）提交图片，后端对 URL 图片执行：

1. **URL 格式校验**：必须为 http/https 且带主机名；
2. **下载与头部校验**：单次 GET，读取 body 前先校验状态码、`Content-Length`（必须存在且 ≤ `ai.max_image_bytes`，默认 10MB）与 `Content-Type`（缺失时按 URL 后缀兜底），通过后才按上限读取内容；
3. **SSRF 防护**：默认拦截内网/保留地址（`ai.allow_private_urls = true` 可放行，仅本地联调用）；
4. **图片压缩**：下载后对 `image/jpeg`、`image/png`、`image/bmp` 解码并按需等比缩放（最长边超过 2048px 时缩小，宽高比不变），统一编码为 webp（quality 80）；静态 webp 输入同样等比缩放后重编码（动画 webp、gif、avif 原样透传）；处理失败自动回退原图，不阻断审核。图片处理链路为纯 Go 实现（[imaging](https://github.com/disintegration/imaging) 缩放 + [deepteams/webp](https://github.com/deepteams/webp) 编码），无 cgo/libvips 依赖；
5. **传给 AI**：预处理完成（含 webp 转换）后以 base64 data URI 送大模型分析；两个接口共用同一并发信号量，总并发受 `server.workers` 限制。

> 说明：webp 压缩降低的是传输体积；token 消耗主要取决于图片像素尺寸——预处理已将最长边限制在 2048px 以内（另设 4000 万总像素上限防解压炸弹），从源头控制了 token 与内存开销。

## Docker 部署

镜像由 GitHub Actions 在推送 `v*` tag 后自动构建，推送到 GHCR（仅 `linux/amd64`）：

```bash
# 拉取（首次使用若包仍为私有，需先在 GitHub Packages 设置中改为 public，或 docker login ghcr.io）
docker pull ghcr.io/helloxz/ztag:latest

# 运行（配置持久化到宿主机 ./data）
docker run -d --name ztag -p 18080:18080 \
  -v "$PWD/data:/app/data" -e TZ=Asia/Shanghai \
  ghcr.io/helloxz/ztag:latest

# 或直接使用仓库内 compose.yaml
docker compose up -d
```

镜像 tag 跟随版本号：`v0.2.3` / `0.2.3` / `0.2` / `latest`（预发布版本不打 `latest`）。补推某个历史版本的镜像：Actions → `Release` → Run workflow，填入已存在的 tag。

本地构建（建议锁定版本，否则构建时会解析 GitHub 上最新的 release）：

```bash
docker build --build-arg VERSION=v0.2.3 -t ztag:v0.2.3 .
```

## 中间件说明

| 中间件 | 作用 | 挂载范围 |
|---|---|---|
| Recovery | panic 兜底，返回统一错误 | 全局 |
| AccessLog | 请求访问日志 | 全局 |
| Auth | API Key 鉴权（可配置开关） | /api/v1 |
| RateLimit | 限流（当前占位透传，后续接入） | /api/v1 |
| AdminSession | 管理台登录会话校验（Cookie，未登录返回 401） | /admin/api（需登录的接口） |

## 路线图（分阶段落地）

- [x] 骨架搭建：框架分层、路由/中间件/助手函数分离、hello world 可运行
- [x] 图片识别接口：URL 校验 / header 探测（≤10MB、MIME、SSRF）/ 下载转 base64 / 主备切换 / goai 真实调用 / 二层分类结构化输出
- [x] 场景决策接口：Gemini Decision 1（Decisions API）场景分类 + 0~100 风险评分；普通安全拦截映射 blocked/99，极危拦截（prohibited_content/blocklist）映射 prohibited/100 并落 Error 日志
- [x] Web 管理台（/admin）：管理员初始化/登录，在线管理配置、渠道、API 密钥并写回 config.toml
- [ ] 端到端真实模型验证（配置 API Key 后）
- [ ] 限流、容量与监控
- [ ] 分类阈值（score 截断值）与风险等级策略可配置化