package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
)

// imageAnalysisOutput 模型结构化输出（JSON Schema 由 goai 依据该 Go 类型自动生成）。
// 字段与模型返回的 JSON 完全对应，映射到 model.AnalyzeResult 后对外返回。
type imageAnalysisOutput struct {
	Keywords       []string `json:"keywords"`    // SEO 关键词（3-5 个）
	Description    string   `json:"description"` // SEO 描述（≤150 字）
	Classification struct {
		Category   string  `json:"category"`    // 图片类型（枚举白名单）
		Score      float64 `json:"score"`       // 该图片类型的匹配分数 0.0~1.0
		RiskLevel  string  `json:"risk_level"`  // 综合风险等级 low/medium/high
		RiskReason string  `json:"risk_reason"` // 判定依据概述（人工复审用）
	} `json:"classification"`
}

// 分类枚举白名单：模型输出必须落在此集合内，越界归为 other_risk（保持业务稳定）。
var allowedCategories = map[string]bool{
	"normal":     true, // 正常
	"porn":       true, // 色情
	"suggestive": true, // 性感/擦边
	"gore":       true, // 血腥
	"violence":   true, // 暴力
	"politics":   true, // 政治敏感
	"gambling":   true, // 赌博
	"drugs":      true, // 毒品/违禁
	"terror":     true, // 恐怖极端
	"other_risk": true, // 其他风险
}

// 图片分析系统提示词。
// 注意：模型对图片的判定政策由本 prompt 定义，调整分类枚举时需同步更新。
// 语言约定：keywords / description / risk_reason 为内容字段，要求简体中文输出；
// category / risk_level 为固定枚举值，保持英文原样（白名单判定依赖）。
const imageAnalysisSystemPrompt = `You are a professional content moderation and SEO assistant. Your analysis is part of a legitimate content-safety pipeline: the output is used solely to detect and classify sensitive images (adult content, gore, violence, politics, etc.) so a platform can moderate them.

IMPORTANT - analysis policy: You MUST analyze every image objectively, including images containing nudity, explicit sexual content, gore, or other sensitive material. Do NOT refuse to analyze them, do NOT apologize, and do NOT censor or soften your classification for such images. For adult-content images, simply set classification.category to "porn" or "suggestive" with an appropriate score and risk_level, then return the JSON schema normally. Treating sensitive content as a reason to refuse the task is a failure.

Respond with JSON ONLY, matching exactly this schema:
{
  "keywords": ["自然风景", "蓝天", "白云"],
  "description": "蓝天白云下的宁静自然风景。",
  "classification": {
    "category": "normal",
    "score": 0.0,
    "risk_level": "low",
    "risk_reason": "未检测到敏感内容。"
  }
}
Rules:
1. keywords: 3-5 concise, search-friendly keywords.
2. description: objective description, at most 150 Chinese characters.
3. classification.category MUST come from the fixed enum: normal, porn, suggestive, gore, violence, politics, gambling, drugs, terror, other_risk. Choose the single most relevant type.
4. classification.score: matching score between 0.0 and 1.0 for the chosen category; use confidence, not extremes.
5. classification.risk_level: low, medium, or high (overall moderation risk).
6. classification.risk_reason: short justification useful for human review.
7. Language: ALL natural-language fields (keywords, description, risk_reason) MUST be written in Simplified Chinese. Keep "category" and "risk_level" as the fixed English enum values listed above, never translate them.
8. Completeness (CRITICAL): ALWAYS emit the COMPLETE JSON object. Never truncate, never omit or skip fields, never add markdown, comments or trailing text. All fields are required: keywords with 3-5 items, description, and all four classification fields. If a value is empty, still output it (empty array or empty string).
9. Termination: never stop or end your response before the JSON is fully closed with the final closing brace. Unfinished or unparseable JSON is a hard failure.
Be objective and conservative: flag only high-confidence content. Output the JSON object only, no markdown, no extra text.`

// goaiAnalyzer 基于 goai 的真实分析器：调用大模型完成图片识别与结构化输出。
type goaiAnalyzer struct {
	channel     *config.ChannelConfig // 所属渠道
	modelName   string                // 使用的模型标识
	timeout     time.Duration         // 单次调用超时
	maxRetries  int                   // 单渠道内失败重试次数
	temperature float64               // 采样温度（0 表示不设置，走模型默认）
	topP        float64               // 核采样概率（0 表示不设置）
}

// AnalyzeImage 调用大模型分析图片：
//  1. 图片以 base64 data URI 形式作为 PartImage 传入（openai / anthropic 均兼容）；
//  2. goai.GenerateObject[T] 输出结构化 JSON，映射为 *model.AnalyzeResult。
func (a *goaiAnalyzer) AnalyzeImage(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error) {
	// 前置校验：图片数据必须已由上层转换为 data URI
	if req.ImageDataURI == "" {
		return nil, model.NewBizError("image data is required")
	}

	// 构建模型实例
	lm, err := buildModel(a.channel, a.modelName)
	if err != nil {
		return nil, model.NewBizError("AI channel unavailable: " + err.Error())
	}

	// 组装用户消息：文本指令 + 图片 part
	userMsg := provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Part{
			{Type: provider.PartText, Text: "Analyze this image."},
			{Type: provider.PartImage, URL: req.ImageDataURI, MediaType: req.ImageMIME},
		},
	}

	// 结构化输出调用：自动从 Go 类型生成 JSON Schema
	opts := []goai.Option{
		goai.WithSystem(imageAnalysisSystemPrompt),
		goai.WithMessages(userMsg),
		goai.WithMaxOutputTokens(4096),    // 结构化输出完整 JSON（中文描述较长），放宽到 4096 防截断
		goai.WithMaxRetries(a.maxRetries), // 渠道内 429/5xx 自动重试
	}
	if a.timeout > 0 {
		opts = append(opts, goai.WithTimeout(a.timeout))
	}
	// 降低随机性，使分类/描述输出更稳定可复现：
	// 结构化审核任务追求稳定而不是创意，温度与 top_p 均取较低/收敛值
	if a.temperature > 0 {
		opts = append(opts, goai.WithTemperature(a.temperature))
	}
	if a.topP > 0 {
		opts = append(opts, goai.WithTopP(a.topP))
	}

	// ProviderOptions 必须合并为一次调用：goai 的 WithProviderOptions 为整体赋值，
	// 多次调用会相互覆盖（后一次覆盖前一次），不能分开传。
	providerOptions := map[string]any{}

	// 按渠道协议显式指定 OpenAI 端点：
	//   - openai-chat     → Chat Completions（/chat/completions），兼容网关（vLLM/DashScope 等）通常只实现该端点
	//   - openai-response → Responses API（/responses）
	//   - anthropic       → 无此开关，走 native Messages
	// goai openai provider 默认全部走 Responses API，此处必须按配置的 type 做动态判断，
	// 否则 openai-chat 渠道会在不支持 /responses 的兼容网关上返回 404。
	switch a.channel.Type {
	case config.ChannelTypeOpenAIChat:
		providerOptions["useResponsesAPI"] = false
	case config.ChannelTypeOpenAIResponse:
		providerOptions["useResponsesAPI"] = true
	}

	// 模型 id 命中「强制思考需显式关闭」列表时，向请求传递关闭思考参数：
	//   - openai-chat：enable_thinking（DashScope/one-api 等）与 chat_template_kwargs（vLLM 系）
	//     两个候选键，兼容主流 Qwen 网关；goai 会把未知 ProviderOptions 键原样写入请求体
	//   - anthropic：thinking: {type: disabled}
	//   - openai-response：goai 该路径不透传 ProviderOptions，故不传（Qwen 网关均走 chat 路径）
	// 未命中列表的模型不传任何思考参数，由模型/网关自行决定。
	if shouldDisableThinking(a.modelName) {
		switch a.channel.Type {
		case config.ChannelTypeOpenAIChat:
			providerOptions["enable_thinking"] = false
			providerOptions["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		case config.ChannelTypeAnthropic:
			providerOptions["thinking"] = map[string]any{"type": "disabled"}
		}
	}

	if len(providerOptions) > 0 {
		opts = append(opts, goai.WithProviderOptions(providerOptions))
	}

	result, err := goai.GenerateObject[imageAnalysisOutput](ctx, lm, opts...)
	if err != nil {
		// 结构化输出解析失败（模型输出截断/不完整 JSON）多为偶发，
		// 且 goai 的 HTTP 层重试不覆盖此类错误，这里自动重试一次；
		// 本次重试不计入 maxRetries（那是针对网络/5xx 的）
		if isStructuredOutputParseError(err) {
			slog.Warn("structured output parse failed, retrying once", "err", err)
			result, err = goai.GenerateObject[imageAnalysisOutput](ctx, lm, opts...)
		}
		if err != nil {
			// 失败原因透传给网关层判断是否切换渠道
			return nil, fmt.Errorf("AI analysis failed: %w", err)
		}
	}

	out := toAnalyzeResult(&result.Object)
	// 模型 ID：优先取真实响应中的模型标识（最准确），为空则用配置的模型名兜底
	if out.ModelID = result.Response.Model; out.ModelID == "" {
		out.ModelID = a.modelName
	}
	return out, nil
}

// isStructuredOutputParseError 判断错误是否为结构化输出（JSON）解析失败。
// goai 在模型输出无法解析为 JSON 时会返回 "parsing structured output" 前缀的错误，
// 这类错误代表模型输出质量问题（截断/非 JSON），适合整体重试一次而非切渠道。
func isStructuredOutputParseError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "parsing structured output")
}

// disabledThinkingKeywords 硬编码的「强制思考、需显式关闭」的模型关键词列表。
// 模型 id 包含任一关键词（不区分大小写、包含匹配）时，请求会带上关闭思考参数；
// 不在列表中的模型不传思考参数，由模型/网关自行决定（避免传入不支持的参数产生副作用）。
var disabledThinkingKeywords = []string{
	"qwen", // Qwen3 系列（含 -Thinking 后缀变体）
	"mimo",
}

// shouldDisableThinking 判断模型 id 是否命中「需显式关闭思考」列表。
func shouldDisableThinking(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, kw := range disabledThinkingKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// toAnalyzeResult 将模型结构化输出映射为对外领域模型，并对分类做白名单归一化。
func toAnalyzeResult(out *imageAnalysisOutput) *model.AnalyzeResult {
	return &model.AnalyzeResult{
		Keywords:    out.Keywords,
		Description: out.Description,
		Classification: model.Classification{
			Category:   sanitizeCategory(out.Classification.Category),
			Score:      out.Classification.Score,
			RiskLevel:  sanitizeRiskLevel(out.Classification.RiskLevel),
			RiskReason: out.Classification.RiskReason,
		},
		Raw: out,
	}
}

// sanitizeCategory 分类白名单归一化：白名单外统一归为 other_risk。
func sanitizeCategory(t string) string {
	if allowedCategories[t] {
		return t
	}
	return "other_risk"
}

// sanitizeRiskLevel 风险等级归一化：仅允许 low / medium / high，否则默认 low。
func sanitizeRiskLevel(lv string) string {
	switch lv {
	case "low", "medium", "high":
		return lv
	}
	return "low"
}
