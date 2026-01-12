package diag

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type procMetric struct {
	TS      string
	Host    string
	Src     string
	Level   string
	Msg     string
	Payload map[string]interface{}
}

var procNames = []string{"pmacctd", "nfacctd", "processor"}

type procSnapshot struct {
	PID        int
	Name       string
	State      string
	PPID       int
	UTicks     uint64
	STicks     uint64
	TotalTicks uint64
	VSizeBytes uint64
	RSSPages   int64
	Threads    int
	StartTicks uint64
	Cmdline    string
	FDCount    int
	ReadBytes  uint64
	WriteBytes uint64
	CancelWB   uint64
}

func collectProcMetricsSnapshot(prev procMetricState) ([]procMetric, procMetricState) {
	totalJiffies, cpuCores := readTotalJiffies()
	snapshots := readProcSnapshots()
	if prev.ProcTicks == nil {
		prev.ProcTicks = map[int]uint64{}
	}

	next := procMetricState{
		TotalJiffies: totalJiffies,
		ProcTicks:    map[int]uint64{},
		ProcName:     map[int]string{},
		SampleTime:   time.Now().Unix(),
	}

	deltaTotal := float64(0)
	if totalJiffies > prev.TotalJiffies {
		deltaTotal = float64(totalJiffies - prev.TotalJiffies)
	}

	var metrics []procMetric
	for _, snap := range snapshots {
		prevTicks := prev.ProcTicks[snap.PID]
		next.ProcTicks[snap.PID] = snap.TotalTicks
		next.ProcName[snap.PID] = snap.Name

		cpuPct := 0.0
		if deltaTotal > 0 && snap.TotalTicks >= prevTicks {
			cpuPct = (float64(snap.TotalTicks-prevTicks) / deltaTotal) * 100.0
		}

		rssKB := int64(snap.RSSPages) * int64(os.Getpagesize()) / 1024
		vsizeKB := int64(snap.VSizeBytes / 1024)

		payload := map[string]interface{}{
			"name":                     snap.Name,
			"pid":                      snap.PID,
			"state":                    snap.State,
			"ppid":                     snap.PPID,
			"cpu_pct":                  round2(cpuPct),
			"cpu_ticks":                snap.TotalTicks,
			"total_jiffies":            totalJiffies,
			"cpu_cores":                cpuCores,
			"rss_kb":                   rssKB,
			"vsize_kb":                 vsizeKB,
			"threads":                  snap.Threads,
			"fd_count":                 snap.FDCount,
			"start_time_tick":          snap.StartTicks,
			"cmdline":                  snap.Cmdline,
			"io_read_bytes":            snap.ReadBytes,
			"io_write_bytes":           snap.WriteBytes,
			"io_cancelled_write_bytes": snap.CancelWB,
		}

		metrics = append(metrics, procMetric{
			TS:      time.Now().UTC().Format(time.RFC3339Nano),
			Level:   "info",
			Msg:     "metrics",
			Payload: payload,
		})
	}

	return metrics, next
}

func readTotalJiffies() (uint64, int) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var total uint64
	for _, v := range fields[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err == nil {
			total += n
		}
	}
	cores := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			cores++
		}
	}
	return total, cores
}

func readProcSnapshots() []procSnapshot {
	var snaps []procSnapshot
	for _, name := range procNames {
		pids := findPidsByName(name)
		for _, pid := range pids {
			if snap, ok := readProcSnapshot(pid); ok {
				snaps = append(snaps, snap)
			}
		}
	}
	return snaps
}

func findPidsByName(name string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == name {
			pids = append(pids, pid)
		}
	}
	return pids
}

func readProcSnapshot(pid int) (procSnapshot, bool) {
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	raw, err := os.ReadFile(statPath)
	if err != nil {
		return procSnapshot{}, false
	}
	line := string(raw)
	open := strings.Index(line, "(")
	close := strings.LastIndex(line, ")")
	if open < 0 || close < 0 || close <= open {
		return procSnapshot{}, false
	}
	comm := line[open+1 : close]
	rest := strings.Fields(line[close+1:])
	if len(rest) < 22 {
		return procSnapshot{}, false
	}
	state := rest[0]
	ppid := atoiDefault(rest[1], 0)
	utime := atou64Default(rest[11], 0)
	stime := atou64Default(rest[12], 0)
	start := atou64Default(rest[19], 0)
	threads := atoiDefault(rest[17], 0)
	vsize := atou64Default(rest[20], 0)
	rss := atoi64Default(rest[21], 0)

	cmdline := readCmdline(pid)
	fdCount := countFDs(pid)
	readBytes, writeBytes, cancelWB := readProcIO(pid)

	return procSnapshot{
		PID:        pid,
		Name:       comm,
		State:      state,
		PPID:       ppid,
		UTicks:     utime,
		STicks:     stime,
		TotalTicks: utime + stime,
		VSizeBytes: vsize,
		RSSPages:   rss,
		Threads:    threads,
		StartTicks: start,
		Cmdline:    cmdline,
		FDCount:    fdCount,
		ReadBytes:  readBytes,
		WriteBytes: writeBytes,
		CancelWB:   cancelWB,
	}, true
}

func readCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return ""
	}
	parts := strings.Split(string(data), "\x00")
	return strings.TrimSpace(strings.Join(parts, " "))
}

func countFDs(pid int) int {
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return 0
	}
	return len(entries)
}

func readProcIO(pid int) (uint64, uint64, uint64) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "io"))
	if err != nil {
		return 0, 0, 0
	}
	var readBytes, writeBytes, cancelWB uint64
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		num := atou64Default(val, 0)
		switch key {
		case "read_bytes":
			readBytes = num
		case "write_bytes":
			writeBytes = num
		case "cancelled_write_bytes":
			cancelWB = num
		}
	}
	return readBytes, writeBytes, cancelWB
}

func atoiDefault(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func atoi64Default(v string, def int64) int64 {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	return def
}

func atou64Default(v string, def uint64) uint64 {
	if n, err := strconv.ParseUint(v, 10, 64); err == nil {
		return n
	}
	return def
}

func round2(v float64) float64 {
	return mathRound(v*100) / 100
}

func mathRound(v float64) float64 {
	if v < 0 {
		return -mathRound(-v)
	}
	return float64(int(v + 0.5))
}
