package execution

import (
	"errors"
	"testing"

	"harness/internal/llm"
)

func TestSyntheticOnlyFallbackNeverResurrectsUsage(t *testing.T) {
	r := &recorder{}
	_, call := (Scope{Observer: r}).ModelCall(t.Context(), llm.RequestPurposeTurn)
	unreported := false
	placeholder := llm.Usage{InputTokens: 7, CostUSD: .2, CostKnown: true}
	call.ObserveStream(llm.StreamEvent{Usage: &placeholder, UsageReported: &unreported})
	call.Finish(placeholder, nil)
	call.Discard("error")
	for _, event := range r.models {
		if event.Phase == ModelUsageDelta || event.Phase == ModelDiscard || event.UsageReported {
			t.Fatalf("synthetic usage was resurrected: %+v", event)
		}
	}
	if len(r.models) != 2 {
		t.Fatalf("events = %+v", r.models)
	}
}

func TestFallbackRetainSealsIndependentResponseSegments(t *testing.T) {
	r := &recorder{}
	_, call := (Scope{Observer: r}).ModelCall(t.Context(), llm.RequestPurposeTurn)
	prefix := llm.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: .4, CostKnown: true}
	call.ObserveStream(llm.StreamEvent{Usage: &prefix})
	call.Retain()
	call.Retain()
	// Replayed terminal placeholders are not new provider usage.
	unreported := false
	call.ObserveStream(llm.StreamEvent{Usage: &prefix, UsageReported: &unreported})
	provisional := llm.Usage{InputTokens: 7, OutputTokens: 10}
	final := llm.Usage{InputTokens: 7, OutputTokens: 4, ReasoningTokens: 6, CostUSD: .1}
	call.ObserveStream(llm.StreamEvent{Usage: &provisional})
	call.ObserveStream(llm.StreamEvent{Usage: &final})
	call.Finish(llm.Usage{InputTokens: 107, OutputTokens: 20, ReasoningTokens: 6}, errors.New("terminal failure"))
	call.Discard("error")
	call.Discard("error")
	var billing, discarded llm.Usage
	var records, disposals int
	for _, event := range r.models {
		if event.Phase == ModelUsageDelta {
			billing = event.Usage
			records++
		}
		if event.Phase == ModelDiscard {
			discarded = event.Usage
			disposals++
		}
	}
	if records != 1 || billing.InputTokens != 107 || billing.OutputTokens != 14 || billing.ReasoningTokens != 6 || billing.CostUSD != .5 || billing.CostKnown {
		t.Fatalf("billing = %+v records=%d", billing, records)
	}
	if disposals != 1 || discarded != final {
		t.Fatalf("discarded = %+v disposals=%d", discarded, disposals)
	}
}

func TestFallbackRetainedPrefixWithoutLaterUsageBillsOnce(t *testing.T) {
	r := &recorder{}
	_, call := (Scope{Observer: r}).ModelCall(t.Context(), llm.RequestPurposeTurn)
	prefix := llm.Usage{InputTokens: 100, OutputTokens: 10, CostKnown: true}
	call.ObserveStream(llm.StreamEvent{Usage: &prefix})
	call.Retain()
	call.Finish(prefix, errors.New("next response never reported usage"))
	call.Discard("error")
	var records int
	for _, event := range r.models {
		if event.Phase == ModelDiscard {
			t.Fatalf("retained prefix discarded: %+v", event)
		}
		if event.Phase == ModelUsageDelta {
			records++
			if event.Usage != prefix {
				t.Fatalf("prefix billed twice: %+v", event)
			}
		}
	}
	if records != 1 {
		t.Fatalf("records=%d", records)
	}
}

func TestPhysicalDiscardUsesOriginalSnapshotsAndRetainedLineage(t *testing.T) {
	r := &recorder{}
	ctx, call := (Scope{Observer: r}).ModelCall(t.Context(), llm.RequestPurposeTurn)
	prefix := llm.StartAttempt(ctx)
	prefix.Usage(llm.Usage{InputTokens: 100, CostKnown: true})
	call.Retain() // The logical boundary can precede the source final fact.
	prefix.Finish(llm.AttemptSucceeded, nil)
	actualCtx := llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Provider: "actual", Model: "served"})
	failed := llm.StartAttempt(actualCtx)
	failed.Usage(llm.Usage{OutputTokens: 10})
	failed.Usage(llm.Usage{OutputTokens: 4, ReasoningTokens: 6, CostUSD: .2, CostKnown: true})
	failed.Finish(llm.AttemptFailed, errors.New("source-only usage"))
	call.Finish(llm.Usage{InputTokens: 999, OutputTokens: 999}, errors.New("logical failure"))
	call.Discard("error")
	call.Discard("stream_retry")
	var records int
	for _, event := range r.models {
		if event.Phase != ModelDiscard {
			continue
		}
		records++
		if event.Usage.InputTokens != 0 || event.Usage.OutputTokens != 4 || event.Usage.ReasoningTokens != 6 || event.Usage.CostUSD != .2 || event.Attempt.Model != "served" || event.Attempt.Provider != "actual" {
			t.Fatalf("discard = %+v", event)
		}
	}
	if records != 1 {
		t.Fatalf("records=%d", records)
	}
}
