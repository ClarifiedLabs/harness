package otel

import (
	"context"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestObserverModelUsageReportingCompleteness(t *testing.T) {
	tests := []struct {
		name       string
		usage      *llm.Usage
		lostFinish bool
		pricing    string
	}{
		{name: "unreported", pricing: "unreported"},
		{name: "reported_zero_unpriced", usage: &llm.Usage{}, pricing: "unpriced"},
		{name: "authoritative_zero", usage: &llm.Usage{CostKnown: true}, pricing: "known"},
		{name: "partial_price", usage: &llm.Usage{InputTokens: 3, CostUSD: .1}, pricing: "unpriced"},
		{name: "fully_priced", usage: &llm.Usage{InputTokens: 3, CostUSD: .2, CostKnown: true}, pricing: "known"},
		{name: "incomplete_unreported", lostFinish: true, pricing: "unreported"},
		{name: "incomplete_reported_zero", usage: &llm.Usage{}, lostFinish: true, pricing: "unpriced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, e := observerSink(t)
			ctx, call := s.Scope().ModelCall(t.Context(), llm.RequestPurposeTurn)
			source := llm.StartAttempt(ctx)
			if tt.usage != nil {
				source.Usage(*tt.usage)
				source.Usage(*tt.usage)
			}
			if !tt.lostFinish {
				source.Finish(llm.AttemptSucceeded, nil)
				source.Finish(llm.AttemptSucceeded, nil)
			}
			// Inclusive aggregate usage must never fill a missing physical record or
			// bill a second record when even an all-zero source snapshot was observed.
			call.Finish(llm.Usage{InputTokens: 999, CostKnown: true, CostUSD: 999}, nil)
			call.Finish(llm.Usage{}, nil)
			requireNumber(t, e, "harness.model.requests", 1, nil)
			requireNumber(t, e, "harness.model.request.outcomes", 1, nil)
			requireNumber(t, e, "harness.model.inflight", 0, nil)
			reported := 0.0
			unpriced := 0.0
			tokens := 0.0
			cost := 0.0
			if tt.usage != nil {
				reported = 1
				tokens = float64(tt.usage.InputTokens)
				cost = tt.usage.CostUSD
			}
			if tt.pricing == "unpriced" {
				unpriced = 1
			}
			requireNumber(t, e, "harness.model.usage.records", reported, map[string]string{"pricing": "reported"})
			for _, pricing := range []string{"known", "unpriced", "unreported"} {
				want := 0.0
				if pricing == tt.pricing {
					want = 1
				}
				requireNumber(t, e, "harness.model.usage.records", want, map[string]string{"pricing": pricing})
			}
			requireNumber(t, e, "harness.cost.unpriced_calls", unpriced, nil)
			requireNumber(t, e, "harness.tokens.total", tokens, nil)
			requireNumber(t, e, "harness.cost.usd", cost, nil)
		})
	}
}

func TestObserverFallbackZeroUsageIsReported(t *testing.T) {
	s, e := observerSink(t)
	_, call := s.Scope().ModelCall(t.Context(), llm.RequestPurposeTurn)
	u := llm.Usage{}
	call.ObserveStream(llm.StreamEvent{Usage: &u})
	call.Finish(llm.Usage{}, nil)
	requireNumber(t, e, "harness.model.usage.records", 1, map[string]string{"pricing": "reported", "scope": "provider_call"})
	requireNumber(t, e, "harness.model.usage.records", 1, map[string]string{"pricing": "unpriced"})
	requireNumber(t, e, "harness.model.usage.records", 0, map[string]string{"pricing": "unreported"})
}

func TestObserverSourceDiscardKeepsActualUsageAndDoesNotRebill(t *testing.T) {
	for _, reason := range []llm.AttemptDiscardReason{llm.AttemptDiscardCompatibility, llm.AttemptDiscardProxyRetry, llm.AttemptDiscardUnknown} {
		t.Run(string(reason), func(t *testing.T) {
			s, e := observerSink(t)
			ctx, call := s.Scope().ModelCall(t.Context(), llm.RequestPurposeTurn)
			actual := llm.AttemptMetadata{Provider: "actual-provider", Model: "actual-model", Scope: llm.AttemptScopeUpstream, Purpose: llm.RequestPurposeTurn}
			llm.EmitAttempt(ctx, llm.AttemptEvent{AttemptMetadata: actual, Sequence: 1, Phase: llm.AttemptStarted})
			u := llm.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: .3}
			llm.EmitAttempt(ctx, llm.AttemptEvent{AttemptMetadata: actual, Sequence: 1, Phase: llm.AttemptFinished, Outcome: llm.AttemptFailed, Usage: &u})
			requireNumber(t, e, "harness.model.discard.records", 0, nil) // Failure alone is not a disposition.
			discard := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Model: "not-the-physical-model"}, Sequence: 1, Phase: llm.AttemptDiscarded, DiscardReason: reason, Usage: &llm.Usage{InputTokens: 999, CostUSD: 999}}
			llm.EmitAttempt(ctx, discard)
			llm.EmitAttempt(ctx, discard)
			call.Finish(llm.Usage{InputTokens: 999, CostUSD: 999}, nil)
			attrs := map[string]string{"model": "actual-model", "provider": "actual-provider", "reason": "source_" + string(reason)}
			requireNumber(t, e, "harness.model.discard.records", 1, attrs)
			requireNumber(t, e, "harness.model.discard.tokens", 12, attrs)
			requireNumber(t, e, "harness.model.discard.cost", .3, attrs)
			requireNumber(t, e, "harness.model.requests", 1, nil)
			requireNumber(t, e, "harness.model.request.outcomes", 1, nil)
			requireNumber(t, e, "harness.tokens.total", 12, nil)
			requireNumber(t, e, "harness.cost.usd", .3, nil)
			requireNumber(t, e, "harness.model.usage.records", 1, map[string]string{"pricing": "reported"})
			requireNumber(t, e, "harness.model.usage.records", 0, map[string]string{"pricing": "unreported"})
		})
	}
}

func TestObserverRealScopeAndSequenceZeroRetryWait(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	calls := 0
	err := scope.RetryWait(t.Context(), 0, llm.RetryLayerAgent, llm.AttemptErrorRateLimit, func() error { calls++; return context.Canceled })
	if err != context.Canceled || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	requireNumber(t, e, "harness.model.requests", 0, nil)
	requireNumber(t, e, "harness.retries.total", 0, nil)
	requireNumber(t, e, "harness.model.usage.records", 0, nil)
	requireHistogram(t, e, "harness.model.retry.backoff", 1, 0, map[string]string{"layer": "agent", "reason": "rate_limit", "outcome": "cancelled", "measurement": "planned"})
	n, _ := observerHistogram(t, e, "harness.model.retry.backoff", map[string]string{"measurement": "actual"})
	if n != 1 {
		t.Fatalf("actual waits=%d", n)
	}
	ctx, call := scope.ModelCall(t.Context(), llm.RequestPurposeCompaction)
	if err := llm.ObserveRetryWait(ctx, time.Second, llm.RetryLayerProvider, llm.AttemptErrorRequest, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	requireNumber(t, e, "harness.model.requests", 0, nil)
	requireHistogram(t, e, "harness.model.retry.backoff", 1, 1, map[string]string{"purpose": "compaction", "measurement": "planned", "layer": "provider"})
	// Finish the enclosing legacy invocation independently of its retry wait.
	call.Finish(llm.Usage{}, nil)
	requireNumber(t, e, "harness.model.requests", 1, nil)
	requireNumber(t, e, "harness.model.usage.records", 1, map[string]string{"pricing": "unreported"})
	requireNumber(t, e, "harness.retries.total", 0, nil)
}
