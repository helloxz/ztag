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
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/helloxz/ztag/internal/ai"
	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/handler"
	"github.com/helloxz/ztag/internal/logger"
	"github.com/helloxz/ztag/internal/router"
	"github.com/helloxz/ztag/internal/service"
	"github.com/helloxz/ztag/internal/version"
)

func main() {
	// 支持通过 -config 指定配置文件路径（docker 部署时可按需覆盖）
	configFlag := flag.String("config", "", "config file path (default: data/config.toml, auto-created if missing)")
	showVersion := flag.Bool("v", false, "print version and exit")
	showVersionLong := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// 优先处理版本号查询：直接打印并退出，不加载配置/启动服务
	if *showVersion || *showVersionLong {
		fmt.Println(version.Version)
		os.Exit(0)
	}
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

	// ---------- 2.1 初始化日志：Warn/Error 落盘到 data/logs/error-YYYY-MM-DD.log ----------
	if err := logger.Init(logger.DefaultLogDir, cfg.Log.Level); err != nil {
		// 日志初始化失败不阻断启动，退化为终端输出
		log.Printf("[warn] init logger failed: %v", err)
	} else {
		slog.Info("logger initialized", "dir", logger.DefaultLogDir, "level", cfg.Log.Level, "retain_days", logger.RetainDays)
	}

	// ---------- 3. 构建核心依赖（由下至上） ----------
	// AI 多渠道网关 → 图片业务服务 → 各 handler
	gateway := ai.NewGateway(cfg.AI)
	imageSvc := service.NewImageService(gateway, cfg.AI)

	// 请求体上限：base64 编码膨胀约 4/3，另留 JSON 外壳与 data URI 前缀余量
	const maxRequestBodyExtra = 64 * 1024
	maxRequestBody := cfg.AI.MaxImageBytes*4/3 + maxRequestBodyExtra

	hs := &router.Handlers{
		Health: handler.NewHealthHandler(),
		Image:  handler.NewImageHandler(imageSvc, maxRequestBody),
	}

	// ---------- 4. 构建路由并启动 HTTP 服务 ----------
	engine := router.SetupRouter(cfg, hs)
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second, // 防慢连接/僵尸连接堆积 goroutine 与缓冲（请求体读取与 AI 调用时长不受限）
	}

	go func() {
		slog.Info("ZTAG server started", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("[startup failed] HTTP server", "err", err)
			os.Exit(1)
		}
	}()
	// ---------- 5. 等待退出信号，优雅关闭 ----------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutdown signal received, closing server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("[exit] server shutdown error", "err", err)
	}
	slog.Info("server exited")
}
