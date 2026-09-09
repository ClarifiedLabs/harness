package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

type executionRecorder struct {
	mu            sync.Mutex
	models        []execution.ModelEvent
	modelFinished chan struct{}
	works         []execution.WorkEvent
	prompts       []execution.PromptEvent
	contexts      []execution.ContextEvent
}

func (r *executionRecorder) ObserveModel(e execution.ModelEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = append(r.models, e)
	if e.Phase == execution.ModelFinish && r.modelFinished != nil {
		r.modelFinished <- struct{}{}
	}
}
func (r *executionRecorder) ObserveWork(e execution.WorkEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.works = append(r.works, e)
}
func (r *executionRecorder) ObservePrompt(e execution.PromptEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, e)
}
func (r *executionRecorder) ObserveContext(e execution.ContextEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contexts = append(r.contexts, e)
}
func (r *executionRecorder) scope(model string) execution.Scope {
	return execution.Scope{Observer: r, Identity: execution.Identity{Provider: "configured", Model: model, Agent: "root"}}
}
func (r *executionRecorder) usage() llm.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total llm.Usage
	for _, e := range r.models {
		if e.Phase == execution.ModelUsageDelta {
			total = llm.AddUsage(total, e.Usage)
		}
	}
	return total
}
func (r *executionRecorder) finished(kind execution.WorkKind) []execution.WorkEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []execution.WorkEvent
	for _, e := range r.works {
		if e.Kind == kind && e.Phase == execution.WorkFinish {
			out = append(out, e)
		}
	}
	return out
}

type executionTestProvider struct {
	run func(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error]
}

func (*executionTestProvider) Name() string { return "fake" }
func (p *executionTestProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return p.run(ctx, req)
}

func TestExecutionAttemptErrorsPreserveUsageAndRetryCause(t *testing.T) {
	r := &executionRecorder{}
	calls := 0
	p := &executionTestProvider{run: func(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			calls++
			meta := llm.AttemptMetadataFromContext(ctx)
			if calls > 1 && (meta.Cause != llm.AttemptRetry || meta.RetryLayer != llm.RetryLayerAgent) {
				t.Errorf("retry metadata = %+v", meta)
			}
			source := llm.StartAttempt(ctx)
			u := llm.Usage{InputTokens: 10, OutputTokens: calls}
			source.Usage(u)
			if calls == 1 {
				err := errors.New("interrupted")
				source.Finish(llm.AttemptFailed, err)
				yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, err)
				return
			}
			source.Finish(llm.AttemptSucceeded, nil)
			yield(llm.StreamEvent{Kind: llm.EventDone, StopReason: llm.StopEndTurn, Usage: &u}, nil)
		}
	}}
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("model")})
	a.sleep = func(context.Context, time.Duration) error { return nil }
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatal(err)
	}
	if got := r.usage(); got.InputTokens != 20 || got.OutputTokens != 3 {
		t.Fatalf("billable usage = %+v", got)
	}
	if len(r.prompts) != 1 || r.prompts[0].Termination != string(TerminationModelCompleted) {
		t.Fatalf("prompt events = %+v", r.prompts)
	}
	if len(sink.promptUsage) != 1 || sink.promptUsage[0].Wasted.InputTokens != 10 {
		t.Fatalf("discarded usage lost: %+v", sink.promptUsage)
	}
	if len(r.contexts) != 2 {
		t.Fatalf("request attempts = %+v", r.contexts)
	}
	mustValid(t, a.Transcript())
}

func TestExecutionPrewarmCapturesScopeBeforeBackgroundSwitch(t *testing.T) {
	old, newRecorder := &executionRecorder{}, &executionRecorder{}
	started, release := make(chan struct{}), make(chan struct{})
	p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 17}}}, Block: func(context.Context) { close(started); <-release }, Err: errors.New("failed warmup")})
	a := newAgent(p, tools.Default(), Options{Model: "old", Execution: old.scope("old")})
	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("no warmup")
	}
	done := make(chan PrewarmResult, 1)
	go func() { done <- warm(context.Background()) }()
	<-started
	a.SetExecution(newRecorder.scope("new"))
	a.SetModel("new", 0)
	close(release)
	result := <-done
	if result.Usage.InputTokens != 17 || old.usage().InputTokens != 17 || newRecorder.usage() != (llm.Usage{}) {
		t.Fatal("warmup usage/scope lost")
	}
	for _, event := range old.models {
		if event.Identity.Model != "old" || event.Attempt.Purpose != llm.RequestPurposePrewarm {
			t.Fatalf("identity/purpose = %+v", event)
		}
	}
	if len(a.Transcript()) != 0 || len(old.prompts) != 0 {
		t.Fatal("prewarm became a prompt")
	}
}

func TestExecutionSummaryRetriesAndBranchUsage(t *testing.T) {
	r := &executionRecorder{}
	var metas []llm.AttemptMetadata
	capture := func(ctx context.Context) { metas = append(metas, llm.AttemptMetadataFromContext(ctx)) }
	p := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 3}}}, Block: capture, Err: errors.New("retry")},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("short")}, Block: capture, Usage: llm.Usage{InputTokens: 5}, Stop: llm.StopMaxTokens},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("complete")}, Block: capture, Usage: llm.Usage{InputTokens: 7}, Stop: llm.StopEndTurn})
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	a.sleep = func(context.Context, time.Duration) error { return nil }
	text, u, err := a.GenerateBranchSummary(context.Background(), makeTurns(2), "")
	if err != nil || text != "complete" || u.InputTokens != 15 || r.usage().InputTokens != 15 {
		t.Fatalf("summary = %q %+v %v billable=%+v", text, u, err, r.usage())
	}
	for i, m := range metas {
		if m.Purpose != llm.RequestPurposeBranchSummary {
			t.Errorf("purpose=%q", m.Purpose)
		}
		if i > 0 && (m.Cause != llm.AttemptRetry || m.RetryLayer != llm.RetryLayerAgent) {
			t.Errorf("retry=%+v", m)
		}
	}
	if len(r.prompts) != 0 {
		t.Fatal("maintenance produced prompt billing")
	}
}

func TestExecutionCompactionIncludesLocalAndFailedNotes(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "failed"}[fail], func(t *testing.T) {
			r := &executionRecorder{}
			a, _, _, _ := notesTestAgent(t, llmtest.New("fake"), Options{Execution: r.scope("notes")})
			a.SetTranscript([]llm.Message{userText("keep instructions"), asstText(strings.Repeat("evidence ", 1000))})
			if fail {
				a.SetCompactionArchiver(func(context.Context, CompactionArchive) (string, error) { return "", errors.New("archive failed") })
			}
			_, err := a.Compact(context.Background(), &recordSink{})
			if (err != nil) != fail {
				t.Fatalf("err=%v", err)
			}
			events := r.finished(execution.WorkCompaction)
			if len(events) != 1 || events[0].Mode != "task_notes" || events[0].RunDuration == nil {
				t.Fatalf("events=%+v", events)
			}
			if fail && events[0].Outcome != "error" {
				t.Fatalf("outcome=%s", events[0].Outcome)
			}
			if !fail && events[0].ContextAfter >= events[0].ContextBefore {
				t.Fatalf("not reclaimed: %+v", events[0])
			}
			if len(r.models) != 0 {
				t.Fatal("local compaction billed a model")
			}
			mustValid(t, a.Transcript())
		})
	}
}

func TestExecutionNativeAndIdleCompaction(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		r := &executionRecorder{}
		p := &nativeCompactionProvider{FakeProvider: llmtest.New("responses"), result: llm.CompactedContext{Items: nativeCompactedItems(), Usage: llm.Usage{InputTokens: 42}}}
		a := newAgent(p, tools.Default(), Options{Execution: r.scope("gpt-5.5"), Model: "gpt-5.5", NativeCompaction: true, ReasoningReplayDomain: "openai:gpt-5"})
		a.SetTranscript(makeTurns(10))
		if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
			t.Fatal(err)
		}
		events := r.finished(execution.WorkCompaction)
		if r.usage().InputTokens != 42 || len(events) != 1 || events[0].Mode != "native" {
			t.Fatalf("usage/events = %+v %+v", r.usage(), events)
		}
	})
	t.Run("idle snapshot", func(t *testing.T) {
		old, next := &executionRecorder{}, &executionRecorder{}
		p := llmtest.New("fake", summaryStep("idle summary", 11, 2))
		a := newAgent(p, tools.Default(), Options{Execution: old.scope("old"), Model: "old", ContextWindow: 10000})
		a.SetTranscript(makeTurns(10))
		work, ok, err := a.PrepareIdleCompaction(1)
		if err != nil || !ok {
			t.Fatalf("prepare=%t %v", ok, err)
		}
		a.SetExecution(next.scope("new"))
		a.SetModel("new", 10000)
		done := make(chan IdleCompactionResult, 1)
		go func() {
			result, err := work(context.Background())
			if err != nil {
				t.Error(err)
			}
			done <- result
		}()
		result := <-done
		if !result.Prepared || old.usage().InputTokens != 11 || next.usage() != (llm.Usage{}) {
			t.Fatal("idle snapshot lost observer")
		}
		events := old.finished(execution.WorkCompaction)
		if len(events) != 1 || events[0].Trigger != "idle" || events[0].Outcome != "prepared" {
			t.Fatalf("events=%+v", events)
		}
		if applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result); err != nil || applied {
			t.Fatal("stale candidate applied")
		}
	})
}

func TestExecutionParallelDispatchSurvivesCompaction(t *testing.T) {
	r := &executionRecorder{}
	started, release := make(chan struct{}, 2), make(chan struct{})
	registry := &tools.Registry{}
	registry.Register(&recordTool{name: "echo", readOnly: true, run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		if execution.FromContext(ctx).Observer != r {
			t.Error("missing tool scope")
		}
		if execution.ToolQueued(ctx).IsZero() {
			t.Error("missing parallel admission timestamp")
		}
		started <- struct{}{}
		<-release
		return "ok", nil
	}})
	p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "a", "echo", `{"n":1}`), toolDone(1, "b", "echo", `{"n":2}`)}, Stop: llm.StopToolUse}, summaryStep("done", 1, 1), summaryStep("checkpoint", 2, 1))
	a := newAgent(p, registry, Options{Execution: r.scope("m")})
	done := make(chan error, 1)
	go func() { done <- a.RunPrompt(context.Background(), "go", &recordSink{}) }()
	<-started
	<-started
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CompactForContinuation(context.Background(), &recordSink{}); err != nil {
		t.Fatal(err)
	}
	events := r.finished(execution.WorkParallel)
	if len(events) != 1 || events[0].Count != 1 || events[0].BatchSize != 2 || events[0].Completed != 2 || events[0].RunDuration == nil {
		t.Fatalf("batch=%+v", events)
	}
	mustValid(t, a.Transcript())
}

func TestExecutionCompatibilityRebuildAndMapReduce(t *testing.T) {
	t.Run("compatibility", func(t *testing.T) {
		r := &executionRecorder{}
		var metas []llm.AttemptMetadata
		capture := func(ctx context.Context) { metas = append(metas, llm.AttemptMetadataFromContext(ctx)) }
		p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 3}}}, Block: capture, Err: &llm.APIError{Code: "previous_response_id", StatusCode: 400}}, llmtest.Step{Block: capture, Usage: llm.Usage{InputTokens: 4}, Stop: llm.StopEndTurn})
		a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
		coordinator := newTurnAttemptCoordinator(a, &recordSink{}, 1)
		req := llm.Request{Purpose: llm.RequestPurposeTurn}
		previous, err := coordinator.request(context.Background(), req, ContextEstimate{Total: 10})
		if err == nil {
			t.Fatal("expected compatibility error")
		}
		if _, err := coordinator.rerun(context.Background(), previous, req, ContextEstimate{Total: 20}); err != nil {
			t.Fatal(err)
		}
		if len(metas) != 2 || metas[1].Cause != llm.AttemptRetry || metas[1].RetryLayer != llm.RetryLayerAgent || coordinator.wasted.InputTokens != 3 || r.usage().InputTokens != 7 {
			t.Fatalf("metadata/waste=%+v %+v", metas, coordinator.wasted)
		}
	})
	t.Run("map reduce", func(t *testing.T) {
		r := &executionRecorder{}
		steps := make([]llmtest.Step, 20)
		for i := range steps {
			steps[i] = summaryStep("summary", 1, 1)
		}
		p := llmtest.New("fake", steps...)
		a := newAgent(p, tools.Default(), Options{Execution: r.scope("m"), ContextWindow: 10000})
		history := makeTurns(4)
		for i := 1; i < len(history); i += 2 {
			history[i] = asstText(strings.Repeat("detail ", 3000))
		}
		_, u, err := a.GenerateBranchSummary(context.Background(), history, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Requests) < 3 || u.InputTokens != len(p.Requests) || r.usage() != u {
			t.Fatalf("requests/usage=%d %+v %+v", len(p.Requests), u, r.usage())
		}
	})
}

func TestExecutionPromptJoinHasNoInclusiveBilling(t *testing.T) {
	r := &executionRecorder{}
	sink := &promptWorkSink{pending: true, waitErr: context.Canceled}
	_, err := waitForPromptWork(execution.WithScope(context.Background(), r.scope("m")), sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	events := r.finished(execution.WorkWait)
	if len(events) != 1 || events[0].Outcome != "cancelled" || events[0].RunDuration == nil || len(r.models) != 0 {
		t.Fatalf("join=%+v", events)
	}
}

func TestExecutionAsyncReuseDoesNotCreateSecondBatch(t *testing.T) {
	r := &executionRecorder{}
	registry := &tools.Registry{}
	started, release := make(chan struct{}, 2), make(chan struct{})
	registry.Register(&recordTool{name: "read", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		started <- struct{}{}
		<-release
		return "read", nil
	}})
	events := []llm.StreamEvent{}
	for _, id := range []string{"a", "b"} {
		input := json.RawMessage(`{"path":"` + id + `"}`)
		events = append(events, llm.StreamEvent{Kind: llm.EventToolCallReady, ToolID: id, ToolName: "read", ToolInput: input, ToolAsync: true}, llm.StreamEvent{Kind: llm.EventToolCallDone, ToolID: id, ToolName: "read", ToolInput: input, ToolAsync: true})
	}
	p := llmtest.New("astra", llmtest.Step{Events: events, Stop: llm.StopToolUse, Block: func(context.Context) { <-started; <-started; close(release) }}, summaryStep("done", 1, 1))
	a := newAgent(p, registry, Options{Execution: r.scope("astra"), Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {AsyncTools: true, ContextWindow: 100000}}), ExperimentalAsyncTools: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.RunPrompt(ctx, "read", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	batches := r.finished(execution.WorkParallel)
	if len(batches) != 1 || batches[0].Mode != "async" || batches[0].Count != 1 || batches[0].BatchSize != 2 {
		t.Fatalf("batches=%+v", batches)
	}
	mustValid(t, a.Transcript())
}

func TestExecutionRetentionAndCancelledPrompt(t *testing.T) {
	t.Run("retention", func(t *testing.T) {
		r := &executionRecorder{}
		registry := &tools.Registry{}
		registry.Register(&recordTool{name: "read", readOnly: true})
		a := newAgent(llmtest.New("fake", summaryStep("done", 1, 1)), registry, Options{Execution: r.scope("m"), RetentionPolicy: RetentionPolicyAge})
		history := []llm.Message{userText("task"), asstToolUse("read", "read", `{"path":"file"}`), toolResult("read", strings.Repeat("old output ", 1000))}
		history = append(history, makeTurns(6)...)
		a.SetTranscript(history)
		if err := a.RunPrompt(context.Background(), "continue", &recordSink{}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range r.contexts {
			if e.Reason == "retention" {
				found = true
				if e.Policy != "age" || e.Before <= e.After || e.Dropped != e.Before-e.After {
					t.Fatalf("retention=%+v", e)
				}
			}
		}
		if !found {
			t.Fatal("retention event missing")
		}
		mustValid(t, a.Transcript())
	})
	t.Run("cancelled", func(t *testing.T) {
		r := &executionRecorder{}
		a := newAgent(llmtest.New("fake"), tools.Default(), Options{Execution: r.scope("m")})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := a.RunPrompt(ctx, "go", &recordSink{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(r.prompts) != 1 || r.prompts[0].Termination != string(TerminationCancelled) || len(r.models) != 0 {
			t.Fatalf("prompt=%+v models=%+v", r.prompts, r.models)
		}
	})
}

func TestExecutionModelFinishPrecedesSpeculativeJoin(t *testing.T) {
	r := &executionRecorder{modelFinished: make(chan struct{}, 4)}
	started, release := make(chan struct{}), make(chan struct{})
	registry := &tools.Registry{}
	registry.Register(&recordTool{name: "read", readOnly: true, run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		select {
		case <-release:
			return "read", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}})
	p := llmtest.New("astra", llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallReady, ToolID: "r", ToolName: "read", ToolInput: json.RawMessage(`{"path":"a"}`), ToolAsync: true}, {Kind: llm.EventToolCallDone, ToolID: "r", ToolName: "read", ToolInput: json.RawMessage(`{"path":"a"}`), ToolAsync: true}}, Stop: llm.StopToolUse, Block: func(context.Context) { <-started }}, summaryStep("done", 1, 1))
	a := newAgent(p, registry, Options{Execution: r.scope("astra"), Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {AsyncTools: true, ContextWindow: 100000}}), ExperimentalAsyncTools: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.RunPrompt(ctx, "read", &recordSink{}) }()
	select {
	case <-r.modelFinished:
	case <-ctx.Done():
		t.Error("model duration included speculative join")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
