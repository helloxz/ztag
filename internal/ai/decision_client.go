package ai

// Gemini Decisions API 原生 HTTP 客户端。
//
// 为什么不用 goai / 官方 Go SDK：
//   - goai provider 只封装 generateContent 等聊天接口，没有 decisions 端点（v0.11.0 仍然没有）；
//   - 官方 google.golang.org/genai 的发布版不保证已支持 Decisions API；
//   - 端点本身只是一次 POST + 固定 JSON，自研可精确控制安全拦截识别与响应解析。
//
// 协议要点（v1beta）：
//
//	POST {base_url}/v1beta/decisions
//	Header: x-goog-api-key
//	Body:   {"model": "...", "input": [...], "questions": {"name": {...}}}
//	返回:   {"answers": {"name": {...}}, "usage": {...}}
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

// decisionMaxResponseBytes 单次响应体读取上限（answers 是结构化的短 JSON，留足余量即可）。
const decisionMaxResponseBytes = 4 << 20

// 问题类型：默认只用 noul（是否）与 score（有序评分），choice 保留常量备用。
const (
	decisionQuestionNoul  = "noul"
	decisionQuestionScore = "score"
)

// decisionContent Gemini 输入内容块（文本 / 图片）。
type decisionContent struct {
	Type       string `json:"type"`                 // text | image
	Text       string `json:"text,omitempty"`       // text 块内容
	Data       string `json:"data,omitempty"`       // image 块：裸 base64（不含 data URI 前缀）
	MIMEType   string `json:"mime_type,omitempty"`  // image 块：真实 MIME，如 image/webp
	Resolution string `json:"resolution,omitempty"` // image 块：low | medium | high | ultra_high
}

// geminiQuestion 单个问题定义。criteria 依 type 不同：
//   - noul：可为空（或 true_description / false_description 对象）
//   - choice：option key → 描述 的对象
//   - score：有序等级描述数组（低 → 高）
type geminiQuestion struct {
	Type         string      `json:"type"`
	Instructions string      `json:"instructions"`
	Criteria     interface{} `json:"criteria,omitempty"`
}

// geminiDecisionRequest 请求体。
type geminiDecisionRequest struct {
	Model     string                    `json:"model"`
	Input     []decisionContent         `json:"input"`
	Questions map[string]geminiQuestion `json:"questions"`
}

// geminiDecisionAnswer 单个问题的回答。
// 指针类型用于区分「字段缺失」与「值为 0」。
type geminiDecisionAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence"`
}

// geminiDecisionResponse 响应体（只取需要的字段）。
type geminiDecisionResponse struct {
	Answers map[string]geminiDecisionAnswer `json:"answers"`
	Usage   struct {
		TotalInputTokens int `json:"total_input_tokens"`
	} `json:"usage"`
}

// decisionAPIError 供应商错误响应体：
// {"error":{"code":"safety","message":"...","status":"INVALID_ARGUMENT"}}
type decisionAPIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// DecisionBlockedError 表示输入被供应商安全策略直接拒绝（HTTP 400 + 安全类错误码），
// 或模型明确拒答（refusal）。调用方据此合成 blocked / prohibited 结果，而不是当作调用失败。
type DecisionBlockedError struct {
	Code    string // 供应商错误码，如 safety / image_safety / refusal
	Message string // 供应商错误描述
}

// Error 实现 error 接口（错误消息保持英文，便于日志检索）。
func (e *DecisionBlockedError) Error() string {
	return "decision input blocked or declined (code: " + e.Code + ")"
}

// geminiDecisionClient Gemini 原生 Decisions API 客户端。
// 并发安全：内部只持有不可变的配置与 *http.Client，可被多个请求复用。
type geminiDecisionClient struct {
	endpoint   string
	keys       apiKeyPool // 多个 Key 用 "|" 分隔配置，每次调用随机取一个
	model      string
	resolution string
	httpClient *http.Client
}

// newGeminiDecisionClient 按渠道配置构建客户端。
// base_url 允许带或不带 /v1beta：缺失时自动补齐，末尾斜杠会被归一化。
func newGeminiDecisionClient(ch config.DecisionChannelConfig, timeout time.Duration) *geminiDecisionClient {
	base := strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	if !strings.HasSuffix(base, "/v1beta") {
		base += "/v1beta"
	}
	return &geminiDecisionClient{
		endpoint:   base + "/decisions",
		keys:       newAPIKeyPool(ch.APIKey),
		model:      ch.Model,
		resolution: ch.Resolution,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// evaluate 执行一次决策调用并归一化回答。
func (c *geminiDecisionClient) evaluate(ctx context.Context, in *DecisionInput) (*decisionAnswers, error) {
	input, err := c.buildInput(in)
	if err != nil {
		return nil, err
	}

	resp, err := c.call(ctx, input, buildGeminiQuestions(in.Mode))
	if err != nil {
		return nil, err
	}
	return answersFromGemini(in.Mode, resp)
}

// buildInput 把决策输入转成 Gemini 的 content 块。
func (c *geminiDecisionClient) buildInput(in *DecisionInput) ([]decisionContent, error) {
	switch in.Mode {
	case DecisionModeImage:
		return []decisionContent{
			{Type: "text", Text: decisionInputHint},
			{
				Type:       "image",
				Data:       stripDataURIPrefix(in.Image.DataURI),
				MIMEType:   in.Image.MIME,
				Resolution: c.resolution,
			},
		}, nil
	case DecisionModeText:
		return []decisionContent{{Type: "text", Text: in.Text}}, nil
	default:
		return nil, model.NewBizError("unsupported decision mode: " + in.Mode)
	}
}

// buildGeminiQuestions 按输入模式构建问题集合：每分类一个 noul + 一个严重度 score。
func buildGeminiQuestions(mode string) map[string]geminiQuestion {
	order := decisionCategoryOrder(mode)
	instructions := decisionCategoryInstructions(mode)

	questions := make(map[string]geminiQuestion, len(order)+1)
	for _, category := range order {
		questions[category] = geminiQuestion{
			Type:         decisionQuestionNoul,
			Instructions: instructions[category],
		}
	}
	questions[decisionSeverityKey] = geminiQuestion{
		Type:         decisionQuestionScore,
		Instructions: "综合评估内容的风险严重程度。",
		Criteria:     decisionSeverityCriteria,
	}
	return questions
}

// answersFromGemini 把 Gemini 的 answers 映射为归一化回答。
func answersFromGemini(mode string, resp *geminiDecisionResponse) (*decisionAnswers, error) {
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
	for _, category := range order {
		answer, ok := resp.Answers[category]
		if !ok || answer.Noul == nil {
			return nil, model.NewBizError("decision response missing category answer: " + category)
		}
		answers.Probabilities[category] = *answer.Noul
		answers.Confidences[category] = answer.Confidence
	}

	severity, ok := resp.Answers[decisionSeverityKey]
	if !ok || severity.Score == nil {
		return nil, model.NewBizError("decision response missing risk severity score")
	}
	answers.Severity = *severity.Score
	return answers, nil
}

// call 执行单次 HTTP 请求并解析响应。
func (c *geminiDecisionClient) call(
	ctx context.Context,
	input []decisionContent,
	questions map[string]geminiQuestion,
) (*geminiDecisionResponse, error) {
	body, err := json.Marshal(geminiDecisionRequest{
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
	req.Header.Set("x-goog-api-key", c.keys.pick())

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
		var out geminiDecisionResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("parse decision response: %w (raw: %s)", err, truncateForLog(string(raw)))
		}
		return &out, nil
	}

	// 错误响应：尽量解析出供应商错误码与描述
	var apiErr decisionAPIError
	_ = json.Unmarshal(raw, &apiErr)
	code := apiErr.Error.Code
	message := apiErr.Error.Message
	if message == "" {
		message = truncateForLog(strings.TrimSpace(string(raw)))
	}

	// 安全策略拦截：整个请求被拒，需要转成 blocked 结果
	if resp.StatusCode == http.StatusBadRequest && decisionBlockedCodes[code] {
		return nil, &DecisionBlockedError{Code: code, Message: message}
	}
	return nil, fmt.Errorf("decision request failed (HTTP %d): %s", resp.StatusCode, message)
}

// stripDataURIPrefix 去掉 data URI 前缀，只保留裸 base64。
func stripDataURIPrefix(dataURI string) string {
	if !strings.HasPrefix(dataURI, "data:") {
		return dataURI
	}
	if idx := strings.Index(dataURI, ","); idx >= 0 {
		return dataURI[idx+1:]
	}
	return dataURI
}
