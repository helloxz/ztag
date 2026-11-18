package ai

// OpenAI 兼容 Decisions API 原生 HTTP 客户端。
//
// 协议要点（以 Vercel AI Gateway 为例）：
//
//	POST {base_url}/decisions
//	Header: Authorization: Bearer <key>
//	Body:   {"model": "...", "input": "文本" | [消息块], "questions": [{...}]}
//	返回:   {"answers": [{name, probability|score, ...}], "model": "...", "usage": {...}}
//
// 与 Gemini 协议的关键差异：
//   - questions / answers 都是**数组**，按 name 匹配；
//   - 分类问题用 predicate（Gemini 用 noul）；
//   - score 是「加权平均的小数档位」（Gemini 是整数档位）；
//   - 模型可能返回 refusal（拒答），这里统一转成 blocked 处理。
//
// 重试与多渠道路由由 DecisionGateway 负责，本客户端只做单次调用。
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// openAIInputPart OpenAI 输入内容块：文本或图片。
type openAIInputPart struct {
	Type     string `json:"type"`                // input_text | input_image
	Text     string `json:"text,omitempty"`      // input_text 内容
	ImageURL string `json:"image_url,omitempty"` // input_image：必须是 base64 data URL
	Detail   string `json:"detail,omitempty"`    // input_image：low | high | auto | original
}

// openAIInputMessage 一条 user 消息。
type openAIInputMessage struct {
	Role    string            `json:"role"`
	Content []openAIInputPart `json:"content"`
}

// openAIQuestionLevel score 问题的等级（从低到高）。
type openAIQuestionLevel struct {
	Label string `json:"label"`
}

// openAIQuestion 单个问题：predicate（是否）或 score（有序评分）。
type openAIQuestion struct {
	Type         string                `json:"type"`
	Name         string                `json:"name"`
	Instructions string                `json:"instructions"`
	Levels       []openAIQuestionLevel `json:"levels,omitempty"`
}

// openAIDecisionRequest 请求体（input 为字符串或消息数组）。
type openAIDecisionRequest struct {
	Model     string           `json:"model"`
	Input     interface{}      `json:"input"`
	Questions []openAIQuestion `json:"questions"`
}

// openAIAnswer 单个回答（按 name 匹配问题）。
type openAIAnswer struct {
	Type        string   `json:"type"` // predicate | score | refusal
	Name        string   `json:"name"`
	Probability *float64 `json:"probability,omitempty"`
	Score       *float64 `json:"score,omitempty"`
	Confidence  *float64 `json:"confidence,omitempty"`
}

// openAIDecisionResponse 响应体（只取需要的字段）。
type openAIDecisionResponse struct {
	Model   string         `json:"model"`
	Answers []openAIAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

// openAIAPIError OpenAI 风格错误体。
type openAIAPIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}

// openAIDecisionClient OpenAI 兼容 Decisions 客户端。
// 并发安全：内部只持有不可变的配置与 *http.Client，可被多个请求复用。
type openAIDecisionClient struct {
	endpoint   string
	keys       apiKeyPool // 多个 Key 用 "|" 分隔配置，每次调用随机取一个
	model      string
	detail     string
	httpClient *http.Client
}

// newOpenAIDecisionClient 按渠道配置构建客户端。
func newOpenAIDecisionClient(ch config.DecisionChannelConfig, timeout time.Duration) *openAIDecisionClient {
	base := strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	return &openAIDecisionClient{
		endpoint:   base + "/decisions",
		keys:       newAPIKeyPool(ch.APIKey),
		model:      ch.Model,
		detail:     openAIDetail(ch.Resolution),
		httpClient: &http.Client{Timeout: timeout},
	}
}

// openAIDetail 把统一的 resolution 档位映射为 OpenAI 的 detail。
func openAIDetail(resolution string) string {
	switch resolution {
	case config.DecisionResolutionLow:
		return "low"
	case config.DecisionResolutionMedium:
		return "auto"
	case config.DecisionResolutionUltraHigh:
		return "original"
	default:
		return "high"
	}
}

// evaluate 执行一次决策调用并归一化回答。
func (c *openAIDecisionClient) evaluate(ctx context.Context, in *DecisionInput) (*decisionAnswers, error) {
	input, err := c.buildInput(in)
	if err != nil {
		return nil, err
	}

	resp, err := c.call(ctx, input, buildOpenAIQuestions(in.Mode))
	if err != nil {
		return nil, err
	}
	return answersFromOpenAI(in.Mode, resp)
}

// buildInput 构造 input：文本直接传字符串，图片走消息块（base64 data URL）。
func (c *openAIDecisionClient) buildInput(in *DecisionInput) (interface{}, error) {
	switch in.Mode {
	case DecisionModeImage:
		return []openAIInputMessage{{
			Role: "user",
			Content: []openAIInputPart{
				{Type: "input_text", Text: decisionInputHint},
				{Type: "input_image", ImageURL: in.Image.DataURI, Detail: c.detail},
			},
		}}, nil
	case DecisionModeText:
		return in.Text, nil
	default:
		return nil, model.NewBizError("unsupported decision mode: " + in.Mode)
	}
}

// buildOpenAIQuestions 按输入模式构建问题数组：每分类一个 predicate + 一个严重度 score。
func buildOpenAIQuestions(mode string) []openAIQuestion {
	order := decisionCategoryOrder(mode)
	instructions := decisionCategoryInstructions(mode)

	questions := make([]openAIQuestion, 0, len(order)+1)
	for _, category := range order {
		questions = append(questions, openAIQuestion{
			Type:         "predicate",
			Name:         category,
			Instructions: instructions[category],
		})
	}

	levels := make([]openAIQuestionLevel, 0, len(decisionSeverityCriteria))
	for _, label := range decisionSeverityCriteria {
		levels = append(levels, openAIQuestionLevel{Label: label})
	}
	questions = append(questions, openAIQuestion{
		Type:         "score",
		Name:         decisionSeverityKey,
		Instructions: "综合评估内容的风险严重程度。",
		Levels:       levels,
	})
	return questions
}

// answersFromOpenAI 把数组形式的 answers 映射为归一化回答。
// 出现 refusal 时返回 *DecisionBlockedError（上层合成 blocked / 99）。
func answersFromOpenAI(mode string, resp *openAIDecisionResponse) (*decisionAnswers, error) {
	if resp == nil || len(resp.Answers) == 0 {
		return nil, model.NewBizError("decision response is empty")
	}

	order := decisionCategoryOrder(mode)
	if len(order) == 0 {
		return nil, model.NewBizError("unsupported decision mode: " + mode)
	}

	answers := &decisionAnswers{
		Probabilities: make(map[string]float64, len(order)),
		Confidences:   make(map[string]float64, len(order)),
	}
	severitySet := false

	for _, answer := range resp.Answers {
		if answer.Type == "refusal" {
			return nil, &DecisionBlockedError{
				Code:    "refusal",
				Message: "decision model declined to answer: " + answer.Name,
			}
		}
		if answer.Name == decisionSeverityKey {
			if answer.Score == nil {
				return nil, model.NewBizError("decision response missing risk severity score")
			}
			answers.Severity = *answer.Score
			severitySet = true
			continue
		}
		if answer.Probability == nil {
			return nil, model.NewBizError("decision response missing probability for category: " + answer.Name)
		}
		answers.Probabilities[answer.Name] = *answer.Probability
		if answer.Confidence != nil {
			answers.Confidences[answer.Name] = *answer.Confidence
		}
	}

	for _, category := range order {
		if _, ok := answers.Probabilities[category]; !ok {
			return nil, model.NewBizError("decision response missing category answer: " + category)
		}
	}
	if !severitySet {
		return nil, model.NewBizError("decision response missing risk severity score")
	}
	return answers, nil
}

// call 执行单次 HTTP 请求并解析响应。
func (c *openAIDecisionClient) call(
	ctx context.Context,
	input interface{},
	questions []openAIQuestion,
) (*openAIDecisionResponse, error) {
	body, err := json.Marshal(openAIDecisionRequest{
		Model:     c.model,
		Input:     input,
		Questions: questions,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal decision request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build decision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.keys.pick())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("decision request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, decisionMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read decision response: %w", err)
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var out openAIDecisionResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("parse decision response: %w (raw: %s)", err, truncateForLog(string(raw)))
		}
		return &out, nil
	}

	var apiErr openAIAPIError
	_ = json.Unmarshal(raw, &apiErr)
	code := apiErr.Error.Code
	if code == "" {
		code = apiErr.Error.Type
	}
	message := apiErr.Error.Message
	if message == "" {
		message = truncateForLog(strings.TrimSpace(string(raw)))
	}

	// 安全策略拦截 / 内容策略拒绝：转成 blocked 结果
	if resp.StatusCode == http.StatusBadRequest &&
		(decisionBlockedCodes[code] || apiErr.Error.Type == "content_policy_violation") {
		return nil, &DecisionBlockedError{Code: code, Message: message}
	}
	return nil, fmt.Errorf("decision request failed (HTTP %d): %s", resp.StatusCode, message)
}
