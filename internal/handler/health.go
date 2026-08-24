package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/helper"
)

// HealthHandler 健康检查处理器，供探活（docker healthcheck / k8s probe）使用。
type HealthHandler struct{}

// NewHealthHandler 构建健康检查处理器。
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// Healthz 处理 GET /healthz，服务可正常处理请求时返回 ok。
// 该接口不经过鉴权与限流，便于负载均衡探活。
func (h *HealthHandler) Healthz(c *gin.Context) {
	helper.OK(c, gin.H{"status": "ok"})
}
