// ZTAG 服务入口。
//
// 启动流程：
//  1. 确保 data/config.toml 存在（不存在时自动从内嵌模板拷贝）；
//  2. 加载并校验配置；
//  3. 按配置构建 AI 多渠道网关与业务依赖；
//  4. 启动 HTTP 服务并支持优雅退出（SIGINT / SIGTERM）。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/handler"
	"github.com/helloxz/ztag/internal/router"
	"github.com/helloxz/ztag/internal/service"
)

func main() {
	// 支持通过 -config 指定配置文件路径（docker 部署时可按需覆盖）
	configFlag := flag.String("config", "", "config file path (default: data/config.toml, auto-created if missing)")
	flag.Parse()

	// ---------- 1. 确保运行时配置文件存在（data/config.toml） ----------
	cfgPath := *configFlag
	if cfgPath == "" {
		// 未指定路径：走默认逻辑，不存在时自动从内嵌模板拷贝到 data/ 目录
		var err error
		cfgPath, err = config.EnsureDataConfig()
		if err != nil {
			log.Fatalf("[startup failed] init config file: %v", err)
		}
	}
	log.Printf("using config file: %s", cfgPath)

	// ---------- 2. 加载并校验配置 ----------
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("[startup failed] load config: %v", err)
	}

	// ---------- 3. 构建核心依赖（由下至上） ----------
	// AI 多渠道网关 → 图片业务服务 → 各 handler
	gateway := ai.NewGateway(cfg.AI)
	imageSvc := service.NewImageService(gateway, cfg.AI)

	hs := &router.Handlers{
		Health: handler.NewHealthHandler(),
		Image:  handler.NewImageHandler(imageSvc),
	}

	// ---------- 4. 构建路由并启动 HTTP 服务 ----------
	engine := router.SetupRouter(cfg, hs)
	srv := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: engine,
	}

	go func() {
		log.Printf("ZTAG server started, listening on %s", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[startup failed] HTTP server: %v", err)
		}
	}()

	// ---------- 5. 等待退出信号，优雅关闭 ----------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutdown signal received, closing server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[exit] server shutdown error: %v", err)
	}
	log.Println("server exited")
}
