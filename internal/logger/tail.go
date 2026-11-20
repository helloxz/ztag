package logger

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 管理台日志页的倒读参数。
const (
	// TailLinesMax 单次最多返回的行数（同时约束页面 DOM 规模）。
	TailLinesMax = 500
	// tailLineMax 单行最多保留的字节数；超长日志截断，避免一条巨日志撑大响应与页面。
	tailLineMax = 4 << 10
	// tailChunk 每次从文件尾部读取的块大小。
	tailChunk = 32 << 10
	// tailMaxBytes 单次倒读的最大字节数，防止超大文件被整篇扫描。
	tailMaxBytes = 4 << 20
)

// TailResult 管理台日志页的一次读取结果。
type TailResult struct {
	Lines []string `json:"lines"` // 按时间正序的日志行
	File  string   `json:"file"`  // 实际读取的文件名（目录内），无文件时为空
	Size  int64    `json:"size"`  // 文件大小，前端据此判断内容是否有变化
	Dir   string   `json:"dir"`   // 日志目录（页面提示用）
}

// TailLatest 读取 dir 下最新日志文件的最后 maxLines 行（时间正序）。
//
// 只倒读文件尾部，不解析日志格式；目录或文件不存在时返回空结果而不报错。
// 注意：logger 只把 WARN 及以上写入文件，INFO/DEBUG 与访问日志不落盘。
func TailLatest(dir string, maxLines int) (TailResult, error) {
	if dir == "" {
		dir = DefaultLogDir
	}
	if maxLines <= 0 || maxLines > TailLinesMax {
		maxLines = TailLinesMax
	}

	res := TailResult{Lines: []string{}, Dir: dir}
	path := latestFile(dir)
	if path == "" {
		return res, nil
	}
	res.File = filepath.Base(path)

	lines, size, err := tailFile(path, maxLines)
	if err != nil {
		return res, err
	}
	res.Lines = lines
	res.Size = size
	return res, nil
}

// latestFile 在 dir 中挑选最新且非空的 error-*.log。
// 按修改时间挑选：按天切分与 lumberjack 大小轮转的备份文件会同时存在。
func latestFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var (
		bestPath  string
		bestMtime time.Time
	)
	for _, e := range entries {
		if e.IsDir() || !isLogName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestMtime) {
			bestPath = filepath.Join(dir, e.Name())
			bestMtime = info.ModTime()
		}
	}
	return bestPath
}

// isLogName 判断是否是本包写出的日志文件（含大小轮转产生的备份名）。
func isLogName(name string) bool {
	return strings.HasPrefix(name, FilePrefix) && strings.HasSuffix(name, FileSuffix)
}

// tailFile 从文件尾部按块倒读，返回最后 maxLines 行与文件大小。
// 内存占用上限约为 tailMaxBytes，与文件总大小无关。
func tailFile(path string, maxLines int) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if size == 0 {
		return []string{}, size, nil
	}

	var (
		chunks   [][]byte // 倒序收集的块
		total    int64    // 已倒读字节
		newlines int      // 已收集的换行数
		pos      = size
	)
	for pos > 0 && total < tailMaxBytes && newlines < maxLines+1 {
		n := int64(tailChunk)
		if pos < n {
			n = pos
		}
		pos -= n

		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		chunks = append(chunks, buf)
		total += n
		newlines += bytes.Count(buf, []byte{'\n'})
	}

	// 按文件顺序拼回读取窗口
	var window bytes.Buffer
	for i := len(chunks) - 1; i >= 0; i-- {
		window.Write(chunks[i])
	}
	raw := window.Bytes()

	// pos > 0 表示没读到文件开头，窗口首行可能是被截断的半行，直接丢弃
	if pos > 0 {
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		} else {
			raw = nil
		}
	}
	if len(raw) == 0 {
		return []string{}, size, nil
	}

	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for i, line := range lines {
		lines[i] = clampLine(strings.TrimSuffix(line, "\r"))
	}
	return lines, size, nil
}

// clampLine 截断超长日志行（按字节），截断处回退到 UTF-8 字符边界。
func clampLine(line string) string {
	if len(line) <= tailLineMax {
		return line
	}
	cut := tailLineMax
	for cut > 0 && line[cut]&0xC0 == 0x80 { // 0x80~0xBF 是 UTF-8 续字节
		cut--
	}
	return line[:cut] + " …(truncated)"
}
