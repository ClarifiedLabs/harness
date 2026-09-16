package main

import (
	"context"
	"time"
)

// startSubscriptionPolling never blocks serving or overlaps poll cycles. The
// command owns cancellation; the injected timer keeps tests sleep-free.
func startSubscriptionPolling(ctx context.Context, enabled bool, interval time.Duration, after func(time.Duration) <-chan time.Time, refresh func(context.Context)) <-chan struct{} {
	done := make(chan struct{})
	if !enabled || interval <= 0 || ctx.Err() != nil {
		close(done)
		return done
	}
	if after == nil {
		after = time.After
	}
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			refresh(ctx)
			if ctx.Err() != nil {
				return
			}
			// Start a fresh delay after completion, even on failure. No queued ticks.
			ticks := after(interval)
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
			}
		}
	}()
	return done
}
