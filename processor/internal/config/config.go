package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const processorPrefix = "processor_"

// ProcessorConfig 包含 processor 的所有配置项
// 配置来源：pmacct.conf 中以 processor_ 开头的 key: value 行（可注释）
type ProcessorConfig struct {
	FTPHost              string
	FTPPort              int
	FTPUser              string
	FTPPass              string
	FTPDir               string
	FTPOptions           FTPOptions // FTP选项配置
	RotateIntervalSec    int
	RotateSizeMB         int
	FilePrefix           string
	UploadIntervalSec    int
	DebugPrintInterval   int // 调试打印间隔（行数），默认为0（不打印）
	DebugPrintStartLines int // 调试打印开始行数，前N行会打印，默认为0（不打印）
	StatusReport         StatusReportConfig
}

// FTPOptions FTP选项配置
type FTPOptions struct {
	TimeoutSec int // FTP操作超时时间（秒）
}

// StatusReportConfig 状态上报配置
type StatusReportConfig struct {
	Enabled     bool
	URL         string
	IntervalSec int
	UUID        string
	FilePath    string
	FileMaxMB   int
	FileBackups int
}

// parseProcessorConfig 解析 pmacct.conf 中的 processor_* 配置行
// 仅解析未注释行：
// processor_foo: value
func parseProcessorConfig(content string) map[string]string {
	kv := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, processorPrefix) {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		kv[key] = unquote(val)
	}
	return kv
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "y", "on":
		return true, nil
	case "false", "0", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("非法布尔值: %s", value)
	}
}

// LoadConfig 从 pmacct.conf 中解析 processor 配置项
func LoadConfig(configPath string) (*ProcessorConfig, error) {
	// 检查文件是否存在
	if _, err := os.Stat(configPath); err != nil {
		return nil, fmt.Errorf("配置文件不存在: %w", err)
	}

	fileContent, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	kv := parseProcessorConfig(string(fileContent))
	if len(kv) == 0 {
		return nil, fmt.Errorf("未找到 processor_* 配置项，请在 pmacct.conf 中添加 processor_ 开头的 key: value")
	}

	cfg := &ProcessorConfig{}

	cfg.FTPHost = kv[processorPrefix+"ftp_host"]
	cfg.FTPUser = kv[processorPrefix+"ftp_user"]
	cfg.FTPPass = kv[processorPrefix+"ftp_pass"]
	cfg.FTPDir = kv[processorPrefix+"ftp_dir"]
	cfg.FilePrefix = kv[processorPrefix+"file_prefix"]
	cfg.StatusReport.URL = kv[processorPrefix+"status_report_url"]
	cfg.StatusReport.UUID = kv[processorPrefix+"status_report_uuid"]
	cfg.StatusReport.FilePath = kv[processorPrefix+"status_report_file_path"]

	if v, ok := kv[processorPrefix+"ftp_port"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_ftp_port 不是整数: %w", err)
		} else {
			cfg.FTPPort = num
		}
	}
	if v, ok := kv[processorPrefix+"rotate_interval_sec"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_rotate_interval_sec 不是整数: %w", err)
		} else {
			cfg.RotateIntervalSec = num
		}
	}
	if v, ok := kv[processorPrefix+"rotate_size_mb"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_rotate_size_mb 不是整数: %w", err)
		} else {
			cfg.RotateSizeMB = num
		}
	}
	if v, ok := kv[processorPrefix+"upload_interval_sec"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_upload_interval_sec 不是整数: %w", err)
		} else {
			cfg.UploadIntervalSec = num
		}
	}
	if v, ok := kv[processorPrefix+"status_report_interval_sec"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_status_report_interval_sec 不是整数: %w", err)
		} else {
			cfg.StatusReport.IntervalSec = num
		}
	}
	if v, ok := kv[processorPrefix+"status_report_file_max_mb"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_status_report_file_max_mb 不是整数: %w", err)
		} else {
			cfg.StatusReport.FileMaxMB = num
		}
	}
	if v, ok := kv[processorPrefix+"status_report_file_backups"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_status_report_file_backups 不是整数: %w", err)
		} else {
			cfg.StatusReport.FileBackups = num
		}
	}

	// 解析FTP选项配置
	if v, ok := kv[processorPrefix+"ftp_timeout"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_ftp_timeout 不是整数: %w", err)
		} else {
			cfg.FTPOptions.TimeoutSec = num
		}
	}

	// 解析调试打印间隔配置
	if v, ok := kv[processorPrefix+"debug_print_interval"]; ok {
		if num, err := strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("processor_debug_print_interval 不是整数: %w", err)
		} else {
			cfg.DebugPrintInterval = num
		}
	}
	if v, ok := kv[processorPrefix+"status_report_enabled"]; ok {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("processor_status_report_enabled 解析失败: %w", err)
		}
		cfg.StatusReport.Enabled = b
	}

	// 验证配置
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	return cfg, nil
}

// validateConfig 验证配置的有效性
func validateConfig(cfg *ProcessorConfig) error {
	if cfg.FTPHost == "" {
		return fmt.Errorf("processor_ftp_host 不能为空")
	}
	if cfg.FTPUser == "" {
		return fmt.Errorf("processor_ftp_user 不能为空")
	}
	if cfg.FTPPass == "" {
		return fmt.Errorf("processor_ftp_pass 不能为空")
	}
	if cfg.RotateIntervalSec < 1 {
		return fmt.Errorf("processor_rotate_interval_sec 必须 >= 1")
	}
	if cfg.RotateSizeMB < 1 {
		return fmt.Errorf("processor_rotate_size_mb 必须 >= 1")
	}
	if cfg.UploadIntervalSec < 1 {
		return fmt.Errorf("processor_upload_interval_sec 必须 >= 1")
	}
	if cfg.FilePrefix == "" {
		cfg.FilePrefix = "flows_"
	}
	if cfg.FTPPort == 0 {
		cfg.FTPPort = 21
	}
	if cfg.FTPDir == "" {
		cfg.FTPDir = "/"
	}
	// 设置FTP选项默认值
	if cfg.FTPOptions.TimeoutSec <= 0 {
		cfg.FTPOptions.TimeoutSec = 60 // 默认60秒超时
	}

	// 设置调试打印间隔默认值
	if cfg.DebugPrintInterval < 0 {
		cfg.DebugPrintInterval = 0 // 默认不打印
	}
	if cfg.StatusReport.Enabled {
		if cfg.StatusReport.IntervalSec < 1 {
			cfg.StatusReport.IntervalSec = 60
		}
		// URL 需非空
		if cfg.StatusReport.URL == "" {
			return fmt.Errorf("processor_status_report_url 不能为空（已启用 processor_status_report_enabled=true）")
		}
		if cfg.StatusReport.FileMaxMB <= 0 {
			cfg.StatusReport.FileMaxMB = 10
		}
		if cfg.StatusReport.FileBackups < 0 {
			cfg.StatusReport.FileBackups = 0
		}
	}

	return nil
}

// EnsureDataDir 确保数据目录存在
func EnsureDataDir(dataDir string) error {
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dataDir, 0755); err != nil {
				return fmt.Errorf("创建数据目录失败: %w", err)
			}
			return nil
		}
		return fmt.Errorf("检查数据目录失败: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("数据目录路径不是目录: %s", dataDir)
	}
	return nil
}
