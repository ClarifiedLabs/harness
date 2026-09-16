package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func waitQuotaEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("quota polling did not advance")
	}
}

func TestSubscriptionPollingImmediateTickNoOverlapAndCancellation(t *testing.T) {
	ticks := make(chan time.Time)
	scheduled := make(chan struct{}, 2)
	after := func(d time.Duration) <-chan time.Time {
		if d != time.Minute {
			t.Errorf("delay %s", d)
		}
		scheduled <- struct{}{}
		return ticks
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, maxActive, calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startSubscriptionPolling(ctx, true, time.Minute, after, func(ctx context.Context) {
		n := active.Add(1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
	})
	waitQuotaEvent(t, started)
	select {
	case <-scheduled:
		t.Fatal("delay scheduled before refresh completed")
	default:
	}
	select {
	case ticks <- time.Now():
		t.Fatal("poller accepted a tick during an active refresh")
	default:
	}
	close(release)
	waitQuotaEvent(t, scheduled)
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not wait for tick")
	}
	waitQuotaEvent(t, started)
	cancel()
	waitQuotaEvent(t, done)
	if calls.Load() != 2 || maxActive.Load() != 1 {
		t.Fatalf("calls=%d maxActive=%d", calls.Load(), maxActive.Load())
	}
}

func TestSubscriptionPollingStopsAnInflightRefresh(t *testing.T) {
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startSubscriptionPolling(ctx, true, time.Minute, nil, func(ctx context.Context) { close(started); <-ctx.Done() })
	waitQuotaEvent(t, started)
	cancel()
	waitQuotaEvent(t, done)
}

func TestSubscriptionPollingDisabledAndClosedTick(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		enabled  bool
		interval time.Duration
		ctx      context.Context
	}{
		{false, time.Minute, context.Background()}, {true, 0, context.Background()}, {true, time.Minute, canceled},
	} {
		done := startSubscriptionPolling(tc.ctx, tc.enabled, tc.interval, nil, func(context.Context) { t.Error("disabled poll ran") })
		waitQuotaEvent(t, done)
	}
	ticks := make(chan time.Time)
	close(ticks)
	calls := 0
	done := startSubscriptionPolling(context.Background(), true, time.Minute, func(time.Duration) <-chan time.Time { return ticks }, func(context.Context) { calls++ })
	waitQuotaEvent(t, done)
	if calls != 1 {
		t.Fatalf("closed tick channel caused %d polls", calls)
	}
}
