package config

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Save 把完整配置写回指定的 TOML 文件（覆盖式写入）。
//
// 设计取舍：
//   - 用 go-toml 直接从结构体序列化（它就是 viper 的 TOML 后端，非新增依赖），
//     写出的键与结构体 tag 一一对应，不会漏字段；
//   - 覆盖式写入不保留原文件注释——管理台改过的配置以结构体为准；
//   - 写盘前用同一套 Validate 规则校验，拒绝写入会导致下次启动失败的配置；
//   - 文件权限设为 0600：其中包含密码哈希与各渠道 API Key。
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", path, err)
	}
	// WriteFile 的权限位只对新建文件生效；已有文件（如默认模板生成的 0644）
	// 需显式收紧，因为其中包含密码哈希与各渠道 API Key。
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("failed to chmod config file %s: %w", path, err)
	}
	return nil
}
