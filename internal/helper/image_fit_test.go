package helper

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"
	"time"

	webpanim "github.com/deepteams/webp/animation"
)

// decodeSize 读取输出图片的尺寸，校验可解码。
func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output image is not decodable: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestFitDecisionImage_Passthrough 已合规的 JPEG/PNG 原样透传，不重复压缩。
func TestFitDecisionImage_Passthrough(t *testing.T) {
	jpeg := makeJPEGBytes(800, 600)
	out, mime, err := FitDecisionImage(jpeg, "image/jpeg")
	if err != nil {
		t.Fatalf("FitDecisionImage(jpeg) error: %v", err)
	}
	if mime != "image/jpeg" || !bytes.Equal(out, jpeg) {
		t.Error("compliant jpeg should be returned unchanged")
	}

	pngData := b64(png1pxB64)
	out, mime, err = FitDecisionImage(pngData, "image/png")
	if err != nil {
		t.Fatalf("FitDecisionImage(png) error: %v", err)
	}
	if mime != "image/png" || !bytes.Equal(out, pngData) {
		t.Error("compliant png should be returned unchanged")
	}
}

// TestFitDecisionImage_Resize 超尺寸/非 JPEG 格式统一转成 JPEG 且最长边 ≤1568。
func TestFitDecisionImage_Resize(t *testing.T) {
	bigPNG := image.NewRGBA(image.Rect(0, 0, 1600, 100))
	for x := 0; x < 1600; x++ {
		bigPNG.Set(x, 50, color.RGBA{R: uint8(x % 255), G: 120, B: 200, A: 255})
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, bigPNG); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"large jpeg", makeJPEGBytes(3000, 2000), "image/jpeg"},
		{"large png", pngBuf.Bytes(), "image/png"},
		{"bmp", makeBMP1x1(), "image/bmp"},
		{"static webp", makeWebPBytes(800, 600, 90), "image/webp"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, mime, err := FitDecisionImage(c.data, c.mime)
			if err != nil {
				t.Fatalf("FitDecisionImage error: %v", err)
			}
			if mime != "image/jpeg" {
				t.Fatalf("mime = %q, want image/jpeg", mime)
			}
			w, h := decodeSize(t, out)
			if w > decisionImageMaxSide || h > decisionImageMaxSide {
				t.Errorf("output size = %dx%d, exceeds %d", w, h, decisionImageMaxSide)
			}
		})
	}
}

// TestFitDecisionImage_StaticGIF 单帧 GIF 按静态图转成 JPEG。
func TestFitDecisionImage_StaticGIF(t *testing.T) {
	out, mime, err := FitDecisionImage(b64(gif1pxB64), "image/gif")
	if err != nil {
		t.Fatalf("FitDecisionImage(gif) error: %v", err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", mime)
	}
	if w, h := decodeSize(t, out); w != 1 || h != 1 {
		t.Errorf("size = %dx%d, want 1x1", w, h)
	}
}

// makeAnimatedGIF 生成 w×h、frames 帧的动画 GIF（纯色帧，便于校验）。
func makeAnimatedGIF(t *testing.T, w, h, frames int) []byte {
	t.Helper()
	palette := color.Palette{
		color.Black,
		color.White,
		color.RGBA{R: 255, A: 255},
		color.RGBA{G: 255, A: 255},
	}
	g := &gif.GIF{
		Image:     make([]*image.Paletted, 0, frames),
		Delay:     make([]int, 0, frames),
		Disposal:  make([]byte, 0, frames),
		LoopCount: 0,
		Config:    image.Config{ColorModel: palette, Width: w, Height: h},
	}
	for i := 0; i < frames; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, w, h), palette)
		for j := range frame.Pix {
			frame.Pix[j] = uint8(i%3 + 1)
		}
		g.Image = append(g.Image, frame)
		g.Delay = append(g.Delay, 10)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode animated gif: %v", err)
	}
	return buf.Bytes()
}

// TestFitDecisionImage_AnimatedGIF 动画 GIF 抽帧拼成一张 JPEG 网格图。
func TestFitDecisionImage_AnimatedGIF(t *testing.T) {
	tests := []struct {
		name   string
		frames int
		wantW  int
		wantH  int
	}{
		{"two frames", 2, decisionImageMaxSide, decisionImageMaxSide / 2},
		{"three frames", 3, decisionImageMaxSide, decisionImageMaxSide},
		{"five frames sampled", 5, decisionImageMaxSide, decisionImageMaxSide},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, mime, err := FitDecisionImage(makeAnimatedGIF(t, 320, 240, tc.frames), "image/gif")
			if err != nil {
				t.Fatalf("FitDecisionImage error: %v", err)
			}
			if mime != "image/jpeg" {
				t.Errorf("mime = %q, want image/jpeg", mime)
			}
			w, h := decodeSize(t, out)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("size = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

// TestFitDecisionImage_AnimatedWebpRejected 动画 WebP 暂不支持，返回明确错误。
func TestFitDecisionImage_AnimatedWebpRejected(t *testing.T) {
	frame1 := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	frame2 := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			frame1.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
			frame2.SetNRGBA(x, y, color.NRGBA{B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	enc := webpanim.NewEncoder(&buf, 64, 64, &webpanim.EncodeOptions{Quality: 80})
	if err := enc.AddFrame(frame1, 100*time.Millisecond); err != nil {
		t.Fatalf("add frame: %v", err)
	}
	if err := enc.AddFrame(frame2, 100*time.Millisecond); err != nil {
		t.Fatalf("add frame: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("encode animated webp: %v", err)
	}

	_, _, err := FitDecisionImage(buf.Bytes(), "image/webp")
	if err == nil || !strings.Contains(err.Error(), "animated webp") {
		t.Fatalf("err = %v, want animated webp error", err)
	}
}

// TestFitDecisionImage_UnsupportedMIME AVIF 等无解码器的格式直接报错。
func TestFitDecisionImage_UnsupportedMIME(t *testing.T) {
	if _, _, err := FitDecisionImage([]byte("not-an-image"), "image/avif"); err == nil {
		t.Fatal("expected error for unsupported mime")
	}
}

// TestFitDecisionImage_TooManyFrames 帧数超过上限时拒绝，避免长时间占用 CPU。
func TestFitDecisionImage_TooManyFrames(t *testing.T) {
	data := makeAnimatedGIF(t, 1, 1, maxGIFDecodeFrames+1)
	if _, _, err := FitDecisionImage(data, "image/gif"); err == nil {
		t.Fatal("expected error for too many gif frames")
	}
}

// TestSampleGIFIndices 均匀采样：包含首尾帧，数量不超过上限。
func TestSampleGIFIndices(t *testing.T) {
	tests := []struct {
		total, limit int
		want         []int
	}{
		{3, 4, []int{0, 1, 2}},
		{4, 4, []int{0, 1, 2, 3}},
		{10, 4, []int{0, 3, 6, 9}},
		{5, 4, []int{0, 1, 3, 4}},
	}

	for _, tc := range tests {
		got := sampleGIFIndices(tc.total, tc.limit)
		if len(got) != len(tc.want) {
			t.Fatalf("total=%d limit=%d: got %v, want %v", tc.total, tc.limit, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("total=%d limit=%d: got %v, want %v", tc.total, tc.limit, got, tc.want)
				break
			}
		}
	}
}
