package agent

import (
	"context"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/tools"
)

// EventKind discriminates the Event union.
type EventKind int

const (
	EventTextDelta              EventKind = iota // incremental assistant text (Text)
	EventReasoningSummary                        // display-ready reasoning summary (Text)
	EventAssistantPhase                          // provider phase metadata, sent before the text it describes (Text)
	EventNotice                                  // out-of-band notice such as max-turns or cancelled (Text)
	EventTurnAttemptStart                        // a provider request is about to start (Turn, Attempt, Context)
	EventTurnAttemptComplete                     // a provider request finished (AttemptUsage)
	EventTurnAttemptAbandoned                    // a finished attempt was discarded for a retry or rebuild (Turn, Attempt)
	EventToolUseStart                            // the model began emitting a tool call (Call)
	EventToolUseDelta                            // partial tool-call arguments, rendering only (Index, Text)
	EventToolStart                               // a tool call is about to run (Call)
	EventToolResult                              // a tool call finished (Result)
	EventToolDiff                                // user-facing file diff after a mutating tool; never transcript content (Call, Path, Text)
	EventToolMutation                            // host-derived paths changed by a successful tool; diagnostics only (Call, Paths)
	EventTurnComplete                            // end of one conversational turn (TurnUsage)
	EventPromptComplete                          // end of the prompt (PromptUsage)
	EventPromptCheckpoint                        // resumable boundary; the loop stays paused while it is handled (Checkpoint)
	EventSteerDelivered                          // queued steer input was appended to a validated transcript (Steer)
	EventModelRequest                            // diagnostics-only request lifecycle telemetry (ModelRequest)
	EventModelError                              // safe record of a final model failure with a compatibility classification (ModelError)
	EventHookDiagnostic                          // bounded hook execution diagnostic (HookDiagnostic)
	EventEvaluatorResult                         // bounded semantic outcome from a Stop hook; diagnostics only (Evaluator)
	EventHookContext                             // hook-generated context to keep for later turns, outside the transcript (HookContext)
	EventCompactionStart                         // compaction began; always balanced by EventCompactionComplete
	EventCompactionComplete                      // compaction ended, including local degradation
	EventRetention                               // one transcript-retention epoch; diagnostics only (Retention)
	EventTranscriptRewritten                     // the transcript was rewritten; request-only state must be re-armed
	EventPromptWorkWaitStart                     // the prompt began joining background work; always balanced by EventPromptWorkWaitComplete
	EventPromptWorkWaitComplete                  // the join ended, including cancellation and errors
	EventClosure                                 // first prompt wind-down trigger; diagnostics only (Closure)
	EventTurnProgress                            // semantic activity after a completed tool turn; diagnostics only (TurnProgress)
	EventMaintenance                             // a model call that supported the prompt without creating a turn (Maintenance)
)

// Event is one observable occurrence in a prompt run. Which fields are
// populated depends on Kind; the constants above name them. Events report what
// happened and are never added to the transcript or a model request. Struct
// payloads are pointers owned by the event: a handler that changes one must
// copy it first.
type Event struct {
	Kind EventKind

	Text        string
	Turn        int
	Attempt     int
	Index       int
	Path        string
	Paths       []string
	HookContext []string
	Call        llm.ToolCall

	Result         *llm.ToolResult
	Context        *ContextEstimate
	AttemptUsage   *TurnAttemptUsage
	TurnUsage      *TurnUsage
	PromptUsage    *PromptUsage
	Checkpoint     *PromptCheckpoint
	Steer          *SteerInput
	ModelRequest   *llm.ModelRequestEvent
	ModelError     *ModelErrorDiagnostic
	HookDiagnostic *hooks.Diagnostic
	Evaluator      *hooks.EvaluatorResult
	Retention      *RetentionEvent
	Closure        *ClosureEvent
	TurnProgress   *TurnProgress
	Maintenance    *MaintenanceUsage
}

// EventSink receives every event of a prompt run, synchronously and in order.
// The agent loop owns the transcript and the control flow; the sink only
// reports, and ignores the kinds it has no use for (design §8.1, §10).
type EventSink interface {
	Emit(Event)
}

// PromptWorkCoordinator owns background work whose results must be
// incorporated before the current parent prompt may finish. Usage is drained
// exactly once into the parent prompt; completion context is delivered
// separately through Host.RequestContext.
type PromptWorkCoordinator interface {
	PendingPromptWork() bool
	WaitForPromptWork(context.Context) (llm.Usage, error)
	DrainPromptWorkUsage() llm.Usage
}

// Host is what a prompt run may ask of its caller, as opposed to the events it
// reports. Every field is optional; a nil field means the service is
// unavailable and the agent skips the behavior that depends on it.
type Host struct {
	// Archiver durably stores the full text of a truncated or trimmed tool
	// result. Without it, retention never drops bytes the model cannot
	// re-derive.
	Archiver ToolResultArchiver
	// RequestContext returns fresh request-only context before a
	// conversational model round. It may consume one-shot signals as it
	// attaches them. Transport and compatibility retries reuse attached
	// context, while a retry after a transcript rewrite may refresh and merge
	// it.
	RequestContext func() []string
	// PeekRequestContext returns what the next request would carry without
	// consuming anything, for local sizing estimates. Without it, sizing omits
	// dynamic context rather than consuming a one-shot signal.
	PeekRequestContext func() []string
	// WorkflowStatus reports optional orchestrator-supplied task state.
	// Detailed model-visible requirements continue to use RequestContext.
	WorkflowStatus func() WorkflowStatus
	// TryStagnationNudge owns the replayable trajectory decision. It returns
	// true only after a lane-scoped trigger has been durably recorded.
	TryStagnationNudge func(threshold int) bool
	// PromptWork coordinates join-required background work.
	PromptWork PromptWorkCoordinator
	// ToolProgress attaches a tool's bounded live-progress source to an
	// outstanding call before it runs, and clears it with nil once the call
	// returns. Without it, no tool is asked to start progress reporting.
	ToolProgress func(call llm.ToolCall, progress tools.BackgroundProgress)
}

// HostProvider is implemented by sinks that also supply Host services. A sink
// without it runs with the zero Host.
type HostProvider interface {
	Host() Host
}

func hostOf(sink EventSink) Host {
	if provider, ok := sink.(HostProvider); ok {
		return provider.Host()
	}
	return Host{}
}

func emit(sink EventSink, event Event) {
	if sink != nil {
		sink.Emit(event)
	}
}

func emitNotice(sink EventSink, text string) {
	emit(sink, Event{Kind: EventNotice, Text: text})
}
