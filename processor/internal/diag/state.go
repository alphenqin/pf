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

type procOffsetState struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

func loadProcOffsets(path string) map[string]procOffsetState {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]procOffsetState{}
	}
	var res map[string]procOffsetState
	if err := json.Unmarshal(data, &res); err != nil {
		return map[string]procOffsetState{}
	}
	return res
}

func saveProcOffsets(path string, data map[string]procOffsetState) error {
	blob, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0644)
}
