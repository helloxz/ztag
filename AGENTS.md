# AGENTS.md

图片内容识别 API 服务：调用后端大模型识别图片，提取关键词/描述并分类（色情、政治、血腥、暴力等），对外提供统一 HTTP API。模块 `github.com/helloxz/ztag`，Go + gin + viper(TOML) + [goai](https://github.com/zendev-sh/goai)。

## Commands
- 编译：`./scripts/build.sh`（输出 `bin/ztag`）或 `make build`
- 开发运行：`./scripts/dev.sh`（go run，可透传 `-config`）；默认从磁盘读管理台资源（`ZTAG_WEB_DIR=internal/webui/web`），改前端文件刷新浏览器即可，无需重启；`ZTAG_WEB_DIR= ./scripts/dev.sh` 可退回内嵌资源
- 检查：`make vet`、`gofmt -l .`
- 配置：`data/config.toml`，首次启动自动从内嵌模板生成（docker 挂载 `data/` 持久化）

## Architecture（依赖单向向下，禁止跨层）
```
router → handler → service → ai
```
- `internal/router`：所有路由统一在这里注册（唯一挂载点）
- `internal/middleware`：Recovery / AccessLog / Auth / RateLimit / AdminSession
- `internal/handler`：表现层——参数解析 + 统一响应（薄层，无业务逻辑）
- `internal/service`：业务编排——校验 → AI 网关调用
- `internal/ai`：多渠道网关（channel 内多模型）；接入真实模型用 `goai.GenerateObject[T]`，当前为 mock。**例外**：风险审核（决策模型）不走 goai（SDK 无 `/decisions` 端点），两套协议各自原生 net/http：`decision_client.go`（Gemini Decisions API）、`decision_openai.go`（OpenAI 兼容 Decisions API），路由/轮询/兜底在 `decision.go`
- `internal/config` / `internal/model` / `internal/helper`：配置、领域模型、通用助手
- `internal/webui`：管理台前端资源（HTML/CSS/JS），`go:embed` 内嵌进二进制

## Conventions（必须遵守）
1. **注释**：关键代码写中文注释；注释保持中文，代码标识符/消息保持英文
2. **输出**：运行时日志、错误、API 返回的 msg 一律英文；**例外**：图片分析的 `keywords` / `description` / `risk_reason` 为内容字段，按 prompt 约定输出简体中文（`category` / `risk_level` 保持英文枚举值不变）
3. **统一响应**：`{"code":200,"msg":"","data":...}`
   - `code`：200 成功（msg 为空）；失败一律 `-1000`（msg 为英文原因）；`data` 类型随场景
   - 成功用 `helper.OK(c, data)`；service 层抛 `model.NewBizError(msg)`，handler 用 `helper.FailWithError(c, err)` 收口；中间件失败响应也按此契约
   - 图片分析 `data`：`keywords`(3-5) + `description`(≤150字) + `classification{category, score, risk_level, risk_reason}` + `model_id`(实际模型) + `elapsed_ms`(毫秒耗时)；`category` 枚举固定白名单 `normal|porn|suggestive|gore|violence|politics|gambling|drugs|terror|other_risk`
   - 风险审核 `data`：`category` + `score`(0~100 整数) + `model_id`(实际决策渠道 id) + `elapsed_ms`(毫秒耗时，handler 填充)；图片与文本**分类白名单互不混用**：图片 `normal|porn|suggestive|gore|violence|politics|gambling|drugs|terror|fraud|other_risk`，文本 `normal|spam|ads|porn|politics|abuse|fraud|illegal|other_risk`；两者都可能返回 `blocked`(供应商拦截 / 模型拒答) 与 `prohibited`(极危拦截)；自然判定与普通拦截封顶 99，`score=100` 且 `category=prohibited` 专指极危拦截（`prohibited_content`/`blocklist`，通常为 CSAM，fail-closed，需落 Error 日志并按极危流程处置）
4. **性能与安全**：始终传递 `context`（超时/取消）；图片 URL 必须校验（≤10MB、图片 MIME、SSRF 默认拦截内网）；图片分析失败按主备切换（default→backup→报错），风险审核按渠道轮询并在失败时换渠道/重试一次；避免重复请求与不必要开销
5. **结构**：新增路由只进 `internal/router`；中间件进 `internal/middleware`；助手函数进 `internal/helper`；对外接口统一挂 `/api/v1` 分组
6. **限制：** 未经允许不得读取data/config.toml 内容

## Notes
- 主备双渠道：`[[ai.channels]]` 的 `id` 仅允许 `default`（必填）/ `backup`（可选）；`models` 为模型标识字符串数组；`type` ∈ `openai-chat | openai-response | anthropic`
- 风险审核：配置在 `[ai.decision]`（enabled/timeout/max_text_chars + `[[ai.decision.channels]]` 多渠道，type ∈ `gemini-decision | openai-decision`）；请求 `model` 指定渠道 id，不指定按配置顺序轮询；单渠道失败重试 1 次、多渠道失败换下一个渠道再试 1 次；模型拒答映射 blocked/99；缺段不阻断启动，仅决策接口返回 `decision model is not configured`
- 风险审核接口：`POST /api/v1/image/decision`（图片）、`POST /api/v1/text/decision`（文本，受 `max_text_chars` 限制）；文本内容不落日志
- 图片预处理统一走 `internal/service/image_prepare.go` 的 `ImagePreparer`（两个接口共用同一并发信号量）
- 根路径 `/` 为运行状态页（不暴露 /admin 入口），`/favicon.ico` 返回 204；`/healthz` 返回 `status / version / started_at`（进程启动时间，管理台重启后据此判断新实例已起）
- 管理台重启：`POST /admin/api/restart`（需登录会话，原子去重，重复请求 409）；响应写回后 500ms 触发 main 的优雅退出，进程退出码 0，依赖 docker/k8s 的重启策略拉起
- Web 管理台：入口 `/admin`（与 API 同端口，独立会话鉴权，不使用 API Key）。`[admin] username / password_hash`（bcrypt）未设置时进入初始化页；账号密码改动立即生效，其余配置保存后需重启。配置按分区保存（`PUT /admin/api/config/{base|channel|decision|key}`，只覆盖本分区字段），落盘为覆盖式整体重写（go-toml ↔ viper 同一后端，不保留注释），写前跑同一套 `Validate()`，文件权限 0600。`base` 分区只覆盖 timeout/image_timeout/max_retries/max_image_bytes；`temperature`/`top_p`/`mock`/`allow_private_urls` 仅支持改配置文件，`disabled_thinking_keywords` 归 `channel` 分区（图像分析页）维护
- 本机 `:8080` / `:18080` 可能被环境占用，本地验证建议换端口
- 路线图：真实模型接入 → 图片下载/校验助手 → 渠道失败切换 → 限流落地 → 分类阈值可配置