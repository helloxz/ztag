package config

import "testing"

// TestValidateDecisionAPIKeys 校验决策渠道的 api_key：全空段拒绝，多 Key 允许。
func TestValidateDecisionAPIKeys(t *testing.T) {
	newConfig := func(apiKey string) *Config {
		return &Config{
			Server: ServerConfig{Addr: ":8080", Mode: "debug"},
			AI: AIConfig{
				Channels: []ChannelConfig{{
					ID: "default", Type: ChannelTypeOpenAIChat,
					BaseURL: "https://example.com", APIKey: "key", Models: []string{"model"},
				}},
				Decision: DecisionConfig{
					Enabled: true, Timeout: 30, MaxTextChars: 4000,
					Channels: []DecisionChannelConfig{{
						ID: "gemini-main", Type: DecisionChannelTypeGemini, Enabled: true,
						BaseURL: "https://example.com", APIKey: apiKey,
						Model: "model", Resolution: DecisionResolutionHigh,
					}},
				},
			},
		}
	}

	if err := newConfig("key1|key2").Validate(); err != nil {
		t.Errorf("multiple keys rejected: %v", err)
	}
	if err := newConfig(" | ").Validate(); err == nil {
		t.Error("api_key with only separators passed validation, want error")
	}
}

// TestSplitAPIKeys 校验 api_key 的多 Key 拆分规则（"|" 分隔、忽略空白、丢弃空段）。
func TestSplitAPIKeys(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "sk-a", []string{"sk-a"}},
		{"multiple", "sk-a|sk-b", []string{"sk-a", "sk-b"}},
		{"spaces", " sk-a | sk-b ", []string{"sk-a", "sk-b"}},
		{"empty segments", "|sk-a||sk-b|", []string{"sk-a", "sk-b"}},
		{"only separators", " | | ", nil},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitAPIKeys(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
