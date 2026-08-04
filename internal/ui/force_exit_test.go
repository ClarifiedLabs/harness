package ui

import (
	"context"
	"errors"
	"testing"
)

func TestForceExitPreflightFinishPreservesLaterExit(t *testing.T) {
	exit := make(chan struct{}, 1)
	preflight := newForceExitPreflight(exit)
	if preflight.Finish() {
		t.Fatal("Finish reported force exit before one was sent")
	}
	if !errors.Is(preflight.Context().Err(), context.Canceled) {
		t.Fatalf("preflight context error = %v, want context.Canceled", preflight.Context().Err())
	}

	exit <- struct{}{}
	if !forceExitRequested(exit) {
		t.Fatal("force exit sent after Finish was lost")
	}
}

func TestForceExitPreflightFinishDoesNotLoseConcurrentExit(t *testing.T) {
	const iterations = 200
	for i := 0; i < iterations; i++ {
		exit := make(chan struct{}, 1)
		preflight := newForceExitPreflight(exit)
		start := make(chan struct{})
		finished := make(chan bool, 1)
		go func() {
			<-start
			finished <- preflight.Finish()
		}()
		close(start)
		exit <- struct{}{}

		if forced := <-finished; !forced && !forceExitRequested(exit) {
			t.Fatalf("iteration %d lost concurrent force exit", i)
		}
	}
}
