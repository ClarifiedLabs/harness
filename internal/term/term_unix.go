//go:build darwin || linux

package term

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// softReset undoes the terminal-emulator modes a crashed full-screen program
// commonly leaves enabled. Unlike RIS (\033c) it does not clear the screen or
// scrollback. DECSTR alone does not reliably disable mouse/focus/paste
// reporting across emulators, hence the explicit DECRST list.
//
// Leaving the alternate screen comes first and is guarded by DECSC: DECRST
// 1049 performs a DECRC cursor-restore even when the alternate screen is not
// active, and with no position ever saved that restores home — jumping the
// cursor to the top of the screen. Saving the cursor immediately before makes
// the restore a no-op in the normal case, while after a crashed 1049h app the
// normal screen's slot still holds the position saved on entry. The pair must
// precede DECSTR, which resets the saved-cursor slot in some emulators.
const softReset = "\x1b7\x1b[?1049l" + // leave alternate screen (DECSC-guarded, see above)
	"\x1b[!p" + // DECSTR: SGR, autowrap, origin/insert mode, cursor visible
	"\x1b[?1003l\x1b[?1002l\x1b[?1000l" + // mouse tracking off (any-event, button-event, normal)
	"\x1b[?1006l\x1b[?1005l\x1b[?1015l" + // mouse coordinate encodings off (SGR, UTF-8, urxvt)
	"\x1b[?1004l" + // focus reporting off (the ESC[I / ESC[O junk on focus changes)
	"\x1b[?2004l" + // bracketed paste off
	"\x1b[?25h" + // show cursor (DECSTR covers it in xterm; explicit for partial emulators)
	"\x1b(B\x0f" + // G0 = ASCII, shift in (undo line-drawing charset)
	"\x1b[0m" // SGR reset (also in DECSTR; explicit for partial emulators)

const (
	bracketedPasteEnable  = "\x1b[?2004h"
	bracketedPasteDisable = "\x1b[?2004l"
	replInputTraceEnv     = "HARNESS_REPL_INPUT_TRACE"
	kittyKeyboardPush     = "\x1b[>25u"
	kittyKeyboardPop      = "\x1b[<u"
	ctrlG                 = 0x07
	esc                   = 0x1B
)

// Reset restores the controlling terminal to a usable state: kernel termios
// to the platform's `stty sane` equivalent (echo, canonical mode, default
// control characters), then the emulator soft reset above. It targets
// /dev/tty so it works regardless of stdin/stderr redirection, and is a
// silent no-op when the process has no controlling terminal.
//
// O_RDWR is required because setTermios issues an ioctl on the fd that
// requires write permission, even though getTermios and WriteString would
// each work with read-only or write-only access.
func Reset() error {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil // no controlling terminal: nothing to fix
	}
	defer f.Close()

	tio, err := getTermios(f.Fd())
	if err != nil {
		if errors.Is(err, syscall.ENOTTY) {
			return nil
		}
		return fmt.Errorf("term: get termios: %w", err)
	}
	sane(&tio)
	if err := setTermios(f.Fd(), &tio); err != nil {
		return fmt.Errorf("term: set termios: %w", err)
	}
	if _, err := f.WriteString(softReset); err != nil {
		return fmt.Errorf("term: write soft reset: %w", err)
	}
	return nil
}

// SetBracketedPaste enables or disables terminal bracketed-paste reporting.
// Like Reset, it targets /dev/tty and is a silent no-op without a controlling
// terminal so tests and redirected runs do not receive escape sequences.
func SetBracketedPaste(enabled bool) error {
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil
	}
	defer f.Close()

	seq := bracketedPasteDisable
	if enabled {
		seq = bracketedPasteEnable
	}
	if _, err := f.WriteString(seq); err != nil {
		return fmt.Errorf("term: set bracketed paste: %w", err)
	}
	return nil
}

// EnableCtrlGLineEnd makes Ctrl-G act as a canonical-mode line delimiter. This
// lets the REPL observe the key immediately while preserving normal terminal
// line editing. The returned cleanup restores the original termios; both setup
// and cleanup are silent no-ops when no controlling terminal exists.
func EnableCtrlGLineEnd() (func() error, error) {
	return withTTYTermios(ctrlGLineEndTermios, "ctrl-g line end")
}

func ctrlGLineEndTermios(t syscall.Termios) syscall.Termios {
	t.Cc[syscall.VEOL] = ctrlG
	t.Lflag &^= syscall.ECHOCTL
	return t
}

// EnablePromptRawMode switches the controlling terminal into the raw-ish mode
// used by the REPL's prompt editor. Signals stay enabled for legacy Ctrl-C
// input; terminals that honor full keyboard reporting may send Ctrl-C as a CSI u
// key event instead. It also asks terminals that implement the kitty keyboard
// protocol to report all keys with associated text so Enter variants are
// disambiguated. The returned cleanup restores the original termios and keyboard
// mode; setup and cleanup are silent no-ops without a controlling terminal.
func EnablePromptRawMode() (func() error, error) {
	tracePromptInputf("term: enable prompt raw setup=%q cleanup=%q", kittyKeyboardPush, kittyKeyboardPop)
	return withTTYTermiosAndSequences(promptRawTermios, "prompt raw mode", kittyKeyboardPush, kittyKeyboardPop)
}

func promptRawTermios(t syscall.Termios) syscall.Termios {
	t.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR
	// ICANON off: read character by character; ECHO/ECHOCTL off: no terminal echo.
	// ISIG is deliberately left on so SIGINT is delivered via ^C on terminals that
	// do not support the kitty keyboard protocol. Terminals that do will send ^C
	// as a CSI u key event instead, handled in the editor's readEscape path.
	t.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ECHOCTL
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return t
}

// WaitReadable reports whether f can be read without blocking before timeout.
// It is used only for disambiguating a bare Escape key from terminal escape
// sequences; callers still read through their existing buffered reader.
func WaitReadable(f *os.File, timeout time.Duration) bool {
	if f == nil {
		return false
	}
	fd := int(f.Fd())
	if fd < 0 {
		return false
	}
	var set syscall.FdSet
	if !fdSet(fd, &set) {
		return false
	}
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	return selectReadable(fd+1, &set, &tv) && fdIsSet(fd, &set)
}

// EnableEscLineEnd makes Escape act as a canonical-mode line delimiter. The
// REPL enables this only while a model turn is active so Esc-Esc can be observed
// immediately without switching the whole prompt to raw mode. The returned
// cleanup restores the original termios; both setup and cleanup are silent
// no-ops when no controlling terminal exists.
func EnableEscLineEnd() (func() error, error) {
	return withTTYTermios(func(t syscall.Termios) syscall.Termios {
		t.Cc[syscall.VEOL2] = esc
		return t
	}, "esc line end")
}

// withTTYTermios applies mutate to the controlling terminal's termios and
// returns a cleanup that restores the original. Setup and cleanup are silent
// no-ops when no controlling terminal exists (open failure or ENOTTY); label
// names the key binding in error messages.
func withTTYTermios(mutate func(syscall.Termios) syscall.Termios, label string) (func() error, error) {
	return withTTYTermiosAndSequences(mutate, label, "", "")
}

// withTTYTermiosAndSequences applies mutate to the controlling terminal's
// termios, optionally writes setupSeq to the terminal first, and returns a
// cleanup closure that restores the original termios and writes cleanupSeq.
//
// Ordering: termios is mutated before setupSeq is written so that the kernel
// and the terminal emulator see the new mode before any escape sequence that
// might provoke a reply (e.g. the kitty keyboard push). On the error path,
// cleanupSeq is written to undo any escape sequence already sent.
//
// The cleanup closure re-opens /dev/tty rather than capturing the setup fd.
// If /dev/tty cannot be opened during cleanup (e.g. after SIGHUP), any
// cleanupSeq is written to os.Stderr as a best-effort fallback so that
// escape sequences like kittyKeyboardPop are not silently lost.
func withTTYTermiosAndSequences(mutate func(syscall.Termios) syscall.Termios, label, setupSeq, cleanupSeq string) (func() error, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		tracePromptInputf("term: %s open /dev/tty skipped: %v", label, err)
		return func() error { return nil }, nil
	}
	defer f.Close()

	orig, err := getTermios(f.Fd())
	if err != nil {
		if errors.Is(err, syscall.ENOTTY) {
			tracePromptInputf("term: %s get termios skipped: %v", label, err)
			return func() error { return nil }, nil
		}
		return nil, fmt.Errorf("term: get termios: %w", err)
	}
	// Mutate termios before writing the setup sequence so the terminal is in
	// the new mode before any escape sequence that may provoke a response.
	next := mutate(orig)
	if err := setTermios(f.Fd(), &next); err != nil {
		tracePromptInputf("term: %s set termios failed: %v", label, err)
		return nil, fmt.Errorf("term: set %s: %w", label, err)
	}
	tracePromptInputf("term: %s set termios ok", label)
	if setupSeq != "" {
		if _, err := f.WriteString(setupSeq); err != nil {
			tracePromptInputf("term: %s write setup failed: %v", label, err)
			// Undo the termios change since setup is incomplete.
			if restoreErr := setTermios(f.Fd(), &orig); restoreErr != nil {
				tracePromptInputf("term: %s restore termios after setup failure: %v", label, restoreErr)
			}
			return nil, fmt.Errorf("term: set %s: %w", label, err)
		}
		tracePromptInputf("term: %s wrote setup=%q", label, setupSeq)
	}

	return func() error {
		f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			// Controlling terminal gone (e.g. SIGHUP). Write any cleanup
			// sequence to stderr as a best-effort fallback so escape sequences
			// such as kittyKeyboardPop are not silently lost.
			tracePromptInputf("term: restore %s open /dev/tty skipped: %v", label, err)
			if cleanupSeq != "" {
				if _, werr := os.Stderr.WriteString(cleanupSeq); werr != nil {
					tracePromptInputf("term: restore %s stderr fallback failed: %v", label, werr)
				} else {
					tracePromptInputf("term: restore %s wrote cleanup to stderr fallback=%q", label, cleanupSeq)
				}
			}
			return nil
		}
		defer f.Close()
		var restoreErr error
		if err := setTermios(f.Fd(), &orig); err != nil {
			tracePromptInputf("term: restore %s termios failed: %v", label, err)
			restoreErr = fmt.Errorf("term: restore %s: %w", label, err)
		}
		if cleanupSeq != "" {
			if _, err := f.WriteString(cleanupSeq); err != nil && restoreErr == nil {
				tracePromptInputf("term: restore %s cleanup failed: %v", label, err)
				restoreErr = fmt.Errorf("term: restore %s: %w", label, err)
			}
			if restoreErr == nil {
				tracePromptInputf("term: restore %s wrote cleanup=%q", label, cleanupSeq)
			}
		}
		return restoreErr
	}, nil
}

func tracePromptInputf(format string, args ...any) {
	path := os.Getenv(replInputTraceEnv)
	if path == "" {
		return
	}
	var f *os.File
	var err error
	if path == "-" {
		f = os.Stderr
	} else {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
	}
	fmt.Fprintf(f, "%s ", time.Now().Format(time.RFC3339Nano))
	fmt.Fprintf(f, format, args...)
	fmt.Fprintln(f)
}

// Size reports the controlling terminal's rows and columns. It returns ok=false
// when there is no controlling terminal or the size cannot be determined.
func Size() (rows, cols int, ok bool) {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	return sizeFromFD(f.Fd())
}

func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := getTermios(f.Fd())
	return err == nil
}

func sizeFromFD(fd uintptr) (rows, cols int, ok bool) {
	var ws windowSize
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)), 0, 0, 0); errno != 0 {
		return 0, 0, false
	}
	if ws.Rows == 0 || ws.Cols == 0 {
		return 0, 0, false
	}
	return int(ws.Rows), int(ws.Cols), true
}

type windowSize struct {
	Rows uint16
	Cols uint16
	X    uint16
	Y    uint16
}

func getTermios(fd uintptr) (syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, reqGet,
		uintptr(unsafe.Pointer(&t)), 0, 0, 0); errno != 0 {
		return t, errno
	}
	return t, nil
}

func setTermios(fd uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, reqSet,
		uintptr(unsafe.Pointer(t)), 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
