package sessionrec

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
)

// Config configures one session Recorder.
type Config struct {
	// Dir is the session directory holding raw.ndjson. Empty records nothing.
	Dir string
	// Prompt is the prompt number stamped on every recorded event.
	Prompt int
	// Clock supplies event timestamps and turn/prompt durations. Nil uses
	// time.Now.
	Clock func() time.Time
	// ReasoningSummaries gates reasoning_summary recording to match the live
	// display configuration.
	ReasoningSummaries bool
	// PriceTurnUsage prices unpriced turn usage (direct providers) against the
	// active model. Nil leaves streamed cost as-is.
	PriceTurnUsage func(llm.Usage) (cost float64, known bool)
	// PricePromptUsage prices the prompt-usage line. Nil leaves the streamed
	// prompt cost as-is.
	PricePromptUsage func(llm.Usage) (cost float64, known bool)
	// PromptUsageLine renders the durable prompt_usage Display line. It exists
	// because the line carries session-cumulative totals the recorder cannot
	// know. Nil uses DefaultPromptUsageLine, which treats the prompt's own
	// usage as the cumulative totals (correct for single-prompt child
	// sessions).
	PromptUsageLine func(u agent.PromptUsage, promptElapsed time.Duration, cost float64, known bool) string
	// OnError, when non-nil, is called for every append failure. The first
	// error is also retained for Err.
	OnError func(error)
}

// DefaultPromptUsageLine renders the prompt_usage Display line for a session
// whose cumulative totals are the prompt's own usage (child sessions run one
// prompt each).
func DefaultPromptUsageLine(u agent.PromptUsage, promptElapsed time.Duration, cost float64, known bool) string {
	return UsageLine(u, promptElapsed, cost, known, u.Usage.InputTokens, u.Usage.OutputTokens, cost)
}

// Recorder appends parent-fidelity replay events to raw.ndjson. It owns the
// pending-tool map and turn/prompt duration math so the parent and child
// sinks cannot drift. A nil *Recorder is valid and records nothing.
type Recorder struct {
	cfg    Config
	events *session.EventAppender

	mu  sync.Mutex
	err error

	turn        int
	attempt     int
	promptStart time.Time
	turnStart   time.Time
	pending     map[string]pendingCall
}

type pendingCall struct {
	call    llm.ToolCall
	started time.Time
}

// New returns a Recorder writing to cfg.Dir. An empty Dir records nothing.
func New(cfg Config) *Recorder {
	if cfg.Prompt == 0 {
		cfg.Prompt = 1
	}
	return &Recorder{
		cfg:     cfg,
		events:  session.NewEventAppender(cfg.Dir),
		pending: make(map[string]pendingCall),
	}
}

func (r *Recorder) now() time.Time {
	if r.cfg.Clock != nil {
		return r.cfg.Clock()
	}
	return time.Now()
}

func (r *Recorder) noteError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	if r.cfg.OnError != nil {
		r.cfg.OnError(err)
	}
}

// Err returns the first append failure, if any.
func (r *Recorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Append records one fully-formed event, stamping the clock time when unset.
// It is the shared path for event kinds the recorder does not assemble itself
// (checkpoints, retention, skill activations, idle compactions).
func (r *Recorder) Append(ev session.Event) {
	if r == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = r.now()
	}
	r.mu.Lock()
	err := r.events.Append(ev)
	r.mu.Unlock()
	r.noteError(err)
}

// Flush writes any buffered assistant-delta chunk.
func (r *Recorder) Flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	err := r.events.Flush()
	r.mu.Unlock()
	r.noteError(err)
}

// User records the prompt's user event and starts the prompt clock.
func (r *Recorder) User(text string) {
	if r == nil {
		return
	}
	r.promptStart = r.now()
	r.Append(session.Event{Type: session.EventUser, Prompt: r.cfg.Prompt, Text: text})
}

// TurnAttemptStart records the attempt boundary and its context snapshot.
func (r *Recorder) TurnAttemptStart(turn, attempt int, ctx agent.ContextEstimate) {
	if r == nil {
		return
	}
	r.turn = turn
	r.attempt = attempt
	if attempt <= 1 {
		r.turnStart = r.now()
	}
	if r.promptStart.IsZero() {
		r.promptStart = r.now()
	}
	r.Append(session.Event{
		Type:    session.EventTurnAttemptStart,
		Prompt:  r.cfg.Prompt,
		Turn:    turn,
		Attempt: attempt,
		Context: ContextSnapshot(ctx),
	})
}

// TurnAttemptAbandoned records a discarded retry attempt.
func (r *Recorder) TurnAttemptAbandoned(turn, attempt int) {
	if r == nil {
		return
	}
	r.Append(session.Event{
		Type:    session.EventTurnAttemptAbandoned,
		Prompt:  r.cfg.Prompt,
		Turn:    turn,
		Attempt: attempt,
		Display: fmt.Sprintf("[turn: %d attempt %d discarded; retrying]", turn, attempt),
	})
}

// TurnAttemptComplete records the per-attempt usage frame.
func (r *Recorder) TurnAttemptComplete(u agent.TurnAttemptUsage) {
	if r == nil {
		return
	}
	usage := u.Usage
	r.Append(session.Event{
		Type:    session.EventTurnAttemptUsage,
		Prompt:  r.cfg.Prompt,
		Turn:    u.Turn,
		Attempt: u.Attempt,
		Usage:   &usage,
	})
}

// TextDelta records one assistant text chunk (coalesced by the appender).
func (r *Recorder) TextDelta(text string) {
	if r == nil {
		return
	}
	r.Append(session.Event{Type: session.EventAssistantDelta, Prompt: r.cfg.Prompt, Turn: r.turn, Attempt: r.attempt, Text: text})
}

// AssistantPhase records a commentary/final phase transition. Unknown phases
// are dropped, matching the live renderer's gate.
func (r *Recorder) AssistantPhase(phase string) {
	if r == nil || phase == "" || !llm.ValidAssistantPhase(phase) {
		return
	}
	r.Append(session.Event{Type: session.EventAssistantPhase, Prompt: r.cfg.Prompt, Turn: r.turn, Attempt: r.attempt, Phase: phase})
}

// ReasoningSummary records a reasoning summary block when summaries are
// enabled in the session configuration.
func (r *Recorder) ReasoningSummary(text string) {
	if r == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" || !r.cfg.ReasoningSummaries {
		return
	}
	r.Append(session.Event{Type: session.EventReasoningSummary, Prompt: r.cfg.Prompt, Turn: r.turn, Attempt: r.attempt, Text: text})
}

// ToolStart records the call and stashes it for the result line and duration.
func (r *Recorder) ToolStart(call llm.ToolCall) {
	if r == nil {
		return
	}
	r.pending[call.ID] = pendingCall{call: call, started: r.now()}
	r.Append(session.Event{Type: session.EventToolStart, Prompt: r.cfg.Prompt, Turn: r.turn, ToolID: call.ID, Tool: call.Name, Input: call.Input})
}

// ToolResult records the shared one-line summary, duration, and metrics.
func (r *Recorder) ToolResult(res llm.ToolResult) {
	if r == nil {
		return
	}
	pending := r.pending[res.ForID]
	delete(r.pending, res.ForID)
	shownBytes := res.ShownBytes
	if shownBytes == 0 {
		shownBytes = len(res.Text)
	}
	originalBytes := res.OriginalBytes
	if originalBytes == 0 {
		originalBytes = shownBytes
	}
	var durationMS int64
	if !pending.started.IsZero() {
		durationMS = r.now().Sub(pending.started).Milliseconds()
	}
	r.Append(session.Event{
		Type:                session.EventToolResult,
		Prompt:              r.cfg.Prompt,
		Turn:                r.turn,
		ToolID:              res.ForID,
		Tool:                pending.call.Name,
		Display:             ToolResultLine(pending.call, res),
		DurationMS:          durationMS,
		ResultError:         res.IsError,
		ResultTruncated:     res.Truncated,
		ResultOriginalBytes: originalBytes,
		ResultShownBytes:    shownBytes,
		ResultMetrics:       maps.Clone(res.Metrics),
	})
}

// ToolDiff records a rendered unified diff with the mutated file path so
// replay can colorize it.
func (r *Recorder) ToolDiff(call llm.ToolCall, path, text string) {
	if r == nil {
		return
	}
	r.Append(session.Event{
		Type:    session.EventToolDiff,
		Prompt:  r.cfg.Prompt,
		Turn:    r.turn,
		ToolID:  call.ID,
		Tool:    call.Name,
		Path:    path,
		Display: strings.TrimRight(text, "\n"),
	})
}

// Notice records one status line. turn overrides the recorder's current turn
// (maintenance notices record turn 0).
func (r *Recorder) Notice(msg string, turn int) {
	if r == nil {
		return
	}
	r.Append(session.Event{Type: session.EventNotice, Prompt: r.cfg.Prompt, Turn: turn, Display: msg})
}

// ModelRequestEvent records the structured provider-lifecycle event with its
// durable display line, when one is warranted.
func (r *Recorder) ModelRequestEvent(event llm.ModelRequestEvent) {
	if r == nil {
		return
	}
	copyEvent := event
	r.Append(session.Event{
		Type:         session.EventModelRequest,
		Prompt:       r.cfg.Prompt,
		Turn:         r.turn,
		Attempt:      r.attempt,
		Display:      ModelRequestDisplayLine(event),
		ModelRequest: &copyEvent,
	})
}

// TurnComplete prices unpriced usage, measures turn/prompt durations, and
// records the shared turn summary line.
func (r *Recorder) TurnComplete(u agent.TurnUsage) {
	if r == nil {
		return
	}
	if !u.Usage.CostKnown && r.cfg.PriceTurnUsage != nil {
		if cost, ok := r.cfg.PriceTurnUsage(u.Usage); ok {
			u.Usage.CostUSD = cost
			u.Usage.CostKnown = true
		}
	}
	var elapsed time.Duration
	if !r.turnStart.IsZero() {
		elapsed = r.now().Sub(r.turnStart)
	}
	promptElapsed := time.Duration(-1)
	if !r.promptStart.IsZero() {
		promptElapsed = r.now().Sub(r.promptStart)
	}
	usage := u.Usage
	r.Append(session.Event{
		Type:    session.EventTurnComplete,
		Prompt:  r.cfg.Prompt,
		Turn:    u.Turn,
		Display: TurnUsageLine(u, elapsed, promptElapsed),
		Usage:   &usage,
	})
}

// MaintenanceComplete records one maintenance (compaction/summarization)
// usage frame.
func (r *Recorder) MaintenanceComplete(u agent.MaintenanceUsage) {
	if r == nil {
		return
	}
	usage := u.Usage
	r.Append(session.Event{Type: session.EventMaintenanceUsage, Prompt: r.cfg.Prompt, Purpose: u.Purpose, Usage: &usage})
}

// PromptComplete prices the prompt usage and records the closing prompt_usage
// event. It must remain the last event written for a session prompt.
func (r *Recorder) PromptComplete(u agent.PromptUsage) {
	if r == nil {
		return
	}
	cost, costKnown := u.Usage.CostUSD, u.Usage.CostKnown
	if r.cfg.PricePromptUsage != nil {
		cost, costKnown = r.cfg.PricePromptUsage(u.Usage)
	}
	var promptElapsed time.Duration
	if !r.promptStart.IsZero() {
		promptElapsed = r.now().Sub(r.promptStart)
	}
	line := DefaultPromptUsageLine
	if r.cfg.PromptUsageLine != nil {
		line = r.cfg.PromptUsageLine
	}
	usage := u.Usage
	r.Append(session.Event{
		Type:              session.EventPromptUsage,
		Prompt:            r.cfg.Prompt,
		Display:           line(u, promptElapsed, cost, costKnown),
		Usage:             &usage,
		TerminationReason: string(u.TerminationReason),
	})
}
