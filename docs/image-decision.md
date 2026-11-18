# ZTAG 风险审核接口使用文档

> 对应接口：
> - `POST /api/v1/image/decision`（图片）
> - `POST /api/v1/text/decision`（文本）
>
> 功能：调用**决策模型**做场景分类 + 风险评分，不生成任何文本，因此没有 `keywords` / `description` / `risk_reason`。
> 支持多个决策渠道，两种协议：Gemini Decision 1（native）与 OpenAI 兼容 Decisions API。

---

## 1. 基本信息

| 项 | 图片 | 文本 |
|---|---|---|
| 请求方法 | `POST` | `POST` |
| 请求路径 | `/api/v1/image/decision` | `/api/v1/text/decision` |
| Content-Type | `application/json` | `application/json` |
| 鉴权 | 可选（同 `/image/analyze`，见第 3 节） | 同左 |
| 响应格式 | 统一 `{ code, msg, data }` | 同左 |

---

## 2. 请求参数

### 2.1 图片接口

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `image_url` | string | 二选一 | 图片 HTTP(S) 地址，由服务端下载 |
| `image_base64` | string | 二选一 | 图片 base64 内容（可带 `data:` 前缀） |
| `model` | string | 否 | 决策渠道 id（见 `[[ai.decision.channels]]`）；不传则按配置顺序轮询 |

### 2.2 文本接口

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `text` | string | 是 | 待审核文本，长度受 `ai.decision.max_text_chars`（默认 4000）限制 |
| `model` | string | 否 | 决策渠道 id；不传则轮询 |

请求示例：

```json
{ "image_url": "https://example.com/demo.jpg" }
{ "text": "加微信 xxx 领福利，稳赚不赔", "model": "openai-main" }
```

---

## 3. 鉴权

与图片分析接口一致：`data/config.toml` 的 `[auth].enabled = true` 时需携带

```
Authorization: Bearer <api_key>
```

未携带或 Key 错误返回 HTTP 401、`code = -1000`。

---

## 4. 响应结构

### 4.1 成功响应

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

| 字段 | 类型 | 说明 |
|---|---|---|
| `category` | string | 场景分类，图片与文本枚举不同（见 4.2） |
| `score` | number(int) | 风险分 **0~100**，越大风险越高 |
| `model_id` | string | 实际处理本次请求的决策渠道 id（轮询或指定后的真实渠道） |
| `elapsed_ms` | int | 请求处理总耗时（毫秒）；图片接口含下载/校验/压缩，文本接口只有模型调用 |

### 4.2 分类枚举（category）

**图片**（11 类，互不混用）：

| 枚举值 | 含义 |
|---|---|
| `normal` | 正常，无敏感/违规内容 |
| `porn` | 露骨色情 |
| `suggestive` | 擦边/性感（未达露骨色情） |
| `gore` | 血腥（伤口、血迹、尸体等） |
| `violence` | 暴力（打斗、武器威胁、虐待等） |
| `politics` | 政治敏感 |
| `gambling` | 赌博/博彩 |
| `fraud` | 诈骗/欺诈（虚假中奖、刷单返利、钓鱼、虚假理财、扫码转账等） |
| `drugs` | 毒品/违禁药物 |
| `terror` | 恐怖主义/极端主义 |
| `other_risk` | 其他违法或高风险内容 |

**文本**（9 类）：

| 枚举值 | 含义 |
|---|---|
| `normal` | 正常 |
| `spam` | 垃圾信息（刷屏、重复、乱码灌水） |
| `ads` | 广告营销（推广、引流、联系方式） |
| `porn` | 色情低俗 |
| `politics` | 政治敏感 |
| `abuse` | 辱骂攻击（侮辱、歧视） |
| `fraud` | 诈骗欺诈（诱导转账、虚假中奖、钓鱼） |
| `illegal` | 违法违禁（毒品、赌博、枪支等） |
| `other_risk` | 其他风险 |

**两种模式共用**的两个服务端合成值：

| 枚举值 | 含义 |
|---|---|
| `blocked` | 供应商安全拦截或模型拒答（refusal），风险分固定 99 |
| `prohibited` | 极危拦截（`prohibited_content` / `blocklist`，通常为 CSAM），风险分固定 100 |

> 分类由「每个分类一个是否问题 + 一个风险等级评分问题」得到；概率并列时按固定优先级（敏感类目优先于 `normal`）判定。

### 4.3 风险分（score）

`score` 来自一个有序 rubric 问题（0~4 级）的加权期望值，归一化后 ×100：

| 等级 | 含义 | 约对应分值 |
|---|---|---|
| 0 | 无风险，可直接放行 | 0 |
| 1 | 低风险 | ~25 |
| 2 | 中风险，建议人工复核 | ~50 |
| 3 | 高风险，建议拦截 | ~75 |
| 4 | 极高风险，必须拦截 | ~100 → **钳制为 99** |

- 自然判定上限固定 **99**；
- `blocked`（普通拦截 / 拒答）固定 **99**；
- `prohibited`（极危拦截）固定 **100**，可直接作为极危处置流程的触发器。

### 4.4 被拦截或模型拒答时

| 来源 | 返回 | 日志级别 | 处置 |
|---|---|---|---|
| `prohibited_content` / `blocklist` | `{"category":"prohibited","score":100}` | Error | 极危流程：停用来源 → 不落盘 → 按当地法规上报 → 留痕 |
| `safety` / `image_safety` / `content_blocked` / `jailbreak` / `content_policy_violation` | `{"category":"blocked","score":99}` | Warn | 按高风险处理，记录来源 |
| OpenAI 协议 `refusal`（模型拒答） | `{"category":"blocked","score":99}` | Warn | 同上 |

> 这些是 **fail-closed 的推导结论**，不等于模型判定内容违规；日志里的 `provider_code` 是唯一依据。

---

## 5. 图片校验与预处理

与 `/api/v1/image/analyze` 完全共用同一条链路（见 `docs/image-analyze.md` 第 4 节）：

1. URL 格式校验（http/https + 主机名）；
2. 下载时先校验状态码 / `Content-Length`（≤ `ai.max_image_bytes`）/ `Content-Type`，通过后再按上限读取；
3. SSRF 防护（默认拦截内网/保留地址）；
4. jpg/png/bmp → webp（quality 80），最长边超过 2048px 等比缩小；gif/avif/动画 webp 原样透传；压缩失败回退原图；
5. 按渠道协议送入决策 API：Gemini 用 `image` 块 + `resolution`，OpenAI 兼容协议用 `input_image` + base64 data URL（`resolution` 映射为 `detail`）。

> 文本接口不做图片预处理，直接送模型，且文本内容不落日志。

---

## 6. 错误响应

失败统一 `code = -1000`，`msg` 为英文原因，`data = null`：

| 场景 | msg |
|---|---|
| 参数缺失（图片） | `image_url or image_base64 must be provided` |
| 参数缺失（文本） | `text must not be empty` |
| 文本超长 | `text is too long (max N characters)` |
| 请求体非法 | `invalid request body: <detail>` |
| URL 非法 | `invalid image URL format` / `image URL must be http(s), got: ...` |
| 图片超限 / 非图片 / SSRF | 同 `/image/analyze` |
| 未配置决策模型 | `decision model is not configured` |
| 指定的渠道不存在 | `decision model "<id>" not found, available: <ids>` |
| 所有尝试都失败 | `decision analysis failed` |
| 模型返回缺字段 | `decision response missing category answer: <name>` / `decision response missing risk severity score` |
| 鉴权失败（HTTP 401） | `unauthorized or invalid API key` |

> 供应商的原始错误只写日志，不返回给调用方。

---

## 7. 调用示例（curl）

```bash
# 图片：URL 方式
curl -X POST http://127.0.0.1:18080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -d '{"image_url":"https://example.com/demo.jpg"}'

# 图片：base64 方式
curl -X POST http://127.0.0.1:18080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -d '{"image_base64":"<base64 内容>"}'

# 文本：不指定渠道（轮询）
curl -X POST http://127.0.0.1:18080/api/v1/text/decision \
  -H "Content-Type: application/json" \
  -d '{"text":"加微信 xxx 领福利，稳赚不赔"}'

# 文本：指定决策渠道
curl -X POST http://127.0.0.1:18080/api/v1/text/decision \
  -H "Content-Type: application/json" \
  -d '{"text":"……","model":"openai-main"}'

# 开启鉴权后
curl -X POST http://127.0.0.1:18080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <your-api-key>" \
  -d '{"image_url":"https://example.com/demo.jpg"}'
```

---

## 8. 配置

```toml
[ai.decision]
enabled = false          # 总开关：false 时两个决策接口都返回 decision model is not configured
timeout = 30             # 单次调用超时（秒），<=0 回退 30s
max_text_chars = 4000    # 文本审核字符上限

# 注意：启用总开关前，至少要把一个渠道的 enabled 改为 true，
# 否则配置校验失败（ai.decision.channels must have at least one enabled channel）。
[[ai.decision.channels]]
id = "gemini-main"       # 渠道 id：调用时 model 填这个值；需唯一
type = "gemini-decision" # gemini-decision | openai-decision
enabled = false          # 默认关闭，需要时改成 true
base_url = "https://generativelanguage.googleapis.com"
api_key = "your-gemini-api-key"
model = "gemini-decision-1"
resolution = "high"      # low | medium | high | ultra_high

# 第二个渠道示例（默认注释）：需要多渠道轮询/兜底时取消注释
# [[ai.decision.channels]]
# id = "openai-main"
# type = "openai-decision"
# enabled = true
# base_url = "https://ai-gateway.vercel.sh/v1"   # 实际请求 {base_url}/decisions
# api_key = "your-ai-gateway-key"
# model = "openai/gpt-6-luna-decisions"
# resolution = "high"      # 映射为 detail：low→low / medium→auto / high→high / ultra_high→original
```

路由与兜底：

- 请求带 `model` → 精确匹配渠道 id（只在该渠道上尝试）；
- 不带 `model` → 按配置顺序**轮询**；
- 单次失败：还有其它渠道就换下一个再试一次，只有一个渠道就用它重试一次；仍失败返回 `decision analysis failed`；
- 所有尝试的渠道 id 与错误都会写日志，便于排查。

复用的全局配置：`ai.max_image_bytes`、`ai.image_timeout`、`ai.allow_private_urls`、`ai.mock`（true 时两个接口都返回固定 `normal/3`，便于本地联调）。
