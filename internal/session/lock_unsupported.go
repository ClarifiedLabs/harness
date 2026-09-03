//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package session

import (
	"fmt"
	"os"
	"runtime"
)

func lockSessionFile(*os.File) error {
	return fmt.Errorf("session locking is unsupported on %s", runtime.GOOS)
}

func unlockSessionFile(*os.File) error { return nil }
