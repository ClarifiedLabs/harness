//go:build !darwin && !linux

package inputimage

import (
	"fmt"
	"os"
)

func openRegular(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("image path is not a regular file")
	}
	return os.Open(path)
}
