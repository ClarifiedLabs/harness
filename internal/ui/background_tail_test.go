package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/tools"
)

func TestParseBackgroundTailArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		lines  int
		follow bool
	}{
		{"default", []string{"job"}, 10, false},
		{"short count before", []string{"-100", "job"}, 100, false},
		{"short count after", []string{"job", "-100"}, 100, false},
		{"separate count", []string{"-n", "23", "job"}, 23, false},
		{"joined count", []string{"job", "-n23"}, 23, false},
		{"legacy", []string{"job", "23"}, 23, false},
		{"follow before", []string{"-f", "job"}, 10, true},
		{"follow after", []string{"job", "-f"}, 10, true},
		{"long follow before", []string{"--follow", "job"}, 10, true},
		{"long follow after", []string{"job", "--follow"}, 10, true},
		{"count then follow", []string{"-n", "23", "--follow", "job"}, 23, true},
		{"follow then count", []string{"job", "-f", "-n23"}, 23, true},
		{"legacy follow", []string{"job", "23", "-f"}, 23, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBackgroundTailArgs(tc.args)
			want := backgroundTailOptions{id: "job", lines: tc.lines, follow: tc.follow}
			if err != nil || got != want {
				t.Fatalf("parse(%q) = %+v, %v; want %+v", tc.args, got, err, want)
			}
		})
	}
	for _, args := range [][]string{
		nil, {"-f"}, {"--follow", "-100"}, {"-n", "10"},
		{"job", "-n"}, {"job", "-n", "-f"}, {"job", "-nnope"},
		{"job", "0"}, {"job", "-0"}, {"job", "-n", "-2"},
		{"job", "-n", "9999999999999999999999999"}, {"job", "--unknown"},
		{"job", "other"}, {"job", "2", "3"}, {"job", "-2", "-n3"},
		{"job", "-n", "2", "2"}, {"job", "-n2", "-n2"},
	} {
		t.Run("invalid/"+strings.Join(args, " "), func(t *testing.T) {
			if got, err := parseBackgroundTailArgs(args); err == nil {
				t.Fatalf("parse(%q) = %+v, want error", args, got)
			}
		})
	}
}

func TestReadBackgroundTail(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		lines             int
		truncated         bool
	}{
		{"empty", "", "", 10, false},
		{"terminated", "one\ntwo\nthree\n", "two\nthree\n", 2, false},
		{"unterminated", "one\ntwo\nthree", "two\nthree", 2, false},
		{"blank last line", "one\ntwo\n\n", "\n", 1, false},
		{"fewer lines", "one\ntwo\n", "one\ntwo\n", 10, false},
		{"exact bound", strings.Repeat("x", backgroundTailMaxBytes), strings.Repeat("x", backgroundTailMaxBytes), 10, false},
		{"oversized line", strings.Repeat("x", backgroundTailMaxBytes+19), strings.Repeat("x", backgroundTailMaxBytes), 10, true},
		{"bounded complete lines", strings.Repeat("x", backgroundTailMaxBytes) + "\nlast\n", "last\n", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := backgroundTailTestFile(t, tc.input)
			got, offset, truncated, err := readBackgroundTail(f, tc.lines)
			if err != nil || string(got) != tc.want || offset != int64(len(tc.input)) || truncated != tc.truncated {
				t.Fatalf("tail = %d bytes, offset %d, truncated %v, err %v; want %d bytes, offset %d, truncated %v (content matches: %v)", len(got), offset, truncated, err, len(tc.want), len(tc.input), tc.truncated, string(got) == tc.want)
			}
			if len(got) > backgroundTailMaxBytes {
				t.Fatalf("tail exceeds byte bound: %d", len(got))
			}
		})
	}
}

// Notifications let tests observe output without sleeping or racing a buffer.
type backgroundTailTestWriter struct {
	mu      sync.Mutex
	text    strings.Builder
	changed chan struct{}
}

func newBackgroundTailTestWriter() *backgroundTailTestWriter {
	return &backgroundTailTestWriter{changed: make(chan struct{}, 1)}
}

func (w *backgroundTailTestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.text.Write(p)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (w *backgroundTailTestWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

func (w *backgroundTailTestWriter) waitFor(t *testing.T, text string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !strings.Contains(w.String(), text) {
		select {
		case <-w.changed:
		case <-timer.C:
			t.Fatalf("waiting for %q; output = %q", text, w.String())
		}
	}
}

func backgroundTailTestFile(t *testing.T, text string) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	backgroundTailTestAppend(t, f, text)
	return f
}

func backgroundTailTestAppend(t *testing.T, f *os.File, text string) {
	t.Helper()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func backgroundTailTestReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tail test event")
		var zero T
		return zero
	}
}

func backgroundTailTestJob(t *testing.T, path string) (*background.Manager, background.Snapshot, context.Context, func()) {
	t.Helper()
	manager := background.NewManager(background.Options{})
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	info, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell", OutputPath: path,
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			started <- ctx
			select {
			case <-release:
				return tools.BackgroundJobResult{}, nil
			case <-ctx.Done():
				return tools.BackgroundJobResult{}, ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		finish()
		manager.ShutdownAndWait(5 * time.Second)
	})
	ctx := backgroundTailTestReceive(t, started)
	job, ok := manager.Get(info.ID)
	if !ok {
		t.Fatal("started job missing")
	}
	return manager, job, ctx, finish
}

func TestFollowBackgroundFileAppendAndUnlinkedCompletion(t *testing.T) {
	f := backgroundTailTestFile(t, "omit\nfirst\nsecond\n")
	manager, job, _, finish := backgroundTailTestJob(t, f.Name())
	w := newBackgroundTailTestWriter()
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- followBackgroundFile(ctx, w, f, 2, manager, job.ID, ticks, nil) }()
	w.waitFor(t, "first\nsecond\n")
	backgroundTailTestAppend(t, f, "third\n")
	ticks <- time.Time{}
	w.waitFor(t, "third\n")
	// The runner removes its output before publishing completion. The follower
	// must drain the open descriptor even though reopening its path now fails.
	backgroundTailTestAppend(t, f, "final\n")
	if err := os.Remove(f.Name()); err != nil {
		t.Fatal(err)
	}
	finish()
	if err := backgroundTailTestReceive(t, done); err != nil {
		t.Fatal(err)
	}
	if got, want := w.String(), "first\nsecond\nthird\nfinal\n"; got != want {
		t.Fatalf("follow output = %q, want %q", got, want)
	}
	if snap, _ := manager.Get(job.ID); snap.Status != background.StatusCompleted {
		t.Fatalf("job status = %s, want completed", snap.Status)
	}
}

func TestFollowBackgroundFileWaitsForCanceledWorkerCleanup(t *testing.T) {
	f := backgroundTailTestFile(t, "initial\n")
	manager := background.NewManager(background.Options{})
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	job, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell", OutputPath: f.Name(),
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			<-ctx.Done()
			<-release
			_, err := f.WriteString("cleanup output\n")
			os.Remove(f.Name())
			return tools.BackgroundJobResult{}, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel(job.ID)
	w := newBackgroundTailTestWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- followBackgroundFile(ctx, w, f, 10, manager, job.ID, ticks, nil) }()
	w.waitFor(t, "initial\n")
	// A follow of an already-canceled but still-live worker must reach its
	// polling wait, rather than exiting based on the canceled public status.
	select {
	case ticks <- time.Time{}:
	case err := <-done:
		t.Fatalf("follow returned before worker cleanup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not wait for worker completion")
	}
	finish()
	if err := backgroundTailTestReceive(t, done); err != nil {
		t.Fatal(err)
	}
	if got := w.String(); got != "initial\ncleanup output\n" {
		t.Fatalf("lost cleanup output: %q", got)
	}
}

func TestFollowBackgroundFileCancellationPreservesJob(t *testing.T) {
	for _, mode := range []string{"context", "force exit"} {
		t.Run(mode, func(t *testing.T) {
			f := backgroundTailTestFile(t, "initial\n")
			manager, job, jobCtx, _ := backgroundTailTestJob(t, f.Name())
			w := newBackgroundTailTestWriter()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exit := make(chan struct{})
			done := make(chan error, 1)
			go func() { done <- followBackgroundFile(ctx, w, f, 10, manager, job.ID, nil, exit) }()
			w.waitFor(t, "initial\n")
			if mode == "context" {
				cancel()
			} else {
				close(exit)
			}
			if err := backgroundTailTestReceive(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("follow error = %v, want context.Canceled", err)
			}
			if snap, _ := manager.Get(job.ID); snap.Status != background.StatusRunning || jobCtx.Err() != nil {
				t.Fatalf("following canceled background job: status %s, context error %v", snap.Status, jobCtx.Err())
			}
		})
	}
}

func TestFollowBackgroundOutputInterruptAndEditorHandoff(t *testing.T) {
	f := backgroundTailTestFile(t, "initial\n")
	manager, job, jobCtx, _ := backgroundTailTestJob(t, f.Name())
	w := newBackgroundTailTestWriter()
	exited := make(chan struct{}, 2)
	watcher := agent.NewInterruptWatcher(nil, nil, func() { exited <- struct{}{} })
	handoffs := make(chan string, 2)
	forceExit := make(chan struct{})
	defer close(forceExit)
	app := &App{
		Errw: w, Background: manager, Interrupt: watcher, ForceExit: forceExit,
		BeforeEditor: func() { handoffs <- "before" },
		AfterEditor:  func() { handoffs <- "after" },
	}
	done := make(chan struct{})
	go func() { app.followBackgroundOutput(job, 10); close(done) }()
	if got := backgroundTailTestReceive(t, handoffs); got != "before" {
		t.Fatalf("first handoff = %q", got)
	}
	w.waitFor(t, "initial\n")
	watcher.InterruptPrompt()
	backgroundTailTestReceive(t, done)
	if got := backgroundTailTestReceive(t, handoffs); got != "after" {
		t.Fatalf("last handoff = %q", got)
	}
	if !strings.Contains(w.String(), "stopped following "+job.ID) {
		t.Fatalf("missing stopped notice: %q", w.String())
	}
	if snap, _ := manager.Get(job.ID); snap.Status != background.StatusRunning || jobCtx.Err() != nil {
		t.Fatalf("Ctrl-C canceled job: status %s, context error %v", snap.Status, jobCtx.Err())
	}
	select {
	case <-exited:
		t.Fatal("first Ctrl-C requested process exit")
	default:
	}
	// EndPrompt must restore idle interrupt behavior after following ends.
	watcher.InterruptPrompt()
	backgroundTailTestReceive(t, exited)
}

func TestBackgroundOutputFilterStreaming(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"CSI", "before\x1b[31mred\x1b[0mafter", "beforeredafter"},
		{"OSC BEL", "before\x1b]0;hidden title\aafter", "beforeafter"},
		{"OSC ST", "before\x1b]8;;https://example.test\x1b\\label\x1b]8;;\x1b\\after", "beforelabelafter"},
		{"DCS", "before\x1bPsecret\x1b\\after", "beforeafter"},
		{"UTF8", "aé界🙂z", "aé界🙂z"},
		{"C1 UTF8 controls", "a\u009b31mb\u009dhidden\u009cc", "abc"},
		{"plain controls", "a\r\x00\b\x7fb\t\nc", "ab\t\nc"},
		{"incomplete rune", "a\xe2\x82", "a\ufffd"},
		{"incomplete escape", "a\x1b]hidden\xe2\x82", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise every two-chunk split and byte-at-a-time streaming.
			for split := 0; split <= len(tc.input); split++ {
				var filter backgroundOutputFilter
				got := filter.text([]byte(tc.input[:split])) + filter.text([]byte(tc.input[split:])) + filter.finish()
				if got != tc.want {
					t.Fatalf("split %d: got %q, want %q", split, got, tc.want)
				}
			}
			var filter backgroundOutputFilter
			var got strings.Builder
			for i := range len(tc.input) {
				got.WriteString(filter.text([]byte(tc.input[i : i+1])))
				if len(filter.pending) >= utf8.UTFMax {
					t.Fatalf("pending buffer too large: %d", len(filter.pending))
				}
			}
			got.WriteString(filter.finish())
			if got.String() != tc.want {
				t.Fatalf("byte chunks: got %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestBackgroundOutputFilterEnormousEscapeStringBounded(t *testing.T) {
	var filter backgroundOutputFilter
	if got := filter.text([]byte("before\x1b]")); got != "before" {
		t.Fatalf("prefix = %q", got)
	}
	chunk := []byte(strings.Repeat("hidden", 16384))
	for range 128 {
		if got := filter.text(chunk); got != "" {
			t.Fatalf("escape payload leaked: %d bytes", len(got))
		}
		if len(filter.pending) != 0 || cap(filter.pending) > utf8.UTFMax {
			t.Fatalf("escape payload retained: len=%d cap=%d", len(filter.pending), cap(filter.pending))
		}
	}
	if got := filter.text([]byte("\x1b")) + filter.text([]byte("\\after")) + filter.finish(); got != "after" {
		t.Fatalf("suffix = %q, want after", got)
	}
}
