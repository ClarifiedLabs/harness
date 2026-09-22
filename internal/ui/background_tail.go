package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"harness/internal/background"
)

type backgroundTailOptions struct {
	id     string
	lines  int
	follow bool
}

func parseBackgroundTailArgs(args []string) (backgroundTailOptions, error) {
	opts := backgroundTailOptions{lines: backgroundTailDefaultLines}
	countSet := false
	setCount := func(value string) error {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("tail lines must be a positive integer")
		}
		if countSet {
			return fmt.Errorf("tail line count specified more than once")
		}
		opts.lines, countSet = n, true
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--follow":
			opts.follow = true
		case arg == "-n":
			i++
			if i == len(args) {
				return opts, fmt.Errorf("tail -n requires a line count")
			}
			if err := setCount(args[i]); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "-n"):
			if err := setCount(strings.TrimPrefix(arg, "-n")); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "-"):
			if err := setCount(strings.TrimPrefix(arg, "-")); err != nil {
				return opts, err
			}
		case opts.id == "":
			opts.id = arg
		default:
			// Preserve the original /background tail <id> [n] spelling.
			if err := setCount(arg); err != nil {
				return opts, err
			}
		}
	}
	if opts.id == "" {
		return opts, fmt.Errorf("tail requires a job id")
	}
	return opts, nil
}

func openBackgroundOutput(job background.Snapshot) (*os.File, error) {
	if job.OutputPath == "" {
		return nil, fmt.Errorf("job %s has no live output (only running single-command shell jobs retain output)", job.ID)
	}
	f, err := os.Open(job.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("job %s output unavailable: %w", job.ID, err)
	}
	return f, nil
}

// readBackgroundTail returns a bounded raw tail and its ending file offset.
// Keeping the descriptor and offset lets follow start without missing or
// duplicating output written between the initial snapshot and its first poll.
func readBackgroundTail(f *os.File, n int) (data []byte, next int64, truncated bool, err error) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	offset := max(int64(0), info.Size()-backgroundTailMaxBytes)
	data = make([]byte, info.Size()-offset)
	read, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, false, err
	}
	data = data[:read]
	next = offset + int64(read)
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			n--
			if n == 0 {
				return data[i+1:], next, false, nil
			}
		}
	}
	return data, next, offset > 0, nil
}

func (app *App) followBackgroundOutput(job background.Snapshot, lines int) {
	f, err := openBackgroundOutput(job)
	if err != nil {
		fmt.Fprintf(app.Errw, "[background: %v]\n", err)
		return
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if app.Interrupt != nil {
		app.Interrupt.BeginPrompt(cancel)
		defer app.Interrupt.EndPrompt()
	}
	// Command dispatch owns the idle reader here. Restore normal terminal input
	// (including leaving kitty key reporting) so Ctrl-C reaches the existing
	// signal watcher instead of waiting unread in the prompt editor's input.
	if app.BeforeEditor != nil {
		app.BeforeEditor()
	}
	if app.AfterEditor != nil {
		defer app.AfterEditor()
	}
	fmt.Fprintf(app.Errw, "[background: following %s; Ctrl-C stops following, not the job]\n", job.ID)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	err = followBackgroundFile(ctx, app.Errw, f, lines, app.Background, job.ID, ticker.C, app.ForceExit)
	fmt.Fprintln(app.Errw)
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(app.Errw, "[background: stopped following %s]\n", job.ID)
	case err != nil:
		fmt.Fprintf(app.Errw, "[background: %v]\n", err)
	default:
		if snap, ok := app.Background.Get(job.ID); ok {
			fmt.Fprintf(app.Errw, "[background: %s %s]\n", snap.ID, snap.Status)
		}
	}
}

// followBackgroundFile holds the descriptor open through completion: the shell
// runner unlinks its output before publishing the final job state. No goroutine
// writes to the UI, and cancellation stops inspection, never the background job.
func followBackgroundFile(ctx context.Context, w io.Writer, f *os.File, lines int, manager *background.Manager, id string, ticks <-chan time.Time, exit <-chan struct{}) error {
	done, ok := manager.JobDone(id)
	if !ok {
		return fmt.Errorf("unknown background job %q", id)
	}
	data, offset, truncated, err := readBackgroundTail(f, lines)
	if err != nil {
		return err
	}
	if truncated {
		if _, err := fmt.Fprintln(w, "[output truncated to last 64 KiB]"); err != nil {
			return err
		}
	}
	var filter backgroundOutputFilter
	write := func(data []byte) error {
		text := filter.text(data)
		if text == "" {
			return nil
		}
		_, err := io.WriteString(w, text)
		return err
	}
	if err := write(data); err != nil {
		return err
	}
	buf := make([]byte, backgroundTailMaxBytes)
	for {
		// Cancellation changes public status before the runner exits. Wait for
		// actual completion before the final drain so cleanup output survives.
		finished := false
		select {
		case <-done:
			finished = true
		default:
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() < offset {
			offset = 0
			filter = backgroundOutputFilter{}
		}
		// Drain a fixed size snapshot in bounded chunks. Even an enormous or
		// continuously growing log cannot delay cancellation indefinitely.
		for offset < info.Size() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-exit:
				return context.Canceled
			default:
			}
			n, err := f.ReadAt(buf[:min(int64(len(buf)), info.Size()-offset)], offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if n == 0 {
				break
			}
			offset += int64(n)
			if err := write(buf[:n]); err != nil {
				return err
			}
		}
		if finished {
			_, err := io.WriteString(w, filter.finish())
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exit:
			return context.Canceled
		case <-ticks:
		case <-done:
		}
	}
}

// backgroundOutputFilter strips controls across read boundaries without
// buffering lines or escape payloads. Only an incomplete UTF-8 rune is retained.
// This keeps long lines/OSC strings bounded and prevents split escapes from
// leaking control payloads into the display.
type backgroundOutputFilter struct {
	state   byte
	pending []byte
}

func (f *backgroundOutputFilter) text(data []byte) string {
	if len(f.pending) > 0 {
		data = append(f.pending, data...)
		f.pending = nil
	}
	var out strings.Builder
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			f.pending = append([]byte(nil), data...)
			break
		}
		r, size := utf8.DecodeRune(data)
		data = data[size:]
		switch f.state {
		case 'e': // ESC, possibly followed by intermediate bytes
			switch r {
			case '[':
				f.state = 'c'
			case ']', 'P', '^', '_', 'X':
				f.state = 's'
			default:
				if r < 0x20 || r > 0x2f {
					f.state = 0
				}
			}
		case 'c': // CSI parameters/intermediates, then a final byte
			if r >= 0x40 && r <= 0x7e {
				f.state = 0
			}
		case 's': // OSC/DCS/etc., terminated by BEL or ST
			switch r {
			case '\a', 0x9c:
				f.state = 0
			case 0x1b:
				f.state = 't'
			}
		case 't': // ESC inside a control string
			switch r {
			case '\\', '\a', 0x9c:
				f.state = 0
			case 0x1b:
			default:
				f.state = 's'
			}
		default:
			switch {
			case r == 0x1b:
				f.state = 'e'
			case r == 0x9b:
				f.state = 'c'
			case r == 0x90 || r == 0x98 || r == 0x9d || r == 0x9e || r == 0x9f:
				f.state = 's'
			case r == '\n' || r == '\t':
				out.WriteRune(r)
			case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			default:
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func (f *backgroundOutputFilter) finish() string {
	if len(f.pending) > 0 && f.state == 0 {
		f.pending = nil
		return string(utf8.RuneError)
	}
	return ""
}
