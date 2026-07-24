package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConnectBackoffWakesOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sleepStarted := make(chan time.Duration, 1)
	releaseSleep := make(chan struct{})
	t.Cleanup(func() { close(releaseSleep) })

	var yielded error
	done := make(chan bool, 1)
	go func() {
		done <- connectBackoff(ctx, func(d time.Duration) {
			sleepStarted <- d
			<-releaseSleep
		}, 0, time.Second, 0, &APIError{Message: "retry", Retryable: true}, func(_ StreamEvent, err error) bool {
			yielded = err
			return true
		})
	}()

	select {
	case <-sleepStarted:
	case <-time.After(time.Second):
		t.Fatal("backoff sleep did not start")
	}
	cancel()

	select {
	case retry := <-done:
		if retry {
			t.Fatal("connectBackoff returned retry=true after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("connectBackoff did not wake after cancellation")
	}
	if !errors.Is(yielded, context.Canceled) {
		t.Fatalf("yielded error = %v, want context.Canceled", yielded)
	}
}

func TestConnectLongRateLimitFailsWithoutRetry(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Retry-After", "61")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"quota_exhausted","message":"token plan exhausted"}}`))
	}))
	defer upstream.Close()

	slept := false
	var events []StreamEvent
	var yieldedErr error
	resp, err := Connect(context.Background(), ConnectOptions{
		Client: upstream.Client(),
		URL:    upstream.URL,
		Header: func(*http.Request) {},
		ParseError: func(resp *http.Response) *APIError {
			apiErr, _, code := ParseErrorResponse(resp)
			apiErr.Code = code
			return apiErr
		},
		Sleep: func(time.Duration) { slept = true },
	}, nil, func(event StreamEvent, err error) bool {
		if event.Kind == EventModelRequest {
			events = append(events, event)
		}
		if err != nil {
			yieldedErr = err
		}
		return true
	})

	if resp != nil || err == nil || yieldedErr == nil {
		t.Fatalf("Connect = resp %v err %v yielded %v, want terminal error", resp, err, yieldedErr)
	}
	if requests != 1 || slept {
		t.Fatalf("requests = %d slept = %v, want one request without retry", requests, slept)
	}
	if len(events) != 1 || events[0].ModelRequest == nil {
		t.Fatalf("model request events = %+v, want one terminal issue", events)
	}
	got := events[0].ModelRequest
	if got.State != ModelRequestUpstreamAttemptFailed || got.Outcome != ModelRequestOutcomeTerminal || got.StatusCode != http.StatusTooManyRequests || got.Message != "token plan exhausted" || got.RetryAfterMS != 61_000 {
		t.Fatalf("terminal issue = %+v", got)
	}
}

func TestConnectShortRateLimitEmitsRetryLifecycleThenSucceeds(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"error":{"message":"capacity temporarily exhausted"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	var slept []time.Duration
	var events []ModelRequestEvent
	resp, err := Connect(context.Background(), ConnectOptions{
		Client: upstream.Client(),
		URL:    upstream.URL,
		Header: func(*http.Request) {},
		ParseError: func(resp *http.Response) *APIError {
			apiErr, _, _ := ParseErrorResponse(resp)
			return apiErr
		},
		Sleep: func(delay time.Duration) { slept = append(slept, delay) },
	}, nil, func(event StreamEvent, err error) bool {
		if err != nil {
			t.Fatalf("unexpected yielded error: %v", err)
		}
		if event.Kind == EventModelRequest && event.ModelRequest != nil {
			events = append(events, *event.ModelRequest)
		}
		return true
	})
	if err != nil || resp == nil {
		t.Fatalf("Connect = resp %v err %v, want success", resp, err)
	}
	resp.Body.Close()
	if requests != 2 || len(slept) != 1 || slept[0] < time.Second || slept[0] > maxRateLimitRetryAfter {
		t.Fatalf("requests = %d slept = %v", requests, slept)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want failed attempt and retry schedule", events)
	}
	if events[0].State != ModelRequestUpstreamAttemptFailed || events[0].Outcome != ModelRequestOutcomeRetrying || events[0].StatusCode != 529 || events[0].Message != "capacity temporarily exhausted" {
		t.Fatalf("failure event = %+v", events[0])
	}
	if events[1].State != ModelRequestRetryScheduled || events[1].RetryDelayMS < 1000 {
		t.Fatalf("retry event = %+v", events[1])
	}
}
