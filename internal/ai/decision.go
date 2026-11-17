package ai

// 场景决策能力（Gemini Decision 1 / Decisions API）。
//
// 设计说明：
//   - 决策模型只输出「概率 / 选项 / 有序评分」，不生成文本，所以这里没有
//     keywords / description / risk_reason 这类需要模型写字的字段；
//   - 不用 choice 单选，而是给每个场景一个独立 noul 问题：一张图可以同时
//     命中「政治 + 二维码」「诈骗 + 二维码」，独立概率不会被强行归一；
//   - 风险分来自单独的 score 问题（有序 rubric），与分类概率解耦，
//     避免正常图因为「匹配度高」而拿到高分；
//   - 供应商安全拦截（400 + 安全错误码）统一映射为 blocked / 100，
//     属于 fail-closed 推导结论，不是模型分类。
import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

const (
	// decisionSeverityKey 风险严重度评分问题的 key。
	decisionSeverityKey = "risk_severity"
	// decisionMaxNaturalScore 自然判定的风险分上限：100 只保留给极危拦截。
	decisionMaxNaturalScore = 99
	// decisionBlockedScore 普通安全拦截（safety / image_safety / jailbreak）的固定风险分。
	decisionBlockedScore = 99
	// decisionProhibitedScore 极危拦截（prohibited_content / blocklist，通常为 CSAM）的固定风险分。
	decisionProhibitedScore = 100
	// decisionInputHint 附在图片前的简短说明（保持中性，避免引导模型）。
	decisionInputHint = "这是一张待审核的图片。请只依据图片可见内容作答。"
)

// DecisionAnalyzer 场景识别与风险评估能力接口。
// 业务层只依赖该接口，不感知底层是原生 HTTP 还是 mock 实现。
type DecisionAnalyzer interface {
	// EvaluateImage 识别图片场景并给出 0~100 的风险评分。
	EvaluateImage(ctx context.Context, img *model.PreparedImage) (*model.DecisionResult, error)
}

// decisionCategoryOrder 参与分类判定的场景，顺序即概率并列时的优先级：
// 敏感类目优先于 normal，保证并列时不会把风险图判成正常图。
var decisionCategoryOrder = []string{
	"porn",
	"gore",
	"violence",
	"terror",
	"politics",
	"gambling",
	"fraud",
	"drugs",
	"suggestive",
	"other_risk",
	"normal",
}

// decisionCategoryInstructions 各场景的判定问题（key 与对外分类枚举一致）。
// 键名不会展示给模型，因此问题必须自解释。
var decisionCategoryInstructions = map[string]string{
	"porn":       "图片是否包含露骨色情内容（性行为、暴露的性器官、明显的色情场景）？",
	"suggestive": "图片是否包含擦边或性感内容（暴露的穿着、性暗示姿势），但未达到露骨色情？",
	"gore":       "图片是否包含血腥内容（伤口、血迹、尸体、内脏等）？",
	"violence":   "图片是否包含暴力内容（打斗、殴打、武器威胁、虐待等）？",
	"politics":   "图片是否包含政治敏感内容（敏感政治人物、政治符号、敏感政治事件或标语）？",
	"gambling":   "图片是否包含赌博内容（赌场、赌桌、筹码、老虎机、博彩广告或下注界面）？",
	"fraud":      "图片是否包含诈骗或欺诈迹象（虚假中奖、刷单返利、冒充客服、钓鱼链接、虚假投资理财，或引导扫描二维码/添加联系方式进行转账、领奖等）？",
	"drugs":      "图片是否包含毒品或违禁药物（吸毒工具、毒品、种植或交易场景）？",
	"terror":     "图片是否包含恐怖主义或极端主义内容（恐怖组织标志、极端口号、爆炸物等）？",
	"other_risk": "图片是否包含上述类别未覆盖的违法或高风险内容（如违法广告、自残、未成年人不当内容等）？",
	"normal":     "图片是否完全不包含任何敏感或违规内容（上述各类风险均不存在）？",
}

// decisionSeverityCriteria 风险严重度 rubric：索引 0~4 由低到高（API 要求 2~255 级）。
var decisionSeverityCriteria = []string{
	"无风险：内容正常，无任何敏感或违规元素。",
	"低风险：轻微擦边或轻微敏感（如轻度性感、暴力暗示、模糊二维码），通常可直接放行。",
	"中风险：存在明确的敏感或违规倾向，建议人工复核。",
	"高风险：明确的违规内容（色情、血腥、暴力、赌博、诈骗等），建议拦截。",
	"极高风险：露骨色情、极端暴力或恐怖、完整诈骗链路等严重违规，必须拦截。",
}

// decisionQuestions 固定的问题集合（构建一次，多请求复用；只读不修改）。
var decisionQuestions = buildDecisionQuestions()

// buildDecisionQuestions 构建全部问题：每场景一个 noul + 一个严重度 score。
func buildDecisionQuestions() map[string]decisionQuestion {
	questions := make(map[string]decisionQuestion, len(decisionCategoryOrder)+1)
	for _, category := range decisionCategoryOrder {
		questions[category] = decisionQuestion{
			Type:         decisionQuestionNoul,
			Instructions: decisionCategoryInstructions[category],
		}
	}
	questions[decisionSeverityKey] = decisionQuestion{
		Type:         decisionQuestionScore,
		Instructions: "综合评估这张图片的内容风险严重程度。",
		Criteria:     decisionSeverityCriteria,
	}
	return questions
}

// NewDecisionAnalyzer 按配置构建场景决策分析器：
//   - ai.mock = true → 返回固定模拟结果（本地联调）；
//   - ai.decision.enabled = false → 返回未配置实现，接口直接报错；
//   - 其余 → 使用原生 HTTP 客户端调用 Decisions API。
func NewDecisionAnalyzer(cfg config.AIConfig) DecisionAnalyzer {
	if cfg.Mock {
		return &mockDecisionAnalyzer{}
	}
	if !cfg.Decision.Enabled {
		return &disabledDecisionAnalyzer{}
	}
	return &decisionAnalyzer{
		client:     newDecisionClient(cfg.Decision, cfg.MaxRetries),
		resolution: cfg.Decision.Resolution,
		model:      cfg.Decision.Model,
	}
}

// decisionAnalyzer 基于 Decisions API 的真实实现。
type decisionAnalyzer struct {
	client     *decisionClient // HTTP 客户端（含重试）
	resolution string          // 图片分辨率档位
	model      string          // 模型标识（仅日志用）
}

// EvaluateImage 调用决策模型完成场景识别与风险评分。
func (a *decisionAnalyzer) EvaluateImage(ctx context.Context, img *model.PreparedImage) (*model.DecisionResult, error) {
	if img == nil || img.DataURI == "" {
		return nil, model.NewBizError("image data is required")
	}

	// data URI → 裸 base64（API 的 image.data 只接受 base64 本体）
	data := img.DataURI
	if strings.HasPrefix(data, "data:") {
		if idx := strings.Index(data, ","); idx >= 0 {
			data = data[idx+1:]
		}
	}

	input := []decisionContent{
		{Type: "text", Text: decisionInputHint},
		{Type: "image", Data: data, MIMEType: img.MIME, Resolution: a.resolution},
	}

	resp, err := a.client.call(ctx, input, decisionQuestions)
	if err != nil {
		// 安全策略拦截：合成 blocked / prohibited 结果，交由上层记录日志（上层才有图片 URL）
		var blockedErr *DecisionBlockedError
		if errors.As(err, &blockedErr) {
			return blockedDecisionResult(blockedErr), nil
		}
		// 其余错误只对外暴露通用原因，细节进日志，避免泄漏供应商响应
		slog.Error("decision analysis failed", "model", a.model, "err", err)
		return nil, model.NewBizError("decision analysis failed")
	}

	result, err := toDecisionResult(resp)
	if err != nil {
		slog.Warn("decision response unusable", "model", a.model, "err", err)
		return nil, err
	}
	return result, nil
}

// blockedDecisionResult 把供应商安全拦截映射为对外结果：
//   - prohibited_content / blocklist（通常为 CSAM）→ prohibited / 100，走极危处置流程；
//   - 其余安全拦截（safety / image_safety / jailbreak）→ blocked / 99。
//
// 说明：这类结果由服务端合成（fail-closed），不是模型分类，具体依据看 BlockedCode。
func blockedDecisionResult(err *DecisionBlockedError) *model.DecisionResult {
	result := &model.DecisionResult{
		Category:       model.CategoryBlocked,
		Score:          decisionBlockedScore,
		BlockedCode:    err.Code,
		BlockedMessage: err.Message,
	}
	if decisionProhibitedCodes[err.Code] {
		result.Category = model.CategoryProhibited
		result.Score = decisionProhibitedScore
	}
	return result
}

// toDecisionResult 把模型回答映射为对外结果：
//  1. 分类 = 全部 noul 概率的最大值（并列时按 decisionCategoryOrder 优先级）；
//  2. 风险分 = score 加权等级 / 最高等级 × 100，钳制在 0~99。
func toDecisionResult(resp *decisionResponse) (*model.DecisionResult, error) {
	if resp == nil || len(resp.Answers) == 0 {
		return nil, model.NewBizError("decision response is empty")
	}

	bestCategory := ""
	bestProb := -1.0
	probabilities := make(map[string]float64, len(decisionCategoryOrder))
	for _, category := range decisionCategoryOrder {
		answer, ok := resp.Answers[category]
		if !ok || answer.Noul == nil {
			return nil, model.NewBizError("decision response missing category answer: " + category)
		}
		probabilities[category] = *answer.Noul
		if *answer.Noul > bestProb {
			bestCategory, bestProb = category, *answer.Noul
		}
	}

	severity, ok := resp.Answers[decisionSeverityKey]
	if !ok || severity.Score == nil {
		return nil, model.NewBizError("decision response missing risk severity score")
	}
	// rubric 等级数 = len(criteria)-1（索引 0 表示最低风险）
	maxLevel := len(decisionSeverityCriteria) - 1
	score := 0
	if maxLevel > 0 {
		score = int(math.Round(*severity.Score / float64(maxLevel) * 100))
	}
	if score < 0 {
		score = 0
	}
	if score > decisionMaxNaturalScore {
		score = decisionMaxNaturalScore
	}

	confidence := 0.0
	if answer, ok := resp.Answers[bestCategory]; ok {
		confidence = answer.Confidence
	}

	// 置信度偏低时留一条调试线索（不改变判定结果）
	if bestProb < 0.5 {
		slog.Debug("decision category confidence is low", "category", bestCategory, "probability", bestProb)
	}

	return &model.DecisionResult{
		Category:      bestCategory,
		Score:         score,
		Confidence:    confidence,
		Probabilities: probabilities,
	}, nil
}

// mockDecisionAnalyzer 模拟实现：不调用任何模型，返回固定的正常结果。
type mockDecisionAnalyzer struct{}

// EvaluateImage 返回固定的模拟结果（normal / 低风险分）。
func (m *mockDecisionAnalyzer) EvaluateImage(ctx context.Context, img *model.PreparedImage) (*model.DecisionResult, error) {
	return &model.DecisionResult{Category: "normal", Score: 3}, nil
}

// disabledDecisionAnalyzer 未配置实现：ai.decision.enabled = false 时使用。
type disabledDecisionAnalyzer struct{}

// EvaluateImage 始终返回未配置错误（具体原因由配置决定，便于运维定位）。
func (d *disabledDecisionAnalyzer) EvaluateImage(ctx context.Context, img *model.PreparedImage) (*model.DecisionResult, error) {
	return nil, model.NewBizError("decision model is not configured")
}
