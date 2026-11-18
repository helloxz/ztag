package handler

import (
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/webui"
)

// HomeHandler 根路径落地页：只做「服务运行中 + 项目地址」的静态展示。
type HomeHandler struct{}

// NewHomeHandler 构建落地页处理器。
func NewHomeHandler() *HomeHandler {
	return &HomeHandler{}
}

// Index 处理 GET /：返回内嵌的落地页。
// 刻意不放 /admin 入口，避免在公网首页暴露管理台地址。
func (h *HomeHandler) Index(c *gin.Context) {
	data, err := readWebFile("home.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "page not found")
		return
	}

	if webui.FromDisk() {
		c.Header("Cache-Control", "no-store")
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

// readWebFile 读取内嵌（或开发模式下磁盘上的）管理台静态文件。
func readWebFile(name string) ([]byte, error) {
	return fs.ReadFile(webui.FS, name)
}

// Favicon 处理 GET /favicon.ico：没有图标，直接返回 204，避免浏览器反复 404。
func (h *HomeHandler) Favicon(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
