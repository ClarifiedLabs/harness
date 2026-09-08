package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObserveRetryWaitNilObserverPreservesCallback(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), WithAttemptObserver(context.Background(), nil)} {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		want := errors.New("unchanged callback error")
		calls := 0
		got := ObserveRetryWait(ctx, time.Hour, RetryLayerAgent, AttemptErrorRateLimit, func() error { calls++; return want })
		if calls != 1 || got != want {
			t.Fatalf("calls=%d error=%v", calls, got)
		}
		if got := ObserveRetryWait(ctx, 0, RetryLayerAgent, AttemptErrorRateLimit, func() error { return nil }); got != nil {
			t.Fatalf("injected cancellation: %v", got)
		}
	}
}

func TestObserveRetryWaitMetadataAndDurations(t *testing.T) {
	for _, planned := range []time.Duration{-1, 0, time.Hour} {
		t.Run(planned.String(), func(t *testing.T) {
			var events []AttemptEvent
			ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { events = append(events, e) }))
			meta := AttemptMetadata{Scope: AttemptScopeUpstream, Purpose: RequestPurposeCompaction, Provider: "configured", API: "responses", Model: "actual-model", Transport: "websocket", Cause: AttemptInitial, RetryLayer: RetryLayerNone}
			ctx = WithAttemptMetadata(ctx, meta)
			calls := 0
			if err := ObserveRetryWait(ctx, planned, RetryLayerProvider, AttemptErrorTransport, func() error { calls++; return nil }); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(events) != 1 {
				t.Fatalf("calls=%d events=%+v", calls, events)
			}
			e := events[0]
			if e.Phase != AttemptRetryWait || e.Sequence != 0 || e.Usage != nil || e.TTFT != nil || e.Duration == nil || *e.Duration < 0 || e.Outcome != AttemptSucceeded || e.ErrorClass != AttemptErrorTransport {
				t.Fatalf("wait=%+v", e)
			}
			if planned < 0 {
				if e.RetryDelay != nil {
					t.Fatal("invented unknown planned delay")
				}
			} else if e.RetryDelay == nil || *e.RetryDelay != planned {
				t.Fatalf("planned=%v event=%+v", planned, e)
			}
			wantMeta := meta
			wantMeta.Cause = AttemptRetry
			wantMeta.RetryLayer = RetryLayerProvider
			if e.AttemptMetadata != wantMeta || AttemptMetadataFromContext(ctx) != meta {
				t.Fatalf("metadata=%+v", e.AttemptMetadata)
			}
			body, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var wire AttemptEvent
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			wire = NormalizeAttemptEvent(wire)
			if wire.Phase != AttemptRetryWait || wire.Duration == nil || *wire.Duration != *e.Duration || (wire.RetryDelay == nil) != (e.RetryDelay == nil) {
				t.Fatalf("wire=%s", body)
			}
			if wire.RetryDelay != nil && *wire.RetryDelay != planned {
				t.Fatalf("wire planned=%v", *wire.RetryDelay)
			}
			a := StartAttempt(ctx)
			a.Finish(AttemptSucceeded, nil)
			if events[1].Phase != AttemptStarted || events[1].Sequence != 1 {
				t.Fatalf("wait consumed request sequence: %+v", events)
			}
		})
	}
}

func TestObserveRetryWaitCancellationAndErrorReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan AttemptEvent, 1)
	ctx = WithAttemptObserver(ctx, AttemptObserverFunc(func(e AttemptEvent) { events <- e }))
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ObserveRetryWait(ctx, time.Hour, RetryLayerConnect, AttemptErrorRateLimit, func() error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	<-entered
	select {
	case e := <-events:
		t.Fatalf("wait observed before callback completed: %+v", e)
	default:
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	e := <-events
	if e.Outcome != AttemptCancelled || e.ErrorClass != AttemptErrorRateLimit || e.Duration == nil || e.RetryDelay == nil || *e.RetryDelay != time.Hour || e.Sequence != 0 {
		t.Fatalf("wait=%+v", e)
	}
	for _, want := range []error{context.DeadlineExceeded, errors.New("PRIVATE error text")} {
		if got := ObserveRetryWait(ctx, 0, RetryLayerAgent, AttemptErrorServer, func() error { return want }); got != want {
			t.Fatalf("error=%v want=%v", got, want)
		}
		e = <-events
		outcome := AttemptFailed
		if want == context.DeadlineExceeded {
			outcome = AttemptCancelled
		}
		if e.Outcome != outcome || e.ErrorClass != AttemptErrorServer {
			t.Fatalf("wait=%+v", e)
		}
		body, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "PRIVATE") {
			t.Fatalf("error content leaked: %s", body)
		}
	}
}

func TestNormalizeRetryWaitNonBillingAndOptionalDurations(t *testing.T) {
	planned := time.Second
	e := NormalizeAttemptEvent(AttemptEvent{Phase: AttemptRetryWait, RetryDelay: &planned, Usage: &Usage{InputTokens: 999, CostUSD: 3}, CacheWriteTTLKnown: true, TTFT: &planned, AttemptMetadata: AttemptMetadata{RetryLayer: "unbounded"}, ErrorClass: "raw provider message"})
	if e.Phase != AttemptRetryWait || e.Usage != nil || e.CacheWriteTTLKnown || e.TTFT != nil || e.Duration != nil || e.RetryLayer != RetryLayerNone || e.ErrorClass != AttemptErrorUnknown || e.RetryDelay == nil {
		t.Fatalf("wait=%+v", e)
	}
	*e.RetryDelay = 0
	if planned != time.Second {
		t.Fatal("normalized pointer aliases source")
	}
	proxy := NormalizeAttemptEvent(AttemptEvent{Phase: AttemptRetryWait, AttemptMetadata: AttemptMetadata{RetryLayer: RetryLayerProxy}})
	if proxy.RetryLayer != RetryLayerProxy {
		t.Fatal("proxy retry layer lost during normalization")
	}
	negative := -time.Second
	e = NormalizeAttemptEvent(AttemptEvent{Phase: AttemptRetryWait, RetryDelay: &negative, Duration: &negative})
	if e.RetryDelay != nil || e.Duration != nil {
		t.Fatal("negative durations became known observations")
	}
}

type retryWaitTransport func(*http.Request) (*http.Response, error)

func (f retryWaitTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConnectRetryContextMatchesPhysicalFacts(t *testing.T) {
	var facts []AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { facts = append(facts, e) }))
	meta := AttemptMetadata{Scope: AttemptScopeUpstream, Purpose: RequestPurposeBranchSummary, Provider: "configured", Model: "model", API: "openai", Transport: "http", Cause: AttemptRetry, RetryLayer: RetryLayerAgent}
	ctx = WithAttemptMetadata(ctx, meta)
	var headers, requests []AttemptMetadata
	client := &http.Client{Transport: retryWaitTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, AttemptMetadataFromContext(r.Context()))
		if AttemptFromContext(r.Context()) == nil {
			t.Error("physical source missing from request context")
		}
		status := http.StatusOK
		if len(requests) == 1 {
			status = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"retry"}}`)), Request: r}, nil
	})}
	resp, err := Connect(ctx, ConnectOptions{Client: client, URL: "https://configured.invalid", Header: func(r *http.Request) { headers = append(headers, AttemptMetadataFromContext(r.Context())) }, ParseError: ParseErrorResponseByType, Sleep: func(time.Duration) {}}, nil, func(StreamEvent, error) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ResponseAttempt(resp).Finish(AttemptSucceeded, nil)
	if len(requests) != 2 || len(headers) != 2 || requests[0] != meta || headers[0] != meta {
		t.Fatalf("requests=%+v headers=%+v", requests, headers)
	}
	want := meta
	want.RetryLayer = RetryLayerConnect
	if requests[1] != want || headers[1] != want || AttemptMetadataFromContext(resp.Request.Context()) != want || AttemptMetadataFromContext(ctx) != meta {
		t.Fatalf("retry requests=%+v headers=%+v", requests, headers)
	}
	if len(facts) != 5 || facts[2].Phase != AttemptRetryWait || facts[2].RetryLayer != RetryLayerConnect || facts[3].Sequence != 2 || facts[3].AttemptMetadata != want {
		t.Fatalf("facts=%+v", facts)
	}
}

func TestConnectCancelledWaitDoesNotCreateRetryAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var facts []AttemptEvent
	ctx = WithAttemptObserver(ctx, AttemptObserverFunc(func(e AttemptEvent) { facts = append(facts, e) }))
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	requests := 0
	client := &http.Client{Transport: retryWaitTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"retry"}}`))}, nil
	})}
	done := make(chan struct{})
	var yielded error
	go func() {
		defer close(done)
		_, _ = Connect(ctx, ConnectOptions{Client: client, URL: "https://configured.invalid", Header: func(*http.Request) {}, ParseError: ParseErrorResponseByType, Sleep: func(time.Duration) { close(entered); <-release }}, nil, func(_ StreamEvent, err error) bool {
			if err != nil {
				yielded = err
			}
			return true
		})
	}()
	<-entered
	cancel()
	<-done
	if requests != 1 || yielded != context.Canceled || len(facts) != 3 || facts[2].Phase != AttemptRetryWait || facts[2].Outcome != AttemptCancelled || facts[2].ErrorClass != AttemptErrorRateLimit || facts[2].Sequence != 0 {
		t.Fatalf("requests=%d yielded=%v facts=%+v", requests, yielded, facts)
	}
}
