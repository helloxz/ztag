// Package logger 提供日志初始化与分级落盘能力。
//
// 需求：Warn/Error 级别不再打印终端，改为写入 /data/logs/error-YYYY-MM-DD.log，
// 按天切分，保留7天，文本格式，不压缩，基于 lumberjack 做单文件轮转兜底。
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// 日志目录与文件前缀
const (
	DefaultLogDir = "data/logs"
	FilePrefix    = "error-"
	FileSuffix    = ".log"
	RetainDays    = 7
	DateLayout    = "2006-01-02"
)

// dailyWriter 按天切分的 Writer，内部每日一个 lumberjack.Logger。
// 满足需求：lumberjack 负责单文件大小轮转与保留，daily 逻辑负责按天命名。
type dailyWriter struct {
	mu          sync.Mutex
	dir         string
	currentDate string
	logger      *lumberjack.Logger
}

// NewDailyWriter 构建按天切分的 writer，启动时确保目录存在。
func NewDailyWriter(dir string) (io.Writer, error) {
	if dir == "" {
		dir = DefaultLogDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create log dir %s: %w", dir, err)
	}
	w := &dailyWriter{dir: dir}
	today := time.Now().Format(DateLayout)
	if err := w.rotate(today); err != nil {
		return nil, err
	}
	_ = cleanupOldLogs(dir)
	return w, nil
}

func (w *dailyWriter) rotate(date string) error {
	filename := filepath.Join(w.dir, FilePrefix+date+FileSuffix)
	if w.logger != nil {
		_ = w.logger.Close()
	}
	w.logger = &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    50,
		MaxAge:     RetainDays,
		MaxBackups: 0,
		Compress:   false,
		LocalTime:  true,
	}
	w.currentDate = date
	return nil
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	today := time.Now().Format(DateLayout)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.currentDate != today {
		if err := w.rotate(today); err != nil {
			return 0, err
		}
		go func(dir string) { _ = cleanupOldLogs(dir) }(w.dir)
	}
	return w.logger.Write(p)
}

// cleanupOldLogs 删除目录下超过 RetainDays 的 error-YYYY-MM-DD.log 文件。
func cleanupOldLogs(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -RetainDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, FilePrefix) || !strings.HasSuffix(name, FileSuffix) {
			continue
		}
		dateStr := strings.TrimPrefix(strings.TrimSuffix(name, FileSuffix), FilePrefix)
		t, err := time.Parse(DateLayout, dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff.Truncate(24 * time.Hour)) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	var datedFiles []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, FilePrefix) && strings.HasSuffix(name, FileSuffix) {
			if _, err := time.Parse(DateLayout, strings.TrimPrefix(strings.TrimSuffix(name, FileSuffix), FilePrefix)); err == nil {
				datedFiles = append(datedFiles, name)
			}
		}
	}
	if len(datedFiles) > RetainDays {
		sort.Strings(datedFiles)
		for i := range len(datedFiles) - RetainDays {
			_ = os.Remove(filepath.Join(dir, datedFiles[i]))
		}
	}
	return nil
}

// routingHandler 根据日志级别路由：
//   - Level >= Warn  → fileHandler（落盘，不打印终端）
//   - Level < Warn   → termHandler（终端）
type routingHandler struct {
	fileHandler slog.Handler
	termHandler slog.Handler
}

func (h *routingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= slog.LevelWarn {
		return h.fileHandler.Enabled(ctx, level)
	}
	return h.termHandler.Enabled(ctx, level)
}

func (h *routingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		return h.fileHandler.Handle(ctx, r)
	}
	return h.termHandler.Handle(ctx, r)
}

func (h *routingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &routingHandler{
		fileHandler: h.fileHandler.WithAttrs(attrs),
		termHandler: h.termHandler.WithAttrs(attrs),
	}
}

func (h *routingHandler) WithGroup(name string) slog.Handler {
	return &routingHandler{
		fileHandler: h.fileHandler.WithGroup(name),
		termHandler: h.termHandler.WithGroup(name),
	}
}

// Init 初始化全局 slog：
//   - 终端：TextHandler 输出到 os.Stdout，按 cfgLevel 过滤
//   - 文件：TextHandler 输出到 dailyWriter，仅 Warn 及以上
func Init(logDir string, cfgLevel string) error {
	if logDir == "" {
		logDir = DefaultLogDir
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	fileWriter, err := NewDailyWriter(logDir)
	if err != nil {
		return err
	}

	termLevel := parseLevel(cfgLevel)

	fileHandler := slog.NewTextHandler(fileWriter, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})
	termHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: termLevel,
	})

	handler := &routingHandler{
		fileHandler: fileHandler,
		termHandler: termHandler,
	}
	slog.SetDefault(slog.New(handler))
	return nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
