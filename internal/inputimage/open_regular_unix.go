//go:build darwin || linux

package inputimage

import (
	"os"
	"syscall"
)

func openRegular(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
