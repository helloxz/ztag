# ZTAG

基于大模型的图片内容识别服务：调用后端 AI 大模型识别图片内容，提取关键词与描述，并执行内容分类（色情、政治、血腥、暴力等），以统一 JSON API 的形式对外提供调用。

## 技术栈

- 语言 / Web 框架：Go + [gin](https://github.com/gin-gonic/gin)
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
│   └── helper/               # ★ 助手函数：统一响应封装、错误转换
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
## 快速开始

```bash
make run          # 或 go run ./cmd/server
```

服务默认监听 `:8080`，启动后：

```bash
# 健康检查
curl http://127.0.0.1:8080/healthz

# 图片分析（骨架阶段返回模拟结果）
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -d '{"image_url": "https://example.com/demo.jpg"}'
```

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

### 图片校验与预处理策略（POST /api/v1/image/analyze）

请求经 `image_url`（或 `image_base64`）提交图片，后端对 URL 图片执行：

1. **URL 格式校验**：必须为 http/https 且带主机名；
2. **响应头校验**（GET 响应头，不下载内容）：`Content-Length` 存在且 ≤ `ai.max_image_bytes`（默认 10MB）；`Content-Type` 必须为图片 MIME（缺失时按 URL 后缀兜底，如 `.png`）；
3. **SSRF 防护**：默认拦截内网/保留地址（`ai.allow_private_urls = true` 可放行，仅本地联调用）；
4. **webp 压缩**：下载后对 `image/jpeg`、`image/png`、`image/bmp` 统一转为 webp（quality 80，基于 libvips/bimg）以减小请求体积；**其余格式（如 gif）原样透传**；压缩失败自动回退原图，不阻断审核；
5. **传给 AI**：预处理完成（含 webp 转换）后以 base64 data URI 送大模型分析。

> 说明：webp 压缩降低的是传输体积；token 消耗主要取决于图片像素尺寸（当前按配置不限制像素）。

## 中间件说明

| 中间件 | 作用 | 挂载范围 |
|---|---|---|
| Recovery | panic 兜底，返回统一错误 | 全局 |
| AccessLog | 请求访问日志 | 全局 |
| Auth | API Key 鉴权（可配置开关） | /api/v1 |
| RateLimit | 限流（当前占位透传，后续接入） | /api/v1 |

## 路线图（分阶段落地）

- [x] 骨架搭建：框架分层、路由/中间件/助手函数分离、hello world 可运行
- [x] 图片识别接口：URL 校验 / header 探测（≤10MB、MIME、SSRF）/ 下载转 base64 / 主备切换 / goai 真实调用 / 二层分类结构化输出
- [ ] 端到端真实模型验证（配置 API Key 后）
- [ ] 限流、容量与监控
- [ ] 分类阈值（score 截断值）与风险等级策略可配置化