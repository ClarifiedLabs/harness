//go:build darwin

package codexclient

import "syscall"

// osVersion returns the macOS product version (e.g. "15.0.1") that the Codex
// CLI reports via os_info.
func osVersion() string {
	value, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return value
}
