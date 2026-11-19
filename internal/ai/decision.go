package ai

// 风险审核（决策模型）能力。
//
// 设计说明：
//   - 决策模型只输出「概率 / 选项 / 有序评分」，不生成文本，所以没有
//     keywords / description 这类需要模型写字的字段；
//   - 支持多个决策渠道（gemini-decision / openai-decision / systemone-decision 三种协议）：
//     请求可用 model 指定渠道 id，不指定则按配置顺序轮询；
//   - 单次失败后的兜底：还有其它渠道就换下一个再试一次，只有一个渠道就重试一次；
//   - 图片与文本使用**各自独立**的分类白名单，互不混用；
//   - 供应商安全拦截 / 模型拒答（refusal）统一映射为 blocked(99)，
//     极危拦截（prohibited_content / blocklist）映射为 prohibited(100)。
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// 决策输入类型。
const (
	DecisionModeImage = "image" // 图片审核
	DecisionModeText  = "text"  // 文本审核
)

const (
	// decisionSeverityKey 风险严重度评分问题的 key。
	decisionSeverityKey = "risk_severity"
	// decisionMaxNaturalScore 自然判定的风险分上限：100 只保留给极危拦截。
	decisionMaxNaturalScore = 99
	// decisionBlockedScore 普通安全拦截（含模型拒答）的固定风险分。
	decisionBlockedScore = 99
	// decisionProhibitedScore 极危拦截（prohibited_content / blocklist，通常为 CSAM）的固定风险分。
	decisionProhibitedScore = 100
	// decisionInputHint 附在图片前的简短说明（保持中性，避免引导模型）。
	decisionInputHint = "这是一张待审核的图片。请只依据图片可见内容作答。"
)

// decisionBlockedCodes 供应商安全策略拦截的错误码白名单。
// 只有命中这些 code 的 400 才会被判定为「违规输入」，其余 400 一律当参数错误。
var decisionBlockedCodes = map[string]bool{
	"safety":                   true,
	"prohibited_content":       true,
	"blocklist":                true,
	"image_safety":             true,
	"content_blocked":          true,
	"content_policy_violation": true,
	"jailbreak":                true,
}

// decisionProhibitedCodes 属于极危拦截（通常为 CSAM）的错误码子集：
// 对外的 category 会从 blocked 升级为 prohibited，风险分固定 100。
// 取 fail-closed：blocklist 可能是已知违规哈希，宁可按最严重处理。
var decisionProhibitedCodes = map[string]bool{
	"prohibited_content": true,
	"blocklist":          true,
}

// decisionImageCategoryOrder 图片分类的判定顺序（顺序即概率并列时的优先级）：
// 敏感类目优先于 normal，保证并列时不会把风险内容判成正常内容。
var decisionImageCategoryOrder = []string{
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

// decisionImageInstructions 图片各场景的判定问题（key 与对外分类枚举一致）。
var decisionImageInstructions = map[string]string{
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

// decisionTextCategoryOrder 文本分类的判定顺序（敏感类目优先于 normal）。
var decisionTextCategoryOrder = []string{
	"porn",
	"politics",
	"spam",
	"ads",
	"abuse",
	"fraud",
	"illegal",
	"other_risk",
	"normal",
}

// decisionTextInstructions 文本各场景的判定问题（key 与对外分类枚举一致）。
var decisionTextInstructions = map[string]string{
	"porn":       "文本是否包含色情或低俗内容（性描写、性暗示、色情交易、露骨词汇）？",
	"politics":   "文本是否包含政治敏感内容（敏感政治人物、事件、口号，或攻击性政治言论）？",
	"spam":       "文本是否为垃圾信息（无意义刷屏、大量重复、乱码灌水、与上下文无关的凑数内容）？",
	"ads":        "文本是否为广告或营销引流（产品推广、优惠活动、留联系方式、加群加微信）？",
	"abuse":      "文本是否包含辱骂或人身攻击（侮辱、歧视、诅咒、恶意挑衅）？",
	"fraud":      "文本是否为诈骗或欺诈（虚假中奖、刷单返利、冒充客服、诱导转账或点击钓鱼链接）？",
	"illegal":    "文本是否涉及违法违禁内容（毒品、赌博、枪支、管制物品交易，或教唆违法犯罪）？",
	"other_risk": "文本是否包含上述类别未覆盖的违法或高风险内容（自残自杀、伤害未成年人、隐私泄露等）？",
	"normal":     "文本是否完全正常，不包含任何敏感或违规内容？",
}

// decisionSeverityCriteria 风险严重度 rubric：索引 0~4 由低到高（API 要求 2~255 级）。
var decisionSeverityCriteria = []string{
	"无风险：内容正常，无任何敏感或违规元素。",
	"低风险：轻微擦边或轻微敏感（如轻度性感、暴力暗示、模糊广告），通常可直接放行。",
	"中风险：存在明确的敏感或违规倾向，建议人工复核。",
	"高风险：明确的违规内容（色情、血腥、暴力、赌博、诈骗等），建议拦截。",
	"极高风险：露骨色情、极端暴力或恐怖、完整诈骗链路等严重违规，必须拦截。",
}

// DecisionInput 一次决策的输入。
type DecisionInput struct {
	Mode  string               // DecisionModeImage | DecisionModeText
	Text  string               // Mode=text 时必填
	Image *model.PreparedImage // Mode=image 时必填
	Model string               // 指定决策渠道 id，为空则轮询
}

// DecisionAnalyzer 场景识别与风险评估能力接口。
type DecisionAnalyzer interface {
	Evaluate(ctx context.Context, in *DecisionInput) (*model.DecisionResult, error)
}

// decisionAnswers 各协议归一化后的回答。
type decisionAnswers struct {
	Probabilities map[string]float64 // 分类 → 概率（0~1）
	Severity      float64            // 风险等级档位（0 ~ len(criteria)-1）
	Confidences   map[string]float64 // 分类 → 置信度（部分模型不返回）
}

// decisionAPI 单个决策渠道的协议实现。
type decisionAPI interface {
	evaluate(ctx context.Context, in *DecisionInput) (*decisionAnswers, error)
}

// decisionChannel 运行期渠道：配置 id + 协议实现 + 配置内序号。
type decisionChannel struct {
	id    string
	api   decisionAPI
	index int
}

// DecisionGateway 多决策渠道网关：指定渠道或轮询，失败按策略兜底。
type DecisionGateway struct {
	channels []decisionChannel
	rr       atomic.Uint64
}

// NewDecisionAnalyzer 按配置构建决策分析器：
//   - ai.mock = true → 固定模拟结果（本地联调）；
//   - ai.decision.enabled = false 或没有启用的渠道 → 未配置实现，接口直接报错；
//   - 其余 → 多渠道路由 + 对应协议客户端。
func NewDecisionAnalyzer(cfg config.AIConfig) DecisionAnalyzer {
	if cfg.Mock {
		return &mockDecisionAnalyzer{}
	}
	if !cfg.Decision.Enabled {
		return &disabledDecisionAnalyzer{}
	}

	timeout := time.Duration(cfg.Decision.Timeout) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(config.DefaultDecisionTimeout) * time.Second
	}

	g := &DecisionGateway{}
	for _, ch := range cfg.Decision.Channels {
		if !ch.Enabled {
			continue
		}
		var api decisionAPI
		switch ch.Type {
		case config.DecisionChannelTypeOpenAI:
			api = newOpenAIDecisionClient(ch, timeout)
		case config.DecisionChannelTypeSystemOne:
			api = newSystemOneDecisionClient(ch, timeout)
		default:
			api = newGeminiDecisionClient(ch, timeout)
		}
		g.channels = append(g.channels, decisionChannel{id: ch.ID, api: api, index: len(g.channels)})
	}
	if len(g.channels) == 0 {
		return &disabledDecisionAnalyzer{}
	}
	return g
}

// Evaluate 执行一次决策：选渠道 → 调用 → 失败按策略兜底。
func (g *DecisionGateway) Evaluate(ctx context.Context, in *DecisionInput) (*model.DecisionResult, error) {
	if err := validateDecisionInput(in); err != nil {
		return nil, err
	}

	first, err := g.pick(in.Model)
	if err != nil {
		return nil, err
	}

	result, firstErr := g.run(ctx, first, in)
	if firstErr == nil {
		return result, nil
	}
	slog.Warn("decision channel failed", "channel", first.id, "mode", in.Mode, "err", firstErr)

	// 兜底：多渠道路径换下一个渠道；单渠道则用同一渠道再试一次
	second := first
	if len(g.channels) > 1 {
		second = &g.channels[(first.index+1)%len(g.channels)]
	}
	result, secondErr := g.run(ctx, second, in)
	if secondErr == nil {
		return result, nil
	}
	slog.Error("decision channel failed on retry",
		"channel", second.id, "mode", in.Mode,
		"first_channel", first.id, "first_err", firstErr, "err", secondErr)

	return nil, model.NewBizError("decision analysis failed")
}

// pick 选择本次调用的渠道：指定了 id 就精确匹配，否则按配置顺序轮询。
func (g *DecisionGateway) pick(name string) (*decisionChannel, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		for i := range g.channels {
			if g.channels[i].id == name {
				return &g.channels[i], nil
			}
		}
		ids := make([]string, 0, len(g.channels))
		for _, ch := range g.channels {
			ids = append(ids, ch.id)
		}
		return nil, model.NewBizError(fmt.Sprintf(
			"decision model %q not found, available: %s", name, strings.Join(ids, ", ")))
	}

	idx := (g.rr.Add(1) - 1) % uint64(len(g.channels))
	return &g.channels[idx], nil
}

// run 在单个渠道上执行一次调用，并把协议回答转换成对外结果。
func (g *DecisionGateway) run(ctx context.Context, ch *decisionChannel, in *DecisionInput) (*model.DecisionResult, error) {
	answers, err := ch.api.evaluate(ctx, in)
	if err != nil {
		var blocked *DecisionBlockedError
		if errors.As(err, &blocked) {
			return blockedDecisionResult(blocked, ch.id), nil
		}
		return nil, err
	}

	result, err := toDecisionResult(in.Mode, answers, ch.id)
	if err != nil {
		return nil, err
	}
	slog.Debug("decision evaluated",
		"channel", ch.id, "mode", in.Mode, "category", result.Category, "score", result.Score)
	return result, nil
}

// validateDecisionInput 校验输入与模式是否匹配。
func validateDecisionInput(in *DecisionInput) error {
	if in == nil {
		return model.NewBizError("decision input is required")
	}
	switch in.Mode {
	case DecisionModeImage:
		if in.Image == nil || in.Image.DataURI == "" {
			return model.NewBizError("image data is required")
		}
	case DecisionModeText:
		if strings.TrimSpace(in.Text) == "" {
			return model.NewBizError("text must not be empty")
		}
	default:
		return model.NewBizError("unsupported decision mode: " + in.Mode)
	}
	return nil
}

// decisionCategoryOrder 按模式返回分类判定顺序。
func decisionCategoryOrder(mode string) []string {
	switch mode {
	case DecisionModeImage:
		return decisionImageCategoryOrder
	case DecisionModeText:
		return decisionTextCategoryOrder
	default:
		return nil
	}
}

// decisionCategoryInstructions 按模式返回各分类的判定问题。
func decisionCategoryInstructions(mode string) map[string]string {
	switch mode {
	case DecisionModeImage:
		return decisionImageInstructions
	case DecisionModeText:
		return decisionTextInstructions
	default:
		return nil
	}
}

// toDecisionResult 把归一化回答映射为对外结果：
//  1. 分类 = 全部概率的最大值（并列时按分类顺序优先级）；
//  2. 风险分 = 档位 / 最高档 × 100，钳制在 0~99（100 只留给极危拦截）。
func toDecisionResult(mode string, answers *decisionAnswers, channelID string) (*model.DecisionResult, error) {
	if answers == nil || len(answers.Probabilities) == 0 {
		return nil, model.NewBizError("decision response is empty")
	}

	order := decisionCategoryOrder(mode)
	if len(order) == 0 {
		return nil, model.NewBizError("unsupported decision mode: " + mode)
	}

	bestCategory := ""
	bestProb := -1.0
	for _, category := range order {
		probability, ok := answers.Probabilities[category]
		if !ok {
			return nil, model.NewBizError("decision response missing category: " + category)
		}
		if probability > bestProb {
			bestCategory, bestProb = category, probability
		}
	}

	maxLevel := len(decisionSeverityCriteria) - 1
	score := 0
	if maxLevel > 0 {
		score = int(math.Round(answers.Severity / float64(maxLevel) * 100))
	}
	if score < 0 {
		score = 0
	}
	if score > decisionMaxNaturalScore {
		score = decisionMaxNaturalScore
	}

	confidence := answers.Confidences[bestCategory]
	if confidence <= 0 {
		confidence = bestProb
	}

	return &model.DecisionResult{
		Category:      bestCategory,
		Score:         score,
		ModelID:       channelID,
		Confidence:    confidence,
		Probabilities: answers.Probabilities,
	}, nil
}

// blockedDecisionResult 把供应商安全拦截 / 模型拒答映射为对外结果：
//   - prohibited_content / blocklist（通常为 CSAM）→ prohibited / 100，走极危处置流程；
//   - 其余（safety / image_safety / refusal 等）→ blocked / 99。
//
// 说明：这类结果由服务端合成（fail-closed），不是模型分类，具体依据看 BlockedCode。
func blockedDecisionResult(err *DecisionBlockedError, channelID string) *model.DecisionResult {
	result := &model.DecisionResult{
		Category:       model.CategoryBlocked,
		Score:          decisionBlockedScore,
		ModelID:        channelID,
		BlockedCode:    err.Code,
		BlockedMessage: err.Message,
	}
	if decisionProhibitedCodes[err.Code] {
		result.Category = model.CategoryProhibited
		result.Score = decisionProhibitedScore
	}
	return result
}

// mockDecisionAnalyzer 模拟实现：不调用任何模型，返回固定的正常结果。
type mockDecisionAnalyzer struct{}

// Evaluate 返回固定的模拟结果（normal / 低风险分）。
func (m *mockDecisionAnalyzer) Evaluate(ctx context.Context, in *DecisionInput) (*model.DecisionResult, error) {
	return &model.DecisionResult{Category: "normal", Score: 3, ModelID: "mock"}, nil
}

// disabledDecisionAnalyzer 未配置实现：ai.decision.enabled = false 时使用。
type disabledDecisionAnalyzer struct{}

// Evaluate 始终返回未配置错误（具体原因由配置决定，便于运维定位）。
func (d *disabledDecisionAnalyzer) Evaluate(ctx context.Context, in *DecisionInput) (*model.DecisionResult, error) {
	return nil, model.NewBizError("decision model is not configured")
}
