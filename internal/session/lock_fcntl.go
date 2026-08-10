//go:build aix || (solaris && !illumos)

package session

import (
	"errors"
	"os"
	"syscall"
)

var errLockHeld = errors.New("session lock held")

func lockSessionFile(f *os.File) error {
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 1}
	err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock)
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) {
		return errLockHeld
	}
	return err
}

func unlockSessionFile(f *os.File) error {
	lock := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 1}
	return syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock)
}
