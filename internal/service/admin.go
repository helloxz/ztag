package service

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// AdminSessionTTL 管理台登录会话有效期。
const AdminSessionTTL = 12 * time.Hour

// 管理员密码长度限制（bcrypt 只处理前 72 字节，超长部分会被静默忽略，故显式限制）。
const (
	AdminPasswordMinLen = 6
	AdminPasswordMaxLen = 72
)

// AdminService Web 管理台业务编排：
//   - 管理员账号初始化 / 登录 / 会话管理；
//   - 运行时配置的读取与整体保存（写回 config.toml）；
//   - 修改管理员密码。
//
// 说明：配置保存后不会热加载，AI 渠道、密钥等改动需重启服务才生效；
// 但管理员账号与密码保存在本服务内存副本中，登录/改密无需重启即可生效。
type AdminService struct {
	mu       sync.Mutex
	cfg      config.Config        // 当前配置副本（保存成功后同步更新）
	path     string               // config.toml 路径（启动时 -config 指定的那个）
	sessions map[string]time.Time // 会话 token → 过期时间
}

// NewAdminService 构建管理台服务。cfg 为启动时加载的配置，path 为配置文件路径。
func NewAdminService(cfg *config.Config, path string) *AdminService {
	return &AdminService{
		cfg:      *cfg,
		path:     path,
		sessions: make(map[string]time.Time),
	}
}

// IsInitialized 管理员账号是否已完成初始化。
func (s *AdminService) IsInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Admin.IsInitialized()
}

// Config 返回当前配置副本（供管理台展示）。
func (s *AdminService) Config() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Init 首次初始化管理员账号并写回配置，成功后返回一个新的会话 token。
func (s *AdminService) Init(username, password string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", model.NewBizError("username must not be empty")
	}
	if err := validateAdminPassword(password); err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", model.NewBizError("failed to hash password")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.Admin.IsInitialized() {
		return "", model.NewBizError("admin account is already initialized")
	}

	candidate := s.cfg
	// 逐字段赋值，保留 enabled 等其它管理台配置（整体替换会把 enabled 重置为 false）
	candidate.Admin.Username = username
	candidate.Admin.PasswordHash = string(hash)
	if err := config.Save(s.path, &candidate); err != nil {
		return "", model.NewBizError(err.Error())
	}
	s.cfg = candidate

	token := s.createSessionLocked()
	if token == "" {
		return "", model.NewBizError("failed to create session")
	}
	return token, nil
}

// Login 校验管理员账号密码，成功后返回一个新的会话 token。
func (s *AdminService) Login(username, password string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Admin.IsInitialized() {
		return "", model.NewBizError("admin account is not initialized")
	}
	// 用户名不区分大小写；密码交给 bcrypt 校验哈希
	if !strings.EqualFold(strings.TrimSpace(username), s.cfg.Admin.Username) {
		return "", model.NewBizError("invalid username or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Admin.PasswordHash), []byte(password)); err != nil {
		return "", model.NewBizError("invalid username or password")
	}
	token := s.createSessionLocked()
	if token == "" {
		return "", model.NewBizError("failed to create session")
	}
	return token, nil
}

// Logout 销毁会话。
func (s *AdminService) Logout(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// ValidSession 判断会话 token 是否仍然有效（顺带清理过期会话）。
func (s *AdminService) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	expireAt, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expireAt) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// BaseSection 基础配置分区的可编辑字段。
// 只接收管理台展示的字段；temperature / top_p / mock / allow_private_urls
// 仅支持直接修改配置文件，这里即使传了也不会生效。
type BaseSection struct {
	Server config.ServerConfig `json:"server"`
	Log    config.LogConfig    `json:"log"`
	AI     config.AIConfig     `json:"ai"`
}

// ChannelSection 图像分析分区：聊天渠道列表 + 关闭思考模式的关键词。
type ChannelSection struct {
	Channels                 []config.ChannelConfig `json:"channels"`
	DisabledThinkingKeywords []string               `json:"disabled_thinking_keywords"`
}

// DecisionSection 场景决策分区：仅 ai.decision。
type DecisionSection struct {
	Decision config.DecisionConfig `json:"decision"`
}

// AuthSection 对外 API 鉴权分区：仅 auth。
type AuthSection struct {
	Auth config.AuthConfig `json:"auth"`
}

// SaveBase 保存基础配置（server / log / ai 标量），返回合并后的完整配置。
func (s *AdminService) SaveBase(p BaseSection) (config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg
	next.Server = p.Server
	next.Log = p.Log

	// 只覆盖管理台展示的 4 个标量。
	// temperature / top_p / mock / allow_private_urls 以及
	// disabled_thinking_keywords 不在这里处理：
	//   - 前 4 项仅支持改配置文件；
	//   - 关键词由「图像分析」分区（ChannelSection）维护。
	next.AI.Timeout = p.AI.Timeout
	next.AI.ImageTimeout = p.AI.ImageTimeout
	next.AI.MaxRetries = p.AI.MaxRetries
	next.AI.MaxImageBytes = p.AI.MaxImageBytes

	return s.saveLocked(&next)
}

// SaveChannels 保存渠道分区（default / backup）。
func (s *AdminService) SaveChannels(p ChannelSection) (config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg
	next.AI.Channels = p.Channels
	// nil 表示调用方未提交该字段，保持原值（空数组才是显式清空）
	if p.DisabledThinkingKeywords != nil {
		next.AI.DisabledThinkingKeywords = p.DisabledThinkingKeywords
	}
	return s.saveLocked(&next)
}

// SaveDecision 保存场景决策分区。
func (s *AdminService) SaveDecision(p DecisionSection) (config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg
	next.AI.Decision = p.Decision
	return s.saveLocked(&next)
}

// SaveAuth 保存对外 API 鉴权分区（开关 + 密钥列表）。
func (s *AdminService) SaveAuth(p AuthSection) (config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg
	next.Auth = p.Auth
	return s.saveLocked(&next)
}

// saveLocked 统一收口：保留管理员账号 → 整体校验 → 写回文件 → 更新内存副本。
// 校验仍针对合并后的完整配置，避免某个分区把配置改成下次启动加载不了的状态。
// 调用方必须持有 s.mu。
func (s *AdminService) saveLocked(next *config.Config) (config.Config, error) {
	// 账号信息只走 Init / ChangePassword，不随任何分区提交覆盖
	next.Admin = s.cfg.Admin

	if err := config.Save(s.path, next); err != nil {
		return config.Config{}, model.NewBizError(err.Error())
	}
	s.cfg = *next

	// 返回合并后的完整配置，前端据此同步界面
	return s.cfg, nil
}

// ChangePassword 校验旧密码后修改管理员密码，并返回新的会话 token
// （改密会使其余已有会话失效，当前页面用新 token 继续使用）。
func (s *AdminService) ChangePassword(current, next string) (string, error) {
	if err := validateAdminPassword(next); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Admin.IsInitialized() {
		return "", model.NewBizError("admin account is not initialized")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Admin.PasswordHash), []byte(current)); err != nil {
		return "", model.NewBizError("current password is incorrect")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return "", model.NewBizError("failed to hash password")
	}

	candidate := s.cfg
	candidate.Admin.PasswordHash = string(hash)
	if err := config.Save(s.path, &candidate); err != nil {
		return "", model.NewBizError(err.Error())
	}
	s.cfg = candidate

	// 改密后使其余会话全部失效，再为当前页面签发新会话
	s.sessions = make(map[string]time.Time)
	token := s.createSessionLocked()
	if token == "" {
		return "", model.NewBizError("failed to create session")
	}
	return token, nil
}

// createSessionLocked 生成会话 token（调用方需持有锁）。
func (s *AdminService) createSessionLocked() string {
	// 清理过期会话，避免 map 无限增长
	now := time.Now()
	for token, expireAt := range s.sessions {
		if now.After(expireAt) {
			delete(s.sessions, token)
		}
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见，退化仍返回 token（会话仅在内存，风险可控）
		return ""
	}
	token := hex.EncodeToString(buf)
	s.sessions[token] = now.Add(AdminSessionTTL)
	return token
}

// validateAdminPassword 校验密码长度。
func validateAdminPassword(password string) error {
	if len(password) < AdminPasswordMinLen {
		return model.NewBizError("password is too short (at least 6 characters)")
	}
	if len(password) > AdminPasswordMaxLen {
		return model.NewBizError("password is too long (at most 72 bytes)")
	}
	return nil
}
