package helper

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"github.com/deepteams/webp" // 显式导入：init 注册 webp 解码器 + 测试生成 webp 样本
	webpanim "github.com/deepteams/webp/animation"
)

// 最小合法图片字节（base64 常量）
const (
	jpeg1pxB64 = "/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/wAALCAABAAEBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AVN//2Q=="
	png1pxB64  = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	gif1pxB64  = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"
)

// makeBMP1x1 手写构造 1x1 24bit BMP（BMP 文件头 14 + BITMAPINFOHEADER 40 + 4 字节像素行）。
func makeBMP1x1() []byte {
	b := make([]byte, 14+40+4)
	b[0], b[1] = 'B', 'M'
	size := uint32(len(b))
	b[2], b[3], b[4], b[5] = byte(size), byte(size>>8), byte(size>>16), byte(size>>24)
	b[10] = byte(14 + 40)           // 像素数据偏移
	b[14] = 40                      // BITMAPINFOHEADER 大小
	b[18], b[22] = 1, 1             // 宽、高
	b[26] = 1                       // planes
	b[28] = 24                      // 每像素位数
	b[54], b[55], b[56] = 0, 0, 255 // BGR 像素（红色），第 4 字节为行对齐填充
	return b
}

func b64(b string) []byte { data, _ := base64.StdEncoding.DecodeString(b); return data }

// TestCompressImageForAI_Convertible 可转换格式（jpeg/png/bmp）应重编码且输出可解码。
func TestCompressImageForAI_Convertible(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"jpeg", b64(jpeg1pxB64), "image/jpeg"},
		{"png", b64(png1pxB64), "image/png"},
		{"bmp", makeBMP1x1(), "image/bmp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, mime, err := CompressImageForAI(c.data, c.mime)
			if err != nil {
				t.Fatalf("CompressImageForAI(%s) 出错: %v", c.name, err)
			}
			// 输出统一为 WebP（有损 q80），保持 1x1 尺寸
			if mime != "image/webp" {
				t.Errorf("目标 MIME = %q，期望 image/webp", mime)
			}
			if len(out) == 0 {
				t.Error("输出为空")
			}
			// 输出应能正常解码，尺寸不变（1x1）
			cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("输出不可解码: %v", err)
			}
			if format == "" {
				t.Error("无法识别输出格式")
			}
			if cfg.Width != 1 || cfg.Height != 1 {
				t.Errorf("输出尺寸 = %dx%d，期望 1x1", cfg.Width, cfg.Height)
			}
		})
	}
}

// makeJPEGBytes 生成指定尺寸的 JPEG 字节（测试用，色块填充保证可压缩）。
func makeJPEGBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y += 8 {
		for x := 0; x < w; x += 8 {
			c := color.RGBA{uint8(x % 255), uint8(y % 255), uint8((x + y) % 255), 255}
			for dy := 0; dy < 8 && y+dy < h; dy++ {
				for dx := 0; dx < 8 && x+dx < w; dx++ {
					img.SetRGBA(x+dx, y+dy, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestCompressImageForAI_DimensionLimit 解码尺寸限制：
// 最长边超过 maxDecodeDimension 的大图等比缩放，小图/边界图不缩放。
func TestCompressImageForAI_DimensionLimit(t *testing.T) {
	cases := []struct {
		name         string
		w, h         int
		wantW, wantH int // 期望输出尺寸
	}{
		{"large-landscape", 3000, 2000, maxDecodeDimension, 1365}, // round(2000*2048/3000)
		{"large-portrait", 2000, 3000, 1365, maxDecodeDimension},  // round(2000*2048/3000)
		{"small-no-resize", 800, 600, 800, 600},                   // 小图不缩放
		{"boundary-no-resize", 2048, 1024, 2048, 1024},            // 恰好等于上限：不缩放
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := makeJPEGBytes(c.w, c.h)
			out, mime, err := CompressImageForAI(data, "image/jpeg")
			if err != nil {
				t.Fatalf("CompressImageForAI 出错: %v", err)
			}
			if mime != "image/webp" {
				t.Fatalf("目标 MIME = %q，期望 image/webp", mime)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("读取输出尺寸失败: %v", err)
			}
			if cfg.Width != c.wantW || cfg.Height != c.wantH {
				t.Errorf("输出尺寸 = %dx%d，期望 %dx%d", cfg.Width, cfg.Height, c.wantW, c.wantH)
			}
		})
	}
}

// TestCompressImageForAI_TransparentPNG 含透明通道的 PNG 应输出 WebP 且 alpha 保留
// （WebP 的 ALPH chunk 承载透明信息，无需像 JPEG 那样丢失 alpha）。
func TestCompressImageForAI_TransparentPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 128}) // 半透明红色
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成 PNG 失败: %v", err)
	}
	out, mime, err := CompressImageForAI(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("CompressImageForAI 出错: %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("目标 MIME = %q，期望 image/webp", mime)
	}
	// 输出应能解码且 alpha 保留（半透明 128/255 → 16 位色域 = 128*257 = 32896）
	decoded, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出不可解码: %v", err)
	}
	_, _, _, a := decoded.At(10, 10).RGBA()
	if a < 30000 || a > 35000 { // 允许有损压缩的轻微偏差，但必须显著低于全不透明 65535
		t.Errorf("alpha 未保留: 解码 alpha=%d（半透明 128/255 期望约 32896）", a)
	}
}

// makeWebPBytes 用 webp.Encode 生成指定尺寸的静态 WebP 字节（测试用）。
// 用渐变+噪点模拟照片数据（避免纯色块图被压到极小导致重编码无收益、误触发透传回退）。
func makeWebPBytes(w, h int, quality float32) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	pix := img.Pix
	i := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pix[i] = uint8(x*255/w + (y*37)%13)           // R
			pix[i+1] = uint8(y*255/h + (x*53)%17)         // G
			pix[i+2] = uint8((x+y)*255/(w+h) + (y*71)%11) // B
			pix[i+3] = 255
			i += 4
		}
	}
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, &webp.EncoderOptions{Quality: quality, Method: 4}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestCompressImageForAI_StaticWebp 静态 webp 输入：
//   - 超过 2048 的静态 webp 等比缩放后重编码为 webp；
//   - 未超尺寸的静态 webp 原样透传（不重编码）。
func TestCompressImageForAI_StaticWebp(t *testing.T) {
	// 大静态 webp：3000x2000 → 缩放 2048x1365 + 重编码
	large := makeWebPBytes(3000, 2000, 90)
	if len(large) == 0 {
		t.Fatal("生成的 webp 样本为空")
	}
	out, mime, err := CompressImageForAI(large, "image/webp")
	if err != nil {
		t.Fatalf("大静态 webp 处理出错: %v", err)
	}
	if mime != "image/webp" {
		t.Fatalf("目标 MIME = %q，期望 image/webp", mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出不可解码: %v", err)
	}
	if cfg.Width != maxDecodeDimension || cfg.Height != 1365 {
		t.Errorf("输出尺寸 = %dx%d，期望 %dx1365", cfg.Width, cfg.Height, maxDecodeDimension)
	}

	// 小静态 webp：800x600 未超尺寸 → 原样透传
	small := makeWebPBytes(800, 600, 95)
	out2, mime2, err := CompressImageForAI(small, "image/webp")
	if err != nil {
		t.Fatalf("小静态 webp 处理出错: %v", err)
	}
	if mime2 != "image/webp" {
		t.Fatalf("目标 MIME = %q，期望 image/webp", mime2)
	}
	if !bytes.Equal(out2, small) {
		t.Error("未超尺寸的静态 webp 应原样透传（不发生重编码）")
	}
}

// TestCompressImageForAI_AnimatedWebp 动画 webp 应原样透传（不做缩放/重编码）。
func TestCompressImageForAI_AnimatedWebp(t *testing.T) {
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
		t.Fatalf("添加帧失败: %v", err)
	}
	if err := enc.AddFrame(frame2, 100*time.Millisecond); err != nil {
		t.Fatalf("添加帧失败: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("生成动画 webp 失败: %v", err)
	}
	ani := buf.Bytes()
	// 确认样本确实是动画（防测试自证失效）
	feat, err := webp.GetFeatures(bytes.NewReader(ani))
	if err != nil || !feat.HasAnimation {
		t.Fatalf("样本不是动画 webp（feat=%+v err=%v），测试无效", feat, err)
	}
	out, mime, err := CompressImageForAI(ani, "image/webp")
	if err != nil {
		t.Fatalf("动画 webp 处理出错: %v", err)
	}
	if mime != "image/webp" {
		t.Fatalf("目标 MIME = %q，期望 image/webp", mime)
	}
	if !bytes.Equal(out, ani) {
		t.Error("动画 webp 应原样透传（不做缩放/重编码）")
	}
}

// TestCompressImageForAI_TooManyPixels 总像素超过 maxDecodePixels 的图应拒绝解码（防解压炸弹）。
func TestCompressImageForAI_TooManyPixels(t *testing.T) {
	// 单像素宽的"细长"图：宽高乘积大但字节量小
	img := image.NewRGBA(image.Rect(0, 0, 1, maxDecodePixels+1))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成 PNG 失败: %v", err)
	}
	if _, _, err := CompressImageForAI(buf.Bytes(), "image/png"); err == nil {
		t.Fatal("超像素图应返回错误（拒绝解码）")
	}
}

// TestCompressImageForAI_Passthrough 非可转换格式（gif 等）应原样透传。
func TestCompressImageForAI_Passthrough(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"gif", b64(gif1pxB64), "image/gif"},
		{"avif", []byte{0x00, 0x00, 0x00}, "image/avif"},
		{"unknown", []byte("data"), "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, mime, err := CompressImageForAI(c.data, c.mime)
			if err != nil {
				t.Fatalf("CompressImageForAI(%s) 出错: %v", c.name, err)
			}
			if mime != c.mime {
				t.Errorf("目标 MIME = %q，期望原样 %q", mime, c.mime)
			}
			if !bytes.Equal(out, c.data) {
				t.Error("非可转换格式不应被修改（应原字节透传）")
			}
		})
	}
}

// TestEncodeDataURI 编码格式验证：data:<mime>;base64,<data> 且可解码还原。
func TestEncodeDataURI(t *testing.T) {
	raw := []byte("hello image")
	uri := EncodeDataURI(raw, "image/webp")
	want := "data:image/webp;base64,aGVsbG8gaW1hZ2U="
	if uri != want {
		t.Errorf("EncodeDataURI = %q，期望 %q", uri, want)
	}
}
