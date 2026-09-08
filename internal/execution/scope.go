// Package execution defines neutral, caller-owned execution observations. It
// imports neither agent nor telemetry SDKs and never wraps Provider interfaces.
package execution

import (
	"context"
	"harness/internal/llm"
	"time"
)

// Identity contains configured low-cardinality names, never session/tool-call/
// delegate IDs. Delegate is a configured delegate role, not a unique child ID.
type Identity struct{ Provider, Model, Agent, Delegate string }

// Scope is immutable by convention; pass values or use Rebind rather than mutate
// shared configuration. Observer implementations must be concurrency-safe.
type Scope struct {
	Observer Observer
	Identity Identity
	Group    *Group
}
type Observer interface {
	ObserveModel(ModelEvent)
	ObserveWork(WorkEvent)
	ObservePrompt(PromptEvent)
	ObserveContext(ContextEvent)
}
type ModelPhase string

const (
	ModelStart      ModelPhase = "start"
	ModelUsageDelta ModelPhase = "usage_delta"
	ModelFinish     ModelPhase = "finish"
	ModelRetry      ModelPhase = "retry_wait"
	// ModelDiscard records usage already billed but discarded by a caller. It
	// must never be folded into the ordinary token or cost counters a second time.
	ModelDiscard ModelPhase = "discard"
)

// ModelEvent emits billable Usage ONLY in usage_delta observations. Attempt.Usage
// is cleared to avoid accidental double billing. Sequence is correlation state,
// not a label. Provider/model in Attempt override caller identity for pricing.
type ModelEvent struct {
	Identity Identity
	Phase    ModelPhase
	Attempt  llm.AttemptEvent
	Usage    llm.Usage
	// UsageReported is meaningful on ModelFinish and distinguishes an observed
	// zero-token snapshot from missing provider usage. Usage remains nonbilling
	// on finish; only ModelUsageDelta carries exclusive spend.
	UsageReported bool
}
type WorkKind string

const (
	WorkTool       WorkKind = "tool"
	WorkBackground WorkKind = "background"
	WorkDelegate   WorkKind = "delegate"
	WorkCompaction WorkKind = "compaction"
	WorkWait       WorkKind = "wait"
	WorkParallel   WorkKind = "parallel"
	WorkCommand    WorkKind = "command"
)

type WorkPhase string

const (
	WorkStart  WorkPhase = "start"
	WorkFinish WorkPhase = "finish"
	// WorkResult describes the logical result returned by tool dispatch. Unlike
	// WorkFinish it can precede actual worker completion after a timeout.
	WorkResult WorkPhase = "result"
)

// WorkEvent carries exclusive lifecycle measurements. Optional durations are nil
// when unknown. Mode/Outcome/Tool/Trigger must be bounded configured categories;
// no arguments, IDs, free-form termination text, or inclusive model Usage belongs
// here. Counts are counts of the operation described by Kind, not model calls.
type WorkEvent struct {
	Identity                                         Identity
	Kind                                             WorkKind
	Phase                                            WorkPhase
	Mode, Outcome, Tool, Trigger                     string
	QueueDuration, RunDuration, DeliveryDuration     *time.Duration
	Count, Active, Completed, Failed, Cancelled      int
	ContextBefore, ContextAfter                      int
	BatchSize, Turns, Compactions                    int
	ErrorKind, Activity, Termination, FallbackReason string
	ResultBytes, OriginalBytes                       int
	Truncated                                        bool
	// Metrics are diagnostics-only numeric process measurements. Observers must
	// allowlist known keys; never turn arbitrary map keys into metric names.
	Metrics map[string]int
}

// PromptEvent is a wall-clock summary, NOT an inclusive token/cost billing event.
// Termination is a bounded caller category, not an error message.
type PromptEvent struct {
	Identity       Identity
	Duration       time.Duration
	Turns          int
	Termination    string
	ClosureTrigger string
}

// ContextEvent describes context utilization/retention without retaining content.
// Reason and Policy are configured categories, never filenames or message text.
type ContextEvent struct {
	Composition                                                         *ContextComposition
	Identity                                                            Identity
	Reason, Policy                                                      string
	Before, After, Limit, Retained, Dropped                             int
	BytesBefore, BytesAfter, BytesRemoved, TokensRemoved, BlocksTrimmed int
	DecisionSource, PreviousRequestMode, NextRequestMode                string
	ResponseStateReset, MeasurementAnchorReset, ContinuationStateReset  bool
}
type scopeKey struct{}

func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}
func FromContext(ctx context.Context) Scope { s, _ := ctx.Value(scopeKey{}).(Scope); return s }

// Rebind copies a scope while preserving its observer (e.g. for delegate roles).
func (s Scope) Rebind(identity Identity) Scope { s.Identity = identity; return s }
func (s Scope) Work(e WorkEvent) {
	if s.Observer != nil {
		e.Identity = s.Identity
		s.Observer.ObserveWork(e)
	}
}
func (s Scope) Prompt(e PromptEvent) {
	if s.Observer != nil {
		e.Identity = s.Identity
		s.Observer.ObservePrompt(e)
	}
}

// Discard records a disjoint subset of already billed usage. Callers must not
// report cumulative maintenance totals repeatedly or overlap physical failures
// with subsequent logical discards.
func (s Scope) Discard(usage llm.Usage, purpose llm.RequestPurpose, reason string) {
	if s.Observer != nil && llm.HasUsageDelta(usage) {
		s.Observer.ObserveModel(ModelEvent{Identity: s.Identity, Phase: ModelDiscard,
			Attempt: llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Purpose: llm.NormalizeRequestPurpose(purpose), Provider: s.Identity.Provider, Model: s.Identity.Model}, ErrorClass: llm.AttemptErrorClass(reason)}, Usage: usage})
	}
}

func (s Scope) Context(e ContextEvent) {
	if s.Observer != nil {
		e.Identity = s.Identity
		s.Observer.ObserveContext(e)
	}
}
