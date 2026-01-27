package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"sync/atomic"
	"time"

	"github.com/pmacct/processor/internal/batchwriter"
	"github.com/pmacct/processor/internal/config"
	"github.com/pmacct/processor/internal/diag"
	"github.com/pmacct/processor/internal/errorlog"
	"github.com/pmacct/processor/internal/model"
	"github.com/pmacct/processor/internal/statusreport"
	"github.com/pmacct/processor/internal/uploader"
	"github.com/pmacct/processor/internal/validator"
)

var (
	configPath = flag.String("config", "", "配置文件路径（pmacct.conf，含 processor_* 配置）")
	dataDir    = flag.String("data-dir", "", "本地缓存目录，存放滚动生成的压缩文件")
	logLevel   = flag.String("log-level", "info", "日志级别: debug|info|warn|error")
)

func main() {
	flag.Parse()
	setupLogger(*logLevel)

	// 验证必需参数
	if *configPath == "" {
		slog.Error("-config 参数是必需的")
		os.Exit(1)
	}
	if *dataDir == "" {
		slog.Error("-data-dir 参数是必需的")
		os.Exit(1)
	}

	// 加载配置
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	slog.Info("配置加载成功",
		"ftp_host", cfg.FTPHost,
		"ftp_port", cfg.FTPPort,
		"rotate_interval_sec", cfg.RotateIntervalSec,
		"rotate_size_mb", cfg.RotateSizeMB,
		"upload_interval_sec", cfg.UploadIntervalSec,
	)

	// 确保数据目录存在
	if err := config.EnsureDataDir(*dataDir); err != nil {
		slog.Error("数据目录准备失败", "err", err)
		os.Exit(1)
	}
	slog.Info("数据目录已就绪", "data_dir", *dataDir)

	// 错误行落盘
	errWriter, err := errorlog.NewLineWriter(*dataDir)
	if err != nil {
		slog.Error("初始化错误行写入器失败", "err", err)
	}

	// 公共上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 创建批处理 Writer
	bw := batchwriter.NewBatchWriter(*dataDir, cfg.FilePrefix, cfg.RotateIntervalSec, cfg.RotateSizeMB)

	// 状态上报器
	reporter, err := statusreport.NewReporter(cfg.StatusReport)
	if err != nil {
		slog.Error("初始化状态上报失败", "err", err)
		os.Exit(1)
	}
	// 运行上报 goroutine（如果启用）
	if reporter != nil {
		go reporter.Run(ctx.Done())
		slog.Info("状态上报已启用", "url", cfg.StatusReport.URL, "interval_sec", cfg.StatusReport.IntervalSec)
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
	slog.Info("FTP 上传器已启动", "interval_sec", cfg.UploadIntervalSec)

	// 启动诊断采集（宿主机日志结构化 + 进程日志）
	var diagCollector *diag.Collector
	var csvTotal atomic.Int64
	var csvDNS atomic.Int64
	if cfg.Diag.Enabled {
		diagCollector = diag.NewCollector(ctx, cfg.Diag, *dataDir)
		diagCollector.SetProcPayloadEnricher(func(procName string) map[string]interface{} {
			if procName != "processor" {
				return nil
			}
			return map[string]interface{}{
				"csv_total": csvTotal.Load(),
				"csv_dns":   csvDNS.Load(),
			}
		})
		diagCollector.Start()
		slog.Info("诊断采集已启用", "interval_sec", cfg.Diag.IntervalSec)
	}

	// 创建数据通道（带缓冲）
	dataChan := make(chan model.DataLine, cfg.IngestChanCapacity)

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
		ingestDone <- runIngest(
			ctx,
			dataChan,
			reporter,
			cfg.DebugPrintInterval,
			time.Duration(cfg.IngestChanTimeoutMs)*time.Millisecond,
			errWriter,
			&csvTotal,
			&csvDNS,
		)
	}()

	// 等待信号或完成
	select {
	case sig := <-sigChan:
		slog.Info("收到信号，开始优雅关闭", "signal", sig.String())
		cancel()
		// 等待所有 goroutine 完成
		<-ingestDone
		close(dataChan)
		<-writerDone
	case err := <-ingestDone:
		if err != nil {
			slog.Error("读取 stdin 时出错", "err", err)
		}
		close(dataChan)
		<-writerDone
	case err := <-writerDone:
		if err != nil {
			slog.Error("批量写入时出错", "err", err)
		}
		cancel()
		<-ingestDone
	}

	// 关闭 batch writer（确保当前文件被正确关闭和重命名）
	if err := bw.Close(); err != nil {
		slog.Error("关闭 batch writer 失败", "err", err)
	} else {
		slog.Info("Batch writer 已关闭")
	}

	if errWriter != nil {
		if err := errWriter.Close(); err != nil {
			slog.Error("关闭错误行写入器失败", "err", err)
		}
	}

	if diagCollector != nil {
		diagCollector.Stop()
	}
	// 停止上传器
	up.Stop()
	slog.Info("程序退出")
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
		"tcp_flags", "timestamp_min", "timestamp_max",
		"packet", "packets", "octet", "bytes",
	}
	for _, kw := range headerKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func isDNSLine(line string) bool {
	fields := strings.Split(line, ",")
	if len(fields) < 4 {
		return false
	}
	srcPort := strings.TrimSpace(fields[2])
	dstPort := strings.TrimSpace(fields[3])
	return srcPort == "53" || dstPort == "53"
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// runIngest 从标准输入读取数据并放入channel
func runIngest(ctx context.Context, dataChan chan<- model.DataLine, reporter *statusreport.Reporter, debugPrintInterval int, chanTimeout time.Duration, errWriter *errorlog.LineWriter, csvTotal *atomic.Int64, csvDNS *atomic.Int64) error {
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
					slog.Error("读取 stdin 失败", "err", err)
					return fmt.Errorf("读取 stdin 失败: %w", err)
				}
				// EOF
				slog.Info("从 stdin 读取完成", "lines", lineCount)
				return nil
			}

			line := scanner.Text()
			if len(line) == 0 {
				continue
			}

			currentLineNo := lineCount + 1

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

			if ok, reason := validator.ValidateLine(line, time.Now()); !ok {
				slog.Warn("无效CSV行", "line_no", currentLineNo, "reason", reason, "line", line)
				if errWriter != nil {
					if err := errWriter.Write(currentLineNo, line, reason); err != nil {
						slog.Error("写入 errorline.csv 失败", "err", err)
					}
				}
				lineCount++
				continue
			}

			if csvTotal != nil {
				csvTotal.Add(1)
			}
			if csvDNS != nil && isDNSLine(line) {
				csvDNS.Add(1)
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
				slog.Debug("CSV数据行", "line_no", lineCount, "line", outputLine)
			}

			// 将数据行放入channel，带超时保护
			if chanTimeout <= 0 {
				select {
				case dataChan <- model.DataLine{Line: outputLine, IsHeader: false}:
				case <-ctx.Done():
					return ctx.Err()
				}
			} else {
				select {
				case dataChan <- model.DataLine{Line: outputLine, IsHeader: false}:
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(chanTimeout):
					// channel满时，记录警告并丢弃数据
					slog.Warn("数据通道满，丢弃数据行", "line", outputLine[:min(len(outputLine), 100)])
				}
			}

			lineCount++
			if lineCount%10000 == 0 {
				slog.Info("已处理行数", "lines", lineCount)
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
			slog.Info("批量写入器统计", "processed_lines", totalLines, "dropped_lines", droppedLines)
			return nil
		case dataLine, ok := <-dataChan:
			if !ok {
				// channel已关闭，刷新剩余数据并退出
				if err := flushBatch(); err != nil {
					return err
				}
				slog.Info("批量写入器统计", "processed_lines", totalLines, "dropped_lines", droppedLines)
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
			slog.Info("批量写入器统计", "processed_lines", totalLines, "dropped_lines", droppedLines)
		}
	}
}

func setupLogger(level string) {
	lvl := parseLogLevel(level)
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}

func parseLogLevel(s string) slog.Level {
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
