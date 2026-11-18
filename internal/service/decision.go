package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/model"
)

// DecisionService 风险审核业务服务（场景分类 + 风险评分，支持图片与文本）。
type DecisionService struct {
	preparer     *ImagePreparer      // 共享的图片预处理器（含并发信号量）
	analyzer     ai.DecisionAnalyzer // 决策分析器（多渠道路由 / mock / 未配置）
	maxTextChars int                 // 文本字符上限
}

// NewDecisionService 构建风险审核业务服务。
// maxTextChars <= 0 时回退 4000。
func NewDecisionService(preparer *ImagePreparer, analyzer ai.DecisionAnalyzer, maxTextChars int) *DecisionService {
	if maxTextChars <= 0 {
		maxTextChars = 4000
	}
	return &DecisionService{
		preparer:     preparer,
		analyzer:     analyzer,
		maxTextChars: maxTextChars,
	}
}

// Evaluate 编排一次图片风险审核：
//
//	共享图片预处理（并发槽位 → URL 校验 → 下载 → 压缩转 webp → base64）→ 决策模型
//
// 被供应商安全策略拦截时返回 blocked（99）或 prohibited（100），
// 并记录 Warn / Error 日志（含图片 URL 与拦截码）。
// 响应字段 elapsed_ms 由 handler 层填充；此处只在日志里拆分 prepare_ms / model_ms，便于定位瓶颈。
func (s *DecisionService) Evaluate(ctx context.Context, req *model.DecisionRequest) (*model.DecisionResult, error) {
	prepareStart := time.Now()
	img, err := s.preparer.Prepare(ctx, &req.ImageInput)
	prepareMs := time.Since(prepareStart).Milliseconds()
	if err != nil {
		return nil, err
	}

	modelStart := time.Now()
	result, err := s.analyzer.Evaluate(ctx, &ai.DecisionInput{
		Mode:  ai.DecisionModeImage,
		Image: img,
		Model: req.Model,
	})
	modelMs := time.Since(modelStart).Milliseconds()
	if err != nil {
		return nil, err
	}

	// 日志定位信息：URL 或 base64 来源
	source, ref := "base64", ""
	if req.ImageURL != "" {
		source, ref = "url", req.ImageURL
	}
	logDecisionResult(result, "image", source, ref, prepareMs, modelMs)
	return result, nil
}

// EvaluateText 编排一次文本风险审核。
//
// 文本不经过图片预处理，直接交给决策模型；日志里只记录长度，不落原文。
func (s *DecisionService) EvaluateText(ctx context.Context, req *model.TextDecisionRequest) (*model.DecisionResult, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, model.NewBizError("text must not be empty")
	}
	if n := utf8.RuneCountInString(text); n > s.maxTextChars {
		return nil, model.NewBizError(fmt.Sprintf("text is too long (max %d characters)", s.maxTextChars))
	}

	modelStart := time.Now()
	result, err := s.analyzer.Evaluate(ctx, &ai.DecisionInput{
		Mode:  ai.DecisionModeText,
		Text:  text,
		Model: req.Model,
	})
	modelMs := time.Since(modelStart).Milliseconds()
	if err != nil {
		return nil, err
	}

	// 文本内容不落日志，只记录长度，避免敏感信息外泄
	logDecisionResult(result, "text", "length", fmt.Sprintf("%d chars", utf8.RuneCountInString(text)), 0, modelMs)
	return result, nil
}

// logDecisionResult 按结果级别记录日志：极危拦截 Error、普通拦截 Warn、正常 Debug。
func logDecisionResult(result *model.DecisionResult, mode, source, ref string, prepareMs, modelMs int64) {
	switch result.Category {
	case model.CategoryProhibited:
		// 极危拦截（通常为 CSAM）：需要立即处置（停来源 / 不落盘 / 按法规上报）
		slog.Error("decision input flagged as prohibited content, handle per incident runbook",
			"mode", mode, "source", source, "ref", ref, "channel", result.ModelID,
			"provider_code", result.BlockedCode, "provider_message", result.BlockedMessage,
			"category", result.Category, "score", result.Score,
			"prepare_ms", prepareMs, "model_ms", modelMs)
	case model.CategoryBlocked:
		slog.Warn("decision input blocked or declined by provider",
			"mode", mode, "source", source, "ref", ref, "channel", result.ModelID,
			"provider_code", result.BlockedCode, "provider_message", result.BlockedMessage,
			"category", result.Category, "score", result.Score,
			"prepare_ms", prepareMs, "model_ms", modelMs)
	default:
		slog.Debug("decision evaluated",
			"mode", mode, "source", source, "ref", ref, "channel", result.ModelID,
			"category", result.Category, "score", result.Score, "confidence", result.Confidence,
			"prepare_ms", prepareMs, "model_ms", modelMs)
	}
}
