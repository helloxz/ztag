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

// ---------- 测试辅助 ----------

// testImage 一张最小的已预处理图片（内容不会被真实解析）。
func testImage() *model.PreparedImage {
	return &model.PreparedImage{
		DataURI: "data:image/webp;base64,AAAA",
		MIME:    "image/webp",
	}
}

// newTestGateway 用 httptest 搭建单渠道网关，避免真实网络调用。
func newTestGateway(t *testing.T, channelType string, handler http.HandlerFunc) *DecisionGateway {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	analyzer := NewDecisionAnalyzer(config.AIConfig{
		Decision: config.DecisionConfig{
			Enabled:      true,
			Timeout:      5,
			MaxTextChars: 100,
			Channels: []config.DecisionChannelConfig{{
				ID:         "test-" + channelType,
				Type:       channelType,
				Enabled:    true,
				BaseURL:    srv.URL,
				APIKey:     "test-key",
				Model:      "test-model",
				Resolution: config.DecisionResolutionHigh,
			}},
		},
	})
	gw, ok := analyzer.(*DecisionGateway)
	if !ok {
		t.Fatalf("analyzer type = %T, want *DecisionGateway", analyzer)
	}
	return gw
}

// geminiAnswers 构造 Gemini 协议回答：指定分类给高概率，其余给最低概率。
func geminiAnswers(t *testing.T, mode, category string, probability, severity float64) string {
	t.Helper()
	order := decisionCategoryOrder(mode)
	answers := make(map[string]any, len(order)+1)
	for _, c := range order {
		p := 0.01
		if c == category {
			p = probability
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

// openAIAnswers 构造 OpenAI 协议回答（数组形式）。
func openAIAnswers(t *testing.T, mode, category string, probability, severity float64) string {
	t.Helper()
	answers := make([]map[string]any, 0, len(decisionCategoryOrder(mode))+1)
	for _, c := range decisionCategoryOrder(mode) {
		p := 0.01
		if c == category {
			p = probability
		}
		answers = append(answers, map[string]any{"type": "predicate", "name": c, "probability": p})
	}
	answers = append(answers, map[string]any{"type": "score", "name": decisionSeverityKey, "score": severity})
	body, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		t.Fatalf("marshal test answers: %v", err)
	}
	return string(body)
}

func imageInput() *DecisionInput {
	return &DecisionInput{Mode: DecisionModeImage, Image: testImage()}
}

func textInput(text string) *DecisionInput {
	return &DecisionInput{Mode: DecisionModeText, Text: text}
}

// ---------- Gemini 协议 ----------

// TestGeminiImageSuccess 正常解析：分类取最大概率，评分按 rubric 归一化。
func TestGeminiImageSuccess(t *testing.T) {
	gw := newTestGateway(t, config.DecisionChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "porn", 0.87, 3))
	})

	result, err := gw.Evaluate(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "porn" {
		t.Errorf("category = %q, want porn", result.Category)
	}
	// rubric 0~4，severity=3 → 3/4*100 = 75
	if result.Score != 75 {
		t.Errorf("score = %d, want 75", result.Score)
	}
	if result.Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", result.Confidence)
	}
	if result.ModelID != "test-"+config.DecisionChannelTypeGemini {
		t.Errorf("model_id = %q, want test channel id", result.ModelID)
	}
	if len(result.Probabilities) != len(decisionCategoryOrder(DecisionModeImage)) {
		t.Errorf("probabilities size = %d, want %d", len(result.Probabilities), len(decisionCategoryOrder(DecisionModeImage)))
	}
}

// TestGeminiScoreCap 自然判定封顶 99，100 只留给极危拦截。
func TestGeminiScoreCap(t *testing.T) {
	gw := newTestGateway(t, config.DecisionChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "gore", 0.99, 4))
	})

	result, err := gw.Evaluate(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Score != decisionMaxNaturalScore {
		t.Errorf("score = %d, want %d", result.Score, decisionMaxNaturalScore)
	}
}

// TestGeminiBlockedCodes 普通安全拦截 → blocked/99；极危拦截 → prohibited/100。
func TestGeminiBlockedCodes(t *testing.T) {
	cases := []struct {
		code     string
		category string
		score    int
	}{
		{"image_safety", model.CategoryBlocked, decisionBlockedScore},
		{"safety", model.CategoryBlocked, decisionBlockedScore},
		{"prohibited_content", model.CategoryProhibited, decisionProhibitedScore},
		{"blocklist", model.CategoryProhibited, decisionProhibitedScore},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			gw := newTestGateway(t, config.DecisionChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":{"code":%q,"message":"blocked"}}`, tc.code)
			})

			result, err := gw.Evaluate(context.Background(), imageInput())
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if result.Category != tc.category {
				t.Errorf("category = %q, want %q", result.Category, tc.category)
			}
			if result.Score != tc.score {
				t.Errorf("score = %d, want %d", result.Score, tc.score)
			}
			if result.BlockedCode != tc.code {
				t.Errorf("blocked_code = %q, want %q", result.BlockedCode, tc.code)
			}
		})
	}
}

// TestGeminiNonSafetyBadRequest 非安全类 400 不能误判为违规；单渠道会重试一次后报错。
func TestGeminiNonSafetyBadRequest(t *testing.T) {
	var calls int32
	gw := newTestGateway(t, config.DecisionChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"INVALID_ARGUMENT","message":"unsupported mime"}}`)
	})

	if _, err := gw.Evaluate(context.Background(), imageInput()); err == nil {
		t.Fatal("expected error for non-safety 400")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (single channel retries once)", got)
	}
}

// TestGeminiRequestShape 校验请求体：端点、鉴权头、问题数、图片块（去掉 data URI 前缀）。
func TestGeminiRequestShape(t *testing.T) {
	var got geminiDecisionRequest
	gw := newTestGateway(t, config.DecisionChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/decisions" {
			t.Errorf("path = %s, want /v1beta/decisions", r.URL.Path)
		}
		if key := r.Header.Get("x-goog-api-key"); key != "test-key" {
			t.Errorf("x-goog-api-key = %q, want test-key", key)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "normal", 0.9, 0))
	})

	if _, err := gw.Evaluate(context.Background(), imageInput()); err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if got.Model != "test-model" {
		t.Errorf("model = %q, want test-model", got.Model)
	}
	if len(got.Questions) != len(decisionCategoryOrder(DecisionModeImage))+1 {
		t.Errorf("questions = %d, want %d", len(got.Questions), len(decisionCategoryOrder(DecisionModeImage))+1)
	}
	if len(got.Input) != 2 || got.Input[1].Type != "image" {
		t.Fatalf("input = %+v, want [text, image]", got.Input)
	}
	if got.Input[1].Data != "AAAA" {
		t.Errorf("image data = %q, want base64 without data URI prefix", got.Input[1].Data)
	}
	if got.Input[1].Resolution != config.DecisionResolutionHigh {
		t.Errorf("resolution = %q, want high", got.Input[1].Resolution)
	}
}

// TestGeminiTextMode 文本模式使用文本分类，且输入只有一段文本。
func TestGeminiTextMode(t *testing.T) {
	var got geminiDecisionRequest
	gw := newTestGateway(t, config.DecisionChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, geminiAnswers(t, DecisionModeText, "spam", 0.9, 1))
	})

	result, err := gw.Evaluate(context.Background(), textInput("加微信领福利"))
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "spam" {
		t.Errorf("category = %q, want spam", result.Category)
	}
	if len(got.Questions) != len(decisionCategoryOrder(DecisionModeText))+1 {
		t.Errorf("questions = %d, want %d", len(got.Questions), len(decisionCategoryOrder(DecisionModeText))+1)
	}
	if len(got.Input) != 1 || got.Input[0].Type != "text" || got.Input[0].Text != "加微信领福利" {
		t.Errorf("input = %+v, want single text block", got.Input)
	}
}

// TestGeminiEndpointNormalization base_url 归一化：带不带 /v1beta、带不带尾斜杠都拼对。
func TestGeminiEndpointNormalization(t *testing.T) {
	cases := map[string]string{
		"https://generativelanguage.googleapis.com":        "https://generativelanguage.googleapis.com/v1beta/decisions",
		"https://generativelanguage.googleapis.com/":       "https://generativelanguage.googleapis.com/v1beta/decisions",
		"https://generativelanguage.googleapis.com/v1beta": "https://generativelanguage.googleapis.com/v1beta/decisions",
		" https://proxy.example.com/gemini/v1beta/ ":       "https://proxy.example.com/gemini/v1beta/decisions",
	}
	for base, want := range cases {
		got := newGeminiDecisionClient(config.DecisionChannelConfig{BaseURL: base}, 0).endpoint
		if got != want {
			t.Errorf("base_url %q → endpoint %q, want %q", base, got, want)
		}
	}
}

// ---------- OpenAI 兼容协议 ----------

// TestOpenAIImageSuccess 数组形式回答解析 + 图片走 input_image data URL。
func TestOpenAIImageSuccess(t *testing.T) {
	var got openAIDecisionRequest
	gw := newTestGateway(t, config.DecisionChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions" {
			t.Errorf("path = %s, want /decisions", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("authorization = %q, want Bearer test-key", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, openAIAnswers(t, DecisionModeImage, "violence", 0.8, 1.3))
	})

	result, err := gw.Evaluate(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "violence" {
		t.Errorf("category = %q, want violence", result.Category)
	}
	// 1.3/4*100 = 32.5 → 33
	if result.Score != 33 {
		t.Errorf("score = %d, want 33", result.Score)
	}

	// 请求体：predicate + score，图片是 data URL
	if len(got.Questions) != len(decisionCategoryOrder(DecisionModeImage))+1 {
		t.Errorf("questions = %d, want %d", len(got.Questions), len(decisionCategoryOrder(DecisionModeImage))+1)
	}
	last := got.Questions[len(got.Questions)-1]
	if last.Type != "score" || len(last.Levels) != len(decisionSeverityCriteria) {
		t.Errorf("score question = %+v, want levels %d", last, len(decisionSeverityCriteria))
	}
	raw, err := json.Marshal(got.Input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	if !strings.Contains(string(raw), `"input_image"`) || !strings.Contains(string(raw), `data:image/webp;base64,AAAA`) {
		t.Errorf("input = %s, want input_image with data URL", raw)
	}
}

// TestOpenAIRefusalMapsToBlocked 模型拒答 → blocked/99（不是报错）。
func TestOpenAIRefusalMapsToBlocked(t *testing.T) {
	gw := newTestGateway(t, config.DecisionChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"answers":[{"type":"refusal","name":"porn"}]}`)
	})

	result, err := gw.Evaluate(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != model.CategoryBlocked {
		t.Errorf("category = %q, want blocked", result.Category)
	}
	if result.Score != decisionBlockedScore {
		t.Errorf("score = %d, want %d", result.Score, decisionBlockedScore)
	}
	if result.BlockedCode != "refusal" {
		t.Errorf("blocked_code = %q, want refusal", result.BlockedCode)
	}
}

// TestOpenAITextInputIsString 文本模式 input 是字符串，不是消息数组。
func TestOpenAITextInputIsString(t *testing.T) {
	var got openAIDecisionRequest
	gw := newTestGateway(t, config.DecisionChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, openAIAnswers(t, DecisionModeText, "ads", 0.9, 2))
	})

	if _, err := gw.Evaluate(context.Background(), textInput("代开发票")); err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if text, ok := got.Input.(string); !ok || text != "代开发票" {
		t.Errorf("input = %#v, want plain string", got.Input)
	}
}

// ---------- 路由：轮询 / 指定 / 兜底 ----------

// newMultiChannelGateway 两个渠道各指向一个 httptest 服务，便于断言路由。
func newMultiChannelGateway(t *testing.T, handlers ...http.HandlerFunc) *DecisionGateway {
	t.Helper()
	channels := make([]config.DecisionChannelConfig, 0, len(handlers))
	for i, h := range handlers {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		channels = append(channels, config.DecisionChannelConfig{
			ID:         fmt.Sprintf("ch-%d", i),
			Type:       config.DecisionChannelTypeGemini,
			Enabled:    true,
			BaseURL:    srv.URL,
			APIKey:     "k",
			Model:      "m",
			Resolution: config.DecisionResolutionHigh,
		})
	}
	analyzer := NewDecisionAnalyzer(config.AIConfig{Decision: config.DecisionConfig{
		Enabled: true, Timeout: 5, MaxTextChars: 100, Channels: channels,
	}})
	gw, ok := analyzer.(*DecisionGateway)
	if !ok {
		t.Fatalf("analyzer type = %T, want *DecisionGateway", analyzer)
	}
	return gw
}

// TestGatewayRoundRobin 未指定模型时按配置顺序轮询。
func TestGatewayRoundRobin(t *testing.T) {
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "normal", 0.9, 0))
	}
	gw := newMultiChannelGateway(t, okHandler, okHandler)

	want := []string{"ch-0", "ch-1", "ch-0"}
	for i, wantID := range want {
		result, err := gw.Evaluate(context.Background(), imageInput())
		if err != nil {
			t.Fatalf("call %d returned error: %v", i, err)
		}
		if result.ModelID != wantID {
			t.Errorf("call %d model_id = %q, want %q", i, result.ModelID, wantID)
		}
	}
}

// TestGatewayExplicitModel 指定渠道 id 时精确命中，不用轮询。
func TestGatewayExplicitModel(t *testing.T) {
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "normal", 0.9, 0))
	}
	gw := newMultiChannelGateway(t, okHandler, okHandler)

	in := imageInput()
	in.Model = "ch-1"
	for i := 0; i < 3; i++ {
		result, err := gw.Evaluate(context.Background(), in)
		if err != nil {
			t.Fatalf("returned error: %v", err)
		}
		if result.ModelID != "ch-1" {
			t.Errorf("model_id = %q, want ch-1", result.ModelID)
		}
	}
}

// TestGatewayUnknownModel 未知渠道 id 报错并带可用列表。
func TestGatewayUnknownModel(t *testing.T) {
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "normal", 0.9, 0))
	}
	gw := newMultiChannelGateway(t, okHandler, okHandler)

	in := imageInput()
	in.Model = "nope"
	_, err := gw.Evaluate(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not found", err)
	}
	if !strings.Contains(err.Error(), "ch-0") || !strings.Contains(err.Error(), "ch-1") {
		t.Errorf("err = %v, want available channel ids", err)
	}
}

// TestGatewayFallbackToNextChannel 多渠道失败后换下一个渠道再试一次。
func TestGatewayFallbackToNextChannel(t *testing.T) {
	failHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":"internal","message":"boom"}}`)
	}
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, geminiAnswers(t, DecisionModeImage, "gambling", 0.9, 1))
	}
	gw := newMultiChannelGateway(t, failHandler, okHandler)

	result, err := gw.Evaluate(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.ModelID != "ch-1" {
		t.Errorf("model_id = %q, want fallback channel ch-1", result.ModelID)
	}
	if result.Category != "gambling" {
		t.Errorf("category = %q, want gambling", result.Category)
	}
}

// ---------- 配置与分类 ----------

// TestDecisionCategories 图片与文本分类各自固定，且都必须有问题描述。
func TestDecisionCategories(t *testing.T) {
	imageWant := []string{
		"porn", "gore", "violence", "terror", "politics", "gambling",
		"fraud", "drugs", "suggestive", "other_risk", "normal",
	}
	textWant := []string{
		"porn", "politics", "spam", "ads", "abuse", "fraud", "illegal", "other_risk", "normal",
	}

	for mode, want := range map[string][]string{
		DecisionModeImage: imageWant,
		DecisionModeText:  textWant,
	} {
		got := decisionCategoryOrder(mode)
		if len(got) != len(want) {
			t.Fatalf("%s category count = %d, want %d", mode, len(got), len(want))
		}
		for i, category := range want {
			if got[i] != category {
				t.Errorf("%s category[%d] = %q, want %q", mode, i, got[i], category)
			}
		}
		instructions := decisionCategoryInstructions(mode)
		for _, category := range got {
			if _, ok := instructions[category]; !ok {
				t.Errorf("%s category %q has no instructions", mode, category)
			}
		}
	}
}

// TestNewDecisionAnalyzerDisabled enabled=false 时返回未配置错误。
func TestNewDecisionAnalyzerDisabled(t *testing.T) {
	analyzer := NewDecisionAnalyzer(config.AIConfig{Decision: config.DecisionConfig{Enabled: false}})
	_, err := analyzer.Evaluate(context.Background(), imageInput())
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want not configured", err)
	}
}

// TestNewDecisionAnalyzerNoEnabledChannels 有渠道但全部禁用时同样视为未配置。
func TestNewDecisionAnalyzerNoEnabledChannels(t *testing.T) {
	analyzer := NewDecisionAnalyzer(config.AIConfig{Decision: config.DecisionConfig{
		Enabled: true,
		Channels: []config.DecisionChannelConfig{{
			ID: "off", Type: config.DecisionChannelTypeGemini, Enabled: false,
			BaseURL: "https://example.com", APIKey: "k", Model: "m", Resolution: config.DecisionResolutionHigh,
		}},
	}})
	_, err := analyzer.Evaluate(context.Background(), imageInput())
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want not configured", err)
	}
}

// TestValidateDecisionInput 输入与模式必须匹配。
func TestValidateDecisionInput(t *testing.T) {
	cases := []struct {
		name string
		in   *DecisionInput
		ok   bool
	}{
		{"nil", nil, false},
		{"image without data", &DecisionInput{Mode: DecisionModeImage}, false},
		{"text empty", &DecisionInput{Mode: DecisionModeText, Text: "   "}, false},
		{"unknown mode", &DecisionInput{Mode: "video"}, false},
		{"image ok", imageInput(), true},
		{"text ok", textInput("hi"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDecisionInput(tc.in)
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected error")
			}
		})
	}
}
