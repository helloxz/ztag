package helper

// 图片处理实现：libvips（govips cgo 绑定）。
//
// 构建：CGO_ENABLED=1 go build -o bin/ztag ./cmd/server
// 依赖：libvips 8.10+（Debian 12 自带 8.14.1）与 pkg-config。
//
// 关键设计：
//   - 解码走 libvips 的 shrink-on-load（JPEG/WebP 按 1/2、1/4、1/8 直接解出小图），
//     大图的中间缓冲远小于"全量解码 + 两遍缩放"；
//   - 缩放与编码在同一管线内流式完成，不再物化全尺寸 NRGBA；
//   - thumbnail 默认按 EXIF Orientation 自动摆正（手机竖拍图不会被当成横图送去审核）；
//   - 生命周期：每张图必须 Close()，否则 native 内存泄漏（Go GC 看不见 libvips 的堆）。

import (
	"fmt"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"

	_ "golang.org/x/image/webp" // 注册静态 WebP 解码器（image.DecodeConfig 探测 MIME 用）
)

var (
	vipsOnce sync.Once
	vipsErr  error
)

// webpEncoderMethod libvips（libwebp）的编码力度：0 = 最快。
// 实测 Method 0 比默认的 4 快约 2~4 倍（2MP 255ms → 117ms，PNG 带 alpha 374ms → 95ms），
// 同质量下输出体积差异在 ±10% 内（高熵照片甚至更小，截图类会略大）。
// 送审图片本就要再上传给模型，这点体积换 2 倍以上的编码速度是划算的。
const webpEncoderMethod = 0

// InitImagePipeline 初始化 libvips（进程内只能初始化一次）。
//
// 关键的降内存参数：
//   - ConcurrencyLevel=1：并发由业务层的信号量控制，禁止 libvips 再开一套线程池，
//     否则 workers×vips 线程会超订 CPU 并把峰值内存放大数倍；
//   - MaxCacheMem/MaxCacheSize/MaxCacheFiles=0：govips 的语义是"字段 >= 0 就用该值"，
//     传 0 即真正关闭操作缓存（默认是 50MB/100 个操作，会持有中间图引用，
//     表现为 RSS 只涨不降、看起来像泄漏）。
func InitImagePipeline() error {
	vipsOnce.Do(func() {
		// libvips 默认把 Info 级别打到 stderr，压到 Warning 避免刷屏
		vips.LoggingSettings(nil, vips.LogLevelWarning)
		vipsErr = vips.Startup(&vips.Config{
			ConcurrencyLevel: 1,
			MaxCacheMem:      0,
			MaxCacheSize:     0,
			MaxCacheFiles:    0,
		})
	})
	return vipsErr
}

// ensureVips 保证 libvips 已按预期参数初始化。
// 不能依赖 govips 内部的 startupIfNeeded：它用默认配置启动（缓存 50MB），
// 会绕过上面的低内存参数。
func ensureVips() error {
	if err := InitImagePipeline(); err != nil {
		return fmt.Errorf("init libvips failed: %w", err)
	}
	return nil
}

// ShutdownImagePipeline 释放 libvips 资源（进程退出前调用）。
func ShutdownImagePipeline() {
	vips.Shutdown()
}

// ImagePipelineName 返回当前图片处理实现标识（启动日志用）。
func ImagePipelineName() string { return "libvips " + vips.Version }

// CompressImageForAI 图片赠送 AI 前的统一压缩入口（libvips 实现）。
// 对外行为约定：
//   - jpeg/png/bmp → 按需缩放后编码为 WebP（有损 q80）；
//   - 静态 webp 未超尺寸 → 原样透传；超尺寸 → 缩放重编码，变大则回退原图；
//   - 动画 webp、gif、avif 等 → 原样透传；
//   - 超过 maxDecodePixels 的图拒绝处理（防解压炸弹）。
func CompressImageForAI(data []byte, srcMIME string) (out []byte, outMIME string, err error) {
	if !resizableMIME[srcMIME] {
		return data, srcMIME, nil
	}
	if srcMIME == "image/webp" && isAnimatedWebP(data) {
		return data, srcMIME, nil
	}

	cfg, _, err := decodeConfigGuarded(data)
	if err != nil {
		return nil, "", err
	}
	// 静态 webp 未超尺寸：不重编码（避免有损转有损）
	if srcMIME == "image/webp" && cfg.Width <= maxDecodeDimension && cfg.Height <= maxDecodeDimension {
		return data, srcMIME, nil
	}

	if err := ensureVips(); err != nil {
		return nil, "", err
	}
	// SizeDown：只缩不放；InterestingNone：等比适应边界框、不裁切
	img, err := vips.NewThumbnailWithSizeFromBuffer(data, maxDecodeDimension, maxDecodeDimension, vips.InterestingNone, vips.SizeDown)
	if err != nil {
		return nil, "", fmt.Errorf("decode image failed: %w", err)
	}
	defer img.Close() // 必须：native 内存不受 Go GC 管理

	out, _, err = img.ExportWebp(&vips.WebpExportParams{
		Quality:         webpQuality,
		ReductionEffort: webpEncoderMethod,
		StripMetadata:   true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("encode webp failed: %w", err)
	}
	if srcMIME == "image/webp" && len(out) >= len(data) {
		return data, srcMIME, nil
	}
	return out, "image/webp", nil
}

// FitDecisionImage 把图片规整为决策模型可接受的形式：JPEG，每边 ≤ 1568（libvips 实现）。
//
// 行为约定：已合规的 JPEG/PNG 原样透传；GIF 仍走纯 Go 抽帧拼图；
// 动画 WebP 返回明确错误。透明通道会合成到白底（避免 JPEG 变黑）。
func FitDecisionImage(data []byte, srcMIME string) (out []byte, outMIME string, err error) {
	switch srcMIME {
	case "image/jpeg", "image/png":
		cfg, _, err := decodeConfigGuarded(data)
		if err != nil {
			return nil, "", err
		}
		if cfg.Width <= decisionImageMaxSide && cfg.Height <= decisionImageMaxSide {
			return data, srcMIME, nil
		}
		return fitDecisionJPEG(data)
	case "image/gif":
		return fitGIFImage(data)
	case "image/bmp":
		if _, _, err := decodeConfigGuarded(data); err != nil {
			return nil, "", err
		}
		return fitDecisionJPEG(data)
	case "image/webp":
		if isAnimatedWebP(data) {
			return nil, "", fmt.Errorf("animated webp is not supported for this decision channel, please convert it to JPEG/PNG")
		}
		if _, _, err := decodeConfigGuarded(data); err != nil {
			return nil, "", err
		}
		return fitDecisionJPEG(data)
	default:
		if srcMIME == "" {
			return nil, "", fmt.Errorf("unsupported or unrecognized image format for this decision channel")
		}
		return nil, "", fmt.Errorf("unsupported image format for this decision channel: %s", srcMIME)
	}
}

// fitDecisionJPEG 用 libvips 缩放并编码为 JPEG（每边 ≤ 1568，白底，去元数据）。
func fitDecisionJPEG(data []byte) ([]byte, string, error) {
	if err := ensureVips(); err != nil {
		return nil, "", err
	}
	img, err := vips.NewThumbnailWithSizeFromBuffer(data, decisionImageMaxSide, decisionImageMaxSide, vips.InterestingNone, vips.SizeDown)
	if err != nil {
		return nil, "", fmt.Errorf("decode image failed: %w", err)
	}
	defer img.Close()

	// 透明通道合成白底（与 GIF/JPEG 输出一致，避免透明像素变黑）
	if img.HasAlpha() {
		if err := img.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
			return nil, "", fmt.Errorf("flatten alpha failed: %w", err)
		}
	}
	out, _, err := img.ExportJpeg(&vips.JpegExportParams{
		Quality:        decisionJPEGQuality,
		Interlace:      false, // 非渐进式：避免整幅缓冲，也更利于上游快速解码
		OptimizeCoding: true,
		StripMetadata:  true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("encode jpeg failed: %w", err)
	}
	return out, "image/jpeg", nil
}
