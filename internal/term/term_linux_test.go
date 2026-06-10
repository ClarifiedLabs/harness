//go:build linux

package term

import (
	"syscall"
	"testing"
)

// sane on linux follows GNU coreutils stty sane: OR/clear specific bits into
// the existing termios (never replacing whole flag words) per the
// SANE_SET/SANE_UNSET entries in src/stty.c, reset the listed control
// characters to their saneval defaults, and leave VMIN/VTIME, baud,
// CSIZE/parity, CLOCAL, IXON, and PENDIN untouched.
func TestSaneLinux(t *testing.T) {
	tio := syscall.Termios{
		Iflag: syscall.INLCR | syscall.IGNCR | syscall.IXOFF | syscall.IUTF8 |
			syscall.IXON, // IXON has no SANE flag; sane must leave it alone
		Oflag: syscall.OLCUC | syscall.OCRNL | syscall.ONOCR | syscall.ONLRET |
			syscall.OFILL | syscall.OFDEL, // OPOST off, junk on
		Cflag: syscall.CLOCAL | syscall.CS8,
		Lflag: syscall.NOFLSH | syscall.TOSTOP | syscall.ECHOPRT |
			syscall.FLUSHO | extPROC | syscall.PENDIN, // all SANE_UNSET except PENDIN
	}
	for i := range tio.Cc {
		tio.Cc[i] = 0xAA
	}
	tio.Cc[syscall.VMIN] = 0
	tio.Cc[syscall.VTIME] = 7

	sane(&tio)

	// iflag: sane bits on, junk off, untouched bits preserved.
	for name, flag := range map[string]uint32{"BRKINT": syscall.BRKINT, "ICRNL": syscall.ICRNL, "IMAXBEL": syscall.IMAXBEL} {
		if tio.Iflag&flag == 0 {
			t.Errorf("Iflag missing %s", name)
		}
	}
	for name, flag := range map[string]uint32{
		"IGNBRK": syscall.IGNBRK, "INLCR": syscall.INLCR, "IGNCR": syscall.IGNCR,
		"IXOFF": syscall.IXOFF, "IUTF8": syscall.IUTF8, "IUCLC": syscall.IUCLC, "IXANY": syscall.IXANY,
	} {
		if tio.Iflag&flag != 0 {
			t.Errorf("Iflag still has %s", name)
		}
	}
	if tio.Iflag&syscall.IXON == 0 {
		t.Error("IXON was cleared; it has no SANE flag and must be left as-is")
	}

	// oflag: OPOST|ONLCR on, every SANE_UNSET output bit off.
	if tio.Oflag&syscall.OPOST == 0 || tio.Oflag&syscall.ONLCR == 0 {
		t.Errorf("Oflag = %#x, want OPOST|ONLCR set", tio.Oflag)
	}
	for name, flag := range map[string]uint32{
		"OLCUC": syscall.OLCUC, "OCRNL": syscall.OCRNL, "ONOCR": syscall.ONOCR,
		"ONLRET": syscall.ONLRET, "OFILL": syscall.OFILL, "OFDEL": syscall.OFDEL,
	} {
		if tio.Oflag&flag != 0 {
			t.Errorf("Oflag still has %s", name)
		}
	}

	// cflag: CREAD added, everything else (CLOCAL, CS8) preserved.
	if tio.Cflag&syscall.CREAD == 0 {
		t.Error("Cflag missing CREAD")
	}
	if tio.Cflag&syscall.CLOCAL == 0 || tio.Cflag&syscall.CS8 != syscall.CS8 {
		t.Errorf("Cflag = %#x, CLOCAL/CS8 not preserved", tio.Cflag)
	}

	// lflag: sane bits on, every SANE_UNSET local bit off, PENDIN preserved.
	for name, flag := range map[string]uint32{
		"ISIG": syscall.ISIG, "ICANON": syscall.ICANON, "IEXTEN": syscall.IEXTEN,
		"ECHO": syscall.ECHO, "ECHOE": syscall.ECHOE, "ECHOK": syscall.ECHOK,
		"ECHOCTL": syscall.ECHOCTL, "ECHOKE": syscall.ECHOKE,
	} {
		if tio.Lflag&flag == 0 {
			t.Errorf("Lflag missing %s", name)
		}
	}
	for name, flag := range map[string]uint32{
		"ECHONL": syscall.ECHONL, "NOFLSH": syscall.NOFLSH, "XCASE": syscall.XCASE,
		"TOSTOP": syscall.TOSTOP, "ECHOPRT": syscall.ECHOPRT,
		"FLUSHO": syscall.FLUSHO, "EXTPROC": extPROC,
	} {
		if tio.Lflag&flag != 0 {
			t.Errorf("Lflag still has %s (SANE_UNSET in coreutils)", name)
		}
	}
	if tio.Lflag&syscall.PENDIN == 0 {
		t.Error("PENDIN was cleared; it has no SANE flag and must be left as-is")
	}

	// control chars: GNU saneval defaults; VMIN/VTIME untouched.
	cc := map[int]uint8{
		syscall.VINTR:    0x03, // ^C
		syscall.VQUIT:    0x1C, // ^\
		syscall.VERASE:   0x7F, // DEL
		syscall.VKILL:    0x15, // ^U
		syscall.VEOF:     0x04, // ^D
		syscall.VEOL:     0x00, // _POSIX_VDISABLE
		syscall.VEOL2:    0x00,
		syscall.VSWTC:    0x00,
		syscall.VSTART:   0x11, // ^Q
		syscall.VSTOP:    0x13, // ^S
		syscall.VSUSP:    0x1A, // ^Z
		syscall.VREPRINT: 0x12, // ^R
		syscall.VWERASE:  0x17, // ^W
		syscall.VLNEXT:   0x16, // ^V
		syscall.VDISCARD: 0x0F, // ^O
	}
	for idx, want := range cc {
		if tio.Cc[idx] != want {
			t.Errorf("Cc[%d] = %#x, want %#x", idx, tio.Cc[idx], want)
		}
	}
	if tio.Cc[syscall.VMIN] != 0 || tio.Cc[syscall.VTIME] != 7 {
		t.Errorf("VMIN/VTIME = %d/%d, want untouched 0/7",
			tio.Cc[syscall.VMIN], tio.Cc[syscall.VTIME])
	}
}

// IXON must also not be force-set: a terminal with flow control deliberately
// off stays that way, matching GNU sane.
func TestSaneLinuxLeavesIXONOff(t *testing.T) {
	var tio syscall.Termios
	sane(&tio)
	if tio.Iflag&syscall.IXON != 0 {
		t.Error("IXON was set; GNU sane leaves it as-is")
	}
}

func TestSaneLinuxIdempotent(t *testing.T) {
	var a syscall.Termios
	sane(&a)
	b := a
	sane(&b)
	if a != b {
		t.Errorf("sane not idempotent:\nonce:  %+v\ntwice: %+v", a, b)
	}
}
