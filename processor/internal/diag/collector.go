package diag

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pmacct/processor/internal/config"
	"github.com/pmacct/processor/internal/host"
)

type Collector struct {
	ctx      context.Context
	cfg      config.DiagConfig
	dataDir  string
	stopChan chan struct{}
	doneChan chan struct{}
	host     string

	procPayloadEnricher func(procName string) map[string]interface{}
	procCSVStats       func(procName string) (int64, int64)
}

const (
	diagHostLogDir = "/var/lib/processor/log"
	diagProcLogDir = "/var/log/pmacct"
)

func NewCollector(ctx context.Context, cfg config.DiagConfig, dataDir string) *Collector {
	return &Collector{
		ctx:      ctx,
		cfg:      cfg,
		dataDir:  dataDir,
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
		host:     host.FQDN(),
	}
}

// SetProcPayloadEnricher sets an optional payload enricher for proc metrics.
// Call before Start().
func (c *Collector) SetProcPayloadEnricher(fn func(procName string) map[string]interface{}) {
	c.procPayloadEnricher = fn
}

// SetProcCSVStats sets optional CSV counters for proc metrics.
// Call before Start().
func (c *Collector) SetProcCSVStats(fn func(procName string) (int64, int64)) {
	c.procCSVStats = fn
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
	outDir := c.dataDir
	stateDir := filepath.Join(c.dataDir, "diag")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		slog.Warn("diag: 创建目录失败", "err", err)
		return
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		slog.Warn("diag: 创建状态目录失败", "err", err)
		return
	}

	ts := time.Now().In(diagLocation()).Format("20060102T150405+0800")
	diagOut := filepath.Join(outDir, fmt.Sprintf("diag_%s_%s.json.gz", c.host, ts))

	syslogEntries, _ := c.collectSyslogEntries(stateDir)
	procMetrics, _ := c.collectProcMetrics(stateDir)
	envData, _, envAvailable, envPath := c.readEnvData(stateDir)

	if len(syslogEntries) == 0 && len(procMetrics) == 0 && !envAvailable {
		return
	}
	if err := writeDiagJSON(diagOut, syslogEntries, procMetrics, envData, envAvailable); err != nil {
		slog.Warn("diag: 写入诊断文件失败", "err", err)
		return
	}
	if err := cleanupDiagSources(stateDir, envPath); err != nil {
		slog.Warn("diag: 清理源文件失败", "err", err)
	}
	slog.Info("diag: 已生成诊断文件", "file", filepath.Base(diagOut), "syslog", len(syslogEntries), "proc", len(procMetrics))
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
		if strings.HasPrefix(name, "syslog_") && strings.HasSuffix(name, ".log") {
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
			slog.Warn("diag: 解析系统日志失败", "path", path, "err", err)
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
			slog.Warn("diag: 保存系统日志状态失败", "err", err)
		}
	}
	return all, changed
}

func (c *Collector) readEnvData(outDir string) (map[string]interface{}, bool, bool, string) {
	envPath, envFound := findLatestEnvFile(diagHostLogDir)
	if !envFound {
		return nil, false, false, ""
	}
	envStatePath := filepath.Join(outDir, "env_latest.state")
	lastEnv := readStringFile(envStatePath)
	envChanged := envPath != lastEnv

	data, err := readFirstJSONLine(envPath)
	if err != nil {
		slog.Warn("diag: 读取环境信息失败", "path", envPath, "err", err)
		return nil, false, true, envPath
	}
	data["src"] = "env"
	if _, ok := data["ts"]; !ok {
		if execTime, ok := data["exec_time"]; ok {
			if ts := unixSecondsToRFC3339(execTime); ts != "" {
				data["ts"] = ts
			}
		}
	}
	if _, ok := data["host"]; !ok {
		if host := extractHostnameFromHostInfo(data); host != "" {
			data["host"] = host
		} else {
			data["host"] = c.host
		}
	}
	if envChanged {
		_ = os.WriteFile(envStatePath, []byte(envPath), 0644)
	}
	return data, envChanged, true, envPath
}

func unixSecondsToRFC3339(v interface{}) string {
	switch t := v.(type) {
	case int64:
		return time.Unix(t, 0).In(diagLocation()).Format(time.RFC3339Nano)
	case int:
		return time.Unix(int64(t), 0).In(diagLocation()).Format(time.RFC3339Nano)
	case float64:
		return time.Unix(int64(t), 0).In(diagLocation()).Format(time.RFC3339Nano)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return time.Unix(n, 0).In(diagLocation()).Format(time.RFC3339Nano)
		}
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return time.Unix(n, 0).In(diagLocation()).Format(time.RFC3339Nano)
		}
	}
	return ""
}

func extractHostnameFromHostInfo(data map[string]interface{}) string {
	raw, ok := data["host_info"]
	if !ok {
		return ""
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	if v, ok := m["hostname"]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func (c *Collector) collectProcMetrics(outDir string) ([]procMetric, bool) {
	statePath := filepath.Join(outDir, "proc_metrics.state.json")
	prev := loadProcMetricState(statePath)
	metrics, next := collectProcMetricsSnapshot(prev)
	if err := saveProcMetricState(statePath, next); err != nil {
		slog.Warn("diag: 保存进程状态失败", "err", err)
	}
	if len(metrics) == 0 {
		return nil, false
	}
	for i := range metrics {
		metrics[i].Host = c.host
		metrics[i].Src = "proc"
		if c.procPayloadEnricher != nil && metrics[i].Payload != nil {
			if name, ok := metrics[i].Payload["name"].(string); ok {
				if extra := c.procPayloadEnricher(name); len(extra) > 0 {
					for k, v := range extra {
						metrics[i].Payload[k] = v
					}
				}
			}
		}
		if c.procCSVStats != nil && metrics[i].Payload != nil {
			if name, ok := metrics[i].Payload["name"].(string); ok {
				csvTotal, csvDNS := c.procCSVStats(name)
				metrics[i].CSVTotal = csvTotal
				metrics[i].CSVDNS = csvDNS
			}
		}
	}
	metrics = aggregateProcMetrics(metrics)
	return metrics, true
}

func aggregateProcMetrics(metrics []procMetric) []procMetric {
	if len(metrics) <= 1 {
		return metrics
	}
	type agg struct {
		base       procMetric
		cpuPct     float64
		cpuTicks   int64
		rssKB      int64
		vsizeKB    int64
		threads    int64
		fdCount    int64
		ioRead     int64
		ioWrite    int64
		ioCancelWB int64
		csvTotal   int64
		csvDNS     int64
		startMin   int64
		pids       []int
		ppids      []int
		cmdlines   []string
		state      string
		mixedState bool
	}

	byName := map[string]*agg{}
	for _, m := range metrics {
		name, _ := m.Payload["name"].(string)
		if name == "" {
			continue
		}
		a, ok := byName[name]
		if !ok {
			basePayload := map[string]interface{}{}
			for k, v := range m.Payload {
				basePayload[k] = v
			}
			a = &agg{
				base:     procMetric{TS: m.TS, Host: m.Host, Src: m.Src, Level: m.Level, Msg: m.Msg, Payload: basePayload},
				startMin: getInt64(m.Payload["start_time_tick"]),
				state:    getString(m.Payload["state"]),
			}
			byName[name] = a
		} else {
			if st := getString(m.Payload["state"]); st != "" && st != a.state {
				a.mixedState = true
			}
		}

		a.cpuPct += getFloat64(m.Payload["cpu_pct"])
		a.cpuTicks += getInt64(m.Payload["cpu_ticks"])
		a.rssKB += getInt64(m.Payload["rss_kb"])
		a.vsizeKB += getInt64(m.Payload["vsize_kb"])
		a.threads += getInt64(m.Payload["threads"])
		a.fdCount += getInt64(m.Payload["fd_count"])
		a.ioRead += getInt64(m.Payload["io_read_bytes"])
		a.ioWrite += getInt64(m.Payload["io_write_bytes"])
		a.ioCancelWB += getInt64(m.Payload["io_cancelled_write_bytes"])
		a.csvTotal += m.CSVTotal
		a.csvDNS += m.CSVDNS

		if st := getInt64(m.Payload["start_time_tick"]); st > 0 && (a.startMin == 0 || st < a.startMin) {
			a.startMin = st
		}
		if pid := getInt64(m.Payload["pid"]); pid > 0 {
			a.pids = append(a.pids, int(pid))
		}
		if ppid := getInt64(m.Payload["ppid"]); ppid > 0 {
			a.ppids = append(a.ppids, int(ppid))
		}
		if cmd := getString(m.Payload["cmdline"]); cmd != "" {
			a.cmdlines = append(a.cmdlines, cmd)
		}
	}

	var out []procMetric
	for _, a := range byName {
		payload := a.base.Payload
		payload["cpu_pct"] = round2(a.cpuPct)
		payload["cpu_ticks"] = a.cpuTicks
		payload["rss_kb"] = a.rssKB
		payload["vsize_kb"] = a.vsizeKB
		payload["threads"] = a.threads
		payload["fd_count"] = a.fdCount
		payload["io_read_bytes"] = a.ioRead
		payload["io_write_bytes"] = a.ioWrite
		payload["io_cancelled_write_bytes"] = a.ioCancelWB
		a.base.CSVTotal = a.csvTotal
		a.base.CSVDNS = a.csvDNS
		if a.startMin > 0 {
			payload["start_time_tick"] = a.startMin
		}
		if len(a.pids) > 0 {
			payload["pid_list"] = a.pids
			payload["instances"] = len(a.pids)
		}
		if len(a.ppids) > 0 {
			payload["ppid_list"] = a.ppids
		}
		if len(a.cmdlines) > 0 {
			payload["cmdline_list"] = a.cmdlines
		}
		if a.mixedState {
			payload["state"] = "mixed"
		}
		out = append(out, a.base)
	}
	return out
}

func getFloat64(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case int32:
		return float64(t)
	case uint32:
		return float64(t)
	default:
		return 0
	}
}

func getInt64(v interface{}) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case uint64:
		return int64(t)
	case int32:
		return int64(t)
	case uint32:
		return int64(t)
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	default:
		return 0
	}
}

func getString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func readFirstJSONLine(path string) (map[string]interface{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf, err := io.ReadAll(f)
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
	src string
}

func writeDiagJSON(path string, syslogEntries []syslogEntry, procMetrics []procMetric, envData map[string]interface{}, envAvailable bool) error {
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
		entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx, src: e.Src})
		idx++
	}
	for _, e := range procMetrics {
		ts := parseEntryTime(e.TS)
		rec := diagRecord{
			TS:      e.TS,
			Host:    e.Host,
			Src:     e.Src,
			Level:   e.Level,
			Msg:     e.Msg,
			Payload: e.Payload,
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx, src: e.Src})
		idx++
	}
	if len(procMetrics) > 0 {
		var totalCSV int64
		var totalDNS int64
		first := procMetrics[0]
		for _, e := range procMetrics {
			totalCSV += e.CSVTotal
			totalDNS += e.CSVDNS
		}
		rec := diagRecord{
			TS:    first.TS,
			Host:  first.Host,
			Src:   "count",
			Level: first.Level,
			Msg:   first.Msg,
			Payload: map[string]interface{}{
				"csv_total": totalCSV,
				"csv_dns":   totalDNS,
			},
		}
		raw, err := json.Marshal(rec)
		if err == nil {
			ts := parseEntryTime(rec.TS)
			entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx, src: rec.Src})
			idx++
		}
	}
	if envAvailable {
		rec := buildEnvRecord(envData)
		raw, err := json.Marshal(rec)
		if err == nil {
			ts := parseEntryTime(rec.TS)
			entries = append(entries, diagEntry{ts: ts, raw: raw, idx: idx, src: rec.Src})
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

	seenByTS := make(map[int64]map[string]struct{})
	deduped := entries[:0]
	for _, e := range entries {
		if e.src != "syslog" {
			deduped = append(deduped, e)
			continue
		}
		tsKey := int64(0)
		if !e.ts.IsZero() {
			tsKey = e.ts.UnixNano()
		}
		seen, ok := seenByTS[tsKey]
		if !ok {
			seen = make(map[string]struct{})
			seenByTS[tsKey] = seen
		}
		key := string(e.raw)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, e)
	}
	entries = deduped

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
		return t.In(diagLocation())
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.In(diagLocation())
	}
	return time.Time{}
}

func cleanupDiagSources(stateDir string, envPath string) error {
	processedPath := filepath.Join(stateDir, "syslog_processed.list")
	envStatePath := filepath.Join(stateDir, "env_latest.state")

	processed := loadStringSet(processedPath)
	for path := range processed {
		if path == "" {
			continue
		}
		_ = os.Remove(path)
	}
	_ = os.WriteFile(processedPath, []byte{}, 0644)
	_ = os.Remove(envStatePath)
	if envPath != "" {
		_ = os.Remove(envPath)
	}
	return nil
}

type diagRecord struct {
	TS       string                 `json:"ts"`
	Host     string                 `json:"host"`
	Src      string                 `json:"src"`
	Level    string                 `json:"level,omitempty"`
	Msg      string                 `json:"msg,omitempty"`
	Payload  map[string]interface{} `json:"payload,omitempty"`
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
		if strings.HasPrefix(name, "env_") && strings.HasSuffix(name, ".json") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return "", false
	}
	sort.Strings(files)
	return files[len(files)-1], true
}
