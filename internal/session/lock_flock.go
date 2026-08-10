//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package session

import (
	"errors"
	"os"
	"syscall"
)

var errLockHeld = errors.New("session lock held")

func lockSessionFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockHeld
	}
	return err
}

func unlockSessionFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
