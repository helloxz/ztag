package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"net/http"
	"strings"
	"testing"

	"github.com/deepteams/webp"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// pngTestDataURI 1x1 PNG，用于 System One 的图片测试（合规图片会被原样透传）。
const pngTestDataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// pngImageInput 构造合规 PNG 输入，避免客户端做二次转换。
func pngImageInput() *DecisionInput {
	return &DecisionInput{
		Mode: DecisionModeImage,
		Image: &model.PreparedImage{
			DataURI: pngTestDataURI,
			MIME:    "image/png",
		},
	}
}

// systemOneAnswers 构造 System One 回答：指定分类给高概率，其余给最低概率。
// noul/score 与 openai 协议不同，answers 是对象映射而非数组。
func systemOneAnswers(t *testing.T, mode, category string, probability, severity float64) string {
	t.Helper()
	answers := make(map[string]any, len(decisionCategoryOrder(mode))+1)
	for _, c := range decisionCategoryOrder(mode) {
		p := 0.01
		if c == category {
			p = probability
		}
		answers[c] = map[string]any{"type": "noul", "noul": p, "confidence": 0.9}
	}
	answers[decisionSeverityKey] = map[string]any{"type": "score", "score": severity}
	body, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		t.Fatalf("marshal test answers: %v", err)
	}
	return string(body)
}

// TestSystemOneTextRequestShape 校验文本请求：路径、鉴权、state 为字符串、questions 映射。
func TestSystemOneTextRequestShape(t *testing.T) {
	var got systemOneRequest

	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions" {
			t.Errorf("path = %q, want /decisions", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprint(w, systemOneAnswers(t, DecisionModeText, "normal", 0.93, 1))
	})

	result, err := gw.Evaluate(context.Background(), textInput("今天天气不错"))
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "normal" {
		t.Errorf("category = %q, want normal", result.Category)
	}
	// rubric 0~4，severity=1 → 25
	if result.Score != 25 {
		t.Errorf("score = %d, want 25", result.Score)
	}
	if got.Model != "test-model" {
		t.Errorf("request model = %q, want test-model", got.Model)
	}
	// 文本模式的 state 是字符串
	state, ok := got.State.(string)
	if !ok || state != "今天天气不错" {
		t.Errorf("state = %#v, want text string", got.State)
	}

	// 每个分类一个 noul，外加一个 score
	if _, ok := got.Questions[decisionSeverityKey]; !ok {
		t.Fatalf("questions missing %q", decisionSeverityKey)
	}
	if q := got.Questions[decisionSeverityKey]; q.Type != decisionQuestionScore {
		t.Errorf("severity question type = %q, want score", q.Type)
	}
	for _, category := range decisionCategoryOrder(DecisionModeText) {
		q, ok := got.Questions[category]
		if !ok {
			t.Errorf("questions missing category %q", category)
			continue
		}
		if q.Type != decisionQuestionNoul {
			t.Errorf("%s question type = %q, want noul", category, q.Type)
		}
	}
}

// TestSystemOneImageRequestShape 校验图片请求：state 数组里的 image_url 块与回答解析。
func TestSystemOneImageRequestShape(t *testing.T) {
	var got systemOneRequest

	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprint(w, systemOneAnswers(t, DecisionModeImage, "porn", 0.87, 3))
	})

	result, err := gw.Evaluate(context.Background(), pngImageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "porn" {
		t.Errorf("category = %q, want porn", result.Category)
	}
	// rubric 0~4，severity=3 → 75
	if result.Score != 75 {
		t.Errorf("score = %d, want 75", result.Score)
	}

	state, ok := got.State.([]interface{})
	if !ok || len(state) != 2 {
		t.Fatalf("state = %#v, want [hint, image]", got.State)
	}
	if _, ok := state[0].(string); !ok {
		t.Errorf("state[0] = %#v, want hint string", state[0])
	}
	part, ok := state[1].(map[string]interface{})
	if !ok {
		t.Fatalf("state[1] = %#v, want image part", state[1])
	}
	if part["type"] != "image_url" {
		t.Errorf("image part type = %v, want image_url", part["type"])
	}
	imageURL, ok := part["image_url"].(map[string]interface{})
	if !ok {
		t.Fatalf("image_url = %#v, want object", part["image_url"])
	}
	if imageURL["url"] != pngTestDataURI {
		t.Errorf("image_url.url = %v, want data URI", imageURL["url"])
	}
}

// TestSystemOneProbabilityFallback 兼容用 probability 而非 noul 表达概率的实现。
func TestSystemOneProbabilityFallback(t *testing.T) {
	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		answers := make(map[string]any, len(decisionCategoryOrder(DecisionModeText))+1)
		for _, c := range decisionCategoryOrder(DecisionModeText) {
			p := 0.01
			if c == "spam" {
				p = 0.82
			}
			answers[c] = map[string]any{"type": "noul", "probability": p, "confidence": 0.7}
		}
		answers[decisionSeverityKey] = map[string]any{"type": "score", "score": 2}
		body, _ := json.Marshal(map[string]any{"answers": answers})
		fmt.Fprint(w, string(body))
	})

	result, err := gw.Evaluate(context.Background(), textInput("加微信领福利"))
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "spam" {
		t.Errorf("category = %q, want spam", result.Category)
	}
	if result.Confidence != 0.7 {
		t.Errorf("confidence = %v, want 0.7", result.Confidence)
	}
}

// TestSystemOneRefusalAnswers 回答中出现 refusal 时映射为 blocked/99。
func TestSystemOneRefusalAnswers(t *testing.T) {
	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"answers":{"porn":{"type":"refusal"}}}`)
	})

	result, err := gw.Evaluate(context.Background(), pngImageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "blocked" || result.Score != decisionBlockedScore {
		t.Errorf("result = %s/%d, want blocked/%d", result.Category, result.Score, decisionBlockedScore)
	}
	if result.BlockedCode != "refusal" {
		t.Errorf("blocked code = %q, want refusal", result.BlockedCode)
	}
}

// TestSystemOneBlockedHTTP 400 + 安全类错误码映射为 blocked。
func TestSystemOneBlockedHTTP(t *testing.T) {
	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"safety","message":"blocked by provider"}}`)
	})

	result, err := gw.Evaluate(context.Background(), pngImageInput())
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result.Category != "blocked" || result.BlockedCode != "safety" {
		t.Errorf("result = %s/%q, want blocked/safety", result.Category, result.BlockedCode)
	}
}

// TestSystemOneMissingCategory 回答缺分类时按调用失败处理。
func TestSystemOneMissingCategory(t *testing.T) {
	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"answers":{"risk_severity":{"type":"score","score":1}}}`)
	})

	if _, err := gw.Evaluate(context.Background(), textInput("hello")); err == nil {
		t.Fatal("expected error for missing category answers")
	}
}

// TestSystemOneConvertsWebP 默认预处理产出 WebP 时，客户端会先转成 JPEG 再发送。
func TestSystemOneConvertsWebP(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 800; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 255), G: uint8(y % 255), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, &webp.EncoderOptions{Quality: 80, Method: 4}); err != nil {
		t.Fatalf("encode webp: %v", err)
	}
	webpURI := "data:image/webp;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())

	var got systemOneRequest
	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprint(w, systemOneAnswers(t, DecisionModeImage, "normal", 0.9, 1))
	})

	if _, err := gw.Evaluate(context.Background(), &DecisionInput{
		Mode:  DecisionModeImage,
		Image: &model.PreparedImage{DataURI: webpURI, MIME: "image/webp"},
	}); err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}

	state, ok := got.State.([]interface{})
	if !ok || len(state) != 2 {
		t.Fatalf("state = %#v, want [hint, image]", got.State)
	}
	part, ok := state[1].(map[string]interface{})
	if !ok {
		t.Fatalf("state[1] = %#v, want image part", state[1])
	}
	imageURL, ok := part["image_url"].(map[string]interface{})
	if !ok {
		t.Fatalf("image_url = %#v, want object", part["image_url"])
	}
	url, _ := imageURL["url"].(string)

	const jpegPrefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(url, jpegPrefix) {
		t.Fatalf("image url prefix = %q, want jpeg data URI", url[:min(40, len(url))])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, jpegPrefix))
	if err != nil {
		t.Fatalf("decode jpeg base64: %v", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("output not decodable: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want jpeg", format)
	}
	if cfg.Width != 800 || cfg.Height != 600 {
		t.Errorf("size = %dx%d, want 800x600", cfg.Width, cfg.Height)
	}
}

// TestSystemOneRejectsUnsupportedImage AVIF 等无法规整的图片让该渠道失败，由网关兜底。
func TestSystemOneRejectsUnsupportedImage(t *testing.T) {
	gw := newTestGatewayWithKey(t, config.DecisionChannelTypeSystemOne, "test-key", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for unsupported image")
	})

	_, err := gw.Evaluate(context.Background(), &DecisionInput{
		Mode: DecisionModeImage,
		Image: &model.PreparedImage{
			DataURI: "data:image/avif;base64,AAAA",
			MIME:    "image/avif",
		},
	})
	if err == nil {
		t.Fatal("expected error for unsupported image format")
	}
}
