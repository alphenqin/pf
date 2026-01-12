package diag

import (
	"encoding/json"
	"os"
	"strings"
)

func loadStringSet(path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]bool{}
	}
	lines := strings.Split(string(data), "\n")
	set := make(map[string]bool, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[line] = true
	}
	return set
}

func writeStringSet(path string, set map[string]bool) error {
	var b strings.Builder
	for k := range set {
		b.WriteString(k)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func readStringFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

type procMetricState struct {
	TotalJiffies uint64         `json:"total_jiffies"`
	ProcTicks    map[int]uint64 `json:"proc_ticks"`
	ProcName     map[int]string `json:"proc_name,omitempty"`
	SampleTime   int64          `json:"sample_time,omitempty"`
}

func loadProcMetricState(path string) procMetricState {
	data, err := os.ReadFile(path)
	if err != nil {
		return procMetricState{ProcTicks: map[int]uint64{}, ProcName: map[int]string{}}
	}
	var res procMetricState
	if err := json.Unmarshal(data, &res); err != nil {
		return procMetricState{ProcTicks: map[int]uint64{}, ProcName: map[int]string{}}
	}
	if res.ProcTicks == nil {
		res.ProcTicks = map[int]uint64{}
	}
	if res.ProcName == nil {
		res.ProcName = map[int]string{}
	}
	return res
}

func saveProcMetricState(path string, data procMetricState) error {
	blob, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0644)
}
