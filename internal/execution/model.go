package execution

import (
	"context"
	"harness/internal/llm"
	"sync"
	"time"
)

type attemptState struct {
	fact              llm.AttemptEvent
	usage             llm.Usage
	started, finished bool
	reported          bool
	discarded         bool
	retained          bool
}

// ModelCall owns one provider invocation, including all physical retries and
// continuations. Never reuse a call across invocations. Finish must run even on
// cancellation/client loss; it closes unfinished sources as incomplete without
// manufacturing upstream durations or TTFT. The observed context preserves every
// optional capability because the original provider is never wrapped.
type ModelCall struct {
	mu                sync.Mutex
	scope             Scope
	purpose           llm.RequestPurpose
	start             time.Time
	attempts          map[uint64]*attemptState
	fallback          llm.Usage
	fallbackReported  bool
	fallbackObserved  bool
	retainedReported  bool
	retainedFallback  llm.Usage
	fallbackDiscarded bool
	finished          bool
	done              func()
}

func (s Scope) ModelCall(ctx context.Context, purpose llm.RequestPurpose) (context.Context, *ModelCall) {
	if s.Observer == nil && s.Group == nil {
		return WithScope(ctx, s), nil
	}
	c := &ModelCall{scope: s, purpose: llm.NormalizeRequestPurpose(purpose), start: time.Now(), attempts: make(map[uint64]*attemptState), done: s.Track()}
	ctx = WithScope(ctx, s)
	ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Provider: s.Identity.Provider, Model: s.Identity.Model, Purpose: c.purpose})
	return llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(c.ObserveAttempt)), c
}
func (c *ModelCall) emit(phase ModelPhase, e llm.AttemptEvent, u llm.Usage, reported ...bool) {
	if c.scope.Observer == nil {
		return
	}
	e.Usage = nil
	c.scope.Observer.ObserveModel(ModelEvent{Identity: c.scope.Identity, Phase: phase, Attempt: e, Usage: u, UsageReported: len(reported) > 0 && reported[0]})
}

// ObserveAttempt accepts local source facts or forwarded proxy facts. Their
// per-call Sequence namespace must be preserved end to end. Repeated cumulative
// snapshots, including final snapshots, bill each bucket/cost at most once.
// Usage deltas commit at physical completion, not on provisional snapshots:
// providers may reclassify output as reasoning or 5m cache writes as 1h writes.
// A lost final fact commits the last snapshot in Finish. Price complete snapshots
// before forwarding them, not provisional deltas; CostKnown includes zero cost.
func (c *ModelCall) ObserveAttempt(e llm.AttemptEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e = llm.NormalizeAttemptEvent(e)
	if !c.finished && e.Phase == llm.AttemptRetryWait {
		if e.Purpose == "" || e.Purpose == llm.RequestPurposeUnknown {
			e.Purpose = c.purpose
		}
		c.emit(ModelRetry, e, llm.Usage{})
		return
	}
	if c.finished || e.Sequence == 0 || e.Phase == "" {
		return
	}
	a := c.attempts[e.Sequence]
	if e.Phase == llm.AttemptDiscarded {
		// Dispositions reference previously committed physical usage. Never
		// manufacture an attempt or repeat billing from a disposition frame.
		if a == nil || !a.finished || a.discarded {
			return
		}
		a.discarded = true
		if a.reported && llm.HasUsageDelta(a.usage) {
			fact := a.fact
			fact.DiscardReason = e.DiscardReason
			fact.ErrorClass = llm.AttemptErrorClass("source_" + string(e.DiscardReason))
			c.emit(ModelDiscard, fact, a.usage)
		}
		return
	}
	if a == nil {
		a = &attemptState{}
		c.attempts[e.Sequence] = a
	}
	if a.finished {
		return
	}
	if e.Provider == "" {
		e.Provider = c.scope.Identity.Provider
	}
	if e.Model == "" {
		e.Model = c.scope.Identity.Model
	}
	if e.Purpose == "" || e.Purpose == llm.RequestPurposeUnknown {
		e.Purpose = c.purpose
	}
	a.fact = e
	if !a.started {
		a.started = true
		start := e
		start.Phase = llm.AttemptStarted
		start.Usage = nil
		start.Duration = nil
		start.TTFT = nil
		start.Outcome = ""
		start.ErrorClass = ""
		c.emit(ModelStart, start, llm.Usage{})
	}
	if e.Usage != nil {
		a.reported = true
		u := *e.Usage
		u.CacheWriteTTLKnown = u.CacheWriteTTLKnown || e.CacheWriteTTLKnown
		a.usage = llm.NormalizeUsageSnapshot(u)
	}
	if e.Phase == llm.AttemptFinished && !a.finished {
		a.finished = true
		if a.reported {
			c.emit(ModelUsageDelta, e, a.usage)
		}
		c.emit(ModelFinish, e, llm.Usage{}, a.reported)
	}
}

// ObserveStream is only the legacy/provider-call fallback accounting path. Once
// any source facts arrive, aggregate StreamEvent Usage is never billed as well.
func (c *ModelCall) ObserveStream(e llm.StreamEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		// Even an explicitly synthetic usage frame is evidence that the stream
		// supplied its own presence contract. Never resurrect that placeholder
		// later from the caller's legacy logical aggregate.
		if e.Usage != nil {
			c.fallbackObserved = true
		}
		if e.HasReportedUsage() {
			c.fallbackReported = true
			c.fallback = llm.NormalizeUsageSnapshot(*e.Usage)
		}
	}
}
func (c *ModelCall) Finish(usage llm.Usage, err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.finished = true
	if c.done != nil {
		defer c.done()
	}
	if len(c.attempts) > 0 {
		for _, a := range c.attempts {
			if !a.finished {
				e := a.fact
				e.Phase = llm.AttemptFinished
				e.Outcome = llm.AttemptIncomplete
				e.ErrorClass, e.StatusCode = llm.ClassifyAttemptError(err, e.StatusCode)
				e.Duration = nil
				if a.reported {
					c.emit(ModelUsageDelta, e, a.usage)
				}
				c.emit(ModelFinish, e, llm.Usage{}, a.reported)
				a.finished = true
			}
		}
		return
	}
	if !c.fallbackObserved && llm.HasUsageDelta(usage) {
		c.fallbackReported = true
		c.fallback = llm.NormalizeUsageSnapshot(usage)
	}
	usage = c.fallback
	if c.retainedReported {
		usage = c.retainedFallback
		if c.fallbackReported {
			usage = addReportedUsage(usage, c.fallback)
		}
	}
	reported := c.retainedReported || c.fallbackReported
	duration := time.Since(c.start)
	e := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Scope: llm.AttemptScopeProviderCall, Purpose: c.purpose, Provider: c.scope.Identity.Provider, Model: c.scope.Identity.Model, Cause: llm.AttemptInitial, RetryLayer: llm.RetryLayerNone}, Sequence: 1, Phase: llm.AttemptStarted}
	c.emit(ModelStart, e, llm.Usage{})
	if reported {
		e.Phase = llm.AttemptUsage
		c.emit(ModelUsageDelta, e, usage)
	}
	e.Phase = llm.AttemptFinished
	e.Duration = &duration
	e.Outcome = llm.AttemptSucceeded
	if err != nil {
		e.Outcome = llm.AttemptFailed
	}
	e.ErrorClass, e.StatusCode = llm.ClassifyAttemptError(err, 0)
	if e.ErrorClass == llm.AttemptErrorCancelled {
		e.Outcome = llm.AttemptCancelled
	}
	c.emit(ModelFinish, e, llm.Usage{}, reported)
}
