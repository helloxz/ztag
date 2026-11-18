package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/helloxz/ztag/internal/model"
)

const (
	// apiDocsURL 管理台 API 文档的远端地址（仓库 main 分支）。
	apiDocsURL = "https://raw.githubusercontent.com/helloxz/ztag/refs/heads/main/docs/api.md"
	// apiDocsTTL 文档内存缓存有效期。
	apiDocsTTL = 24 * time.Hour
	// apiDocsMaxBytes 单次下载的文档大小上限，防止异常响应占满内存。
	apiDocsMaxBytes = 1 << 20
)

// APIDoc 管理台展示的 API 文档快照。
type APIDoc struct {
	Content   string `json:"content"`    // Markdown 原文
	FetchedAt int64  `json:"fetched_at"` // 本次内容抓取时间（Unix 秒）
	Stale     bool   `json:"stale"`      // true 表示刷新失败，返回的是过期缓存
}

// DocsService 负责抓取并缓存 API 文档。
// 采用懒加载：首次请求时才访问远端，之后 24 小时内直接命中内存缓存；
// 缓存过期后刷新失败时继续返回旧内容，避免文档页因网络问题完全不可用。
type DocsService struct {
	mu        sync.Mutex
	url       string
	client    *http.Client
	ttl       time.Duration
	content   string
	fetchedAt time.Time
}

// NewDocsService 构建 API 文档服务。
func NewDocsService() *DocsService {
	return &DocsService{
		url:    apiDocsURL,
		client: &http.Client{Timeout: 10 * time.Second},
		ttl:    apiDocsTTL,
	}
}

// Get 返回文档内容：缓存有效时直接返回，否则重新抓取。
// 刷新失败但有旧缓存时返回旧内容（Stale=true）；没有任何缓存时返回业务错误。
func (s *DocsService) Get(ctx context.Context) (*APIDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.content != "" && time.Since(s.fetchedAt) < s.ttl {
		return &APIDoc{Content: s.content, FetchedAt: s.fetchedAt.Unix()}, nil
	}

	content, err := s.fetch(ctx)
	if err != nil {
		if s.content != "" {
			slog.Warn("failed to refresh API docs, serving stale cache", "url", s.url, "error", err)
			return &APIDoc{Content: s.content, FetchedAt: s.fetchedAt.Unix(), Stale: true}, nil
		}
		slog.Warn("failed to fetch API docs", "url", s.url, "error", err)
		return nil, model.NewBizError("failed to load API docs")
	}

	s.content = content
	s.fetchedAt = time.Now()
	return &APIDoc{Content: s.content, FetchedAt: s.fetchedAt.Unix()}, nil
}

// fetch 下载远端 Markdown 原文。
func (s *DocsService) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "ztag-admin-docs")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, apiDocsMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if len(body) > apiDocsMaxBytes {
		return "", fmt.Errorf("document exceeds %d bytes", apiDocsMaxBytes)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("empty document")
	}
	return string(body), nil
}
