package background

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/tools"
)

func TestManagerShutdownClosesAdmissionBeforeDelayedRegistration(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	m := NewManager(Options{Now: func() time.Time {
		pauseOnce.Do(func() { close(entered); <-release })
		return time.Now()
	}})
	defer m.Shutdown()
	defer unblock()
	group := &execution.Group{}
	scope := execution.Scope{Group: group}
	launcherDone := scope.Track()
	returned := make(chan error, 1)
	go func() {
		defer launcherDone()
		_, err := m.StartBackgroundJob(tools.BackgroundJobRequest{Kind: "shell", Execution: scope, Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			<-ctx.Done()
			return tools.BackgroundJobResult{}, ctx.Err()
		}})
		returned <- err
	}()
	<-entered
	m.ShutdownAndWait(time.Second)
	unblock()
	if err := <-returned; !errors.Is(err, ErrClosed) {
		t.Fatalf("late admission error = %v, want ErrClosed", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := group.Wait(waitCtx); err != nil {
		t.Fatalf("group required another shutdown: %v", err)
	}
	if jobs := m.List(); len(jobs) != 0 {
		t.Fatalf("late launcher created jobs: %+v", jobs)
	}
}

func TestManagerClearReopensAdmissionWithoutBindingAcceptedLifetime(t *testing.T) {
	m := NewManager(Options{})
	defer m.Shutdown()
	m.Shutdown()
	m.Clear()
	group := &execution.Group{}
	oldCtx, oldCancel := context.WithCancel(context.Background())
	oldCancel()
	_, err := m.StartBackgroundJob(tools.BackgroundJobRequest{AdmissionContext: oldCtx, Execution: execution.Scope{Group: group}, Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
		t.Error("stale canceled launcher ran")
		return tools.BackgroundJobResult{}, nil
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stale admission error: %v", err)
	}
	if err := group.Wait(oldCtx); err != nil {
		t.Fatalf("rejected admission registered work: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	lifetime := make(chan error, 1)
	job, err := m.StartBackgroundJob(tools.BackgroundJobRequest{AdmissionContext: ctx, Execution: execution.Scope{Group: group}, Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
		<-release
		lifetime <- ctx.Err()
		return tools.BackgroundJobResult{Text: "done"}, nil
	}})
	if err != nil {
		t.Fatalf("clear did not reopen admission: %v", err)
	}
	cancel()
	close(release)
	if err := <-lifetime; err != nil {
		t.Fatalf("admission cancellation bound accepted lifetime: %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := group.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Get(job.ID); !ok || got.Status != StatusCompleted {
		t.Fatalf("accepted job: %+v", got)
	}
}

type gatedAdmission struct {
	manager *Manager
	entered chan context.Context
	release <-chan struct{}
}

func (g gatedAdmission) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	g.entered <- req.AdmissionContext
	<-g.release
	return g.manager.StartBackgroundJob(req)
}

func TestToolBackgroundAdmissionRejectsCanceledLaunchAfterClear(t *testing.T) {
	for _, name := range []string{"shell", "web_fetch"} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(Options{})
			defer m.Shutdown()
			entered, release := make(chan context.Context, 1), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			registry := tools.DefaultWithOptions(tools.Options{Background: gatedAdmission{manager: m, entered: entered, release: release}})
			group := &execution.Group{}
			ctx, cancel := context.WithCancel(execution.WithScope(context.Background(), execution.Scope{Group: group}))
			defer cancel()
			input := json.RawMessage(`{"argv":["/usr/bin/true"],"background":true}`)
			if name == "web_fetch" {
				input = json.RawMessage(`{"url":"http://unused.invalid","background":true}`)
			}
			returned := make(chan struct{})
			var result llm.ToolResult
			go func() { result = registry.Dispatch(ctx, llm.ToolCall{Name: name, Input: input}); close(returned) }()
			admission := <-entered
			if admission == nil {
				t.Fatal("tool omitted admission context")
			}
			cancel()
			<-returned
			if !result.IsError {
				t.Fatalf("canceled launch result: %+v", result)
			}
			m.Clear()
			unblock()
			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
			defer waitCancel()
			if err := group.Wait(waitCtx); err != nil {
				t.Fatal(err)
			}
			if jobs := m.List(); len(jobs) != 0 {
				t.Fatalf("stale launch entered new session: %+v", jobs)
			}
		})
	}
}
