// Package router 路由统一集中管理：项目内所有 HTTP 路由
// 只在 SetupRouter 中注册，中间件的挂载顺序也在这里统一控制。
package router

import (
	"net/http"
	_ "net/http/pprof" // 注册 pprof handler 到 http.DefaultServeMux（仅 debug 模式挂载使用）

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/handler"
	"github.com/helloxz/ztag/internal/middleware"
)

// Handlers 聚合所有业务 handler，避免 SetupRouter 参数过多。
// 新增业务模块时，在此追加对应 handler 字段并在 main 中注入。
type Handlers struct {
	Health *handler.HealthHandler // 健康检查
	Image  *handler.ImageHandler  // 图片分析
}

// SetupRouter 构建并返回配置好的 gin 引擎，注册全部路由与中间件。
//
// 中间件挂载策略：
//   - 全局（Recovery / AccessLog）：所有请求都经过；
//   - 业务分组（Auth / RateLimit）：只挂在 /api/v1 对外接口上，
//     健康检查不鉴权，便于探活。
func SetupRouter(cfg *config.Config, hs *Handlers) *gin.Engine {
	gin.SetMode(cfg.Server.Mode)

	r := gin.New()
	r.Use(
		middleware.Recovery(),  // panic 兜底
		middleware.AccessLog(), // 访问日志
	)

	// 健康检查探活接口：不鉴权、不限流
	r.GET("/healthz", hs.Health.Healthz)

	// 仅 debug 模式挂载 pprof 监控端点（heap/goroutine/profile 等），release 不暴露。
	// 复用 Auth 中间件，防止误配 debug 上线时 pprof 未鉴权裸奔。
	if cfg.Server.Mode == "debug" {
		pprofGroup := r.Group("/debug/pprof", middleware.Auth(cfg.Auth))
		pprofGroup.GET("", gin.WrapH(http.DefaultServeMux))
		pprofGroup.GET("/*any", gin.WrapH(http.DefaultServeMux))
	}

	// ============ 对外 API v1 分组 ============
	v1 := r.Group("/api/v1",
		middleware.Auth(cfg.Auth), // API Key 鉴权（可配置开关）
		middleware.RateLimit(),    // 限流（当前占位透传）
	)
	{
		// 图片分析：识别内容、提取关键词/描述并分类
		v1.POST("/image/analyze", hs.Image.Analyze)
	}

	return r
}
