//go:build darwin || linux

package tools

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

func configureProcessGroup(cmd *exec.Cmd) {
	// run_command captures stdio and is not interactive. Give the child its own
	// session so /dev/tty is unavailable instead of job-control stopping either
	// the child or harness. Setsid also makes the child a process-group leader,
	// preserving negative-pid group kills on timeout/cancel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func hasForegroundTTY() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return false
	}
	defer tty.Close()

	pgrp, err := tcgetpgrp(tty.Fd())
	return err == nil && pgrp > 0 && pgrp == syscall.Getpgrp()
}

func tcgetpgrp(fd uintptr) (int, error) {
	var pgrp int32
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)), 0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(pgrp), nil
}
