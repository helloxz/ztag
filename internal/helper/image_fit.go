package helper

// 决策模型（System One / Jev 系渠道）图片规整。
//
// 上游对图片有明确限制（例如 microsoft/microsoft-decision-1.1 只收 PNG/JPEG、
// 每边 280~1568），而默认预处理会统一转成 WebP，所以决策渠道需要单独走一条
// 规整链路：
//   - JPEG/PNG 且尺寸合规 → 原样透传，避免无谓的二次有损压缩；
//   - BMP、静态 WebP、静态 GIF → 解码后按需缩放，统一输出 JPEG；
//   - 动画 GIF → 均匀抽帧（默认 4 帧）拼成一张网格 JPEG，避免只取首帧漏内容；
//   - 动画 WebP、AVIF 等 → 返回明确错误，不把不兼容的图送去上游。
//
// 输出统一为 JPEG（透明通道合成为白底），最长边不超过 decisionImageMaxSide。
import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"math"

	"github.com/deepteams/webp" // 静态 WebP 解码 + 动画探测
	"github.com/disintegration/imaging"
)

const (
	decisionImageMaxSide = 1568 // 上游允许的每边上限
	decisionJPEGQuality  = 88   // 决策模型图片质量（审核场景不宜过低）
	gifContactFrames     = 4    // 动画 GIF 抽帧数（3~5 之间）
	gifContactGap        = 4    // 拼图单元格之间的留白（像素）
	maxGIFDecodeFrames   = 600  // 动画帧数上限，防止极端 GIF 长时间占用 CPU
)

// FitDecisionImage 把图片规整为决策模型可接受的形式：JPEG，每边 ≤ 1568。
//
// 返回值中的 MIME 固定为 image/jpeg；已是合规 JPEG/PNG 时原样返回。
// 任意格式不兼容或解码失败都返回错误，由调用方直接报错（不回退原图）。
func FitDecisionImage(data []byte, srcMIME string) (out []byte, outMIME string, err error) {
	switch srcMIME {
	case "image/jpeg", "image/png":
		return fitStaticImage(data, srcMIME)
	case "image/gif":
		return fitGIFImage(data)
	case "image/bmp":
		return convertStaticImage(data)
	case "image/webp":
		if feat, ferr := webp.GetFeatures(bytes.NewReader(data)); ferr == nil && feat.HasAnimation {
			return nil, "", fmt.Errorf("animated webp is not supported for this decision channel, please convert it to JPEG/PNG")
		}
		return convertStaticImage(data)
	default:
		if srcMIME == "" {
			return nil, "", fmt.Errorf("unsupported or unrecognized image format for this decision channel")
		}
		return nil, "", fmt.Errorf("unsupported image format for this decision channel: %s", srcMIME)
	}
}

// fitStaticImage 处理 JPEG/PNG：尺寸合规直接透传，否则缩放并转 JPEG。
func fitStaticImage(data []byte, srcMIME string) ([]byte, string, error) {
	cfg, _, err := decodeConfigGuarded(data)
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= decisionImageMaxSide && cfg.Height <= decisionImageMaxSide {
		return data, srcMIME, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image failed: %w", err)
	}
	out, err := encodeDecisionJPEG(img)
	if err != nil {
		return nil, "", err
	}
	return out, "image/jpeg", nil
}

// convertStaticImage 处理需要转码的静态格式（BMP / 静态 WebP）。
func convertStaticImage(data []byte) ([]byte, string, error) {
	if _, _, err := decodeConfigGuarded(data); err != nil {
		return nil, "", err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image failed: %w", err)
	}
	out, err := encodeDecisionJPEG(img)
	if err != nil {
		return nil, "", err
	}
	return out, "image/jpeg", nil
}

// fitGIFImage 处理 GIF：单帧按静态图转 JPEG；多帧抽帧拼成一张网格 JPEG。
func fitGIFImage(data []byte) ([]byte, string, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode gif failed: %w", err)
	}
	if len(g.Image) == 0 {
		return nil, "", fmt.Errorf("gif has no frames")
	}

	width, height := g.Config.Width, g.Config.Height
	if width <= 0 || height <= 0 {
		bounds := g.Image[0].Bounds()
		width, height = bounds.Dx(), bounds.Dy()
	}
	if len(g.Image) > maxGIFDecodeFrames {
		return nil, "", fmt.Errorf("animated gif has too many frames (%d, max %d)", len(g.Image), maxGIFDecodeFrames)
	}
	// 解码预算：逻辑画布像素 × 帧数，防止大动画把内存打爆
	if int64(width)*int64(height)*int64(len(g.Image)) > maxDecodePixels {
		return nil, "", fmt.Errorf("animated gif is too large to process (%dx%d x %d frames)", width, height, len(g.Image))
	}

	if len(g.Image) == 1 {
		out, err := encodeDecisionJPEG(g.Image[0])
		if err != nil {
			return nil, "", err
		}
		return out, "image/jpeg", nil
	}

	frames := composeGIFContactFrames(g, sampleGIFIndices(len(g.Image), gifContactFrames))
	out, mime, err := buildGIFContactSheet(frames)
	if err != nil {
		return nil, "", err
	}
	return out, mime, nil
}

// sampleGIFIndices 从 total 帧中均匀采样最多 limit 帧（含首帧与末帧）。
func sampleGIFIndices(total, limit int) []int {
	if total <= 0 || limit <= 0 {
		return nil
	}
	if total <= limit {
		indices := make([]int, total)
		for i := range indices {
			indices[i] = i
		}
		return indices
	}
	if limit == 1 {
		return []int{0}
	}

	indices := make([]int, 0, limit)
	for i := 0; i < limit; i++ {
		index := int(math.Round(float64(i) * float64(total-1) / float64(limit-1)))
		if len(indices) > 0 && index == indices[len(indices)-1] {
			continue
		}
		indices = append(indices, index)
	}
	return indices
}

// composeGIFContactFrames 按 GIF 的 disposal 规则回放帧，在采样点做快照。
// GIF 的增量帧只包含变化区域，必须从第一帧开始合成，不能单独解码某一帧。
func composeGIFContactFrames(g *gif.GIF, indices []int) []image.Image {
	width, height := g.Config.Width, g.Config.Height
	if width <= 0 || height <= 0 {
		bounds := g.Image[0].Bounds()
		width, height = bounds.Dx(), bounds.Dy()
	}
	bounds := image.Rect(0, 0, width, height)

	canvas := image.NewRGBA(bounds)
	// 用白色打底，避免透明区域在 JPEG 编码后变黑
	draw.Draw(canvas, bounds, image.NewUniform(color.White), image.Point{}, draw.Src)
	previous := image.NewRGBA(bounds)

	wanted := make(map[int]bool, len(indices))
	for _, index := range indices {
		wanted[index] = true
	}

	frames := make([]image.Image, 0, len(indices))
	for i, frame := range g.Image {
		disposal := byte(gif.DisposalNone)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		// DisposalPrevious 需要先保存当前画面，供绘制后恢复
		if disposal == gif.DisposalPrevious {
			copy(previous.Pix, canvas.Pix)
		}

		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		if wanted[i] {
			snapshot := image.NewRGBA(bounds)
			copy(snapshot.Pix, canvas.Pix)
			frames = append(frames, snapshot)
		}

		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(canvas, frame.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			copy(canvas.Pix, previous.Pix)
		}
	}
	return frames
}

// buildGIFContactSheet 把多帧拼成一张白底网格图：
//   - 2 帧 → 2×1；3~4 帧 → 2×2（不足补白）；
//   - 每格 784×784，整体最长边不超过 1568。
func buildGIFContactSheet(frames []image.Image) ([]byte, string, error) {
	if len(frames) == 0 {
		return nil, "", fmt.Errorf("gif has no frames to compose")
	}

	cols, rows := 2, 2
	switch len(frames) {
	case 1:
		cols, rows = 1, 1
	case 2:
		cols, rows = 2, 1
	}
	cellSize := decisionImageMaxSide / 2

	canvas := image.NewRGBA(image.Rect(0, 0, cellSize*cols, cellSize*rows))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	for i, frame := range frames {
		col, row := i%cols, i/cols
		scaled := imaging.Fit(frame, cellSize-2*gifContactGap, cellSize-2*gifContactGap, imaging.Lanczos)
		x := col*cellSize + (cellSize-scaled.Bounds().Dx())/2
		y := row*cellSize + (cellSize-scaled.Bounds().Dy())/2
		draw.Draw(canvas,
			image.Rect(x, y, x+scaled.Bounds().Dx(), y+scaled.Bounds().Dy()),
			scaled, scaled.Bounds().Min, draw.Over)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: decisionJPEGQuality}); err != nil {
		return nil, "", fmt.Errorf("encode jpeg failed: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

// encodeDecisionJPEG 缩放（如需要）、白底合成并编码为 JPEG。
func encodeDecisionJPEG(img image.Image) ([]byte, error) {
	scaled := scaleToMaxSide(img, decisionImageMaxSide)
	flat := flattenOnWhite(scaled)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: decisionJPEGQuality}); err != nil {
		return nil, fmt.Errorf("encode jpeg failed: %w", err)
	}
	return buf.Bytes(), nil
}

// scaleToMaxSide 等比缩放到最长边不超过 maxSide；未超限时原样返回。
func scaleToMaxSide(img image.Image, maxSide int) image.Image {
	bounds := img.Bounds()
	if bounds.Dx() <= maxSide && bounds.Dy() <= maxSide {
		return img
	}
	return imaging.Fit(img, maxSide, maxSide, imaging.Lanczos)
}

// flattenOnWhite 把图片合成到白色背景，避免透明像素在 JPEG 中变黑。
func flattenOnWhite(img image.Image) image.Image {
	bounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Over)
	return dst
}

// decodeConfigGuarded 读取图片尺寸并做解压炸弹防护。
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
