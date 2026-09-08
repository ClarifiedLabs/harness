package acptool

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/config"
	"harness/internal/execution"
	"harness/internal/tools"
)

type acpObserver struct {
	work   chan execution.WorkEvent
	models atomic.Int64
}

func (o *acpObserver) ObserveWork(e execution.WorkEvent)   { o.work <- e }
func (o *acpObserver) ObserveModel(execution.ModelEvent)   { o.models.Add(1) }
func (*acpObserver) ObservePrompt(execution.PromptEvent)   {}
func (*acpObserver) ObserveContext(execution.ContextEvent) {}

type acpScopeRuntime struct {
	calls   chan context.Context
	release chan struct{}
}

func (r *acpScopeRuntime) Prompt(ctx context.Context, _ agentsession.Prompt, _ agentsession.EventSink) (agentsession.Outcome, error) {
	r.calls <- ctx
	select {
	case <-r.release:
		return agentsession.Outcome{Reusable: true, Result: tools.BackgroundJobResult{Text: "remote reply"}}, nil
	case <-ctx.Done():
		return agentsession.Outcome{Reusable: true}, ctx.Err()
	}
}
func (*acpScopeRuntime) Close(context.Context) error { return nil }

func TestACPExecutionScopeFactoryAndFollowup(t *testing.T) {
	observer := &acpObserver{work: make(chan execution.WorkEvent, 8)}
	other := &acpObserver{work: make(chan execution.WorkEvent, 8)}
	parentScope := execution.Scope{Observer: observer, Identity: execution.Identity{Provider: "parent-provider", Model: "parent-model", Agent: "parent"}}
	otherScope := execution.Scope{Observer: other, Identity: execution.Identity{Provider: "switched-provider", Model: "switched-model", Agent: "other"}}
	want := execution.Identity{Agent: "remote", Delegate: "true"} // The protocol supplies no upstream provider/model.
	jobs := background.NewManager(background.Options{})
	manager := agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs})
	t.Cleanup(func() { _ = manager.CloseAll(context.Background()); jobs.ShutdownAndWait(time.Second) })
	runtime := &acpScopeRuntime{calls: make(chan context.Context, 2), release: make(chan struct{}, 2)}
	type factoryCall struct {
		ctx    context.Context
		target config.ACPTargetConfig
	}
	opened := make(chan factoryCall, 1)
	configured := config.ACPTargetConfig{Command: "remote-command", Args: []string{"--acp"}, Env: map[string]string{"TOKEN": "original"}, WorkspaceAccess: tools.BackgroundAccessReadOnly}
	tool := NewTool(manager, config.ACPConfig{Targets: map[string]config.ACPTargetConfig{"remote": configured}}, func(target config.ACPTargetConfig, _ string) agentsession.Factory {
		return func(ctx context.Context, _ agentsession.SessionInfo) (agentsession.Runtime, error) {
			opened <- factoryCall{ctx: ctx, target: target}
			return runtime, nil
		}
	})
	configured.Args[0], configured.Env["TOKEN"] = "changed", "changed"
	type contextKey struct{}
	parent, cancel := context.WithCancel(execution.WithScope(context.WithValue(context.Background(), contextKey{}, "opening"), parentScope))
	defer cancel()
	started, err := tool.RunResult(parent, json.RawMessage(`{"action":"start","target":"remote","prompt":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	factory := <-opened
	assertContext := func(ctx context.Context, value string) {
		t.Helper()
		scope := execution.FromContext(ctx)
		if scope.Observer != observer || scope.Identity != want || ctx.Err() != nil || ctx.Value(contextKey{}) != value {
			t.Fatalf("ACP context: scope=%+v error=%v value=%v", scope, ctx.Err(), ctx.Value(contextKey{}))
		}
	}
	assertContext(factory.ctx, "opening")
	if factory.target.Args[0] != "--acp" || factory.target.Env["TOKEN"] != "original" {
		t.Fatalf("factory config was not captured: %+v", factory.target)
	}
	assertContext(<-runtime.calls, "opening")
	runtime.release <- struct{}{}
	waitJob := func(id, outcome string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := jobs.WaitFor(ctx, []string{id}, "all", 2*time.Second); err != nil {
			t.Fatal(err)
		}
		job, ok := jobs.Get(id)
		if !ok || job.Status != outcome {
			t.Fatalf("job = %+v, want %s", job, outcome)
		}
		if job.Result.Usage.InputTokens != 0 || job.Result.Usage.OutputTokens != 0 {
			t.Fatalf("fabricated ACP usage: %+v", job.Result.Usage)
		}
		for _, phase := range []execution.WorkPhase{execution.WorkStart, execution.WorkFinish} {
			select {
			case event := <-observer.work:
				if event.Identity != want || event.Kind != execution.WorkBackground || event.Tool != "acp" || event.Phase != phase || event.Count != 1 {
					t.Fatalf("ACP lifecycle = %+v", event)
				}
				if phase == execution.WorkFinish && event.Outcome != outcome {
					t.Fatalf("ACP outcome = %+v", event)
				}
			case <-ctx.Done():
				t.Fatal("missing ACP lifecycle observation")
			}
		}
	}
	waitJob(started.BackgroundJobID, background.StatusCompleted)
	sessions := manager.List()
	if len(sessions) != 1 || sessions[0].State != agentsession.StateIdle {
		t.Fatalf("sessions = %+v", sessions)
	}
	caller, cancelCaller := context.WithCancel(execution.WithScope(context.WithValue(context.Background(), contextKey{}, "followup"), otherScope))
	next, err := manager.Prompt(caller, agentsession.PromptRequest{SessionID: sessions[0].ID, Prompt: "second"})
	cancelCaller()
	if err != nil {
		t.Fatal(err)
	}
	assertContext(<-runtime.calls, "followup")
	if next.Job.ID == started.BackgroundJobID {
		t.Fatal("followup reused immutable background job")
	}
	if id, err := manager.Interrupt(sessions[0].ID); err != nil || id != next.Job.ID {
		t.Fatalf("Interrupt = %q, %v", id, err)
	}
	waitJob(next.Job.ID, background.StatusCanceled)
	if observer.models.Load() != 0 || other.models.Load() != 0 || len(observer.work) != 0 || len(other.work) != 0 {
		t.Fatal("invented model observations, duplicated lifecycle, or attribution to later caller")
	}
}
