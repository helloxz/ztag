package handler

import (
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/helper"
	"github.com/helloxz/ztag/internal/middleware"
	"github.com/helloxz/ztag/internal/model"
	"github.com/helloxz/ztag/internal/service"
	"github.com/helloxz/ztag/internal/version"
	"github.com/helloxz/ztag/internal/webui"
)

// AdminHandler 管理台表现层：页面路由 + 管理 JSON 接口。
// 业务逻辑全部在 service.AdminService，这里只做参数解析、Cookie 下发与响应封装。
type AdminHandler struct {
	svc        *service.AdminService
	docs       *service.DocsService
	logs       *service.LogsService
	restart    func()      // 触发进程优雅退出（由 main 注入，退出后由容器编排拉起）
	restarting atomic.Bool // 防止并发重复重启
}

// NewAdminHandler 构建管理台处理器。restart 为空表示当前运行环境不支持重启。
func NewAdminHandler(svc *service.AdminService, docs *service.DocsService, logs *service.LogsService, restart func()) *AdminHandler {
	return &AdminHandler{svc: svc, docs: docs, logs: logs, restart: restart}
}

// ValidSession 供管理台会话中间件复用（避免中间件反向依赖 service 包）。
func (h *AdminHandler) ValidSession(token string) bool {
	return h.svc.ValidSession(token)
}

// ---------- 页面 ----------

// InitPage 处理 GET /admin/init：未初始化时展示初始化页面。
func (h *AdminHandler) InitPage(c *gin.Context) {
	if h.svc.IsInitialized() {
		c.Redirect(http.StatusFound, "/admin/login")
		return
	}
	h.page(c, "init.html")
}

// LoginPage 处理 GET /admin/login：已初始化时展示登录页面。
func (h *AdminHandler) LoginPage(c *gin.Context) {
	if !h.svc.IsInitialized() {
		c.Redirect(http.StatusFound, "/admin/init")
		return
	}
	if h.sessionValid(c) {
		c.Redirect(http.StatusFound, "/admin")
		return
	}
	h.page(c, "login.html")
}

// ManagePage 处理 GET /admin：登录后展示管理页面。
func (h *AdminHandler) ManagePage(c *gin.Context) {
	if !h.svc.IsInitialized() {
		c.Redirect(http.StatusFound, "/admin/init")
		return
	}
	if !h.sessionValid(c) {
		c.Redirect(http.StatusFound, "/admin/login")
		return
	}
	h.page(c, "manage.html")
}

// Asset 处理 GET /admin/assets/:name：返回 CSS / JS（内嵌或开发模式下的磁盘文件）。
func (h *AdminHandler) Asset(c *gin.Context) {
	name := c.Param("name")
	// 只允许访问 web 目录下的普通文件名，防止路径穿越
	if name == "" || name != path.Base(name) || strings.Contains(name, "..") {
		c.Status(http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(webui.FS, name)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if webui.FromDisk() {
		// 开发模式：禁止缓存，改完样式刷新即可
		c.Header("Cache-Control", "no-store")
	}
	contentType := "application/octet-stream"
	switch path.Ext(name) {
	case ".css":
		contentType = "text/css; charset=utf-8"
	case ".js":
		contentType = "application/javascript; charset=utf-8"
	}
	c.Data(http.StatusOK, contentType, data)
}

// ---------- JSON 接口 ----------

// Status 处理 GET /admin/api/status：告知前端是否需要初始化、是否已登录。
func (h *AdminHandler) Status(c *gin.Context) {
	initialized := h.svc.IsInitialized()
	data := gin.H{
		"initialized":   initialized,
		"authenticated": initialized && h.sessionValid(c),
		"version":       version.Version, // 侧栏底部展示当前版本
	}
	if initialized {
		data["username"] = h.svc.Config().Admin.Username
	}
	helper.OK(c, data)
}

// adminCredentialRequest 初始化 / 登录共用的请求体。
type adminCredentialRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Init 处理 POST /admin/api/init：首次设置管理员账号密码，成功后自动登录。
func (h *AdminHandler) Init(c *gin.Context) {
	var req adminCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	token, err := h.svc.Init(req.Username, req.Password)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	h.setSessionCookie(c, token)
	helper.OK(c, gin.H{"initialized": true})
}

// Login 处理 POST /admin/api/login。
func (h *AdminHandler) Login(c *gin.Context) {
	var req adminCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	token, err := h.svc.Login(req.Username, req.Password)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	h.setSessionCookie(c, token)
	helper.OK(c, gin.H{"authenticated": true})
}

// Logout 处理 POST /admin/api/logout。
func (h *AdminHandler) Logout(c *gin.Context) {
	if token, err := c.Cookie(middleware.AdminSessionCookieName); err == nil {
		h.svc.Logout(token)
	}
	h.clearSessionCookie(c)
	helper.OK(c, gin.H{"authenticated": false})
}

// GetConfig 处理 GET /admin/api/config：返回当前配置（密码哈希不外发）。
func (h *AdminHandler) GetConfig(c *gin.Context) {
	helper.OK(c, h.svc.Config())
}

// Docs 处理 GET /admin/api/docs：返回缓存后的 API 文档 Markdown。
// 文档抓取与 24 小时缓存由 service.DocsService 负责。
func (h *AdminHandler) Docs(c *gin.Context) {
	if h.docs == nil {
		helper.Fail(c, "API docs service is not configured")
		return
	}
	doc, err := h.docs.Get(c.Request.Context())
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	// 缓存由服务端管理，禁止浏览器再缓存一份
	c.Header("Cache-Control", "no-store")
	helper.OK(c, doc)
}

// Logs 处理 GET /admin/api/logs：返回最新日志文件尾部若干行。
// limit 为期望行数（缺省 200，最大 500），只倒读文件尾部，不解析日志格式。
func (h *AdminHandler) Logs(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			limit = v
		}
	}
	if h.logs == nil {
		helper.Fail(c, "logs service is not configured")
		return
	}
	res, err := h.logs.Tail(limit)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	// 日志内容随运行变化，禁止浏览器缓存
	c.Header("Cache-Control", "no-store")
	helper.OK(c, res)
}

// SaveBase 处理 PUT /admin/api/config/base：只保存基础配置分区。
func (h *AdminHandler) SaveBase(c *gin.Context) {
	var req service.BaseSection
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	saved, err := h.svc.SaveBase(req)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	helper.OK(c, saved)
}

// SaveChannels 处理 PUT /admin/api/config/channel：只保存渠道分区。
func (h *AdminHandler) SaveChannels(c *gin.Context) {
	var req service.ChannelSection
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	saved, err := h.svc.SaveChannels(req)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	helper.OK(c, saved)
}

// SaveDecision 处理 PUT /admin/api/config/decision：只保存场景决策分区。
func (h *AdminHandler) SaveDecision(c *gin.Context) {
	var req service.DecisionSection
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	saved, err := h.svc.SaveDecision(req)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	helper.OK(c, saved)
}

// SaveAuth 处理 PUT /admin/api/config/key：只保存鉴权与密钥分区。
func (h *AdminHandler) SaveAuth(c *gin.Context) {
	var req service.AuthSection
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	saved, err := h.svc.SaveAuth(req)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	helper.OK(c, saved)
}

// adminPasswordRequest 修改密码请求体。
type adminPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword 处理 POST /admin/api/password：改密后下发新会话。
func (h *AdminHandler) ChangePassword(c *gin.Context) {
	var req adminPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helper.Fail(c, "invalid request body")
		return
	}
	token, err := h.svc.ChangePassword(req.CurrentPassword, req.NewPassword)
	if err != nil {
		helper.FailWithError(c, err)
		return
	}
	h.setSessionCookie(c, token)
	helper.OK(c, gin.H{"changed": true})
}

// Restart 处理 POST /admin/api/restart：先确认响应已写回，再触发进程优雅退出。
//
// 进程退出码为 0，依赖 docker / k8s 的自动重启策略拉起新实例；
// 裸进程部署点击后服务会直接停止，前端会在确认弹窗里提示这一点。
func (h *AdminHandler) Restart(c *gin.Context) {
	if h.restart == nil {
		helper.Fail(c, "restart is not available")
		return
	}
	if !h.restarting.CompareAndSwap(false, true) {
		c.JSON(http.StatusConflict, gin.H{
			"code": model.CodeFailed,
			"msg":  "restart already in progress",
			"data": nil,
		})
		return
	}

	helper.OK(c, gin.H{"restarting": true})

	go func() {
		// 留一点时间把响应刷回浏览器
		time.Sleep(500 * time.Millisecond)
		h.restart()
	}()
}

// ---------- 内部助手 ----------

// page 输出管理台 HTML 页面（内嵌资源，或开发模式下的磁盘文件）。
func (h *AdminHandler) page(c *gin.Context, name string) {
	data, err := fs.ReadFile(webui.FS, name)
	if err != nil {
		c.String(http.StatusInternalServerError, "admin page not found")
		return
	}
	if webui.FromDisk() {
		// 开发模式：禁止缓存，改完刷新即可看到变化
		c.Header("Cache-Control", "no-store")
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

// sessionValid 判断当前请求携带的管理台会话是否有效。
func (h *AdminHandler) sessionValid(c *gin.Context) bool {
	token, err := c.Cookie(middleware.AdminSessionCookieName)
	if err != nil {
		return false
	}
	return h.svc.ValidSession(token)
}

// setSessionCookie 下发管理台会话 Cookie（HttpOnly + SameSite=Lax）。
// 反代终止 TLS 时按 X-Forwarded-Proto 判断 Secure，避免 HTTPS 下漏发 Secure 属性。
func (h *AdminHandler) setSessionCookie(c *gin.Context, token string) {
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(
		middleware.AdminSessionCookieName,
		token,
		int(service.AdminSessionTTL.Seconds()),
		"/admin",
		"",
		secure,
		true,
	)
}

// clearSessionCookie 清除管理台会话 Cookie。
func (h *AdminHandler) clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(middleware.AdminSessionCookieName, "", -1, "/admin", "", false, true)
}
