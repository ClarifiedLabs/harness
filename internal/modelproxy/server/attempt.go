package server

import (
	"context"

	"harness/internal/llm"
	"harness/internal/modelproxy/pricing"
	"harness/internal/modelproxy/protocol"
)

func withTargetAttemptMetadata(ctx context.Context, target resolvedTarget, api string, purpose llm.RequestPurpose, caller *protocol.CallerAttempt) context.Context {
	meta := llm.AttemptMetadata{
		Scope: llm.AttemptScopeUpstream, Provider: target.pc.Name, API: api,
		Model: target.entry.Name, Purpose: purpose,
	}
	if caller != nil {
		origin := caller.Normalized()
		meta.Cause, meta.RetryLayer = origin.Cause, origin.RetryLayer
	}
	return llm.WithAttemptMetadata(ctx, meta)
}

// priceAttempt prices an entire cumulative physical snapshot, never a delta or
// the aggregate stream usage. Keep authoritative provider cost (including zero)
// and known partial spend when no complete cost was reported. Pricing metadata
// omitted from Usage's wire representation is carried explicitly by the fact.
func (h *Handler) priceAttempt(target resolvedTarget, request llm.Request, event llm.AttemptEvent) llm.AttemptEvent {
	// Normalization clears non-billing wait/disposition payloads before any
	// pricing. Dispositions reference retained physical usage at the consumer.
	event = llm.NormalizeAttemptEvent(event)
	if event.Usage == nil {
		return event
	}
	usage := *event.Usage
	usage.CacheWriteTTLKnown = usage.CacheWriteTTLKnown || event.CacheWriteTTLKnown
	if !usage.CostKnown && usage.CostUSD == 0 {
		if snapshot := h.snapshot.Load(); snapshot != nil && snapshot.pricer != nil {
			result := snapshot.pricer.PriceUsage(pricing.Input{
				TargetID: target.targetID, Provider: target.pc, Model: target.entry,
				Request: request, Usage: usage,
			})
			if result.Known {
				usage.CostUSD, usage.CostKnown = result.CostUSD, true
			}
		}
	}
	event.Usage = &usage
	event.CacheWriteTTLKnown = usage.CacheWriteTTLKnown
	return event
}
