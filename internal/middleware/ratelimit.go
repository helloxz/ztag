package middleware

import "github.com/gin-gonic/gin"

// RateLimit 限流中间件（占位实现，当前仅透传）。
//
// 对外 API 必须配合限流，避免被恶意刷量。后续接入具体方案时
// 只需替换本函数的返回内容，挂载位置（router 中）无需改动：
//   - 单机限流：golang.org/x/time/rate 令牌桶；
//   - 分布式限流：Redis 计数器 / 滑动窗口（如 github.com/ulule/limiter/v3）。
//
// 若需要按调用方维度限流（如按 API Key / IP），可在 Auth 通过后
// 写入 c.Set("client_key", ...)，本中间件据此取值。
func RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}
