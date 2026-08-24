package ai

import (
	"context"
	"fmt"
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
const imageAnalysisSystemPrompt = `You are an expert image moderation and SEO assistant. Analyze the provided image and respond with JSON ONLY, matching exactly this schema:
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
Be objective and conservative: flag only high-confidence content. Output the JSON object only, no markdown, no extra text.`

// goaiAnalyzer 基于 goai 的真实分析器：调用大模型完成图片识别与结构化输出。
type goaiAnalyzer struct {
	channel    *config.ChannelConfig // 所属渠道
	modelName  string                // 使用的模型标识
	timeout    time.Duration         // 单次调用超时
	maxRetries int                   // 单渠道内失败重试次数
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
		goai.WithMaxOutputTokens(1024),    // 结构化输出按需求落到 JSON 即可，无需过长
		goai.WithMaxRetries(a.maxRetries), // 渠道内 429/5xx 自动重试
	}
	if a.timeout > 0 {
		opts = append(opts, goai.WithTimeout(a.timeout))
	}

	result, err := goai.GenerateObject[imageAnalysisOutput](ctx, lm, opts...)
	if err != nil {
		// 失败原因透传给网关层判断是否切换渠道
		return nil, fmt.Errorf("AI analysis failed: %w", err)
	}

	return toAnalyzeResult(&result.Object), nil
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
