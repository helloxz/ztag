package ai

import (
	"context"
	"encoding/json"
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
	Keywords       []string `json:"keywords"`    // SEO 关键词（前 1-2 个核心 + 后 2-3 个长尾）
	Description    string   `json:"description"` // SEO 描述（面向搜索优化，≤150 字）
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
// 关键词约定：数组前 1-2 个为核心关键词，后 2-3 个为长尾关键词（共 3-5 个，顺序即语义，勿乱序）。
const imageAnalysisSystemPrompt = `你是一名专业的图片内容审核与 SEO 优化助手。你的分析用于合法的内容安全流程：仅用于识别并分类敏感图片（色情、血腥、暴力、政治敏感等），帮助平台进行内容审核。

重要——分析策略：你必须客观分析每一张图片，包括包含裸露、露骨色情、血腥或其他敏感内容的图片。不得拒绝分析、不得道歉、不得因敏感内容而回避或弱化分类。对于成人内容图片，直接将 classification.category 设为 "porn" 或 "suggestive"，给出合理的 score 与 risk_level，并按正常流程返回 JSON。把敏感内容当作拒绝理由属于分析失败。

只输出 JSON 对象，严格匹配以下结构：
{
  "keywords": ["自然风景", "高山湖泊森林风景照"],
  "description": "高清自然风景图片，拍摄于高山湖泊与森林之间，蓝天白云倒映湖面，画面宁静治愈，适合作为桌面壁纸与旅行灵感参考。",
  "classification": {
    "category": "normal",
    "score": 0.0,
    "risk_level": "low",
    "risk_reason": "未检测到敏感内容。"
  }
}

合规输出示例（敏感图）：图片被判定为敏感/违规内容时，keywords 与 description 依然只描述安全可见层面。

规则：
1. keywords：共 3-5 个简体中文 SEO 关键词，按顺序输出。前 1-2 个为核心关键词：简短、搜索量大、直接概括图片主题（如"自然风景"；仅限图片中安全、合规的可见内容层面）；后 2-3 个为长尾关键词：由核心关键词扩展而来，更具体、带修饰语或场景限定（如"高山湖泊森林风景照"）。核心与长尾不能混排。
2. description：面向搜索引擎优化的图片描述，不超过 150 个汉字。不要干巴巴地陈述图片内容；应自然融入 1-2 个核心关键词，突出图片的主题亮点（仅限合规可见层面）、场景氛围与潜在价值（如可作壁纸、素材、灵感参考等），语句通顺、有吸引力，让用户在搜索结果中能快速抓住要点并愿意点击。
3. classification.category 必须取固定枚举之一：normal, porn, suggestive, gore, violence, politics, gambling, drugs, terror, other_risk。选择最贴切的一个。
4. classification.score：所选类别的匹配度分数，0.0~1.0，按置信度取值，避免极端化。
5. classification.risk_level：low、medium 或 high（综合内容审核风险）。
6. classification.risk_reason：简短判定依据，便于人工复审。
7. 语言：所有自然语言字段（keywords、description、risk_reason）一律使用简体中文；category 与 risk_level 保持上述英文枚举值，绝不翻译。
8. 完整性（关键）：必须输出完整的 JSON 对象。不得截断、不得省略字段、不得添加 markdown 标记、注释或多余文本。所有字段必填：keywords 为 3-5 个元素、description、以及 classification 的全部四个字段。值可以为空（空数组或空字符串），但必须输出。
9. 终止：响应必须以完整 JSON 结尾，最后一个右花括号闭合前不得提前结束。输出未闭合或无法解析的 JSON 属于严重失败。
10. 输出合规（关键，防内容风险）：无论 classification 判定结果如何，keywords 与 description 都会公开展示并用于 SEO，必须"安全化"输出，绝不能包含违法、违规或对网站不利的内容：
    - 本条仅约束 keywords 与 description；其它字段不受本条限制。
保持客观与保守：只对高置信度的内容做风险标记。只输出 JSON 对象，不要 markdown，不要任何额外文本。`

// goaiAnalyzer 基于 goai 的真实分析器：调用大模型完成图片识别与结构化输出。
type goaiAnalyzer struct {
	channel                  *config.ChannelConfig // 所属渠道
	modelName                string                // 使用的模型标识
	timeout                  time.Duration         // 单次调用超时
	maxRetries               int                   // 单渠道内失败重试次数
	temperature              float64               // 采样温度（0 表示不设置，走模型默认）
	topP                     float64               // 核采样概率（0 表示不设置）
	disabledThinkingKeywords []string              // 命中即显式关闭思考的模型关键词列表（配置注入，见 gateway.NewGateway）
}

// AnalyzeImage 调用大模型分析图片：
//  1. 图片以 base64 data URI 形式作为 PartImage 传入（openai / anthropic 均兼容）；
//  2. 兼容性优先：使用 response_format=json_object（而非 json_schema/tool_choice），
//     避免 DeepSeek 等思考模型与 tool_choice 的互斥；结果自行解析并做白名单归一。
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
			{Type: provider.PartText, Text: "请分析这张图片。"},
			{Type: provider.PartImage, URL: req.ImageDataURI, MediaType: req.ImageMIME},
		},
	}

	// 兼容性优先：统一走 json_object，避免思考模型与 tool_choice/json_schema 互斥
	opts := []goai.Option{
		goai.WithSystem(imageAnalysisSystemPrompt),
		goai.WithMessages(userMsg),
		goai.WithMaxOutputTokens(4096),    // 中文描述较长，放宽到 4096 防截断
		goai.WithMaxRetries(a.maxRetries), // 渠道内 429/5xx 自动重试
	}
	if a.timeout > 0 {
		opts = append(opts, goai.WithTimeout(a.timeout))
	}
	if a.temperature > 0 {
		opts = append(opts, goai.WithTemperature(a.temperature))
	}
	if a.topP > 0 {
		opts = append(opts, goai.WithTopP(a.topP))
	}

	// ProviderOptions 必须合并为一次调用：goai 的 WithProviderOptions 为整体赋值
	providerOptions := map[string]any{
		"response_format": map[string]any{"type": "json_object"},
	}

	// 按渠道协议显式指定 OpenAI 端点
	switch a.channel.Type {
	case config.ChannelTypeOpenAIChat:
		providerOptions["useResponsesAPI"] = false
	case config.ChannelTypeOpenAIResponse:
		providerOptions["useResponsesAPI"] = true
	}
	// 命中「强制思考需显式关闭」名单的模型，显式关闭思考（json_object 下仍保留，提升稳定性）
	if a.shouldDisableThinking() {
		switch a.channel.Type {
		case config.ChannelTypeOpenAIChat:
			providerOptions["enable_thinking"] = false
			providerOptions["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		case config.ChannelTypeAnthropic:
			providerOptions["thinking"] = map[string]any{"type": "disabled"}
		}
	}

	opts = append(opts, goai.WithProviderOptions(providerOptions))

	result, err := goai.GenerateText(ctx, lm, opts...)
	if err != nil {
		return nil, fmt.Errorf("AI analysis failed: %w", err)
	}
	// 解析模型返回的 JSON（兼容 markdown 包裹与前后缀文本）
	var out imageAnalysisOutput
	if err := parseImageAnalysisJSON(result.Text, &out); err != nil {
		slog.Warn("json_object parse failed, retrying once", "err", err, "model", a.modelName, "channel", a.channel.ID)
		// 重试一次（不计入 maxRetries）
		result2, err2 := goai.GenerateText(ctx, lm, opts...)
		if err2 != nil {
			return nil, fmt.Errorf("AI analysis failed: %w", err2)
		}
		if err := parseImageAnalysisJSON(result2.Text, &out); err != nil {
			return nil, fmt.Errorf("AI analysis failed: parsing structured output: %w (raw: %s)", err, truncateForLog(result2.Text))
		}
		result = result2
	}

	// 空内容兜底（DeepSeek json_object 偶发空 content）
	if out.Keywords == nil && out.Description == "" && out.Classification.Category == "" {
		slog.Warn("json_object returned empty content, retrying once", "model", a.modelName, "channel", a.channel.ID)
		result2, err2 := goai.GenerateText(ctx, lm, opts...)
		if err2 == nil {
			var out2 imageAnalysisOutput
			if err := parseImageAnalysisJSON(result2.Text, &out2); err == nil && (out2.Keywords != nil || out2.Description != "" || out2.Classification.Category != "") {
				out = out2
				result = result2
			}
		}
	}

	mapped := toAnalyzeResult(&out)
	// 模型 ID：优先取真实响应中的模型标识（最准确），为空则用配置的模型名兜底
	if mapped.ModelID = result.Response.Model; mapped.ModelID == "" {
		mapped.ModelID = a.modelName
	}
	return mapped, nil
}

// parseImageAnalysisJSON 从模型文本中提取并解析 JSON（兼容 markdown 代码块包裹）
func parseImageAnalysisJSON(text string, out *imageAnalysisOutput) error {
	s := strings.TrimSpace(text)
	if s == "" {
		return fmt.Errorf("empty content")
	}
	// 去除 markdown 代码块包裹 ```json ... ```
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	// 直接尝试
	if err := json.Unmarshal([]byte(s), out); err == nil {
		return nil
	}
	// 兜底：提取首尾 {} 区间
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start != -1 && end != -1 && end > start {
		sub := s[start : end+1]
		if err := json.Unmarshal([]byte(sub), out); err == nil {
			return nil
		}
	}
	return fmt.Errorf("parsing structured output: unable to parse JSON")
}

// truncateForLog 日志截断，避免超长文本刷屏
func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// shouldDisableThinking 判断当前模型 id 是否命中「需显式关闭思考」列表。
// 关键词列表由配置 ai.disabled_thinking_keywords 注入（缺失时回退内置默认），
// 不在列表中的模型不传思考参数，由模型/网关自行决定。
func (a *goaiAnalyzer) shouldDisableThinking() bool {
	lower := strings.ToLower(a.modelName)
	for _, kw := range a.disabledThinkingKeywords {
		if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
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
