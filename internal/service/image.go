// Package service 业务编排层：负责请求校验、流程编排与结果规范化，
// 向上为 handler 提供能力，向下依赖 ai 网关，不直接接触 HTTP 细节。
package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"log/slog"
	"strings"
	"time"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/model"
)

// ImageService 图片分析业务服务。
type ImageService struct {
	gateway *ai.Gateway
	cfg     config.AIConfig
	sem     chan struct{} // 并发分析信号量：控制同时进行的重内存操作数（下载/解码/base64）
}

// effectiveWorkers 根据配置值决定并发工作者数：
//   - 配置缺失时 viper 已默认 4（见 config.Load），此处仅作最小值钳位；
//   - 配置值 <1（显式错误值如 0、负数）→ 钳位为 1；
//   - 正常值 → 直接使用配置值。
//
// 约束：无论何种情况，并发不能小于 1；未配置时为 4。
func effectiveWorkers(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// NewImageService 构建图片分析业务服务。
func NewImageService(gateway *ai.Gateway, cfg config.AIConfig, workers int) *ImageService {
	return &ImageService{
		gateway: gateway,
		cfg:     cfg,
		sem:     make(chan struct{}, effectiveWorkers(workers)),
	}
}

// AnalyzeImage 编排一次图片分析请求，完整链路：
//
//	并发信号量 → URL 格式校验 → header 探测校验（大小/MIME）→ 下载 → 压缩转换 → base64 → AI 网关分析
func (s *ImageService) AnalyzeImage(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error) {
	// 0. 并发信号量：占用一个槽位；槽位耗尽时请求排队等待，
	// 排队期间 ctx 取消/超时则退出（不占用槽位）。
	// 保证任一瞬间重内存分析操作不超过上限，防止并发峰值打爆容器内存。
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, model.NewBizError("request canceled while waiting for analysis slot: " + ctx.Err().Error())
	}

	// 1. 输入校验：图片 URL 与 base64 二选一
	if req.ImageURL == "" && req.ImageBase64 == "" {
		return nil, model.NewBizError("image_url or image_base64 must be provided")
	}

	// 2. 构造图片抓取参数（下载超时与 AI 超时分离，缺省/非法回退 30s）
	//    Header 探测超时 = 下载超时 -20s，兜底不低于 10s（按用户方案）
	imageTimeout := time.Duration(s.cfg.ImageTimeout) * time.Second
	if imageTimeout <= 0 {
		imageTimeout = 30 * time.Second
	}
	probeTimeout := imageTimeout - 20*time.Second
	if probeTimeout < 10*time.Second {
		probeTimeout = 10 * time.Second
	}
	probeOpt := helper.ImageFetchOptions{
		MaxBytes:     s.cfg.MaxImageBytes,
		AllowPrivate: s.cfg.AllowPrivateURLs,
		Timeout:      probeTimeout,
	}
	fetchOpt := helper.ImageFetchOptions{
		MaxBytes:     s.cfg.MaxImageBytes,
		AllowPrivate: s.cfg.AllowPrivateURLs,
		Timeout:      imageTimeout,
	}

	// 3. 图片来源处理：URL 或 base64 → 统一为 data URI 供 AI 网关使用
	switch {
	case req.ImageURL != "":
		// 3.1 校验 URL 基本格式（未发起网络请求）
		if err := helper.ValidateImageURL(req.ImageURL); err != nil {
			return nil, model.NewBizError(err.Error())
		}
		// 3.2 探测响应头：大小 ≤ 上限 且 MIME 为图片（不下载内容）
		if _, err := helper.ProbeImage(ctx, req.ImageURL, probeOpt); err != nil {
			slog.Warn("probe image failed",
				"url", req.ImageURL, "probe_timeout", probeTimeout.String(), "image_timeout", imageTimeout.String(), "err", err)
			return nil, model.NewBizError(err.Error())
		}
		// 3.3 下载图片原始字节（下载前再次头部校验，防 TOCTOU）
		imgData, srcMIME, err := helper.FetchImage(ctx, req.ImageURL, fetchOpt)
		if err != nil {
			slog.Warn("fetch image failed",
				"url", req.ImageURL, "image_timeout", imageTimeout.String(), "err", err)
			return nil, model.NewBizError(err.Error())
		}
		// 3.4 压缩转换：jpg/png/bmp → webp（其余原样）；压缩失败回退原图，不阻断主流程
		prepared, outMIME, err := helper.CompressImageForAI(imgData, srcMIME)
		if err != nil {
			slog.Warn("image compression failed, fallback to original",
				"url", req.ImageURL, "src_mime", srcMIME, "err", err)
			prepared, outMIME = imgData, srcMIME
		} else {
			slog.Info("image prepared for AI",
				"url", req.ImageURL, "src_mime", srcMIME, "mime", outMIME,
				"original_bytes", len(imgData), "prepared_bytes", len(prepared))
		}
		req.ImageDataURI = helper.EncodeDataURI(prepared, outMIME)
		req.ImageMIME = outMIME

	case req.ImageBase64 != "":
		// base64 直传：剥掉 data URI 前缀（如带）后解码为原始字节
		b64s := req.ImageBase64
		if strings.HasPrefix(b64s, "data:") {
			if i := strings.Index(b64s, ","); i >= 0 {
				b64s = b64s[i+1:]
			}
		}
		raw, err := base64.StdEncoding.DecodeString(b64s)
		if err != nil {
			return nil, model.NewBizError("invalid image_base64: " + err.Error())
		}
		// 探测真实 MIME（避免 jpeg 内容被标 png 导致 AI 网关解码失败），
		// 可识别格式走与 URL 分支相同的压缩路径；识别失败按原样透传
		srcMIME := detectImageMIME(raw)
		prepared, outMIME, err := helper.CompressImageForAI(raw, srcMIME)
		if err != nil || srcMIME == "" {
			if srcMIME == "" {
				srcMIME = "image/png" // 未知格式兜底（保持旧行为）
			}
			slog.Warn("image compression failed, fallback to original",
				"source", "base64", "src_mime", srcMIME, "err", err)
			prepared, outMIME = raw, srcMIME
		} else {
			slog.Info("image prepared for AI",
				"source", "base64", "src_mime", srcMIME, "mime", outMIME,
				"original_bytes", len(raw), "prepared_bytes", len(prepared))
		}
	}

	// 4. 调用 AI 网关分析（含主备切换）
	return s.gateway.Analyze(ctx, req)
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
