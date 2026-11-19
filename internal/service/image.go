// Package service 业务编排层：负责请求校验、流程编排与结果规范化，
// 向上为 handler 提供能力，向下依赖 ai 网关，不直接接触 HTTP 细节。
package service

import (
	"context"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/model"
)

// ImageService 图片分析业务服务（关键词/描述 + 分类）。
type ImageService struct {
	gateway  *ai.Gateway
	preparer *ImagePreparer // 共享的图片预处理器（含并发信号量）
}

// NewImageService 构建图片分析业务服务。
// preparer 与场景决策服务共用同一实例，从而共享并发上限。
func NewImageService(gateway *ai.Gateway, preparer *ImagePreparer) *ImageService {
	return &ImageService{
		gateway:  gateway,
		preparer: preparer,
	}
}

// AnalyzeImage 编排一次图片分析请求，完整链路：
//
//	共享图片预处理（并发槽位 → URL 校验 → 下载 → 压缩转 webp → base64）→ AI 网关分析
func (s *ImageService) AnalyzeImage(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error) {
	img, err := s.preparer.Prepare(ctx, &req.ImageInput, ImageTargetWebP)
	if err != nil {
		return nil, err
	}

	// 回填内部字段：AI 网关按 data URI 构造图片消息（对外 JSON 不输出）
	req.ImageDataURI = img.DataURI
	req.ImageMIME = img.MIME

	// 调用 AI 网关分析（含主备切换）
	return s.gateway.Analyze(ctx, req)
}
