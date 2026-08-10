//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package session

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

var errLockHeld = errors.New("session lock held")

func lockSessionFile(*os.File) error {
	return fmt.Errorf("session locking is unsupported on %s", runtime.GOOS)
}

func unlockSessionFile(*os.File) error { return nil }
