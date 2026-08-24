package sessionrec

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"harness/internal/agent"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/trajectory"
)

// Config configures one session Recorder.
type Config struct {
	// Dir is the session directory holding raw.ndjson. Empty records nothing.
	Dir string
	// Prompt is the prompt number stamped on every recorded event.
	Prompt int
	// Initial execution identity covers providers that do not emit successful
	// model_request lifecycle events. Later lifecycle events refresh the model.
	Agent       string
	ModelTarget string
	Provider    string
	APIType     string
	Model       string
	// Clock supplies event timestamps and turn/prompt durations. Nil uses
	// time.Now.
	Clock func() time.Time
	// ReasoningSummaries gates reasoning_summary recording to match the live
	// display configuration.
	ReasoningSummaries bool
	// CWD is the session working directory; path args in tool Display lines
	// are shown relative to it when they lie under it.
	CWD string
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
	// Mirror, when non-nil, receives every event after it has been durably
	// written to raw.ndjson (post-coalescing). JSON run modes use it to
	// mirror the replay stream to stdout.
	Mirror func(session.Event)
	// OnError, when non-nil, is called for every append failure. The first
	// error is also retained for Err.
	OnError func(error)
	// Trajectory receives diagnostics-only host observations after their source
	// events are durably appended. Nil disables the shadow projection.
	Trajectory *trajectory.Tracker
}

// DefaultPromptUsageLine renders the prompt_usage Display line for a session
// whose cumulative totals are the prompt's own usage (child sessions run one
// prompt each).
func DefaultPromptUsageLine(u agent.PromptUsage, promptElapsed time.Duration, cost float64, known bool) string {
	return UsageLine(u, promptElapsed, cost, known, u.Usage.InputTokens, u.Usage.OutputTokens, cost, u.Compactions)
}

// ExecutionIdentity identifies the agent/model execution that launched work.
type ExecutionIdentity struct {
	Agent       string
	ModelTarget string
	Provider    string
	APIType     string
	Model       string
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
	model       modelIdentity
}

type modelIdentity struct {
	targetID string
	provider string
	apiType  string
	model    string
}

type pendingCall struct {
	call    llm.ToolCall
	started time.Time
	model   modelIdentity
}

// New returns a Recorder writing to cfg.Dir. An empty Dir records nothing.
func New(cfg Config) *Recorder {
	if cfg.Prompt == 0 {
		cfg.Prompt = 1
	}
	events := session.NewEventAppender(cfg.Dir)
	events.Mirror = cfg.Mirror
	return &Recorder{
		cfg:     cfg,
		events:  events,
		pending: make(map[string]pendingCall),
		model:   modelIdentity{targetID: cfg.ModelTarget, provider: cfg.Provider, apiType: cfg.APIType, model: cfg.Model},
	}
}

func (r *Recorder) now() time.Time {
	if r.cfg.Clock != nil {
		return r.cfg.Clock()
	}
	return time.Now()
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
//
// The Mirror callback (JSON run-stream mirroring) deliberately runs under the
// mutex so mirrored events keep their append order; the unlock is deferred so
// a panicking mirror cannot wedge every later recorder call.
func (r *Recorder) Append(ev session.Event) {
	_ = r.appendEvent(ev)
}

func (r *Recorder) appendEvent(ev session.Event) error {
	return r.appendEventAfterFlush(ev, false)
}

func (r *Recorder) appendEventAfterFlush(ev session.Event, requireFlush bool) error {
	if r == nil {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = r.now()
	}
	err := r.appendLocked(func() error {
		// Diagnostics are acknowledged only after this returns nil. Flush pending
		// assistant text first and do not attempt the diagnostic append if that
		// flush fails, avoiding a successful write followed by a duplicate retry.
		if requireFlush {
			if err := r.events.Flush(); err != nil {
				return err
			}
		}
		return r.events.Append(ev)
	})
	if err != nil && r.cfg.OnError != nil {
		r.cfg.OnError(err)
	}
	return err
}

// Flush writes any buffered assistant-delta chunk. Mirror delivery semantics
// match Append.
func (r *Recorder) Flush() {
	if r == nil {
		return
	}
	if err := r.appendLocked(r.events.Flush); err != nil && r.cfg.OnError != nil {
		r.cfg.OnError(err)
	}
}

// appendLocked runs one EventAppender write under the mutex with a deferred
// unlock and retains the first failure.
func (r *Recorder) appendLocked(write func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := write()
	if err != nil && r.err == nil {
		r.err = err
	}
	return err
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
		Type:        session.EventTurnAttemptStart,
		Prompt:      r.cfg.Prompt,
		Turn:        turn,
		Attempt:     attempt,
		Agent:       r.cfg.Agent,
		ModelTarget: r.model.targetID,
		Provider:    r.model.provider,
		APIType:     r.model.apiType,
		Model:       r.model.model,
		Context:     ContextSnapshot(ctx),
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
	r.pending[call.ID] = pendingCall{call: call, started: r.now(), model: r.model}
	r.Append(session.Event{
		Type: session.EventToolStart, Prompt: r.cfg.Prompt, Turn: r.turn,
		ToolID: call.ID, Tool: call.Name, Input: call.Input,
		ModelTarget: r.model.targetID, Provider: r.model.provider,
		APIType: r.model.apiType, Model: r.model.model,
	})
}

// PendingToolIdentity returns the execution identity captured when a tool call
// started. It lets detached completion diagnostics retain launch attribution.
func (r *Recorder) PendingToolIdentity(id string) (ExecutionIdentity, bool) {
	if r == nil {
		return ExecutionIdentity{}, false
	}
	pending, ok := r.pending[id]
	if !ok {
		return ExecutionIdentity{}, false
	}
	return ExecutionIdentity{
		Agent:       r.cfg.Agent,
		ModelTarget: pending.model.targetID,
		Provider:    pending.model.provider,
		APIType:     pending.model.apiType,
		Model:       pending.model.model,
	}, true
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
	var errorExcerpt string
	if res.IsError {
		errorExcerpt = session.ErrorExcerpt(res.Text)
	}
	var artifactRef string
	if res.Truncated {
		artifactRef = session.ToolResultArtifactReference(r.cfg.Prompt, r.turn, res.ForID)
	}
	r.Append(session.Event{
		Type:                session.EventToolResult,
		Prompt:              r.cfg.Prompt,
		Turn:                r.turn,
		ToolID:              res.ForID,
		Tool:                pending.call.Name,
		Display:             ToolResultLine(pending.call, res, r.cfg.CWD),
		DurationMS:          durationMS,
		ResultError:         res.IsError,
		ErrorKind:           string(res.ErrorKind),
		ErrorExcerpt:        errorExcerpt,
		ResultTruncated:     res.Truncated,
		ResultOriginalBytes: originalBytes,
		ResultShownBytes:    shownBytes,
		ArtifactRef:         artifactRef,
		ResultMetrics:       maps.Clone(res.Metrics),
		ModelTarget:         pending.model.targetID,
		Provider:            pending.model.provider,
		APIType:             pending.model.apiType,
		Model:               pending.model.model,
	})
}

// BackgroundJobResult records diagnostics for a completed detached job without
// presenting it as a second ordinary tool result. The launch receipt remains the
// tool result paired with the original call; analysis consumes this metrics-only
// event as the eventual command outcome.
func (r *Recorder) BackgroundJobResult(id, tool, status string, duration time.Duration, metrics map[string]int) {
	r.BackgroundJobResultWithIdentity(id, tool, status, duration, metrics, ExecutionIdentity{})
}

// BackgroundJobResultWithIdentity records a detached outcome against its launch
// identity. A zero identity falls back to the recorder's current execution.
func (r *Recorder) BackgroundJobResultWithIdentity(id, tool, status string, duration time.Duration, metrics map[string]int, identity ExecutionIdentity) error {
	if r == nil || len(metrics) == 0 {
		return nil
	}
	if identity == (ExecutionIdentity{}) {
		identity = ExecutionIdentity{
			Agent:       r.cfg.Agent,
			ModelTarget: r.model.targetID,
			Provider:    r.model.provider,
			APIType:     r.model.apiType,
			Model:       r.model.model,
		}
	}
	return r.appendEventAfterFlush(session.Event{
		Type:          session.EventBackgroundJobResult,
		Prompt:        r.cfg.Prompt,
		Turn:          r.turn,
		ToolID:        id,
		Tool:          tool,
		Summary:       status,
		DurationMS:    duration.Milliseconds(),
		ResultMetrics: maps.Clone(metrics),
		Agent:         identity.Agent,
		ModelTarget:   identity.ModelTarget,
		Provider:      identity.Provider,
		APIType:       identity.APIType,
		Model:         identity.Model,
	}, true)
}

// ToolDiff records a rendered unified diff with the mutated file path so
// replay can colorize it.
func (r *Recorder) ToolDiff(call llm.ToolCall, path, text string) {
	if r == nil {
		return
	}
	if err := r.appendEvent(session.Event{
		Type:    session.EventToolDiff,
		Prompt:  r.cfg.Prompt,
		Turn:    r.turn,
		ToolID:  call.ID,
		Tool:    call.Name,
		Path:    path,
		Display: strings.TrimRight(text, "\n"),
	}); err == nil && r.cfg.Trajectory != nil {
		r.cfg.Trajectory.ConfirmModifiedPaths([]string{path})
	}
}

// ToolMutation records bounded host-derived mutation paths independently of
// diff rendering, then advances the shadow projection after durable append.
func (r *Recorder) ToolMutation(call llm.ToolCall, paths []string) {
	if r == nil {
		return
	}
	snapshot := ToolMutationSnapshot(paths)
	if snapshot == nil || len(snapshot.Paths) == 0 {
		return
	}
	if err := r.appendEvent(session.Event{
		Type:         session.EventToolMutation,
		Prompt:       r.cfg.Prompt,
		Turn:         r.turn,
		ToolID:       call.ID,
		Tool:         call.Name,
		ToolMutation: snapshot,
	}); err == nil && r.cfg.Trajectory != nil {
		r.cfg.Trajectory.ObserveModifiedPaths(snapshot.Paths)
	}
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
	if event.TargetID != "" {
		r.model.targetID = event.TargetID
	}
	if event.Provider != "" {
		r.model.provider = event.Provider
	}
	if event.APIType != "" {
		r.model.apiType = event.APIType
	}
	if event.Model != "" {
		r.model.model = event.Model
	}
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

// HookDiagnostic records bounded hook execution metadata without model-visible
// or command/output content.
func (r *Recorder) HookDiagnostic(diagnostic hooks.Diagnostic) {
	if r == nil {
		return
	}
	r.Append(session.Event{
		Type:           session.EventHookDiagnostic,
		Prompt:         r.cfg.Prompt,
		Turn:           r.turn,
		HookDiagnostic: HookDiagnosticSnapshot(diagnostic),
	})
}

// EvaluatorResult records bounded candidate fitness without free-form hook
// output. The detailed evaluator evidence remains behind EvidenceRef.
func (r *Recorder) EvaluatorResult(result hooks.EvaluatorResult) {
	if r == nil {
		return
	}
	snapshot := EvaluatorResultSnapshot(result)
	if err := r.appendEvent(session.Event{
		Type:            session.EventEvaluatorResult,
		Prompt:          r.cfg.Prompt,
		Turn:            r.turn,
		EvaluatorResult: snapshot,
	}); err == nil && r.cfg.Trajectory != nil {
		r.cfg.Trajectory.ObserveEvaluation(trajectory.EvaluationInput{
			Handler:               snapshot.Handler,
			Accepted:              snapshot.Accepted,
			Score:                 snapshot.Score,
			ScoreDirection:        snapshot.ScoreDirection,
			Candidate:             snapshot.Candidate,
			RemainingRequirements: snapshot.RemainingRequirements,
			EvidenceRef:           snapshot.EvidenceRef,
			Prompt:                r.cfg.Prompt,
			Turn:                  r.turn,
		})
	}
}

// LineageAdvance records one durable, content-free accepted-lineage receipt.
// The detailed candidate, score, tree, patch, and evidence metadata live in
// <session>/lineage/state.json. Returning the append error lets the opt-in
// workflow fail visibly instead of claiming an unrecorded advancement.
func (r *Recorder) LineageAdvance(snapshot session.LineageAdvanceSnapshot) error {
	if r == nil {
		return nil
	}
	return r.appendEvent(session.Event{
		Type:           session.EventLineageAdvance,
		Prompt:         r.cfg.Prompt,
		Turn:           r.turn,
		LineageAdvance: &snapshot,
	})
}

// TryStagnationNudge decides, records, and advances one lane-scoped
// strategy-reset trigger. It returns true only when the canonical event was
// persisted, so model-visible control flow never outruns replayable state.
func (r *Recorder) TryStagnationNudge(threshold int) bool {
	if r == nil || r.cfg.Trajectory == nil {
		return false
	}
	current := r.cfg.Trajectory.Snapshot()
	if !trajectory.CanStagnationNudge(current, threshold) {
		return false
	}
	if err := r.appendEvent(session.Event{
		Type:    session.EventStagnationNudge,
		Prompt:  r.cfg.Prompt,
		Turn:    r.turn,
		Display: StagnationNudgeDisplay(threshold),
		StagnationNudge: &session.StagnationNudgeSnapshot{
			Threshold: threshold,
			Streak:    current.NoImprovementStreak,
		},
	}); err != nil {
		return false
	}
	r.cfg.Trajectory.ObserveStagnationNudge()
	return true
}

// StagnationNudgeDisplay is the content-free live/replay status line for a
// delivered trigger.
func StagnationNudgeDisplay(threshold int) string {
	return fmt.Sprintf("[strategy reset: evaluator no-improvement threshold %d reached]", threshold)
}

// SeedTrajectory makes inherited state replayable in a fresh physical child
// session. Ordinary root sessions derive from their own event stream and do
// not need a seed.
func (r *Recorder) SeedTrajectory(state *trajectory.State, purpose string) {
	if r == nil || state == nil {
		return
	}
	normalized := trajectory.Normalize(state)
	if err := r.appendEvent(session.Event{
		Type:       session.EventTrajectorySeed,
		Prompt:     r.cfg.Prompt,
		Purpose:    strings.TrimSpace(purpose),
		Trajectory: &normalized,
	}); err == nil && r.cfg.Trajectory != nil {
		r.cfg.Trajectory.Replace(&normalized)
	}
}

// Branch records a host-owned conversation branch transition. Callers reset
// their live projection only when the returned append succeeds.
func (r *Recorder) Branch(from, to, summary, source string) error {
	if r == nil {
		return nil
	}
	err := r.appendEvent(session.Event{
		Type:        session.EventBranch,
		Prompt:      r.cfg.Prompt,
		Display:     fmt.Sprintf("[%s: %s → %s; working directory unchanged]", source, shortID(from), shortID(to)),
		FromEntryID: from,
		ToEntryID:   to,
		Purpose:     source,
		Summary:     summary,
	})
	if err == nil && r.cfg.Trajectory != nil {
		r.cfg.Trajectory.ResetForBranch()
	}
	return err
}

func shortID(id string) string {
	if id == "" {
		return "root"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// TurnProgress records diagnostics-only semantic activity after a tool turn.
func (r *Recorder) TurnProgress(progress agent.TurnProgress) {
	if r == nil {
		return
	}
	r.Append(session.Event{
		Type:         session.EventTurnProgress,
		Prompt:       r.cfg.Prompt,
		Turn:         progress.Turn,
		TurnProgress: TurnProgressSnapshot(progress),
	})
}

// ClosureStarted records the first closure trigger while a prompt is still in
// flight, so interrupted sessions retain the exact trigger turn.
func (r *Recorder) ClosureStarted(event agent.ClosureEvent) {
	if r == nil {
		return
	}
	r.Append(session.Event{
		Type:             session.EventClosure,
		Prompt:           r.cfg.Prompt,
		Turn:             event.Turn,
		ClosureTrigger:   string(event.Trigger),
		ClosureTurn:      event.Turn,
		WorkflowStatus:   WorkflowStatusSnapshot(event.WorkflowStatus),
		TelemetryVersion: session.ReliabilityTelemetryVersion,
	})
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
		Type:                session.EventPromptUsage,
		Prompt:              r.cfg.Prompt,
		Display:             line(u, promptElapsed, cost, costKnown),
		Usage:               &usage,
		Compactions:         u.Compactions,
		TerminationReason:   string(u.TerminationReason),
		ClosureTrigger:      string(u.ClosureTrigger),
		ClosureTurn:         u.ClosureTurn,
		TurnBudgetExhausted: u.TurnBudgetExhausted,
		WorkflowStatus:      WorkflowStatusSnapshot(u.WorkflowStatus),
		TelemetryVersion:    session.ReliabilityTelemetryVersion,
	})
}
