package llm

import (
	"context"
	"errors"
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
		}, 0, time.Second, &APIError{Message: "retry", Retryable: true}, func(_ StreamEvent, err error) bool {
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
