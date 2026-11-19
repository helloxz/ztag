package model

import "testing"

// TestDecisionRequestShouldConvertWebP 校验 convert_webp 缺省语义：
// 不传 / null → true（保持历史行为），显式 false → false。
func TestDecisionRequestShouldConvertWebP(t *testing.T) {
	yes, no := true, false

	tests := []struct {
		name string
		req  *DecisionRequest
		want bool
	}{
		{"nil request", nil, true},
		{"absent", &DecisionRequest{}, true},
		{"explicit true", &DecisionRequest{ConvertWebP: &yes}, true},
		{"explicit false", &DecisionRequest{ConvertWebP: &no}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.req.ShouldConvertWebP(); got != tt.want {
				t.Errorf("ShouldConvertWebP() = %v, want %v", got, tt.want)
			}
		})
	}
}
