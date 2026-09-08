package agentsession

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/background"
	"harness/internal/execution"
	"harness/internal/tools"
)

type sessionObserver struct {
	work   chan execution.WorkEvent
	models atomic.Int64
}

func newSessionObserver() *sessionObserver {
	return &sessionObserver{work: make(chan execution.WorkEvent, 16)}
}
func (o *sessionObserver) ObserveWork(e execution.WorkEvent)   { o.work <- e }
func (o *sessionObserver) ObserveModel(execution.ModelEvent)   { o.models.Add(1) }
func (*sessionObserver) ObservePrompt(execution.PromptEvent)   {}
func (*sessionObserver) ObserveContext(execution.ContextEvent) {}

type scopeRecordingStarter struct {
	manager  *background.Manager
	scopes   chan execution.Scope
	runScope *execution.Scope
}

func (s *scopeRecordingStarter) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	s.scopes <- req.Execution
	if s.runScope != nil {
		// A protocol-neutral starter may supply unrelated execution values. The
		// session's Run adapter must still pass its pinned scope to the driver.
		run := req.Run
		req.Run = func(ctx context.Context, id string) (tools.BackgroundJobResult, error) {
			return run(execution.WithScope(ctx, *s.runScope), id)
		}
	}
	return s.manager.StartBackgroundJob(req)
}

func waitSessionJobs(t *testing.T, jobs *background.Manager, ids ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := jobs.WaitFor(ctx, ids, "all", 2*time.Second); err != nil {
		t.Fatalf("wait for background jobs: %v", err)
	}
	for _, id := range ids {
		job, ok := jobs.Get(id)
		if !ok || job.Status == background.StatusRunning {
			t.Fatalf("background job not terminal: %+v", job)
		}
	}
}

func assertSessionScope(t *testing.T, got, want execution.Scope) {
	t.Helper()
	if got.Identity != want.Identity || got.Observer != want.Observer {
		t.Fatalf("execution scope = %+v, want %+v", got, want)
	}
}

func assertSessionWork(t *testing.T, observer *sessionObserver, identity execution.Identity, outcome string) {
	t.Helper()
	for _, phase := range []execution.WorkPhase{execution.WorkStart, execution.WorkFinish} {
		select {
		case event := <-observer.work:
			if event.Identity != identity || event.Kind != execution.WorkBackground || event.Phase != phase || event.Tool != "delegate" || event.Count != 1 {
				t.Fatalf("background lifecycle = %+v", event)
			}
			if phase == execution.WorkFinish && event.Outcome != outcome {
				t.Fatalf("background outcome = %q, want %q", event.Outcome, outcome)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing background %s observation", phase)
		}
	}
}

func TestManagerExecutionPinnedAcrossReuseAndCancellation(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "inherited"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			observer, other := newSessionObserver(), newSessionObserver()
			want := execution.Scope{Observer: observer, Identity: execution.Identity{Provider: "child-provider", Model: "child-model", Agent: "worker", Delegate: "true"}}
			otherScope := execution.Scope{Observer: other, Identity: execution.Identity{Provider: "parent-provider", Model: "switched-model", Agent: "parent"}}
			jobs := background.NewManager(background.Options{})
			starter := &scopeRecordingStarter{manager: jobs, scopes: make(chan execution.Scope, 4), runScope: &otherScope}
			manager := NewManager(Options{Background: starter, Canceler: jobs})
			t.Cleanup(func() { _ = manager.CloseAll(context.Background()); jobs.ShutdownAndWait(time.Second) })
			runtime := newFakeRuntime()
			factoryContext := make(chan context.Context, 1)
			type valueKey struct{}
			parentScope := want
			req := StartRequest{Kind: "delegate", Prompt: "first", Factory: func(ctx context.Context, _ SessionInfo) (Runtime, error) {
				factoryContext <- ctx
				return runtime, nil
			}}
			if explicit {
				req.Execution = want
				parentScope = otherScope
			}
			parent, cancel := context.WithCancel(execution.WithScope(context.WithValue(context.Background(), valueKey{}, "opening-value"), parentScope))
			defer cancel()
			started, err := manager.Start(parent, req)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			req.Execution = otherScope // Mutating the caller's copy must not rebind the session.
			assertSessionScope(t, <-starter.scopes, want)
			opened := <-factoryContext
			assertSessionScope(t, execution.FromContext(opened), want)
			if opened.Err() != nil || opened.Value(valueKey{}) != "opening-value" {
				t.Fatalf("factory context lost detached values: err=%v value=%v", opened.Err(), opened.Value(valueKey{}))
			}
			first := <-runtime.calls
			assertSessionScope(t, execution.FromContext(first.ctx), want)
			if first.ctx.Err() != nil {
				t.Fatalf("parent canceled runtime: %v", first.ctx.Err())
			}
			runtime.releases <- Outcome{Reusable: true}
			waitSessionJobs(t, jobs, started.Job.ID)
			waitState(t, manager, started.Session.ID, StateIdle)
			assertSessionWork(t, observer, want.Identity, background.StatusCompleted)

			for operation := 2; operation <= 3; operation++ {
				caller, cancelCaller := context.WithCancel(execution.WithScope(context.WithValue(context.Background(), valueKey{}, "followup-value"), otherScope))
				next, err := manager.Prompt(caller, PromptRequest{SessionID: started.Session.ID, Prompt: "followup"})
				cancelCaller()
				if err != nil {
					t.Fatal(err)
				}
				if next.Operation != operation || next.Job.ID == started.Job.ID {
					t.Fatalf("followup = %+v", next)
				}
				assertSessionScope(t, <-starter.scopes, want)
				call := <-runtime.calls
				assertSessionScope(t, execution.FromContext(call.ctx), want)
				if call.ctx.Err() != nil || call.ctx.Value(valueKey{}) != "followup-value" {
					t.Fatalf("followup context lost detached values: err=%v value=%v", call.ctx.Err(), call.ctx.Value(valueKey{}))
				}
				outcome := background.StatusCompleted
				if operation == 2 {
					id, err := manager.Interrupt(started.Session.ID)
					if err != nil || id != next.Job.ID {
						t.Fatalf("Interrupt = %q, %v", id, err)
					}
					outcome = background.StatusCanceled
				} else {
					runtime.releases <- Outcome{Reusable: true}
				}
				waitSessionJobs(t, jobs, next.Job.ID)
				waitState(t, manager, started.Session.ID, StateIdle)
				assertSessionWork(t, observer, want.Identity, outcome)
			}
			if observer.models.Load() != 0 || other.models.Load() != 0 || len(observer.work) != 0 || len(other.work) != 0 {
				t.Fatal("fabricated model events, duplicate lifecycle, or observations attributed to the later caller")
			}
		})
	}
}

func TestManagerExplicitUnobservedIdentityDoesNotInheritCaller(t *testing.T) {
	observer := newSessionObserver()
	jobs := background.NewManager(background.Options{})
	starter := &scopeRecordingStarter{manager: jobs, scopes: make(chan execution.Scope, 1)}
	manager := NewManager(Options{Background: starter, Canceler: jobs})
	t.Cleanup(func() { _ = manager.CloseAll(context.Background()); jobs.ShutdownAndWait(time.Second) })
	runtime := newFakeRuntime()
	want := execution.Scope{Identity: execution.Identity{Agent: "remote"}}
	ctx := execution.WithScope(context.Background(), execution.Scope{Observer: observer, Identity: execution.Identity{Model: "parent"}})
	started, err := manager.Start(ctx, StartRequest{Kind: "acp", Prompt: "first", Execution: want, Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil }})
	if err != nil {
		t.Fatal(err)
	}
	assertSessionScope(t, <-starter.scopes, want)
	call := <-runtime.calls
	assertSessionScope(t, execution.FromContext(call.ctx), want)
	runtime.releases <- Outcome{Reusable: true}
	waitSessionJobs(t, jobs, started.Job.ID)
	if len(observer.work) != 0 {
		t.Fatal("explicit unobserved identity inherited caller observation")
	}
}
