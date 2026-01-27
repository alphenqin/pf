package validator

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// ValidateLine validates a CSV line with 11 columns:
// SRC_IP,DST_IP,SRC_PORT,DST_PORT,TCP_FLAGS,PROTOCOL,TOS,TIMESTAMP_MIN,TIMESTAMP_MAX,PACKETS,BYTES
func ValidateLine(line string, now time.Time) (bool, string) {
	fields := strings.Split(line, ",")
	if len(fields) != 11 {
		return false, "column count != 11"
	}

	sip := strings.TrimSpace(fields[0])
	dip := strings.TrimSpace(fields[1])
	if ip := net.ParseIP(sip); ip == nil || ip.To4() == nil {
		return false, "SRC_IP is not valid"
	}
	if ip := net.ParseIP(dip); ip == nil || ip.To4() == nil {
		return false, "DST_IP is not valid"
	}

	if !validPort(fields[2]) {
		return false, "SRC_PORT is not valid"
	}
	if !validPort(fields[3]) {
		return false, "DST_PORT is not valid"
	}
	if !validByte(fields[4]) {
		return false, "TCP_FLAGS is not valid"
	}
	if !validByte(fields[5]) {
		return false, "PROTOCOL is not valid"
	}
	if !validByte(fields[6]) {
		return false, "TOS is not valid"
	}

	tmin, ok := parseEpochSeconds(fields[7])
	if !ok {
		return false, "TIMESTAMP_MIN is not valid"
	}
	tmax, ok := parseEpochSeconds(fields[8])
	if !ok {
		return false, "TIMESTAMP_MAX is not valid"
	}
	if tmin > tmax {
		return false, "TIMESTAMP_MAX is not valid"
	}
	if tmax > float64(now.UnixNano())/1e9 {
		return false, "TIMESTAMP_MAX is not valid"
	}

	if !validUint(fields[9]) {
		return false, "PACKETS is not valid"
	}
	if !validUint(fields[10]) {
		return false, "BYTES is not valid"
	}

	return true, ""
}

func parseEpochSeconds(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func validPort(s string) bool {
	v, ok := parseInt(s)
	return ok && v >= 0 && v <= 65535
}

func validByte(s string) bool {
	v, ok := parseInt(s)
	return ok && v >= 0 && v <= 255
}

func validUint(s string) bool {
	v, ok := parseInt(s)
	return ok && v >= 0
}

func parseInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
