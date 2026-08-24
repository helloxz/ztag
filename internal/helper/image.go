package helper

import (
	"context"
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
	maxRedirects    = 5 // 图片下载最大重定向次数
	acceptHeader    = "image/webp,image/apng,image/*,*/*;q=0.8"
	chromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// originOf 提取 URL 的源（scheme://host），用作图片请求的 Referer 头。
// 模拟浏览器「从图片自身域名发起请求」的行为，降低被图片源站反爬拦截的风险。
// 例：https://s3.bmp.ovh/2026/08/23/ElpjG6BK.png → https://s3.bmp.ovh
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

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
// 请求携带浏览器特征（Chrome UA、Accept、Referer=图片自身域名），降低被源站拦截风险。
// 校验不通过返回描述性错误，由调用方统一转换为业务错误。
func ProbeImage(ctx context.Context, raw string, opt ImageFetchOptions) (mimeType string, err error) {
	// SSRF 防护：初始请求 URL 同样校验（重定向目标由 CheckRedirect 兜底）
	if err := checkURLAllowed(raw, opt.AllowPrivate); err != nil {
		return "", err
	}
	client := newImageClient(opt)
	req, err := newImageRequest(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("invalid image URL: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close() // 只读响应头，立即关闭不下载内容

	return validateImageResponse(resp, raw, opt.MaxBytes)
}

// FetchImage 下载图片并返回原始字节与 MIME 类型。
// 下载前会复用头部校验，防止「探测后内容被替换/放大」的 TOCTOU 风险；
// 读取 body 时按 MaxBytes 硬性截断保护，杜绝响应头虚报大小。
// 返回的原始字节交给 CompressImageForAI / EncodeDataURI 做本地预处理。
func FetchImage(ctx context.Context, raw string, opt ImageFetchOptions) (data []byte, mimeType string, err error) {
	// SSRF 防护：初始请求 URL 同样校验（重定向目标由 CheckRedirect 兜底）
	if err := checkURLAllowed(raw, opt.AllowPrivate); err != nil {
		return nil, "", err
	}
	client := newImageClient(opt)
	req, err := newImageRequest(ctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("invalid image URL: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	mimeType, err = validateImageResponse(resp, raw, opt.MaxBytes)
	if err != nil {
		return nil, "", err
	}

	// 限制读取：超过 MaxBytes+1 字节即判定超限（防响应头虚报）
	body, err := io.ReadAll(io.LimitReader(resp.Body, opt.MaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image body: %w", err)
	}
	if int64(len(body)) > opt.MaxBytes {
		return nil, "", fmt.Errorf("image too large (exceeds %d bytes)", opt.MaxBytes)
	}
	return body, mimeType, nil
}

// newImageRequest 构造带浏览器特征的 GET 请求：
//   - User-Agent：模拟 Chrome，降低被源站反爬拦截的概率；
//   - Referer：取图片自身域名（scheme://host），模拟同源发起的图片请求；
//   - Accept：限定图片类型。
func newImageRequest(ctx context.Context, raw string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", chromeUserAgent)
	req.Header.Set("Accept", acceptHeader)
	if ref := originOf(raw); ref != "" {
		req.Header.Set("Referer", ref)
	}
	return req, nil
}

// newImageClient 构造用于图片请求的 HTTP 客户端：
// 限制单请求超时，并对每一跳重定向执行 SSRF 校验与次数上限；
// 重定向跳转时刷新浏览器特征头（Referer 取每一跳目标自身域名）。
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
			if err := checkURLAllowed(req.URL.String(), opt.AllowPrivate); err != nil {
				return err
			}
			// 刷新浏览器特征头：UA/Accept 保持 Chrome 特征，Referer 取当前目标自身域名
			req.Header.Set("User-Agent", chromeUserAgent)
			req.Header.Set("Accept", acceptHeader)
			if ref := originOf(req.URL.String()); ref != "" {
				req.Header.Set("Referer", ref)
			}
			return nil
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
