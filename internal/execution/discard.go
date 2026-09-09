package execution

import "harness/internal/llm"

// Retain marks the physical lineage observed so far as accepted by the caller.
// Native steering can retain completed response prefixes before the enclosing
// Provider.Stream returns. A source whose final fact follows the logical Done
// event is retained as well. Later source sequences remain discardable.
func (c *ModelCall) Retain() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.attempts {
		a.retained = true
	}
	// Legacy native boundaries carry independent per-response snapshots, not
	// monotonically growing invocation totals. Seal the accepted segment before
	// the next response can replace it (or reclassify its own usage buckets).
	if c.fallbackReported {
		if c.retainedReported {
			c.retainedFallback = addReportedUsage(c.retainedFallback, c.fallback)
		} else {
			c.retainedFallback = c.fallback
		}
		c.retainedReported = true
		c.fallback = llm.Usage{}
		c.fallbackReported = false
	}
}

// Discard records a caller-rejected subset of previously committed physical
// usage, with its original pricing and execution identity. Call after Finish.
// Source-only failure usage is included even if no logical Usage event escaped
// the decoder. Hidden source dispositions and retained native prefixes are
// excluded, and repeated calls cannot discard the same physical slice twice.
// This never adds to exclusive billing or modifies legacy prompt accounting.
func (c *ModelCall) Discard(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		return
	}
	if len(c.attempts) > 0 {
		for _, a := range c.attempts {
			if !a.finished || a.retained || a.discarded {
				continue
			}
			a.discarded = true
			if a.reported && llm.HasUsageDelta(a.usage) {
				fact := a.fact
				fact.ErrorClass = llm.AttemptErrorClass(reason)
				c.emit(ModelDiscard, fact, a.usage)
			}
		}
		return
	}
	if c.fallbackDiscarded || !c.fallbackReported {
		return
	}
	c.fallbackDiscarded = true
	usage := c.fallback
	if !llm.HasUsageDelta(usage) {
		return
	}
	fact := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{
		Scope: llm.AttemptScopeProviderCall, Purpose: c.purpose,
		Provider: c.scope.Identity.Provider, Model: c.scope.Identity.Model,
	}, ErrorClass: llm.AttemptErrorClass(reason)}
	c.emit(ModelDiscard, fact, usage)
}

// addReportedUsage combines disjoint, complete fallback response snapshots.
// Unlike general usage totals, an explicitly reported unpriced zero response
// makes the cost incomplete. Callers keep missing snapshots out of this sum.
func addReportedUsage(a, b llm.Usage) llm.Usage {
	out := llm.AddUsage(a, b)
	out.CostKnown = a.CostKnown && b.CostKnown
	out.CacheWriteTTLKnown = a.CacheWriteTTLKnown || b.CacheWriteTTLKnown
	return out
}
