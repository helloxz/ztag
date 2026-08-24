package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// AccessLog 记录每个请求的访问日志：客户端 IP、方法、路径、状态码与耗时。
// 当前使用标准库 slog 输出；后续如需接入日志系统/链路追踪，替换本函数内部实现即可。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// 先放行请求，待 handler 处理完成后统一记录
		c.Next()

		slog.Info("access log",
			"ip", c.ClientIP(),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
		)
	}
}
