//go:build linux

package codexclient

import (
	"os"
	"strings"
)

// osVersion returns the Linux kernel release, the closest stdlib-readable
// analogue of the version the Codex CLI reports via os_info.
func osVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
