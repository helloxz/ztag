package config

import "testing"

// testDecisionConfig 构造一份可通过校验的最小配置（仅替换决策渠道 key / type）。
func testDecisionConfig(apiKey, channelType string) *Config {
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
					ID: "gemini-main", Type: channelType, Enabled: true,
					BaseURL: "https://example.com", APIKey: apiKey,
					Model: "model", Resolution: DecisionResolutionHigh,
				}},
			},
		},
	}
}

// TestValidateDecisionAPIKeys 校验决策渠道的 api_key：全空段拒绝，多 Key 允许。
func TestValidateDecisionAPIKeys(t *testing.T) {
	if err := testDecisionConfig("key1|key2", DecisionChannelTypeGemini).Validate(); err != nil {
		t.Errorf("multiple keys rejected: %v", err)
	}
	if err := testDecisionConfig(" | ", DecisionChannelTypeGemini).Validate(); err == nil {
		t.Error("api_key with only separators passed validation, want error")
	}
}

// TestValidateDecisionChannelTypes 三种协议类型都应通过校验。
func TestValidateDecisionChannelTypes(t *testing.T) {
	for _, channelType := range []string{
		DecisionChannelTypeGemini,
		DecisionChannelTypeOpenAI,
		DecisionChannelTypeSystemOne,
	} {
		t.Run(channelType, func(t *testing.T) {
			if err := testDecisionConfig("key", channelType).Validate(); err != nil {
				t.Errorf("channel type %q rejected: %v", channelType, err)
			}
		})
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
