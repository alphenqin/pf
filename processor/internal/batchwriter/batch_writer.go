package batchwriter

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pmacct/processor/internal/model"
)

// BatchWriter 负责批量写入数据到文件
type BatchWriter struct {
	dataDir           string
	filePrefix        string
	rotateIntervalSec int
	rotateSizeMB      int

	buffer     *bufio.Writer
	gzipWriter *gzip.Writer
	file       *os.File
	currentPath string

	writtenBytes int64
	startTime    time.Time
	fileIndex    int

	mu sync.Mutex
	closed bool
}

// NewBatchWriter 创建新的 BatchWriter
func NewBatchWriter(dataDir, filePrefix string, rotateIntervalSec, rotateSizeMB int) *BatchWriter {
	return &BatchWriter{
		dataDir:           dataDir,
		filePrefix:        filePrefix,
		rotateIntervalSec: rotateIntervalSec,
		rotateSizeMB:      rotateSizeMB,
		fileIndex:         0,
	}
}

// WriteBatch 批量写入数据行
func (bw *BatchWriter) WriteBatch(lines []model.DataLine) error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return fmt.Errorf("batch writer 已关闭")
	}

	// 如果当前文件不存在，创建新文件
	if bw.file == nil {
		if err := bw.rotateFile(); err != nil {
			return fmt.Errorf("创建新文件失败: %w", err)
		}
	}

	// 写入所有行
	for _, dataLine := range lines {
		// 写入数据（包括换行符）
		data := dataLine.Line + "\n"
		n, err := bw.buffer.Write([]byte(data))
		if err != nil {
			return fmt.Errorf("写入数据失败: %w", err)
		}
		bw.writtenBytes += int64(len(data))
		_ = n // 避免未使用变量警告
	}

	// 检查是否需要滚动
	if bw.shouldRotate() {
		if err := bw.flushAndRotate(); err != nil {
			return fmt.Errorf("滚动文件失败: %w", err)
		}
	}

	return nil
}

// Flush 强制刷新缓冲区到磁盘
func (bw *BatchWriter) Flush() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed || bw.buffer == nil {
		return nil
	}

	return bw.buffer.Flush()
}

// shouldRotate 检查是否应该滚动文件
func (bw *BatchWriter) shouldRotate() bool {
	// 检查时间间隔
	if time.Since(bw.startTime) >= time.Duration(bw.rotateIntervalSec)*time.Second {
		return true
	}

	// 检查文件大小（原始字节数，不是压缩后）
	if bw.writtenBytes >= int64(bw.rotateSizeMB)*1024*1024 {
		return true
	}

	return false
}

// flushAndRotate 刷新缓冲区并滚动文件
func (bw *BatchWriter) flushAndRotate() error {
	// 刷新当前缓冲区
	if err := bw.buffer.Flush(); err != nil {
		return fmt.Errorf("刷新缓冲区失败: %w", err)
	}

	// 关闭当前文件并重命名
	if err := bw.closeAndRenameCurrentFile(); err != nil {
		return fmt.Errorf("关闭并重命名文件失败: %w", err)
	}

	// 创建新文件
	if err := bw.rotateFile(); err != nil {
		return fmt.Errorf("创建新文件失败: %w", err)
	}

	return nil
}

// closeAndRenameCurrentFile 关闭当前文件并重命名为最终文件名
func (bw *BatchWriter) closeAndRenameCurrentFile() error {
	if bw.gzipWriter != nil {
		if err := bw.gzipWriter.Close(); err != nil {
			return fmt.Errorf("关闭 gzip writer 失败: %w", err)
		}
		bw.gzipWriter = nil
	}

	if bw.file != nil {
		if err := bw.file.Close(); err != nil {
			return fmt.Errorf("关闭文件失败: %w", err)
		}
		bw.file = nil
	}

	// 将 .part 文件重命名为 .csv.gz
	if bw.currentPath != "" {
		// 确保路径长度足够并且以 .part 结尾
		if len(bw.currentPath) >= 5 && bw.currentPath[len(bw.currentPath)-5:] == ".part" {
			finalPath := bw.currentPath[:len(bw.currentPath)-5] + ".csv.gz"
			if err := os.Rename(bw.currentPath, finalPath); err != nil {
				return fmt.Errorf("重命名文件失败: %w", err)
			}
		} else {
			// 如果不是以 .part 结尾，添加 .csv.gz 后缀
			finalPath := bw.currentPath + ".csv.gz"
			if err := os.Rename(bw.currentPath, finalPath); err != nil {
				return fmt.Errorf("重命名文件失败: %w", err)
			}
		}
	}

	return nil
}

// rotateFile 滚动到新文件
func (bw *BatchWriter) rotateFile() error {
	// 生成新文件名
	now := time.Now()
	timestamp := now.Format("20060102_150405")
	filename := fmt.Sprintf("%s%s_%03d.part", bw.filePrefix, timestamp, bw.fileIndex)
	bw.currentPath = filepath.Join(bw.dataDir, filename)

	// 创建新文件
	file, err := os.Create(bw.currentPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}

	// 创建 gzip writer
	gzipWriter := gzip.NewWriter(file)

	// 创建带缓冲的 writer（使用 4MB 缓冲区）
	buffer := bufio.NewWriterSize(gzipWriter, 4*1024*1024)

	bw.file = file
	bw.gzipWriter = gzipWriter
	bw.buffer = buffer
	bw.startTime = now
	bw.writtenBytes = 0
	bw.fileIndex++

	return nil
}

// Close 关闭 writer，确保当前文件被正确关闭和重命名
func (bw *BatchWriter) Close() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return nil
	}

	bw.closed = true

	// 刷新缓冲区
	if bw.buffer != nil {
		if err := bw.buffer.Flush(); err != nil {
			return fmt.Errorf("刷新缓冲区失败: %w", err)
		}
	}

	// 关闭当前文件并重命名
	if err := bw.closeAndRenameCurrentFile(); err != nil {
		return fmt.Errorf("关闭并重命名文件失败: %w", err)
	}

	return nil
}

// GetDataDir 返回数据目录路径
func (bw *BatchWriter) GetDataDir() string {
	return bw.dataDir
}