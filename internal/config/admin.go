package config

// AdminConfig Web 管理台（/admin）管理员账号配置。
//
// 单用户模型：只保存一个用户名与密码哈希（bcrypt，不可逆）。
// 两字段任一为空都视为「未初始化」，此时访问 /admin 会进入初始化页面；
// 初始化完成后由管理台自行写回 config.toml。
type AdminConfig struct {
	Username     string `mapstructure:"username" toml:"username" json:"username"`    // 管理员用户名
	PasswordHash string `mapstructure:"password_hash" toml:"password_hash" json:"-"` // 密码哈希（bcrypt），永不返回给前端
}

// IsInitialized 管理员账号是否已完成初始化（用户名与密码哈希都已设置）。
func (a AdminConfig) IsInitialized() bool {
	return a.Username != "" && a.PasswordHash != ""
}
