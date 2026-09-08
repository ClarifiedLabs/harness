package background

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
	"harness/internal/tools"
)

type workRecorder struct {
	mu      sync.Mutex
	events  []execution.WorkEvent
	manager *Manager
}

func (*workRecorder) ObserveModel(execution.ModelEvent)     {}
func (*workRecorder) ObservePrompt(execution.PromptEvent)   {}
func (*workRecorder) ObserveContext(execution.ContextEvent) {}
func (r *workRecorder) ObserveWork(e execution.WorkEvent) {
	// Reentrant manager reads prove callbacks do not hold Manager.mu.
	if r.manager != nil {
		r.manager.List()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
func (r *workRecorder) snapshot() []execution.WorkEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]execution.WorkEvent(nil), r.events...)
}

func TestExecutionBackgroundLifecycleAndPinnedScope(t *testing.T) {
	for _, outcome := range []string{StatusCompleted, StatusFailed, StatusCanceled, StatusAbandoned, "panic"} {
		t.Run(outcome, func(t *testing.T) {
			m := NewManager(Options{})
			recorder := &workRecorder{manager: m}
			scope := execution.Scope{Observer: recorder, Identity: execution.Identity{Model: "launch-model"}}
			started, release := make(chan struct{}), make(chan struct{})
			seen := make(chan execution.Scope, 1)
			metrics := map[string]int{tools.CommandMetricStepsExecuted: 2, "SECRET": 99}
			info, err := m.StartBackgroundJob(tools.BackgroundJobRequest{Kind: "shell", Description: "SECRET task", Execution: scope, Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
				seen <- execution.FromContext(ctx)
				close(started)
				<-release
				if outcome == "panic" {
					panic("SECRET panic")
				}
				if outcome == StatusFailed {
					return tools.BackgroundJobResult{}, fmt.Errorf("SECRET failure")
				}
				return tools.BackgroundJobResult{Text: "SECRET output", Metrics: metrics}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			<-started
			scope = scope.Rebind(execution.Identity{Model: "replacement"})
			if got := <-seen; got.Identity.Model != "launch-model" {
				t.Fatalf("detached scope: %+v", got)
			}
			m.SetDiagnosticIdentity(info.ID, DiagnosticIdentity{Model: "replacement"})
			m.mu.Lock()
			done := m.jobs[info.ID].done
			m.mu.Unlock()
			if outcome == StatusCanceled {
				m.Cancel(info.ID)
			}
			if outcome == StatusAbandoned {
				m.Shutdown()
				events := recorder.snapshot()
				if len(events) != 2 || events[1].RunDuration != nil {
					t.Fatalf("abandonment: %+v", events)
				}
			}
			close(release)
			<-done
			events := recorder.snapshot()
			if len(events) != 2 || events[0].Phase != execution.WorkStart || events[1].Phase != execution.WorkFinish {
				t.Fatalf("exactly once lifecycle: %+v", events)
			}
			want := outcome
			if want == "panic" {
				want = StatusFailed
			}
			if events[1].Outcome != want {
				t.Fatal(events)
			}
			if outcome != StatusAbandoned && events[1].RunDuration == nil {
				t.Fatal("missing actual duration")
			}
			for _, e := range events {
				if e.Identity.Model != "launch-model" || e.Mode != "background" || strings.Contains(fmt.Sprint(e), "SECRET") {
					t.Fatalf("unsafe observation: %+v", e)
				}
			}
			if outcome == StatusCompleted {
				events[1].Metrics[tools.CommandMetricStepsExecuted] = 123
				snap, _ := m.Get(info.ID)
				if snap.Result.Metrics[tools.CommandMetricStepsExecuted] != 2 {
					t.Fatal("observer mutated job metrics")
				}
			}
			m.Shutdown()
			if len(recorder.snapshot()) != 2 {
				t.Fatal("duplicate shutdown finish")
			}
		})
	}
}

func TestExecutionExplicitWaitOnly(t *testing.T) {
	m := NewManager(Options{})
	recorder := &workRecorder{manager: m}
	ctx := execution.WithScope(context.Background(), execution.Scope{Observer: recorder})
	if _, err := m.WaitFor(ctx, nil, "first", time.Second); err != nil {
		t.Fatal(err)
	}
	if len(recorder.snapshot()) != 0 {
		t.Fatal("parent wait duplicated")
	}
	if _, err := NewJobsTool(m).RunResult(ctx, json.RawMessage(`{"action":"wait"}`)); err != nil {
		t.Fatal(err)
	}
	events := recorder.snapshot()
	if len(events) != 2 || events[1].Kind != execution.WorkWait || events[1].Mode != "explicit" || events[1].Outcome != "no_running" || events[1].RunDuration == nil {
		t.Fatalf("wait: %+v", events)
	}
}

func TestExecutionBackgroundShellLaunchNotCommandSuccess(t *testing.T) {
	m := NewManager(Options{})
	recorder := &workRecorder{manager: m}
	ctx := execution.WithScope(context.Background(), execution.Scope{Observer: recorder, Identity: execution.Identity{Model: "launch-model"}})
	registry := tools.DefaultWithOptions(tools.Options{Background: m})
	result := registry.Dispatch(ctx, llm.ToolCall{Name: "shell", Input: json.RawMessage(`{"argv":["/usr/bin/false"],"background":true}`)})
	if result.IsError || result.BackgroundJobID == "" {
		t.Fatal(result)
	}
	m.mu.Lock()
	done := m.jobs[result.BackgroundJobID].done
	m.mu.Unlock()
	<-done
	starts, finishes := 0, 0
	for _, e := range recorder.snapshot() {
		if e.Identity.Model != "launch-model" {
			t.Fatal(e)
		}
		if e.Kind == execution.WorkTool && e.Phase == execution.WorkResult && len(e.Metrics) != 0 {
			t.Fatalf("launch counted as command: %+v", e)
		}
		if e.Kind == execution.WorkCommand {
			if e.Mode != "background" {
				t.Fatal(e)
			}
			if e.Phase == execution.WorkStart {
				starts++
			}
			if e.Phase == execution.WorkFinish {
				finishes++
				if e.Outcome != "failed" {
					t.Fatal(e)
				}
			}
		}
	}
	if starts != 1 || finishes != 1 {
		t.Fatal("command observations", starts, finishes)
	}
}

func TestExecutionDetachedFetchSurvivesParentCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, "SECRET body")
	}))
	defer server.Close()
	m := NewManager(Options{})
	recorder := &workRecorder{manager: m}
	ctx, cancel := context.WithCancel(execution.WithScope(context.Background(), execution.Scope{Observer: recorder, Identity: execution.Identity{Model: "launch-model"}}))
	defer cancel()
	registry := tools.DefaultWithOptions(tools.Options{Background: m})
	result := registry.Dispatch(ctx, llm.ToolCall{Name: "web_fetch", Input: json.RawMessage(fmt.Sprintf(`{"url":%q,"background":true}`, server.URL+"/SECRET"))})
	if result.IsError {
		t.Fatal(result)
	}
	<-started
	cancel()
	jobs := m.List()
	m.mu.Lock()
	done := m.jobs[jobs[0].ID].done
	m.mu.Unlock()
	close(release)
	<-done
	events := recorder.snapshot()
	count := 0
	for _, e := range events {
		if e.Kind == execution.WorkBackground {
			count++
			if e.Identity.Model != "launch-model" || e.Tool != "web_fetch" || e.Phase == execution.WorkFinish && e.Outcome != StatusCompleted {
				t.Fatal(e)
			}
		}
		if strings.Contains(fmt.Sprint(e), "SECRET") {
			t.Fatal("content leaked")
		}
	}
	if count != 2 {
		t.Fatal(events)
	}
}

type shutdownRecorder struct {
	workRecorder
	once sync.Once
}

func (r *shutdownRecorder) ObserveWork(e execution.WorkEvent) {
	r.workRecorder.ObserveWork(e)
	if e.Kind == execution.WorkBackground && e.Phase == execution.WorkStart {
		r.once.Do(func() { r.manager.Shutdown() })
	}
}

func TestExecutionStartObserverCanShutdown(t *testing.T) {
	m := NewManager(Options{})
	recorder := &shutdownRecorder{workRecorder: workRecorder{manager: m}}
	job, err := m.StartBackgroundJob(tools.BackgroundJobRequest{Kind: "SECRET", Execution: execution.Scope{Observer: recorder}, Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
		return tools.BackgroundJobResult{}, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	done := m.jobs[job.ID].done
	m.mu.Unlock()
	<-done
	events := recorder.snapshot()
	if len(events) != 2 || events[0].Phase != execution.WorkStart || events[1].Outcome != StatusAbandoned || events[0].Tool != "other" {
		t.Fatal(events)
	}
}

func TestExecutionExplicitWaitOutcomes(t *testing.T) {
	for _, want := range []string{"completed", "canceled", "timeout", "failed"} {
		t.Run(want, func(t *testing.T) {
			m := NewManager(Options{})
			recorder := &workRecorder{manager: m}
			ctx, cancel := context.WithCancel(execution.WithScope(context.Background(), execution.Scope{Observer: recorder}))
			defer cancel()
			release := make(chan struct{})
			job, err := m.StartBackgroundJob(tools.BackgroundJobRequest{Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
				<-release
				return tools.BackgroundJobResult{}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			done := m.jobs[job.ID].done
			m.mu.Unlock()
			ids := []string{job.ID}
			timeout := time.Second
			switch want {
			case "completed":
				close(release)
				<-done
			case "canceled":
				cancel()
			case "timeout":
				timeout = time.Nanosecond
			case "failed":
				ids = []string{"SECRET unknown"}
			}
			_, _ = NewJobsTool(m).waitObserved(ctx, ids, "all", timeout)
			if want != "completed" {
				close(release)
				<-done
			}
			events := recorder.snapshot()
			if len(events) != 2 || events[1].Outcome != want || events[1].RunDuration == nil {
				t.Fatal(events)
			}
		})
	}
}

func TestExecutionGroupRetainsActualBackgroundWorker(t *testing.T) {
	for _, outcome := range []string{StatusCompleted, StatusCanceled, StatusAbandoned, "panic"} {
		t.Run(outcome, func(t *testing.T) {
			m := NewManager(Options{})
			group := &execution.Group{}
			recorder := &workRecorder{manager: m}
			scope := execution.Scope{Observer: recorder, Group: group}
			started, release := make(chan struct{}), make(chan struct{})
			seenGroup := make(chan *execution.Group, 1)
			job, err := m.StartBackgroundJob(tools.BackgroundJobRequest{Kind: "shell", Execution: scope, ResourceKey: "repo", Access: tools.BackgroundAccessExclusive, Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
				seenGroup <- execution.FromContext(ctx).Group
				close(started)
				<-release
				if outcome == "panic" {
					panic("late panic")
				}
				return tools.BackgroundJobResult{Text: "late result"}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if got := <-seenGroup; got != group {
				t.Fatal("detached context lost root group")
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := group.Wait(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("live job escaped group: %v", err)
			}
			// A rejected lease must not add work to its request's independent group.
			rejectedGroup := &execution.Group{}
			_, err = m.StartBackgroundJob(tools.BackgroundJobRequest{Kind: "shell", ResourceKey: "repo", Access: tools.BackgroundAccessExclusive, Execution: execution.Scope{Group: rejectedGroup}, Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
				t.Error("conflicting job ran")
				return tools.BackgroundJobResult{}, nil
			}})
			if err == nil {
				t.Fatal("expected lease conflict")
			}
			if err := rejectedGroup.Wait(canceled); err != nil {
				t.Fatalf("rejected lease registered work: %v", err)
			}
			switch outcome {
			case StatusCanceled:
				m.Cancel(job.ID)
			case StatusAbandoned:
				m.Shutdown()
			}
			if err := group.Wait(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("logical termination released live job: %v", err)
			}
			m.mu.Lock()
			done := m.jobs[job.ID].done
			m.mu.Unlock()
			close(release)
			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
			defer waitCancel()
			if err := group.Wait(waitCtx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			default:
				t.Fatal("group released before job completion")
			}
			snapshot, ok := m.Get(job.ID)
			want := outcome
			if want == "panic" {
				want = StatusFailed
			}
			if !ok || snapshot.Status != want || outcome != "panic" && snapshot.Result.Text != "late result" {
				t.Fatalf("owner state incomplete: %+v", snapshot)
			}
			events := recorder.snapshot()
			if len(events) != 2 || events[1].Phase != execution.WorkFinish || events[1].Outcome != want {
				t.Fatalf("final observation incomplete: %+v", events)
			}
			if err := group.Wait(canceled); err != nil {
				t.Fatalf("group did not drain to zero: %v", err)
			}
		})
	}
}
