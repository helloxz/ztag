package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// newTestDecisionAnalyzer 用 httptest 搭建决策分析器，避免真实网络调用。
func newTestDecisionAnalyzer(t *testing.T, handler http.HandlerFunc) *decisionAnalyzer {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := newDecisionClient(config.DecisionConfig{
		Enabled:    true,
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "gemini-decision-1",
		Resolution: "high",
		Timeout:    5,
	}, 1)
	return &decisionAnalyzer{client: client, resolution: "high", model: "gemini-decision-1"}
}

// decisionAnswersJSON 构造模型回答：指定分类给高概率，其余给最低概率。
func decisionAnswersJSON(t *testing.T, category string, prob, severity float64) string {
	t.Helper()
	answers := make(map[string]any, len(decisionCategoryOrder)+1)
	for _, c := range decisionCategoryOrder {
		p := 0.01
		if c == category {
			p = prob
		}
		answers[c] = map[string]any{"type": "noul", "noul": p, "confidence": 0.9}
	}
	answers[decisionSeverityKey] = map[string]any{"type": "score", "score": severity, "confidence": 0.8}
	body, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		t.Fatalf("marshal test answers: %v", err)
	}
	return string(body)
}

// testImage 一张最小的已预处理图片（内容不会被真实解析）。
func testImage() *model.PreparedImage {
	return &model.PreparedImage{
		DataURI: "data:image/webp;base64,AAAA",
		MIME:    "image/webp",
	}
}

// TestEvaluateImageSuccess 覆盖正常解析：分类取最大概率、评分按 rubric 归一化。
func TestEvaluateImageSuccess(t *testing.T) {
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, decisionAnswersJSON(t, "porn", 0.87, 3))
	})

	result, err := a.EvaluateImage(context.Background(), testImage())
	if err != nil {
		t.Fatalf("EvaluateImage returned error: %v", err)
	}
	if result.Category != "porn" {
		t.Errorf("category = %q, want %q", result.Category, "porn")
	}
	// rubric 0~4，severity=3 → 3/4*100 = 75
	if result.Score != 75 {
		t.Errorf("score = %d, want 75", result.Score)
	}
	if result.Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", result.Confidence)
	}
	if len(result.Probabilities) != len(decisionCategoryOrder) {
		t.Errorf("probabilities size = %d, want %d", len(result.Probabilities), len(decisionCategoryOrder))
	}
}

// TestEvaluateImageNormalLowRisk 普通图片应为 normal 且低分。
func TestEvaluateImageNormalLowRisk(t *testing.T) {
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, decisionAnswersJSON(t, "normal", 0.96, 0))
	})

	result, err := a.EvaluateImage(context.Background(), testImage())
	if err != nil {
		t.Fatalf("EvaluateImage returned error: %v", err)
	}
	if result.Category != "normal" {
		t.Errorf("category = %q, want normal", result.Category)
	}
	if result.Score != 0 {
		t.Errorf("score = %d, want 0", result.Score)
	}
}

// TestEvaluateImageScoreCap 自然判定最高只能到 99，100 专用于安全拦截。
func TestEvaluateImageScoreCap(t *testing.T) {
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, decisionAnswersJSON(t, "gore", 0.99, 4))
	})

	result, err := a.EvaluateImage(context.Background(), testImage())
	if err != nil {
		t.Fatalf("EvaluateImage returned error: %v", err)
	}
	if result.Score != decisionMaxNaturalScore {
		t.Errorf("score = %d, want %d", result.Score, decisionMaxNaturalScore)
	}
}

// TestEvaluateImageBlocked 普通安全拦截 → blocked / 99，且不当作错误。
func TestEvaluateImageBlocked(t *testing.T) {
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"image_safety","message":"Request blocked due to safety violations."}}`)
	})

	result, err := a.EvaluateImage(context.Background(), testImage())
	if err != nil {
		t.Fatalf("EvaluateImage returned error: %v", err)
	}
	if result.Category != model.CategoryBlocked {
		t.Errorf("category = %q, want %q", result.Category, model.CategoryBlocked)
	}
	if result.Score != decisionBlockedScore {
		t.Errorf("score = %d, want %d", result.Score, decisionBlockedScore)
	}
	if result.BlockedCode != "image_safety" {
		t.Errorf("blocked code = %q, want image_safety", result.BlockedCode)
	}
}

// TestEvaluateImageProhibited 极危拦截（通常为 CSAM）→ prohibited / 100。
func TestEvaluateImageProhibited(t *testing.T) {
	cases := []string{"prohibited_content", "blocklist"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":{"code":%q,"message":"Request blocked."}}`, code)
			})

			result, err := a.EvaluateImage(context.Background(), testImage())
			if err != nil {
				t.Fatalf("EvaluateImage returned error: %v", err)
			}
			if result.Category != model.CategoryProhibited {
				t.Errorf("category = %q, want %q", result.Category, model.CategoryProhibited)
			}
			if result.Score != decisionProhibitedScore {
				t.Errorf("score = %d, want %d", result.Score, decisionProhibitedScore)
			}
			if result.BlockedCode != code {
				t.Errorf("blocked code = %q, want %q", result.BlockedCode, code)
			}
		})
	}
}

// TestEvaluateImageOtherBadRequest 非安全类 400（如参数错误）不能误判为违规。
func TestEvaluateImageOtherBadRequest(t *testing.T) {
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"INVALID_ARGUMENT","message":"Unsupported mime type."}}`)
	})

	result, err := a.EvaluateImage(context.Background(), testImage())
	if err == nil {
		t.Fatalf("expected error, got result: %+v", result)
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

// TestEvaluateImageAuthFailure 鉴权失败不重试、不映射为违规。
func TestEvaluateImageAuthFailure(t *testing.T) {
	var calls int32
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"UNAUTHENTICATED","message":"API key not valid."}}`)
	})

	if _, err := a.EvaluateImage(context.Background(), testImage()); err == nil {
		t.Fatal("expected error for unauthorized response")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 401)", got)
	}
}

// TestEvaluateImageRetryOn429 429 后重试成功。
func TestEvaluateImageRetryOn429(t *testing.T) {
	var calls int32
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`)
			return
		}
		fmt.Fprint(w, decisionAnswersJSON(t, "gambling", 0.8, 2))
	})

	result, err := a.EvaluateImage(context.Background(), testImage())
	if err != nil {
		t.Fatalf("EvaluateImage returned error: %v", err)
	}
	if result.Category != "gambling" {
		t.Errorf("category = %q, want gambling", result.Category)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// TestEvaluateImageMissingAnswer 回答缺少字段时必须报错，不能默认成 normal。
func TestEvaluateImageMissingAnswer(t *testing.T) {
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"answers":{"risk_severity":{"type":"score","score":2,"confidence":0.9}}}`)
	})

	if _, err := a.EvaluateImage(context.Background(), testImage()); err == nil {
		t.Fatal("expected error for missing category answers")
	}
}

// TestEvaluateImageRequestShape 校验请求体：端点、鉴权头、问题数与图片块。
func TestEvaluateImageRequestShape(t *testing.T) {
	var got decisionRequest
	a := newTestDecisionAnalyzer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1beta/decisions" {
			t.Errorf("path = %s, want /v1beta/decisions", r.URL.Path)
		}
		if key := r.Header.Get("x-goog-api-key"); key != "test-key" {
			t.Errorf("x-goog-api-key = %q, want test-key", key)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, decisionAnswersJSON(t, "normal", 0.9, 0))
	})

	if _, err := a.EvaluateImage(context.Background(), testImage()); err != nil {
		t.Fatalf("EvaluateImage returned error: %v", err)
	}
	if got.Model != "gemini-decision-1" {
		t.Errorf("model = %q, want gemini-decision-1", got.Model)
	}
	if len(got.Questions) != len(decisionCategoryOrder)+1 {
		t.Errorf("questions = %d, want %d", len(got.Questions), len(decisionCategoryOrder)+1)
	}
	if len(got.Input) != 2 || got.Input[1].Type != "image" {
		t.Fatalf("input = %+v, want [text, image]", got.Input)
	}
	if got.Input[1].Data != "AAAA" {
		t.Errorf("image data = %q, want base64 payload without data URI prefix", got.Input[1].Data)
	}
	if got.Input[1].MIMEType != "image/webp" {
		t.Errorf("image mime = %q, want image/webp", got.Input[1].MIMEType)
	}
	if got.Input[1].Resolution != "high" {
		t.Errorf("image resolution = %q, want high", got.Input[1].Resolution)
	}
}

// TestDecisionCategories 分类集合固定：已移除二维码场景（识别不准），保留风险类目。
func TestDecisionCategories(t *testing.T) {
	want := []string{
		"porn", "gore", "violence", "terror", "politics", "gambling",
		"fraud", "drugs", "suggestive", "other_risk", "normal",
	}
	if len(decisionCategoryOrder) != len(want) {
		t.Fatalf("category count = %d, want %d (%v)", len(decisionCategoryOrder), len(want), decisionCategoryOrder)
	}
	for i, category := range want {
		if decisionCategoryOrder[i] != category {
			t.Errorf("category[%d] = %q, want %q", i, decisionCategoryOrder[i], category)
		}
	}
	for _, category := range decisionCategoryOrder {
		if _, ok := decisionCategoryInstructions[category]; !ok {
			t.Errorf("category %q has no instructions", category)
		}
	}
}

// TestNewDecisionClientEndpoint base_url 归一化：带不带 /v1beta、带不带尾部斜杠都能拼对。
func TestNewDecisionClientEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://generativelanguage.googleapis.com":        "https://generativelanguage.googleapis.com/v1beta/decisions",
		"https://generativelanguage.googleapis.com/":       "https://generativelanguage.googleapis.com/v1beta/decisions",
		"https://generativelanguage.googleapis.com/v1beta": "https://generativelanguage.googleapis.com/v1beta/decisions",
		" https://proxy.example.com/gemini/v1beta/ ":       "https://proxy.example.com/gemini/v1beta/decisions",
		"": config.DefaultDecisionBaseURL + "/v1beta/decisions",
	}
	for base, want := range cases {
		got := newDecisionClient(config.DecisionConfig{BaseURL: base}, 0).endpoint
		if got != want {
			t.Errorf("base_url %q → endpoint %q, want %q", base, got, want)
		}
	}
}

// TestNewDecisionAnalyzerDisabled enabled=false 时返回未配置错误。
func TestNewDecisionAnalyzerDisabled(t *testing.T) {
	analyzer := NewDecisionAnalyzer(config.AIConfig{Decision: config.DecisionConfig{Enabled: false}})
	_, err := analyzer.EvaluateImage(context.Background(), testImage())
	if err == nil {
		t.Fatal("expected error when decision model is disabled")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v, want not configured", err)
	}
}
