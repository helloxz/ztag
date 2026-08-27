// Package config 负责应用配置的加载、默认值填充与合法性校验。
//
// 配置设计：
//   - 运行时配置统一放在 data/config.toml（便于 docker 挂载 data 目录持久化）；
//   - 默认配置模板通过 go:embed 内嵌进二进制，首次启动自动拷贝到 data/ 下；
//   - 支持配置多个后端大模型渠道（channel），每个渠道内可配置多个模型（models 数组）。
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

//go:embed default.toml
var defaultConfig []byte // 内嵌的默认配置模板（首次启动拷贝到 data/ 目录）

// Config 应用全局配置根结构。
type Config struct {
	Server ServerConfig `mapstructure:"server"`
	Log    LogConfig    `mapstructure:"log"`
	Auth   AuthConfig   `mapstructure:"auth"`
	AI     AIConfig     `mapstructure:"ai"`
}

// ServerConfig HTTP 服务配置。
type ServerConfig struct {
	Addr    string `mapstructure:"addr"`    // HTTP 监听地址，如 ":8080"
	Mode    string `mapstructure:"mode"`    // gin 运行模式：debug | release
	Workers int    `mapstructure:"workers"` // 并发工作者数（同时进行的重内存分析任务数），未配置默认 4，最小 1
}

// LogConfig 日志配置。
type LogConfig struct {
	Level string `mapstructure:"level"` // 日志级别：debug | info | warn | error
}

// AuthConfig 对外 API 鉴权配置。
type AuthConfig struct {
	Enabled bool     `mapstructure:"enabled"`  // 是否开启 API Key 鉴权
	APIKeys []string `mapstructure:"api_keys"` // 允许访问的 API Key 列表
}

// AIConfig 多后端大模型渠道配置。
// 固定支持两个渠道 id：default（默认/主渠道）与 backup（备用渠道），
// 其中 default 必填，backup 可选。
type AIConfig struct {
	Timeout                  int             `mapstructure:"timeout"`                    // 单次 AI 调用超时（秒）
	ImageTimeout             int             `mapstructure:"image_timeout"`              // 图片下载超时（秒），缺省/非法时回退 30s
	MaxRetries               int             `mapstructure:"max_retries"`                // 单渠道内失败重试次数
	Mock                     bool            `mapstructure:"mock"`                       // 是否启用模拟 AI（本地无 API Key 联调用）
	MaxImageBytes            int64           `mapstructure:"max_image_bytes"`            // 图片大小上限（字节），默认 10MB
	AllowPrivateURLs         bool            `mapstructure:"allow_private_urls"`         // 是否放行内网/保留地址图片（SSRF 防护，生产保持 false）
	Temperature              float64         `mapstructure:"temperature"`                // 采样温度（0~2，越低输出越稳定，默认 0.2）
	TopP                     float64         `mapstructure:"top_p"`                      // 核采样（0~1，控制随机性，默认 0.9）
	DisabledThinkingKeywords []string        `mapstructure:"disabled_thinking_keywords"` // 命中即显式关闭思考的模型关键词列表（如 qwen/mimo/deepseek）
	Channels                 []ChannelConfig `mapstructure:"channels"`                   // 渠道列表（id 仅允许 default / backup）
}

// 渠道 id 常量（固定主备双渠道）。
const (
	ChannelIDDefault = "default" // 默认渠道（必填）
	ChannelIDBackup  = "backup"  // 备用渠道（可选）
)

// 渠道协议类型常量（决定 AI 调用的接口形态）。
const (
	ChannelTypeOpenAIChat     = "openai-chat"     // OpenAI Chat Completions 兼容接口
	ChannelTypeOpenAIResponse = "openai-response" // OpenAI Responses 兼容接口
	ChannelTypeAnthropic      = "anthropic"       // Anthropic Messages 兼容接口（含阿里云等）
)

// ChannelConfig 单个 AI 渠道（对应一个后端大模型服务商或兼容网关）。
type ChannelConfig struct {
	ID      string   `mapstructure:"id"`       // 渠道唯一标识：default | backup
	Type    string   `mapstructure:"type"`     // 协议类型：openai-chat | openai-response | anthropic
	BaseURL string   `mapstructure:"base_url"` // 接口基础地址
	APIKey  string   `mapstructure:"api_key"`  // API Key
	Models  []string `mapstructure:"models"`   // 该渠道下的模型列表（模型标识字符串数组）
}

// DefaultDataDir 运行时数据目录（配置、后续的日志/缓存等都放这里，便于 docker 挂载）。
const DefaultDataDir = "data"

// DefaultConfigPath 返回默认运行时配置文件路径：data/config.toml。
func DefaultConfigPath() string {
	return filepath.Join(DefaultDataDir, "config.toml")
}

// EnsureDataConfig 确保 data 目录下的配置文件存在：
//   - 若已存在，直接返回其路径（不对用户配置做任何覆盖）；
//   - 若不存在，创建 data 目录并把内嵌的默认模板拷贝过去。
//
// 返回最终生效的配置文件路径。
func EnsureDataConfig() (string, error) {
	path := DefaultConfigPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	// 创建 data 目录（含父级目录）
	if err := os.MkdirAll(DefaultDataDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}

	// 写入默认配置模板
	if err := os.WriteFile(path, defaultConfig, 0o644); err != nil {
		return "", fmt.Errorf("failed to write default config: %w", err)
	}
	return path, nil
}

// 默认图片大小上限：10MB（与需求对齐）
const DefaultMaxImageBytes int64 = 10 * 1024 * 1024

// DefaultDisabledThinkingKeywords 默认「命中即显式关闭思考」的模型关键词列表。
// 与 default.toml 模板保持一致；配置文件缺失该键或置空时按此兜底（与旧硬编码行为等价）。
var DefaultDisabledThinkingKeywords = []string{
	"qwen",     // Qwen3 系列（含 -Thinking 后缀变体）
	"mimo",     // MiMo 系列（默认开启思考）
	"deepseek", // DeepSeek 系列（R1/思考模式）
}

// Load 从指定路径加载 TOML 配置，填充默认值并做合法性校验。
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("toml")
	v.SetConfigFile(path)

	// 设置默认值（配置文件中缺失的键使用这些值）
	v.SetDefault("server.addr", ":8080")
	v.SetDefault("server.mode", "debug")
	v.SetDefault("server.workers", 4)
	v.SetDefault("log.level", "info")
	v.SetDefault("ai.timeout", 60)
	v.SetDefault("ai.image_timeout", 30)
	v.SetDefault("ai.max_retries", 2)
	v.SetDefault("ai.max_image_bytes", DefaultMaxImageBytes)
	v.SetDefault("ai.temperature", 0.2)
	v.SetDefault("ai.top_p", 0.9)
	v.SetDefault("ai.disabled_thinking_keywords", DefaultDisabledThinkingKeywords)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	// 图片下载超时与 AI 超时分离：未配置/非法值回退 30s，不干扰 AI 超时
	if cfg.AI.ImageTimeout <= 0 {
		cfg.AI.ImageTimeout = 30
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return &cfg, nil
}

// validate 校验配置的必填项与取值范围。
func (c *Config) validate() error {
	// 服务基础配置
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr must not be empty")
	}
	switch c.Server.Mode {
	case "debug", "release", "test":
	default:
		return fmt.Errorf("server.mode must be one of debug / release / test, got: %q", c.Server.Mode)
	}

	// 渠道配置校验：id 仅允许 default / backup，其中 default 必填、backup 可选
	if len(c.AI.Channels) == 0 {
		return fmt.Errorf("ai.channels must not be empty (at least one channel with id=\"default\" required)")
	}

	seen := make(map[string]bool, len(c.AI.Channels)) // 已出现的渠道 id，用于查重
	hasDefault := false                               // default 渠道是否已配置
	for i := range c.AI.Channels {
		ch := &c.AI.Channels[i]

		// 渠道 id 校验：只允许固定主备两个取值
		switch ch.ID {
		case ChannelIDDefault:
			hasDefault = true
		case ChannelIDBackup:
		default:
			return fmt.Errorf("ai.channels[%d].id must be \"default\" or \"backup\", got: %q", i, ch.ID)
		}
		if seen[ch.ID] {
			return fmt.Errorf("channel id %q is duplicated", ch.ID)
		}
		seen[ch.ID] = true

		switch ch.Type {
		case ChannelTypeOpenAIChat, ChannelTypeOpenAIResponse, ChannelTypeAnthropic:
		default:
			return fmt.Errorf("channel %q type must be one of openai-chat / openai-response / anthropic, got: %q", ch.ID, ch.Type)
		}
		if ch.BaseURL == "" {
			return fmt.Errorf("channel %q base_url must not be empty", ch.ID)
		}
		if len(ch.Models) == 0 {
			return fmt.Errorf("channel %q must have at least one model (models array must not be empty)", ch.ID)
		}
		for j, m := range ch.Models {
			if m == "" {
				return fmt.Errorf("channel %q models[%d] must not be empty", ch.ID, j)
			}
		}
	}

	// 默认渠道（id=default）必须存在，备用渠道（backup）允许缺省
	if !hasDefault {
		return fmt.Errorf("ai.channels must include a channel with id=\"default\"")
	}
	return nil
}
