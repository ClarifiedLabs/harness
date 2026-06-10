//go:build darwin

package term

import (
	"syscall"
	"testing"
)

// sane on darwin follows BSD f_sane's flag-word semantics (replace wholesale
// from TTYDEF_*, preserving CLOCAL and the LKEEP lflag bits) plus cfmakesane's
// control-character reset, which /bin/stty sane accidentally omits.
func TestSaneDarwin(t *testing.T) {
	tio := syscall.Termios{
		Iflag:  syscall.INLCR | syscall.IGNCR | syscall.IXOFF, // junk in
		Oflag:  0,                                             // OPOST off (raw-ish)
		Cflag:  syscall.CLOCAL | syscall.PARENB | syscall.CSTOPB,
		Lflag:  syscall.TOSTOP | syscall.ECHONL, // TOSTOP is LKEEP, ECHONL is not
		Ispeed: 38400,
		Ospeed: 38400,
	}
	for i := range tio.Cc {
		tio.Cc[i] = 0xAA // garbage control chars
	}

	sane(&tio)

	if want := uint64(syscall.BRKINT | syscall.ICRNL | syscall.IMAXBEL | syscall.IXON | syscall.IXANY); tio.Iflag != want {
		t.Errorf("Iflag = %#x, want TTYDEF_IFLAG %#x", tio.Iflag, want)
	}
	if want := uint64(syscall.OPOST | syscall.ONLCR); tio.Oflag != want {
		t.Errorf("Oflag = %#x, want TTYDEF_OFLAG %#x", tio.Oflag, want)
	}
	if want := uint64(syscall.CREAD | syscall.CS8 | syscall.HUPCL | syscall.CLOCAL); tio.Cflag != want {
		t.Errorf("Cflag = %#x, want TTYDEF_CFLAG+CLOCAL %#x", tio.Cflag, want)
	}
	if want := uint64(syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN |
		syscall.ECHOE | syscall.ECHOKE | syscall.ECHOCTL | syscall.TOSTOP); tio.Lflag != want {
		t.Errorf("Lflag = %#x, want TTYDEF_LFLAG+TOSTOP %#x", tio.Lflag, want)
	}

	// cfmakesane control chars (BSD ttydefchars).
	cc := map[int]uint8{
		syscall.VEOF:     0x04, // ^D
		syscall.VEOL:     0xFF, // _POSIX_VDISABLE
		syscall.VEOL2:    0xFF,
		syscall.VERASE:   0x7F, // DEL
		syscall.VWERASE:  0x17, // ^W
		syscall.VKILL:    0x15, // ^U
		syscall.VREPRINT: 0x12, // ^R
		syscall.VINTR:    0x03, // ^C
		syscall.VQUIT:    0x1C, // ^\
		syscall.VSUSP:    0x1A, // ^Z
		syscall.VDSUSP:   0x19, // ^Y
		syscall.VSTART:   0x11, // ^Q
		syscall.VSTOP:    0x13, // ^S
		syscall.VLNEXT:   0x16, // ^V
		syscall.VDISCARD: 0x0F, // ^O
		syscall.VMIN:     1,
		syscall.VTIME:    0,
		syscall.VSTATUS:  0x14, // ^T
	}
	for idx, want := range cc {
		if tio.Cc[idx] != want {
			t.Errorf("Cc[%d] = %#x, want %#x", idx, tio.Cc[idx], want)
		}
	}

	if tio.Ispeed != 38400 || tio.Ospeed != 38400 {
		t.Errorf("speeds changed: %d/%d, want 38400/38400", tio.Ispeed, tio.Ospeed)
	}
}

func TestSaneDarwinIdempotent(t *testing.T) {
	var a syscall.Termios
	sane(&a)
	b := a
	sane(&b)
	if a != b {
		t.Errorf("sane not idempotent:\nonce:  %+v\ntwice: %+v", a, b)
	}
}
