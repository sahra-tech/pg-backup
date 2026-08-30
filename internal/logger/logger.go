package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

type Logger struct {
	file   *os.File
	logger *log.Logger
}

// New returns a Logger that always writes to stderr, so `docker logs`,
// journald and any other supervisor see backup activity without extra setup.
// When filename is non-empty, output is additionally teed to that file.
//
// The returned Logger is ALWAYS usable. If the file cannot be opened, the
// Logger falls back to stderr-only and the error explains why: a log-file
// permission problem must never stop backups from running.
func New(filename string) (*Logger, error) {
	stderrOnly := &Logger{logger: log.New(os.Stderr, "", 0)}
	if filename == "" {
		return stderrOnly, nil
	}

	if dir := filepath.Dir(filename); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return stderrOnly, fmt.Errorf("cannot create log directory %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return stderrOnly, fmt.Errorf("cannot open log file %s: %w", filename, err)
	}

	return &Logger{
		file:   file,
		logger: log.New(io.MultiWriter(file, os.Stderr), "", 0),
	}, nil
}

// NewTo builds a Logger writing to w, with no file backing it. Intended for
// tests and for callers that manage their own output.
func NewTo(w io.Writer) *Logger {
	return &Logger{logger: log.New(w, "", 0)}
}

func (l *Logger) Info(format string, args ...any) {
	l.log("INFO", format, args...)
}

func (l *Logger) Error(format string, args ...any) {
	l.log("ERROR", format, args...)
}

func (l *Logger) Warning(format string, args ...any) {
	l.log("WARNING", format, args...)
}

func (l *Logger) log(level, format string, args ...any) {
	timestamp := time.Now().Format(time.RFC3339)
	l.logger.Printf("[%s] %s: %s", timestamp, level, fmt.Sprintf(format, args...))
}

func (l *Logger) Close() error {
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}
