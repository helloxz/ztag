package helper

// 决策渠道的 GIF 处理与公共 JPEG 编码助手。
//
// GIF 抽帧拼图使用纯 Go 实现（imaging + stdlib），原因：
//   - 动画 GIF 需要按 disposal 规则回放后均匀抽帧拼成网格，libvips 的
//     页面语义无法直接表达"按时间均匀采样 + 网格拼接"；
//   - GIF 体积小、出现频率低，走纯 Go 不算瓶颈。

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"math"

	"github.com/disintegration/imaging"
)

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

// encodeDecisionJPEG 缩放（如需要）、白底合成并编码为 JPEG（GIF 路径使用）。
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
