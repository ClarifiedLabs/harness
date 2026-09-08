package server

import (
	"context"
	"math"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
)

func modelDispositionTotals(events []execution.ModelEvent) (billed, discarded int, cost, discardedCost float64) {
	for _, event := range events {
		switch event.Phase {
		case execution.ModelUsageDelta:
			billed += event.Usage.InputTokens
			cost += event.Usage.CostUSD
		case execution.ModelDiscard:
			discarded += event.Usage.InputTokens
			discardedCost += event.Usage.CostUSD
		}
	}
	return
}

func TestProxyHiddenAttemptDispositionsBillOnce(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "successive-proxy-retries"
		if nested {
			name = "nested-compatibility-and-proxy-retry"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			p := &attemptProvider{stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
				calls++
				input := 2
				var failure error
				if nested && calls == 1 {
					groupCtx, group := llm.TrackAttempts(ctx)
					func() {
						a := llm.StartAttempt(groupCtx)
						a.Usage(llm.Usage{InputTokens: 7})
						defer a.Finish(llm.AttemptFailed, &llm.APIError{StatusCode: 400})
					}()
					group.Discard(llm.AttemptDiscardCompatibility)
					_ = llm.ObserveRetryWait(ctx, 0, llm.RetryLayerProvider, llm.AttemptErrorRequest, func() error { return nil })
					ctx = llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerProvider)
					input = 11
					failure = &llm.APIError{StatusCode: 400, Message: "unsupported tool web_search"}
				} else if !nested {
					switch calls {
					case 1:
						input = 7
						failure = &llm.APIError{StatusCode: 400, Message: "unsupported tool web_search"}
					case 2:
						input = 11
						failure = &llm.APIError{StatusCode: 400, Message: "Invalid 'max_tokens': must be greater than or equal to 16."}
					}
				}
				a := llm.StartAttempt(ctx)
				u := llm.Usage{InputTokens: input}
				a.Usage(u)
				if failure != nil {
					// Finish deliberately runs after the proxy has received the
					// iterator error. Disposition must wait for iterator cleanup.
					defer a.Finish(llm.AttemptFailed, failure)
					yield(llm.StreamEvent{}, failure)
					return
				}
				a.Finish(llm.AttemptSucceeded, nil)
				yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
			}}
			_, client := attemptProxy(t, "openai", p)
			recorder := &modelRecorder{}
			scope := execution.Scope{Observer: recorder, Identity: execution.Identity{Provider: "proxy", Model: "alias"}}
			ctx, call := scope.ModelCall(context.Background(), llm.RequestPurposeTurn)
			var wireEvents []llm.AttemptEvent
			ctx = llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(event llm.AttemptEvent) {
				wireEvents = append(wireEvents, event)
				call.ObserveAttempt(event)
			}))
			var logicalUsage llm.Usage
			req := llm.Request{Purpose: llm.RequestPurposeTurn, MaxTokens: 1, ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}}}
			for event, err := range client.Provider("actual:real-model").Stream(ctx, req) {
				if err != nil {
					t.Fatal(err)
				}
				call.ObserveStream(event)
				if event.Usage != nil {
					logicalUsage = *event.Usage
				}
			}
			call.Finish(logicalUsage, nil)
			billed, discarded, cost, discardedCost := modelDispositionTotals(recorder.events)
			if logicalUsage.InputTokens != 2 || billed != 20 || discarded != 18 || math.Abs(cost-40e-6) > 1e-12 || math.Abs(discardedCost-36e-6) > 1e-12 {
				t.Fatalf("logical=%+v billed=%d discarded=%d cost=%g discarded_cost=%g", logicalUsage, billed, discarded, cost, discardedCost)
			}
			var starts, finishes, waits, discards int
			finished := map[uint64]bool{}
			dispositionSeen := map[uint64]bool{}
			for _, event := range wireEvents {
				if event.Provider != "actual" || event.Model != "real-model" || event.API != "openai" || event.Purpose != llm.RequestPurposeTurn {
					t.Fatalf("source attribution=%+v", event)
				}
				switch event.Phase {
				case llm.AttemptStarted:
					starts++
					if event.Sequence != uint64(starts) || event.StatusCode != 0 || event.ErrorClass != "" {
						t.Fatalf("start reused sequence or invented prior failure status: %+v", event)
					}
					if starts > 1 && event.Cause != llm.AttemptRetry {
						t.Fatalf("retry cause=%+v", event)
					}
				case llm.AttemptFinished:
					finishes++
					finished[event.Sequence] = true
					if event.Sequence < 3 && (event.StatusCode != 400 || event.ErrorClass != llm.AttemptErrorRequest) {
						t.Fatalf("actual failed attempt metadata=%+v", event)
					}
				case llm.AttemptRetryWait:
					waits++
					if event.Sequence != 0 || event.Usage != nil || event.ErrorClass != llm.AttemptErrorRequest {
						t.Fatalf("wait=%+v", event)
					}
				case llm.AttemptDiscarded:
					discards++
					if !finished[event.Sequence] || dispositionSeen[event.Sequence] || event.Sequence == 3 || event.Usage != nil || event.Duration != nil || event.TTFT != nil || event.StatusCode != 0 {
						t.Fatalf("invalid, premature, or duplicate disposition=%+v", event)
					}
					want := llm.AttemptDiscardProxyRetry
					if nested && event.Sequence == 1 {
						want = llm.AttemptDiscardCompatibility
					}
					if event.DiscardReason != want {
						t.Fatalf("disposition reason=%s want %s", event.DiscardReason, want)
					}
					dispositionSeen[event.Sequence] = true
				}
			}
			if starts != 3 || finishes != 3 || waits != 2 || discards != 2 {
				t.Fatalf("starts=%d finishes=%d waits=%d discards=%d", starts, finishes, waits, discards)
			}
			// A caller can discard only the returned logical usage later. The
			// hidden source subset is absent, so the two reports remain disjoint.
			scope.Discard(logicalUsage, llm.RequestPurposeTurn, "caller_rejected")
			billed, discarded, cost, discardedCost = modelDispositionTotals(recorder.events)
			if billed != 20 || discarded != 20 || math.Abs(cost-40e-6) > 1e-12 || math.Abs(discardedCost-40e-6) > 1e-12 {
				t.Fatalf("logical discard overlapped source billing: billed=%d discarded=%d cost=%g discarded_cost=%g", billed, discarded, cost, discardedCost)
			}
		})
	}
}

func TestProxyDoesNotDiscardPartiallyExposedAttempt(t *testing.T) {
	calls := 0
	p := &attemptProvider{stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
		calls++
		a := llm.StartAttempt(ctx)
		u := llm.Usage{InputTokens: 7}
		a.Usage(u)
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "partial", Usage: &u}, nil) {
			return
		}
		// A newer physical snapshot does not authorize the proxy to classify
		// this entire attempt as discarded after exposing logical usage.
		a.Usage(llm.Usage{InputTokens: 11})
		err := &llm.APIError{StatusCode: 400, Message: "unsupported tool web_search"}
		a.Finish(llm.AttemptFailed, err)
		yield(llm.StreamEvent{}, err)
	}}
	_, client := attemptProxy(t, "openai", p)
	recorder := &modelRecorder{}
	scope := execution.Scope{Observer: recorder}
	ctx, call := scope.ModelCall(context.Background(), llm.RequestPurposeTurn)
	var logicalUsage llm.Usage
	var streamErr error
	for event, err := range client.Provider("actual:real-model").Stream(ctx, llm.Request{ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}}}) {
		call.ObserveStream(event)
		if event.Usage != nil {
			logicalUsage = *event.Usage
		}
		if err != nil {
			streamErr = err
		}
	}
	call.Finish(logicalUsage, streamErr)
	billed, discarded, _, _ := modelDispositionTotals(recorder.events)
	if streamErr == nil || calls != 1 || billed != 11 || discarded != 0 || logicalUsage.InputTokens != 7 {
		t.Fatalf("err=%v calls=%d billed=%d discarded=%d logical=%+v", streamErr, calls, billed, discarded, logicalUsage)
	}
	scope.Discard(logicalUsage, llm.RequestPurposeTurn, "caller_rejected")
	billed, discarded, _, _ = modelDispositionTotals(recorder.events)
	if billed != 11 || discarded != 7 {
		t.Fatalf("caller discard billed=%d discarded=%d", billed, discarded)
	}
}

func TestProxyCompactionForwardsSourceDisposition(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			p := &attemptProvider{compact: func(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
				groupCtx, group := llm.TrackAttempts(ctx)
				a := llm.StartAttempt(groupCtx)
				a.Usage(llm.Usage{InputTokens: 7})
				a.Finish(llm.AttemptFailed, &llm.APIError{StatusCode: 400})
				group.Discard(llm.AttemptDiscardCompatibility)
				a = llm.StartAttempt(llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerProvider))
				u := llm.Usage{InputTokens: 2}
				a.Usage(u)
				var err error
				outcome := llm.AttemptSucceeded
				if fail {
					err = &llm.APIError{StatusCode: 502}
					outcome = llm.AttemptFailed
				}
				a.Finish(outcome, err)
				return llm.CompactedContext{Usage: u}, err
			}}
			_, client := attemptProxy(t, "responses", p)
			recorder := &modelRecorder{}
			ctx, call := (execution.Scope{Observer: recorder}).ModelCall(context.Background(), llm.RequestPurposeCompaction)
			result, err := client.Provider("actual:real-model").(llm.ContextCompactor).CompactContext(ctx, llm.Request{})
			call.Finish(result.Usage, err)
			billed, discarded, cost, discardedCost := modelDispositionTotals(recorder.events)
			if (err != nil) != fail || result.Usage.InputTokens != 2 || billed != 9 || discarded != 7 || math.Abs(cost-18e-6) > 1e-12 || math.Abs(discardedCost-14e-6) > 1e-12 {
				t.Fatalf("err=%v result=%+v billed=%d discarded=%d cost=%g discarded_cost=%g", err, result, billed, discarded, cost, discardedCost)
			}
		})
	}
}

func TestPriceAttemptDispositionDoesNotInventUsage(t *testing.T) {
	h, _ := attemptProxy(t, "openai", nil)
	target, err := h.resolveTarget("actual:real-model")
	if err != nil {
		t.Fatal(err)
	}
	d := time.Second
	u := llm.Usage{InputTokens: 20, CostUSD: 1, CostKnown: true}
	event := h.priceAttempt(target, llm.Request{}, llm.AttemptEvent{
		Phase: llm.AttemptDiscarded, Sequence: 2, DiscardReason: llm.AttemptDiscardProxyRetry,
		Usage: &u, Duration: &d, TTFT: &d, RetryDelay: &d, CacheWriteTTLKnown: true,
		StatusCode: 400, ErrorClass: llm.AttemptErrorRequest, Outcome: llm.AttemptFailed,
	})
	if event.Phase != llm.AttemptDiscarded || event.Sequence != 2 || event.DiscardReason != llm.AttemptDiscardProxyRetry || event.Usage != nil || event.Duration != nil || event.TTFT != nil || event.RetryDelay != nil || event.CacheWriteTTLKnown || event.StatusCode != 0 || event.Outcome != "" || event.ErrorClass != "" {
		t.Fatalf("disposition=%+v", event)
	}
	if u.InputTokens != 20 || u.CostUSD != 1 || !u.CostKnown {
		t.Fatalf("source usage mutated=%+v", u)
	}
}
