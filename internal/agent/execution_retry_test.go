package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func (r *executionRecorder) retryWaits() []execution.ModelEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []execution.ModelEvent
	for _, e := range r.models {
		if e.Phase == execution.ModelRetry {
			events = append(events, e)
		}
	}
	return events
}
func assertExecutionRetry(t *testing.T, e execution.ModelEvent, purpose llm.RequestPurpose, planned time.Duration, reason llm.AttemptErrorClass, outcome llm.AttemptOutcome) {
	t.Helper()
	a := e.Attempt
	if e.Usage != (llm.Usage{}) || a.Usage != nil || a.Sequence != 0 || a.TTFT != nil {
		t.Fatalf("retry observation became a request/billing event: %+v", e)
	}
	if a.Phase != llm.AttemptRetryWait || a.Purpose != purpose || a.Cause != llm.AttemptRetry || a.RetryLayer != llm.RetryLayerAgent || a.ErrorClass != reason || a.Outcome != outcome || a.RetryDelay == nil || *a.RetryDelay != planned || a.Duration == nil || *a.Duration < 0 {
		t.Fatalf("retry observation = %+v", a)
	}
}

func TestExecutionRetryWaitSurvivesClosedProviderCall(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cancelled"}[cancelWait], func(t *testing.T) {
			r := &executionRecorder{}
			p := llmtest.New("fake", executionFailure(11, errors.New("stream interrupted")), summaryStep("kept", 13, 1))
			a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan time.Duration, 1)
			a.sleep = func(ctx context.Context, delay time.Duration) error {
				entered <- delay
				if cancelWait {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			done := make(chan error, 1)
			go func() { done <- a.RunPrompt(ctx, "go", &recordSink{}) }()
			planned := <-entered
			if planned <= 0 {
				t.Fatalf("planned delay=%v", planned)
			}
			if cancelWait {
				cancel()
			}
			err := <-done
			if cancelWait && !errors.Is(err, context.Canceled) || !cancelWait && err != nil {
				t.Fatalf("run error=%v", err)
			}
			events := r.retryWaits()
			if len(events) != 1 {
				t.Fatalf("retry waits=%+v", events)
			}
			outcome := llm.AttemptSucceeded
			requests := 2
			if cancelWait {
				outcome = llm.AttemptCancelled
				requests = 1
			}
			assertExecutionRetry(t, events[0], llm.RequestPurposeTurn, planned, llm.AttemptErrorUnknown, outcome)
			if p.RequestCount() != requests {
				t.Fatalf("requests=%d want=%d", p.RequestCount(), requests)
			}
			starts, finishes := 0, 0
			for _, e := range r.models {
				if e.Phase == execution.ModelStart {
					starts++
				}
				if e.Phase == execution.ModelFinish {
					finishes++
				}
			}
			if starts != requests || finishes != requests {
				t.Fatalf("wait manufactured requests: starts=%d finishes=%d", starts, finishes)
			}
			assertExecutionDiscard(t, r, map[string]int{"stream_retry": 11})
		})
	}
}

func TestExecutionImmediateCompatibilityRetryAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cancelled"}[cancelled], func(t *testing.T) {
			r := &executionRecorder{}
			p := llmtest.New("fake", executionFailure(3, &llm.APIError{StatusCode: 400, Code: "previous_response_id"}), summaryStep("done", 5, 1))
			a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
			coordinator := newTurnAttemptCoordinator(a, &recordSink{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := llm.Request{Purpose: llm.RequestPurposeTurn}
			previous, err := coordinator.request(ctx, req, ContextEstimate{})
			if err == nil {
				t.Fatal("expected compatibility rejection")
			}
			if cancelled {
				cancel()
			}
			_, err = coordinator.rerun(ctx, previous, req, ContextEstimate{})
			if cancelled && !errors.Is(err, context.Canceled) || !cancelled && err != nil {
				t.Fatalf("rerun=%v", err)
			}
			waits := r.retryWaits()
			if len(waits) != 1 {
				t.Fatalf("waits=%+v", waits)
			}
			outcome := llm.AttemptSucceeded
			requests := 2
			if cancelled {
				outcome = llm.AttemptCancelled
				requests = 1
			}
			assertExecutionRetry(t, waits[0], llm.RequestPurposeTurn, 0, llm.AttemptErrorRequest, outcome)
			if p.RequestCount() != requests {
				t.Fatalf("requests=%d", p.RequestCount())
			}
		})
	}
}

func TestExecutionSummaryRetryWaitAndBudgetTransition(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", executionFailure(3, &llm.APIError{StatusCode: 503, Retryable: true}), llmtest.Step{Events: []llm.StreamEvent{textDelta("short")}, Usage: llm.Usage{InputTokens: 5}, Stop: llm.StopMaxTokens}, summaryStep("accepted", 7, 1))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	var planned time.Duration
	a.sleep = func(_ context.Context, delay time.Duration) error { planned = delay; return nil }
	text, u, err := a.GenerateBranchSummary(context.Background(), makeTurns(2), "")
	if err != nil || text != "accepted" || u.InputTokens != 15 {
		t.Fatalf("summary=%q %+v %v", text, u, err)
	}
	waits := r.retryWaits()
	if len(waits) != 2 {
		t.Fatalf("waits=%+v", waits)
	}
	assertExecutionRetry(t, waits[0], llm.RequestPurposeBranchSummary, planned, llm.AttemptErrorServer, llm.AttemptSucceeded)
	assertExecutionRetry(t, waits[1], llm.RequestPurposeBranchSummary, 0, llm.AttemptErrorRequest, llm.AttemptSucceeded)
	if p.RequestCount() != 3 {
		t.Fatalf("requests=%d", p.RequestCount())
	}
}

func TestExecutionDiscardIdleCompactionAPI(t *testing.T) {
	for _, disposition := range []string{"unused", "applied", "archive_error", "stale"} {
		t.Run(disposition, func(t *testing.T) {
			old, next := &executionRecorder{}, &executionRecorder{}
			a := newAgent(llmtest.New("fake", executionFailure(3, errors.New("retry")), summaryStep("prepared", 7, 1)), tools.Default(), Options{Execution: old.scope("old"), Model: "old", ContextWindow: 10000})
			executionNoSleep(a)
			a.SetTranscript(makeTurns(10))
			work, ok, err := a.PrepareIdleCompaction(1)
			if err != nil || !ok {
				t.Fatalf("prepare=%t %v", ok, err)
			}
			result, err := work(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch disposition {
			case "applied":
				if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err != nil || !applied {
					t.Fatalf("apply=%t %v", applied, err)
				}
			case "archive_error":
				a.SetCompactionArchiver(func(context.Context, CompactionArchive) (string, error) { return "", errors.New("unavailable") })
				if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err == nil || applied {
					t.Fatalf("apply=%t %v", applied, err)
				}
			case "stale":
				a.SetModel("changed", 10000)
				if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err != nil || applied {
					t.Fatalf("apply=%t %v", applied, err)
				}
			}
			a.SetExecution(next.scope("next"))
			before := cloneMessages(a.Transcript())
			count := a.CompactionCount()
			a.DiscardIdleCompaction(result)
			a.DiscardIdleCompaction(result) // result copies share the terminal disposition.
			a.DiscardIdleCompaction(IdleCompactionResult{})
			if !reflect.DeepEqual(before, a.Transcript()) || count != a.CompactionCount() {
				t.Fatal("disposal mutated transcript")
			}
			if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err != nil || applied {
				t.Fatalf("consumed candidate applied: %t %v", applied, err)
			}
			want := map[string]int{"summary_retry": 3}
			if disposition != "applied" {
				want["stale_idle"] = 7
			}
			assertExecutionDiscard(t, old, want)
			if len(next.models) != 0 || len(next.works) != 0 {
				t.Fatal("disposal used live rather than captured scope")
			}
			terminal := 0
			for _, e := range old.works {
				if e.Phase == execution.WorkResult && (e.Outcome == "discarded" || e.Outcome == "stale" || e.Outcome == "applied") {
					terminal++
					if e.Outcome == "discarded" && (e.ContextBefore != e.ContextAfter || e.Compactions != 0 || e.Count != 0) {
						t.Fatalf("disposal reclaimed context or billed work: %+v", e)
					}
				}
			}
			if terminal != 1 {
				t.Fatalf("terminal dispositions=%d", terminal)
			}
		})
	}
}

func TestExecutionDiscardIdleUsesResultScopeAfterAgentReplacement(t *testing.T) {
	r := &executionRecorder{}
	a := newAgent(llmtest.New("fake", summaryStep("prepared", 7, 1)), tools.Default(), Options{Execution: r.scope("m"), ContextWindow: 10000})
	a.SetTranscript(makeTurns(10))
	work, ok, err := a.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("prepare=%t %v", ok, err)
	}
	result, err := work(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := newAgent(llmtest.New("fake"), tools.Default(), Options{})
	other.DiscardIdleCompaction(result)
	assertExecutionDiscard(t, r, map[string]int{"stale_idle": 7})
	if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err != nil || applied {
		t.Fatalf("disposed candidate remained applicable: %t %v", applied, err)
	}
}

func TestExecutionDiscardIdleCompletedResultIsAgentIndependent(t *testing.T) {
	r := &executionRecorder{}
	a := newAgent(llmtest.New("fake", summaryStep("prepared", 7, 1)), tools.Default(), Options{Execution: r.scope("old"), ContextWindow: 10000})
	a.SetTranscript(makeTurns(10))
	work, ok, err := a.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("prepare=%t %v", ok, err)
	}
	result, err := work(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := cloneMessages(a.Transcript())
	done := make(chan struct{})
	go func() {
		// Completed-result exclusive ownership needs no live Agent at all. This is
		// the same independent operation used by shutdown/late-result drainers.
		var noLiveAgent *Agent
		noLiveAgent.DiscardIdleCompaction(result)
		close(done)
	}()
	<-done
	assertExecutionDiscard(t, r, map[string]int{"stale_idle": 7})
	if !reflect.DeepEqual(before, a.Transcript()) {
		t.Fatal("off-owner disposal mutated Agent")
	}
}
