package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
)

type workRecorder struct {
	mu     sync.Mutex
	events []execution.WorkEvent
}

func (*workRecorder) ObserveModel(execution.ModelEvent)     {}
func (*workRecorder) ObservePrompt(execution.PromptEvent)   {}
func (*workRecorder) ObserveContext(execution.ContextEvent) {}
func (r *workRecorder) ObserveWork(e execution.WorkEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
func (r *workRecorder) snapshot() []execution.WorkEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]execution.WorkEvent(nil), r.events...)
}
func workContext(r *workRecorder) context.Context {
	return execution.WithScope(context.Background(), execution.Scope{Observer: r, Identity: execution.Identity{Model: "launch-model"}})
}

type telemetryTool struct {
	name string
	run  func(context.Context) (RunResult, error)
}

func (t telemetryTool) Name() string                { return t.name }
func (telemetryTool) Description() string           { return "fixture" }
func (telemetryTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (telemetryTool) ReadOnly(json.RawMessage) bool { return true }
func (telemetryTool) Activity(json.RawMessage) Activity {
	return Activity{Class: "SECRET", Source: "SECRET"}
}
func (t telemetryTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	r, e := t.run(ctx)
	return r.Text, e
}
func (t telemetryTool) RunResult(ctx context.Context, _ json.RawMessage) (RunResult, error) {
	return t.run(ctx)
}

func TestExecutionParallelHTTPActualCompletion(t *testing.T) {
	slowStarted, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(slowStarted)
			<-release
		}
		fmt.Fprint(w, "SECRET response")
	}))
	defer server.Close()
	recorder := &workRecorder{}
	ctx := workContext(recorder)
	registry := &Registry{}
	registry.Register(webFetch{})
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		registry.Dispatch(ctx, llm.ToolCall{Name: "web_fetch", Input: json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL+"/slow"))})
	}()
	<-slowStarted
	result, done := registry.DispatchWithCompletion(ctx, llm.ToolCall{Name: "web_fetch", Input: json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL+"/fast"))})
	<-done
	if result.IsError {
		t.Fatal(result)
	}
	events := recorder.snapshot()
	starts, finishes, results := 0, 0, 0
	for _, e := range events {
		switch e.Phase {
		case execution.WorkStart:
			starts++
		case execution.WorkFinish:
			finishes++
			if e.RunDuration == nil {
				t.Fatal("missing duration")
			}
		case execution.WorkResult:
			results++
		}
	}
	close(release)
	<-slowDone
	if starts != 2 || finishes != 1 || results != 1 {
		t.Fatalf("fast completion was batched: %+v", events)
	}
	if strings.Contains(fmt.Sprint(recorder.snapshot()), "SECRET") {
		t.Fatal("content leaked")
	}
}

func TestExecutionDeadlineResultBeforeActualFinish(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			recorder := &workRecorder{}
			ctx, cancel := context.WithCancel(workContext(recorder))
			defer cancel()
			ctx = execution.WithToolQueued(ctx, time.Now().Add(-time.Second))
			started, release := make(chan struct{}), make(chan struct{})
			registry := &Registry{}
			registry.Register(telemetryTool{name: "worker", run: func(context.Context) (RunResult, error) {
				close(started)
				<-release
				return RunResult{Text: "SECRET"}, nil
			}})
			if deadline {
				registry.SetDispatchTimeout(20 * time.Millisecond)
			}
			returned := make(chan struct{})
			var result llm.ToolResult
			var completed <-chan struct{}
			go func() {
				result, completed = registry.DispatchWithCompletion(ctx, llm.ToolCall{Name: "worker"})
				close(returned)
			}()
			<-started
			if !deadline {
				cancel()
			}
			<-returned
			events := recorder.snapshot()
			if len(events) != 2 || events[0].Phase != execution.WorkStart || events[1].Phase != execution.WorkResult || events[0].QueueDuration == nil || *events[0].QueueDuration < time.Second {
				t.Fatalf("events: %+v", events)
			}
			if !result.IsError {
				t.Fatal("expected timeout/cancel")
			}
			select {
			case <-completed:
				t.Fatal("completion closed with worker still alive")
			default:
			}
			close(release)
			<-completed
			events = recorder.snapshot()
			if len(events) != 3 || events[2].Phase != execution.WorkFinish || events[2].RunDuration == nil {
				t.Fatalf("late completion: %+v", events)
			}
		})
	}
}

func TestExecutionLogicalErrorsAndSanitization(t *testing.T) {
	for _, kind := range []string{"unknown", "invalid", "guard", "panic", "result"} {
		t.Run(kind, func(t *testing.T) {
			recorder := &workRecorder{}
			registry := &Registry{}
			registry.SetResultLimits(80, 10)
			registry.Register(telemetryTool{name: "worker", run: func(context.Context) (RunResult, error) {
				if kind == "panic" {
					panic("SECRET panic")
				}
				return RunResult{Text: strings.Repeat("SECRET", 200), Metrics: map[string]int{CommandMetricStepsTotal: 3, "SECRET": 99}}, nil
			}})
			call := llm.ToolCall{Name: "worker", Input: json.RawMessage(`{}`)}
			if kind == "unknown" {
				call.Name = "SECRET unknown"
			}
			if kind == "invalid" {
				call.Input = json.RawMessage(`{"_stage":0}`)
			}
			if kind == "guard" {
				registry.SetDispatchGuard(func(llm.ToolCall, Activity) error { return fmt.Errorf("SECRET guard") })
			}
			result, done := registry.DispatchWithCompletion(workContext(recorder), call)
			<-done
			results := 0
			for _, e := range recorder.snapshot() {
				if strings.Contains(fmt.Sprint(e), "SECRET") {
					t.Fatalf("content leaked: %+v", e)
				}
				if e.Phase == execution.WorkResult {
					results++
					if e.ResultBytes != len(result.Text) {
						t.Fatal("wrong result bytes")
					}
					if kind == "result" && (!e.Truncated || e.OriginalBytes != 1200 || e.Metrics[CommandMetricStepsTotal] != 3 || e.Activity != "other") {
						t.Fatalf("metadata: %+v", e)
					}
				}
			}
			if results != 1 {
				t.Fatal("logical results", results)
			}
		})
	}
}

func TestExecutionCommandStepsAndPipeline(t *testing.T) {
	for _, input := range []string{`{"command":"printf SECRET | cat"}`, `{"steps":[{"argv":["/usr/bin/true"]},{"argv":["/usr/bin/false"]},{"argv":["/usr/bin/true"]}]}`} {
		recorder := &workRecorder{}
		result, err := (shell{}).RunResult(workContext(recorder), json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		events := recorder.snapshot()
		want := 2
		if strings.Contains(input, "steps") {
			want = 4
			if result.Metrics[CommandMetricStepsSkipped] != 1 {
				t.Fatal(result)
			}
		}
		if len(events) != want {
			t.Fatalf("events: %+v", events)
		}
		for _, e := range events {
			if e.Kind != execution.WorkCommand || e.Count != 1 || strings.Contains(fmt.Sprint(e), "SECRET") {
				t.Fatal(e)
			}
		}
		if want == 4 && (events[3].Outcome != "failed" || events[3].Trigger != "step" || events[3].Tool != "argv") {
			t.Fatal(events)
		}
	}
}

// Holding the final callback proves Group.Wait includes observation delivery,
// not only Tool.Run's return or the logical dispatch result.
type groupFinishRecorder struct {
	workRecorder
	finishing chan struct{}
	release   chan struct{}
}

func (r *groupFinishRecorder) ObserveWork(event execution.WorkEvent) {
	if event.Kind == execution.WorkTool && event.Phase == execution.WorkFinish {
		close(r.finishing)
		<-r.release
	}
	r.workRecorder.ObserveWork(event)
}

func TestExecutionGroupRetainsTimedOutWorker(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			group := &execution.Group{}
			recorder := &groupFinishRecorder{finishing: make(chan struct{}), release: make(chan struct{})}
			ctx, cancel := context.WithCancel(execution.WithScope(context.Background(), execution.Scope{Observer: recorder, Group: group}))
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			registry := &Registry{}
			registry.Register(telemetryTool{name: "worker", run: func(context.Context) (RunResult, error) {
				close(started)
				<-release
				return RunResult{Text: "late result"}, nil
			}})
			if deadline {
				registry.SetDispatchTimeout(20 * time.Millisecond)
			}
			returned := make(chan struct{})
			var result llm.ToolResult
			var completed <-chan struct{}
			go func() {
				result, completed = registry.DispatchWithCompletion(ctx, llm.ToolCall{Name: "worker"})
				close(returned)
			}()
			<-started
			if !deadline {
				cancel()
			}
			<-returned
			if !result.IsError {
				t.Fatal("expected logical cancellation/timeout")
			}
			canceled, stop := context.WithCancel(context.Background())
			stop()
			if err := group.Wait(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("live worker escaped group: %v", err)
			}
			close(release)
			<-recorder.finishing
			if err := group.Wait(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("unfinished observer escaped group: %v", err)
			}
			close(recorder.release)
			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
			defer waitCancel()
			if err := group.Wait(waitCtx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-completed:
			default:
				t.Fatal("group released before actual completion signal")
			}
			events := recorder.snapshot()
			if len(events) != 3 || events[2].Phase != execution.WorkFinish {
				t.Fatalf("group released before final observation: %+v", events)
			}
			if err := group.Wait(canceled); err != nil {
				t.Fatalf("group did not drain to zero: %v", err)
			}
		})
	}
}
