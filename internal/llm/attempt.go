package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// AttemptPhase identifies transport-safe source facts, independent of diagnostic
// stream events. Usage is always a cumulative snapshot of ONE physical attempt.
type AttemptPhase string

const (
	AttemptStarted  AttemptPhase = "started"
	AttemptUsage    AttemptPhase = "usage"
	AttemptFinished AttemptPhase = "finished"
	// AttemptRetryWait observes a completed retry wait, not a model request.
	// It never carries billable Usage or consumes an attempt sequence.
	AttemptRetryWait AttemptPhase = "retry_wait"
	// AttemptDiscarded is a non-billing disposition of an already finished
	// sequence whose usage was fully omitted from the logical stream result.
	AttemptDiscarded AttemptPhase = "discarded"
)

type AttemptScope string

const (
	AttemptScopeUpstream     AttemptScope = "upstream"
	AttemptScopeProviderCall AttemptScope = "provider_call"
)

type AttemptCause string

const (
	AttemptInitial      AttemptCause = "initial"
	AttemptRetry        AttemptCause = "retry"
	AttemptContinuation AttemptCause = "continuation"
)

type RetryLayer string

const (
	RetryLayerNone     RetryLayer = "none"
	RetryLayerConnect  RetryLayer = "connect"
	RetryLayerProvider RetryLayer = "provider"
	RetryLayerAgent    RetryLayer = "agent"
	RetryLayerProxy    RetryLayer = "proxy"
)

type AttemptOutcome string

const (
	AttemptSucceeded  AttemptOutcome = "success"
	AttemptFailed     AttemptOutcome = "error"
	AttemptCancelled  AttemptOutcome = "cancelled"
	AttemptIncomplete AttemptOutcome = "incomplete"
)

type AttemptErrorClass string

const (
	AttemptErrorNone      AttemptErrorClass = "none"
	AttemptErrorCancelled AttemptErrorClass = "cancelled"
	AttemptErrorTimeout   AttemptErrorClass = "timeout"
	AttemptErrorRateLimit AttemptErrorClass = "rate_limit"
	AttemptErrorAuth      AttemptErrorClass = "auth"
	AttemptErrorRequest   AttemptErrorClass = "request"
	AttemptErrorServer    AttemptErrorClass = "server"
	AttemptErrorTransport AttemptErrorClass = "transport"
	AttemptErrorStream    AttemptErrorClass = "stream"
	AttemptErrorUnknown   AttemptErrorClass = "unknown"
)

// AttemptDiscardReason describes why a completed physical attempt's usage was
// fully omitted from logical StreamEvent usage. It is not an error outcome and
// never reverses physical billing. Only the owner of a suppression boundary may
// emit a disposition; failure alone does not imply discard.
type AttemptDiscardReason string

const (
	AttemptDiscardCompatibility AttemptDiscardReason = "compatibility"
	AttemptDiscardProxyRetry    AttemptDiscardReason = "proxy_retry"
	AttemptDiscardUnknown       AttemptDiscardReason = "unknown"
)

// AttemptMetadata is source identity, not correlation identity. Provider and Model
// must be configured/resolved names, never request IDs or URLs. API and Transport
// describe the actual upstream protocol, not an enclosing proxy connection.
type AttemptMetadata struct {
	Scope      AttemptScope   `json:"scope"`
	Purpose    RequestPurpose `json:"purpose"`
	Provider   string         `json:"provider,omitempty"`
	API        string         `json:"api,omitempty"`
	Model      string         `json:"model,omitempty"`
	Transport  string         `json:"transport,omitempty"`
	Cause      AttemptCause   `json:"cause"`
	RetryLayer RetryLayer     `json:"retry_layer"`
}

// AttemptEvent contains no content, errors, or correlation IDs. Sequence starts
// at one within a provider call and is NOT a metric label. Duration and TTFT are
// nanoseconds, nil when unknown; TTFT measures first generated visible text,
// reasoning, or tool output, never HTTP headers or keepalives. Finish includes
// the last cumulative Usage even on failure. A pricing intermediary may replace
// Usage.CostUSD/CostKnown using this event's actual Provider/Model; consumers
// must de-duplicate cumulative costs by Sequence, never add aggregate billing.
// CacheWriteTTLKnown preserves Usage's nonserialized pricing metadata in transit.
// Retry-wait observations may have Sequence zero: RetryDelay is the planned
// delay and Duration is actual callback elapsed time, not request duration.
// Discarded references an existing nonzero Sequence after its finish. It carries
// only identity and DiscardReason, not new billing or another request outcome;
// consumers recover discarded usage from that sequence's retained physical fact.
type AttemptEvent struct {
	AttemptMetadata
	Phase              AttemptPhase         `json:"phase"`
	Sequence           uint64               `json:"sequence"`
	Usage              *Usage               `json:"usage,omitempty"`
	CacheWriteTTLKnown bool                 `json:"cache_write_ttl_known,omitempty"`
	Duration           *time.Duration       `json:"duration_ns,omitempty"`
	RetryDelay         *time.Duration       `json:"retry_delay_ns,omitempty"`
	TTFT               *time.Duration       `json:"ttft_ns,omitempty"`
	Outcome            AttemptOutcome       `json:"outcome,omitempty"`
	ErrorClass         AttemptErrorClass    `json:"error_class,omitempty"`
	StatusCode         int                  `json:"status_code,omitempty"`
	DiscardReason      AttemptDiscardReason `json:"discard_reason,omitempty"`
}

// AttemptObserver receives source facts synchronously, outside all diagnostic
// buffers. Implementations must be concurrency-safe and must not block on model
// progress. Observation never modifies Request, history, or Provider capabilities.
type AttemptObserver interface{ ObserveAttempt(AttemptEvent) }
type AttemptObserverFunc func(AttemptEvent)

func (f AttemptObserverFunc) ObserveAttempt(e AttemptEvent) { f(e) }

type attemptObserverKey struct{}
type attemptMetadataKey struct{}
type attemptSourceKey struct{}
type attemptObserverState struct {
	observer AttemptObserver
	sequence *attemptSequence
}

// A namespace belongs to one provider call. Tracking subgroups share both its
// allocator and disposition deduplication, never copying an atomic counter.
type attemptSequence struct {
	next      atomic.Uint64
	mu        sync.Mutex
	discarded map[uint64]struct{}
}

// WithAttemptObserver starts a new per-call sequence namespace and replaces any
// inherited observer. Proxy clients forward remote facts with EmitAttempt rather
// than creating fictitious upstream attempts for the proxy connection.
func WithAttemptObserver(ctx context.Context, observer AttemptObserver) context.Context {
	return context.WithValue(ctx, attemptObserverKey{}, &attemptObserverState{observer: observer, sequence: &attemptSequence{}})
}
func AttemptObserverFromContext(ctx context.Context) AttemptObserver {
	if s, _ := ctx.Value(attemptObserverKey{}).(*attemptObserverState); s != nil {
		return s.observer
	}
	return nil
}
func EmitAttempt(ctx context.Context, event AttemptEvent) {
	if s, _ := ctx.Value(attemptObserverKey{}).(*attemptObserverState); s != nil && s.observer != nil {
		if event.Phase == AttemptDiscarded {
			s.sequence.markDiscarded(event.Sequence)
		}
		s.observer.ObserveAttempt(event)
	}
}
func WithAttemptMetadata(ctx context.Context, meta AttemptMetadata) context.Context {
	old := AttemptMetadataFromContext(ctx)
	if meta.Provider == "" {
		meta.Provider = old.Provider
	}
	if meta.Model == "" {
		meta.Model = old.Model
	}
	if meta.Purpose == "" {
		meta.Purpose = old.Purpose
	}
	if meta.API == "" {
		meta.API = old.API
	}
	if meta.Transport == "" {
		meta.Transport = old.Transport
	}
	if meta.Scope == "" {
		meta.Scope = old.Scope
	}
	if meta.Cause == "" {
		meta.Cause = old.Cause
	}
	if meta.RetryLayer == "" {
		meta.RetryLayer = old.RetryLayer
	}
	return context.WithValue(ctx, attemptMetadataKey{}, meta)
}
func AttemptMetadataFromContext(ctx context.Context) AttemptMetadata {
	m, _ := ctx.Value(attemptMetadataKey{}).(AttemptMetadata)
	return m
}

// WithAttemptCause changes only the cause/layer for a retry or continuation.
func WithAttemptCause(ctx context.Context, cause AttemptCause, layer RetryLayer) context.Context {
	m := AttemptMetadataFromContext(ctx)
	m.Cause, m.RetryLayer = cause, layer
	return WithAttemptMetadata(ctx, m)
}

// ObserveRetryWait invokes wait exactly once and returns its error unchanged.
// It emits one non-billing observation after the callback returns, outside all
// diagnostic buffers. Duration measures only callback execution, not observer
// work. A nonnegative planned delay is known (including an immediate zero-delay
// retry); a negative planned delay means unknown and leaves RetryDelay nil.
// ErrorClass retains the bounded reason FOR retrying, even when the wait itself
// is cancelled. Outcome describes the callback: success, error, or cancelled
// (including context deadline expiration). Context cancellation is not injected
// into the callback or its result. No attempt sequence is allocated, so a wait
// that is cancelled before another request cannot inflate request/retry counts.
func ObserveRetryWait(ctx context.Context, planned time.Duration, layer RetryLayer, reason AttemptErrorClass, wait func() error) error {
	observer := AttemptObserverFromContext(ctx)
	if observer == nil {
		return wait()
	}
	meta := AttemptMetadataFromContext(ctx)
	meta.Cause, meta.RetryLayer = AttemptRetry, layer
	if meta.Provider == "" {
		meta.Provider = meta.API
	}
	start := time.Now()
	err := wait()
	elapsed := time.Since(start)
	event := AttemptEvent{
		AttemptMetadata: meta,
		Phase:           AttemptRetryWait,
		Duration:        &elapsed,
		Outcome:         AttemptSucceeded,
		ErrorClass:      reason,
	}
	if planned >= 0 {
		event.RetryDelay = &planned
	}
	if err != nil {
		event.Outcome = AttemptFailed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		event.Outcome = AttemptCancelled
	}
	observer.ObserveAttempt(NormalizeAttemptEvent(event))
	return err
}

// AttemptSource owns one physical lifecycle. It is nil-safe when unobserved.
// Finish is idempotent. Always defer Finish(AttemptIncomplete,nil) when abandoning
// a stream; explicit completion/error wins. No duration is invented for a remote
// attempt whose start was not observed locally.
type AttemptSource struct {
	mu       sync.Mutex
	observer AttemptObserver
	event    AttemptEvent
	start    time.Time
	finished bool
}

func StartAttempt(ctx context.Context) *AttemptSource { return startAttempt(ctx, true) }

// StartAttemptUnknown records a server-initiated response whose request start
// was not observed. It never reports a fabricated duration or TTFT.
func StartAttemptUnknown(ctx context.Context) *AttemptSource { return startAttempt(ctx, false) }

func startAttempt(ctx context.Context, knownStart bool) *AttemptSource {
	s, _ := ctx.Value(attemptObserverKey{}).(*attemptObserverState)
	if s == nil || s.observer == nil {
		return nil
	}
	m := AttemptMetadataFromContext(ctx)
	if m.Scope == "" {
		m.Scope = AttemptScopeUpstream
	}
	if m.Cause == "" {
		m.Cause = AttemptInitial
	}
	if m.RetryLayer == "" {
		m.RetryLayer = RetryLayerNone
	}
	if m.Provider == "" {
		m.Provider = m.API
	}
	m.Purpose = NormalizeRequestPurpose(m.Purpose)
	a := &AttemptSource{observer: s.observer, start: time.Now(), event: AttemptEvent{AttemptMetadata: m, Phase: AttemptStarted, Sequence: s.sequence.next.Add(1)}}
	if !knownStart {
		a.start = time.Time{}
	}
	a.observer.ObserveAttempt(NormalizeAttemptEvent(a.event))
	return a
}
func (a *AttemptSource) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, attemptSourceKey{}, a)
}
func AttemptFromContext(ctx context.Context) *AttemptSource {
	a, _ := ctx.Value(attemptSourceKey{}).(*AttemptSource)
	return a
}
func ResponseAttempt(resp *http.Response) *AttemptSource {
	if resp == nil || resp.Request == nil {
		return nil
	}
	return AttemptFromContext(resp.Request.Context())
}
func (a *AttemptSource) Status(code int) {
	if a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		if code >= 100 && code <= 599 {
			a.event.StatusCode = code
		}
	}
}
func (a *AttemptSource) Generated() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.finished && !a.start.IsZero() && a.event.TTFT == nil {
		d := time.Since(a.start)
		a.event.TTFT = &d
	}
}
func (a *AttemptSource) Usage(u Usage) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.event.Usage = &u
	a.event.CacheWriteTTLKnown = u.CacheWriteTTLKnown
	e := a.event
	e.Phase = AttemptUsage
	a.observer.ObserveAttempt(NormalizeAttemptEvent(e))
}
func (a *AttemptSource) ObserveStream(e StreamEvent, err error) {
	if a == nil {
		return
	}
	if (e.Kind == EventTextDelta || e.Kind == EventReasoningSummary) && e.Text != "" || e.Kind == EventToolCallStart && e.ToolName != "" || e.Kind == EventToolCallDelta && e.ArgsDelta != "" || (e.Kind == EventToolCallDone || e.Kind == EventToolCallReady) && e.ToolName != "" {
		a.Generated()
	}
	if e.HasReportedUsage() {
		a.Usage(*e.Usage)
	}
	if err != nil {
		a.Finish(AttemptFailed, err)
	} else if e.Kind == EventDone {
		a.Finish(AttemptSucceeded, nil)
	}
}
func (a *AttemptSource) WrapYield(yield func(StreamEvent, error) bool) func(StreamEvent, error) bool {
	return func(e StreamEvent, err error) bool { a.ObserveStream(e, err); return yield(e, err) }
}
func (a *AttemptSource) Finish(outcome AttemptOutcome, err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	a.event.Phase = AttemptFinished
	a.event.Outcome = outcome
	a.event.ErrorClass, a.event.StatusCode = ClassifyAttemptError(err, a.event.StatusCode)
	if errors.Is(err, context.Canceled) {
		a.event.Outcome = AttemptCancelled
	}
	if !a.start.IsZero() {
		d := time.Since(a.start)
		a.event.Duration = &d
	}
	a.observer.ObserveAttempt(NormalizeAttemptEvent(a.event))
}

// ClassifyAttemptError deliberately excludes provider error messages/codes.
func ClassifyAttemptError(err error, status int) (AttemptErrorClass, int) {
	if status < 100 || status > 599 {
		status = 0
	}
	if err == nil {
		return AttemptErrorNone, status
	}
	if errors.Is(err, context.Canceled) {
		return AttemptErrorCancelled, status
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return AttemptErrorTimeout, status
	}
	var api *APIError
	if errors.As(err, &api) {
		if api.StatusCode >= 100 && api.StatusCode <= 599 {
			status = api.StatusCode
		}
		switch {
		case status == 429 || status == 529:
			return AttemptErrorRateLimit, status
		case status == 401 || status == 403:
			return AttemptErrorAuth, status
		case status >= 500:
			return AttemptErrorServer, status
		case status >= 400:
			return AttemptErrorRequest, status
		}
		if api.Stage == APIErrorStageUpstreamConnect {
			return AttemptErrorTransport, status
		}
		return AttemptErrorStream, status
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return AttemptErrorTimeout, status
	}
	return AttemptErrorUnknown, status
}

// NormalizeAttemptEvent bounds remote enum/status fields and copies Usage so
// observers can price their event without mutating a source's retained snapshot.
// An unknown Phase is cleared and should be ignored by consumers.
func NormalizeAttemptEvent(e AttemptEvent) AttemptEvent {
	switch e.Phase {
	case AttemptStarted, AttemptUsage, AttemptFinished, AttemptRetryWait, AttemptDiscarded:
	default:
		e.Phase = ""
	}
	switch e.Scope {
	case AttemptScopeUpstream, AttemptScopeProviderCall:
	default:
		e.Scope = AttemptScopeProviderCall
	}
	switch e.Cause {
	case AttemptInitial, AttemptRetry, AttemptContinuation:
	default:
		e.Cause = AttemptInitial
	}
	switch e.RetryLayer {
	case RetryLayerNone, RetryLayerConnect, RetryLayerProvider, RetryLayerAgent, RetryLayerProxy:
	default:
		e.RetryLayer = RetryLayerNone
	}
	switch e.Outcome {
	case "", AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptIncomplete:
	default:
		e.Outcome = AttemptIncomplete
	}
	switch e.ErrorClass {
	case "", AttemptErrorNone, AttemptErrorCancelled, AttemptErrorTimeout, AttemptErrorRateLimit, AttemptErrorAuth, AttemptErrorRequest, AttemptErrorServer, AttemptErrorTransport, AttemptErrorStream, AttemptErrorUnknown:
	default:
		e.ErrorClass = AttemptErrorUnknown
	}
	switch e.API {
	case "openai", "anthropic", "responses", "interactions":
	default:
		e.API = "unknown"
	}
	switch e.Transport {
	case "http", "websocket":
	default:
		e.Transport = "unknown"
	}
	e.Purpose = NormalizeRequestPurpose(e.Purpose)
	if e.StatusCode < 100 || e.StatusCode > 599 {
		e.StatusCode = 0
	}
	if e.Phase == AttemptDiscarded {
		switch e.DiscardReason {
		case AttemptDiscardCompatibility, AttemptDiscardProxyRetry, AttemptDiscardUnknown:
		default:
			e.DiscardReason = AttemptDiscardUnknown
		}
		e.Usage = nil
		e.CacheWriteTTLKnown = false
		e.TTFT = nil
		e.Duration = nil
		e.RetryDelay = nil
		e.Outcome = ""
		e.ErrorClass = ""
		e.StatusCode = 0
		if e.Sequence == 0 {
			e.Phase = "" // a disposition must reference an existing attempt
		}
	} else {
		e.DiscardReason = ""
	}
	if e.Phase == AttemptRetryWait {
		e.Usage = nil
		e.CacheWriteTTLKnown = false
		e.TTFT = nil
	}
	if e.RetryDelay != nil {
		d := *e.RetryDelay
		e.RetryDelay = nil
		if d >= 0 {
			e.RetryDelay = &d
		}
	}
	if e.Usage != nil {
		u := NormalizeUsageSnapshot(*e.Usage)
		e.Usage = &u
	}
	if e.Duration != nil {
		d := *e.Duration
		e.Duration = nil
		if d >= 0 {
			e.Duration = &d
		}
	}
	if e.TTFT != nil {
		d := *e.TTFT
		e.TTFT = nil
		if d >= 0 {
			e.TTFT = &d
		}
	}
	return e
}
