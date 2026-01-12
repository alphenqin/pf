package diag

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pmacct/processor/internal/config"
)

type Collector struct {
	ctx      context.Context
	cfg      config.DiagConfig
	dataDir  string
	stopChan chan struct{}
	doneChan chan struct{}
	host     string
}

const (
	diagHostLogDir = "/var/lib/processor/log"
	diagProcLogDir = "/var/log/pmacct"
	diagOutSubDir  = "diag"
)

func NewCollector(ctx context.Context, cfg config.DiagConfig, dataDir string) *Collector {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return &Collector{
		ctx:      ctx,
		cfg:      cfg,
		dataDir:  dataDir,
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
		host:     host,
	}
}

func (c *Collector) Start() {
	go c.run()
}

func (c *Collector) Stop() {
	close(c.stopChan)
	<-c.doneChan
}

func (c *Collector) run() {
	defer close(c.doneChan)
	ticker := time.NewTicker(time.Duration(c.cfg.IntervalSec) * time.Second)
	defer ticker.Stop()

	c.collectOnce()
	for {
		select {
		case <-ticker.C:
			c.collectOnce()
		case <-c.stopChan:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Collector) collectOnce() {
	outDir := filepath.Join(c.dataDir, diagOutSubDir)
	tmpDir := filepath.Join(outDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		log.Printf("[WARN] diag: 创建目录失败: %v", err)
		return
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	diagOut := filepath.Join(outDir, fmt.Sprintf("diag_%s_%s_v1.json.gz", c.host, ts))

	syslogEntries, syslogChanged := c.collectSyslogEntries(outDir)
	procEntries, procChanged := c.collectProcEntries(outDir)
	envData, envChanged, envAvailable := c.readEnvData(outDir)

	shouldWrite := syslogChanged || procChanged || envChanged
	if !shouldWrite {
		return
	}
	if err := writeDiagJSON(diagOut, syslogEntries, procEntries, envData, envAvailable); err != nil {
		log.Printf("[WARN] diag: 写入诊断文件失败: %v", err)
		return
	}
	log.Printf("[INFO] diag: 已生成诊断文件: %s (syslog=%d, proc=%d)", filepath.Base(diagOut), len(syslogEntries), len(procEntries))
}

func (c *Collector) collectSyslogEntries(outDir string) ([]syslogEntry, bool) {
	processedPath := filepath.Join(outDir, "syslog_processed.list")
	processed := loadStringSet(processedPath)

	files, err := os.ReadDir(diagHostLogDir)
	if err != nil {
		return nil, false
	}
	var targets []string
	for _, entry := range files {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "syslog_") && strings.HasSuffix(name, ".log.gz") {
			targets = append(targets, filepath.Join(diagHostLogDir, name))
		}
	}
	sort.Strings(targets)

	changed := false
	var all []syslogEntry
	for _, path := range targets {
		if processed[path] {
			continue
		}
		entries, err := parseSyslogFile(path, c.host)
		if err != nil {
			log.Printf("[WARN] diag: 解析系统日志失败: %s -> %v", path, err)
			continue
		}
		if len(entries) > 0 {
			all = append(all, entries...)
			changed = true
		}
		processed[path] = true
	}

	if changed {
		if err := writeStringSet(processedPath, processed); err != nil {
			log.Printf("[WARN] diag: 保存系统日志状态失败: %v", err)
		}
	}
	return all, changed
}

func (c *Collector) readEnvData(outDir string) (map[string]interface{}, bool, bool) {
	envPath, envFound := findLatestEnvFile(diagHostLogDir)
	if !envFound {
		return nil, false, false
	}
	envStatePath := filepath.Join(outDir, "env_latest.state")
	lastEnv := readStringFile(envStatePath)
	envChanged := envPath != lastEnv

	data, err := readFirstJSONLine(envPath)
	if err != nil {
		log.Printf("[WARN] diag: 读取环境信息失败: %s -> %v", envPath, err)
		return nil, false, true
	}
	data["src"] = "env"
	if _, ok := data["host"]; !ok {
		data["host"] = c.host
	}
	if envChanged {
		_ = os.WriteFile(envStatePath, []byte(envPath), 0644)
	}
	return data, envChanged, true
}

func (c *Collector) collectProcEntries(outDir string) ([]procEntry, bool) {
	offsetPath := filepath.Join(outDir, "proc_offsets.json")
	offsets := loadProcOffsets(offsetPath)

	entries, err := os.ReadDir(diagProcLogDir)
	if err != nil {
		return nil, false
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".err.log") {
			files = append(files, filepath.Join(diagProcLogDir, name))
		}
	}
	sort.Strings(files)

	changed := false
	var all []procEntry
	for _, path := range files {
		procEntries, newOffset, inode, err := parseProcFile(path, c.host, offsets[path])
		if err != nil {
			log.Printf("[WARN] diag: 解析进程日志失败: %s -> %v", path, err)
			continue
		}
		if len(procEntries) > 0 {
			all = append(all, procEntries...)
			changed = true
		}
		if inode != 0 {
			offsets[path] = procOffsetState{Inode: inode, Offset: newOffset}
		}
	}
	if changed {
		if err := saveProcOffsets(offsetPath, offsets); err != nil {
			log.Printf("[WARN] diag: 保存进程日志状态失败: %v", err)
		}
	}
	return all, changed
}

func readFirstJSONLine(path string) (map[string]interface{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gr.Close()

	buf, err := io.ReadAll(gr)
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(string(buf))
	if line == "" {
		return nil, fmt.Errorf("empty json")
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(line), &data); err != nil {
		return nil, err
	}
	return data, nil
}

func writeEnvJSON(path string, data map[string]interface{}) error {
	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	gw := gzip.NewWriter(outFile)
	if err := json.NewEncoder(gw).Encode(data); err != nil {
		_ = gw.Close()
		return err
	}
	return gw.Close()
}

type diagEntry struct {
	ts  time.Time
	raw []byte
	idx int
}

func writeDiagJSON(path string, syslogEntries []syslogEntry, procEntries []procEntry, envData map[string]interface{}, envAvailable bool) error {
	var entries []diagEntry
	idx := 0
	for _, e := range syslogEntries {
		ts := parseEntryTime(e.TS)
		rec := diagRecord{
			TS:    e.TS,
			Host:  e.Host,
			Src:   e.Src,
			Level: e.Level,
			Msg:   e.Msg,
			Payload: map[string]interface{}{
				"app":      e.App,
				"pid":      e.PID,
				"facility": e.Facility,
				"severity": e.Severity,
				"raw":      e.Raw,
			},
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx})
		idx++
	}
	for _, e := range procEntries {
		ts := parseEntryTime(e.TS)
		rec := diagRecord{
			TS:    e.TS,
			Host:  e.Host,
			Src:   e.Src,
			Level: e.Level,
			Msg:   e.Msg,
			Payload: map[string]interface{}{
				"app":  e.App,
				"tag":  e.Tag,
				"file": e.File,
				"raw":  e.Raw,
			},
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx})
		idx++
	}
	if envAvailable {
		rec := buildEnvRecord(envData)
		raw, err := json.Marshal(rec)
		if err == nil {
			ts := parseEntryTime(rec.TS)
			entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx})
		}
	}
	if len(entries) == 0 {
		return nil
	}

	sort.SliceStable(entries, func(i, j int) bool {
		ti, tj := entries[i].ts, entries[j].ts
		if ti.IsZero() && tj.IsZero() {
			return entries[i].idx < entries[j].idx
		}
		if ti.IsZero() {
			return false
		}
		if tj.IsZero() {
			return true
		}
		if ti.Equal(tj) {
			return entries[i].idx < entries[j].idx
		}
		return ti.Before(tj)
	})

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	gw := gzip.NewWriter(out)
	defer gw.Close()

	for _, e := range entries {
		if _, err := gw.Write(e.raw); err != nil {
			return err
		}
		if _, err := gw.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

func parseEntryTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

type diagRecord struct {
	TS      string                 `json:"ts"`
	Host    string                 `json:"host"`
	Src     string                 `json:"src"`
	Level   string                 `json:"level,omitempty"`
	Msg     string                 `json:"msg,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

func buildEnvRecord(envData map[string]interface{}) diagRecord {
	rec := diagRecord{
		Src:   "env",
		Level: "info",
		Msg:   "snapshot",
	}
	payload := map[string]interface{}{}
	for k, v := range envData {
		switch k {
		case "ts":
			rec.TS = fmt.Sprint(v)
		case "host":
			rec.Host = fmt.Sprint(v)
		case "src":
			// ignore
		default:
			payload[k] = v
		}
	}
	rec.Payload = payload
	return rec
}

func findLatestEnvFile(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "env_") && strings.HasSuffix(name, ".json.gz") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return "", false
	}
	sort.Strings(files)
	return files[len(files)-1], true
}
