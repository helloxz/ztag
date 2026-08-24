// Package helper 图片本地处理助手：送 AI 前的压缩转换与编码。
package helper

import (
	"encoding/base64"
	"fmt"

	"github.com/h2non/bimg"
)

// webpConvertibleMIME 支持转 webp 的源 MIME 白名单：
// 仅这些常见格式做压缩转换，其余（gif、avif、webp 等）原样透传。
var webpConvertibleMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/bmp":  true,
}

// webpQuality webp 压缩质量（0~100，越高越清晰、体积越大）。
// 有损压缩会损失小目标细节，审核场景下慎用过低质量。
const webpQuality = 80

// CompressImageForAI 图片赠送 AI 前的统一压缩入口（独立函数，便于其它调用方复用）：
//   - image/jpeg、image/png、image/bmp → 转换为 webp 并压缩（quality 80）；
//   - 其它格式（gif、avif、png 透明图、webp 等）→ 原样返回，不做任何处理。
//
// 返回 (处理后的字节, 目标 MIME)。bimg 处理失败时返回错误，
// 由调用方决定是否回退使用原始图片。
func CompressImageForAI(data []byte, srcMIME string) (out []byte, outMIME string, err error) {
	// 不在转换白名单内的格式：原样透传，零开销
	if !webpConvertibleMIME[srcMIME] {
		return data, srcMIME, nil
	}

	out, err = bimg.Resize(data, bimg.Options{
		Type:    bimg.WEBP, // 输出 webp
		Quality: webpQuality,
	})
	if err != nil {
		return nil, "", fmt.Errorf("convert to webp failed: %w", err)
	}
	return out, "image/webp", nil
}

// EncodeDataURI 将图片字节编码为标准 base64 data URI（data:<mime>;base64,<data>），
// 供 AI 网关作为 PartImage 传入（openai / anthropic 均兼容该格式）。
func EncodeDataURI(data []byte, mimeType string) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
