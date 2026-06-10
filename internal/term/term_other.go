//go:build !darwin && !linux

package term

import (
	"os"
	"time"
)

// Reset is a no-op on platforms without termios support.
func Reset() error {
	return nil
}

func SetBracketedPaste(enabled bool) error {
	return nil
}

func EnableCtrlGLineEnd() (func() error, error) {
	return func() error { return nil }, nil
}

func EnablePromptRawMode() (func() error, error) {
	return func() error { return nil }, nil
}

func WaitReadable(f *os.File, timeout time.Duration) bool {
	return false
}

func EnableEscLineEnd() (func() error, error) {
	return func() error { return nil }, nil
}

func Size() (rows, cols int, ok bool) {
	return 0, 0, false
}

func IsTerminal(f *os.File) bool {
	return false
}
