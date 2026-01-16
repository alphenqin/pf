package host

import (
	"os"
	"os/exec"
	"strings"
)

// FQDN returns a long hostname if possible, falling back to shorter forms.
func FQDN() string {
	if v := run("hostname", "-f"); v != "" {
		return v
	}
	if v := run("uname", "-n"); v != "" {
		return v
	}
	if v := strings.TrimSpace(getHostname()); v != "" {
		return v
	}
	return "unknown"
}

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getHostname() string {
	h, _ := os.Hostname()
	return h
}
