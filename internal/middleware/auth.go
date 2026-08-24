package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// Auth 对外 API 鉴权中间件。
// 鉴权方式：请求头携带 API Key，
//   - Authorization: Bearer <key>
//   - 或 X-API-Key: <key>
//
// 仅当配置 auth.enabled = true 时才校验；未开启时直接放行，
// 方便本地联调与开发环境使用。
func Auth(cfg config.AuthConfig) gin.HandlerFunc {
	// 预置一张 key 查找表，避免每次请求遍历切片
	keySet := make(map[string]struct{}, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		keySet[k] = struct{}{}
	}

	return func(c *gin.Context) {
		if !cfg.Enabled {
			c.Next()
			return
		}

		// 从请求头提取 API Key
		key := extractAPIKey(c)
		if key == "" {
			abortUnauthorized(c)
			return
		}
		if _, ok := keySet[key]; !ok {
			abortUnauthorized(c)
			return
		}

		// 鉴权通过，继续后续处理
		c.Next()
	}
}

// extractAPIKey 依次尝试从 Authorization / X-API-Key 请求头提取 API Key。
func extractAPIKey(c *gin.Context) string {
	// 优先取 Authorization: Bearer <key>
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	// 其次取 X-API-Key
	return strings.TrimSpace(c.GetHeader("X-API-Key"))
}

// abortUnauthorized 输出统一的未授权响应（HTTP 401）并终止请求链。
// body 遵循统一契约：code=-1000，msg 为英文描述。
func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"code": model.CodeFailed,
		"msg":  "unauthorized or invalid API key",
		"data": nil,
	})
}
