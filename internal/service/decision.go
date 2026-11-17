package service

import (
	"context"
	"log/slog"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/model"
)

// DecisionService 场景决策业务服务（仅场景分类 + 风险评分）。
type DecisionService struct {
	preparer *ImagePreparer      // 共享的图片预处理器（含并发信号量）
	analyzer ai.DecisionAnalyzer // 场景决策分析器（真实 / mock / 未配置）
}

// NewDecisionService 构建场景决策业务服务。
func NewDecisionService(preparer *ImagePreparer, analyzer ai.DecisionAnalyzer) *DecisionService {
	return &DecisionService{
		preparer: preparer,
		analyzer: analyzer,
	}
}

// Evaluate 编排一次场景决策请求：
//
//	共享图片预处理（并发槽位 → URL 校验 → 下载 → 压缩转 webp → base64）→ 决策模型
//
// 被供应商安全策略拦截时返回 blocked / 100，并记录 Warn 日志（含图片 URL 与拦截码）。
func (s *DecisionService) Evaluate(ctx context.Context, req *model.DecisionRequest) (*model.DecisionResult, error) {
	img, err := s.preparer.Prepare(ctx, &req.ImageInput)
	if err != nil {
		return nil, err
	}

	result, err := s.analyzer.EvaluateImage(ctx, img)
	if err != nil {
		return nil, err
	}

	// 日志定位信息：URL 或 base64 来源
	source, ref := "base64", ""
	if req.ImageURL != "" {
		source, ref = "url", req.ImageURL
	}

	switch result.Category {
	case model.CategoryProhibited:
		// 极危拦截（通常为 CSAM）：需要立即处置（停来源 / 不落盘 / 按法规上报）
		slog.Error("decision input flagged as prohibited content, handle per incident runbook",
			"source", source, "url", ref,
			"provider_code", result.BlockedCode, "provider_message", result.BlockedMessage,
			"category", result.Category, "score", result.Score)
	case model.CategoryBlocked:
		slog.Warn("decision input blocked by provider safety filter",
			"source", source, "url", ref,
			"provider_code", result.BlockedCode, "provider_message", result.BlockedMessage,
			"category", result.Category, "score", result.Score)
	default:
		slog.Debug("decision evaluated",
			"source", source, "url", ref,
			"category", result.Category, "score", result.Score,
			"confidence", result.Confidence)
	}

	return result, nil
}
