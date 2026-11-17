package service

// 图片来源预处理（图片分析 / 场景决策共用）。
//
// 流程：并发槽位 → 入参校验 → URL 格式校验 → 下载（含头部校验）→
// 压缩转换 webp → base64 data URI。
//
// 与旧实现的差异（优化点）：
//   - 合并了原先的 Probe + Fetch 两次 GET：FetchImage 在读 body 之前已执行
//     状态码 / Content-Length / MIME 校验，Probe 属于多余的往返；
//   - 信号量从 ImageService 上移到这里：两个业务服务共用同一实例即可共享
//     并发上限，避免各自 4 并发导致内存峰值翻倍。
import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"log/slog"
	"strings"
	"time"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/model"
)

// ImagePreparer 图片统一预处理器。
// 并发安全：内部仅持有配置与信号量，可被多个 service 共享。
type ImagePreparer struct {
	cfg config.AIConfig
	sem chan struct{} // 并发信号量：限制同时进行的重内存操作数（下载/解码/编码/base64）
}

// NewImagePreparer 构建图片预处理器。
// workers 为同时进行的重内存操作上限，非法值钳位为 1。
func NewImagePreparer(cfg config.AIConfig, workers int) *ImagePreparer {
	return &ImagePreparer{
		cfg: cfg,
		sem: make(chan struct{}, effectiveWorkers(workers)),
	}
}

// effectiveWorkers 根据配置值决定并发工作者数：
//   - 配置缺失时 viper 已默认 4（见 config.Load），此处仅作最小值钳位；
//   - 配置值 <1（显式错误值如 0、负数）→ 钳位为 1；
//   - 正常值 → 直接使用配置值。
func effectiveWorkers(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// Prepare 把图片 URL 或 base64 转换为统一的可送 AI 的 data URI。
// 返回的 PreparedImage 可直接交给 AI 网关 / 决策客户端使用。
func (p *ImagePreparer) Prepare(ctx context.Context, in *model.ImageInput) (*model.PreparedImage, error) {
	// 0. 并发信号量：槽位耗尽时排队；排队期间 ctx 取消/超时则退出（不占槽位）
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, model.NewBizError("request canceled while waiting for analysis slot: " + ctx.Err().Error())
	}

	// 1. 入参校验：URL 与 base64 二选一
	if in.ImageURL == "" && in.ImageBase64 == "" {
		return nil, model.NewBizError("image_url or image_base64 must be provided")
	}

	// 2. 下载超时（与 AI 超时分离，缺省/非法回退 30s）
	imageTimeout := time.Duration(p.cfg.ImageTimeout) * time.Second
	if imageTimeout <= 0 {
		imageTimeout = 30 * time.Second
	}
	fetchOpt := helper.ImageFetchOptions{
		MaxBytes:     p.cfg.MaxImageBytes,
		AllowPrivate: p.cfg.AllowPrivateURLs,
		Timeout:      imageTimeout,
	}

	switch {
	case in.ImageURL != "":
		// 2.1 URL 基本格式校验（不发起网络请求）
		if err := helper.ValidateImageURL(in.ImageURL); err != nil {
			return nil, model.NewBizError(err.Error())
		}
		// 2.2 下载（内部先校验响应头，再按上限读取 body，防 TOCTOU / 虚报大小）
		raw, srcMIME, err := helper.FetchImage(ctx, in.ImageURL, fetchOpt)
		if err != nil {
			slog.Warn("fetch image failed",
				"url", in.ImageURL, "image_timeout", imageTimeout.String(), "err", err)
			return nil, model.NewBizError(err.Error())
		}
		return p.compressAndEncode(raw, srcMIME, "url", in.ImageURL), nil

	default:
		// 3.1 base64 直传：剥掉 data URI 前缀后解码
		b64s := in.ImageBase64
		if strings.HasPrefix(b64s, "data:") {
			if i := strings.Index(b64s, ","); i >= 0 {
				b64s = b64s[i+1:]
			}
		}
		raw, err := base64.StdEncoding.DecodeString(b64s)
		if err != nil {
			return nil, model.NewBizError("invalid image_base64: " + err.Error())
		}
		// 3.2 探测真实 MIME（避免 jpeg 内容被标 png 导致解码失败），
		//     识别失败按 image/png 兜底透传（保持旧行为）
		srcMIME := detectImageMIME(raw)
		return p.compressAndEncode(raw, srcMIME, "base64", ""), nil
	}
}

// compressAndEncode 压缩转换并编码为 data URI：
//   - 可识别格式走 CompressImageForAI（jpg/png/bmp → webp q80，webp 按需缩放）；
//   - 压缩失败或 MIME 未知时回退原图，不阻断主流程；
//   - source / ref 仅用于日志定位（url 或 base64）。
func (p *ImagePreparer) compressAndEncode(raw []byte, srcMIME, source, ref string) *model.PreparedImage {
	prepared := raw
	outMIME := srcMIME

	switch {
	case srcMIME == "":
		// 未知格式：按 image/png 兜底透传（保持旧行为，不做无谓解码尝试）
		outMIME = "image/png"
		slog.Warn("image compression skipped, unknown mime, fallback to image/png",
			"source", source, "url", ref)
	default:
		compressed, mime, err := helper.CompressImageForAI(raw, srcMIME)
		if err != nil {
			slog.Warn("image compression failed, fallback to original",
				"source", source, "url", ref, "src_mime", srcMIME, "err", err)
		} else {
			prepared, outMIME = compressed, mime
			slog.Info("image prepared for AI",
				"source", source, "url", ref, "src_mime", srcMIME, "mime", outMIME,
				"original_bytes", len(raw), "prepared_bytes", len(prepared))
		}
	}

	return &model.PreparedImage{
		DataURI: helper.EncodeDataURI(prepared, outMIME),
		MIME:    outMIME,
	}
}

// detectImageMIME 根据图片文件头探测真实 MIME（标准库 image 可识别的格式），
// 识别失败返回空串。
func detectImageMIME(data []byte) string {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width == 0 || cfg.Height == 0 {
		return ""
	}
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	case "webp":
		return "image/webp"
	}
	return ""
}
