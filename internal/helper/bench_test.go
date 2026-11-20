package helper

// 图片处理基准：测量缩放 / 转 WebP / 转 JPEG 三条链路的耗时与内存分配。
//
//	go test ./internal/helper -run '^$' -bench . -benchmem
//
// 注意：libvips 的像素缓冲在 native 堆，不会出现在 B/op 里；
// 进程级峰值内存要用 RSS 单独测（见 scripts/bench-images.sh）。
import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// benchPhoto 生成带噪声的"照片类"JPEG（梯度打底 + 满幅噪声，熵接近真实照片，
// 避免生成过于平滑、压缩后只有几十 KB 的失真样本）。
func benchPhoto(w, h int, quality int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(0x9E3779B9)
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride:]
		gy := uint8(y * 255 / max(h-1, 1))
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			n1 := uint8(seed >> 24)
			seed = seed*1664525 + 1013904223
			n2 := uint8(seed >> 24)
			seed = seed*1664525 + 1013904223
			n3 := uint8(seed >> 24)
			gx := uint8(x * 255 / max(w-1, 1))
			row[x*4+0] = uint8((int(gx) + int(n1)) / 2)
			row[x*4+1] = uint8((int(gy) + int(n2)) / 2)
			row[x*4+2] = uint8((int(gx)+int(gy))/2+int(n3)) / 2
			row[x*4+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// benchScreenshot 生成"截图类"PNG（白底 + 色块 + 细线，高压缩率）。
func benchScreenshot(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	fill := func(x0, y0, x1, y1 int, c color.NRGBA) {
		for y := y0; y < y1 && y < h; y++ {
			for x := x0; x < x1 && x < w; x++ {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	fill(40, 40, w-40, 120, color.NRGBA{R: 32, G: 33, B: 36, A: 255})   // 顶栏
	fill(80, 200, 700, 260, color.NRGBA{R: 60, G: 120, B: 220, A: 255}) // 标题块
	for y := 320; y < h-80; y += 28 {
		fill(80, y, w-160, y+8, color.NRGBA{R: 120, G: 120, B: 120, A: 255}) // 文本行
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// benchAlphaPNG 生成带透明通道的 PNG（验证 alpha 处理路径）。
func benchAlphaPNG(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x % 256), G: uint8(y % 256), B: 128,
				A: uint8(255 - (x+y)%256/2),
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

var (
	benchJPEG2MP  = benchPhoto(1600, 1200, 90) // 1.9MP，典型手机缩略图
	benchJPEG6MP  = benchPhoto(3000, 2000, 90) // 6MP，典型手机照片
	benchJPEG24MP = benchPhoto(6000, 4000, 90) // 24MP，相机原图
	benchPNGShot  = benchScreenshot(2560, 1440)
	benchPNGAlpha = benchAlphaPNG(2000, 1500)
)

// TestGenerateBenchImages 按需把基准图集写到 ZTAG_BENCH_IMAGES 指定的目录，
// 供 scripts/bench-images.sh 做二进制的端到端对比（默认跳过，不污染常规测试）。
func TestGenerateBenchImages(t *testing.T) {
	dir := os.Getenv("ZTAG_BENCH_IMAGES")
	if dir == "" {
		t.Skip("set ZTAG_BENCH_IMAGES to dump benchmark images")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	files := map[string][]byte{
		"photo-2mp.jpg":       benchJPEG2MP,
		"photo-6mp.jpg":       benchJPEG6MP,
		"photo-24mp.jpg":      benchJPEG24MP,
		"screenshot.png":      benchPNGShot,
		"alpha-2000x1500.png": benchPNGAlpha,
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(data))
	}
}

// BenchmarkCompressImageForAI 图片分析链路：解码 → 缩放（如需）→ WebP 编码。
func BenchmarkCompressImageForAI(b *testing.B) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"jpeg-2MP", benchJPEG2MP, "image/jpeg"},
		{"jpeg-6MP", benchJPEG6MP, "image/jpeg"},
		{"jpeg-24MP", benchJPEG24MP, "image/jpeg"},
		{"png-screenshot", benchPNGShot, "image/png"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.data)))
			var out []byte
			var mime string
			var err error
			for i := 0; i < b.N; i++ {
				out, mime, err = CompressImageForAI(c.data, c.mime)
			}
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("impl=%s in=%dB out=%dB mime=%s", ImagePipelineName(), len(c.data), len(out), mime)
		})
	}
}

// BenchmarkFitDecisionImage 内容审核链路：规整为 JPEG（每边 ≤1568，白底）。
func BenchmarkFitDecisionImage(b *testing.B) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"jpeg-2MP-resize", benchJPEG2MP, "image/jpeg"},
		{"jpeg-6MP-resize", benchJPEG6MP, "image/jpeg"},
		{"jpeg-800x600-passthrough", benchPhoto(800, 600, 90), "image/jpeg"},
		{"png-alpha-transcode", benchPNGAlpha, "image/png"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.data)))
			var out []byte
			var mime string
			var err error
			for i := 0; i < b.N; i++ {
				out, mime, err = FitDecisionImage(c.data, c.mime)
			}
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("impl=%s in=%dB out=%dB mime=%s", ImagePipelineName(), len(c.data), len(out), mime)
		})
	}
}
