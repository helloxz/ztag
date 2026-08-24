// Package handler 表现层：负责参数解析、调用业务服务、输出统一响应。
// 只做“翻译”，不包含业务逻辑。
package handler

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/model"
	"github.com/helloxz/ztag/internal/service"
)

// ImageHandler 图片分析相关接口的处理器。
type ImageHandler struct {
	svc *service.ImageService
}

// NewImageHandler 构建图片分析处理器。
func NewImageHandler(svc *service.ImageService) *ImageHandler {
	return &ImageHandler{svc: svc}
}

// Analyze 处理 POST /api/v1/image/analyze。
//
// 请求体示例：
//
//	{ "image_url": "https://example.com/x.jpg" }
//	或 { "image_base64": "<base64 内容>" }
//	可选：{ "model": "default/gpt-4o-mini" } 或 { "load_balance": true }
//
// 响应为统一结构 { code, message, data }，data 为分析结果。
func (h *ImageHandler) Analyze(c *gin.Context) {
	// 记录请求起始时间，用于计算总处理耗时（毫秒）
	start := time.Now()

	var req model.AnalyzeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body: "+err.Error())
		return
	}

	result, err := h.svc.AnalyzeImage(c.Request.Context(), &req)
	if err != nil {
		// 统一错误转换：业务错误带码返回，其余按内部错误兜底
		helper.FailWithError(c, err)
		return
	}

	// 填充请求总耗时（从解析请求到分析完成，含图片下载与 AI 调用）
	result.ElapsedMs = time.Since(start).Milliseconds()
	helper.OK(c, result)
}
