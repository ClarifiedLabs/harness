package ui

import (
	"context"
	"sync"
)

func forceExitRequested(exit <-chan struct{}) bool {
	select {
	case <-exit:
		return true
	default:
		return false
	}
}

// forceExitPreflight supplies a context for synchronous preflight work while
// preserving a force-exit notification for the caller. Finish must be called
// once the work returns so its watcher cannot consume a later active-run exit.
type forceExitPreflight struct {
	ctx    context.Context
	cancel context.CancelFunc
	exit   <-chan struct{}

	stop        chan struct{}
	stopped     chan struct{}
	interrupted chan struct{}

	finishOnce sync.Once
	forced     bool
}

func newForceExitPreflight(exit <-chan struct{}) *forceExitPreflight {
	ctx, cancel := context.WithCancel(context.Background())
	preflight := &forceExitPreflight{
		ctx:    ctx,
		cancel: cancel,
		exit:   exit,
	}
	if exit == nil {
		return preflight
	}
	preflight.stop = make(chan struct{})
	preflight.stopped = make(chan struct{})
	preflight.interrupted = make(chan struct{})
	go func() {
		defer close(preflight.stopped)
		select {
		case <-exit:
			close(preflight.interrupted)
			cancel()
		case <-preflight.stop:
		}
	}()
	return preflight
}

func (p *forceExitPreflight) Context() context.Context { return p.ctx }

// Finish stops the watcher and reports whether force-exit occurred during
// preflight. A force-exit that races watcher shutdown remains available through
// the original channel and is consumed here instead of being lost.
func (p *forceExitPreflight) Finish() bool {
	p.finishOnce.Do(func() {
		if p.stop != nil {
			close(p.stop)
			<-p.stopped
		}
		p.cancel()
		select {
		case <-p.interrupted:
			p.forced = true
		default:
			p.forced = forceExitRequested(p.exit)
		}
	})
	return p.forced
}
