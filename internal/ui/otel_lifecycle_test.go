package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/otel"
	"harness/internal/runstream"
)

func waitOTelSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: " + label)
	}
}

func assertOTelOwnerBusy(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := app.WaitOTel(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitOTel while owner active = %v, want deadline exceeded", err)
	}
}

func waitOTelOwners(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.WaitOTel(ctx); err != nil {
		t.Fatal(err)
	}
}

// These exercise the real Sink and group, including the interval after the
// provider finishes but PromptComplete is still updating UI/session state.
func TestOTelForceExitWaitsForWholeForegroundOwner(t *testing.T) {
	for _, mode := range []string{"oneshot", "repl", "json"} {
		t.Run(mode, func(t *testing.T) {
			started, releaseProvider := make(chan struct{}), make(chan struct{})
			reachedTail, releaseTail := make(chan struct{}), make(chan struct{})
			var releaseProviderOnce, releaseTailOnce sync.Once
			releaseAll := func() {
				releaseProviderOnce.Do(func() { close(releaseProvider) })
				releaseTailOnce.Do(func() { close(releaseTail) })
			}
			t.Cleanup(releaseAll)
			fp := llmtest.New("anthropic", llmtest.Step{Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 11}, Block: func(context.Context) { close(started); <-releaseProvider }})
			var out lockedBuffer
			errw := &gatedBuffer{needle: "[prompt:", reached: reachedTail, release: releaseTail}
			app := newTestApp(t, &out, errw, fp)
			exp := testUIExporter(t, app)
			force := make(chan struct{})
			app.ForceExit = force
			if mode == "json" {
				app.RunStream = runstream.NewWriter(io.Discard, runstream.RunStart{}, io.Discard)
				defer app.RunStream.Close(runstream.RunEnd{})
			}
			done := make(chan int, 1)
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			go func() {
				switch mode {
				case "oneshot":
					done <- OneShot(app, "go")
				case "repl":
					done <- RunWithInitialPrompt(pr, app, force, "go")
				case "json":
					done <- RunJSON(pr, app)
				}
			}()
			if mode == "json" {
				writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"go\"}\n")
			}
			waitOTelSignal(t, started, "provider start")
			close(force)
			select {
			case code := <-done:
				if code != ExitInterrupt {
					t.Fatalf("exit = %d", code)
				}
			case <-time.After(time.Second):
				t.Fatal("force exit waited for provider")
			}
			assertOTelOwnerBusy(t, app)
			// Root cleanup must skip aggregates on a failed join, not read App/Agent.
			if got := uiMetricTotal(t, exp, "harness.session.total"); got != 0 {
				t.Fatalf("premature session aggregate = %v", got)
			}
			releaseProviderOnce.Do(func() { close(releaseProvider) })
			waitOTelSignal(t, reachedTail, "post-provider prompt accounting")
			assertOTelOwnerBusy(t, app)
			releaseTailOnce.Do(func() { close(releaseTail) })
			waitOTelOwners(t, app)
			app.RecordOTelSession()
			if got := uiMetricTotal(t, exp, "harness.session.total"); got != 1 {
				t.Fatalf("joined session aggregate = %v", got)
			}
			if got := app.usage.InputTokens; got != 11 {
				t.Fatalf("final usage = %d, want 11", got)
			}
			if err := llm.ValidateTranscript(app.Agent.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOTelREPLTracksPostRunOwnerMutation(t *testing.T) {
	started, releaseProvider := make(chan struct{}), make(chan struct{})
	reachedTail, releaseTail := make(chan struct{}), make(chan struct{})
	var providerOnce, tailOnce sync.Once
	t.Cleanup(func() { providerOnce.Do(func() { close(releaseProvider) }); tailOnce.Do(func() { close(releaseTail) }) })
	var out, errw lockedBuffer
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", llmtest.Step{Stop: llm.StopEndTurn, Block: func(context.Context) { close(started); <-releaseProvider }}))
	testUIExporter(t, app)
	force := make(chan struct{})
	app.ForceExit = force
	app.OnPromptFinished = func() {
		close(reachedTail)
		<-releaseTail
		app.QueueMaintenanceUsageForModel("late-owner", agent.MaintenanceUsage{Purpose: "prewarm", Usage: llm.Usage{InputTokens: 19}})
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	done := make(chan int, 1)
	go func() { done <- RunWithInitialPrompt(pr, app, force, "go") }()
	waitOTelSignal(t, started, "provider start")
	close(force)
	select {
	case code := <-done:
		if code != ExitInterrupt {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("force exit waited")
	}
	providerOnce.Do(func() { close(releaseProvider) })
	waitOTelSignal(t, reachedTail, "post-Agent.Run owner callback")
	assertOTelOwnerBusy(t, app)
	tailOnce.Do(func() { close(releaseTail) })
	waitOTelOwners(t, app)
	app.drainMaintenanceUsage()
	if got := app.usageByModel["late-owner"].InputTokens; got != 19 {
		t.Fatalf("owner tail mutation missing: %d", got)
	}
}

func TestOTelREPLTracksIdlePreparationAfterExit(t *testing.T) {
	var out, errw lockedBuffer
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("UNUSED SUMMARY")}, Stop: llm.StopEndTurn,
		Block: func(context.Context) { close(started); <-release },
	}))
	testUIExporter(t, app)
	app.Agent.SetModel(app.Model, 10000)
	app.Agent.SetTranscript(compactionSeed())
	app.IdleCompactionAfter = time.Minute
	app.IdleCompactionTriggerPercent = 1
	idle := make(chan time.Time, 1)
	app.idleCompactionAfter = func(time.Duration) <-chan time.Time { return idle }
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	done := make(chan int, 1)
	go func() { done <- Run(pr, app, nil) }()
	idle <- time.Now()
	waitOTelSignal(t, started, "idle preparation start")
	writePipe(t, pw, "/exit\n")
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("REPL exit waited for idle preparation")
	}
	assertOTelOwnerBusy(t, app)
	releaseOnce.Do(func() { close(release) })
	waitOTelOwners(t, app)
	if got := len(app.Agent.Transcript()); got != 20 {
		t.Fatalf("late idle work mutated transcript: %d", got)
	}
}

func TestOTelTracksDetachedPrewarmBeforeSpawnAndAfterModelReturn(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", llmtest.Step{Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 13}}))
	testUIExporter(t, app)
	warm, ok := app.Agent.PrewarmFunc()
	if !ok {
		t.Fatal("no prewarm")
	}
	start, modelDone, releaseQueue := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, queueOnce sync.Once
	t.Cleanup(func() { startOnce.Do(func() { close(start) }); queueOnce.Do(func() { close(releaseQueue) }) })
	modelKey := app.usageKey()
	finish := app.TrackOTel()
	// Admission occurs synchronously, even before the child gets scheduled.
	assertOTelOwnerBusy(t, app)
	go func() {
		defer finish()
		<-start
		result := warm(context.Background())
		close(modelDone)
		<-releaseQueue
		app.QueuePrewarmResultForModel(modelKey, result)
	}()
	startOnce.Do(func() { close(start) })
	waitOTelSignal(t, modelDone, "prewarm model completion")
	assertOTelOwnerBusy(t, app)
	queueOnce.Do(func() { close(releaseQueue) })
	waitOTelOwners(t, app)
	app.drainMaintenanceUsage()
	if app.usage.InputTokens != 13 {
		t.Fatalf("queued prewarm usage = %+v", app.usage)
	}
}

type gatedIdleDisposition struct {
	*otel.Sink
	entered chan struct{}
	release <-chan struct{}
}

func (g *gatedIdleDisposition) ObserveWork(event execution.WorkEvent) {
	g.Sink.ObserveWork(event)
	if event.Kind == execution.WorkCompaction && event.Phase == execution.WorkResult && event.Trigger == "idle" {
		close(g.entered)
		<-g.release
	}
}

func TestOTelTracksLateIdleDispositionBeforeCleanup(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", llmtest.Step{Events: []llm.StreamEvent{textDelta("IDLE SUMMARY")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 100, OutputTokens: 12}}))
	exp := testUIExporter(t, app)
	app.Agent.SetModel(app.Model, 10000)
	app.Agent.SetTranscript(compactionSeed())
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	scope := app.otelSink.Scope()
	scope.Observer = &gatedIdleDisposition{Sink: app.otelSink, entered: entered, release: release}
	app.Agent.SetExecution(scope)
	work, ok, err := app.Agent.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("PrepareIdleCompaction = %v, %v", ok, err)
	}
	result, err := work(context.Background())
	if err != nil || !result.Prepared {
		t.Fatalf("idle result = %+v, %v", result, err)
	}
	waitOTelOwners(t, app)
	done := make(chan idleCompactionFinished, 1)
	app.discardIdleCompactionWhenReady(done)
	assertOTelOwnerBusy(t, app)
	done <- idleCompactionFinished{result: result}
	waitOTelSignal(t, entered, "idle disposal observation")
	assertOTelOwnerBusy(t, app)
	releaseOnce.Do(func() { close(release) })
	waitOTelOwners(t, app)
	if got := uiMetricTotal(t, exp, "harness.model.discard.tokens"); got != 112 {
		t.Fatalf("discarded tokens = %v", got)
	}
	if got := len(app.Agent.Transcript()); got != 20 {
		t.Fatalf("disposal mutated live transcript: %d messages", got)
	}
}

func TestOTelSessionRecordingDoesNotRebindExecution(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", llmtest.Step{Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 7}}))
	exp := testUIExporter(t, app)
	// A caller may retain another execution scope. Terminal recording is not an
	// identity-switch path and must not install the App's Sink on the Agent.
	app.Agent.SetExecution(execution.Scope{})
	app.RecordOTelSession()
	if code := OneShot(app, "go"); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if got := uiMetricTotal(t, exp, "harness.prompt.total"); got != 0 {
		t.Fatalf("terminal recording rebound execution: %v prompts", got)
	}
}

func TestOTelLifecycleDisabledIsNoop(t *testing.T) {
	for _, app := range []*App{nil, {}} {
		done := app.TrackOTel()
		done()
		done()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := app.WaitOTel(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
