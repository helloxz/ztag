package ai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/helloxz/ztag/internal/config"
)

// TestNewAPIKeyPool 校验 Key 池解析：单个、多个、空格、空段。
func TestNewAPIKeyPool(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "sk-a", []string{"sk-a"}},
		{"multiple", "sk-a|sk-b|sk-c", []string{"sk-a", "sk-b", "sk-c"}},
		{"spaces around keys", " sk-a | sk-b ", []string{"sk-a", "sk-b"}},
		{"empty segments", "|sk-a||sk-b|", []string{"sk-a", "sk-b"}},
		{"only separators", " | | ", nil},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newAPIKeyPool(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("pool size = %d, want %d (got %v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("pool[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestAPIKeyPoolPick 校验随机选择：单 Key 固定、空池为空串、多 Key 只可能命中池内值。
func TestAPIKeyPoolPick(t *testing.T) {
	if got := newAPIKeyPool("sk-only").pick(); got != "sk-only" {
		t.Errorf("single key pick = %q, want sk-only", got)
	}
	if got := newAPIKeyPool("").pick(); got != "" {
		t.Errorf("empty pool pick = %q, want empty", got)
	}

	pool := newAPIKeyPool("sk-a|sk-b|sk-c")
	allowed := map[string]bool{"sk-a": true, "sk-b": true, "sk-c": true}
	seen := make(map[string]bool, len(pool))
	for i := 0; i < 200; i++ {
		key := pool.pick()
		if !allowed[key] {
			t.Fatalf("pick returned key outside pool: %q", key)
		}
		seen[key] = true
	}
	// 200 次全部命中同一个 Key 的概率约为 (1/3)^199，可忽略
	if len(seen) < 2 {
		t.Errorf("pick always returned the same key: %v", seen)
	}
}

// TestDecisionClientsUseKeyPool 校验两个决策客户端每次请求都从多 Key 池取值。
func TestDecisionClientsUseKeyPool(t *testing.T) {
	tests := []struct {
		name        string
		channelType string
		header      string
		bearer      bool
		answers     func(*testing.T) string
	}{
		{
			name:        "gemini",
			channelType: config.DecisionChannelTypeGemini,
			header:      "x-goog-api-key",
			answers:     func(t *testing.T) string { return geminiAnswers(t, DecisionModeText, "normal", 0.9, 0) },
		},
		{
			name:        "openai",
			channelType: config.DecisionChannelTypeOpenAI,
			header:      "Authorization",
			bearer:      true,
			answers:     func(t *testing.T) string { return openAIAnswers(t, DecisionModeText, "normal", 0.9, 0) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			seen := make(map[string]bool)

			gw := newTestGatewayWithKey(t, tt.channelType, "sk-a|sk-b", func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get(tt.header)
				if tt.bearer {
					key = strings.TrimPrefix(key, "Bearer ")
				}
				mu.Lock()
				seen[key] = true
				mu.Unlock()
				fmt.Fprint(w, tt.answers(t))
			})

			for i := 0; i < 40; i++ {
				if _, err := gw.Evaluate(context.Background(), textInput("hello")); err != nil {
					t.Fatalf("Evaluate returned error: %v", err)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("server saw no request")
			}
			for key := range seen {
				if key != "sk-a" && key != "sk-b" {
					t.Errorf("server received unexpected key: %q", key)
				}
			}
			if len(seen) < 2 {
				t.Errorf("key pool was not used for random selection, seen: %v", seen)
			}
		})
	}
}
