//go:build darwin || linux

package term

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// softReset must disable every emulator mode a crashed TUI commonly leaves on
// (mouse tracking, focus reporting, bracketed paste, alternate screen) without
// clearing the screen or scrollback — that is the whole point of replacing RIS.
func TestSoftResetDisablesLeftoverModes(t *testing.T) {
	want := []string{
		"\x1b[!p",     // DECSTR soft reset
		"\x1b[?1000l", // mouse: normal tracking off
		"\x1b[?1002l", // mouse: button-event tracking off
		"\x1b[?1003l", // mouse: any-event tracking off
		"\x1b[?1005l", // mouse: UTF-8 coords off
		"\x1b[?1006l", // mouse: SGR coords off
		"\x1b[?1015l", // mouse: urxvt coords off
		"\x1b[?1004l", // focus in/out reporting off (ESC[I / ESC[O junk)
		"\x1b[?2004l", // bracketed paste off
		"\x1b[?25h",   // show cursor
		"\x1b(B\x0f",  // G0 = ASCII, shift in (undo line-drawing charset)
		"\x1b[0m",     // SGR reset
	}
	for _, seq := range want {
		if !strings.Contains(softReset, seq) {
			t.Errorf("softReset missing %q", seq)
		}
	}
	for _, seq := range []string{"\x1bc", "[2J", "[3J"} {
		if strings.Contains(softReset, seq) {
			t.Errorf("softReset contains screen-destroying sequence %q", seq)
		}
	}

	// Leaving the alternate screen (?1049l) performs a DECRC cursor-restore
	// even when the alternate screen is not active; without a DECSC (ESC 7)
	// immediately before it, the cursor jumps to the never-saved default —
	// home — and the prompt prints over the top of the scrollback. The pair
	// must also precede DECSTR, which resets the saved-cursor slot in some
	// emulators and would defeat the restore after a crashed 1049h app.
	guarded := strings.Index(softReset, "\x1b7\x1b[?1049l")
	if guarded == -1 {
		t.Fatal("softReset must exit the alt screen as \\x1b7\\x1b[?1049l (DECSC immediately before DECRST 1049)")
	}
	if decstr := strings.Index(softReset, "\x1b[!p"); decstr < guarded {
		t.Error("DECSC+1049l must come before DECSTR")
	}
	if n := strings.Count(softReset, "\x1b[?1049l"); n != 1 {
		t.Errorf("softReset contains %d copies of ?1049l, want exactly 1 (guarded)", n)
	}
}

// Reset must be a silent no-op when the process has no controlling terminal
// (tests, pipes, CI). When one is present this test skips rather than mutate
// the developer's terminal; TestResetOnRealTTY covers that side.
func TestResetNoTTY(t *testing.T) {
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0); err == nil {
		f.Close()
		t.Skip("controlling terminal present; no-op path not reachable")
	}
	if err := Reset(); err != nil {
		t.Fatalf("Reset() without a controlling terminal = %v, want nil", err)
	}
}

func TestLineEndHelpersNoTTY(t *testing.T) {
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0); err == nil {
		f.Close()
		t.Skip("controlling terminal present; no-op path not reachable")
	}
	for name, fn := range map[string]func() (func() error, error){
		"ctrl-g": EnableCtrlGLineEnd,
		"esc":    EnableEscLineEnd,
	} {
		cleanup, err := fn()
		if err != nil {
			t.Fatalf("%s helper without a controlling terminal = %v, want nil", name, err)
		}
		if err := cleanup(); err != nil {
			t.Fatalf("%s cleanup without a controlling terminal = %v, want nil", name, err)
		}
	}
}

func TestCtrlGLineEndSuppressesControlEcho(t *testing.T) {
	var tio syscall.Termios
	tio.Lflag = syscall.ECHO | syscall.ECHOCTL

	got := ctrlGLineEndTermios(tio)
	if got.Cc[syscall.VEOL] != ctrlG {
		t.Fatalf("VEOL = %#x, want Ctrl-G", got.Cc[syscall.VEOL])
	}
	if got.Lflag&syscall.ECHO == 0 {
		t.Fatal("Ctrl-G line-end setup should preserve ordinary input echo")
	}
	if got.Lflag&syscall.ECHOCTL != 0 {
		t.Fatal("Ctrl-G line-end setup should suppress ^G control echo")
	}
}

func TestPromptRawTermiosKeepsSignals(t *testing.T) {
	var tio syscall.Termios
	tio.Iflag = syscall.ICRNL | syscall.INLCR | syscall.IGNCR
	tio.Lflag = syscall.ICANON | syscall.ECHO | syscall.ECHOCTL | syscall.ISIG | syscall.IEXTEN

	got := promptRawTermios(tio)
	if got.Iflag&(syscall.ICRNL|syscall.INLCR|syscall.IGNCR) != 0 {
		t.Fatalf("prompt raw mode should preserve CR/LF distinctions; Iflag=%#x", got.Iflag)
	}
	if got.Lflag&syscall.ICANON != 0 {
		t.Fatal("prompt raw mode should disable canonical input")
	}
	if got.Lflag&syscall.ECHO != 0 {
		t.Fatal("prompt raw mode should disable echo")
	}
	if got.Lflag&syscall.ECHOCTL != 0 {
		t.Fatal("prompt raw mode should disable control-character echo")
	}
	if got.Lflag&syscall.ISIG == 0 {
		t.Fatal("prompt raw mode should preserve signal delivery")
	}
	if got.Cc[syscall.VMIN] != 1 || got.Cc[syscall.VTIME] != 0 {
		t.Fatalf("VMIN/VTIME = %d/%d, want 1/0", got.Cc[syscall.VMIN], got.Cc[syscall.VTIME])
	}
}

func TestPromptRawModeRequestsKittyAllKeysWithText(t *testing.T) {
	if kittyKeyboardPush != "\x1b[>25u" {
		t.Fatalf("kittyKeyboardPush = %q, want CSI > 25 u", kittyKeyboardPush)
	}
	if kittyKeyboardPop != "\x1b[<u" {
		t.Fatalf("kittyKeyboardPop = %q, want CSI < u", kittyKeyboardPop)
	}
}

func TestPromptInputTraceWritesTermEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.log")
	t.Setenv(replInputTraceEnv, path)

	tracePromptInputf("term: test setup=%q", kittyKeyboardPush)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(data), `term: test setup="\x1b[>25u"`) {
		t.Fatalf("trace missing setup sequence:\n%s", data)
	}
}

// TestResetOnRealTTY runs only when a controlling terminal is available (e.g.
// under script(1) or a developer's shell): it deliberately breaks the terminal
// (echo and canonical mode off), calls Reset, and verifies both are restored.
// The original termios is restored afterwards either way.
func TestResetOnRealTTY(t *testing.T) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no controlling terminal")
	}
	defer f.Close()

	orig, err := getTermios(f.Fd())
	if err != nil {
		t.Skipf("get termios on /dev/tty: %v", err)
	}
	defer setTermios(f.Fd(), &orig)

	broken := orig
	broken.Lflag &^= syscall.ECHO | syscall.ICANON
	if err := setTermios(f.Fd(), &broken); err != nil {
		t.Fatalf("breaking termios: %v", err)
	}

	if err := Reset(); err != nil {
		t.Fatalf("Reset() = %v", err)
	}

	got, err := getTermios(f.Fd())
	if err != nil {
		t.Fatalf("get termios after Reset: %v", err)
	}
	if got.Lflag&syscall.ECHO == 0 {
		t.Error("ECHO still off after Reset")
	}
	if got.Lflag&syscall.ICANON == 0 {
		t.Error("ICANON still off after Reset")
	}
}
