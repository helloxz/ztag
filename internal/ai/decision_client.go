package ai

// Gemini Decisions API 原生 HTTP 客户端。
//
// 为什么不用 goai / 官方 Go SDK：
//   - goai provider 只封装 generateContent 等聊天接口，没有 decisions 端点；
//   - 官方 google.golang.org/genai 的发布版不保证已支持 Decisions API；
//   - 端点本身只是一次 POST + 固定 JSON，自研可精确控制重试与安全拦截识别。
//
// 协议要点（v1beta）：
//   POST {base_url}/v1beta/decisions
//   Header: x-goog-api-key
//   Body:   {"model": "...", "input": [...], "questions": {...}}
//   返回:   {"answers": {...}, "usage": {...}}，输出 token 恒为 0
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/helloxz/ztag/internal/config"
)

// decisionMaxResponseBytes 单次响应体读取上限（answers 是结构化的短 JSON，留足余量即可）。
const decisionMaxResponseBytes = 4 << 20

// decisionRetryBackoffBase 重试退避基数：第 n 次重试等待 n × base。
const decisionRetryBackoffBase = 300 * time.Millisecond

// 问题类型：默认只用 noul（是否）与 score（有序评分），choice 保留常量备用。
const (
	decisionQuestionNoul  = "noul"
	decisionQuestionScore = "score"
)

// decisionBlockedCodes 供应商安全策略拦截的错误码白名单。
// 只有命中这些 code 的 400 才会被判定为「违规输入」，其余 400 一律当参数错误。
var decisionBlockedCodes = map[string]bool{
	"safety":             true,
	"prohibited_content": true,
	"blocklist":          true,
	"image_safety":       true,
	"content_blocked":    true,
	"jailbreak":          true,
}

// decisionProhibitedCodes 属于极危拦截（通常为 CSAM）的错误码子集：
// 对外的 category 会从 blocked 升级为 prohibited，风险分固定 100。
// 取 fail-closed：blocklist 可能是已知违规哈希，宁可按最严重处理。
var decisionProhibitedCodes = map[string]bool{
	"prohibited_content": true,
	"blocklist":          true,
}

// decisionContent 输入内容块（文本 / 图片）。
type decisionContent struct {
	Type       string `json:"type"`                 // text | image
	Text       string `json:"text,omitempty"`       // text 块内容
	Data       string `json:"data,omitempty"`       // image 块：裸 base64（不含 data URI 前缀）
	MIMEType   string `json:"mime_type,omitempty"`  // image 块：真实 MIME，如 image/webp
	Resolution string `json:"resolution,omitempty"` // image 块：low | medium | high | ultra_high
}

// decisionQuestion 单个问题定义。criteria 依 type 不同：
//   - noul：可为空（或 true_description / false_description 对象）
//   - choice：option key → 描述 的对象
//   - score：有序等级描述数组（低 → 高）
type decisionQuestion struct {
	Type         string      `json:"type"`
	Instructions string      `json:"instructions"`
	Criteria     interface{} `json:"criteria,omitempty"`
}

// decisionRequest 请求体。
type decisionRequest struct {
	Model     string                      `json:"model"`
	Input     []decisionContent           `json:"input"`
	Questions map[string]decisionQuestion `json:"questions"`
}

// decisionAnswer 单个问题的回答。
// 指针类型用于区分「字段缺失」与「值为 0」。
type decisionAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence"`
}

// decisionResponse 响应体（只取需要的字段）。
type decisionResponse struct {
	Answers map[string]decisionAnswer `json:"answers"`
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

// DecisionBlockedError 表示输入被供应商安全策略直接拒绝（HTTP 400 + 安全类错误码）。
// 调用方据此合成 blocked / 100 的结果，而不是当作调用失败。
type DecisionBlockedError struct {
	Code    string // 供应商错误码，如 safety / image_safety
	Message string // 供应商错误描述
}

// Error 实现 error 接口（错误消息保持英文，便于日志检索）。
func (e *DecisionBlockedError) Error() string {
	return "decision input blocked by provider safety filter (code: " + e.Code + ")"
}

// decisionClient Decisions API 轻量客户端。
// 并发安全：内部只持有不可变的配置与 *http.Client，可被多个请求复用。
type decisionClient struct {
	httpClient *http.Client
	endpoint   string        // 完整端点地址
	apiKey     string        // x-goog-api-key
	model      string        // 模型标识
	timeout    time.Duration // 单次调用总超时（含重试）
	maxRetries int           // 失败重试次数（0 表示不重试）
}

// newDecisionClient 按配置构建客户端。
// base_url 允许带或不带 /v1beta：缺失时自动补齐，末尾斜杠会被归一化。
func newDecisionClient(cfg config.DecisionConfig, maxRetries int) *decisionClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = config.DefaultDecisionBaseURL
	}
	if !strings.HasSuffix(base, "/v1beta") {
		base += "/v1beta"
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &decisionClient{
		httpClient: &http.Client{},
		endpoint:   base + "/decisions",
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		timeout:    time.Duration(cfg.Timeout) * time.Second,
		maxRetries: maxRetries,
	}
}

// call 执行一次决策请求（含重试）：
//   - 网络错误 / 429 / 5xx → 按 maxRetries 指数式退避重试；
//   - 400 命中安全类错误码 → 返回 *DecisionBlockedError（不重试）；
//   - 其余 4xx（含参数错误、鉴权失败）→ 直接返回错误，不重试。
func (c *decisionClient) call(ctx context.Context, input []decisionContent, questions map[string]decisionQuestion) (*decisionResponse, error) {
	// 总超时覆盖「序列化 + 全部重试」，避免重试把请求拖到不可控的时长
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	body, err := json.Marshal(decisionRequest{
		Model:     c.model,
		Input:     input,
		Questions: questions,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal decision request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepWithContext(ctx, time.Duration(attempt)*decisionRetryBackoffBase); err != nil {
				return nil, err
			}
		}

		resp, retryable, err := c.once(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// 安全拦截与不可重试错误立即返回
		var blockedErr *DecisionBlockedError
		if errors.As(err, &blockedErr) || !retryable {
			return nil, err
		}
		slog.Warn("decision request failed, retrying",
			"attempt", attempt+1, "max_retries", c.maxRetries, "model", c.model, "err", err)
	}
	return nil, lastErr
}

// once 执行单次 HTTP 请求，返回 (响应, 是否可重试, 错误)。
func (c *decisionClient) once(ctx context.Context, body []byte) (*decisionResponse, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("build decision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// 超时或主动取消：不再重试
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("decision request canceled: %w", ctx.Err())
		}
		return nil, true, fmt.Errorf("decision request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, decisionMaxResponseBytes))
	if readErr != nil {
		return nil, true, fmt.Errorf("read decision response: %w", readErr)
	}

	// 成功响应
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var out decisionResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false, fmt.Errorf("parse decision response: %w (raw: %s)", err, truncateForLog(string(raw)))
		}
		return &out, false, nil
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
		return nil, false, &DecisionBlockedError{Code: code, Message: message}
	}

	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	return nil, retryable, fmt.Errorf("decision request failed (HTTP %d): %s", resp.StatusCode, message)
}

// sleepWithContext 等待指定时长，期间响应 ctx 取消。
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
