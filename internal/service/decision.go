package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// DecisionService 风险审核业务服务（场景分类 + 风险评分，支持图片与文本）。
type DecisionService struct {
	preparer          *ImagePreparer      // 共享的图片预处理器（含并发信号量）
	analyzer          ai.DecisionAnalyzer // 决策分析器（多渠道路由 / mock / 未配置）
	maxTextChars      int                 // 文本字符上限
	fitDecisionImages bool                // 存在 System One 渠道时，图片统一规整为 JPEG ≤1568
}

// NewDecisionService 构建风险审核业务服务。
// maxTextChars <= 0 时回退 4000。
//
// fitDecisionImages 为 true 时（启用的决策渠道中存在 systemone-decision），
// 图片请求统一按“决策渠道规整”预处理：JPEG、每边 ≤1568、动画 GIF 抽帧拼图，
// 此时请求的 convert_webp 参数不生效——这样轮询/兜底无论落到哪个渠道都能被接受。
func NewDecisionService(preparer *ImagePreparer, analyzer ai.DecisionAnalyzer, maxTextChars int, fitDecisionImages bool) *DecisionService {
	if maxTextChars <= 0 {
		maxTextChars = 4000
	}
	return &DecisionService{
		preparer:          preparer,
		analyzer:          analyzer,
		maxTextChars:      maxTextChars,
		fitDecisionImages: fitDecisionImages,
	}
}

// NeedsDecisionImageFit 判断是否需要启用决策图片规整：
// 只要启用的决策渠道里存在 System One（systemone-decision），就返回 true。
//
// 图片预处理发生在选渠道之前，无法预知轮询/兜底最终落到哪个渠道，
// 因此统一按最严格的要求规整（JPEG ≤1568），三种协议都能接受。
func NeedsDecisionImageFit(channels []config.DecisionChannelConfig) bool {
	for _, ch := range channels {
		if ch.Enabled && ch.Type == config.DecisionChannelTypeSystemOne {
			return true
		}
	}
	return false
}

// Evaluate 编排一次图片风险审核：
//
//	共享图片预处理（并发槽位 → URL 校验 → 下载 → 按目标格式处理 → base64）→ 决策模型
//
// 请求可传 convert_webp=false 关闭转码，图片按原始格式直接送模型
// （用于不支持 WebP 的上游，如 microsoft/decision-1.1）；当启用的决策渠道里
// 存在 System One 渠道时，图片统一规整为 JPEG ≤1568（含动画 GIF 抽帧拼图），
// 该参数不再生效。
//
// 被供应商安全策略拦截时返回 blocked（99）或 prohibited（100），
// 并记录 Warn / Error 日志（含图片 URL 与拦截码）。
// 响应字段 elapsed_ms 由 handler 层填充；此处只在日志里拆分 prepare_ms / model_ms，便于定位瓶颈。
func (s *DecisionService) Evaluate(ctx context.Context, req *model.DecisionRequest) (*model.DecisionResult, error) {
	target := ImageTargetWebP
	if !req.ShouldConvertWebP() {
		target = ImageTargetOriginal
	}
	if s.fitDecisionImages {
		target = ImageTargetDecision
	}

	prepareStart := time.Now()
	img, err := s.preparer.Prepare(ctx, &req.ImageInput, target)
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
