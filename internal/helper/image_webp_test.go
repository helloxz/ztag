package helper

import (
	"bytes"
	"encoding/base64"
	"testing"
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

// TestCompressImageForAI_Convertible 可转换格式（jpeg/png/bmp）应转为 webp 且体积合理。
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
			if mime != "image/webp" {
				t.Errorf("目标 MIME = %q，期望 image/webp", mime)
			}
			if len(out) == 0 {
				t.Error("输出为空")
			}
			// webp 文件头应包含 RIFF/WEBP 标记
			if !bytes.Contains(out, []byte("WEBP")) {
				t.Errorf("输出缺少 WEBP 标记，前几个字节: %x", out[:min(12, len(out))])
			}
		})
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
