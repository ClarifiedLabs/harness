package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"harness/internal/agent"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

type delegateObserver struct {
	mu       sync.Mutex
	models   []execution.ModelEvent
	works    []execution.WorkEvent
	prompts  []execution.PromptEvent
	contexts []execution.ContextEvent
}

func (r *delegateObserver) ObserveModel(e execution.ModelEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = append(r.models, e)
}
func (r *delegateObserver) ObserveWork(e execution.WorkEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.works = append(r.works, e)
}
func (r *delegateObserver) ObservePrompt(e execution.PromptEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, e)
}
func (r *delegateObserver) ObserveContext(e execution.ContextEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contexts = append(r.contexts, e)
}
func (r *delegateObserver) scope() execution.Scope {
	return execution.Scope{Observer: r, Identity: execution.Identity{Provider: "parent-provider", Model: "parent-model", Agent: "parent"}}
}
func (r *delegateObserver) finishes(mode string) []execution.WorkEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []execution.WorkEvent
	for _, e := range r.works {
		if e.Kind == execution.WorkDelegate && e.Phase == execution.WorkFinish && e.Mode == mode {
			out = append(out, e)
		}
	}
	return out
}
func (r *delegateObserver) assertUsage(t *testing.T, want map[string]int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	got := map[string]int{}
	for _, e := range r.models {
		if e.Phase == execution.ModelUsageDelta {
			got[e.Identity.Model] += e.Usage.InputTokens
		}
		if e.Attempt.Usage != nil {
			t.Fatal("physical usage duplicated in Attempt")
		}
		if e.Identity.Model != "parent-model" && e.Identity.Delegate != "true" {
			t.Fatalf("child identity = %+v", e.Identity)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("usage = %v, want %v", got, want)
	}
	for model, n := range want {
		if got[model] != n {
			t.Fatalf("usage = %v, want %v", got, want)
		}
	}
}

// Emit physical source callbacks as well as aggregate stream usage: counting
// either a parent inclusive result or the aggregate again fails the assertions.
type delegatePhysicalProvider struct{ llm.Provider }

func (p delegatePhysicalProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		a := llm.StartAttempt(ctx)
		defer a.Finish(llm.AttemptIncomplete, ctx.Err())
		p.Provider.Stream(ctx, req)(a.WrapYield(yield))
	}
}
func delegateUsageStep(n int) llmtest.Step {
	return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: n, OutputTokens: 2}}
}
func bindExecutionFixture(f continuationFixture) {
	resolve := f.runner.resolve
	f.runner.resolve = func(runtime Runtime, name string) (Launch, error) {
		launch, err := resolve(runtime, name)
		launch.Provider = delegatePhysicalProvider{launch.Provider}
		launch.ProviderName, launch.Model = "child-provider", "child-model"
		return launch, err
	}
}

func TestDelegateExecutionCrossModelForegroundBackgroundContinuation(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			f := newContinuationFixture(t, 100000, false, delegateUsageStep(7), delegateUsageStep(11))
			bindExecutionFixture(f)
			r := &delegateObserver{}
			starter := &fakeBackgroundStarter{}
			tool := NewTool(f.runner, starter)
			parent, cancel := context.WithCancel(execution.WithScope(context.Background(), r.scope()))
			defer cancel()
			var firstID string
			if background {
				if _, err := tool.RunMetered(parent, json.RawMessage(`{"task":"first","background":true}`)); err != nil {
					t.Fatal(err)
				}
				if starter.req.AdmissionContext != parent {
					t.Fatal("background admission must use the cancelable launcher context")
				}
				if got := starter.req.Execution.Identity; got.Model != "child-model" || got.Provider != "child-provider" || got.Agent != "worker" || got.Delegate != "true" {
					t.Fatalf("job identity = %+v", got)
				}
				// Mutation after enqueue must not change either observer or launch.
				original := f.state.Snapshot()
				next := original
				next.Provider = llmtest.New("replacement")
				next.Execution = (&delegateObserver{}).scope()
				next.Model = "replacement"
				f.state.Set(next)
				cancel()
				if _, err := starter.req.Run(context.Background(), "first"); err != nil {
					t.Fatalf("parent cancellation leaked: %v", err)
				}
				f.state.Set(original)
				firstID = "first"
			} else {
				result, err := f.runner.Run(parent, RunRequest{Task: "first"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				firstID = result.ChildID
			}
			// A new observer is permitted on continuation: observation state must
			// not serialize or enter the continuation fingerprint.
			r2 := &delegateObserver{}
			ctx := execution.WithScope(context.Background(), r2.scope())
			input, _ := json.Marshal(map[string]any{"task": "followup", "continue_child_id": firstID, "background": background})
			if _, err := tool.RunMetered(ctx, input); err != nil {
				t.Fatal(err)
			}
			if background {
				if _, err := starter.req.Run(context.Background(), "second"); err != nil {
					t.Fatal(err)
				}
			}
			r.assertUsage(t, map[string]int{"child-model": 7})
			r2.assertUsage(t, map[string]int{"child-model": 11})
			mode := map[bool]string{false: "foreground", true: "background"}[background]
			for _, observer := range []*delegateObserver{r, r2} {
				finishes := observer.finishes(mode)
				if len(finishes) != 1 || finishes[0].Turns != 1 || finishes[0].Outcome != "success" || finishes[0].Termination != "model_completed" || finishes[0].RunDuration == nil {
					t.Fatalf("finishes = %+v", finishes)
				}
				if len(observer.contexts) == 0 || len(observer.prompts) != 1 {
					t.Fatalf("missing child context/prompt telemetry: %+v", observer)
				}
			}
		})
	}
}

func TestDelegateExecutionBackgroundRejectsStaleLauncherAfterClear(t *testing.T) {
	f := newContinuationFixture(t, 100000, false, delegateUsageStep(7))
	bindExecutionFixture(f)
	r := &delegateObserver{}
	parent, cancel := context.WithCancel(execution.WithScope(context.Background(), r.scope()))
	defer cancel()
	jobs := background.NewManager(background.Options{})
	t.Cleanup(func() { jobs.Shutdown() })
	resolve := f.runner.resolve
	resolved := false
	f.runner.resolve = func(runtime Runtime, name string) (Launch, error) {
		launch, err := resolve(runtime, name)
		resolved = true
		// Cancellation and reset happen after RunMetered's initial context
		// check but before admission. Reopening must not admit this old caller.
		cancel()
		jobs.Clear()
		return launch, err
	}
	_, err := NewTool(f.runner, jobs).RunMetered(parent, json.RawMessage(`{"task":"stale","background":true,"access":"read_only"}`))
	if !resolved || !errors.Is(err, context.Canceled) {
		t.Fatalf("resolved=%v, stale launcher error=%v", resolved, err)
	}
	if got := jobs.List(); len(got) != 0 {
		t.Fatalf("stale launcher registered jobs: %+v", got)
	}
	if f.provider.RequestCount() != 0 {
		t.Fatal("stale launcher reached the child provider")
	}
}

func TestDelegateExecutionNestedUsageIsExclusive(t *testing.T) {
	r := &delegateObserver{}
	call := func(id, role string) llmtest.Step {
		return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: id, ToolName: "delegate", ToolInput: json.RawMessage(`{"task":"work","agent":"` + role + `"}`)}}, Stop: llm.StopToolUse, Usage: llm.Usage{InputTokens: 3}}
	}
	providers := map[string]llm.Provider{
		"middle": delegatePhysicalProvider{llmtest.New("middle", call("nested", "leaf"), delegateUsageStep(5))},
		"leaf":   delegatePhysicalProvider{llmtest.New("leaf", delegateUsageStep(7))},
	}
	parentProvider := delegatePhysicalProvider{llmtest.New("parent", call("child", "middle"), delegateUsageStep(11))}
	state := NewState(Runtime{Provider: parentProvider, ProviderName: "parent-provider", Model: "parent-model", ToolNames: []string{"delegate"}, Registry: llm.NewRegistry(nil)})
	registry := &tools.Registry{}
	runner := NewRunner(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{Provider: providers[name], ProviderName: name + "-provider", Model: name + "-model", Agent: name, Tools: registry, Registry: llm.NewRegistry(nil), ContextWindow: 100000}, nil
	}, Options{MaxTurns: 4, DisableAutoCompaction: true})
	registry.Register(NewTool(runner))
	parent := agent.New(parentProvider, registry, agent.Options{Model: "parent-model", Execution: r.scope(), MaxTurns: 4, DisableAutoCompaction: true})
	sink := newChildSink("", nil, false, NewProgress(), nil)
	if err := parent.RunPrompt(context.Background(), "start", sink); err != nil {
		t.Fatal(err)
	}
	r.assertUsage(t, map[string]int{"parent-model": 14, "middle-model": 8, "leaf-model": 7})
	if sink.usage.Usage.InputTokens != 29 {
		t.Fatalf("parent inclusive usage = %+v", sink.usage.Usage)
	}
	if len(r.finishes("foreground")) != 2 {
		t.Fatalf("nested lifecycle = %+v", r.works)
	}
}

func TestDelegateExecutionInteractiveLifetimeAndPinnedFollowup(t *testing.T) {
	f, manager, jobs, tool := newInteractiveDelegateTest(t, delegateUsageStep(7), delegateUsageStep(11))
	bindExecutionFixture(f)
	r := &delegateObserver{}
	parent, cancel := context.WithCancel(execution.WithScope(context.Background(), r.scope()))
	defer cancel()
	result, err := tool.RunMetered(parent, json.RawMessage(`{"task":"first","background":true,"interactive":true,"access":"read_only"}`))
	if err != nil {
		t.Fatal(err)
	}
	s := onlyDelegateSession(t, manager)
	waitDelegateSessionState(t, manager, s.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, result.BackgroundJobID)
	if len(r.finishes("interactive_session")) != 0 || len(r.finishes("interactive_prompt")) != 1 {
		t.Fatal("session lifetime confused with first prompt")
	}
	cancel()
	other := &delegateObserver{}
	next, err := manager.Prompt(execution.WithScope(context.Background(), other.scope()), agentsession.PromptRequest{SessionID: s.ID, Prompt: "followup"})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateSessionState(t, manager, s.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, next.Job.ID)
	if err := manager.Close(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	r.assertUsage(t, map[string]int{"child-model": 18})
	other.assertUsage(t, map[string]int{})
	if len(r.finishes("interactive_session")) != 1 || len(r.finishes("interactive_prompt")) != 2 {
		t.Fatalf("lifecycle = %+v", r.works)
	}
}

func TestDelegateExecutionErrorsCancellationAndAbandon(t *testing.T) {
	for _, runErr := range []error{&llm.APIError{StatusCode: 400, Message: "private error text", Retryable: false}, context.Canceled} {
		f := newContinuationFixture(t, 100000, false, llmtest.Step{Err: runErr})
		r := &delegateObserver{}
		runtime := f.state.Snapshot()
		runtime.Execution = r.scope()
		f.state.Set(runtime)
		_, err := f.runner.Run(context.Background(), RunRequest{Task: "fail"}, nil)
		if !errors.Is(err, runErr) {
			t.Fatalf("error = %v", err)
		}
		events := r.finishes("foreground")
		if len(events) != 1 || events[0].Outcome == "success" || strings.Contains(events[0].ErrorKind, "private") {
			t.Fatalf("events = %+v", events)
		}
	}
	r := &delegateObserver{}
	work := startDelegateWork(r.scope(), "interactive_session")
	runtime := &interactiveRuntime{work: work, active: "private-job-id"}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	work.finish(RunResult{}, nil, "") // a late return cannot complete it again
	if events := r.finishes("interactive_session"); len(events) != 1 || events[0].Outcome != "abandoned" {
		t.Fatalf("abandon events = %+v", events)
	}
}

type delegateScopeTool struct {
	fakeChildTool
	check func(context.Context)
}

func (t delegateScopeTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	t.check(ctx)
	return "ok", nil
}

func TestDelegateExecutionChildToolScopeAndIndependentCancellation(t *testing.T) {
	r := &delegateObserver{}
	parent, cancelParent := context.WithCancel(execution.WithScope(context.Background(), r.scope()))
	defer cancelParent()
	worker, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	f := newContinuationFixture(t, 100000, false, llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "scope", ToolName: "read", ToolInput: json.RawMessage(`{}`)}}, Stop: llm.StopToolUse}, delegateUsageStep(5))
	bindExecutionFixture(f)
	resolve := f.runner.resolve
	called := false
	f.runner.resolve = func(runtime Runtime, name string) (Launch, error) {
		launch, err := resolve(runtime, name)
		launch.Tools = &tools.Registry{}
		launch.Tools.Register(delegateScopeTool{fakeChildTool: fakeChildTool{name: "read"}, check: func(ctx context.Context) {
			called = true
			if got := execution.FromContext(ctx); got.Observer != r || got.Identity.Model != "child-model" || got.Identity.Delegate != "true" {
				t.Fatalf("tool scope = %+v", got)
			}
			cancelWorker()
		}})
		return launch, err
	}
	starter := &fakeBackgroundStarter{}
	if _, err := NewTool(f.runner, starter).RunMetered(parent, json.RawMessage(`{"task":"work","background":true}`)); err != nil {
		t.Fatal(err)
	}
	_, err := starter.req.Run(worker, "cancelled-child")
	if !called || !errors.Is(err, context.Canceled) || parent.Err() != nil {
		t.Fatalf("called=%v worker=%v parent=%v", called, err, parent.Err())
	}
	if events := r.finishes("background"); len(events) != 1 || events[0].Outcome != "cancelled" {
		t.Fatalf("cancelled events = %+v", events)
	}
}

func TestDelegateExecutionRuntimeSnapshotNotSerialized(t *testing.T) {
	r := &delegateObserver{}
	state := NewState(Runtime{Execution: r.scope()})
	copy := state.Snapshot()
	copy.Execution.Identity.Model = "changed"
	if state.Snapshot().Execution.Identity.Model != "parent-model" {
		t.Fatal("scope identity was shared")
	}
	data, err := json.Marshal(state.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Execution") || strings.Contains(string(data), "parent-model") {
		t.Fatalf("serialized observation state: %s", data)
	}
}
