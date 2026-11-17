# ZTAG 场景决策接口使用文档

> 对应接口：`POST /api/v1/image/decision`
> 功能：调用 Gemini Decision 1（Decisions API）对图片做**场景分类 + 风险评分**。
> 与 `/api/v1/image/analyze` 的区别：模型不生成任何文本，因此**没有** `keywords` / `description` / `risk_reason`，只返回场景与 0~100 风险分。

---

## 1. 基本信息

| 项 | 值 |
|---|---|
| 请求方法 | `POST` |
| 请求路径 | `/api/v1/image/decision` |
| Content-Type | `application/json` |
| 鉴权 | 可选（同 `/image/analyze`，见第 3 节） |
| 响应格式 | 统一 `{ code, msg, data }` |

---

## 2. 请求参数

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `image_url` | string | 二选一 | 图片 HTTP(S) 地址，由服务端下载 |
| `image_base64` | string | 二选一 | 图片 base64 内容（可带 `data:` 前缀） |

> 与图片分析接口不同，本接口**不接受** `model` / `load_balance`：模型与渠道完全由 `[ai.decision]` 配置决定。

请求示例：

```json
{ "image_url": "https://example.com/demo.jpg" }
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
{ "code": 200, "msg": "", "data": { "category": "porn", "score": 87, "elapsed_ms": 312 } }
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `category` | string | 场景分类，固定枚举（见 4.2） |
| `score` | number(int) | 风险分 **0~100**，越大风险越高 |
| `elapsed_ms` | number(int) | 请求处理总耗时（毫秒）：含请求解析、并发信号量排队、图片下载/校验/压缩、模型调用（含重试）；不含 gin 中间件耗时 |

### 4.2 分类枚举（category）

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
| `blocked` | **服务端合成**：输入被供应商安全策略拦截（非模型分类），风险分固定 99 |
| `prohibited` | **服务端合成**：极危拦截（`prohibited_content` / `blocklist`，通常为 CSAM），风险分固定 100 |

> 分类由 **11 个**独立的是/否问题取概率最大者得到；概率并列时按固定优先级 `porn > gore > violence > terror > politics > gambling > fraud > drugs > suggestive > other_risk > normal`。
>
> 二维码不再作为独立分类（识别准确率不足）；扫码诈骗类图片由 `fraud` 覆盖。

### 4.3 风险分（score）

`score` 来自一个有序 rubric 问题（0~4 级）的加权期望值，归一化后 ×100：

| 等级 | 含义 | 约对应分值 |
|---|---|---|
| 0 | 无风险，可直接放行 | 0 |
| 1 | 低风险（轻微擦边、暴力暗示） | ~25 |
| 2 | 中风险，建议人工复核 | ~50 |
| 3 | 高风险，建议拦截 | ~75 |
| 4 | 极高风险，必须拦截 | ~100 → **钳制为 99** |

- 模型自然判定上限固定为 **99**，正常图片通常在 **0~10**；
- `blocked`（普通拦截）固定 **99**；
- `prohibited`（极危拦截）固定 **100** —— 因此 **`score == 100` 可直接作为极危处置流程的触发器**。

### 4.4 被供应商安全策略拦截时

Decisions API 遇到违规输入会直接返回 HTTP 400。服务端不重试，按 `error.code` 分成两类：

| 供应商 code | 返回 | 日志级别 | 处置 |
|---|---|---|---|
| `prohibited_content` / `blocklist` | `{"category":"prohibited","score":100}` | Error | 极危流程：停用来源 → 不落盘 → 按当地法规上报 → 留痕 |
| `safety` / `image_safety` / `content_blocked` / `jailbreak` | `{"category":"blocked","score":99}` | Warn | 按高风险图处理，记录来源 |

```json
{ "code": 200, "msg": "", "data": { "category": "blocked", "score": 99 } }
{ "code": 200, "msg": "", "data": { "category": "prohibited", "score": 100 } }
```

> 注意：两者都是 **fail-closed 的推导结论**，不等于模型判定该图违规（被误拦的正常图也会落到 99/100）。`prohibited` 表示"Google 判定为极危内容（通常为 CSAM）"，**不代表已经确认为 CSAM**，最终定性需人工/法务判断；日志中的 `provider_code` 是唯一依据。

---

## 5. 图片校验与预处理

与 `/api/v1/image/analyze` 完全共用同一条链路（见 `docs/image-analyze.md` 第 4 节）：

1. URL 格式校验（http/https + 主机名）；
2. 下载时先校验状态码 / `Content-Length`（≤ `ai.max_image_bytes`）/ `Content-Type`，通过后再按上限读取；
3. SSRF 防护（默认拦截内网/保留地址）；
4. jpg/png/bmp → webp（quality 80），最长边超过 2048px 等比缩小；gif/avif/动画 webp 原样透传；压缩失败回退原图；
5. 以 base64 内联方式作为 `image` 块送入 Decisions API，`resolution` 取 `[ai.decision].resolution`。

> 两个接口共用同一个并发信号量，总并发受 `server.workers` 限制。

---

## 6. 错误响应

失败统一 `code = -1000`，`msg` 为英文原因，`data = null`：

| 场景 | msg |
|---|---|
| 参数缺失 | `image_url or image_base64 must be provided` |
| 请求体非法 | `invalid request body: <detail>` |
| URL 非法 | `invalid image URL format` / `image URL must be http(s), got: ...` |
| 图片超限 / 非图片 / SSRF | 同 `/image/analyze` |
| 未配置决策模型 | `decision model is not configured` |
| 模型调用失败（重试后） | `decision analysis failed` |
| 模型返回缺字段 | `decision response missing category answer: <key>` / `decision response missing risk severity score` |
| 鉴权失败（HTTP 401） | `unauthorized or invalid API key` |

> 供应商的原始错误只写日志，不返回给调用方。

---

## 7. 调用示例（curl）

```bash
# URL 方式
curl -X POST http://127.0.0.1:8080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -d '{"image_url":"https://example.com/demo.jpg"}'

# base64 方式
curl -X POST http://127.0.0.1:8080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -d '{"image_base64":"<base64 内容>"}'

# 开启鉴权后
curl -X POST http://127.0.0.1:8080/api/v1/image/decision \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <your-api-key>" \
  -d '{"image_url":"https://example.com/demo.jpg"}'
```

---

## 8. 配置

```toml
[ai.decision]
enabled = false                                        # 未启用时接口返回 decision model is not configured
base_url = "https://generativelanguage.googleapis.com" # 可带 /v1beta，也支持兼容网关
api_key = "your-gemini-api-key"                        # x-goog-api-key
model = "gemini-decision-1"
resolution = "high"                                    # low | medium | high | ultra_high
timeout = 30                                           # 秒；<=0 回退 30s
```

复用的全局配置：`ai.max_retries`（429/5xx/网络错误重试次数）、`ai.max_image_bytes`、`ai.image_timeout`、`ai.allow_private_urls`、`ai.mock`（true 时返回固定 `normal/3`，便于本地联调）。

> 旧部署的 `data/config.toml` 不会被自动改写：需手工补上 `[ai.decision]` 段并重启。缺失该段不会阻断启动，仅本接口不可用。
