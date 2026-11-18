package ai

import (
	"math/rand/v2"

	"github.com/helloxz/ztag/internal/config"
)

// apiKeyPool 决策渠道的 API Key 池。
//
// 配置里的 api_key 支持用 "|" 分隔多个 Key；每次调用随机取一个，
// 便于在多个配额/账号之间分摊请求。池在构建客户端时解析完成，之后只读，
// 可被多个请求并发复用。
type apiKeyPool []string

// newAPIKeyPool 从配置原文解析 Key 池（去两侧空白、丢弃空段）。
func newAPIKeyPool(raw string) apiKeyPool {
	return apiKeyPool(config.SplitAPIKeys(raw))
}

// pick 随机返回一个 Key：单 Key 直接返回，空池返回空字符串。
//
// 空池不会出现在正常流程里（配置校验要求至少一个非空 Key），
// 这里返回空串只是让请求按缺省鉴权失败处理，而不是 panic。
func (p apiKeyPool) pick() string {
	if len(p) == 0 {
		return ""
	}
	if len(p) == 1 {
		return p[0]
	}
	return p[rand.IntN(len(p))]
}
