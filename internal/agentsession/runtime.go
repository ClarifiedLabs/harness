// Package agentsession manages reusable logical agent sessions whose individual
// prompt operations are immutable background jobs.
package agentsession

import (
	"context"

	"harness/internal/tools"
)

// Prompt identifies one finite operation within a reusable runtime.
type Prompt struct {
	Text       string
	SessionID  string
	Generation uint64
	Operation  int
	JobID      string
}

// Event is bounded process-local progress or diagnostic state emitted by a
// runtime. It never becomes a parent transcript message.
type Event struct {
	Progress   tools.BackgroundProgressSnapshot
	Diagnostic string
}

// EventSink accepts runtime events for the operation that created it.
type EventSink func(Event)

// Publish emits an event when the sink is configured.
func (s EventSink) Publish(event Event) {
	if s != nil {
		s(event)
	}
}

// Outcome is one backend-reported logical turn completion. Reusable must be set
// explicitly; false is the safe default after errors or uncertain cancellation.
type Outcome struct {
	Result     tools.BackgroundJobResult
	StopReason string
	Reusable   bool
}

// Runtime is one long-lived backend conversation. Prompt is finite; Close owns
// persistent resource cleanup and must not rely on a prompt context remaining
// alive. Close must honor its context; the manager bounds non-cooperative
// implementations but cannot reclaim goroutines blocked inside their Close.
type Runtime interface {
	Prompt(context.Context, Prompt, EventSink) (Outcome, error)
	Close(context.Context) error
}

// CleanupObservable is implemented by runtimes whose Close may return before
// owned resource cleanup finishes. CleanupDone must be stable and close under
// the runtime's own finite teardown bounds after Close or unexpected death.
// CloseAll joins it even when its caller's context is canceled; ordinary Close
// remains bounded by the caller/manager timeout.
type CleanupObservable interface {
	CleanupDone() <-chan struct{}
}

// Factory opens a runtime inside the first operation's background job. It must
// honor cancellation under finite bounds and release any resources before
// returning an error. CloseAll cancels and joins in-flight factories before
// joining runtime cleanup.
type Factory func(context.Context, SessionInfo) (Runtime, error)

// Steerable is implemented by runtimes that accept live input during a prompt.
type Steerable interface {
	Steer(context.Context, string) error
}

// Observable is implemented by runtimes whose unexpected death can be observed
// while the logical session is idle.
type Observable interface {
	Done() <-chan struct{}
	Err() error
}

// SessionInfo is immutable construction metadata supplied to Factory.
type SessionInfo struct {
	ID         string
	Kind       string
	Label      string
	Generation uint64
}
