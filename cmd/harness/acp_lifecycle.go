package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/config"
	"harness/internal/logging"
)

// A protocol close timeout does not mean the root stopped executing. Retain
// completion independently of the ACP server's bounded close result, so the
// process exporter can include cleanup observations from a late root.
const acpRootSettleTimeout = time.Second

// Allows the child owner's 15s EOF grace, TERM grace, and final reap. This is
// independent of protocol/telemetry deadlines, not an extension of fake roots.
const acpOwnedCleanupTimeout = 30 * time.Second

// Start all independent owners before joining any of them. Each receives a
// fresh bounded lifetime, so one slow owner cannot consume another's grace.
func startACPCleanups(cleanups []func(context.Context)) func() {
	var wg sync.WaitGroup
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanup := cleanups[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), acpOwnedCleanupTimeout)
			defer cancel()
			cleanup(ctx)
		}()
	}
	return wg.Wait
}

// Only the concrete production root exposes this join. Its resource owners
// have finite teardown bounds; a generic RootSession.Close may ignore ctx.
// Resources are immutable after construction and do not require the prompt mu.
func (r *acpRootSession) startOwnedCleanup(ctx context.Context) <-chan struct{} {
	r.cleanupOnce.Do(func() {
		r.cleanupDone = make(chan struct{})
		go func() {
			defer close(r.cleanupDone)
			join := startACPCleanups(r.cleanups)
			r.cleanupErr = r.agentSessions.CloseAll(ctx)
			if r.jobs != nil {
				r.jobs.ShutdownAndWait(time.Second)
			}
			join()
		}()
	})
	return r.cleanupDone
}

type acpTrackedRoot struct {
	root          acpagent.RootSession
	lifetime      context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	active        int
	closing       bool
	closeReturned bool
	closeErr      error
	closeDone     chan struct{}
	settled       chan struct{}
}

func trackACPRoot(root acpagent.RootSession) *acpTrackedRoot {
	ctx, cancel := context.WithCancel(context.Background())
	return &acpTrackedRoot{root: root, lifetime: ctx, cancel: cancel, settled: make(chan struct{}), closeDone: make(chan struct{})}
}

func (r *acpTrackedRoot) Prompt(ctx context.Context, text string, sink acpagent.UpdateSink) (acp.StopReason, error) {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return acp.StopReasonRefusal, errors.New("ACP root session is closing")
	}
	r.active++
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.lifetime, cancel)
	defer func() {
		stop()
		cancel()
		r.mu.Lock()
		r.active--
		r.settleLocked()
		r.mu.Unlock()
	}()
	return r.root.Prompt(ctx, text, sink)
}

func (r *acpTrackedRoot) settleLocked() {
	if r.closeReturned && r.active == 0 {
		select {
		case <-r.settled:
		default:
			close(r.settled)
		}
	}
}

func (r *acpTrackedRoot) startClose(ctx context.Context) {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.closing = true
	r.mu.Unlock()
	r.cancel()
	go func() {
		err := closeTrackedACPRoot(ctx, r.root)
		r.mu.Lock()
		r.closeErr = err
		r.closeReturned = true
		r.settleLocked()
		close(r.closeDone)
		r.mu.Unlock()
	}()
}

func (r *acpTrackedRoot) Close(ctx context.Context) error {
	// The protocol bounds this call externally. Do not report settlement while
	// the actual root is still closing; the factory uses startClose + settled
	// directly so its bounded owner never waits on a concurrent sync.Once.
	r.startClose(ctx)
	<-r.closeDone
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

func closeTrackedACPRoot(ctx context.Context, root acpagent.RootSession) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("ACP root close panicked")
		}
	}()
	return root.Close(ctx)
}

func (f *acpRootFactory) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), acpRootSettleTimeout)
	defer cancel()
	f.close(ctx)
}

// acpConstruction is registered before waiting for the construction gate.
// Its lifetime includes disposing a root returned after process shutdown.
type acpConstruction struct {
	done   chan struct{}
	cancel context.CancelFunc
	owned  bool // production builder: cancellation includes bounded resource teardown
}

func (f *acpRootFactory) beginConstruction(parent context.Context) (context.Context, func(), error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, nil, errors.New("ACP root factory is closed")
	}
	if f.construct == nil {
		f.construct = make(chan struct{}, 1)
	}
	if f.pending == nil {
		f.pending = make(map[*acpConstruction]struct{})
	}
	ctx, cancel := context.WithCancel(parent)
	pending := &acpConstruction{done: make(chan struct{}), cancel: cancel, owned: f.build == nil}
	f.pending[pending] = struct{}{}
	gate := f.construct
	f.mu.Unlock()
	finish := func() {
		cancel()
		f.mu.Lock()
		delete(f.pending, pending)
		close(pending.done)
		f.mu.Unlock()
	}
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			finish()
			return nil, nil, err
		}
		return ctx, func() { <-gate; finish() }, nil
	case <-ctx.Done():
		finish()
		return nil, nil, ctx.Err()
	}
}

// Called under constructor serialization, never under the lifecycle mutex
// while validation/exporter allocation runs. Close can always stop admission.
func (f *acpRootFactory) telemetryFor(cfg config.Config) (*rootTelemetry, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errors.New("ACP root factory is closed")
	}
	telemetry, initialized := f.telemetry, f.telemetryInitialized
	f.mu.Unlock()
	if initialized {
		return telemetry, nil
	}
	if f.logLevel != nil {
		level, err := logging.ParseLevel(cfg.LogLevel)
		if err != nil {
			return nil, err
		}
		f.logLevel.Set(level)
	}
	telemetry, err := newRootTelemetry(cfg, f.logger)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		telemetry.discard()
		return nil, errors.New("ACP root factory closed during telemetry construction")
	}
	f.telemetry, f.telemetryInitialized = telemetry, true
	f.mu.Unlock()
	return telemetry, nil
}

func (f *acpRootFactory) close(ctx context.Context) {
	f.mu.Lock()
	if f.closed {
		done := f.closeDone
		f.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
		return
	}
	f.closed = true
	f.closeDone = make(chan struct{})
	done := f.closeDone
	roots := f.roots
	f.roots = nil
	telemetry := f.telemetry
	pending := make([]*acpConstruction, 0, len(f.pending))
	for construction := range f.pending {
		pending = append(pending, construction)
	}
	f.mu.Unlock()
	defer close(done)

	for _, construction := range pending {
		construction.cancel()
	}
	waits := make([]<-chan struct{}, 0, len(roots)+len(pending))
	var owned []<-chan struct{}
	for _, root := range roots {
		root.startClose(ctx)
		if production, ok := root.root.(*acpRootSession); ok {
			owned = append(owned, production.startOwnedCleanup(ctx))
		}
		waits = append(waits, root.settled)
	}
	// Serve's deadline and the final telemetry budget may both have expired.
	// Neither permits the executable to abandon bounded owned process teardown.
	// Do not join closeDone/settled here: generic roots or prompts may hang.
	for _, done := range owned {
		<-done
	}
	// Construction may include non-context-aware filesystem reads, not just
	// teardown. Give production rollback its own cleanup budget, but never
	// treat completion of arbitrary initialization as an unbounded owner join.
	constructionCtx, cancelConstruction := context.WithTimeout(context.Background(), acpOwnedCleanupTimeout)
	defer cancelConstruction()
	for _, construction := range pending {
		if construction.owned {
			select {
			case <-construction.done:
			case <-constructionCtx.Done():
			}
		}
		waits = append(waits, construction.done)
	}
	for _, settled := range waits {
		select {
		case <-settled:
			continue
		default:
		}
		select {
		case <-settled:
		case <-ctx.Done():
			if f.logger != nil {
				f.logger.Warn("ACP roots or construction did not settle before final telemetry export; late metric detail may be lost", "err", ctx.Err())
			}
			telemetry.Finalize(ctx, nil)
			return
		}
	}
	telemetry.Finalize(ctx, nil)
}
