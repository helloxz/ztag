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

// concurrencyLimitByMemory 按进程可用内存上限动态决定并发分析槽位数：
//   - < 500MB：2
//   - 500MB ~ 1GB：4
//   - > 1GB：8
//
// 依据：单请求峰值内存约 90MB（6000x4500 大图解码 + 缩放 + base64），
// 并发峰值 ≈ 槽位数 × 90MB，需为 Go 运行时/连接池预留余量，防止打爆容器内存；
// 内存探测失败（返回 0）时按最保守的 2 处理。
func concurrencyLimitByMemory() int {
	mem := helper.MemoryLimitBytes()
	switch {
	case mem == 0 || mem < 500*1024*1024:
		return 2
	case mem < 1024*1024*1024:
		return 4
	default:
		return 8
	}
}

// NewImageService 构建图片分析业务服务。
func NewImageService(gateway *ai.Gateway, cfg config.AIConfig) *ImageService {
	return &ImageService{
		gateway: gateway,
		cfg:     cfg,
		sem:     make(chan struct{}, concurrencyLimitByMemory()),
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
				"src_mime", srcMIME, "err", err)
			prepared, outMIME = raw, srcMIME
		} else {
			slog.Info("image prepared for AI",
				"src_mime", srcMIME, "mime", outMIME,
				"original_bytes", len(raw), "prepared_bytes", len(prepared))
		}
		req.ImageDataURI = helper.EncodeDataURI(prepared, outMIME)
		req.ImageMIME = outMIME
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
