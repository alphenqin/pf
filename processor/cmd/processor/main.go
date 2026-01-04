package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pmacct/processor/internal/config"
	"github.com/pmacct/processor/internal/statusreport"
	"github.com/pmacct/processor/internal/uploader"
	"github.com/pmacct/processor/internal/batchwriter"
	"github.com/pmacct/processor/internal/model"
)

var (
	configPath = flag.String("config", "", "配置文件路径（pmacct.conf，含 processor_* 配置）")
	dataDir    = flag.String("data-dir", "", "本地缓存目录，存放滚动生成的压缩文件")
	logLevel   = flag.String("log-level", "info", "日志级别: debug|info|warn|error")
)

func main() {
	flag.Parse()

	// 验证必需参数
	if *configPath == "" {
		log.Fatal("[ERROR] -config 参数是必需的")
	}
	if *dataDir == "" {
		log.Fatal("[ERROR] -data-dir 参数是必需的")
	}

	// 加载配置
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[ERROR] 加载配置失败: %v", err)
	}
	log.Printf("[INFO] 配置加载成功: FTP=%s:%d, 滚动间隔=%ds, 滚动大小=%dMB, 上传间隔=%ds",
		cfg.FTPHost, cfg.FTPPort, cfg.RotateIntervalSec, cfg.RotateSizeMB, cfg.UploadIntervalSec)

	// 确保数据目录存在
	if err := config.EnsureDataDir(*dataDir); err != nil {
		log.Fatalf("[ERROR] 数据目录准备失败: %v", err)
	}
	log.Printf("[INFO] 数据目录已就绪: %s", *dataDir)

	// 公共上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 创建批处理 Writer
	bw := batchwriter.NewBatchWriter(*dataDir, cfg.FilePrefix, cfg.RotateIntervalSec, cfg.RotateSizeMB)

	// 状态上报器
	reporter, err := statusreport.NewReporter(cfg.StatusReport)
	if err != nil {
		log.Fatalf("[ERROR] 初始化状态上报失败: %v", err)
	}
	// 运行上报 goroutine（如果启用）
	if reporter != nil {
		go reporter.Run(ctx.Done())
		log.Printf("[INFO] 状态上报已启用，目标: %s，周期: %ds", cfg.StatusReport.URL, cfg.StatusReport.IntervalSec)
	}

	// 创建 Uploader
	up := uploader.NewUploader(
		ctx,
		cfg.FTPHost,
		cfg.FTPPort,
		cfg.FTPUser,
		cfg.FTPPass,
		cfg.FTPDir,
		cfg.FTPOptions.TimeoutSec,
		*dataDir,
		cfg.UploadIntervalSec,
	)

	// 启动上传器
	up.Start()
	log.Printf("[INFO] FTP 上传器已启动，上传间隔: %ds", cfg.UploadIntervalSec)

	// 创建数据通道（带缓冲）
	dataChan := make(chan model.DataLine, 10000)

	// 启动 writer goroutine
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- runBatchWriter(ctx, bw, dataChan)
	}()

	// 设置信号处理
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 启动从 stdin 读取的 goroutine
	ingestDone := make(chan error, 1)
	go func() {
		ingestDone <- runIngest(ctx, dataChan, reporter, cfg.DebugPrintInterval)
	}()

	// 等待信号或完成
	select {
	case sig := <-sigChan:
		log.Printf("[INFO] 收到信号: %v，开始优雅关闭...", sig)
		cancel()
		// 等待所有 goroutine 完成
		<-ingestDone
		close(dataChan)
		<-writerDone
	case err := <-ingestDone:
		if err != nil {
			log.Printf("[ERROR] 读取 stdin 时出错: %v", err)
		}
		close(dataChan)
		<-writerDone
	case err := <-writerDone:
		if err != nil {
			log.Printf("[ERROR] 批量写入时出错: %v", err)
		}
		cancel()
		<-ingestDone
	}

	// 关闭 batch writer（确保当前文件被正确关闭和重命名）
	if err := bw.Close(); err != nil {
		log.Printf("[ERROR] 关闭 batch writer 失败: %v", err)
	} else {
		log.Printf("[INFO] Batch writer 已关闭")
	}

	// 停止上传器
	up.Stop()
	log.Printf("[INFO] 程序退出")
}


// parseCounts 从行中按索引解析包/字节数
func parseCounts(line string, pktIdx, byteIdx int) (int64, int64) {
	fields := strings.Split(line, "|")
	if pktIdx >= len(fields) || byteIdx >= len(fields) {
		return 0, 0
	}
	pkts, _ := strconv.ParseInt(strings.TrimSpace(fields[pktIdx]), 10, 64)
	bytes, _ := strconv.ParseInt(strings.TrimSpace(fields[byteIdx]), 10, 64)
	return pkts, bytes
}

// isHeaderLine 判断是否为表头行（用于丢弃表头）
func isHeaderLine(line string) bool {
	lower := strings.ToLower(line)
	headerKeywords := []string{
		"flowstart", "timestamp",
		"src_ip", "dst_ip", "src_host", "dst_host",
		"src_port", "dst_port", "protocol", "proto", "tos",
		"packet", "octet", "bytes",
	}
	for _, kw := range headerKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// runIngest 从标准输入读取数据并放入channel
func runIngest(ctx context.Context, dataChan chan<- model.DataLine, reporter *statusreport.Reporter, debugPrintInterval int) error {
	scanner := bufio.NewScanner(os.Stdin)
	lineCount := 0
	headerProcessed := false
	packetIdx := -1
	octetIdx := -1

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					log.Printf("[ERROR] 读取 stdin 失败: %v", err)
					return fmt.Errorf("读取 stdin 失败: %w", err)
				}
				// EOF
				log.Printf("[INFO] 从 stdin 读取完成，共处理 %d 行", lineCount)
				return nil
			}

			line := scanner.Text()
			if len(line) == 0 {
				continue
			}

			// 处理表头行：解析字段索引（表头行会被丢弃）
			if !headerProcessed {
			// 检查是否是表头行
			if isHeaderLine(line) {
				headerProcessed = true

					// 解析字段索引（包/字节统计）
					fields := strings.Split(line, "|")
					for i, f := range fields {
						ft := strings.TrimSpace(f)
						switch ft {
						case "packetTotalCount":
							packetIdx = i
						case "octetTotalCount":
							octetIdx = i
						}
					}
				continue
			}
			}

			// 处理数据行
			outputLine := line

			// 统计包/字节数
			if reporter != nil && packetIdx >= 0 && octetIdx >= 0 {
				if pkts, bytes := parseCounts(outputLine, packetIdx, octetIdx); pkts > 0 || bytes > 0 {
					reporter.Add(pkts, bytes)
				}
			}

			// 每隔指定行数打印CSV数据行内容用于调试
			if debugPrintInterval > 0 && lineCount > 0 && lineCount%debugPrintInterval == 0 {
				log.Printf("[DEBUG] CSV数据行 #%d: %s", lineCount, outputLine)
			}

			// 将数据行放入channel，带超时保护
			select {
			case dataChan <- model.DataLine{Line: outputLine, IsHeader: false}:
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				// channel满时，记录警告并丢弃数据
				log.Printf("[WARN] 数据通道满，丢弃数据行: %s", outputLine[:min(len(outputLine), 100)])
			}

			lineCount++
			if lineCount%10000 == 0 {
				log.Printf("[INFO] 已处理 %d 行数据", lineCount)
			}
		}
	}
}

// runBatchWriter 从channel批量读取数据并写入文件
func runBatchWriter(ctx context.Context, bw *batchwriter.BatchWriter, dataChan <-chan model.DataLine) error {
	// 批量处理的缓冲区
	batch := make([]model.DataLine, 0, 1000)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// 统计信息
	totalLines := int64(0)
	droppedLines := int64(0)

	flushBatch := func() error {
		if len(batch) > 0 {
			if err := bw.WriteBatch(batch); err != nil {
				return fmt.Errorf("批量写入失败: %w", err)
			}
			// 强制刷新缓冲区到磁盘
			if err := bw.Flush(); err != nil {
				return fmt.Errorf("刷新缓冲区失败: %w", err)
			}
			totalLines += int64(len(batch))
			batch = batch[:0] // 清空批次
		}
		return nil
	}

	// 定期报告处理统计信息
	reportTicker := time.NewTicker(30 * time.Second)
	defer reportTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 上下文取消，刷新剩余数据并退出
			if err := flushBatch(); err != nil {
				return err
			}
			log.Printf("[INFO] 批量写入器已处理 %d 行数据，丢弃 %d 行数据", totalLines, droppedLines)
			return nil
		case dataLine, ok := <-dataChan:
			if !ok {
				// channel已关闭，刷新剩余数据并退出
				if err := flushBatch(); err != nil {
					return err
				}
				log.Printf("[INFO] 批量写入器已处理 %d 行数据，丢弃 %d 行数据", totalLines, droppedLines)
				return nil
			}

			// 添加到批次
			batch = append(batch, dataLine)

			// 如果批次已满，立即刷新
			if len(batch) >= 1000 {
				if err := flushBatch(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			// 定期刷新，避免数据在缓冲区中停留太久
			if err := flushBatch(); err != nil {
				return err
			}
		case <-reportTicker.C:
			// 定期报告处理统计信息
			log.Printf("[INFO] 批量写入器统计 - 已处理: %d 行, 丢弃: %d 行", totalLines, droppedLines)
		}
	}
}
