package service

import (
	"github.com/helloxz/ztag/internal/logger"
	"github.com/helloxz/ztag/internal/model"
)

// DefaultLogLines 日志页默认返回的行数。
const DefaultLogLines = 200

// LogsService 管理台「运行日志」：从日志目录倒读最新文件的尾部。
//
// 说明：logger 只把 WARN/ERROR 落盘（INFO/DEBUG 与访问日志只输出到终端），
// 因此本页展示的是告警与错误日志；历史文件由 logger 按天清理（保留 7 天）。
type LogsService struct {
	dir string
}

// NewLogsService 构建日志服务，dir 为日志目录（通常传 logger.DefaultLogDir）。
func NewLogsService(dir string) *LogsService {
	return &LogsService{dir: dir}
}

// Tail 返回最新日志文件最后 limit 行，limit 钳位到 1..logger.TailLinesMax。
func (s *LogsService) Tail(limit int) (logger.TailResult, error) {
	if limit <= 0 {
		limit = DefaultLogLines
	}
	if limit > logger.TailLinesMax {
		limit = logger.TailLinesMax
	}
	res, err := logger.TailLatest(s.dir, limit)
	if err != nil {
		return logger.TailResult{}, model.NewBizError("failed to read log file: " + err.Error())
	}
	return res, nil
}
