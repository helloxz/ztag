// Package handler 表现层：负责参数解析、调用业务服务、输出统一响应。
// 只做“翻译”，不包含业务逻辑。
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/model"
	"github.com/helloxz/ztag/internal/service"
)

// DecisionHandler 场景决策接口的处理器。
type DecisionHandler struct {
	svc            *service.DecisionService
	maxRequestBody int64 // 请求体大小上限（字节）；0 表示不限制
}

// NewDecisionHandler 构建场景决策处理器。
// maxRequestBody 为请求体字节上限（须覆盖 base64 编码膨胀与 JSON 包装）。
func NewDecisionHandler(svc *service.DecisionService, maxRequestBody int64) *DecisionHandler {
	return &DecisionHandler{svc: svc, maxRequestBody: maxRequestBody}
}

// Evaluate 处理 POST /api/v1/image/decision。
//
// 请求体示例：
//
//	{ "image_url": "https://example.com/x.jpg" }
//	或 { "image_base64": "<base64 内容>" }
//
// 响应为统一结构 { code, msg, data }，data 只包含场景分类与风险评分：
//
//	{ "category": "porn", "score": 87 }
func (h *DecisionHandler) Evaluate(c *gin.Context) {
	var req model.DecisionRequest
	// 源头拦截超大请求体：在读取阶段即掐断，避免超大 base64 负载全量进入内存
	if h.maxRequestBody > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxRequestBody)
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body: "+err.Error())
		return
	}

	result, err := h.svc.Evaluate(c.Request.Context(), &req)
	if err != nil {
		// 统一错误转换：业务错误带码返回，其余按内部错误兜底
		helper.FailWithError(c, err)
		return
	}

	helper.OK(c, result)
}
