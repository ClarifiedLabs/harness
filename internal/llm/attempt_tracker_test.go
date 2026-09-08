package llm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrackAttemptsNestedNamespacesAndDispositionDedup(t *testing.T) {
	var events []AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { events = append(events, e) }))
	ctx = WithAttemptMetadata(ctx, AttemptMetadata{Scope: AttemptScopeUpstream, Provider: "actual", Model: "model", API: "responses", Transport: "http", Purpose: RequestPurposeTurn})
	before := StartAttempt(ctx)
	before.Finish(AttemptSucceeded, nil)
	outerCtx, outer := TrackAttempts(ctx)
	first := StartAttempt(outerCtx)
	first.Usage(Usage{InputTokens: 7, CostKnown: true, CostUSD: 0.2})
	first.Finish(AttemptSucceeded, nil) // discard is independent of outcome
	innerCtx, inner := TrackAttempts(outerCtx)
	second := StartAttempt(innerCtx)
	second.Usage(Usage{InputTokens: 11})
	second.Finish(AttemptFailed, context.Canceled)
	_ = ObserveRetryWait(innerCtx, 0, RetryLayerProvider, AttemptErrorRequest, func() error { return nil })
	unfinished := StartAttempt(innerCtx)
	inner.Discard(AttemptDiscardCompatibility)
	inner.Discard(AttemptDiscardProxyRetry)
	outer.Discard(AttemptDiscardProxyRetry)
	outer.Discard(AttemptDiscardProxyRetry)
	after := StartAttempt(ctx)
	after.Finish(AttemptSucceeded, nil)
	finished := map[uint64]bool{}
	discarded := map[uint64]AttemptDiscardReason{}
	var started []uint64
	for _, e := range events {
		switch e.Phase {
		case AttemptStarted:
			started = append(started, e.Sequence)
		case AttemptFinished:
			finished[e.Sequence] = true
		case AttemptDiscarded:
			if !finished[e.Sequence] {
				t.Fatalf("discard preceded physical finish: %+v", e)
			}
			if _, exists := discarded[e.Sequence]; exists {
				t.Fatalf("duplicate disposition: %+v", e)
			}
			if e.Usage != nil || e.Duration != nil || e.TTFT != nil || e.RetryDelay != nil || e.Provider != "actual" || e.Model != "model" {
				t.Fatalf("discard payload=%+v", e)
			}
			discarded[e.Sequence] = e.DiscardReason
		}
	}
	if len(started) != 5 {
		t.Fatalf("starts=%v", started)
	}
	for i, sequence := range started {
		if sequence != uint64(i+1) {
			t.Fatalf("subgroup reset allocator: %v", started)
		}
	}
	if len(discarded) != 2 || discarded[2] != AttemptDiscardProxyRetry || discarded[3] != AttemptDiscardCompatibility {
		t.Fatalf("dispositions=%v", discarded)
	}
	unfinished.Finish(AttemptSucceeded, nil)
	// No automatic discard is inferred when the previously unfinished source ends.
	if events[len(events)-1].Phase != AttemptFinished {
		t.Fatal("late finish inferred discard")
	}
}

func TestTrackAttemptsConcurrentGroupsShareAllocatorAndDedup(t *testing.T) {
	var mu sync.Mutex
	var events []AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { mu.Lock(); defer mu.Unlock(); events = append(events, e) }))
	ctx, outer := TrackAttempts(ctx)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			childCtx, child := TrackAttempts(ctx)
			source := StartAttempt(childCtx)
			source.Usage(Usage{InputTokens: 1})
			source.Finish(AttemptSucceeded, nil)
			child.Discard(AttemptDiscardCompatibility)
			child.Discard(AttemptDiscardCompatibility)
		})
	}
	wg.Wait()
	outer.Discard(AttemptDiscardProxyRetry)
	starts, finishes, discards := map[uint64]int{}, map[uint64]int{}, map[uint64]int{}
	for _, e := range events {
		switch e.Phase {
		case AttemptStarted:
			starts[e.Sequence]++
		case AttemptFinished:
			finishes[e.Sequence]++
		case AttemptDiscarded:
			if finishes[e.Sequence] != 1 {
				t.Fatalf("disposition before finish: %+v", e)
			}
			discards[e.Sequence]++
		}
	}
	if len(starts) != 16 || len(discards) != 16 {
		t.Fatalf("starts=%v discards=%v", starts, discards)
	}
	for seq := uint64(1); seq <= 16; seq++ {
		if starts[seq] != 1 || discards[seq] != 1 {
			t.Fatalf("sequence %d starts=%d discards=%d", seq, starts[seq], discards[seq])
		}
	}
}

func TestTrackAttemptsNoObserverAndNewCall(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), WithAttemptObserver(context.Background(), nil)} {
		tracked, tracker := TrackAttempts(ctx)
		if tracked != ctx || tracker != nil {
			t.Fatal("unobserved tracker changed context")
		}
		tracker.Discard(AttemptDiscardCompatibility)
	}
	var events []AttemptEvent
	observer := AttemptObserverFunc(func(e AttemptEvent) { events = append(events, e) })
	for range 2 {
		ctx, tracker := TrackAttempts(WithAttemptObserver(context.Background(), observer))
		s := StartAttempt(ctx)
		s.Finish(AttemptSucceeded, nil)
		tracker.Discard(AttemptDiscardCompatibility)
	}
	if len(events) != 6 || events[2].Sequence != 1 || events[5].Sequence != 1 || events[2].Phase != AttemptDiscarded || events[5].Phase != AttemptDiscarded {
		t.Fatalf("call namespaces=%+v", events)
	}
}

func TestTrackAttemptsRecognizesForwardedDisposition(t *testing.T) {
	var events []AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { events = append(events, e) }))
	tracked, tracker := TrackAttempts(ctx)
	EmitAttempt(tracked, AttemptEvent{Phase: AttemptFinished, Sequence: 7, Usage: &Usage{InputTokens: 3}})
	// A forwarded disposition on the root context also participates in local
	// deduplication, even though it bypasses this subgroup's tee observer.
	EmitAttempt(ctx, AttemptEvent{Phase: AttemptDiscarded, Sequence: 7, DiscardReason: AttemptDiscardCompatibility})
	tracker.Discard(AttemptDiscardProxyRetry)
	if len(events) != 2 {
		t.Fatalf("duplicate forwarded disposition=%+v", events)
	}
}

func TestDiscardWireIsNonBillingAndReasonBounded(t *testing.T) {
	duration := time.Second
	for _, reason := range []AttemptDiscardReason{AttemptDiscardCompatibility, AttemptDiscardProxyRetry, "PRIVATE arbitrary reason"} {
		e := NormalizeAttemptEvent(AttemptEvent{Phase: AttemptDiscarded, Sequence: 9, DiscardReason: reason, Usage: &Usage{InputTokens: 10, CostUSD: 2, CostKnown: true}, CacheWriteTTLKnown: true, Duration: &duration, TTFT: &duration, RetryDelay: &duration, Outcome: AttemptFailed, ErrorClass: AttemptErrorServer, StatusCode: 503})
		if e.Usage != nil || e.TTFT != nil || e.Duration != nil || e.RetryDelay != nil || e.CacheWriteTTLKnown || e.Outcome != "" || e.ErrorClass != "" || e.StatusCode != 0 {
			t.Fatalf("disposition repeated physical facts: %+v", e)
		}
		want := reason
		if reason != AttemptDiscardCompatibility && reason != AttemptDiscardProxyRetry {
			want = AttemptDiscardUnknown
		}
		if e.DiscardReason != want {
			t.Fatalf("reason=%q", e.DiscardReason)
		}
		wire, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"usage", "duration", "ttft", "cost_usd", "PRIVATE"} {
			if strings.Contains(string(wire), forbidden) {
				t.Fatalf("%q leaked in %s", forbidden, wire)
			}
		}
		var decoded AttemptEvent
		if err := json.Unmarshal(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Phase != AttemptDiscarded || decoded.Sequence != 9 || decoded.DiscardReason != want {
			t.Fatalf("wire=%s", wire)
		}
	}
	zero := NormalizeAttemptEvent(AttemptEvent{Phase: AttemptDiscarded, DiscardReason: AttemptDiscardCompatibility})
	if zero.Phase != "" {
		t.Fatal("discard accepted without sequence reference")
	}
	physical := NormalizeAttemptEvent(AttemptEvent{Phase: AttemptFinished, Sequence: 9, DiscardReason: AttemptDiscardCompatibility})
	if physical.DiscardReason != "" {
		t.Fatal("physical finish inferred disposition")
	}
}
