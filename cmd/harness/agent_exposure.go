package main

import (
	"context"
	"sync"
	"sync/atomic"

	"harness/internal/agentdef"
	"harness/internal/delegate"
	"harness/internal/tools"
)

// agentExposure serializes discovery mutations with background delegate reads.
// Candidate snapshots are separate: building tool schemas under the lock may
// query candidates recursively, so that callback must never acquire the lock.
type agentExposure struct {
	mu       sync.RWMutex
	agents   map[string]agentdef.Definition
	snapshot atomic.Value
}

func newAgentExposure(agents map[string]agentdef.Definition) *agentExposure {
	e := &agentExposure{agents: agents}
	e.publish()
	return e
}

// publish runs during startup or while holding the write lock.
func (e *agentExposure) publish() { e.snapshot.Store(delegateAgentCandidates(e.agents)) }
func (e *agentExposure) candidates() []delegate.AgentCandidate {
	return e.snapshot.Load().([]delegate.AgentCandidate)
}

func (e *agentExposure) resolve(resolve func(delegate.Runtime, string) (delegate.Launch, error)) func(delegate.Runtime, string) (delegate.Launch, error) {
	return func(runtime delegate.Runtime, name string) (delegate.Launch, error) {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return resolve(runtime, name)
	}
}

func (e *agentExposure) refresh(refresh func(context.Context, string) (*tools.Registry, string)) func(context.Context, string) (*tools.Registry, string) {
	return func(ctx context.Context, name string) (*tools.Registry, string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		registry, notice := refresh(ctx, name)
		e.publish()
		return registry, notice
	}
}
