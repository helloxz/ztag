package ai

import (
	"fmt"

	"github.com/helloxz/ztag/internal/config"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/anthropic"
	"github.com/zendev-sh/goai/provider/openai"
)

// buildModel 按渠道协议构造 goai 语言模型（统一接口 provider.LanguageModel）：
//   - openai-chat / openai-response → goai provider/openai（内部自动适配 Chat / Responses 协议）
//   - anthropic（含阿里云等兼容网关）→ goai provider/anthropic
//
// 模型实例只做一次构建，请求级参数（超时/重试/prompt）在调用时通过 goai Option 传入。
func buildModel(ch *config.ChannelConfig, modelName string) (provider.LanguageModel, error) {
	switch ch.Type {
	case config.ChannelTypeOpenAIChat, config.ChannelTypeOpenAIResponse:
		return openai.Chat(modelName,
			openai.WithBaseURL(ch.BaseURL),
			openai.WithAPIKey(ch.APIKey)), nil
	case config.ChannelTypeAnthropic:
		return anthropic.Chat(modelName,
			anthropic.WithBaseURL(ch.BaseURL),
			anthropic.WithAPIKey(ch.APIKey)), nil
	}
	// 配置校验已限定 type，此处兜底防御
	return nil, fmt.Errorf("unsupported channel type %q", ch.Type)
}
