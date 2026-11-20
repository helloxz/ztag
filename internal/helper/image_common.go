// Package helper 图片本地处理助手：送 AI 前的压缩转换与编码。
//
// 图片管线使用 libvips（govips cgo 绑定）：解码走 shrink-on-load，
// 缩放/转 WebP/转 JPEG 在同一管线内流式完成，避免全尺寸像素拷贝。
// 构建需要 CGO_ENABLED=1 与系统 libvips；GIF 抽帧拼图仍用纯 Go 实现。
package helper

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"

	_ "image/gif" // 注册 GIF 解码器（图片格式识别与 GIF 处理用）
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp" // 注册 BMP 解码器（标准库 image 不含）
)

// webpEncodeMIME 转 WebP 编码白名单：仅这些格式解码后重编码为 WebP（有损 q80）。
var webpEncodeMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/bmp":  true,
}

// resizableMIME 可缩放白名单 = webpEncodeMIME ∪ 静态 webp。
// 其余格式（gif、avif、动画 webp 等）原样透传，不做缩放/转码。
var resizableMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/bmp":  true,
	"image/webp": true, // 仅静态 webp（动画在 CompressImageForAI 内透传）
}

const (
	// webpQuality WebP 有损编码质量（0~100，越高越清晰、体积越大）。
	// 有损压缩会损失小目标细节，审核场景下慎用过低质量。
	webpQuality = 80

	// WebP 编码力度（0=最快，6=最小体积）定义在 image_vips.go 的 webpEncoderMethod。

	// maxDecodeDimension 送 AI 前图片的最长边上限（像素）。
	// 超过该值的图片等比缩放到最长边 = maxDecodeDimension 后再编码：
	// 控制解码/编码缓冲大小与 base64 体积（单请求内存峰值的关键因子）；
	// 小图不缩放，保持原始质量。
	//
	// 解码走 libvips 的 shrink-on-load（按 1/2、1/4、1/8 直接解出小图），
	// 大图不会全量解码后再缩放；内存由 libvips 的引用计数管理，每张图必须 Close()。
	maxDecodeDimension = 2048

	// maxDecodePixels 解码前的总像素上限（宽 × 高）。
	// 防止解压炸弹：文件字节量小但像素巨大的图片（如 PNG bomb 200KB
	// 可解出 30000×30000）在解码瞬间撑爆内存。超过上限直接报错，
	// 由调用方回退使用原图（原图未超过 MaxImageBytes，载荷可控）。
	maxDecodePixels = 40_000_000 // 约 6325×6325，覆盖常见 4000×6000 相机原图

	// decisionImageMaxSide 决策渠道图片的每边上限（上游常见限制 280~1568）。
	decisionImageMaxSide = 1568
	// decisionJPEGQuality 决策模型图片质量（审核场景不宜过低）。
	decisionJPEGQuality = 88

	// gifContactFrames 动画 GIF 抽帧数（3~5 之间）。
	gifContactFrames = 4
	// gifContactGap 拼图单元格之间的留白（像素）。
	gifContactGap = 4
	// maxGIFDecodeFrames 动画帧数上限，防止极端 GIF 长时间占用 CPU。
	maxGIFDecodeFrames = 600
)

// EncodeDataURI 将图片字节编码为标准 base64 data URI（data:<mime>;base64,<data>），
// 供 AI 网关作为 PartImage 传入（openai / anthropic 均兼容）。
func EncodeDataURI(data []byte, mimeType string) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// decodeConfigGuarded 读取图片尺寸并做解压炸弹防护（只读文件头，不解码像素）。
func decodeConfigGuarded(data []byte) (image.Config, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, "", fmt.Errorf("read image config failed: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return image.Config{}, "", fmt.Errorf("invalid image dimensions: %dx%d", cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return image.Config{}, "", fmt.Errorf("image too large to decode (%dx%d pixels, exceeds %d)", cfg.Width, cfg.Height, maxDecodePixels)
	}
	return cfg, format, nil
}

// isAnimatedWebP 判断 WebP 是否为动画：解析 RIFF 容器找到 VP8X 扩展块，
// 其 flags 字节的 bit1（0x02）表示文件带 ANIM 动画块。
// 直接解析容器头，不依赖具体解码库。
func isAnimatedWebP(data []byte) bool {
	if len(data) < 21 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false
	}
	// 从第一个 chunk 开始逐个扫描（chunk = 4 字节 id + 4 字节小端长度 + 数据，按偶数对齐）
	for off := 12; off+8 <= len(data); {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		body := off + 8
		switch id {
		case "VP8X":
			// VP8X 载荷首字节是 flags：0x02 = animation
			if body < len(data) {
				return data[body]&0x02 != 0
			}
			return false
		case "VP8 ", "VP8L":
			// 简单格式（无扩展块）不可能是动画
			return false
		}
		if size < 0 {
			return false
		}
		next := body + size + (size & 1)
		if next <= off {
			return false
		}
		off = next
	}
	return false
}
