package agent

import (
	"os"
	"sync"
	"time"
)

// InterruptWatcher is the SIGINT state machine (design §8.4). A single handler
// drives both behaviors via a per-prompt cancel func:
//
//   - First ^C during a prompt cancels the prompt (aborting the stream and any
//     shell process group).
//   - A second ^C while that prompt remains active, or any ^C at the idle
//     prompt, requests exit.
//
// The signal channel and clock are injected so the state machine is unit-tested
// without real signals or sleeps. Actual save+exit wiring lives in Phase 10's
// main; this watcher only invokes the cancel func and the requestExit callback.
type InterruptWatcher struct {
	sig         <-chan os.Signal
	requestExit func()

	mu        sync.Mutex
	inPrompt  bool
	cancel    func()
	cancelled bool // a cancel already fired for the current prompt
}

// NewInterruptWatcher builds a watcher reading signals from sig and calling
// requestExit when an exit is warranted. The clock argument remains injectable
// for callers that share construction with other prompt timing code.
func NewInterruptWatcher(sig <-chan os.Signal, _ func() time.Time, requestExit func()) *InterruptWatcher {
	return &InterruptWatcher{sig: sig, requestExit: requestExit}
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

// BeginPrompt marks a prompt active and registers its cancel func. Called by the
// prompt loop before streaming.
func (w *InterruptWatcher) BeginPrompt(cancel func()) {
	w.mu.Lock()
	w.inPrompt = true
	w.cancel = cancel
	w.cancelled = false
	w.mu.Unlock()
}

// EndPrompt marks the prompt idle again. Called by the prompt loop when the prompt
// completes (normally or via cancel).
func (w *InterruptWatcher) EndPrompt() {
	w.mu.Lock()
	w.inPrompt = false
	w.cancel = nil
	w.mu.Unlock()
}

// CancelPrompt cancels the active prompt without requesting process exit. It is
// used by non-signal interrupt gestures such as Esc-Esc, which should behave
// like the first ^C during a prompt but never like the second ^C exit shortcut.
func (w *InterruptWatcher) CancelPrompt() {
	w.mu.Lock()
	if !w.inPrompt {
		w.mu.Unlock()
		return
	}
	cancel := w.cancel
	w.cancelled = true
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// InterruptPrompt applies the same two-stage behavior as a SIGINT to an
// interrupt key decoded by the raw prompt editor.
func (w *InterruptWatcher) InterruptPrompt() {
	w.handle()
}

// handle applies one signal to the state machine.
func (w *InterruptWatcher) handle() {
	w.mu.Lock()

	// Idle prompt: any ^C requests exit.
	if !w.inPrompt {
		w.mu.Unlock()
		w.requestExit()
		return
	}

	// Once cancellation has been requested, another ^C force-exits whenever the
	// same prompt is still active. Providers that ignore context cancellation
	// must not trap the user behind a timing window.
	if w.cancelled {
		w.mu.Unlock()
		w.requestExit()
		return
	}

	// First ^C of this prompt: cancel the prompt.
	cancel := w.cancel
	w.cancelled = true
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
