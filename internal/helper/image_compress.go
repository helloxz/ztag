// Package helper 图片本地处理助手：送 AI 前的压缩转换与编码。
package helper

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"

	_ "image/gif" // 注册 GIF 解码器（detectImageMIME / image.Decode 识别用）
	_ "image/jpeg"
	_ "image/png"

	"github.com/deepteams/webp"
	"github.com/disintegration/imaging"
	_ "golang.org/x/image/bmp" // 注册 BMP 解码器（标准库 image 不含）
)

// webpEncodeMIME 转 WebP 编码白名单：仅这些格式解码后重编码为 WebP（有损 q80）。
// 其余格式即使可缩放（如 webp），也不进入 WebP 重编码白名单以外的转换。
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

// webpQuality WebP 有损编码质量（0~100，越高越清晰、体积越大）。
// 有损压缩会损失小目标细节，审核场景下慎用过低质量。
const webpQuality = 80

// maxDecodeDimension 送 AI 前图片的最长边上限（像素）。
// 超过该值的图片等比缩放到最长边 = maxDecodeDimension 后再编码：
// 控制解码/编码缓冲大小与 base64 体积（单请求内存峰值的关键因子）；
// 小图不缩放，保持原始质量。
//
// 注意：不使用 bimg/libvips 实现缩放——实测 bimg 1.1.9 的
// shrink-on-load 路径每操作泄漏约 30MB C 堆内存（vipsShrinkJpeg
// 未释放输入 image），长时间运行必然 OOM。纯标准库实现的内存
// 全部由 Go GC 管理，无 C 层泄漏。
const maxDecodeDimension = 2048

// maxDecodePixels 解码前的总像素上限（宽 × 高）。
// 防止解压炸弹：文件字节量小但像素巨大的图片（如 PNG bomb 200KB
// 可解出 30000×30000）在解码瞬间撑爆内存。超过上限直接报错，
// 由调用方回退使用原图（原图未超过 MaxImageBytes，载荷可控）。
const maxDecodePixels = 40_000_000 // 约 6325×6325，覆盖常见 4000×6000 相机原图

// CompressImageForAI 图片赠送 AI 前的统一压缩入口（独立函数，便于其它调用方复用）：
//   - image/jpeg、image/png、image/bmp → 解码后按需等比缩放，再编码为 WebP（有损 quality 80）；
//   - image/webp（静态）→ 按需等比缩放后同样重编码为 WebP；未超尺寸或重编码无收益时原样透传；
//   - 动画 webp、gif、avif 等 → 原样返回，不做任何处理；
//   - 最长边超过 maxDecodeDimension 的大图等比缩小到最长边 2048（宽高比不变，不裁切）；
//   - 总像素超过 maxDecodePixels 的图拒绝解码（防解压炸弹），返回错误由调用方回退；
//   - 含透明通道的图 alpha 由 WebP 的 ALPH chunk 承载，无需单独分支。
//
// 缩放使用 github.com/disintegration/imaging（纯 Go、Lanczos 高质量重采样、
// 水平/垂直两遍扫描式实现，不物化全尺寸源拷贝）；WebP 编码使用
// github.com/deepteams/webp（纯 Go、零 C 依赖，位流兼容 libwebp）。
// 输出与旧 bimg 版一致为 image/webp。
//
// 返回 (处理后的字节, 目标 MIME)。处理失败时返回错误，
// 由调用方决定是否回退使用原始图片。
func CompressImageForAI(data []byte, srcMIME string) (out []byte, outMIME string, err error) {
	// 不在可缩放白名单内的格式：原样透传，零开销
	if !resizableMIME[srcMIME] {
		return data, srcMIME, nil
	}

	// 动画 webp 不做缩放/重编码（帧序列重编码会丢失动画语义）：原样透传
	if srcMIME == "image/webp" {
		feat, err := webp.GetFeatures(bytes.NewReader(data))
		if err == nil && feat.HasAnimation {
			return data, srcMIME, nil
		}
		// GetFeatures 失败（如畸形文件）不在这里兜底，交由下方解码路径统一处理
	}

	// 1. 尺寸探测（仅读文件头，不解码像素）：
	//    超限的图等比缩放到最长边 maxDecodeDimension
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("read image config failed: %w", err)
	}
	// 解压炸弹防护：总像素超限直接拒绝解码（像素内存不随文件字节线性）
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, "", fmt.Errorf("image too large to decode (%dx%d pixels, exceeds %d)", cfg.Width, cfg.Height, maxDecodePixels)
	}
	needResize := cfg.Width > maxDecodeDimension || cfg.Height > maxDecodeDimension

	// 2. 解码（JPEG 解为 YCbCr，内存约 1.5 字节/像素，直接作为缩放源，
	//    避免先转 NRGBA 造成的全尺寸放大拷贝）
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image failed: %w", err)
	}

	// 3. 等比缩放：仅当最长边超过上限时，缩放到最长边 = maxDecodeDimension
	//    （imaging.Fit 等比适应 2048x2048 边界框，宽高比不变、不裁切、不放大）；
	//    小图（未超限）原尺寸直接编码
	var srcImage image.Image = img
	if needResize {
		srcImage = imaging.Fit(img, maxDecodeDimension, maxDecodeDimension, imaging.Lanczos)
	}

	// 4. 编码输出：
	//    - jpeg/png/bmp → WebP（有损 q80）
	//    - 静态 webp 未超尺寸 → 原样透传（不做无谓的有损重编码）
	//    - 静态 webp 超尺寸 → 缩小后重编码为 WebP；若重编码结果不比原图小
	//      （如已是高压缩小体积），回退原样透传，避免有损转有损反而变大
	if srcMIME == "image/webp" && !needResize {
		return data, srcMIME, nil
	}
	var buf bytes.Buffer
	if err := webp.Encode(&buf, srcImage, &webp.EncoderOptions{Quality: webpQuality, Method: 4}); err != nil {
		return nil, "", fmt.Errorf("encode webp failed: %w", err)
	}
	if srcMIME == "image/webp" && buf.Len() >= len(data) {
		return data, srcMIME, nil
	}
	return buf.Bytes(), "image/webp", nil
}

// EncodeDataURI 将图片字节编码为标准 base64 data URI（data:<mime>;base64,<data>），
// 供 AI 网关作为 PartImage 传入（openai / anthropic 均兼容该格式）。
func EncodeDataURI(data []byte, mimeType string) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
