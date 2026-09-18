//go:build !darwin && !linux

package codexclient

// osVersion is unavailable on this platform; the User-Agent omits the version
// field rather than guessing.
func osVersion() string { return "" }
