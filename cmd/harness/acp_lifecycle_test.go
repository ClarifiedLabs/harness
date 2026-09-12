package main

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"harness/internal/agent"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/plan"
	"harness/internal/todo"
)

func newACPCleanupTestRoot(t *testing.T) *acpRootSession {
	t.Helper()
	jobs := background.NewManager(background.Options{})
	return &acpRootSession{
		agent: &agent.Agent{}, path: t.TempDir(), now: time.Now,
		todos: todo.NewStore(), plans: plan.NewStore(), jobs: jobs,
		agentSessions: agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs}),
	}
}

func TestACPRootFactoryJoinsOwnedCleanupBeyondSettlement(t *testing.T) {
	root := newACPCleanupTestRoot(t)
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		root.cleanups = []func(context.Context){func(ctx context.Context) {
			close(started)
			<-release
			if err := ctx.Err(); err != nil {
				t.Errorf("owned cleanup inherited settlement deadline: %v", err)
			}
		}}
		tracked := trackACPRoot(root)
		factory := &acpRootFactory{roots: []*acpTrackedRoot{tracked}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Serve already exhausted its protocol close budget.
		tracked.startClose(ctx)
		<-started
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		// Let the factory's real final-settlement deadline expire virtually.
		<-time.After(2 * acpRootSettleTimeout)
		synctest.Wait()
		select {
		case <-done:
			t.Error("factory abandoned production cleanup at the telemetry deadline")
		default:
		}
		close(release)
		<-done
		<-tracked.closeDone
	})
}

func TestACPRootFactoryCleanupDoesNotJoinStuckPrompt(t *testing.T) {
	root := newACPCleanupTestRoot(t)
	root.mu.Lock()
	tracked := trackACPRoot(root)
	defer func() { root.mu.Unlock(); <-tracked.closeDone }()
	started, release := make(chan struct{}), make(chan struct{})
	root.cleanups = []func(context.Context){func(context.Context) { close(started); <-release }}
	factory := &acpRootFactory{roots: []*acpTrackedRoot{tracked}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { factory.close(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("owned cleanup waited for prompt mutex")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("factory joined prompt instead of only bounded owned cleanup")
	}
}

func TestACPRootFactoryJoinsProductionConstructionCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		factory := &acpRootFactory{}
		ctx, finish, err := factory.beginConstruction(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		<-ctx.Done()
		<-time.After(2 * acpRootSettleTimeout)
		synctest.Wait()
		select {
		case <-done:
			t.Error("factory abandoned production construction teardown")
		default:
		}
		finish()
		<-done
	})
}

func TestACPRootFactoryStuckProductionConstructionRemainsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		factory := &acpRootFactory{}
		ctx, finish, err := factory.beginConstruction(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer finish()
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		<-ctx.Done()
		<-done // A non-context-aware preflight must not hang the executable.
	})
}

func TestACPRootFactoryGenericCloseRemainsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := &lateACPRoot{closeStarted: make(chan struct{}), release: make(chan struct{}), finish: func() {}}
		tracked := trackACPRoot(root)
		factory := &acpRootFactory{roots: []*acpTrackedRoot{tracked}}
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		<-root.closeStarted
		<-done // Virtual deadline must release the factory, not the fake Close.
		close(root.release)
		<-tracked.closeDone
	})
}

type acpCleanupTestRuntime struct {
	started chan struct{}
	cleanup chan struct{}
	once    sync.Once
}

func (r *acpCleanupTestRuntime) Prompt(context.Context, agentsession.Prompt, agentsession.EventSink) (agentsession.Outcome, error) {
	return agentsession.Outcome{Reusable: true}, nil
}
func (r *acpCleanupTestRuntime) Close(ctx context.Context) error {
	r.once.Do(func() { close(r.started) })
	return ctx.Err()
}
func (r *acpCleanupTestRuntime) CleanupDone() <-chan struct{} { return r.cleanup }

func TestACPRootCloseStartsIndependentCleanupsBeforeManagerJoin(t *testing.T) {
	root := newACPCleanupTestRoot(t)
	synctest.Test(t, func(t *testing.T) {
		runtime := &acpCleanupTestRuntime{started: make(chan struct{}), cleanup: make(chan struct{})}
		started, err := root.agentSessions.Start(context.Background(), agentsession.StartRequest{
			Kind: "acp", Prompt: "open", Factory: func(context.Context, agentsession.SessionInfo) (agentsession.Runtime, error) {
				return runtime, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		for {
			changed := root.agentSessions.Changed()
			state, _ := root.agentSessions.Get(started.Session.ID)
			if state.State == agentsession.StateIdle {
				break
			}
			<-changed
		}
		first, second, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		root.cleanups = []func(context.Context){
			func(ctx context.Context) {
				close(first)
				<-release
				if ctx.Err() != nil {
					t.Error("first cleanup received depleted context")
				}
			},
			func(ctx context.Context) {
				close(second)
				<-release
				if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
					t.Error("cleanup needs a fresh internally bounded context")
				}
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() { done <- root.Close(ctx) }()
		<-runtime.started
		synctest.Wait()
		for _, started := range []<-chan struct{}{first, second} {
			select {
			case <-started:
			default:
				t.Error("independent cleanup blocked behind another owner")
			}
		}
		select {
		case <-done:
			t.Error("root returned before owned cleanup")
		default:
		}
		close(runtime.cleanup)
		close(release)
		<-done
	})
}
