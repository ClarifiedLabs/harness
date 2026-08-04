package runstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/session"
)

// lockedBuffer is a bytes.Buffer safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// decodeLines splits NDJSON output and decodes each line into a generic map.
func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestWriterEmitsVersionedRunStartFirst(t *testing.T) {
	var out lockedBuffer
	var errw bytes.Buffer
	w := NewWriter(&out, RunStart{
		Mode:      ModeOneshot,
		SessionID: "20260730T020622Z",
		Agent:     "auto",
		Provider:  "anthropic",
		Model:     "claude-opus-4-8",
		Images:    2,
	}, &errw)
	w.Close(RunEnd{ExitCode: 0})

	lines := decodeLines(t, out.String())
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want run_start + run_end: %q", len(lines), out.String())
	}
	start := lines[0]
	if start["type"] != TypeRunStart {
		t.Fatalf("first line type = %v, want run_start", start["type"])
	}
	if start["v"] != float64(Version) {
		t.Fatalf("run_start v = %v, want %d", start["v"], Version)
	}
	if start["mode"] != ModeOneshot || start["session_id"] != "20260730T020622Z" ||
		start["provider"] != "anthropic" || start["model"] != "claude-opus-4-8" ||
		start["agent"] != "auto" || start["images"] != float64(2) {
		t.Fatalf("run_start = %v", start)
	}
	end := lines[1]
	if end["type"] != TypeRunEnd || end["exit_code"] != float64(0) {
		t.Fatalf("run_end = %v", end)
	}
	if errw.Len() != 0 {
		t.Fatalf("errw = %q, want no warnings", errw.String())
	}
}

func TestWriterMirrorsSessionEventsVerbatim(t *testing.T) {
	var out lockedBuffer
	w := NewWriter(&out, RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"}, nil)
	w.PromptStart(PromptStart{Cause: "detached_background_wait"})
	w.Mirror(session.Event{Type: session.EventUser, Prompt: 1, Text: "do it"})
	w.Mirror(session.Event{Type: session.EventAssistantDelta, Prompt: 1, Turn: 1, Attempt: 1, Text: "hello world"})
	remaining := 1
	w.PromptEnd(PromptEnd{
		Cause:               "detached_background_wait",
		ExitCode:            0,
		TerminationReason:   "model_completed",
		ClosureTrigger:      "turn_budget",
		ClosureTurn:         3,
		TurnBudgetExhausted: true,
		WorkflowStatus: &session.WorkflowStatusSnapshot{
			Outcome:               "in_progress",
			RemainingRequirements: &remaining,
		},
		FinalText: "hello world",
		Usage:     PromptEndUsage{Compactions: 2},
	})
	w.Close(RunEnd{ExitCode: 0})

	lines := decodeLines(t, out.String())
	if len(lines) != 6 {
		t.Fatalf("lines = %d, want 6: %q", len(lines), out.String())
	}
	wantTypes := []string{TypeRunStart, TypePromptStart, "user", "assistant_delta", TypePromptEnd, TypeRunEnd}
	for i, want := range wantTypes {
		if lines[i]["type"] != want {
			t.Fatalf("line %d type = %v, want %v (stream: %q)", i, lines[i]["type"], want, out.String())
		}
	}
	if lines[2]["text"] != "do it" || lines[3]["text"] != "hello world" {
		t.Fatalf("mirrored events lost their payload: %q", out.String())
	}
	if lines[1]["cause"] != "detached_background_wait" {
		t.Fatalf("prompt_start cause = %v", lines[1])
	}
	if lines[4]["final_text"] != "hello world" || lines[4]["termination_reason"] != "model_completed" ||
		lines[4]["cause"] != "detached_background_wait" || lines[4]["closure_trigger"] != "turn_budget" || lines[4]["closure_turn"] != float64(3) || lines[4]["turn_budget_exhausted"] != true {
		t.Fatalf("prompt_end = %v", lines[4])
	}
	for _, index := range []int{4, 5} {
		workflow, _ := lines[index]["workflow_status"].(map[string]any)
		if workflow["outcome"] != "in_progress" || workflow["remaining_requirements"] != float64(1) {
			t.Fatalf("line %d workflow status = %v", index, workflow)
		}
	}
	if lines[5]["closure_trigger"] != "turn_budget" || lines[5]["turn_budget_exhausted"] != true {
		t.Fatalf("run_end closure fields = %v", lines[5])
	}
	usage, _ := lines[4]["usage"].(map[string]any)
	if usage["compactions"] != float64(2) {
		t.Fatalf("prompt_end compactions = %v, want 2", usage["compactions"])
	}
}

// gateWriter blocks all writes until the gate opens, forcing the writer's
// buffer to fill so the backpressure path is exercised.
type gateWriter struct {
	gate    <-chan struct{}
	entered chan<- struct{}
	once    *sync.Once
	out     *lockedBuffer
}

func (g gateWriter) Write(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.gate
	return g.out.Write(p)
}

func TestWriterBackpressureNeverDropsOrReorders(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var out lockedBuffer
	var once sync.Once
	w := NewWriter(gateWriter{gate: gate, entered: entered, once: &once, out: &out}, RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"}, nil)
	<-entered

	const events = 600 // comfortably beyond the 256-message buffer
	for i := 0; i < cap(w.messages); i++ {
		w.Mirror(session.Event{Type: session.EventNotice, Text: fmt.Sprintf("n%04d", i)})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := cap(w.messages); i < events; i++ {
			w.Mirror(session.Event{Type: session.EventNotice, Text: fmt.Sprintf("n%04d", i)})
		}
	}()
	close(gate)
	<-done
	w.Close(RunEnd{ExitCode: 0})

	lines := decodeLines(t, out.String())
	if len(lines) != events+2 {
		t.Fatalf("lines = %d, want %d mirrored events + envelopes", len(lines), events+2)
	}
	for i := 0; i < events; i++ {
		want := fmt.Sprintf("n%04d", i)
		if lines[i+1]["text"] != want {
			t.Fatalf("event %d = %v, want %q: stream reordered or dropped", i, lines[i+1]["text"], want)
		}
	}
	if lines[len(lines)-1]["type"] != TypeRunEnd {
		t.Fatalf("last line = %v, want run_end", lines[len(lines)-1])
	}
}

func TestWriterCloseFillsOneshotOutcomeFromPromptEnd(t *testing.T) {
	var out lockedBuffer
	w := NewWriter(&out, RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"}, nil)
	w.PromptEnd(PromptEnd{ExitCode: 1, TerminationReason: "error", Error: "provider exploded"})
	w.Close(RunEnd{ExitCode: 1})

	lines := decodeLines(t, out.String())
	end := lines[len(lines)-1]
	if end["termination_reason"] != "error" || end["error"] != "provider exploded" {
		t.Fatalf("run_end = %v, want one-shot outcome filled from prompt_end", end)
	}
}

func TestWriterCloseInteractiveLeavesRunEndProcessLevel(t *testing.T) {
	var out lockedBuffer
	w := NewWriter(&out, RunStart{Mode: ModeInteractive, SessionID: "s", Provider: "p", Model: "m"}, nil)
	w.PromptEnd(PromptEnd{ExitCode: 0, TerminationReason: "model_completed"})
	w.Close(RunEnd{ExitCode: 0})

	lines := decodeLines(t, out.String())
	end := lines[len(lines)-1]
	if _, ok := end["termination_reason"]; ok {
		t.Fatalf("interactive run_end = %v, want process-level (no termination reason)", end)
	}
}

func TestWriterCloseTwiceEmitsOneRunEnd(t *testing.T) {
	var out lockedBuffer
	w := NewWriter(&out, RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"}, nil)
	w.Close(RunEnd{ExitCode: 0})
	w.Close(RunEnd{ExitCode: 1})
	// Sends after close are dropped silently.
	w.Mirror(session.Event{Type: session.EventNotice, Text: "late"})
	w.PromptEnd(PromptEnd{ExitCode: 0})

	lines := decodeLines(t, out.String())
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want run_start + one run_end: %q", len(lines), out.String())
	}
	if lines[1]["exit_code"] != float64(0) {
		t.Fatalf("run_end exit_code = %v, want first close's 0", lines[1]["exit_code"])
	}
}

func TestWriterWarnsOnceOnWriteFailure(t *testing.T) {
	var errw bytes.Buffer
	w := NewWriter(failWriter{}, RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"}, &errw)
	w.Mirror(session.Event{Type: session.EventNotice, Text: "x"})
	if err := w.Close(RunEnd{ExitCode: 0}); err == nil {
		t.Fatalf("Close error = nil, want the write failure")
	}
	if w.Err() == nil {
		t.Fatalf("Err = nil, want the write failure")
	}
	if got := strings.Count(errw.String(), "runstream: stdout write failed"); got != 1 {
		t.Fatalf("warnings = %d, want exactly one: %q", got, errw.String())
	}
}

func TestWriterWriteFailureTruncatesStreamWithoutMisleadingRunEnd(t *testing.T) {
	wantErr := errors.New("second write failed")
	var out lockedBuffer
	fw := &failSecondWriter{err: wantErr, out: &out}
	w := NewWriter(fw, RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"}, nil)
	w.Mirror(session.Event{Type: session.EventNotice, Text: "fails here"})
	w.Mirror(session.Event{Type: session.EventNotice, Text: "must not resume"})
	if err := w.Close(RunEnd{ExitCode: 0}); !errors.Is(err, wantErr) {
		t.Fatalf("Close error = %v, want %v", err, wantErr)
	}

	lines := decodeLines(t, out.String())
	if len(lines) != 1 || lines[0]["type"] != TypeRunStart {
		t.Fatalf("stream after write failure = %v, want only run_start and no misleading run_end", lines)
	}
}

func TestWriterAbortReleasesBlockedClose(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	abort := make(chan struct{})
	var out lockedBuffer
	var once sync.Once
	w := NewWriterWithAbort(
		gateWriter{gate: gate, entered: entered, once: &once, out: &out},
		RunStart{Mode: ModeOneshot, SessionID: "s", Provider: "p", Model: "m"},
		nil,
		abort,
	)
	<-entered
	for i := 0; i < cap(w.messages); i++ {
		w.Mirror(session.Event{Type: session.EventNotice, Text: fmt.Sprintf("n%d", i)})
	}

	closed := make(chan error, 1)
	go func() { closed <- w.Close(RunEnd{ExitCode: 130}) }()
	close(abort)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close remained blocked after abort")
	}
	close(gate)
	<-w.drained
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("broken pipe") }

type failSecondWriter struct {
	mu    sync.Mutex
	calls int
	err   error
	out   *lockedBuffer
}

func (w *failSecondWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls == 2 {
		return 0, w.err
	}
	return w.out.Write(p)
}
