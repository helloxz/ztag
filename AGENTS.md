# AGENTS.md

图片内容识别 API 服务：调用后端大模型识别图片，提取关键词/描述并分类（色情、政治、血腥、暴力等），对外提供统一 HTTP API。模块 `github.com/helloxz/ztag`，Go + gin + viper(TOML) + [goai](https://github.com/zendev-sh/goai)。

## Commands
- 编译：`./scripts/build.sh`（输出 `bin/ztag`）或 `make build`
- 开发运行：`./scripts/dev.sh`（go run，可透传 `-config`）
- 检查：`make vet`、`gofmt -l .`
- 配置：`data/config.toml`，首次启动自动从内嵌模板生成（docker 挂载 `data/` 持久化）

## Architecture（依赖单向向下，禁止跨层）
```
router → handler → service → ai
```
- `internal/router`：所有路由统一在这里注册（唯一挂载点）
- `internal/middleware`：Recovery / AccessLog / Auth / RateLimit
- `internal/handler`：表现层——参数解析 + 统一响应（薄层，无业务逻辑）
- `internal/service`：业务编排——校验 → AI 网关调用
- `internal/ai`：多渠道网关（channel 内多模型）；接入真实模型用 `goai.GenerateObject[T]`，当前为 mock
- `internal/config` / `internal/model` / `internal/helper`：配置、领域模型、通用助手

## Conventions（必须遵守）
1. **注释**：关键代码写中文注释；注释保持中文，代码标识符/消息保持英文
2. **输出**：运行时日志、错误、API 返回的 msg 一律英文；**例外**：图片分析的 `keywords` / `description` / `risk_reason` 为内容字段，按 prompt 约定输出简体中文（`category` / `risk_level` 保持英文枚举值不变）
3. **统一响应**：`{"code":200,"msg":"","data":...}`
   - `code`：200 成功（msg 为空）；失败一律 `-1000`（msg 为英文原因）；`data` 类型随场景
   - 成功用 `helper.OK(c, data)`；service 层抛 `model.NewBizError(msg)`，handler 用 `helper.FailWithError(c, err)` 收口；中间件失败响应也按此契约
   - 图片分析 `data`：`keywords`(3-5) + `description`(≤150字) + `classification{category, score, risk_level, risk_reason}` + `model_id`(实际模型) + `elapsed_ms`(毫秒耗时)；`category` 枚举固定白名单 `normal|porn|suggestive|gore|violence|politics|gambling|drugs|terror|other_risk`
4. **性能与安全**：始终传递 `context`（超时/取消）；图片 URL 必须校验（≤10MB、图片 MIME、SSRF 默认拦截内网）；AI 调用失败按主备切换（default→backup→报错）；避免重复请求与不必要开销
5. **结构**：新增路由只进 `internal/router`；中间件进 `internal/middleware`；助手函数进 `internal/helper`；对外接口统一挂 `/api/v1` 分组

## Notes
- 主备双渠道：`[[ai.channels]]` 的 `id` 仅允许 `default`（必填）/ `backup`（可选）；`models` 为模型标识字符串数组；`type` ∈ `openai-chat | openai-response | anthropic`
- 本机 `:8080` / `:18080` 可能被环境占用，本地验证建议换端口
- 路线图：真实模型接入 → 图片下载/校验助手 → 渠道失败切换 → 限流落地 → 分类阈值可配置