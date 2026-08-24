# ZTAG 图片识别接口使用文档

> 对应接口：`POST /api/v1/image/analyze`
> 功能：调用后端大模型识别图片内容，提取 SEO 关键词/描述，并输出内容分类（色情、政治、血腥、暴力等）与风险等级。

---

## 1. 基本信息

| 项 | 值 |
|---|---|
| 请求方法 | `POST` |
| 请求路径 | `/api/v1/image/analyze` |
| Content-Type | `application/json` |
| 鉴权 | 可选（见第 3 节，默认关闭） |
| 响应格式 | 统一 `{ code, msg, data }` |

---

## 2. 请求参数

请求体为 JSON，字段如下：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `image_url` | string | 二选一 | 图片 HTTP(S) 地址，由服务端下载 |
| `image_base64` | string | 二选一 | 图片 base64 内容（直传，不经过 URL 下载） |
| `model` | string | 否 | 格式 `channel/modelId`，channel 仅 `default`/`backup`，modelId 可含 `/`；缺省走 `default` 第一个模型；`load_balance`为 true 时随机选 default 模型（与指定 model 互斥，model 优先） |
| `load_balance` | boolean | 否 | 仅当 `model` 为空时生效，`true` 时从 `default` 渠道随机选择一个模型；默认 `false` 取第一个 |

> `image_url` 与 `image_base64` 至少提供一个，同时为空返回错误。
> `model` 非空时必须含 `/`（如 `default/gpt-4o-mini`、`backup/claude-3-5-sonnet`、`default/openai/gpt-4o`），否则返回 `model must be in format "channel/model"`。

请求示例：

```json
{
  "image_url": "https://example.com/demo.jpg",
  "model": "default/gpt-4o-mini"
}
```
---

## 3. 鉴权

由配置文件 `data/config.toml` 的 `[auth]` 段控制：

| 配置 | 说明 |
|---|---|
| `enabled = true` | 开启鉴权，请求需携带 API Key |
| `enabled = false` | 关闭鉴权，**匿名可访问** |
| `api_keys = [...]` | 允许访问的 Key 列表 |

开启鉴权后，请求头需携带：

```
Authorization: Bearer <api_key>
```

未携带或 Key 错误时返回 HTTP 401，`code = -1000`。

---

## 4. 图片校验与预处理流程（`image_url` 方式）

服务端对 URL 图片按顺序执行以下校验，任一不通过即拒绝（`code = -1000`）：

1. **URL 格式校验**：必须为 `http`/`https` 且带主机名；
2. **响应头探测**（GET 只读响应头，不下载内容）：
   - `Content-Length` 必须存在（chunked 等缺失时拒绝）且 ≤ `ai.max_image_bytes`（默认 10MB）；
   - `Content-Type` 必须是图片 MIME：`image/jpeg`、`image/png`、`image/gif`、`image/webp`、`image/bmp`、`image/avif`；
   - Content-Type 缺失时按 URL 后缀兜底（`.jpg/.jpeg/.png/.gif/.webp/.bmp/.avif`）；
3. **SSRF 防护**：默认拦截内网/保留地址（如 `127.0.0.1`、`localhost`）；本地联调可设 `ai.allow_private_urls = true` 放行；
4. **webp 压缩**：`image/jpeg`、`image/png`、`image/bmp` 下载后转 webp（quality 80）以减小请求体积；**其余格式（如 gif）原样透传**；压缩失败自动回退原图，不阻断；
5. **传给 AI**：预处理后以 base64 data URI 送模型分析。

---

## 5. 响应结构

### 5.1 成功响应

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

### 5.2 data 字段说明

| 字段 | 类型 | 说明 |
|---|---|---|
| `keywords` | string[] | 图片 SEO 关键词，3~5 个，简体中文 |
| `description` | string | 图片 SEO 描述，≤150 字，简体中文 |
| `classification.category` | string | 图片类型，固定枚举（见 5.3） |
| `classification.score` | number | 该图片类型的匹配分数（0.0 ~ 1.0） |
| `classification.risk_level` | string | 综合风险等级：`low` / `medium` / `high` |
| `classification.risk_reason` | string | 判定依据概述，简体中文（人工复审用） |
| `model_id` | string | 实际处理该图的大模型标识（含主备切换后） |
| `elapsed_ms` | number | 请求总耗时（毫秒，含图片下载与 AI 调用） |

### 5.3 分类枚举（category）

| 枚举值 | 含义 |
|---|---|
| `normal` | 正常 |
| `porn` | 色情 |
| `suggestive` | 性感/擦边 |
| `gore` | 血腥 |
| `violence` | 暴力 |
| `politics` | 政治敏感 |
| `gambling` | 赌博 |
| `drugs` | 毒品/违禁 |
| `terror` | 恐怖极端 |
| `other_risk` | 其他风险（模型输出白名单外时归入） |

---

## 6. 错误响应

失败时统一 `code = -1000`，`msg` 为英文原因，`data = null`：

```json
{
  "code": -1000,
  "msg": "image_url or image_base64 must be provided",
  "data": null
}
```

常见 `msg`：

| 场景 | msg |
|---|---|
| 参数缺失 | `image_url or image_base64 must be provided` |
| 请求体非法 | `invalid request body: <detail>` |
| URL 非法 | `invalid image URL format` / `image URL must be http(s), got: ...` |
| 图片超限 | `image too large (exceeds ... bytes)` / `image size unknown (missing Content-Length)` |
| 非图片 | `not an image (Content-Type: ...)` |
| SSRF 拦截 | `url target "..." is not allowed (private/reserved address)` |
| 鉴权失败（HTTP 401） | `unauthorized or invalid API key` |
| AI 全部渠道失败 | `AI analysis failed on both channels` |

---
## 7. 调用示例（curl）

```bash
# 匿名调用（auth.enabled = false）
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -d '{"image_url":"https://example.com/demo.jpg"}'

# 负载均衡（随机选 default 中一个模型）
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -d '{"image_url":"https://example.com/demo.jpg","load_balance":true}'

# 鉴权调用（auth.enabled = true）
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <your-api-key>" \
  -d '{"image_url":"https://example.com/demo.jpg"}'

# base64 直传 + 指定模型（channel/model 格式）
curl -X POST http://127.0.0.1:8080/api/v1/image/analyze \
  -H "Content-Type: application/json" \
  -d '{"image_base64":"<base64 内容>","model":"backup/claude-3-5-sonnet"}'
```
## 8. 行为说明

- **模型选择**：`model="channel/modelId"`按第一个`/`切分校验，未传`model`时`load_balance=false`取`default`第一个，`load_balance=true`随机取`default`一个；`model`与`load_balance`互斥，`model`优先。
- **主备切换**：`default`选中模型失败 → 随机选`backup`一个模型重试；`backup`显式指定失败 → 再随机选`backup`一个重试；仍失败返回 `AI analysis failed on both channels`。
- **耗时字段**：`elapsed_ms` 覆盖请求解析至分析完成的完整耗时；`mock` 模式下（本地联调）耗时通常接近 0。
- **模型输出语言**：`keywords` / `description` / `risk_reason` 为简体中文；`category` / `risk_level` 为固定英文枚举。
- **JSON 稳定性**：请求已启用结构化输出（`response_format=json_schema`）、低采样温度（`temperature=0.2`），并对偶发的截断解析失败自动重试一次。
- **思考模式**：模型 id 命中内置关键词列表（如 `qwen`）时自动携带关闭思考参数；其余模型不传思考参数由模型决定。
## 9. 相关配置（data/config.toml）

| 配置段 | 关键项 | 说明 |
|---|---|---|
| `[server]` | `addr` / `mode` | 监听地址与运行模式 |
| `[auth]` | `enabled` / `api_keys` | 接口鉴权开关与 Key 列表 |
| `[ai]` | `timeout` / `max_retries` / `max_image_bytes` | 超时 / 重试 / 图片大小上限 |
| `[ai]` | `temperature` / `top_p` | 采样参数（输出稳定性） |
| `[ai]` | `mock` / `allow_private_urls` | 本地联调与 SSRF 放行开关 |
| `[[ai.channels]]` | `id` / `type` / `base_url` / `api_key` / `models` | 主备渠道与模型配置 |