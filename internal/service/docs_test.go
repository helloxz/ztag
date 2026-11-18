package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newDocsTestService 构造指向测试服务器的文档服务。
func newDocsTestService(t *testing.T, srv *httptest.Server) *DocsService {
	t.Helper()
	return &DocsService{
		url:    srv.URL,
		client: srv.Client(),
		ttl:    time.Hour,
	}
}

// TestDocsServiceCacheAndStale 验证缓存命中、过期刷新与刷新失败回退旧内容。
func TestDocsServiceCacheAndStale(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	status := http.StatusOK
	body := "# doc v1\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	svc := newDocsTestService(t, srv)

	// 首次请求：抓取并写入缓存
	doc, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("首次 Get 出错: %v", err)
	}
	if doc.Content != "# doc v1\n" || doc.Stale {
		t.Fatalf("首次结果 = %+v", doc)
	}

	// 缓存有效：不再访问远端
	if _, err := svc.Get(context.Background()); err != nil {
		t.Fatalf("缓存命中 Get 出错: %v", err)
	}
	mu.Lock()
	if hits != 1 {
		t.Fatalf("缓存有效时请求次数 = %d，期望 1", hits)
	}
	mu.Unlock()

	// 缓存过期：重新抓取新内容
	mu.Lock()
	body = "# doc v2\n"
	mu.Unlock()
	svc.mu.Lock()
	svc.fetchedAt = time.Now().Add(-2 * svc.ttl)
	svc.mu.Unlock()

	doc, err = svc.Get(context.Background())
	if err != nil {
		t.Fatalf("过期后 Get 出错: %v", err)
	}
	if doc.Content != "# doc v2\n" || doc.Stale {
		t.Fatalf("刷新结果 = %+v", doc)
	}

	// 再次过期且远端失败：返回旧内容并标记 stale
	mu.Lock()
	status = http.StatusInternalServerError
	mu.Unlock()
	svc.mu.Lock()
	svc.fetchedAt = time.Now().Add(-2 * svc.ttl)
	svc.mu.Unlock()

	doc, err = svc.Get(context.Background())
	if err != nil {
		t.Fatalf("远端失败时应回退旧缓存，却返回错误: %v", err)
	}
	if doc.Content != "# doc v2\n" || !doc.Stale {
		t.Fatalf("失败回退结果 = %+v", doc)
	}
}

// TestDocsServiceNoCacheFailure 无缓存且远端失败时返回业务错误。
func TestDocsServiceNoCacheFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	svc := newDocsTestService(t, srv)
	if _, err := svc.Get(context.Background()); err == nil || err.Error() != "failed to load API docs" {
		t.Fatalf("err = %v，期望 failed to load API docs", err)
	}
}
