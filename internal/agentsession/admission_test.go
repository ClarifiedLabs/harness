package agentsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/background"
	"harness/internal/tools"
)

// Pause after the session's entry-point check but before background admission.
// Only one operation is gated, allowing the same runtime to be reused afterward.
type admissionGateStarter struct {
	manager   *background.Manager
	operation int
	entered   chan context.Context
	release   chan struct{}
	once      sync.Once
}

func (s *admissionGateStarter) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	if req.Operation == s.operation {
		s.once.Do(func() {
			s.entered <- req.AdmissionContext
			<-s.release
		})
	}
	return s.manager.StartBackgroundJob(req)
}

func TestManagerRejectsCanceledAdmissionAfterBackgroundClear(t *testing.T) {
	for _, opening := range []bool{true, false} {
		name, operation := "opening", 1
		if !opening {
			name, operation = "followup", 2
		}
		t.Run(name, func(t *testing.T) {
			jobs := background.NewManager(background.Options{})
			starter := &admissionGateStarter{manager: jobs, operation: operation, entered: make(chan context.Context, 1), release: make(chan struct{})}
			manager := NewManager(Options{Background: starter, Canceler: jobs, MaxSessions: 1})
			old, cancel := context.WithCancel(context.Background())
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(starter.release) }) }
			t.Cleanup(func() {
				cancel()
				release()
				_ = manager.CloseAll(context.Background())
				jobs.ShutdownAndWait(time.Second)
			})
			runtime := newFakeRuntime()
			var factories atomic.Int64
			req := StartRequest{Kind: "acp", Prompt: "opening", Factory: func(context.Context, SessionInfo) (Runtime, error) {
				factories.Add(1)
				return runtime, nil
			}}
			var id string
			if !opening {
				started, err := manager.Start(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				id = started.Session.ID
				<-runtime.calls
				runtime.releases <- Outcome{Reusable: true}
				waitSessionJobs(t, jobs, started.Job.ID)
				waitState(t, manager, id, StateIdle)
			}
			returned := make(chan error, 1)
			go func() {
				if opening {
					_, err := manager.Start(old, req)
					returned <- err
				} else {
					_, err := manager.Prompt(old, PromptRequest{SessionID: id, Prompt: "stale followup"})
					returned <- err
				}
			}()
			admission := <-starter.entered
			if admission != old {
				t.Fatalf("admission context = %v, want current caller", admission)
			}
			sessions := manager.List()
			if len(sessions) != 1 {
				t.Fatalf("sessions = %+v", sessions)
			}
			id = sessions[0].ID
			manager.mu.Lock()
			done := manager.sessions[id].operationDone
			manager.mu.Unlock()
			cancel()
			jobs.Clear() // Reopens admission; the canceled old caller must still fail.
			release()
			if err := <-returned; !errors.Is(err, context.Canceled) {
				t.Fatalf("admission error = %v", err)
			}
			if got := jobs.List(); len(got) != 0 {
				t.Fatalf("stale job admitted after Clear: %+v", got)
			}
			select {
			case <-done:
			default:
				t.Fatal("rejected operation did not release close waiters")
			}
			manager.mu.Lock()
			s := manager.sessions[id]
			retainedFactory, retainedOperation := s.factory != nil, s.operationDone != nil
			manager.mu.Unlock()
			if retainedFactory || retainedOperation {
				t.Fatal("rejected operation retained startup resources")
			}
			snap, _ := manager.Get(id)
			if !snap.Progress.Finished {
				t.Fatal("rejected operation left progress running")
			}
			if opening {
				if snap.State != StateFailed || factories.Load() != 0 {
					t.Fatalf("rejected startup = %+v; factories = %d", snap, factories.Load())
				}
				if err := manager.Close(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				// Rejected startup consumes no live-session slot or runtime factory.
				started, err := manager.Start(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				<-runtime.calls
				runtime.releases <- Outcome{Reusable: true}
				waitSessionJobs(t, jobs, started.Job.ID)
			} else {
				if snap.State != StateIdle || factories.Load() != 1 {
					t.Fatalf("rejected followup = %+v; factories = %d", snap, factories.Load())
				}
				next, err := manager.Prompt(context.Background(), PromptRequest{SessionID: id, Prompt: "fresh followup"})
				if err != nil {
					t.Fatal(err)
				}
				<-runtime.calls
				runtime.releases <- Outcome{Reusable: true}
				waitSessionJobs(t, jobs, next.Job.ID)
				if factories.Load() != 1 {
					t.Fatal("followup replaced reusable runtime")
				}
			}
		})
	}
}

func TestManagerAcceptedAdmissionRemainsDetached(t *testing.T) {
	manager, jobs := newTestManager()
	t.Cleanup(func() { _ = manager.CloseAll(context.Background()); jobs.ShutdownAndWait(time.Second) })
	runtime := newFakeRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, err := manager.Start(ctx, StartRequest{Kind: "acp", Prompt: "opening", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for operation := 1; operation <= 2; operation++ {
		id := started.Job.ID
		if operation == 2 {
			ctx, cancel = context.WithCancel(context.Background())
			next, err := manager.Prompt(ctx, PromptRequest{SessionID: started.Session.ID, Prompt: "followup"})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			id = next.Job.ID
		}
		call := <-runtime.calls
		cancel()
		if call.ctx.Err() != nil {
			t.Fatalf("accepted operation canceled with launcher: %v", call.ctx.Err())
		}
		runtime.releases <- Outcome{Reusable: true}
		waitSessionJobs(t, jobs, id)
		job, ok := jobs.Get(id)
		if !ok || job.Status != background.StatusCompleted {
			t.Fatalf("accepted operation = %+v", job)
		}
		waitState(t, manager, started.Session.ID, StateIdle)
	}
}
