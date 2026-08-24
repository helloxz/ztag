// Package service 业务编排层：负责请求校验、流程编排与结果规范化，
// 向上为 handler 提供能力，向下依赖 ai 网关，不直接接触 HTTP 细节。
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/model"
)

// ImageService 图片分析业务服务。
type ImageService struct {
	gateway *ai.Gateway // AI 多渠道网关
	cfg     config.AIConfig
}

// NewImageService 构建图片分析业务服务。
func NewImageService(gateway *ai.Gateway, cfg config.AIConfig) *ImageService {
	return &ImageService{gateway: gateway, cfg: cfg}
}

// AnalyzeImage 编排一次图片分析请求，完整链路：
//
//	URL 格式校验 → header 探测校验（大小/MIME）→ 下载 → 压缩转换 webp → base64 → AI 网关分析
func (s *ImageService) AnalyzeImage(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error) {
	// 1. 输入校验：图片 URL 与 base64 二选一
	if req.ImageURL == "" && req.ImageBase64 == "" {
		return nil, model.NewBizError("image_url or image_base64 must be provided")
	}

	// 2. 构造图片抓取参数
	fetchOpt := helper.ImageFetchOptions{
		MaxBytes:     s.cfg.MaxImageBytes,
		AllowPrivate: s.cfg.AllowPrivateURLs,
		Timeout:      time.Duration(s.cfg.Timeout) * time.Second, // 与 AI 调用同源超时，避免慢源图片拖垮请求
	}

	// 3. 图片来源处理：URL 或 base64 → 统一为 data URI 供 AI 网关使用
	switch {
	case req.ImageURL != "":
		// 3.1 校验 URL 基本格式（未发起网络请求）
		if err := helper.ValidateImageURL(req.ImageURL); err != nil {
			return nil, model.NewBizError(err.Error())
		}
		// 3.2 探测响应头：大小 ≤ 上限 且 MIME 为图片（不下载内容）
		if _, err := helper.ProbeImage(ctx, req.ImageURL, fetchOpt); err != nil {
			return nil, model.NewBizError(err.Error())
		}
		// 3.3 下载图片原始字节（下载前再次头部校验，防 TOCTOU）
		imgData, srcMIME, err := helper.FetchImage(ctx, req.ImageURL, fetchOpt)
		if err != nil {
			return nil, model.NewBizError(err.Error())
		}
		// 3.4 压缩转换：jpg/png/bmp → webp（其余原样）；压缩失败回退原图，不阻断主流程
		prepared, outMIME, err := helper.CompressImageForAI(imgData, srcMIME)
		if err != nil {
			slog.Warn("image compression failed, fallback to original",
				"src_mime", srcMIME, "err", err)
			prepared, outMIME = imgData, srcMIME
		} else {
			slog.Info("image prepared for AI",
				"src_mime", srcMIME, "mime", outMIME,
				"original_bytes", len(imgData), "prepared_bytes", len(prepared))
		}
		req.ImageDataURI = helper.EncodeDataURI(prepared, outMIME)
		req.ImageMIME = outMIME

	case req.ImageBase64 != "":
		// base64 直传：补全 data URI（暂不校验大小，后续可加）
		req.ImageDataURI = "data:image/png;base64," + req.ImageBase64
		req.ImageMIME = "image/png"
	}

	// 4. 调用 AI 网关分析（含主备切换）
	return s.gateway.Analyze(ctx, req)
}
