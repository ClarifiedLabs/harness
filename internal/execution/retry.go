package execution

import (
	"context"
	"time"

	"harness/internal/llm"
)

// RetryWait observes a caller-owned wait between provider invocations. It does
// not reopen a finished ModelCall, allocate an attempt, or bill any usage. The
// callback is invoked exactly once and its error is returned unchanged.
func (s Scope) RetryWait(ctx context.Context, planned time.Duration, layer llm.RetryLayer, reason llm.AttemptErrorClass, wait func() error) error {
	if s.Observer == nil {
		return wait()
	}
	ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{
		Scope: llm.AttemptScopeProviderCall, Provider: s.Identity.Provider, Model: s.Identity.Model,
	})
	ctx = llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(event llm.AttemptEvent) {
		if event.Phase == llm.AttemptRetryWait {
			s.Observer.ObserveModel(ModelEvent{Identity: s.Identity, Phase: ModelRetry, Attempt: event})
		}
	}))
	return llm.ObserveRetryWait(ctx, planned, layer, reason, wait)
}
