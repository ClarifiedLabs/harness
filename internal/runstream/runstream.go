// Package runstream owns the JSON run-event vocabulary for the
// `harness -format json` run modes: a versioned NDJSON stream on stdout that
// mirrors the session's durable raw.ndjson event stream between run_start and
// run_end envelopes, with prompt_start/prompt_end envelopes bracketing each
// prompt. stdout carries only this stream; human diagnostics stay on stderr.
//
// The event vocabulary is JSON-schema-first: the mirrored session.Event lines
// are exactly what raw.ndjson records (post-coalescing), so the live stream
// and the replay log can never drift. Consumers key off the "type" field,
// must ignore unknown event types, and must tolerate EOF without run_end
// (process crash).
package runstream

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"harness/internal/session"
)

// Version is the JSON run-stream protocol version, published as run_start.v.
const Version = 1

// Envelope type names. Mirrored session events between run_start and run_end
// keep their raw.ndjson type names ("user", "assistant_delta", "tool_start",
// ...), which never collide with these envelope names.
const (
	TypeRunStart        = "run_start"
	TypeRunEnd          = "run_end"
	TypePromptStart     = "prompt_start"
	TypePromptEnd       = "prompt_end"
	TypeApprovalRequest = "approval_request"
	TypeInputError      = "input_error"
)

// ApprovalKindImplementationHandoff marks an approval_request asking whether
// to execute a request_implementation tool handoff.
const ApprovalKindImplementationHandoff = "implementation_handoff"

// Run modes published as run_start.mode.
const (
	ModeOneshot     = "oneshot"
	ModeInteractive = "interactive"
)

// RunStart is always the first line of a JSON run stream.
type RunStart struct {
	Type      string    `json:"type"`
	V         int       `json:"v"`
	Mode      string    `json:"mode"`
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent,omitempty"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Images    int       `json:"images,omitempty"`
	Time      time.Time `json:"time"`
}

// RunEnd is always the last line of a JSON run stream (best-effort on
// error/interrupt paths). ExitCode mirrors the process exit code.
type RunEnd struct {
	Type              string    `json:"type"`
	ExitCode          int       `json:"exit_code"`
	TerminationReason string    `json:"termination_reason,omitempty"`
	Error             string    `json:"error,omitempty"`
	Time              time.Time `json:"time"`
}

// PromptStart opens a prompt. Prompt is the server-assigned prompt number
// (interactive mode); ID echoes the client-supplied prompt id for
// correlation. Text/Agent/Model describe the accepted prompt in interactive
// mode; one-shot leaves them to run_start and the user event.
type PromptStart struct {
	Type      string    `json:"type"`
	Prompt    int       `json:"prompt,omitempty"`
	ID        string    `json:"id,omitempty"`
	Text      string    `json:"text,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Model     string    `json:"model,omitempty"`
	HasImages bool      `json:"has_images,omitempty"`
	Time      time.Time `json:"time"`
}

// PromptEndUsage summarizes one prompt's token and cost accounting. Full
// fidelity stays in the mirrored prompt_usage session event.
type PromptEndUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	CostKnown    bool    `json:"cost_known,omitempty"`
	Turns        int     `json:"turns"`
}

// PromptEnd closes a prompt with its outcome. FinalText is the last assistant
// text message (the delegate child-report extraction pattern).
type PromptEnd struct {
	Type              string         `json:"type"`
	Prompt            int            `json:"prompt,omitempty"`
	ID                string         `json:"id,omitempty"`
	ExitCode          int            `json:"exit_code"`
	TerminationReason string         `json:"termination_reason,omitempty"`
	Error             string         `json:"error,omitempty"`
	Usage             PromptEndUsage `json:"usage"`
	FinalText         string         `json:"final_text"`
	DurationMS        int64          `json:"duration_ms,omitempty"`
	Time              time.Time      `json:"time"`
}

// ApprovalRequest asks the client to approve or deny a pending action. The
// client answers with an approval_response input message carrying the same
// ID; the prompt boundary waits (interrupt/shutdown still work).
type ApprovalRequest struct {
	Type     string    `json:"type"`
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Brief    string    `json:"brief"`
	PlanPath string    `json:"plan_path"`
	Agent    string    `json:"agent,omitempty"`
	Model    string    `json:"model,omitempty"`
	Time     time.Time `json:"time"`
}

// InputError reports one rejected input line or message; the session keeps
// running. ID echoes the offending message's id when it had one.
type InputError struct {
	Type    string    `json:"type"`
	ID      string    `json:"id,omitempty"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
}

// Writer serializes envelopes and mirrored session events as NDJSON on
// stdout. A buffered writer goroutine does the encoding so event production
// never blocks the agent loop on formatting; when a slow consumer falls a
// full buffer behind, producers apply backpressure instead — the stream is
// never dropped or reordered. Writes are serialized through one channel, so
// concurrent producers (app goroutine, agent loop goroutine) stay safe.
type Writer struct {
	mode  string
	errw  io.Writer
	abort <-chan struct{}

	messages chan any
	drained  chan struct{}

	metaMu     sync.Mutex // guards closed, queue sealing, and lastPrompt
	closed     bool
	lastPrompt PromptEnd

	errMu    sync.Mutex // guards writeErr
	writeErr error
}

type closeMessage struct {
	end RunEnd
}

// NewWriter emits the run_start envelope immediately (it is always line 1)
// and starts the writer goroutine. errw receives one warning line if a stdout
// write ever fails.
func NewWriter(out io.Writer, start RunStart, errw io.Writer) *Writer {
	return newWriter(out, start, errw, nil)
}

// NewWriterWithAbort is NewWriter with a force-exit broadcast. Closing abort
// releases blocked producers and Close without waiting for a stalled stdout
// write. The process may then exit with an intentionally truncated stream.
func NewWriterWithAbort(out io.Writer, start RunStart, errw io.Writer, abort <-chan struct{}) *Writer {
	return newWriter(out, start, errw, abort)
}

func newWriter(out io.Writer, start RunStart, errw io.Writer, abort <-chan struct{}) *Writer {
	if start.Mode == "" {
		start.Mode = ModeOneshot
	}
	if start.Time.IsZero() {
		start.Time = time.Now()
	}
	start.Type = TypeRunStart
	start.V = Version
	w := &Writer{
		mode:     start.Mode,
		errw:     errw,
		abort:    abort,
		messages: make(chan any, 256),
		drained:  make(chan struct{}),
	}
	go func() {
		defer close(w.drained)
		writeFailed := false
		for v := range w.messages {
			// Keep draining after an output failure so producers and Close cannot
			// deadlock, but never resume writing: a later run_end would falsely make
			// the truncated stream look complete.
			if writeFailed {
				continue
			}
			if closeMsg, ok := v.(closeMessage); ok {
				v = closeMsg.end
			}
			if err := json.NewEncoder(out).Encode(v); err != nil {
				w.noteWriteError(err)
				writeFailed = true
			}
		}
	}()
	w.send(start)
	return w
}

// send queues v for the writer goroutine. A full buffer means the consumer is
// stalling; send then blocks (backpressure, the same contract stderr has)
// rather than dropping or reordering events. A force-exit abort releases the
// producer without enqueueing the event.
func (w *Writer) send(v any) bool {
	w.metaMu.Lock()
	defer w.metaMu.Unlock()
	if w.closed {
		return false
	}
	return w.sendLocked(v)
}

func (w *Writer) sendLocked(v any) bool {
	if w.aborted() {
		return false
	}
	select {
	case w.messages <- v:
		return true
	case <-w.abort:
		return false
	}
}

func (w *Writer) aborted() bool {
	if w.abort == nil {
		return false
	}
	select {
	case <-w.abort:
		return true
	default:
		return false
	}
}

func (w *Writer) noteWriteError(err error) {
	w.errMu.Lock()
	if w.writeErr != nil {
		w.errMu.Unlock()
		return
	}
	w.writeErr = err
	w.errMu.Unlock()
	if w.errw != nil {
		fmt.Fprintf(w.errw, "runstream: stdout write failed: %v\n", err)
	}
}

// Err returns the first stdout write failure, if any.
func (w *Writer) Err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.writeErr
}

// Mirror queues one durable session event for the stream. It is installed as
// the sessionrec.Recorder/session.EventAppender mirror in JSON run modes, so
// stdout carries exactly the events raw.ndjson carries, post-coalescing.
func (w *Writer) Mirror(ev session.Event) {
	w.send(ev)
}

// PromptStart emits the prompt_start envelope.
func (w *Writer) PromptStart(start PromptStart) {
	if start.Type == "" {
		start.Type = TypePromptStart
	}
	if start.Time.IsZero() {
		start.Time = time.Now()
	}
	w.send(start)
}

// PromptEnd emits the prompt_end envelope and remembers it so Close can
// mirror the one-shot prompt's outcome into run_end.
func (w *Writer) PromptEnd(end PromptEnd) {
	if end.Type == "" {
		end.Type = TypePromptEnd
	}
	if end.Time.IsZero() {
		end.Time = time.Now()
	}
	w.metaMu.Lock()
	defer w.metaMu.Unlock()
	if w.closed {
		return
	}
	if w.sendLocked(end) {
		w.lastPrompt = end
	}
}

// RequestApproval emits an approval_request envelope for a pending action.
func (w *Writer) RequestApproval(req ApprovalRequest) {
	if req.Type == "" {
		req.Type = TypeApprovalRequest
	}
	if req.Time.IsZero() {
		req.Time = time.Now()
	}
	w.send(req)
}

// InputError reports one rejected input line or message; the session keeps
// running.
func (w *Writer) InputError(id, message string) {
	w.send(InputError{Type: TypeInputError, ID: id, Message: message, Time: time.Now()})
}

// Close emits the run_end envelope, closes the queue, and normally waits for
// the writer goroutine to drain. In one-shot mode an empty TerminationReason
// and Error are filled from the last prompt_end, so run_end mirrors the single
// prompt's outcome. A force-exit abort returns without waiting for a blocked
// output write. Close is safe to call twice and returns the first stdout error.
func (w *Writer) Close(end RunEnd) error {
	w.metaMu.Lock()
	if w.closed {
		w.metaMu.Unlock()
		w.waitForDrain()
		return w.Err()
	}
	w.closed = true
	if w.mode == ModeOneshot {
		if end.TerminationReason == "" {
			end.TerminationReason = w.lastPrompt.TerminationReason
		}
		if end.Error == "" {
			end.Error = w.lastPrompt.Error
		}
	}
	if end.Type == "" {
		end.Type = TypeRunEnd
	}
	if end.Time.IsZero() {
		end.Time = time.Now()
	}
	// Every producer holds metaMu through its enqueue. With closed set, this
	// terminal marker and channel close cannot race another send.
	w.sendLocked(closeMessage{end: end})
	close(w.messages)
	w.metaMu.Unlock()

	w.waitForDrain()
	return w.Err()
}

func (w *Writer) waitForDrain() {
	if w.abort == nil {
		<-w.drained
		return
	}
	select {
	case <-w.drained:
	case <-w.abort:
	}
}
