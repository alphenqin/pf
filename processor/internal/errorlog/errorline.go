package errorlog

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// LineWriter writes invalid CSV lines to error/errorline.csv.
type LineWriter struct {
	file   *os.File
	writer *csv.Writer
}

func NewLineWriter(dataDir string) (*LineWriter, error) {
	errDir := filepath.Join(dataDir, "error")
	if err := os.MkdirAll(errDir, 0755); err != nil {
		return nil, fmt.Errorf("create error dir failed: %w", err)
	}
	path := filepath.Join(errDir, "errorline.csv")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open errorline.csv failed: %w", err)
	}
	return &LineWriter{
		file:   f,
		writer: csv.NewWriter(f),
	}, nil
}

func (w *LineWriter) Write(lineCount int, line, reason string) error {
	if w == nil {
		return nil
	}
	if err := w.writer.Write([]string{strconv.Itoa(lineCount), line, reason}); err != nil {
		return err
	}
	w.writer.Flush()
	return w.writer.Error()
}

func (w *LineWriter) Close() error {
	if w == nil {
		return nil
	}
	w.writer.Flush()
	if err := w.writer.Error(); err != nil {
		_ = w.file.Close()
		return err
	}
	return w.file.Close()
}
