package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/model"
)

// AdminSessionCookieName 管理台登录会话的 Cookie 名称。
const AdminSessionCookieName = "ztag_admin_session"

// AdminSession 管理台会话校验中间件。
// 仅用于 /admin/api 下的管理接口：未登录直接返回 HTTP 401 + 统一契约体，
// 前端据此跳转登录页；页面路由的跳转由 handler 自行处理。
func AdminSession(valid func(token string) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, _ := c.Cookie(AdminSessionCookieName)
		if !valid(token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code": model.CodeFailed,
				"msg":  "admin session expired, please login again",
				"data": nil,
			})
			return
		}
		c.Next()
	}
}
