package background

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/toolresult"
	"harness/internal/tools"
)

func TestManagerStartBackgroundJobCompletesAndDrainsContext(t *testing.T) {
	m := NewManager(Options{Now: func() time.Time {
		return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	}})

	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "run_command",
		Description: "echo hi",
		Run: func(ctx context.Context, id string) (tools.BackgroundJobResult, error) {
			if id == "" {
				t.Fatal("background job id should be passed to runner")
			}
			return tools.BackgroundJobResult{Text: "command output\n[exit code: 0]"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}

	done := waitJob(t, m, started.ID)
	if done.Status != StatusCompleted {
		t.Fatalf("job status = %q, want completed", done.Status)
	}
	if done.Kind != "run_command" || done.Task != "echo hi" {
		t.Fatalf("job identity = kind %q task %q", done.Kind, done.Task)
	}
	if !strings.Contains(done.Result.Text, "command output") {
		t.Fatalf("job text = %q", done.Result.Text)
	}
	ctx := m.DrainCompletedContext(nil)
	for _, want := range []string{"kind: run_command", "command output"} {
		if len(ctx) != 1 || !strings.Contains(ctx[0], want) {
			t.Fatalf("drained context missing %q: %+v", want, ctx)
		}
	}
}

type recordingArchiver struct {
	result llm.ToolResult
}

func (a *recordingArchiver) ArchiveToolResult(result llm.ToolResult) (toolresult.Archive, error) {
	a.result = result
	return toolresult.Archive{
		DisplayPath: "artifacts/tool-results/background.txt",
		ModelPath:   "/tmp/session/artifacts/tool-results/background.txt",
	}, nil
}

func TestManagerWaitsForPromptWorkAndDrainsUsageOnce(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:          "delegate",
		Description:   "inspect",
		Agent:         "explore",
		WaitForPrompt: true,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{
				Text:  "child report",
				Usage: llm.Usage{InputTokens: 70, OutputTokens: 30, CostUSD: 0.25, CostKnown: true},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun
	if !m.PendingPromptWork() {
		t.Fatal("running required job should be pending turn work")
	}
	close(release)
	usage, err := m.WaitForPromptWork(context.Background())
	if err != nil {
		t.Fatalf("WaitForPromptWork: %v", err)
	}
	if usage.InputTokens != 70 || usage.OutputTokens != 30 || usage.CostUSD != 0.25 || !usage.CostKnown {
		t.Fatalf("joined usage = %+v, want child usage", usage)
	}
	if got := m.DrainPromptWorkUsage(); got != (llm.Usage{}) {
		t.Fatalf("second usage drain = %+v, want zero", got)
	}
	snap, ok := m.Get(started.ID)
	if !ok || snap.Agent != "explore" || snap.Status != StatusCompleted {
		t.Fatalf("completed snapshot = %+v, ok=%v", snap, ok)
	}
	if !m.PendingPromptWork() {
		t.Fatal("completed result should remain pending until context delivery")
	}
	ctx := m.DrainCompletedContext(nil)
	if len(ctx) != 1 || !strings.Contains(ctx[0], "child report") {
		t.Fatalf("completed context = %v", ctx)
	}
	if m.PendingPromptWork() {
		t.Fatal("delivered completed result should no longer be pending")
	}
}

func TestManagerTruncatedContextUsesForegroundPreparationAndArchiveHint(t *testing.T) {
	m := NewManager(Options{})
	registry := &tools.Registry{}
	registry.SetResultLimits(1000, 1000)
	registry.SetToolResultLimits("run_command", 32, 1000)
	m.SetResultPreparer(registry.PrepareResult)

	full := strings.Repeat("background output ", 20)
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "run_command",
		Description: "noisy command",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Text: full}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	waitJob(t, m, started.ID)

	archiver := &recordingArchiver{}
	contexts := m.DrainCompletedContext(archiver)
	if len(contexts) != 1 {
		t.Fatalf("contexts = %d, want 1", len(contexts))
	}
	for _, want := range []string{
		"[truncated:",
		"full output archived at \"/tmp/session/artifacts/tool-results/background.txt\"",
		`use read_file {"path":`,
	} {
		if !strings.Contains(contexts[0], want) {
			t.Fatalf("background context missing %q:\n%s", want, contexts[0])
		}
	}
	if !archiver.result.Truncated || archiver.result.OriginalText != full {
		t.Fatalf("archived result did not preserve full output: %+v", archiver.result)
	}
	if archiver.result.ForID != started.ID {
		t.Fatalf("archive result id = %q, want %q", archiver.result.ForID, started.ID)
	}
}

func waitJob(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		snap, _ := m.Get(id)
		if snap.Status != StatusRunning {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("job %s still running", id)
		case <-tick.C:
		}
	}
}

func TestJobsToolCancelUnknownJob(t *testing.T) {
	tool := NewJobsTool(NewManager(Options{}))
	if _, err := tool.Run(context.Background(), []byte(`{"action":"cancel","id":"missing"}`)); err == nil {
		t.Fatalf("canceling an unknown job should return an error")
	}
}

func TestManagerWaitAlreadyCompletedDeliversContextAndPreservesNoticeAndUsage(t *testing.T) {
	m := NewManager(Options{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:          "delegate",
		Description:   "inspect",
		WaitForPrompt: true,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{
				Text:  "finished report",
				Usage: llm.Usage{InputTokens: 11, OutputTokens: 7, CostKnown: true},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	awaitJobDone(t, m, started.ID)

	result, err := m.Wait(context.Background(), started.ID, time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.TimedOut || result.NoRunning || len(result.Jobs) != 1 {
		t.Fatalf("wait result = %+v", result)
	}
	if result.Jobs[0].Status != StatusCompleted || result.Jobs[0].ContextPending {
		t.Fatalf("completed snapshot = %+v", result.Jobs[0])
	}
	if got := m.DrainCompletedContext(nil); len(got) != 0 {
		t.Fatalf("completion context delivered twice: %v", got)
	}
	if got := m.DrainNotices(); len(got) != 1 {
		t.Fatalf("completion notices = %v, want one", got)
	}
	usage := m.DrainPromptWorkUsage()
	if usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if got := m.DrainPromptWorkUsage(); got != (llm.Usage{}) {
		t.Fatalf("second usage drain = %+v, want zero", got)
	}
}

func TestManagerWaitSpecificJobCompletesOnNotification(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "run_command",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{Text: "done"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun

	waited := make(chan WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, waitError := m.Wait(context.Background(), started.ID, time.Second)
		waited <- result
		waitErr <- waitError
	}()
	select {
	case result := <-waited:
		t.Fatalf("wait returned before completion: %+v", result)
	default:
	}

	close(release)
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if len(result.Jobs) != 1 || result.Jobs[0].Status != StatusCompleted {
			t.Fatalf("wait result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not wake when the job completed")
	}
}

func TestManagerWaitAnySnapshotsRunningJobsAndReturnsFirstCompletion(t *testing.T) {
	m := NewManager(Options{})
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	first, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "first",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(firstStarted)
			<-firstRelease
			return tools.BackgroundJobResult{Text: "first"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start first: %v", err)
	}
	second, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "second",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(secondStarted)
			<-secondRelease
			return tools.BackgroundJobResult{Text: "second"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start second: %v", err)
	}
	<-firstStarted
	<-secondStarted

	waitEnteredSelect := make(chan struct{})
	waited := make(chan WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		waitCtx := &doneObservedContext{
			Context:  context.Background(),
			observed: waitEnteredSelect,
		}
		result, waitError := m.Wait(waitCtx, "", time.Second)
		waited <- result
		waitErr <- waitError
	}()
	<-waitEnteredSelect
	close(secondRelease)
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if len(result.Jobs) != 1 || result.Jobs[0].ID != second.ID {
			t.Fatalf("wait result = %+v, want second job only", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after a selected job completed")
	}

	close(firstRelease)
	awaitJobDone(t, m, first.ID)
}

type doneObservedContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestManagerWaitWithoutRunningJobsReturnsCurrentList(t *testing.T) {
	m := NewManager(Options{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "completed",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Text: "done"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	awaitJobDone(t, m, started.ID)

	result, err := m.Wait(context.Background(), "", time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !result.NoRunning || result.TimedOut || len(result.Jobs) != 1 {
		t.Fatalf("wait result = %+v", result)
	}
	if !result.Jobs[0].ContextPending {
		t.Fatal("an immediate list must not consume automatic completion context")
	}
}

func TestManagerWaitTimeoutReturnsLatestState(t *testing.T) {
	m := NewManager(Options{})
	release := make(chan struct{})
	startedRun := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "blocked",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun

	result, err := m.Wait(context.Background(), started.ID, time.Nanosecond)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !result.TimedOut || len(result.Jobs) != 1 || result.Jobs[0].Status != StatusRunning {
		t.Fatalf("timeout result = %+v", result)
	}

	close(release)
	awaitJobDone(t, m, started.ID)
}

func TestManagerWaitHonorsContextCancellationAndUnknownID(t *testing.T) {
	m := NewManager(Options{})
	if _, err := m.Wait(context.Background(), "missing", time.Second); err == nil {
		t.Fatal("waiting for an unknown job should fail")
	}

	release := make(chan struct{})
	startedRun := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "blocked",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Wait(ctx, started.ID, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context canceled", err)
	}
	close(release)
	awaitJobDone(t, m, started.ID)
}

func TestManagerWaitWakesAfterCancelFinishes(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "blocked",
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-ctx.Done()
			return tools.BackgroundJobResult{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun
	waited := make(chan WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, waitError := m.Wait(context.Background(), started.ID, time.Second)
		waited <- result
		waitErr <- waitError
	}()
	if _, ok := m.Cancel(started.ID); !ok {
		t.Fatal("Cancel did not find job")
	}
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if len(result.Jobs) != 1 || result.Jobs[0].Status != StatusCanceled {
			t.Fatalf("wait result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not wake after canceled job finished")
	}
}

func TestJobsToolWaitContract(t *testing.T) {
	tool := NewJobsTool(NewManager(Options{}))
	if !tool.ReadOnly([]byte(`{"action":"wait"}`)) {
		t.Fatal("wait should be read-only")
	}
	if tool.ReadOnly([]byte(`{"action":"cancel","id":"bg"}`)) {
		t.Fatal("cancel should not be read-only")
	}
	if got, err := tool.Run(context.Background(), []byte(`{"action":"wait"}`)); err != nil || got != "No running background jobs." {
		t.Fatalf("empty wait = %q, %v", got, err)
	}
	if _, err := tool.Run(context.Background(), []byte(`{"action":"wait","timeout_seconds":-1}`)); err == nil {
		t.Fatal("negative timeout should fail")
	}
	if timeout, ok := tool.SelfTimeout([]byte(`{"action":"wait"}`)); !ok || timeout != defaultWaitTimeout+waitDispatchGrace {
		t.Fatalf("default SelfTimeout = %s, %v", timeout, ok)
	}
	if timeout, ok := tool.SelfTimeout([]byte(`{"action":"wait","timeout_seconds":900}`)); !ok || timeout != 900*time.Second+waitDispatchGrace {
		t.Fatalf("explicit SelfTimeout = %s, %v", timeout, ok)
	}
	if _, ok := tool.SelfTimeout([]byte(`{"action":"list"}`)); ok {
		t.Fatal("list should not advertise a self timeout")
	}
}

func awaitJobDone(t *testing.T, m *Manager, id string) {
	t.Helper()
	m.mu.Lock()
	job := m.jobs[id]
	m.mu.Unlock()
	if job == nil {
		t.Fatalf("unknown job %s", id)
	}
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("job %s did not finish", id)
	}
}
