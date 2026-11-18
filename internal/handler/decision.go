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

// maxTextRequestBody 文本审核请求体上限（文本本身很小，留足 JSON 余量即可）。
const maxTextRequestBody = 64 << 10

// Evaluate 处理 POST /api/v1/image/decision。
//
// 请求体示例：
//
//	{ "image_url": "https://example.com/x.jpg" }
//	或 { "image_base64": "<base64 内容>" }
//	可选 { "model": "gemini-main" } 指定决策渠道，缺省按配置顺序轮询
//
// 响应为统一结构 { code, msg, data }，data 包含场景分类、风险评分、实际渠道与总耗时：
//
//	{ "category": "porn", "score": 87, "model_id": "gemini-main", "elapsed_ms": 312 }
func (h *DecisionHandler) Evaluate(c *gin.Context) {
	// 记录请求起始时间，用于计算总处理耗时（毫秒）
	start := time.Now()

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

	// 填充请求总耗时（含请求解析、信号量排队、图片下载/压缩、模型调用）
	result.ElapsedMs = time.Since(start).Milliseconds()
	helper.OK(c, result)
}

// EvaluateText 处理 POST /api/v1/text/decision。
//
// 请求体示例：
//
//	{ "text": "加微信 xxx 领福利" }
//	可选 { "model": "openai-main" } 指定决策渠道
//
// 文本与图片使用各自独立的分类白名单，响应结构一致（含 model_id / elapsed_ms）。
func (h *DecisionHandler) EvaluateText(c *gin.Context) {
	start := time.Now()

	var req model.TextDecisionRequest
	// 文本请求体很小，单独限制，避免复用图片接口的大上限
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTextRequestBody)
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body: "+err.Error())
		return
	}

	result, err := h.svc.EvaluateText(c.Request.Context(), &req)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}

	result.ElapsedMs = time.Since(start).Milliseconds()
	helper.OK(c, result)
}
