# ZTAG API 文档

统一约定：

- 默认地址：`http://127.0.0.1:18080`
- 请求/响应：`application/json`
- 统一响应：`{"code":200,"msg":"","data":...}`；失败 `code=-1000`，`msg` 为英文原因，`data` 为 `null`
- 业务失败仍返回 HTTP 200；鉴权失败返回 HTTP 401
- 对外接口挂在 `/api/v1`，需 API Key（`[auth].enabled = true` 时）：

```
Authorization: Bearer <api_key>
# 或
X-API-Key: <api_key>
```

---

## 1. 图片分析

`POST /api/v1/image/analyze`

识别图片内容，输出关键词、描述、分类与风险等级。

### 请求

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `image_url` | string | 二选一 | 图片 HTTP(S) 地址，服务端下载 |
| `image_base64` | string | 二选一 | 图片 base64（可带 `data:` 前缀） |
| `model` | string | 否 | `channel/modelId`，channel 仅 `default` / `backup`，如 `default/gpt-4o-mini` |
| `load_balance` | bool | 否 | `model` 为空时生效，`true` 从 `default` 随机选模型 |

```json
{
  "image_url": "https://example.com/demo.jpg",
  "model": "default/gpt-4o-mini"
}
```

### 响应 data

| 字段 | 类型 | 说明 |
|---|---|---|
| `keywords` | string[] | 关键词 3~5 个，简体中文 |
| `description` | string | 描述，≤150 字，简体中文 |
| `classification.category` | string | 图片分类，见下表 |
| `classification.score` | number | 分类匹配分 0.0~1.0 |
| `classification.risk_level` | string | `low` / `medium` / `high` |
| `classification.risk_reason` | string | 判定依据，简体中文 |
| `model_id` | string | 实际使用的模型 |
| `elapsed_ms` | number | 总耗时（毫秒） |

```json
{
  "code": 200,
  "msg": "",
  "data": {
    "keywords": ["自然风景", "蓝天", "白云"],
    "description": "蓝天白云下的自然风景。",
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

图片分类：`normal` `porn` `suggestive` `gore` `violence` `politics` `gambling` `drugs` `terror` `other_risk`

### 图片校验

`image_url` 方式会校验：URL 必须 http(s)、大小 ≤ `ai.max_image_bytes`（默认 10MB）、MIME 必须为图片、默认拦截内网地址（SSRF）。jpg/png/bmp 会转 webp 压缩后再送模型。

---

## 2. 风险审核（图片）

`POST /api/v1/image/decision`

调用决策模型，只返回场景分类与 0~100 风险分。

### 请求

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `image_url` | string | 二选一 | 图片地址 |
| `image_base64` | string | 二选一 | 图片 base64 |
| `model` | string | 否 | 决策渠道 id，不传按配置顺序轮询 |

### 响应 data

| 字段 | 类型 | 说明 |
|---|---|---|
| `category` | string | 分类，见下表 |
| `score` | int | 风险分 0~100，越大越危险 |
| `model_id` | string | 实际渠道 id |
| `elapsed_ms` | number | 总耗时（毫秒） |

```json
{
  "code": 200,
  "msg": "",
  "data": {
    "category": "porn",
    "score": 87,
    "model_id": "gemini-main",
    "elapsed_ms": 312
  }
}
```

图片分类：`normal` `porn` `suggestive` `gore` `violence` `politics` `gambling` `fraud` `drugs` `terror` `other_risk` `blocked` `prohibited`

- `score=99`：自然判定上限或普通安全拦截（`blocked`）
- `score=100` 且 `category=prohibited`：极危拦截（通常为 CSAM），需按极危流程处置

---

## 3. 风险审核（文本）

`POST /api/v1/text/decision`

### 请求

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `text` | string | 是 | 待审核文本，长度 ≤ `ai.decision.max_text_chars`（默认 4000） |
| `model` | string | 否 | 决策渠道 id，不传轮询 |

```json
{ "text": "加微信 xxx 领福利，稳赚不赔", "model": "openai-main" }
```

### 响应

同图片审核：`category` / `score` / `model_id` / `elapsed_ms`。

文本分类：`normal` `spam` `ads` `porn` `politics` `abuse` `fraud` `illegal` `other_risk` `blocked` `prohibited`

---

## 4. 错误响应

失败统一：

```json
{ "code": -1000, "msg": "image_url or image_base64 must be provided", "data": null }
```

常见 `msg`：

| 场景 | msg |
|---|---|
| 请求体非法 | `invalid request body: <detail>` |
| 图片参数缺失 | `image_url or image_base64 must be provided` |
| 文本为空 | `text must not be empty` |
| 文本超长 | `text is too long (max N characters)` |
| 模型格式错误 | `model must be in format "channel/model", got: "..."` |
| 图片 URL 非法 | `invalid image URL format` / `image URL must be http(s), got: "..."` |
| 图片超限/非图片 | `image too large (exceeds N bytes)` / `not an image (Content-Type: "...")` |
| SSRF 拦截 | `url target "..." is not allowed (private/reserved address)` |
| 决策模型未配置 | `decision model is not configured` |
| 决策渠道不存在 | `decision model "x" not found, available: ...` |
| 决策全部失败 | `decision analysis failed` |
| 分析全部失败 | `AI analysis failed on both channels` |
| 鉴权失败（HTTP 401） | `unauthorized or invalid API key` |
