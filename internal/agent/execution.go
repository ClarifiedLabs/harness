package agent

import (
	"context"
	"errors"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
)

// executionScope returns a value snapshot. In particular, background work must
// never consult the live agent's identity after a model/agent switch.
func (a *Agent) executionScope() execution.Scope {
	scope := a.execution
	if scope.Identity.Model == "" {
		scope.Identity.Model = a.model
	}
	if scope.Identity.Provider == "" && a.provider != nil {
		scope.Identity.Provider = a.provider.Name()
	}
	return scope
}

func executionOutcome(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	if err != nil {
		return "error"
	}
	return "success"
}

func observeModelCall(ctx context.Context, scope execution.Scope, req llm.Request, window int) (context.Context, *execution.ModelCall) {
	if scope.Observer != nil {
		tokens := req.EstimatedInputTokens
		if tokens <= 0 {
			tokens = estimateRequest(req, window).Total
		}
		observeRequestContext(scope, req, tokens, window)
	}
	return scope.ModelCall(ctx, req.Purpose)
}

func observeRequestContext(scope execution.Scope, req llm.Request, tokens, window int) {
	if scope.Observer == nil {
		return
	}
	composition := execution.ComposeRequest(req)
	scope.Context(execution.ContextEvent{Reason: "request_attempt", Before: tokens, After: tokens, Limit: window, Composition: &composition})
}

func (a *Agent) observeRetention(event RetentionEvent) {
	a.executionScope().Context(execution.ContextEvent{
		Reason: "retention", Policy: string(event.Policy),
		Before: event.ContextTokensBefore, After: event.ContextTokensAfter,
		Limit: a.window(), Retained: event.ContextTokensAfter, Dropped: event.EstimatedTokensRemoved,
		BytesBefore: event.BytesBefore, BytesAfter: event.BytesAfter, BytesRemoved: event.BytesRemoved,
		TokensRemoved: event.EstimatedTokensRemoved, BlocksTrimmed: event.BlocksTrimmed,
		DecisionSource: event.DecisionContextSource, PreviousRequestMode: string(event.PreviousRequestMode), NextRequestMode: string(event.NextRequestMode),
		ResponseStateReset: event.ResponseStateReset, MeasurementAnchorReset: event.MeasurementAnchorReset, ContinuationStateReset: event.ContinuationStateReset,
	})
}

type compactionObservation struct {
	scope                   execution.Scope
	started                 time.Time
	trigger, mode, fallback string
	before                  int
}

func (a *Agent) startCompaction(opts compactOptions) *compactionObservation {
	o := &compactionObservation{scope: a.executionScope(), started: time.Now(), trigger: opts.trigger, mode: "textual", before: a.estimateContext(nil).Total}
	if a.contextManager() != nil {
		o.mode = "task_notes"
	} else if a.nativeCompactionEligible(opts.trigger, opts.collapseAll, opts.focus) {
		o.mode = "native"
	}
	o.scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkStart, Trigger: o.trigger, Mode: o.mode, Count: 1, ContextBefore: o.before})
	return o
}

func (o *compactionObservation) finish(a *Agent, changed bool, err error) {
	duration := time.Since(o.started)
	after := a.estimateContext(nil).Total
	outcome := executionOutcome(err)
	if err == nil && !changed {
		outcome = "noop"
	} else if err == nil && o.fallback != "" {
		outcome = "fallback"
	} else if err == nil && o.trigger == "idle" {
		outcome = "prepared"
	}
	// A private idle worker has not reclaimed the owner's live context. Its
	// eventual application emits a separate, uncounted result observation.
	if o.trigger == "idle" {
		after = o.before
	}
	compactions := 0
	if changed && o.trigger != "idle" {
		compactions = 1
	}
	o.scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkFinish, Trigger: o.trigger, Mode: o.mode, Outcome: outcome, FallbackReason: o.fallback, RunDuration: &duration, Count: 1, Compactions: compactions, ContextBefore: o.before, ContextAfter: after})
	if o.trigger != "idle" {
		o.scope.Context(execution.ContextEvent{Reason: "compaction", Policy: o.mode, Before: o.before, After: after, Limit: a.window(), Retained: after, Dropped: max(0, o.before-after), TokensRemoved: max(0, o.before-after)})
	}
}

// dominantActivity preserves the logical-turn classifier's fixed tie order.
// Operation counts, not outer tool-call counts, determine the dominant class.
func dominantActivity(activity ToolActivityCounts) string {
	counts := [...]int{activity.Inspect, activity.Mutate, activity.Verify, activity.Wait, activity.Coordinate, activity.Other}
	names := [...]string{"inspect", "mutate", "verify", "wait", "coordinate", "other"}
	dominant := 0
	for i := 1; i < len(counts); i++ {
		if counts[i] > counts[dominant] {
			dominant = i
		}
	}
	return names[dominant]
}

// cachedAsyncCall is only a dispatch decision: reuse never counts as new work.
func cachedAsyncCall(ctx context.Context, call llm.ToolCall) bool {
	cached, _ := ctx.Value(asyncResultsKey{}).(map[string]asyncReadResult)
	result, ok := cached[call.ID]
	return ok && result.name == call.Name && result.inputHash == llm.NormalizedToolCallHash(call.Input)
}

// maintenanceLedger tracks whole provider invocations, not cumulative summary
// totals. Nested map/reduce and retry layers share a ledger; a discarded entry
// can never be discarded again by a later summary replacement or stale idle
// result. This is diagnostics-only: it never changes billing or archived usage.
// One maintenance operation is serialized, with idle ownership handed back to
// the foreground through the candidate result.
type maintenanceLedger struct{ entries []*execution.ModelCall }
type maintenanceLedgerKey struct{}

func trackMaintenance(ctx context.Context) (context.Context, *maintenanceLedger, bool) {
	if ledger, ok := ctx.Value(maintenanceLedgerKey{}).(*maintenanceLedger); ok {
		return ctx, ledger, false
	}
	ledger := &maintenanceLedger{}
	return context.WithValue(ctx, maintenanceLedgerKey{}, ledger), ledger, true
}
func (l *maintenanceLedger) mark() int { return len(l.entries) }
func recordMaintenanceCall(ctx context.Context, call *execution.ModelCall) {
	if ledger, ok := ctx.Value(maintenanceLedgerKey{}).(*maintenanceLedger); ok {
		ledger.entries = append(ledger.entries, call)
	}
}
func (l *maintenanceLedger) discardFrom(mark int, reason string) {
	for i := mark; i < len(l.entries); i++ {
		l.entries[i].Discard(reason)
	}
}

// An idle result is delivered once for telemetry even if a caller retries an
// already-consumed candidate. Preparation billed its model calls; this only
// records disposition and effective live-context reclamation.
func (c *idleCompactionCandidate) observeResult(a *Agent, applied bool, err error) {
	if err != nil {
		// Archive/validation errors do not consume the candidate: the caller
		// may retry applying it while its fingerprint still matches. Report
		// the failed delivery, but do not invent a terminal usage discard.
		delivery := time.Since(c.preparedAt)
		c.scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkResult, Mode: "textual", Trigger: "idle", Outcome: executionOutcome(err), ContextBefore: c.archive.TokensBefore, ContextAfter: c.archive.TokensBefore, DeliveryDuration: &delivery})
		return
	}
	c.observed.Do(func() {
		c.consumed = true
		outcome := "stale"
		before, after := c.archive.TokensBefore, c.archive.TokensBefore
		compactions := 0
		if applied {
			outcome = "applied"
			after = a.estimateContext(nil).Total
			compactions = 1
			c.scope.Context(execution.ContextEvent{Reason: "compaction", Policy: "textual", Before: before, After: after, Limit: a.window(), Retained: after, Dropped: max(0, before-after), TokensRemoved: max(0, before-after)})
		} else if c.ledger != nil {
			c.ledger.discardFrom(0, "stale_idle")
		}
		delivery := time.Since(c.preparedAt)
		c.scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkResult, Mode: "textual", Trigger: "idle", Outcome: outcome, Compactions: compactions, ContextBefore: before, ContextAfter: after, DeliveryDuration: &delivery})
	})
}

// Retry waits between provider calls cannot use the already-finished call's
// observer. Scope owns their independent non-billing lifecycle instead.
func (a *Agent) retryWait(ctx context.Context, purpose llm.RequestPurpose, planned time.Duration, reason llm.AttemptErrorClass, wait func() error) error {
	ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Purpose: purpose})
	return a.executionScope().RetryWait(ctx, planned, llm.RetryLayerAgent, reason, wait)
}

func (c *idleCompactionCandidate) discard() {
	c.observed.Do(func() {
		c.consumed = true
		if c.ledger != nil {
			c.ledger.discardFrom(0, "stale_idle")
		}
		delivery := time.Since(c.preparedAt)
		c.scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkResult, Mode: "textual", Trigger: "idle", Outcome: "discarded", ContextBefore: c.archive.TokensBefore, ContextAfter: c.archive.TokensBefore, DeliveryDuration: &delivery})
	})
}
