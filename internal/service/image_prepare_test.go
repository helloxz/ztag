package service

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// png1pxB64 最小 1x1 PNG（base64），用于测试预处理的各条路径。
const png1pxB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestImagePreparerTargets 校验三种预处理目标：
//   - WebP：默认压缩转 WebP；
//   - Original：保持原始 PNG 字节与 MIME；
//   - Decision：1x1 PNG 尺寸合规，原样透传（PNG）。
func TestImagePreparerTargets(t *testing.T) {
	if _, err := base64.StdEncoding.DecodeString(png1pxB64); err != nil {
		t.Fatalf("decode png fixture: %v", err)
	}

	preparer := NewImagePreparer(config.AIConfig{
		ImageTimeout:  5,
		MaxImageBytes: 1 << 20,
	}, 1)

	tests := []struct {
		name        string
		target      ImageTarget
		wantMIME    string
		wantDataURI string
	}{
		{
			name:     "webp by default",
			target:   ImageTargetWebP,
			wantMIME: "image/webp",
		},
		{
			name:        "keep original format",
			target:      ImageTargetOriginal,
			wantMIME:    "image/png",
			wantDataURI: "data:image/png;base64," + png1pxB64,
		},
		{
			name:        "decision target keeps compliant png",
			target:      ImageTargetDecision,
			wantMIME:    "image/png",
			wantDataURI: "data:image/png;base64," + png1pxB64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := preparer.Prepare(context.Background(),
				&model.ImageInput{ImageBase64: png1pxB64}, tt.target)
			if err != nil {
				t.Fatalf("Prepare returned error: %v", err)
			}
			if prepared.MIME != tt.wantMIME {
				t.Errorf("MIME = %q, want %q", prepared.MIME, tt.wantMIME)
			}
			if tt.wantDataURI != "" {
				if prepared.DataURI != tt.wantDataURI {
					t.Errorf("DataURI = %q, want %q", prepared.DataURI, tt.wantDataURI)
				}
				return
			}
			if !strings.HasPrefix(prepared.DataURI, "data:image/webp;base64,") {
				t.Errorf("DataURI prefix = %q, want image/webp data URI", prepared.DataURI[:min(40, len(prepared.DataURI))])
			}
		})
	}
}

// TestImageInputValidation 图片输入为空时仍按原逻辑报错。
func TestImageInputValidation(t *testing.T) {
	preparer := NewImagePreparer(config.AIConfig{ImageTimeout: 5, MaxImageBytes: 1 << 20}, 1)
	for _, target := range []ImageTarget{ImageTargetWebP, ImageTargetOriginal, ImageTargetDecision} {
		if _, err := preparer.Prepare(context.Background(), &model.ImageInput{}, target); err == nil {
			t.Errorf("target=%v: expected error for empty input", target)
		}
	}
}
