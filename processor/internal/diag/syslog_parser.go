package diag

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	syslogRFC5424 = regexp.MustCompile(`^<(\d+)>\d+\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(?:\[(.*?)\]|-)\s*(.*)$`)
	syslogRFC3164 = regexp.MustCompile(`^([A-Z][a-z]{2})\s+(\d{1,2})\s+(\d{2}:\d{2}:\d{2})\s+(\S+)\s+([^:]+):\s*(.*)$`)
	appPidRe      = regexp.MustCompile(`^([^\[]+)\[(\d+)\]$`)
)

type syslogEntry struct {
	TS       string `json:"ts"`
	Host     string `json:"host"`
	Src      string `json:"src"`
	Level    string `json:"level"`
	App      string `json:"app,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Facility int    `json:"facility,omitempty"`
	Severity int    `json:"severity,omitempty"`
	Msg      string `json:"msg"`
	Raw      string `json:"raw"`
}

func parseSyslogFile(path string, defaultHost string) ([]syslogEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var entries []syslogEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		entry := parseSyslogLine(line, defaultHost)
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func parseSyslogLine(line, defaultHost string) syslogEntry {
	entry := syslogEntry{
		Src: "syslog",
		Raw: line,
	}

	if m := syslogRFC5424.FindStringSubmatch(line); len(m) > 0 {
		pri, _ := strconv.Atoi(m[1])
		ts := parseSyslogTime(m[2])
		entry.TS = ts
		entry.Host = fallbackHost(m[3], defaultHost)
		entry.App = strings.TrimSpace(m[4])
		if pid, err := strconv.Atoi(m[5]); err == nil {
			entry.PID = pid
		}
		entry.Msg = strings.TrimSpace(m[8])
		entry.Facility = pri / 8
		entry.Severity = pri % 8
		entry.Level = severityToLevel(entry.Severity)
		return entry
	}

	if m := syslogRFC3164.FindStringSubmatch(line); len(m) > 0 {
		ts := parseRFC3164Time(m[1], m[2], m[3])
		entry.TS = ts
		entry.Host = fallbackHost(m[4], defaultHost)
		entry.App, entry.PID = splitAppPid(m[5])
		entry.Msg = strings.TrimSpace(m[6])
		entry.Level = inferLevel(entry.Msg)
		return entry
	}

	entry.TS = time.Now().In(diagLocation()).Format(time.RFC3339Nano)
	entry.Host = defaultHost
	entry.Level = inferLevel(line)
	entry.Msg = line
	return entry
}

func parseSyslogTime(value string) string {
	if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ts.In(diagLocation()).Format(time.RFC3339Nano)
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts.In(diagLocation()).Format(time.RFC3339Nano)
	}
	return time.Now().In(diagLocation()).Format(time.RFC3339Nano)
}

func parseRFC3164Time(month, day, clock string) string {
	year := time.Now().In(diagLocation()).Year()
	raw := fmt.Sprintf("%d %s %s %s", year, month, day, clock)
	ts, err := time.ParseInLocation("2006 Jan 2 15:04:05", raw, diagLocation())
	if err != nil {
		return time.Now().In(diagLocation()).Format(time.RFC3339Nano)
	}
	return ts.In(diagLocation()).Format(time.RFC3339Nano)
}

func splitAppPid(value string) (string, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0
	}
	if m := appPidRe.FindStringSubmatch(value); len(m) > 0 {
		pid, _ := strconv.Atoi(m[2])
		return strings.TrimSpace(m[1]), pid
	}
	return value, 0
}

func severityToLevel(sev int) string {
	switch sev {
	case 0, 1, 2, 3:
		return "error"
	case 4:
		return "warn"
	case 5:
		return "notice"
	case 6:
		return "info"
	case 7:
		return "debug"
	default:
		return "info"
	}
}

func inferLevel(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal"):
		return "error"
	case strings.Contains(lower, "warn"):
		return "warn"
	case strings.Contains(lower, "debug"):
		return "debug"
	default:
		return "info"
	}
}

func fallbackHost(h, def string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return def
	}
	def = strings.TrimSpace(def)
	if def == "" {
		return h
	}
	// If syslog provides a short hostname that matches our local short name,
	// prefer the FQDN to keep host identifiers consistent.
	if !strings.Contains(h, ".") {
		short := def
		if i := strings.IndexByte(def, '.'); i > 0 {
			short = def[:i]
		}
		if h == short {
			return def
		}
	}
	return h
}
