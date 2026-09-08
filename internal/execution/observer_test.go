package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestObserverRetryWaitDoesNotAllocateOrBillAttempt(t *testing.T) {
	r := &recorder{}
	s := Scope{Observer: r, Identity: Identity{Provider: "provider", Model: "model", Agent: "child", Delegate: "true"}}
	ctx, call := s.ModelCall(t.Context(), llm.RequestPurposeCompaction)
	first := llm.StartAttempt(ctx)
	first.Finish(llm.AttemptFailed, errors.New("failed"))
	calls := 0
	wantErr := errors.New("wait failed")
	if err := llm.ObserveRetryWait(ctx, time.Second, llm.RetryLayerConnect, llm.AttemptErrorRateLimit, func() error {
		calls++
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("wait error = %v", err)
	}
	second := llm.StartAttempt(ctx)
	second.Finish(llm.AttemptSucceeded, nil)
	call.Finish(llm.Usage{}, nil)
	var starts, finishes, waits int
	for _, event := range r.models {
		switch event.Phase {
		case ModelStart:
			starts++
			if event.Attempt.Sequence != uint64(starts) {
				t.Fatalf("wait allocated sequence: %+v", event)
			}
		case ModelFinish:
			finishes++
		case ModelUsageDelta:
			t.Fatal("retry wait manufactured usage")
		case ModelRetry:
			waits++
			if event.Attempt.Sequence != 0 || event.Attempt.RetryDelay == nil || *event.Attempt.RetryDelay != time.Second || event.Attempt.Duration == nil || event.Attempt.Purpose != llm.RequestPurposeCompaction || event.Identity != s.Identity {
				t.Fatalf("wait = %+v", event)
			}
		}
	}
	if calls != 1 || starts != 2 || finishes != 2 || waits != 1 {
		t.Fatalf("callback/start/finish/wait = %d/%d/%d/%d", calls, starts, finishes, waits)
	}
}

func TestScopeInterCallRetryWaitPreservesIdentityPurposeAndError(t *testing.T) {
	r := &recorder{}
	s := Scope{Observer: r, Identity: Identity{Provider: "old", Model: "old-model"}}
	ctx := llm.WithAttemptMetadata(t.Context(), llm.AttemptMetadata{Purpose: llm.RequestPurposeBranchSummary})
	want := context.Canceled
	calls := 0
	if err := s.RetryWait(ctx, 0, llm.RetryLayerAgent, llm.AttemptErrorRequest, func() error { calls++; return want }); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	_ = s.Rebind(Identity{Model: "new"})
	if calls != 1 || len(r.models) != 1 {
		t.Fatalf("calls/events = %d/%d", calls, len(r.models))
	}
	event := r.models[0]
	if event.Phase != ModelRetry || event.Identity != s.Identity || event.Attempt.Purpose != llm.RequestPurposeBranchSummary || event.Attempt.Outcome != llm.AttemptCancelled || event.Attempt.RetryDelay == nil || *event.Attempt.RetryDelay != 0 {
		t.Fatalf("event = %+v", event)
	}
	if err := (Scope{}).RetryWait(ctx, 0, llm.RetryLayerAgent, llm.AttemptErrorRequest, func() error { calls++; return want }); !errors.Is(err, want) || calls != 2 {
		t.Fatalf("disabled observer changed callback: calls=%d err=%v", calls, err)
	}
}

func TestModelUsageReportingDistinguishesZeroFromMissing(t *testing.T) {
	for _, physical := range []bool{false, true} {
		for _, reported := range []bool{false, true} {
			r := &recorder{}
			ctx, call := (Scope{Observer: r}).ModelCall(t.Context(), llm.RequestPurposeTurn)
			if physical {
				a := llm.StartAttempt(ctx)
				if reported {
					a.Usage(llm.Usage{})
				}
				a.Finish(llm.AttemptSucceeded, nil)
			} else if reported {
				call.ObserveStream(llm.StreamEvent{Kind: llm.EventUsage, Usage: &llm.Usage{}})
			}
			call.Finish(llm.Usage{}, nil)
			var records int
			for _, e := range r.models {
				if e.Phase == ModelUsageDelta {
					records++
				}
				if e.Phase == ModelFinish && e.UsageReported != reported {
					t.Fatalf("physical=%v reported=%v finish=%+v", physical, reported, e)
				}
			}
			want := 0
			if reported {
				want = 1
			}
			if records != want {
				t.Fatalf("physical=%v reported=%v records=%d", physical, reported, records)
			}
		}
	}
}

func TestSourceDispositionIsDeduplicatedAndNeverRebills(t *testing.T) {
	r := &recorder{}
	ctx, call := (Scope{Observer: r, Identity: Identity{Provider: "configured"}}).ModelCall(t.Context(), llm.RequestPurposeTurn)
	tracked, tracker := llm.TrackAttempts(ctx)
	tracked, nested := llm.TrackAttempts(tracked)
	tracked = llm.WithAttemptMetadata(tracked, llm.AttemptMetadata{Provider: "actual", Model: "actual-model"})
	a := llm.StartAttempt(tracked)
	a.Usage(llm.Usage{InputTokens: 7, CostUSD: .1})
	a.Finish(llm.AttemptFailed, errors.New("hidden compatibility failure"))
	nested.Discard(llm.AttemptDiscardCompatibility)
	tracker.Discard(llm.AttemptDiscardProxyRetry)
	// Replayed or forged dispositions must not start attempts, bill again, or
	// allocate state for unknown sequences.
	llm.EmitAttempt(ctx, llm.AttemptEvent{Phase: llm.AttemptDiscarded, Sequence: 1, DiscardReason: llm.AttemptDiscardCompatibility, Usage: &llm.Usage{InputTokens: 999}})
	llm.EmitAttempt(ctx, llm.AttemptEvent{Phase: llm.AttemptDiscarded, Sequence: 999, DiscardReason: llm.AttemptDiscardProxyRetry})
	b := llm.StartAttempt(ctx)
	b.Usage(llm.Usage{InputTokens: 3, CostKnown: true})
	b.Finish(llm.AttemptSucceeded, nil)
	call.ObserveStream(llm.StreamEvent{Usage: &llm.Usage{InputTokens: 3}})
	call.Finish(llm.Usage{InputTokens: 3}, nil)
	var bill, waste, starts, disposals int
	var cost, wastedCost float64
	for _, e := range r.models {
		switch e.Phase {
		case ModelStart:
			starts++
		case ModelUsageDelta:
			bill += e.Usage.InputTokens
			cost += e.Usage.CostUSD
		case ModelDiscard:
			disposals++
			waste += e.Usage.InputTokens
			wastedCost += e.Usage.CostUSD
			if e.Attempt.Provider != "actual" || e.Attempt.Model != "actual-model" || e.Attempt.ErrorClass != "source_compatibility" || e.Attempt.Usage != nil {
				t.Fatalf("disposition lost exclusive identity: %+v", e)
			}
		}
	}
	if bill != 10 || waste != 7 || starts != 2 || disposals != 1 || cost != .1 || wastedCost != .1 {
		t.Fatalf("billing/waste/starts/dispositions/cost/wastedCost = %d/%d/%d/%d/%v/%v", bill, waste, starts, disposals, cost, wastedCost)
	}
}
