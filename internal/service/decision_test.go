package service

import (
	"context"
	"strings"
	"testing"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/model"
)

// stubDecisionAnalyzer 记录入参，避免真实模型调用。
type stubDecisionAnalyzer struct {
	got    *ai.DecisionInput
	result *model.DecisionResult
	err    error
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
		Text:  "  加微信领福利  ",
		Model: "openai-main",
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
	if stub.got.Model != "openai-main" {
		t.Errorf("model = %q, want openai-main", stub.got.Model)
	}
}

// TestNewDecisionServiceDefaultMaxTextChars 上限非法时回退 4000。
func TestNewDecisionServiceDefaultMaxTextChars(t *testing.T) {
	svc := NewDecisionService(nil, &stubDecisionAnalyzer{}, 0)
	if svc.maxTextChars != 4000 {
		t.Errorf("maxTextChars = %d, want 4000", svc.maxTextChars)
	}
}
