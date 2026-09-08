package server

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func TestPriceAttemptRetryWaitNeverPricesUsage(t *testing.T) {
	h, _ := attemptProxy(t, "openai", nil)
	target, err := h.resolveTarget("actual:real-model")
	if err != nil {
		t.Fatal(err)
	}
	planned, elapsed := time.Duration(0), time.Nanosecond
	usage := llm.Usage{InputTokens: 100, CostUSD: 1, CostKnown: true}
	event := h.priceAttempt(target, llm.Request{}, llm.AttemptEvent{
		AttemptMetadata: llm.AttemptMetadata{RetryLayer: llm.RetryLayerProxy},
		Phase:           llm.AttemptRetryWait, RetryDelay: &planned, Duration: &elapsed,
		Usage: &usage, CacheWriteTTLKnown: true, TTFT: &elapsed,
	})
	if event.Phase != llm.AttemptRetryWait || event.Sequence != 0 || event.RetryLayer != llm.RetryLayerProxy || event.Usage != nil || event.CacheWriteTTLKnown || event.TTFT != nil || event.RetryDelay == nil || *event.RetryDelay != 0 || event.Duration == nil || *event.Duration != elapsed {
		t.Fatalf("wait = %+v", event)
	}
	if usage.CostUSD != 1 || usage.InputTokens != 100 {
		t.Fatalf("source usage mutated: %+v", usage)
	}
}

func TestProxyForwardsSourceRetryWaitWithoutAllocatingAttempt(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		name := "success"
		if cancelWait {
			name = "cancelled-before-retry"
		}
		t.Run(name, func(t *testing.T) {
			p := &attemptProvider{stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
				a := llm.StartAttempt(ctx)
				a.Usage(llm.Usage{InputTokens: 2})
				a.Finish(llm.AttemptFailed, &llm.APIError{StatusCode: 429})
				err := llm.ObserveRetryWait(ctx, 7*time.Second, llm.RetryLayerConnect, llm.AttemptErrorRateLimit, func() error {
					if cancelWait {
						return context.Canceled
					}
					return nil
				})
				if err != nil {
					yield(llm.StreamEvent{}, err)
					return
				}
				a = llm.StartAttempt(llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerConnect))
				u := llm.Usage{InputTokens: 3}
				a.Usage(u)
				a.Finish(llm.AttemptSucceeded, nil)
				yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
			}}
			_, client := attemptProxy(t, "openai", p)
			recorder := &modelRecorder{}
			ctx, call := (execution.Scope{Observer: recorder}).ModelCall(context.Background(), llm.RequestPurposeTurn)
			var streamErr error
			for event, err := range client.Provider("actual:real-model").Stream(ctx, llm.Request{Purpose: llm.RequestPurposeTurn}) {
				call.ObserveStream(event)
				if err != nil {
					streamErr = err
				}
			}
			call.Finish(llm.Usage{}, streamErr)
			if (streamErr != nil) != cancelWait {
				t.Fatalf("stream err=%v cancelWait=%v", streamErr, cancelWait)
			}
			var starts, retries, waits, bills, input int
			for _, event := range recorder.events {
				switch event.Phase {
				case execution.ModelStart:
					starts++
					if event.Attempt.Sequence != uint64(starts) {
						t.Fatalf("physical sequence=%d want %d", event.Attempt.Sequence, starts)
					}
					if event.Attempt.Cause == llm.AttemptRetry {
						retries++
					}
				case execution.ModelRetry:
					waits++
					want := llm.AttemptSucceeded
					if cancelWait {
						want = llm.AttemptCancelled
					}
					fact := event.Attempt
					if fact.Sequence != 0 || fact.RetryDelay == nil || *fact.RetryDelay != 7*time.Second || fact.Duration == nil || *fact.Duration < 0 || fact.RetryLayer != llm.RetryLayerConnect || fact.ErrorClass != llm.AttemptErrorRateLimit || fact.Outcome != want || llm.HasUsageDelta(event.Usage) {
						t.Fatalf("wait=%+v", event)
					}
				case execution.ModelUsageDelta:
					bills++
					input += event.Usage.InputTokens
					if !event.Usage.CostKnown || math.Abs(event.Usage.CostUSD-float64(event.Usage.InputTokens)*2e-6) > 1e-12 {
						t.Fatalf("priced usage=%+v", event.Usage)
					}
				}
			}
			wantStarts, wantRetries, wantInput := 2, 1, 5
			if cancelWait {
				wantStarts, wantRetries, wantInput = 1, 0, 2
			}
			if starts != wantStarts || retries != wantRetries || bills != wantStarts || waits != 1 || input != wantInput {
				t.Fatalf("starts=%d retries=%d bills=%d waits=%d input=%d", starts, retries, bills, waits, input)
			}
		})
	}
}

func TestProxyCompactionForwardsRetryWaitOnSuccessAndError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			p := &attemptProvider{compact: func(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
				a := llm.StartAttempt(ctx)
				a.Finish(llm.AttemptFailed, &llm.APIError{StatusCode: 503})
				err := llm.ObserveRetryWait(ctx, time.Second, llm.RetryLayerConnect, llm.AttemptErrorServer, func() error {
					if fail {
						return context.Canceled
					}
					return nil
				})
				if err == nil {
					a = llm.StartAttempt(llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerConnect))
					a.Finish(llm.AttemptSucceeded, nil)
				}
				return llm.CompactedContext{}, err
			}}
			_, client := attemptProxy(t, "responses", p)
			facts := &llmtest.AttemptRecorder{}
			_, err := client.Provider("actual:real-model").(llm.ContextCompactor).CompactContext(facts.Context(context.Background()), llm.Request{})
			if (err != nil) != fail {
				t.Fatalf("err=%v fail=%v", err, fail)
			}
			var waits int
			for _, event := range facts.Events() {
				if event.Phase == llm.AttemptRetryWait {
					waits++
					if event.Sequence != 0 || event.Usage != nil || event.Purpose != llm.RequestPurposeCompaction || event.RetryDelay == nil || *event.RetryDelay != time.Second || event.Duration == nil {
						t.Fatalf("wait=%+v", event)
					}
				}
			}
			if waits != 1 {
				t.Fatalf("waits=%d", waits)
			}
		})
	}
}

func TestProxyRetryWaitDoesNotSuppressProviderCallFallback(t *testing.T) {
	p := &attemptProvider{stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
		_ = llm.ObserveRetryWait(ctx, 0, llm.RetryLayerProvider, llm.AttemptErrorRequest, func() error { return nil })
		u := llm.Usage{InputTokens: 7}
		yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
	}}
	_, client := attemptProxy(t, "openai", p)
	recorder := &modelRecorder{}
	ctx, call := (execution.Scope{Observer: recorder}).ModelCall(context.Background(), llm.RequestPurposeTurn)
	for event, err := range client.Provider("actual:real-model").Stream(ctx, llm.Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		call.ObserveStream(event)
	}
	call.Finish(llm.Usage{}, nil)
	if len(recorder.events) != 4 || recorder.events[0].Phase != execution.ModelRetry || recorder.events[1].Attempt.Scope != llm.AttemptScopeProviderCall || recorder.events[2].Usage.InputTokens != 7 {
		t.Fatalf("events=%+v", recorder.events)
	}
}

func TestProxyZeroDelayWaitReportsCancellationWithoutChangingRetryDecision(t *testing.T) {
	// This is the same callback used at the immediate proxy retry boundary:
	// report cancellation, but leave the existing provider cancellation path in
	// charge of whether another physical request can start.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	facts := &llmtest.AttemptRecorder{}
	ctx = facts.Context(ctx)
	err := llm.ObserveRetryWait(ctx, 0, llm.RetryLayerProxy, llm.AttemptErrorRequest, ctx.Err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait err=%v", err)
	}
	events := facts.Events()
	if len(events) != 1 || events[0].Phase != llm.AttemptRetryWait || events[0].Sequence != 0 || events[0].Outcome != llm.AttemptCancelled || events[0].Usage != nil || events[0].RetryDelay == nil || *events[0].RetryDelay != 0 {
		t.Fatalf("events=%+v", events)
	}
}
