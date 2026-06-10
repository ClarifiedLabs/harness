package agent

import (
	"os"
	"sync"
	"time"
)

// doublePressWindow is the interval within which a second ^C after a turn-cancel
// is treated as an exit request rather than another cancel (design §8.4).
const doublePressWindow = time.Second

// InterruptWatcher is the SIGINT state machine (design §8.4). A single handler
// drives both behaviors via a per-turn cancel func:
//
//   - First ^C during a turn cancels the turn (aborting the stream and any
//     run_command process group).
//   - A second ^C within doublePressWindow, or any ^C at the idle prompt,
//     requests exit.
//
// The signal channel and clock are injected so the state machine is unit-tested
// without real signals or sleeps. Actual save+exit wiring lives in Phase 10's
// main; this watcher only invokes the cancel func and the requestExit callback.
type InterruptWatcher struct {
	sig         <-chan os.Signal
	now         func() time.Time
	requestExit func()

	mu         sync.Mutex
	inTurn     bool
	cancel     func()
	lastCancel time.Time
	cancelled  bool // a cancel already fired for the current turn
}

// NewInterruptWatcher builds a watcher reading signals from sig, reading time
// from now, and calling requestExit when an exit is warranted.
func NewInterruptWatcher(sig <-chan os.Signal, now func() time.Time, requestExit func()) *InterruptWatcher {
	if now == nil {
		now = time.Now
	}
	return &InterruptWatcher{sig: sig, now: now, requestExit: requestExit}
}

// Start launches the watcher goroutine and returns a stop function that ends it.
func (w *InterruptWatcher) Start() (stop func()) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case _, ok := <-w.sig:
				if !ok {
					return
				}
				w.handle()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// BeginTurn marks a turn active and registers its cancel func. Called by the
// turn loop before streaming.
func (w *InterruptWatcher) BeginTurn(cancel func()) {
	w.mu.Lock()
	w.inTurn = true
	w.cancel = cancel
	w.cancelled = false
	w.mu.Unlock()
}

// EndTurn marks the prompt idle again. Called by the turn loop when the turn
// completes (normally or via cancel).
func (w *InterruptWatcher) EndTurn() {
	w.mu.Lock()
	w.inTurn = false
	w.cancel = nil
	w.mu.Unlock()
}

// CancelTurn cancels the active turn without requesting process exit. It is
// used by non-signal interrupt gestures such as Esc-Esc, which should behave
// like the first ^C during a turn but never like the second ^C exit shortcut.
func (w *InterruptWatcher) CancelTurn() {
	w.mu.Lock()
	if !w.inTurn {
		w.mu.Unlock()
		return
	}
	cancel := w.cancel
	w.cancelled = true
	w.lastCancel = w.now()
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// handle applies one signal to the state machine.
func (w *InterruptWatcher) handle() {
	w.mu.Lock()

	// Idle prompt: any ^C requests exit.
	if !w.inTurn {
		w.mu.Unlock()
		w.requestExit()
		return
	}

	// A second ^C within the window after a cancel requests exit.
	if w.cancelled && w.now().Sub(w.lastCancel) <= doublePressWindow {
		w.mu.Unlock()
		w.requestExit()
		return
	}

	// First ^C of this turn (or beyond the window): cancel the turn.
	cancel := w.cancel
	w.cancelled = true
	w.lastCancel = w.now()
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
