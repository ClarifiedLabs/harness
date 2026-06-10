//go:build linux

package term

import "syscall"

const (
	reqGet = syscall.TCGETS
	reqSet = syscall.TCSETS

	// EXTPROC from <asm-generic/termbits.h> (0x10000 on amd64 and arm64);
	// missing from stdlib syscall on linux/amd64.
	extPROC = 0x10000
)

// sane applies GNU coreutils' `stty sane` (the SANE_SET/SANE_UNSET entries in
// src/stty.c and the control_info saneval defaults, as of coreutils 9.x):
// specific bits are OR'd in or cleared from the existing termios — whole flag
// words are never replaced — the listed control characters are reset, and
// VMIN/VTIME, baud, CSIZE/parity, CLOCAL, IXON, and PENDIN are deliberately
// left untouched.
// The output delay masks (NLDLY etc.), which GNU sane zeroes, are omitted:
// the constants are not in stdlib syscall and zero delays have been
// universal for decades.
func sane(t *syscall.Termios) {
	t.Iflag &^= syscall.IGNBRK | syscall.INLCR | syscall.IGNCR |
		syscall.IXOFF | syscall.IUTF8 | syscall.IUCLC | syscall.IXANY
	t.Iflag |= syscall.BRKINT | syscall.ICRNL | syscall.IMAXBEL
	// IXON is left as-is, matching GNU.

	t.Oflag |= syscall.OPOST | syscall.ONLCR
	t.Oflag &^= syscall.OLCUC | syscall.OCRNL | syscall.ONOCR | syscall.ONLRET |
		syscall.OFILL | syscall.OFDEL

	t.Cflag |= syscall.CREAD

	t.Lflag |= syscall.ISIG | syscall.ICANON | syscall.IEXTEN |
		syscall.ECHO | syscall.ECHOE | syscall.ECHOK |
		syscall.ECHOCTL | syscall.ECHOKE
	t.Lflag &^= syscall.ECHONL | syscall.NOFLSH | syscall.XCASE |
		syscall.TOSTOP | syscall.ECHOPRT | syscall.FLUSHO | extPROC

	// control_info saneval defaults; _POSIX_VDISABLE is 0 on linux.
	t.Cc[syscall.VINTR] = 0x03    // ^C
	t.Cc[syscall.VQUIT] = 0x1C    // ^\
	t.Cc[syscall.VERASE] = 0x7F   // DEL
	t.Cc[syscall.VKILL] = 0x15    // ^U
	t.Cc[syscall.VEOF] = 0x04     // ^D
	t.Cc[syscall.VEOL] = 0x00     // disabled
	t.Cc[syscall.VEOL2] = 0x00    // disabled
	t.Cc[syscall.VSWTC] = 0x00    // disabled
	t.Cc[syscall.VSTART] = 0x11   // ^Q
	t.Cc[syscall.VSTOP] = 0x13    // ^S
	t.Cc[syscall.VSUSP] = 0x1A    // ^Z
	t.Cc[syscall.VREPRINT] = 0x12 // ^R
	t.Cc[syscall.VWERASE] = 0x17  // ^W
	t.Cc[syscall.VLNEXT] = 0x16   // ^V
	t.Cc[syscall.VDISCARD] = 0x0F // ^O
	// VMIN and VTIME are left untouched (GNU sane_mode breaks before them).
}

func fdSet(fd int, set *syscall.FdSet) bool {
	const bitsPerMask = 64
	if fd < 0 || fd/bitsPerMask >= len(set.Bits) {
		return false
	}
	set.Bits[fd/bitsPerMask] |= int64(uint64(1) << uint(fd%bitsPerMask))
	return true
}

func fdIsSet(fd int, set *syscall.FdSet) bool {
	const bitsPerMask = 64
	if fd < 0 || fd/bitsPerMask >= len(set.Bits) {
		return false
	}
	return set.Bits[fd/bitsPerMask]&int64(uint64(1)<<uint(fd%bitsPerMask)) != 0
}

func selectReadable(n int, set *syscall.FdSet, tv *syscall.Timeval) bool {
	ready, err := syscall.Select(n, set, nil, nil, tv)
	return err == nil && ready > 0
}
