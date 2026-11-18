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
	Health   *handler.HealthHandler   // 健康检查
	Home     *handler.HomeHandler     // 根路径落地页
	Image    *handler.ImageHandler    // 图片分析
	Decision *handler.DecisionHandler // 场景决策
	Admin    *handler.AdminHandler    // Web 管理台
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

	// 根路径落地页：只提示服务在运行，不暴露管理台入口
	r.GET("/", hs.Home.Index)
	// 没有图标，直接 204，避免浏览器反复 404
	r.GET("/favicon.ico", hs.Home.Favicon)

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
		// 风险审核（图片）：仅识别场景并给出 0~100 风险评分（决策模型）
		v1.POST("/image/decision", hs.Decision.Evaluate)
		// 风险审核（文本）：同一套决策模型，分类白名单与图片分开
		v1.POST("/text/decision", hs.Decision.EvaluateText)
	}

	// ============ Web 管理台（/admin） ============
	// 与对外 API 分开鉴权：管理台走登录会话（Cookie），不使用 API Key。
	admin := r.Group("/admin")
	{
		// 页面：未初始化 → /admin/init，未登录 → /admin/login，均由 handler 重定向
		admin.GET("", hs.Admin.ManagePage)
		admin.GET("/init", hs.Admin.InitPage)
		admin.GET("/login", hs.Admin.LoginPage)
		admin.GET("/assets/:name", hs.Admin.Asset)

		api := admin.Group("/api")
		{
			// 无需登录：状态探测、初始化、登录
			api.GET("/status", hs.Admin.Status)
			api.POST("/init", hs.Admin.Init)
			api.POST("/login", hs.Admin.Login)

			// 需要登录会话
			authed := api.Group("", middleware.AdminSession(hs.Admin.ValidSession))
			authed.POST("/logout", hs.Admin.Logout)
			authed.GET("/config", hs.Admin.GetConfig)
			// 按分区保存：只覆盖本分区的字段，其余配置保持不变
			authed.PUT("/config/base", hs.Admin.SaveBase)
			authed.PUT("/config/channel", hs.Admin.SaveChannels)
			authed.PUT("/config/decision", hs.Admin.SaveDecision)
			authed.PUT("/config/key", hs.Admin.SaveAuth)
			authed.POST("/password", hs.Admin.ChangePassword)
			// 重启：响应返回后进程退出，由容器编排拉起新实例
			authed.POST("/restart", hs.Admin.Restart)
		}
	}

	return r
}
