//go:build darwin || linux

package procgroup

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// waitExit observes terminal status without reaping. The zombie retains its PID
// and hence prevents reuse of the process group's numeric identity until Wait.
func waitExit(pid int) error {
	for {
		exited, err := waitid(pid, syscall.WEXITED|syscall.WNOWAIT)
		if err != nil || exited {
			return err
		}
		// Darwin can report a stopped child even with only WEXITED requested
		// (Go issue 19314). WNOWAIT preserves that status: avoid a busy loop,
		// and never mistake it for exit or reap it with wait4.
		time.Sleep(10 * time.Millisecond)
	}
}

func waitid(pid, options int) (bool, error) {
	// Linux siginfo_t is 128 bytes; Darwin's is smaller. uint64 gives the
	// buffer sufficient alignment on both 32- and 64-bit architectures.
	var info [16]uint64
	for {
		const pPID = 1
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info)), uintptr(options), 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return false, errno
		}
		fields := (*[32]int32)(unsafe.Pointer(&info))
		if runtime.GOOS == "darwin" {
			// Darwin si_code follows si_signo and si_errno. CLD_EXITED,
			// CLD_KILLED and CLD_DUMPED are the only terminal events.
			return fields[2] >= 1 && fields[2] <= 3, nil
		}
		// Linux honors WEXITED. A zero si_signo means WNOHANG found no
		// status; interpreting no other fields avoids ABI-specific unions.
		return fields[0] != 0, nil
	}
}
