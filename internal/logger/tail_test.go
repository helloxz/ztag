package logger

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// writeLog 在临时目录里写一个日志文件并返回其路径。
func writeLog(t *testing.T, dir, name, content string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	return path
}

func TestTailLatestLastLines(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 1000; i++ {
		b.WriteString("line-")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	writeLog(t, dir, "error-2026-11-20.log", b.String(), time.Now())

	res, err := TailLatest(dir, 100)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if len(res.Lines) != 100 {
		t.Fatalf("lines = %d, want 100", len(res.Lines))
	}
	if res.Lines[0] != "line-901" {
		t.Errorf("first line = %q, want line-901", res.Lines[0])
	}
	if res.Lines[len(res.Lines)-1] != "line-1000" {
		t.Errorf("last line = %q, want line-1000", res.Lines[len(res.Lines)-1])
	}
	if res.Size != int64(b.Len()) {
		t.Errorf("size = %d, want %d", res.Size, b.Len())
	}
	if res.File != "error-2026-11-20.log" {
		t.Errorf("file = %q", res.File)
	}
}

func TestTailLatestFewerLinesAndNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "error-2026-11-20.log", "a\nb\nc", time.Now())

	res, err := TailLatest(dir, 100)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	want := []string{"a", "b", "c"}
	if strings.Join(res.Lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v, want %v", res.Lines, want)
	}
}

func TestTailLatestMissingDir(t *testing.T) {
	res, err := TailLatest(filepath.Join(t.TempDir(), "no-such-dir"), 10)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if len(res.Lines) != 0 || res.File != "" || res.Size != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestTailLatestSkipsEmptyFileAndPicksNewest(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "error-2026-11-19.log", "old\n", now.Add(-48*time.Hour))
	writeLog(t, dir, "error-2026-11-20.log", "new\n", now)
	writeLog(t, dir, "error-2026-11-20-1.log", "", now.Add(time.Hour)) // 空文件不选
	// 非日志文件不参与挑选
	writeLog(t, dir, "other.txt", "ignore\n", now.Add(time.Hour))

	res, err := TailLatest(dir, 10)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if res.File != "error-2026-11-20.log" {
		t.Fatalf("file = %q, want error-2026-11-20.log", res.File)
	}
	if len(res.Lines) != 1 || res.Lines[0] != "new" {
		t.Fatalf("lines = %v", res.Lines)
	}
}

func TestTailLatestCRLF(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "error-2026-11-20.log", "a\r\nb\r\n", time.Now())

	res, err := TailLatest(dir, 10)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if strings.Join(res.Lines, "|") != "a|b" {
		t.Fatalf("lines = %q", res.Lines)
	}
}

// TestTailLatestTruncatesLongLine 保证超长行被截断且不产生非法 UTF-8。
func TestTailLatestTruncatesLongLine(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("中", tailLineMax) // 3 字节 × 4096，远超单行上限
	writeLog(t, dir, "error-2026-11-20.log", long+"\n", time.Now())

	res, err := TailLatest(dir, 10)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("lines = %d", len(res.Lines))
	}
	if !utf8.ValidString(res.Lines[0]) {
		t.Fatal("truncated line is not valid UTF-8")
	}
	if !strings.HasSuffix(res.Lines[0], "…(truncated)") {
		t.Fatalf("line is not truncated: %q", res.Lines[len(res.Lines)-1])
	}
}

// TestTailLatestChunkBoundary 用超过一个读取块的内容验证：
// 倒读窗口起点落在行中间时不会返回半行，且中文不会被切断。
func TestTailLatestChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	total := 0
	for i := 1; i <= 5000; i++ {
		line := "第" + strconv.Itoa(i) + "行 内容内容内容内容内容内容内容\n"
		b.WriteString(line)
		total += len(line)
	}
	if total < 3*tailChunk {
		t.Fatalf("test data too small: %d", total)
	}
	writeLog(t, dir, "error-2026-11-20.log", b.String(), time.Now())

	res, err := TailLatest(dir, 50)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if len(res.Lines) != 50 {
		t.Fatalf("lines = %d, want 50", len(res.Lines))
	}
	if res.Lines[len(res.Lines)-1] != "第5000行 内容内容内容内容内容内容内容" {
		t.Fatalf("last line = %q", res.Lines[len(res.Lines)-1])
	}
	for _, line := range res.Lines {
		if !utf8.ValidString(line) {
			t.Fatalf("invalid UTF-8 line: %q", line)
		}
		if !strings.HasPrefix(line, "第") {
			t.Fatalf("partial line leaked: %q", line)
		}
	}
}

func TestTailLatestClampsMaxLines(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("x\n")
	}
	writeLog(t, dir, "error-2026-11-20.log", b.String(), time.Now())

	res, err := TailLatest(dir, 0)
	if err != nil {
		t.Fatalf("TailLatest: %v", err)
	}
	if len(res.Lines) != 10 {
		t.Fatalf("lines = %d, want 10", len(res.Lines))
	}
}
