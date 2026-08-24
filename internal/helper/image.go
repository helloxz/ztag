package helper

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// 允许的图片 MIME 类型白名单（svg 含脚本风险，不纳入）。
var allowedImageMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
	"image/avif": true,
}

// 图片 URL 后缀 → MIME 兜底表（服务器不返回 Content-Type 时使用）。
var imageExtToMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".avif": "image/avif",
}

const (
	maxRedirects = 5 // 图片下载最大重定向次数
	acceptHeader = "image/webp,image/apng,image/*,*/*;q=0.8"
	userAgent    = "ZTAG-ImageModeration/1.0"
)

// ImageFetchOptions 图片探测/下载的参数。
type ImageFetchOptions struct {
	MaxBytes     int64         // 图片大小上限（字节），如 10MB
	AllowPrivate bool          // 是否放行内网/保留地址（SSRF 防护开关，生产保持 false）
	Timeout      time.Duration // 单次请求超时
}

// ValidateImageURL 校验图片 URL 的基本格式：
// 必须为 http/https 且携带主机名。仅做格式校验，不发起任何网络请求。
func ValidateImageURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return &urlFormatError{raw: raw}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("image URL must be http(s), got: %q", u.Scheme)
	}
	return nil
}

// urlFormatError 标识 URL 格式不合法（供上层统一映射错误消息）。
type urlFormatError struct{ raw string }

func (e *urlFormatError) Error() string { return "invalid image URL format" }

// IsInvalidURLError 判断错误是否为 URL 格式不合法。
func IsInvalidURLError(err error) bool {
	var e *urlFormatError
	return errors.As(err, &e)
}

// ProbeImage 发起 GET 请求校验图片响应头（不读取响应体，即不下载内容）：
//   - Content-Length 缺失或不大于 MaxBytes；
//   - Content-Type 为图片 MIME（缺失时按 URL 后缀兜底）。
//
// 校验不通过返回描述性错误，由调用方统一转换为业务错误。
func ProbeImage(raw string, opt ImageFetchOptions) (mimeType string, err error) {
	// SSRF 防护：初始请求 URL 同样校验（重定向目标由 CheckRedirect 兜底）
	if err := checkURLAllowed(raw, opt.AllowPrivate); err != nil {
		return "", err
	}
	client := newImageClient(opt)
	resp, err := client.Get(raw)
	if err != nil {
		return "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close() // 只读响应头，立即关闭不下载内容

	return validateImageResponse(resp, raw, opt.MaxBytes)
}

// FetchImageAsDataURI 下载图片并转换为 data URI（如 data:image/jpeg;base64,...）。
// 下载前会复用头部校验，防止「探测后内容被替换/放大」的 TOCTOU 风险；
// 读取 body 时按 MaxBytes 硬性截断保护，杜绝响应头虚报大小。
func FetchImageAsDataURI(raw string, opt ImageFetchOptions) (dataURI string, mimeType string, err error) {
	// SSRF 防护：初始请求 URL 同样校验（重定向目标由 CheckRedirect 兜底）
	if err := checkURLAllowed(raw, opt.AllowPrivate); err != nil {
		return "", "", err
	}
	client := newImageClient(opt)
	resp, err := client.Get(raw)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	mimeType, err = validateImageResponse(resp, raw, opt.MaxBytes)
	if err != nil {
		return "", "", err
	}

	// 限制读取：超过 MaxBytes+1 字节即判定超限（防响应头虚报）
	body, err := io.ReadAll(io.LimitReader(resp.Body, opt.MaxBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("failed to read image body: %w", err)
	}
	if int64(len(body)) > opt.MaxBytes {
		return "", "", fmt.Errorf("image too large (exceeds %d bytes)", opt.MaxBytes)
	}

	encoded := base64.StdEncoding.EncodeToString(body)
	return "data:" + mimeType + ";base64," + encoded, mimeType, nil
}

// newImageClient 构造用于图片请求的 HTTP 客户端：
// 限制单请求超时，并对每一跳重定向执行 SSRF 校验与次数上限。
func newImageClient(opt ImageFetchOptions) *http.Client {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			// 重定向目标同样校验，防止跳转到内网
			return checkURLAllowed(req.URL.String(), opt.AllowPrivate)
		},
	}
}

// validateImageResponse 校验响应头：状态码、大小（Content-Length）、MIME 类型。
// 返回最终确定的图片 MIME 类型（供 base64 data URI 使用）。
func validateImageResponse(resp *http.Response, raw string, maxBytes int64) (string, error) {
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("failed to fetch image (HTTP %d)", resp.StatusCode)
	}

	// 1. 大小校验：Content-Length 缺失（如 chunked）或超过上限直接拒绝
	switch {
	case resp.ContentLength < 0:
		return "", errors.New("image size unknown (missing Content-Length)")
	case resp.ContentLength > maxBytes:
		return "", fmt.Errorf("image too large (exceeds %d bytes)", maxBytes)
	}

	// 2. MIME 校验：Content-Type 优先，缺失时按 URL 后缀兜底
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mimeType, _, _ := mime.ParseMediaType(ct)
		if mimeType == "" {
			mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
		}
		if allowedImageMIME[mimeType] {
			return mimeType, nil
		}
		return "", fmt.Errorf("not an image (Content-Type: %q)", ct)
	}

	// Content-Type 缺失：按 URL 后缀兜底判断
	u, _ := url.Parse(raw)
	ext := strings.ToLower(filepath.Ext(u.Path))
	if m, ok := imageExtToMIME[ext]; ok {
		return m, nil
	}
	return "", fmt.Errorf("unrecognized image type (Content-Type missing, suffix %q)", ext)
}

// checkURLAllowed SSRF 防护：拦截内网 / 保留地址等危险目标。
// 入参 raw 为图片 URL 字符串；校验方式：解析主机名 IP（域名也做解析），
// 任一 IP 落在私网/保留段即拒绝。
func checkURLAllowed(raw string, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid image URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http(s), got: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url host is empty")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve host %q failed: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("url target %q is not allowed (private/reserved address)", host)
		}
	}
	return nil
}

// isBlockedIP 判断 IP 是否为私网/保留地址（SSRF 黑名单）。
func isBlockedIP(ip net.IP) bool {
	// 覆盖：环回、RFC1918 私网、链路本地、未指定、多播及保留段
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}
