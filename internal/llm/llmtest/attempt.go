package llmtest

import (
	"context"
	"harness/internal/llm"
	"sync"
)

// AttemptRecorder collects independent source facts, not diagnostics.
type AttemptRecorder struct {
	mu     sync.Mutex
	events []llm.AttemptEvent
}

func (r *AttemptRecorder) ObserveAttempt(e llm.AttemptEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
func (r *AttemptRecorder) Context(ctx context.Context) context.Context {
	return llm.WithAttemptObserver(ctx, r)
}
func (r *AttemptRecorder) Events() []llm.AttemptEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.AttemptEvent(nil), r.events...)
}
func (r *AttemptRecorder) Finished() []llm.AttemptEvent {
	var out []llm.AttemptEvent
	for _, e := range r.Events() {
		if e.Phase == llm.AttemptFinished {
			out = append(out, e)
		}
	}
	return out
}
