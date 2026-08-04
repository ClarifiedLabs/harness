package main

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	"harness/internal/agent"
	"harness/internal/ui"
)

// jsonRunInterrupts keeps one signal watcher alive across JSON startup and the
// active run. Its commit boundary decides whether a SIGINT is a startup failure
// or an active-run exit, so signals cannot be lost while swapping watchers.
type jsonRunInterrupts struct {
	ctx    context.Context
	cancel context.CancelFunc

	watcher  *agent.InterruptWatcher
	stop     func()
	exitCh   chan struct{}
	exitOnce sync.Once

	mu          sync.Mutex
	committed   bool
	interrupted bool
}

func newJSONRunInterrupts(sig <-chan os.Signal, now func() time.Time) *jsonRunInterrupts {
	ctx, cancel := context.WithCancel(context.Background())
	interrupts := &jsonRunInterrupts{
		ctx:    ctx,
		cancel: cancel,
		exitCh: make(chan struct{}),
	}
	if sig != nil {
		interrupts.watcher = agent.NewInterruptWatcher(sig, now, interrupts.requestExit)
		interrupts.stop = interrupts.watcher.Start()
	}
	return interrupts
}

func (i *jsonRunInterrupts) Context() context.Context { return i.ctx }

func (i *jsonRunInterrupts) StartupInterrupted() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.interrupted
}

func (i *jsonRunInterrupts) ExitCh() chan struct{} { return i.exitCh }

func (i *jsonRunInterrupts) Watcher() *agent.InterruptWatcher { return i.watcher }

// Commit marks run_start admission while holding the same lock used by SIGINT.
// start must enqueue run_start without blocking.
func (i *jsonRunInterrupts) Commit(start func()) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.interrupted {
		return false
	}
	i.committed = true
	start()
	return true
}

// StopStartup releases startup operations without stopping the signal watcher:
// after Commit, that watcher handles active prompt and exit behavior.
func (i *jsonRunInterrupts) StopStartup() { i.cancel() }

func (i *jsonRunInterrupts) Stop() {
	i.cancel()
	if i.stop != nil {
		i.stop()
	}
}

func (i *jsonRunInterrupts) requestExit() {
	i.mu.Lock()
	if !i.committed {
		i.interrupted = true
		i.mu.Unlock()
		i.cancel()
		return
	}
	i.mu.Unlock()
	i.exitOnce.Do(func() { close(i.exitCh) })
}

type startupPromptResult struct {
	prompt string
	err    error
}

// buildPromptWithStartupContext makes piped one-shot input interruptible while
// startup is still pending. The blocked reader may outlive this return, but the
// process exits immediately after a startup interruption and the buffered
// result channel prevents it from blocking when input eventually completes.
func buildPromptWithStartupContext(ctx context.Context, flagText string, stdin io.Reader, readStdin bool) (string, error) {
	if !readStdin {
		return ui.BuildPrompt(flagText, stdin, false)
	}
	resultCh := make(chan startupPromptResult, 1)
	go func() {
		prompt, err := ui.BuildPrompt(flagText, stdin, true)
		resultCh <- startupPromptResult{prompt: prompt, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.prompt, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
