package diag

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	procFmt1 = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}) \[([a-zA-Z]+)\] \[([^\]]+)\] (.*)$`)
	procFmt2 = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)
)

type procEntry struct {
	TS    string `json:"ts"`
	Host  string `json:"host"`
	Src   string `json:"src"`
	Level string `json:"level"`
	App   string `json:"app,omitempty"`
	Tag   string `json:"tag,omitempty"`
	File  string `json:"file,omitempty"`
	Msg   string `json:"msg"`
	Raw   string `json:"raw"`
}

func parseProcFile(path string, host string, prev procOffsetState) ([]procEntry, int64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, 0, 0, fmt.Errorf("stat unsupported")
	}
	inode := stat.Ino
	size := info.Size()
	offset := prev.Offset
	if prev.Inode != inode || offset > size {
		offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, inode, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, inode, err
	}

	reader := bufio.NewReader(f)
	var entries []procEntry
	currentOffset := offset
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			currentOffset += int64(len(line))
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, currentOffset, inode, err
				}
				continue
			}
			entry := parseProcLine(line, host, filepath.Base(path))
			entries = append(entries, entry)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, currentOffset, inode, err
		}
	}

	return entries, currentOffset, inode, nil
}

func parseProcLine(line, host, file string) procEntry {
	entry := procEntry{
		Host: host,
		Src:  "proc",
		File: file,
		Raw:  line,
		Msg:  line,
	}

	if m := procFmt1.FindStringSubmatch(line); len(m) > 0 {
		ts, err := time.ParseInLocation("2006-01-02 15:04:05.000", m[1], time.Local)
		if err == nil {
			entry.TS = ts.UTC().Format(time.RFC3339Nano)
		} else {
			entry.TS = time.Now().UTC().Format(time.RFC3339Nano)
		}
		entry.Level = strings.ToLower(m[2])
		entry.Tag = m[3]
		entry.Msg = strings.TrimSpace(m[4])
		entry.App = procAppFromFile(file)
		return entry
	}
	if m := procFmt2.FindStringSubmatch(line); len(m) > 0 {
		ts, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.Local)
		if err == nil {
			entry.TS = ts.UTC().Format(time.RFC3339Nano)
		} else {
			entry.TS = time.Now().UTC().Format(time.RFC3339Nano)
		}
		entry.Msg = strings.TrimSpace(m[2])
		entry.Level = inferLevel(entry.Msg)
		entry.App = procAppFromFile(file)
		return entry
	}

	entry.TS = time.Now().UTC().Format(time.RFC3339Nano)
	entry.Level = inferLevel(line)
	entry.App = procAppFromFile(file)
	return entry
}

func procAppFromFile(file string) string {
	base := strings.TrimSuffix(file, filepath.Ext(file))
	base = strings.TrimSuffix(base, ".err")
	return base
}
