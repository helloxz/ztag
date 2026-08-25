// Package handler 表现层：负责参数解析、调用业务服务、输出统一响应。
// 只做“翻译”，不包含业务逻辑。
package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/model"
	"github.com/helloxz/ztag/internal/service"
)

// ImageHandler 图片分析相关接口的处理器。
type ImageHandler struct {
	svc            *service.ImageService
	maxRequestBody int64 // 请求体大小上限（字节）；0 表示不限制
}

// NewImageHandler 构建图片分析处理器。
// maxRequestBody 为请求体字节上限（须覆盖 base64 编码膨胀与 JSON 包装），
// 用于在读取阶段源头拦截超大请求体，避免超大负载全量进内存。
func NewImageHandler(svc *service.ImageService, maxRequestBody int64) *ImageHandler {
	return &ImageHandler{svc: svc, maxRequestBody: maxRequestBody}
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
	// 源头拦截超大请求体：在读取阶段即掐断（超限返回 "http: request body too large"），
	// 防止超大 base64 负载全量进入内存后再被业务层拒绝
	if h.maxRequestBody > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxRequestBody)
	}
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
