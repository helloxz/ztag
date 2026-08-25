// Package middleware 集中存放所有 HTTP 中间件：
//   - Recovery：panic 兜底，避免单次请求异常导致进程崩溃；
//   - AccessLog：请求访问日志；
//   - Auth：对外 API Key 鉴权；
//   - RateLimit：限流（当前为占位实现）。
//
// 所有中间件统一在 internal/router 中挂载，业务代码不感知。
package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/model"
)

// Recovery 捕获 handler 中抛出的 panic，返回统一的内部错误响应，
// 错误日志通过 slog.Error 落盘到 /data/logs/error-YYYY-MM-DD.log，不再打印终端。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// 记录堆栈信息（Warn/Error 级别 → 文件）
				slog.Error("[recovery] panic",
					"error", err,
					"stack", string(debug.Stack()),
					"path", c.Request.URL.Path,
					"method", c.Request.Method,
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code": model.CodeFailed,
					"msg":  "internal server error",
					"data": nil,
				})
			}
		}()
		c.Next()
	}
}
