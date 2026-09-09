package ui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/goal"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
)

func TestPromptExecutionFinalizationOrder(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		displayed  bool
		cancel     bool
		canRetry   bool
		printError bool
	}{
		{name: "success"},
		{name: "API error", err: &llm.APIError{Message: "failure"}, canRetry: true, printError: true},
		{name: "displayed API error", err: &llm.APIError{Message: "failure"}, displayed: true, canRetry: true},
		{name: "other error", err: errors.New("failure"), printError: true},
		{name: "cancellation", err: context.Canceled, cancel: true},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			app := newTestApp(t, &out, &errw, llmtest.New("fake"))
			defer app.Renderer.StopProgress()
			app.Agent.SetTranscript([]llm.Message{uiUserMsg("task")})
			app.PromptNumber = 1
			var order []string
			app.Interrupt = agent.NewInterruptWatcher(nil, nil, func() { order = append(order, "idle") })
			var runContext context.Context
			app.OnPromptFinished = func() {
				order = append(order, "finished")
				if !errors.Is(runContext.Err(), context.Canceled) {
					t.Error("completion callback ran before context cleanup")
				}
				loaded, err := session.Load(app.SessionPath)
				if err != nil || loaded.System != "saved after onEnd" {
					t.Fatalf("completion callback ran before save: system=%q err=%v", loaded.System, err)
				}
				app.Interrupt.InterruptPrompt() // EndPrompt must already have made this an idle interrupt.
			}
			run := app.preparePromptExecution(context.Background(), 1, []string{"request context"}, func(ctx context.Context, sink *accumulatingSink) error {
				order = append(order, "run")
				runContext = ctx
				sink.TextDelta("buffered assistant text")
				sink.terminalModelErrorDisplayed = test.displayed
				if test.cancel {
					app.Interrupt.CancelPrompt()
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatal("active prompt was not cancellable")
					}
				}
				return test.err
			}, func(ctx context.Context, err error) {
				order = append(order, "onEnd")
				if ctx != runContext || err != test.err || app.apiContinuationAvailable() != test.canRetry {
					t.Fatal("onEnd did not receive the finalized run state")
				}
				raw, readErr := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
				if readErr != nil || !bytes.Contains(raw, []byte("buffered assistant text")) {
					t.Fatalf("onEnd ran before buffered events flushed: %s, %v", raw, readErr)
				}
				if _, statErr := os.Stat(filepath.Join(app.SessionPath, "state.json")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("state saved before onEnd: %v", statErr)
				}
				app.System = "saved after onEnd"
			})
			run()
			if want := []string{"run", "onEnd", "finished", "idle"}; !reflect.DeepEqual(order, want) {
				t.Fatalf("lifecycle order = %v, want %v", order, want)
			}
			if printed := strings.Contains(errw.String(), "[error:"); printed != test.printError {
				t.Fatalf("error printed = %v, want %v: %s", printed, test.printError, errw.String())
			}
		})
	}
}

func TestPromptExecutionEntryPointGoalPolicies(t *testing.T) {
	for _, kind := range []string{"prompt", "steered", "detached wait", "API continuation"} {
		for _, cancel := range []bool{false, true} {
			name := kind + "/success"
			if cancel {
				name = kind + "/cancelled"
			}
			t.Run(name, func(t *testing.T) {
				var out, errw bytes.Buffer
				fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("answer")}, Stop: llm.StopEndTurn})
				app := newTestAppWithGoal(t, &out, &errw, fp)
				defer app.Renderer.StopProgress()
				if err := app.Goal.Set("finish the task"); err != nil {
					t.Fatal(err)
				}
				app.Goal.BumpContinuations()
				before := app.Goal.Snapshot()
				app.lastPromptInterrupted = !cancel // Detect both setting and clearing by host-created turns.
				app.Interrupt = agent.NewInterruptWatcher(nil, nil, func() {})
				finished := 0
				app.OnPromptFinished = func() { finished++ }
				var run func()
				var ok bool
				switch kind {
				case "prompt":
					run, ok = app.preparePromptRun("task", promptOptions{})
				case "steered":
					run, ok = app.prepareSteeredPrompt(agent.SteerInput{Text: "task"})
				case "detached wait":
					run, ok = app.prepareDetachedWaitContinuation()
				case "API continuation":
					app.Agent.SetTranscript([]llm.Message{uiUserMsg("task")})
					app.finishPromptRun(&llm.APIError{Message: "retry"}, nil)
					run, ok = app.prepareAPIContinuation()
				}
				if !ok {
					t.Fatal("prompt preparation rejected")
				}
				if cancel {
					app.Interrupt.CancelPrompt()
				}
				run()
				if finished != 1 {
					t.Fatalf("completion callbacks = %d, want 1", finished)
				}
				hostTurn := kind == "detached wait" || kind == "API continuation"
				wantInterrupted := cancel
				if hostTurn {
					wantInterrupted = !cancel
				} else if cancel {
					before.Status = goal.StatusPaused
				}
				if got := app.Goal.Snapshot(); !reflect.DeepEqual(got, before) || app.lastPromptInterrupted != wantInterrupted {
					t.Fatalf("goal=%+v interrupted=%v, want %+v/%v", got, app.lastPromptInterrupted, before, wantInterrupted)
				}
				loaded, err := session.Load(app.SessionPath)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Goal == nil || loaded.Goal.Objective != before.Objective || loaded.Goal.Status != before.Status || loaded.Goal.Continuations != before.Continuations || !loaded.Goal.SetAt.Equal(before.SetAt) {
					t.Fatalf("saved goal=%+v, want %+v", loaded.Goal, before)
				}
				if err := llm.ValidateTranscript(loaded.Messages); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
