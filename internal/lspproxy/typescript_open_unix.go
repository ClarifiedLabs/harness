//go:build unix

package lspproxy

import (
	"os"
	"syscall"
)

// openTypeScriptPackageFile prevents a workspace-controlled replacement with a
// symlink and makes FIFO/device races nonblocking before descriptor validation.
func openTypeScriptPackageFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
