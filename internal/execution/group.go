package execution

import (
	"context"
	"sync"
)

// Group tracks actual execution and owner completion, independently of logical
// timeout results. Roots stop accepting new user work before Wait. A tracked
// parent registers detached children before returning, so a zero count is an
// ownership handoff, not merely a returned tool result. It does not cancel work.
type Group struct {
	mu     sync.Mutex
	active int
	idle   chan struct{}
}

// Begin registers work before its goroutine is started. The completion function
// is idempotent and must run after the final observation and owner mutation.
func (g *Group) Begin() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	if g.active == 0 {
		g.idle = make(chan struct{})
	}
	g.active++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			if g.active == 0 {
				close(g.idle)
			}
			g.mu.Unlock()
		})
	}
}

// Wait waits for registered owners and workers, respecting the caller's budget.
// No mutable owner state may be read when it returns an error. A caller must
// close admission before waiting; Group deliberately does not reject child work.
func (g *Group) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	for {
		g.mu.Lock()
		if g.active == 0 {
			g.mu.Unlock()
			return nil
		}
		idle := g.idle
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle:
		}
	}
}

// Track registers work in the root's group, if lifecycle tracking is enabled.
func (s Scope) Track() func() { return s.Group.Begin() }
