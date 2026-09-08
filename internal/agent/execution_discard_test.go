package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func (r *executionRecorder) discarded() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, e := range r.models {
		if e.Phase == execution.ModelDiscard {
			out[string(e.Attempt.ErrorClass)] += e.Usage.InputTokens
		}
	}
	return out
}
func assertExecutionDiscard(t *testing.T, r *executionRecorder, want map[string]int) {
	t.Helper()
	got := r.discarded()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discarded=%v want=%v", got, want)
	}
	total := 0
	for _, n := range got {
		total += n
	}
	if total > r.usage().InputTokens {
		t.Fatalf("discarded %d exceeds billed %+v", total, r.usage())
	}
}
func executionFailure(n int, err error) llmtest.Step {
	return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: n}}}, Err: err}
}
func executionNoSleep(a *Agent) { a.sleep = func(context.Context, time.Duration) error { return nil } }

func TestExecutionDiscardsRetryAndTerminalFailureDisjointly(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", executionFailure(3, errors.New("retry one")), executionFailure(5, errors.New("retry two")), executionFailure(7, errors.New("terminal")))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	executionNoSleep(a)
	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err == nil {
		t.Fatal("expected error")
	}
	assertExecutionDiscard(t, r, map[string]int{"stream_retry": 8, "error": 7})
	if r.usage().InputTokens != 15 {
		t.Fatalf("billing changed: %+v", r.usage())
	}
	mustValid(t, a.Transcript())
}

func TestExecutionDiscardCancelledOutputOnlyWhenNotRetained(t *testing.T) {
	for _, text := range []string{"", "kept partial answer"} {
		t.Run(text, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &executionRecorder{}
			p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta(text), {Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 11}}}, Block: func(context.Context) { cancel() }})
			a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
			if err := a.RunPrompt(ctx, "go", &recordSink{}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			want := map[string]int{}
			if text == "" {
				want["error"] = 11
			}
			assertExecutionDiscard(t, r, want)
			mustValid(t, a.Transcript())
		})
	}
	t.Run("retry backoff cancellation", func(t *testing.T) {
		r := &executionRecorder{}
		a := newAgent(llmtest.New("fake", executionFailure(13, errors.New("retry"))), tools.Default(), Options{Execution: r.scope("m")})
		a.sleep = func(context.Context, time.Duration) error { return context.Canceled }
		if err := a.RunPrompt(context.Background(), "go", &recordSink{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		assertExecutionDiscard(t, r, map[string]int{"stream_retry": 13})
	})
}

func TestExecutionDiscardCompatibilityRetryOnlyOnce(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", executionFailure(3, errors.New("transport")), executionFailure(5, &llm.APIError{StatusCode: 400, Code: "previous_response_id"}), summaryStep("kept", 7, 1))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	executionNoSleep(a)
	coordinator := newTurnAttemptCoordinator(a, &recordSink{}, 1)
	req := llm.Request{Purpose: llm.RequestPurposeTurn}
	previous, err := coordinator.request(context.Background(), req, ContextEstimate{})
	if err == nil {
		t.Fatal("expected rejection")
	}
	if _, err := coordinator.rerun(context.Background(), previous, req, ContextEstimate{}); err != nil {
		t.Fatal(err)
	}
	coordinator.abandon(previous) // recorder metadata can be replayed; no second discard.
	assertExecutionDiscard(t, r, map[string]int{"stream_retry": 3, "request_rebuild": 5})
	if r.usage().InputTokens != 15 {
		t.Fatal("billing changed")
	}
}

func TestExecutionDiscardSummaryRetriesAndReplacementDisjointly(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", executionFailure(3, errors.New("retry")), llmtest.Step{Events: []llm.StreamEvent{textDelta("truncated")}, Usage: llm.Usage{InputTokens: 5}, Stop: llm.StopMaxTokens}, summaryStep("accepted", 7, 1))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	executionNoSleep(a)
	text, u, err := a.GenerateBranchSummary(context.Background(), makeTurns(2), "")
	if err != nil || text != "accepted" || u.InputTokens != 15 {
		t.Fatalf("summary=%q %+v %v", text, u, err)
	}
	assertExecutionDiscard(t, r, map[string]int{"summary_retry": 3, "summary_replaced": 5})
}

func TestExecutionDiscardMapReduceFailureIncludesSuccessfulMap(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", summaryStep("first map", 11, 1), executionFailure(13, &llm.APIError{StatusCode: 400}))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m"), ContextWindow: 10000})
	history := makeTurns(4)
	for i := 1; i < len(history); i += 2 {
		history[i] = asstText(strings.Repeat("details ", 3000))
	}
	_, u, err := a.GenerateBranchSummary(context.Background(), history, "")
	if err == nil || u.InputTokens != 24 {
		t.Fatalf("summary=%+v %v", u, err)
	}
	assertExecutionDiscard(t, r, map[string]int{"error": 24})
}

func TestExecutionDiscardCompactionBoundaryReplacement(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", summaryStep(strings.Repeat("large summary ", 5000), 11, 1), summaryStep("replacement", 13, 1))
	a := newAgent(p, &tools.Registry{}, Options{Execution: r.scope("m"), ContextWindow: 10000})
	a.SetTranscript(makeTurns(10))
	u, err := a.Compact(context.Background(), &recordSink{})
	if err != nil || len(p.Requests) != 2 || u.InputTokens != 24 {
		t.Fatalf("compaction=%+v %v requests=%d", u, err, len(p.Requests))
	}
	assertExecutionDiscard(t, r, map[string]int{"summary_replaced": 11})
	mustValid(t, a.Transcript())
}

func TestExecutionCompactionFallbackAndArchiveFailureDiscardLineages(t *testing.T) {
	for _, mode := range []string{"fallback", "archive_error", "native_fallback"} {
		t.Run(mode, func(t *testing.T) {
			r := &executionRecorder{}
			var p llm.Provider = llmtest.New("fake", executionFailure(3, errors.New("retry")), executionFailure(5, errors.New("retry")), executionFailure(7, errors.New("failed")))
			opts := Options{Execution: r.scope("m"), ContextWindow: 10000}
			if mode == "archive_error" {
				p = llmtest.New("fake", executionFailure(3, errors.New("retry")), summaryStep("accepted summary", 17, 1))
			}
			if mode == "native_fallback" {
				p = &nativeCompactionProvider{FakeProvider: llmtest.New("responses", summaryStep("text fallback", 19, 1)), result: llm.CompactedContext{Usage: llm.Usage{InputTokens: 23}}, err: llm.ErrContextCompactionUnsupported}
				opts.Model = "gpt-5.5"
				opts.NativeCompaction = true
				opts.ReasoningReplayDomain = "openai:gpt-5"
			}
			a := newAgent(p, tools.Default(), opts)
			executionNoSleep(a)
			a.SetTranscript(makeTurns(10))
			a.SetCompactionArchiver(func(context.Context, CompactionArchive) (string, error) {
				if mode == "archive_error" {
					return "", errors.New("archive failed")
				}
				return "archive", nil
			})
			_, err := a.Compact(context.Background(), &recordSink{})
			if (err != nil) != (mode == "archive_error") {
				t.Fatal(err)
			}
			work := r.finished(execution.WorkCompaction)
			if len(work) != 1 {
				t.Fatalf("work=%+v", work)
			}
			switch mode {
			case "fallback":
				assertExecutionDiscard(t, r, map[string]int{"summary_retry": 8, "compaction_fallback": 7})
				if work[0].Outcome != "fallback" || work[0].FallbackReason != compactionFallbackProviderError {
					t.Fatalf("work=%+v", work[0])
				}
			case "archive_error":
				assertExecutionDiscard(t, r, map[string]int{"summary_retry": 3, "error": 17})
			case "native_fallback":
				assertExecutionDiscard(t, r, map[string]int{"compaction_fallback": 23})
				if work[0].FallbackReason != "native" {
					t.Fatalf("work=%+v", work[0])
				}
			}
			mustValid(t, a.Transcript())
		})
	}
}

func TestExecutionIdleDispositionIsOnceAndOnlyAppliedReclaims(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "applied", true: "stale"}[stale], func(t *testing.T) {
			old, next := &executionRecorder{}, &executionRecorder{}
			p := llmtest.New("fake", executionFailure(3, errors.New("retry")), llmtest.Step{Events: []llm.StreamEvent{textDelta("truncated")}, Usage: llm.Usage{InputTokens: 5}, Stop: llm.StopMaxTokens}, summaryStep("idle summary", 7, 1))
			a := newAgent(p, &tools.Registry{}, Options{Execution: old.scope("old"), Model: "old", ContextWindow: 10000})
			a.SetSystem(strings.Repeat("system ", 100)) // cross the idle eligibility threshold
			executionNoSleep(a)
			a.SetTranscript(makeTurns(10))
			work, ok, err := a.PrepareIdleCompaction(1)
			if err != nil || !ok {
				t.Fatalf("prepare=%t %v", ok, err)
			}
			result, err := work(context.Background())
			if err != nil || !result.Prepared {
				t.Fatalf("result=%+v %v", result, err)
			}
			preparation := old.finished(execution.WorkCompaction)
			if len(preparation) != 1 || preparation[0].Outcome != "prepared" || preparation[0].ContextBefore != preparation[0].ContextAfter || preparation[0].Compactions != 0 {
				t.Fatalf("preparation reclaimed live context: %+v", preparation)
			}
			for _, e := range old.contexts {
				if e.Reason == "compaction" {
					t.Fatal("prepared context counted as applied")
				}
			}
			if stale {
				a.SetModel("next", 10000)
				a.SetExecution(next.scope("next"))
			}
			applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result)
			if err != nil || applied == stale {
				t.Fatalf("apply=%t %v", applied, err)
			}
			// A repeated delivery must neither re-discard nor turn an applied candidate
			// into a stale result just because applying it changed the fingerprint.
			_, _ = a.ApplyIdleCompaction(context.Background(), &recordSink{}, result)
			want := map[string]int{"summary_retry": 3, "summary_replaced": 5}
			if stale {
				want["stale_idle"] = 7
			}
			assertExecutionDiscard(t, old, want)
			if len(next.models) != 0 || len(next.works) != 0 {
				t.Fatal("idle disposition rebound to new model")
			}
			results := []execution.WorkEvent{}
			for _, e := range old.works {
				if e.Kind == execution.WorkCompaction && e.Phase == execution.WorkResult {
					results = append(results, e)
				}
			}
			if len(results) != 1 || results[0].Count != 0 || results[0].RunDuration != nil || results[0].DeliveryDuration == nil {
				t.Fatalf("results=%+v", results)
			}
			wantOutcome := "applied"
			if stale {
				wantOutcome = "stale"
			}
			if results[0].Outcome != wantOutcome || stale && results[0].ContextBefore != results[0].ContextAfter {
				t.Fatalf("result=%+v", results[0])
			}
			if !stale && results[0].Compactions != 1 {
				t.Fatal("application not counted")
			}
		})
	}
}

func TestExecutionWindDownDiscardAndClosure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "failed"}[failed], func(t *testing.T) {
			r := &executionRecorder{}
			step := summaryStep("", 11, 1)
			if failed {
				step = executionFailure(11, &llm.APIError{StatusCode: 400})
			}
			a := newAgent(llmtest.New("fake", step), tools.Default(), Options{Execution: r.scope("m")})
			a.SetTranscript(makeTurns(1))
			_, _, _, _ = a.finalizeWithSummary(context.Background(), &recordSink{}, nil, 1)
			reason := "summary_replaced"
			if failed {
				reason = "error"
			}
			assertExecutionDiscard(t, r, map[string]int{reason: 11})
		})
	}
	r := &executionRecorder{}
	a := newAgent(llmtest.New("fake", summaryStep("done", 1, 1)), tools.Default(), Options{Execution: r.scope("m"), MaxTurns: 1})
	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if len(r.prompts) != 1 || r.prompts[0].ClosureTrigger != string(ClosureTriggerTurnBudget) {
		t.Fatalf("prompt=%+v", r.prompts)
	}
}

func TestExecutionRetentionForwardsDetailedEffects(t *testing.T) {
	r := &executionRecorder{}
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Execution: r.scope("m"), ContextWindow: 100000})
	a.observeRetention(RetentionEvent{Policy: RetentionEventPolicyPressureEpoch, ContextTokensBefore: 70000, ContextTokensAfter: 40000, LocalEstimateTokensBefore: 60000, LocalEstimateTokensAfter: 40000, EstimatedTokensRemoved: 20000, BytesBefore: 240000, BytesAfter: 160000, BytesRemoved: 80000, BlocksTrimmed: 4, DecisionContextSource: ContextEstimateSourceResponseUsageDelta, PreviousRequestMode: RetentionRequestModeStatefulSuffix, NextRequestMode: RetentionRequestModeFull, ResponseStateReset: true, MeasurementAnchorReset: true, ContinuationStateReset: true})
	if len(r.contexts) != 1 {
		t.Fatal("missing context")
	}
	e := r.contexts[0]
	if e.Before != 70000 || e.After != 40000 || e.TokensRemoved != 20000 || e.BytesBefore != 240000 || e.BytesAfter != 160000 || e.BytesRemoved != 80000 || e.BlocksTrimmed != 4 || !e.ResponseStateReset || !e.MeasurementAnchorReset || !e.ContinuationStateReset || e.DecisionSource != ContextEstimateSourceResponseUsageDelta || e.PreviousRequestMode != string(RetentionRequestModeStatefulSuffix) || e.NextRequestMode != string(RetentionRequestModeFull) {
		t.Fatalf("lost retention details: %+v", e)
	}
}

func TestExecutionIdleArchiveErrorDoesNotDiscardReusableCandidate(t *testing.T) {
	r := &executionRecorder{}
	a := newAgent(llmtest.New("fake", summaryStep("prepared", 11, 1)), tools.Default(), Options{Execution: r.scope("m"), ContextWindow: 10000})
	a.SetTranscript(makeTurns(10))
	work, ok, err := a.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("prepare=%t %v", ok, err)
	}
	result, err := work(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.SetCompactionArchiver(func(context.Context, CompactionArchive) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("temporary archive failure")
		}
		return "archive", nil
	})
	if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err == nil || applied {
		t.Fatalf("first delivery=%t %v", applied, err)
	}
	assertExecutionDiscard(t, r, map[string]int{})
	if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err != nil || !applied {
		t.Fatalf("retry delivery=%t %v", applied, err)
	}
	assertExecutionDiscard(t, r, map[string]int{})
	outcomes := []string{}
	for _, e := range r.works {
		if e.Kind == execution.WorkCompaction && e.Phase == execution.WorkResult {
			outcomes = append(outcomes, e.Outcome)
		}
	}
	if !reflect.DeepEqual(outcomes, []string{"error", "applied"}) {
		t.Fatalf("outcomes=%v", outcomes)
	}
}

func TestExecutionQueueAdmissionPrecedesStageWait(t *testing.T) {
	firstStarted, release := make(chan time.Time, 1), make(chan struct{})
	admitted := make(chan time.Time, 1)
	registry := &tools.Registry{}
	registry.Register(&recordTool{name: "echo", run: func(ctx context.Context, input json.RawMessage) (string, error) {
		var args struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", err
		}
		queued := execution.ToolQueued(ctx)
		if args.N == 1 {
			firstStarted <- queued
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		} else {
			admitted <- queued
		}
		return "done", nil
	}})
	a := newAgent(llmtest.New("fake"), registry, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.dispatchCalls(ctx, []llm.ToolCall{{ID: "one", Name: "echo", Input: json.RawMessage(`{"n":1,"_stage":1}`)}, {ID: "two", Name: "echo", Input: json.RawMessage(`{"n":2,"_stage":2}`)}}, 1, 1, &recordSink{})
	}()
	var first time.Time
	select {
	case first = <-firstStarted:
	case <-ctx.Done():
		t.Fatal("stage one did not run")
	}
	close(release)
	var second time.Time
	select {
	case second = <-admitted:
	case <-ctx.Done():
		t.Fatal("stage two did not run")
	}
	<-done
	if first.IsZero() || !first.Equal(second) {
		t.Fatalf("stage wait lost original admission: %v %v", first, second)
	}
}
