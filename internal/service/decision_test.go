package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// stubDecisionAnalyzer 记录入参，避免真实模型调用。
type stubDecisionAnalyzer struct {
	got             *ai.DecisionInput
	result          *model.DecisionResult
	err             error
	target          ai.DecisionChannelTarget // Resolve 返回的渠道与格式（ID 为空时默认 mock/WebP）
	resErr          error
	resolve         int    // Resolve 调用次数
	resolvedChannel string // Resolve 收到的原始 channel 参数
}

func (s *stubDecisionAnalyzer) Resolve(channel string) (ai.DecisionChannelTarget, error) {
	s.resolve++
	s.resolvedChannel = channel
	if s.resErr != nil {
		return ai.DecisionChannelTarget{}, s.resErr
	}
	t := s.target
	if t.ID == "" {
		t.ID = "mock"
	}
	if channel != "" {
		// 模拟真实网关：指定渠道 id 时精确匹配，格式仍取该渠道配置
		t.ID = channel
	}
	return t, nil
}

func (s *stubDecisionAnalyzer) Evaluate(ctx context.Context, in *ai.DecisionInput) (*model.DecisionResult, error) {
	s.got = in
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// TestEvaluateTextValidation 空文本与超长文本直接拒绝，且不调用模型。
func TestEvaluateTextValidation(t *testing.T) {
	stub := &stubDecisionAnalyzer{result: &model.DecisionResult{Category: "normal"}}
	svc := NewDecisionService(nil, stub, 10)

	cases := []struct {
		name string
		text string
		want string
	}{
		{"empty", "   ", "text must not be empty"},
		{"too long", strings.Repeat("字", 11), "text is too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.EvaluateText(context.Background(), &model.TextDecisionRequest{Text: tc.text})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if stub.got != nil {
				t.Error("analyzer should not be called for invalid input")
			}
		})
	}
}

// TestEvaluateTextPassesTrimmedText 文本去空白后透传，并带上渠道 id。
func TestEvaluateTextPassesTrimmedText(t *testing.T) {
	stub := &stubDecisionAnalyzer{result: &model.DecisionResult{Category: "spam"}}
	svc := NewDecisionService(nil, stub, 10)

	result, err := svc.EvaluateText(context.Background(), &model.TextDecisionRequest{
		Text:    "  加微信领福利  ",
		Channel: "openai-main",
	})
	if err != nil {
		t.Fatalf("EvaluateText returned error: %v", err)
	}
	if result.Category != "spam" {
		t.Errorf("category = %q, want spam", result.Category)
	}
	if stub.got == nil {
		t.Fatal("analyzer not called")
	}
	if stub.got.Mode != ai.DecisionModeText {
		t.Errorf("mode = %q, want text", stub.got.Mode)
	}
	if stub.got.Text != "加微信领福利" {
		t.Errorf("text = %q, want trimmed text", stub.got.Text)
	}
	if stub.got.Channel != "openai-main" {
		t.Errorf("channel = %q, want openai-main", stub.got.Channel)
	}
}

// TestEvaluateImagePassesChannel 图片审核把 channel 透传给决策网关。
func TestEvaluateImagePassesChannel(t *testing.T) {
	preparer := NewImagePreparer(config.AIConfig{ImageTimeout: 5, MaxImageBytes: 10 << 20}, 1)
	stub := &stubDecisionAnalyzer{result: &model.DecisionResult{Category: "normal"}}
	svc := NewDecisionService(preparer, stub, 10)

	if _, err := svc.Evaluate(context.Background(), &model.DecisionRequest{
		ImageInput: model.ImageInput{ImageBase64: makeOversizePNG(t)},
		Channel:    "gemini-main",
	}); err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if stub.got == nil {
		t.Fatal("analyzer not called")
	}
	if stub.got.Channel != "gemini-main" {
		t.Errorf("channel = %q, want gemini-main", stub.got.Channel)
	}
}

// TestRejectLegacyModelParam 旧参数 model 必须显式报错：
// JSON 绑定会静默忽略未知字段，若不拦截，老调用方会悄悄退化成「按配置轮询」。
func TestRejectLegacyModelParam(t *testing.T) {
	stub := &stubDecisionAnalyzer{result: &model.DecisionResult{Category: "normal"}}
	svc := NewDecisionService(nil, stub, 10)

	cases := []struct {
		name string
		call func() error
	}{
		{"text", func() error {
			_, err := svc.EvaluateText(context.Background(), &model.TextDecisionRequest{
				Text:        "hi",
				LegacyModel: "openai-main",
			})
			return err
		}},
		{"image", func() error {
			// 校验先于图片预处理：即使没提供图片，也应先报改名错误
			_, err := svc.Evaluate(context.Background(), &model.DecisionRequest{
				LegacyModel: "gemini-main",
			})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), `parameter "model" was renamed to "channel"`) {
				t.Fatalf("err = %v, want rename hint", err)
			}
			if stub.got != nil {
				t.Error("analyzer should not be called with legacy parameter")
			}
		})
	}
}

// TestNewDecisionServiceDefaultMaxTextChars 上限非法时回退 4000。
func TestNewDecisionServiceDefaultMaxTextChars(t *testing.T) {
	svc := NewDecisionService(nil, &stubDecisionAnalyzer{}, 0)
	if svc.maxTextChars != 4000 {
		t.Errorf("maxTextChars = %d, want 4000", svc.maxTextChars)
	}
}

// makeOversizePNG 生成 1600×100 的 PNG（超过 1568 上限），用于验证决策图片规整。
func makeOversizePNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1600, 100))
	for x := 0; x < 1600; x++ {
		img.Set(x, 50, color.RGBA{R: uint8(x % 255), G: 120, B: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestEvaluateImageTargetPolicy 校验图片预处理目标：
//   - 渠道要求 JPEG（systemone）→ 直接规整为 JPEG ≤1568，不再走 WebP 再转码；
//   - 通用渠道（默认）→ 压缩转 WebP；
//   - convert_webp=false → 无论什么渠道都规整为 JPEG（兼容旧行为）。
func TestEvaluateImageTargetPolicy(t *testing.T) {
	imageB64 := makeOversizePNG(t)
	no := false

	newService := func(target ai.DecisionChannelTarget) (*DecisionService, *stubDecisionAnalyzer) {
		preparer := NewImagePreparer(config.AIConfig{ImageTimeout: 5, MaxImageBytes: 10 << 20}, 1)
		stub := &stubDecisionAnalyzer{
			result: &model.DecisionResult{Category: "normal"},
			target: target,
		}
		return NewDecisionService(preparer, stub, 10), stub
	}

	t.Run("convert_webp=false fits for all channels", func(t *testing.T) {
		svc, stub := newService(ai.DecisionChannelTarget{})
		if _, err := svc.Evaluate(context.Background(), &model.DecisionRequest{
			ImageInput:  model.ImageInput{ImageBase64: imageB64},
			ConvertWebP: &no,
		}); err != nil {
			t.Fatalf("Evaluate returned error: %v", err)
		}
		if stub.got == nil || stub.got.Image == nil {
			t.Fatal("analyzer did not receive image input")
		}
		if stub.got.Image.MIME != "image/jpeg" {
			t.Errorf("MIME = %q, want image/jpeg", stub.got.Image.MIME)
		}
		if !strings.HasPrefix(stub.got.Image.DataURI, "data:image/jpeg;base64,") {
			t.Errorf("data URI prefix = %q, want image/jpeg", stub.got.Image.DataURI[:min(40, len(stub.got.Image.DataURI))])
		}
	})

	t.Run("generic channel keeps webp", func(t *testing.T) {
		svc, stub := newService(ai.DecisionChannelTarget{ID: "gemini-main", Format: ai.FormatWebP})
		if _, err := svc.Evaluate(context.Background(), &model.DecisionRequest{
			ImageInput: model.ImageInput{ImageBase64: imageB64},
		}); err != nil {
			t.Fatalf("Evaluate returned error: %v", err)
		}
		if stub.got == nil || stub.got.Image == nil {
			t.Fatal("analyzer did not receive image input")
		}
		if stub.got.Image.MIME != "image/webp" {
			t.Errorf("MIME = %q, want image/webp", stub.got.Image.MIME)
		}
	})

	// 核心回归：System One 渠道直接拿到 JPEG，不再"先 WebP 再解码转 JPEG"
	t.Run("jpeg channel avoids double encode", func(t *testing.T) {
		svc, stub := newService(ai.DecisionChannelTarget{ID: "jev-main", Format: ai.FormatJPEG})
		if _, err := svc.Evaluate(context.Background(), &model.DecisionRequest{
			ImageInput: model.ImageInput{ImageBase64: imageB64},
		}); err != nil {
			t.Fatalf("Evaluate returned error: %v", err)
		}
		if stub.got == nil || stub.got.Image == nil {
			t.Fatal("analyzer did not receive image input")
		}
		if stub.got.Image.MIME != "image/jpeg" {
			t.Errorf("MIME = %q, want image/jpeg", stub.got.Image.MIME)
		}
		if stub.got.Channel != "jev-main" {
			t.Errorf("channel = %q, want jev-main（必须回传 Resolve 得到的渠道，避免轮询推进两次）", stub.got.Channel)
		}
	})
}

// TestEvaluateResolveErrorAbortsBeforePrepare 渠道解析失败时不应下载/处理图片。
func TestEvaluateResolveErrorAbortsBeforePrepare(t *testing.T) {
	preparer := NewImagePreparer(config.AIConfig{ImageTimeout: 5, MaxImageBytes: 10 << 20}, 1)
	stub := &stubDecisionAnalyzer{resErr: model.NewBizError("decision model is not configured")}
	svc := NewDecisionService(preparer, stub, 10)

	_, err := svc.Evaluate(context.Background(), &model.DecisionRequest{
		ImageInput: model.ImageInput{ImageBase64: makeOversizePNG(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want decision model is not configured", err)
	}
	if stub.got != nil {
		t.Error("analyzer should not be called when resolve fails")
	}
	if stub.resolve != 1 {
		t.Errorf("resolve calls = %d, want 1", stub.resolve)
	}
}
