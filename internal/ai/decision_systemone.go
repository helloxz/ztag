package ai

// System One（Jev 系）Decisions 原生 HTTP 客户端。
//
// 协议要点（OpenRouter /api/alpha/decisions、TypeSafe 等）：
//
//	POST {base_url}/decisions
//	Header: Authorization: Bearer <key>
//	Body:   {"model": "...", "state": [...], "questions": {"name": {...}}}
//	返回:   {"answers": {"name": {...}}}
//
// 与 openai-decision 的关键差异：
//   - 输入字段是 state（字符串，或字符串/图片块组成的数组），不是 input；
//   - 图片块是 {"type":"image_url","image_url":{"url":"data:..."}}，
//     不是 Responses API 的 input_image 平铺写法；
//   - questions 是「问题名 → 定义」的对象映射，类型为 noul / score；
//   - answers 同样是对象映射。
//
// 图片格式与尺寸由上游决定（例如 microsoft/microsoft-decision-1.1 只收
// PNG/JPEG、每边 280~1568），本客户端不做额外的图片规整。
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

// systemOneImageURL 图片引用：上游要求嵌套的 url 对象。
type systemOneImageURL struct {
	URL string `json:"url"`
}

// systemOnePart state 数组中的内容块（当前只用到图片）。
type systemOnePart struct {
	Type     string            `json:"type"` // image_url
	ImageURL systemOneImageURL `json:"image_url"`
}

// systemOneQuestion 单个问题定义。criteria 依 type 不同：
//   - noul：可为空（或 true/false 描述对象）
//   - score：有序等级描述数组（低 → 高）
type systemOneQuestion struct {
	Type         string      `json:"type"`
	Instructions string      `json:"instructions"`
	Criteria     interface{} `json:"criteria,omitempty"`
}

// systemOneRequest 请求体：state 为字符串或内容块数组。
type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     interface{}                  `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

// systemOneAnswer 单个问题的回答。
// 部分实现用 noul，部分用 probability 表达「是否」的概率，这里都兼容。
type systemOneAnswer struct {
	Type        string   `json:"type"` // noul | score | refusal
	Noul        *float64 `json:"noul,omitempty"`
	Probability *float64 `json:"probability,omitempty"`
	Score       *float64 `json:"score,omitempty"`
	Confidence  *float64 `json:"confidence,omitempty"`
}

// systemOneResponse 响应体（只取需要的字段）。
type systemOneResponse struct {
	Answers map[string]systemOneAnswer `json:"answers"`
}

// systemOneAPIError OpenAI 风格错误体（OpenRouter 等使用同一信封）。
type systemOneAPIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}

// systemOneDecisionClient System One Decisions 客户端。
// 并发安全：内部只持有不可变的配置与 *http.Client，可被多个请求复用。
type systemOneDecisionClient struct {
	endpoint   string
	keys       apiKeyPool // 多个 Key 用 "|" 分隔配置，每次调用随机取一个
	model      string
	httpClient *http.Client
}

// newSystemOneDecisionClient 按渠道配置构建客户端。
// base_url 末尾斜杠会被归一化，实际请求 {base_url}/decisions。
func newSystemOneDecisionClient(ch config.DecisionChannelConfig, timeout time.Duration) *systemOneDecisionClient {
	base := strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	return &systemOneDecisionClient{
		endpoint:   base + "/decisions",
		keys:       newAPIKeyPool(ch.APIKey),
		model:      ch.Model,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// evaluate 执行一次决策调用并归一化回答。
func (c *systemOneDecisionClient) evaluate(ctx context.Context, in *DecisionInput) (*decisionAnswers, error) {
	state, err := buildSystemOneState(in)
	if err != nil {
		return nil, err
	}

	resp, err := c.call(ctx, state, buildSystemOneQuestions(in.Mode))
	if err != nil {
		return nil, err
	}
	return answersFromSystemOne(in.Mode, resp)
}

// buildSystemOneState 构造输入：文本直接用字符串，图片用「文字 + 图片块」数组。
func buildSystemOneState(in *DecisionInput) (interface{}, error) {
	switch in.Mode {
	case DecisionModeImage:
		return []interface{}{
			decisionInputHint,
			systemOnePart{
				Type:     "image_url",
				ImageURL: systemOneImageURL{URL: in.Image.DataURI},
			},
		}, nil
	case DecisionModeText:
		return in.Text, nil
	default:
		return nil, model.NewBizError("unsupported decision mode: " + in.Mode)
	}
}

// buildSystemOneQuestions 按输入模式构建问题映射：每分类一个 noul + 一个严重度 score。
func buildSystemOneQuestions(mode string) map[string]systemOneQuestion {
	order := decisionCategoryOrder(mode)
	instructions := decisionCategoryInstructions(mode)

	questions := make(map[string]systemOneQuestion, len(order)+1)
	for _, category := range order {
		questions[category] = systemOneQuestion{
			Type:         decisionQuestionNoul,
			Instructions: instructions[category],
		}
	}
	questions[decisionSeverityKey] = systemOneQuestion{
		Type:         decisionQuestionScore,
		Instructions: "综合评估内容的风险严重程度。",
		Criteria:     decisionSeverityCriteria,
	}
	return questions
}

// answersFromSystemOne 把 answers 映射转换为归一化回答。
// 出现 refusal 时返回 *DecisionBlockedError（上层合成 blocked / 99）。
func answersFromSystemOne(mode string, resp *systemOneResponse) (*decisionAnswers, error) {
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

	// 模型拒答：任意问题返回 refusal 都按 blocked 处理
	for name, answer := range resp.Answers {
		if answer.Type == "refusal" {
			return nil, &DecisionBlockedError{
				Code:    "refusal",
				Message: "decision model declined to answer: " + name,
			}
		}
	}

	for _, category := range order {
		answer, ok := resp.Answers[category]
		if !ok {
			return nil, model.NewBizError("decision response missing category answer: " + category)
		}
		probability := answer.Noul
		if probability == nil {
			probability = answer.Probability
		}
		if probability == nil {
			return nil, model.NewBizError("decision response missing probability for category: " + category)
		}
		answers.Probabilities[category] = *probability
		if answer.Confidence != nil {
			answers.Confidences[category] = *answer.Confidence
		}
	}

	severity, ok := resp.Answers[decisionSeverityKey]
	if !ok || severity.Score == nil {
		return nil, model.NewBizError("decision response missing risk severity score")
	}
	answers.Severity = *severity.Score
	return answers, nil
}

// call 执行单次 HTTP 请求并解析响应。
func (c *systemOneDecisionClient) call(
	ctx context.Context,
	state interface{},
	questions map[string]systemOneQuestion,
) (*systemOneResponse, error) {
	body, err := json.Marshal(systemOneRequest{
		Model:     c.model,
		State:     state,
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
		var out systemOneResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("parse decision response: %w (raw: %s)", err, truncateForLog(string(raw)))
		}
		return &out, nil
	}

	var apiErr systemOneAPIError
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
