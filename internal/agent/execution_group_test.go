package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

type executionTrackingRecorder struct {
	*executionRecorder
	blockModel func(execution.ModelEvent)
	blockWork  func(execution.WorkEvent)
}

func (r *executionTrackingRecorder) ObserveModel(event execution.ModelEvent) {
	r.executionRecorder.ObserveModel(event)
	if r.blockModel != nil {
		r.blockModel(event)
	}
}
func (r *executionTrackingRecorder) ObserveWork(event execution.WorkEvent) {
	r.executionRecorder.ObserveWork(event)
	if r.blockWork != nil {
		r.blockWork(event)
	}
}

type executionTrackingSink struct {
	recordSink
	block func()
}

func (s *executionTrackingSink) PromptComplete(usage PromptUsage) {
	s.block()
	s.recordSink.PromptComplete(usage)
}

func assertExecutionGroupBusy(t *testing.T, group *execution.Group) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := group.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("group became idle before final owner observation: %v", err)
	}
}

func TestExecutionGroupTracksPromptThroughFinalOwnerCallback(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "group_only", true: "observed"}[observed], func(t *testing.T) {
			group := &execution.Group{}
			scope := execution.Scope{Group: group}
			if observed {
				scope.Observer = &executionRecorder{}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			sink := &executionTrackingSink{block: func() { close(entered); <-release }}
			a := newAgent(llmtest.New("fake", textStep("done")), tools.Default(), Options{Execution: scope})
			done := make(chan error, 1)
			go func() { done <- a.RunPrompt(context.Background(), "go", sink) }()
			<-entered // All model calls and the prompt observation already finished.
			assertExecutionGroupBusy(t, group)
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := group.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(sink.promptUsage) != 1 {
				t.Fatal("group completed before final prompt owner callback")
			}
		})
	}
}

func TestExecutionGroupBackgroundClosuresTrackCapturedScopeThroughDisposition(t *testing.T) {
	for _, mode := range []string{"prewarm", "idle"} {
		t.Run(mode, func(t *testing.T) {
			group, nextGroup := &execution.Group{}, &execution.Group{}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			block := func() { once.Do(func() { close(entered); <-release }) }
			r := &executionTrackingRecorder{executionRecorder: &executionRecorder{}}
			var work func(context.Context)
			var result IdleCompactionResult
			var workErr error
			var a *Agent
			scope := execution.Scope{Observer: r, Group: group}
			if mode == "prewarm" {
				r.blockModel = func(event execution.ModelEvent) {
					if event.Phase == execution.ModelDiscard {
						block() // ModelCall.Finish released the physical call already.
					}
				}
				a = newAgent(llmtest.New("fake", executionFailure(7, errors.New("warmup failed"))), tools.Default(), Options{Execution: scope})
				warm, ok := a.PrewarmFunc()
				if !ok {
					t.Fatal("prewarm unavailable")
				}
				work = func(ctx context.Context) { warm(ctx) }
			} else {
				r.blockWork = func(event execution.WorkEvent) {
					if event.Kind == execution.WorkCompaction && event.Phase == execution.WorkFinish {
						block() // Model calls finished; candidate ownership is not delivered.
					}
				}
				a = newAgent(llmtest.New("fake", summaryStep("prepared", 7, 1)), tools.Default(), Options{Execution: scope, ContextWindow: 10000})
				a.SetTranscript(makeTurns(10))
				idle, ok, err := a.PrepareIdleCompaction(1)
				if err != nil || !ok {
					t.Fatalf("idle prepare = %t %v", ok, err)
				}
				work = func(ctx context.Context) { result, workErr = idle(ctx) }
			}
			a.SetExecution(execution.Scope{Group: nextGroup})
			done := make(chan struct{})
			go func() { work(context.Background()); close(done) }()
			<-entered
			assertExecutionGroupBusy(t, group)
			if err := nextGroup.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			close(release)
			<-done
			if err := group.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "idle" {
				if workErr != nil || !result.Prepared {
					t.Fatalf("idle result = %+v %v", result, workErr)
				}
				a.DiscardIdleCompaction(result)
			}
		})
	}
}
