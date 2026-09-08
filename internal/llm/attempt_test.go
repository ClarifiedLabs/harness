package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAttemptSourceTimingAndSnapshots(t *testing.T) {
	var events []AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { events = append(events, e) }))
	a := StartAttempt(ctx)
	a.ObserveStream(StreamEvent{Kind: EventModelRequest}, nil)
	a.Usage(Usage{InputTokens: 3, CacheWriteTTLKnown: true})
	if events[len(events)-1].TTFT != nil || events[len(events)-1].Duration != nil {
		t.Fatal("headers/usage manufactured timing")
	}
	a.ObserveStream(StreamEvent{Kind: EventReasoningSummary, Text: "visible"}, nil)
	a.Usage(Usage{InputTokens: 3, OutputTokens: 1})
	a.Finish(AttemptFailed, errors.New("PRIVATE error body"))
	a.Finish(AttemptSucceeded, nil)
	e := events[len(events)-1]
	if len(events) != 4 || e.Outcome != AttemptFailed || e.Duration == nil || e.TTFT == nil || *e.Duration < *e.TTFT || e.Usage.OutputTokens != 1 {
		t.Fatalf("event=%+v count=%d", e, len(events))
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var round AttemptEvent
	if err := json.Unmarshal(raw, &round); err != nil || round.Sequence != e.Sequence {
		t.Fatalf("round trip: %s %v", raw, err)
	}
	unknown := StartAttemptUnknown(ctx)
	unknown.Generated()
	unknown.Finish(AttemptSucceeded, nil)
	e = events[len(events)-1]
	if e.Duration != nil || e.TTFT != nil {
		t.Fatal("unknown start fabricated timings")
	}
}
func TestConnectEmitsRetriesOutsideYield(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"private"}}`))
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	var events []AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) { events = append(events, e) }))
	resp, err := Connect(ctx, ConnectOptions{Client: srv.Client(), URL: srv.URL, Header: func(*http.Request) {}, ParseError: ParseErrorResponseByType, Sleep: func(time.Duration) {}}, nil, func(StreamEvent, error) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(events) != 4 || events[1].Phase != AttemptFinished || events[1].StatusCode != 503 || events[1].TTFT != nil || events[3].Cause != AttemptRetry || events[3].RetryLayer != RetryLayerConnect || events[3].Sequence != 2 {
		t.Fatalf("events=%+v", events)
	}
	wait := events[2]
	if wait.Phase != AttemptRetryWait || wait.Sequence != 0 || wait.RetryDelay == nil || wait.Duration == nil || wait.ErrorClass != AttemptErrorServer || wait.RetryLayer != RetryLayerConnect || wait.Usage != nil {
		t.Fatalf("wait=%+v", wait)
	}
	source := ResponseAttempt(resp)
	source.Finish(AttemptSucceeded, nil)
	if len(events) != 5 || events[4].StatusCode != 200 || events[4].Duration == nil {
		t.Fatalf("final=%+v", events)
	}
}

func TestAttemptObserverCannotMutateSourceAndBoundsEnums(t *testing.T) {
	var final AttemptEvent
	ctx := WithAttemptObserver(context.Background(), AttemptObserverFunc(func(e AttemptEvent) {
		if e.Phase == AttemptUsage {
			e.Usage.InputTokens = 999
		}
		if e.Phase == AttemptFinished {
			final = e
		}
	}))
	a := StartAttempt(ctx)
	a.Usage(Usage{InputTokens: 7})
	a.Finish(AttemptSucceeded, nil)
	if final.Usage.InputTokens != 7 {
		t.Fatal("observer mutated source snapshot")
	}
	e := NormalizeAttemptEvent(AttemptEvent{AttemptMetadata: AttemptMetadata{Scope: "private-id", Purpose: "free text", Cause: "unbounded", RetryLayer: "secret", API: "arbitrary", Transport: "hostname"}, Phase: AttemptFinished, Outcome: "error text", ErrorClass: "error code", StatusCode: 999})
	if e.Scope != AttemptScopeProviderCall || e.Purpose != RequestPurposeUnknown || e.Outcome != AttemptIncomplete || e.ErrorClass != AttemptErrorUnknown || e.StatusCode != 0 || e.API != "unknown" || e.Transport != "unknown" {
		t.Fatalf("unbounded event=%+v", e)
	}
}
