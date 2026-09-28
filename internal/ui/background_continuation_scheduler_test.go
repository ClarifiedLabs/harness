package ui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestREPLBackgroundCompletionBetweenIdleCheckAndSubscriptionAutoContinues(t *testing.T) {
	var out, errw lockedBuffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("continued after raced completion")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "ready> "
	app.BackgroundAutoContinue = true
	manager := background.NewManager(background.Options{})
	app.Background = manager
	releaseJob, jobDone := backgroundSchedulerJob(t, manager, "result from idle admission race")

	// This hook runs after idle admission checked CompletedContextPending. The
	// job must finish before the hook returns, so a Changed subscription taken
	// afterward observes the replacement channel and loses the only wakeup.
	app.IdleCompactionAfter = time.Hour
	never := make(chan time.Time)
	app.idleCompactionAfter = func(time.Duration) <-chan time.Time {
		releaseJob()
		<-jobDone
		return never
	}
	finished := make(chan struct{})
	app.OnPromptFinished = sync.OnceFunc(func() { close(finished) })
	_, stop := runBackgroundSchedulerREPL(t, app)
	waitBackgroundSchedulerSignal(t, finished, "background continuation after idle admission race")
	stop()

	if got := fp.RequestCount(); got != 1 {
		t.Fatalf("model requests = %d, want one background continuation", got)
	}
	if got := strings.Join(fp.Requests[0].RequestContext, "\n"); !strings.Contains(got, "result from idle admission race") {
		t.Fatalf("continuation request context = %q, want raced background result", got)
	}
	if got := transcriptPrompts(app); got != backgroundJobCompletedContinuation {
		t.Fatalf("transcript prompts = %q, want background completion continuation", got)
	}
}

func TestREPLBackgroundCompletionAfterHostCancellationDoesNotContinue(t *testing.T) {
	for _, runner := range []string{"api_continue", "detached_wait"} {
		t.Run(runner, func(t *testing.T) {
			var out lockedBuffer
			armed := make(chan struct{})
			idle := make(chan struct{})
			releaseIdle := make(chan struct{})
			unblockIdle := sync.OnceFunc(func() { close(releaseIdle) })
			defer unblockIdle()
			errw := &backgroundSchedulerPromptGate{
				armed: armed,
				gate: gatedBuffer{
					needle: "ready> ", reached: idle, release: releaseIdle,
				},
			}
			started := make(chan struct{})
			unexpected := make(chan struct{})
			steps := []llmtest.Step{
				{
					Stop: llm.StopEndTurn,
					Block: func(ctx context.Context) {
						close(started)
						<-ctx.Done()
					},
				},
				{
					Stop:  llm.StopEndTurn,
					Block: func(context.Context) { close(unexpected) },
				},
			}
			wantRequests := 1
			if runner == "api_continue" {
				steps = append([]llmtest.Step{{Err: &llm.APIError{StatusCode: 401, Message: "unauthorized"}}}, steps...)
				wantRequests = 2
			}
			fp := llmtest.New("fake", steps...)
			app := newTestApp(t, &out, errw, fp)
			app.Prompt = "ready> "
			app.BackgroundAutoContinue = true
			app.Interrupt = agent.NewInterruptWatcher(nil, time.Now, func() {})
			manager := background.NewManager(background.Options{})
			app.Background = manager
			releaseJob, jobDone := backgroundSchedulerJob(t, manager, "ordinary result pending at cancellation")
			if runner == "detached_wait" {
				prepareBackgroundSchedulerDetachedWait(t, manager)
			}
			finished := make(chan struct{}, 3)
			app.OnPromptFinished = func() { finished <- struct{}{} }
			writer, stop := runBackgroundSchedulerREPL(t, app)
			if runner == "api_continue" {
				writePipe(t, writer, "initial request\n")
				waitBackgroundSchedulerSignal(t, finished, "initial non-retryable API failure")
				writePipe(t, writer, "/continue\n")
			}
			waitBackgroundSchedulerSignal(t, started, "host continuation request")

			// Complete ordinary work only after the continuation has already drained
			// its request context, but before canceling it. This leaves a pending
			// result for the scheduler's post-prompt admission check.
			releaseJob()
			waitBackgroundSchedulerSignal(t, jobDone, "ordinary background job completion")
			if !manager.CompletedContextPending() {
				t.Fatal("ordinary completion was not pending before cancellation")
			}
			close(armed)
			app.Interrupt.CancelPrompt()
			waitBackgroundSchedulerSignal(t, idle, "settled idle prompt after cancellation")

			// The REPL is blocked while drawing its next idle prompt, after all
			// autonomous admission checks. No timing-based negative wait is needed.
			select {
			case <-unexpected:
				t.Error("canceled host continuation started a background continuation")
			default:
			}
			if got := fp.RequestCount(); got != wantRequests {
				t.Errorf("model requests = %d, want %d before returning control to the user", got, wantRequests)
			}
			if !manager.CompletedContextPending() {
				t.Error("cancellation consumed the pending ordinary background result")
			}
			unblockIdle()
			stop()
			if !app.lastPromptInterrupted {
				t.Error("host continuation cancellation did not mark the prompt interrupted")
			}
		})
	}
}

// backgroundSchedulerPromptGate arms the existing output gate only after the
// request to cancel has started; earlier idle prompts must not satisfy the test.
// Its separate buffer preserves all output without blocking String on the gate.
type backgroundSchedulerPromptGate struct {
	lockedBuffer
	armed <-chan struct{}
	gate  gatedBuffer
}

func (w *backgroundSchedulerPromptGate) Write(p []byte) (int, error) {
	select {
	case <-w.armed:
		if _, err := w.gate.Write(p); err != nil {
			return 0, err
		}
	default:
	}
	return w.lockedBuffer.Write(p)
}

func backgroundSchedulerJob(t *testing.T, manager *background.Manager, text string) (func(), <-chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	job, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			select {
			case <-release:
				return tools.BackgroundJobResult{Text: text}, nil
			case <-ctx.Done():
				return tools.BackgroundJobResult{}, ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	done, ok := manager.JobDone(job.ID)
	if !ok {
		t.Fatalf("missing background job %q", job.ID)
	}
	t.Cleanup(func() {
		unblock()
		waitBackgroundSchedulerSignal(t, done, "background worker cleanup")
	})
	return unblock, done
}

func prepareBackgroundSchedulerDetachedWait(t *testing.T, manager *background.Manager) {
	t.Helper()
	release, done := backgroundSchedulerJob(t, manager, "detached wait result")
	jobs := manager.List()
	id := jobs[len(jobs)-1].ID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	returned := make(chan struct{})
	var result background.WaitResult
	var err error
	go func() {
		result, err = manager.Wait(&waitEntryContext{Context: ctx, entered: entered}, id, time.Hour)
		close(returned)
	}()
	defer func() {
		cancel()
		waitBackgroundSchedulerSignal(t, returned, "detached wait cleanup")
	}()
	waitBackgroundSchedulerSignal(t, entered, "background wait registration")
	manager.NotifyAcceptedSteer()
	waitBackgroundSchedulerSignal(t, returned, "detached background wait")
	if err != nil || !result.Detached {
		t.Fatalf("background wait = %+v, %v; want detached", result, err)
	}
	ready := manager.DetachedWaitReady()
	release()
	waitBackgroundSchedulerSignal(t, done, "detached background job completion")
	waitBackgroundSchedulerSignal(t, ready, "detached wait outcome publication")
}

func runBackgroundSchedulerREPL(t *testing.T, app *App) (*io.PipeWriter, func()) {
	t.Helper()
	reader, writer := io.Pipe()
	done := make(chan struct{})
	var code int
	go func() {
		code = run(reader, app, nil, false)
		close(done)
	}()
	t.Cleanup(func() {
		if app.Interrupt != nil {
			app.Interrupt.CancelPrompt()
		}
		writer.Close()
		reader.Close()
		waitBackgroundSchedulerSignal(t, done, "REPL cleanup")
	})
	return writer, func() {
		writePipe(t, writer, "/exit\n")
		if err := writer.Close(); err != nil {
			t.Fatalf("close REPL input: %v", err)
		}
		waitBackgroundSchedulerSignal(t, done, "REPL exit")
		if code != ExitOK {
			t.Fatalf("REPL exit code = %d, want %d", code, ExitOK)
		}
	}
}

func waitBackgroundSchedulerSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
