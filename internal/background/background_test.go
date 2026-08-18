package background

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/toolresult"
	"harness/internal/tools"
)

func TestJobsToolOnlyCancelRequiresSequentialDispatch(t *testing.T) {
	tool := NewJobsTool(NewManager(Options{}))
	for _, action := range []string{"", "list", "get", "wait"} {
		input, err := json.Marshal(map[string]string{"action": action})
		if err != nil {
			t.Fatal(err)
		}
		if tool.RequiresSequential(input) {
			t.Errorf("action %q unexpectedly requires sequential dispatch", action)
		}
	}
	if !tool.RequiresSequential(json.RawMessage(`{"action":"cancel","id":"job"}`)) {
		t.Fatal("cancel must preserve co-issued state-change order")
	}
	if tool.RequiresSequential(json.RawMessage(`{"action":`)) {
		t.Fatal("invalid input cannot perform cancellation and must not become a barrier")
	}
}

func TestManagerStartBackgroundJobCompletesAndDrainsContext(t *testing.T) {
	m := NewManager(Options{Now: func() time.Time {
		return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	}})

	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "shell",
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
	if done.Kind != "shell" || done.Task != "echo hi" {
		t.Fatalf("job identity = kind %q task %q", done.Kind, done.Task)
	}
	if !strings.Contains(done.Result.Text, "command output") {
		t.Fatalf("job text = %q", done.Result.Text)
	}
	ctx := m.DrainCompletedContext(nil)
	for _, want := range []string{"kind: shell", "command output"} {
		if len(ctx) != 1 || !strings.Contains(ctx[0], want) {
			t.Fatalf("drained context missing %q: %+v", want, ctx)
		}
	}
}

func TestManagerDrainsCompletedDiagnosticsOnce(t *testing.T) {
	m := NewManager(Options{})
	metrics := map[string]int{
		tools.CommandMetricOutcomeAvailable: 1,
		tools.CommandMetricFailed:           1,
		tools.CommandMetricStepsTotal:       3,
		tools.CommandMetricStepsFailed:      1,
	}
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Text: "batch failed", Metrics: metrics}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	identity := DiagnosticIdentity{
		Agent: "launch-agent", ModelTarget: "target-a", Provider: "provider-a", APIType: "responses", Model: "model-a",
	}
	if !m.SetDiagnosticIdentity(started.ID, identity) {
		t.Fatal("SetDiagnosticIdentity = false")
	}
	waitJob(t, m, started.ID)

	// The manager and its snapshots own independent metric maps.
	metrics[tools.CommandMetricStepsTotal] = 99
	snapshot, ok := m.Get(started.ID)
	if !ok || snapshot.Result.Metrics[tools.CommandMetricStepsTotal] != 3 {
		t.Fatalf("stored metrics = %+v", snapshot.Result.Metrics)
	}
	snapshot.Result.Metrics[tools.CommandMetricStepsTotal] = 88
	fresh, _ := m.Get(started.ID)
	if fresh.Result.Metrics[tools.CommandMetricStepsTotal] != 3 {
		t.Fatalf("snapshot mutated manager metrics: %+v", fresh.Result.Metrics)
	}

	diagnostics := m.DrainCompletedDiagnostics()
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", diagnostics)
	}
	got := diagnostics[0]
	if got.ID != started.ID || got.Kind != "shell" || got.Status != StatusCompleted ||
		got.Metrics[tools.CommandMetricFailed] != 1 || got.Metrics[tools.CommandMetricStepsTotal] != 3 ||
		got.Identity == nil || *got.Identity != identity {
		t.Fatalf("diagnostic = %+v", got)
	}
	got.Metrics[tools.CommandMetricStepsTotal] = 77
	if again := m.DrainCompletedDiagnostics(); len(again) != 0 {
		t.Fatalf("second diagnostics drain = %+v, want empty", again)
	}
}

func TestManagerShutdownAndWaitRetainsCancellationDiagnostics(t *testing.T) {
	m := NewManager(Options{})
	started := make(chan struct{})
	job, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			close(started)
			<-ctx.Done()
			return tools.BackgroundJobResult{Metrics: map[string]int{
				tools.CommandMetricOutcomeAvailable: 1,
				tools.CommandMetricCancelled:        1,
				tools.CommandMetricStepsCancelled:   1,
			}}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-started

	m.ShutdownAndWait(time.Second)
	snapshot, ok := m.Get(job.ID)
	if !ok || snapshot.Status != StatusAbandoned || snapshot.Result.Metrics[tools.CommandMetricCancelled] != 1 {
		t.Fatalf("shutdown snapshot = %+v", snapshot)
	}
	diagnostics := m.DrainCompletedDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0].ID != job.ID || diagnostics[0].Metrics[tools.CommandMetricStepsCancelled] != 1 {
		t.Fatalf("shutdown diagnostics = %+v", diagnostics)
	}
}

func TestManagerAcknowledgesDiagnosticsAfterPersistence(t *testing.T) {
	m := NewManager(Options{})
	job, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Metrics: map[string]int{tools.CommandMetricSucceeded: 1}}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	waitJob(t, m, job.ID)
	for attempt := 0; attempt < 2; attempt++ {
		pending := m.PeekCompletedDiagnostics()
		if len(pending) != 1 || pending[0].ID != job.ID {
			t.Fatalf("pending diagnostics attempt %d = %+v", attempt, pending)
		}
	}
	if !m.AcknowledgeCompletedDiagnostic(job.ID) {
		t.Fatal("AcknowledgeCompletedDiagnostic = false")
	}
	if pending := m.PeekCompletedDiagnostics(); len(pending) != 0 {
		t.Fatalf("pending diagnostics after acknowledgement = %+v", pending)
	}
}

func TestBackgroundJobsToolDescriptionFitsBudget(t *testing.T) {
	if got := len((&JobsTool{}).Description()); got > 80 {
		t.Fatalf("background_jobs description = %d bytes, budget 80", got)
	}
}

func TestBackgroundJobsPreservesSchemaDescriptions(t *testing.T) {
	tool := NewJobsTool(nil)
	if !tool.PreserveSchemaDescriptions() {
		t.Fatal("background_jobs must opt into schema descriptions")
	}
	registry := &tools.Registry{}
	registry.Register(tool)
	if parameters := string(registry.Specs()[0].Parameters); !strings.Contains(parameters, `"description":"Default: list. Use wait, not polling, when completion matters."`) {
		t.Fatalf("model-facing schema lost parameter description: %s", parameters)
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
	registry.SetToolResultLimits("shell", 32, 1000)
	m.SetResultPreparer(registry.PrepareResultWithOriginal)

	full := strings.Repeat("background output ", 20)
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "shell",
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
		`use read {"path":`,
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

func TestManagerArchivesProactiveBackgroundOriginal(t *testing.T) {
	m := NewManager(Options{})
	registry := &tools.Registry{}
	registry.SetResultLimits(64*1024, 1000)
	m.SetResultPreparer(registry.PrepareResultWithOriginal)

	full := strings.Repeat("verbose command output\n", 200)
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "shell",
		Description: "large check",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{
				Text:         "PASS large check (1s; exit 0; 4.5KB output)",
				OriginalText: full,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	awaitJobDone(t, m, started.ID)

	archiver := &recordingArchiver{}
	contexts := m.DrainCompletedContext(archiver)
	if len(contexts) != 1 ||
		!strings.Contains(contexts[0], "PASS large check") ||
		!strings.Contains(contexts[0], toolresult.ArchivedHintMarker) {
		t.Fatalf("background receipt context = %v", contexts)
	}
	if archiver.result.OriginalText != full || archiver.result.Text == full || !archiver.result.Truncated {
		t.Fatalf("archived proactive result = %+v", archiver.result)
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

func TestManagerResourceLeasesAllowReadsAndRejectConflictingAccess(t *testing.T) {
	m := NewManager(Options{})
	resource := t.TempDir()
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})

	first, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "delegate",
		ResourceKey: resource,
		Access:      tools.BackgroundAccessReadOnly,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(firstStarted)
			<-firstRelease
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("start first read: %v", err)
	}
	<-firstStarted
	second, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "rg",
		ResourceKey: resource,
		Access:      tools.BackgroundAccessReadOnly,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(secondStarted)
			<-secondRelease
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("start second read: %v", err)
	}
	<-secondStarted
	if first.ResourceKey == "" || first.ResourceKey != second.ResourceKey ||
		first.Access != tools.BackgroundAccessReadOnly || second.Access != tools.BackgroundAccessReadOnly {
		t.Fatalf("read leases = %+v / %+v", first, second)
	}
	firstSnapshot, _ := m.Get(first.ID)
	for label, output := range map[string]string{
		"get":  formatGet(firstSnapshot),
		"list": formatList(m.List()),
	} {
		if !strings.Contains(output, first.ResourceKey) || !strings.Contains(output, tools.BackgroundAccessReadOnly) {
			t.Fatalf("%s output omits lease: %q", label, output)
		}
	}

	_, err = m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "shell",
		ResourceKey: resource,
		Access:      tools.BackgroundAccessExclusive,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), first.ID) ||
		!strings.Contains(err.Error(), tools.BackgroundAccessReadOnly) {
		t.Fatalf("exclusive conflict = %v, want deterministic first job %s", err, first.ID)
	}

	close(firstRelease)
	close(secondRelease)
	awaitJobDone(t, m, first.ID)
	awaitJobDone(t, m, second.ID)
	completedContext := m.DrainCompletedContext(nil)
	if len(completedContext) != 2 ||
		!strings.Contains(completedContext[0], "resource: "+first.ResourceKey) ||
		!strings.Contains(completedContext[0], "access: "+tools.BackgroundAccessReadOnly) {
		t.Fatalf("completion context omits lease: %v", completedContext)
	}

	exclusiveStarted := make(chan struct{})
	exclusiveRelease := make(chan struct{})
	exclusive, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "shell",
		ResourceKey: resource,
		Access:      tools.BackgroundAccessExclusive,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(exclusiveStarted)
			<-exclusiveRelease
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("start exclusive after reads: %v", err)
	}
	<-exclusiveStarted
	for _, access := range []string{tools.BackgroundAccessReadOnly, tools.BackgroundAccessExclusive} {
		_, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
			Kind:        "delegate",
			ResourceKey: resource,
			Access:      access,
			Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
				return tools.BackgroundJobResult{}, nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), exclusive.ID) {
			t.Fatalf("%s conflict = %v, want active job %s", access, err, exclusive.ID)
		}
	}
	unrelated, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:        "shell",
		ResourceKey: t.TempDir(),
		Access:      tools.BackgroundAccessExclusive,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("start unrelated exclusive: %v", err)
	}
	awaitJobDone(t, m, unrelated.ID)
	close(exclusiveRelease)
	awaitJobDone(t, m, exclusive.ID)
}

func TestManagerResourceLeaseReleaseLifecycle(t *testing.T) {
	resource := t.TempDir()
	startImmediate := func(t *testing.T, m *Manager, run func(context.Context, string) (tools.BackgroundJobResult, error)) tools.BackgroundJobInfo {
		t.Helper()
		started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
			Kind:        "shell",
			ResourceKey: resource,
			Access:      tools.BackgroundAccessExclusive,
			Run:         run,
		})
		if err != nil {
			t.Fatalf("start exclusive job: %v", err)
		}
		return started
	}

	t.Run("completion and failure", func(t *testing.T) {
		m := NewManager(Options{})
		completed := startImmediate(t, m, func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		})
		awaitJobDone(t, m, completed.ID)
		if got, _ := m.Get(completed.ID); got.Status != StatusCompleted {
			t.Fatalf("completed status = %q", got.Status)
		}
		failed := startImmediate(t, m, func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, errors.New("failed")
		})
		awaitJobDone(t, m, failed.ID)
		if got, _ := m.Get(failed.ID); got.Status != StatusFailed {
			t.Fatalf("failed status = %q", got.Status)
		}
		afterFailure := startImmediate(t, m, func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		})
		awaitJobDone(t, m, afterFailure.ID)
	})

	t.Run("cancellation retains lease through cleanup", func(t *testing.T) {
		m := NewManager(Options{})
		startedRun := make(chan struct{})
		cancelObserved := make(chan struct{})
		cleanupRelease := make(chan struct{})
		started := startImmediate(t, m, func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-ctx.Done()
			close(cancelObserved)
			<-cleanupRelease
			return tools.BackgroundJobResult{}, ctx.Err()
		})
		<-startedRun
		if _, ok := m.Cancel(started.ID); !ok {
			t.Fatal("cancel did not find job")
		}
		<-cancelObserved
		if _, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
			Kind:        "delegate",
			ResourceKey: resource,
			Access:      tools.BackgroundAccessReadOnly,
			Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
				return tools.BackgroundJobResult{}, nil
			},
		}); err == nil || !strings.Contains(err.Error(), started.ID) {
			t.Fatalf("cleanup conflict = %v, want active canceled job %s", err, started.ID)
		}
		close(cleanupRelease)
		awaitJobDone(t, m, started.ID)
		if got, _ := m.Get(started.ID); got.Status != StatusCanceled {
			t.Fatalf("canceled status = %q", got.Status)
		}
		afterCancel := startImmediate(t, m, func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		})
		awaitJobDone(t, m, afterCancel.ID)
	})

	t.Run("abandonment releases lease", func(t *testing.T) {
		m := NewManager(Options{})
		startedRun := make(chan struct{})
		cancelObserved := make(chan struct{})
		cleanupRelease := make(chan struct{})
		returned := make(chan struct{})
		started := startImmediate(t, m, func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-ctx.Done()
			close(cancelObserved)
			<-cleanupRelease
			close(returned)
			return tools.BackgroundJobResult{Text: "[delegate completion: unknown, report host/unavailable]"}, ctx.Err()
		})
		<-startedRun
		m.Shutdown()
		<-cancelObserved
		if got, _ := m.Get(started.ID); got.Status != StatusAbandoned || !got.ContextPending {
			t.Fatalf("abandoned snapshot = %+v", got)
		}
		afterAbandon := startImmediate(t, m, func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		})
		awaitJobDone(t, m, afterAbandon.ID)
		m.mu.Lock()
		done := m.jobs[started.ID].done
		m.mu.Unlock()
		close(cleanupRelease)
		<-returned
		<-done
		if got, _ := m.Get(started.ID); got.Status != StatusAbandoned || !strings.Contains(got.Result.Text, "host/unavailable") {
			t.Fatalf("late abandoned result = %+v", got)
		}
	})
}

func TestJobsToolCancelUnknownJob(t *testing.T) {
	tool := NewJobsTool(NewManager(Options{}))
	if _, err := tool.Run(context.Background(), []byte(`{"action":"cancel","id":"missing"}`)); err == nil {
		t.Fatalf("canceling an unknown job should return an error")
	}
}

func TestJobsToolRejectsWaitOnlyArgsOnNonWaitActions(t *testing.T) {
	tool := NewJobsTool(NewManager(Options{}))
	for _, input := range []string{
		`{"action":"list","ids":["a"]}`,
		`{"action":"list","until":"all"}`,
		`{"action":"get","id":"a","ids":["a"]}`,
		`{"action":"get","id":"a","until":"first"}`,
		`{"action":"cancel","id":"a","ids":["a"]}`,
		`{"action":"cancel","id":"a","until":"all"}`,
	} {
		if _, err := tool.Run(context.Background(), []byte(input)); err == nil || !strings.Contains(err.Error(), "only valid for wait") {
			t.Fatalf("%s: err = %v, want wait-only rejection", input, err)
		}
	}
}

func TestManagerWaitAlreadyCompletedDeliversContextAndPreservesNoticeAndUsage(t *testing.T) {
	m := NewManager(Options{})
	usage := llm.Usage{
		InputTokens: 11, CacheReadTokens: 5, OutputTokens: 7, ReasoningTokens: 3,
		CacheWriteTokens: 2, CacheWrite1hTokens: 1, CostUSD: 0.25, CostKnown: true,
	}
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:          "delegate",
		Description:   "inspect",
		WaitForPrompt: true,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{
				Text:           "finished report",
				TranscriptPath: "/tmp/child",
				Usage:          usage,
				Compactions:    3,
				Metrics:        map[string]int{"child": 1},
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
	notices := m.DrainNoticeSnapshots()
	if len(notices) != 1 {
		t.Fatalf("completion notices = %+v, want one", notices)
	}
	notice := notices[0]
	if notice.ID != started.ID || notice.Status != StatusCompleted || notice.Result.Usage != usage ||
		notice.Result.Compactions != 3 || notice.Result.TranscriptPath != "/tmp/child" || notice.NoticePending {
		t.Fatalf("notice snapshot = %+v", notice)
	}
	notice.Result.Metrics["child"] = 99
	stored, _ := m.Get(started.ID)
	if stored.Result.Metrics["child"] != 1 {
		t.Fatalf("notice snapshot mutated manager result: %+v", stored.Result.Metrics)
	}
	if got := m.DrainNoticeSnapshots(); len(got) != 0 {
		t.Fatalf("second notice drain = %+v, want empty", got)
	}
	diagnostics := m.DrainCompletedDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0].ID != started.ID || diagnostics[0].Metrics["child"] != 1 {
		t.Fatalf("diagnostics after notice drain = %+v, want independent result", diagnostics)
	}
	if got := m.DrainCompletedDiagnostics(); len(got) != 0 {
		t.Fatalf("second diagnostics drain = %+v, want empty", got)
	}
	drainedUsage := m.DrainPromptWorkUsage()
	if drainedUsage != usage {
		t.Fatalf("usage = %+v, want %+v", drainedUsage, usage)
	}
	if got := m.DrainPromptWorkUsage(); got != (llm.Usage{}) {
		t.Fatalf("second usage drain = %+v, want zero", got)
	}
}

func TestManagerNoticeSnapshotsRetainFailedAndCanceledPartialResults(t *testing.T) {
	m := NewManager(Options{})
	failed, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "delegate",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{
				Usage:       llm.Usage{InputTokens: 5, OutputTokens: 2},
				Compactions: 1,
			}, errors.New("provider unavailable")
		},
	})
	if err != nil {
		t.Fatalf("start failed job: %v", err)
	}
	awaitJobDone(t, m, failed.ID)

	startedRun := make(chan struct{})
	canceled, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "delegate",
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-ctx.Done()
			return tools.BackgroundJobResult{
				TranscriptPath: "/tmp/canceled-child",
				Usage:          llm.Usage{InputTokens: 9, ReasoningTokens: 4},
				Compactions:    2,
			}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("start canceled job: %v", err)
	}
	<-startedRun
	if _, ok := m.Cancel(canceled.ID); !ok {
		t.Fatal("cancel job = false")
	}
	awaitJobDone(t, m, canceled.ID)

	notices := m.DrainNoticeSnapshots()
	if len(notices) != 2 || notices[0].ID != failed.ID || notices[1].ID != canceled.ID {
		t.Fatalf("notice order = %+v", notices)
	}
	if notices[0].Status != StatusFailed || notices[0].Error != "provider unavailable" ||
		notices[0].Result.Usage.InputTokens != 5 || notices[0].Result.Compactions != 1 {
		t.Fatalf("failed notice = %+v", notices[0])
	}
	if notices[1].Status != StatusCanceled || notices[1].Result.Usage.InputTokens != 9 ||
		notices[1].Result.Usage.ReasoningTokens != 4 || notices[1].Result.Compactions != 2 ||
		notices[1].Result.TranscriptPath != "/tmp/canceled-child" {
		t.Fatalf("canceled notice = %+v", notices[1])
	}
}

func TestManagerWaitSpecificJobCompletesOnNotification(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
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

func TestManagerWaitAllSnapshotsRunningJobsAndExcludesLaterLaunches(t *testing.T) {
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
		result, waitError := m.WaitFor(waitCtx, nil, "all", 30*time.Second)
		waited <- result
		waitErr <- waitError
	}()
	<-waitEnteredSelect

	later, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "later",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Text: "later"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start later job: %v", err)
	}
	awaitJobDone(t, m, later.ID)
	select {
	case result := <-waited:
		t.Fatalf("wait-all included or returned for later job: %+v", result)
	default:
	}

	close(firstRelease)
	awaitJobDone(t, m, first.ID)
	select {
	case result := <-waited:
		t.Fatalf("wait-all returned before second selected job: %+v", result)
	default:
	}

	close(secondRelease)
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("WaitFor: %v", err)
		}
		if len(result.Jobs) != 2 || result.Jobs[0].ID != first.ID || result.Jobs[1].ID != second.ID {
			t.Fatalf("wait-all result = %+v, want first and second only", result)
		}
		for _, job := range result.Jobs {
			if job.ContextPending {
				t.Fatalf("waited job still has pending context: %+v", job)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait-all did not return after every selected job completed")
	}

	contexts := m.DrainCompletedContext(nil)
	if len(contexts) != 1 || !strings.Contains(contexts[0], "later") {
		t.Fatalf("automatic completion context = %v, want later job only", contexts)
	}
	if duplicate := m.DrainCompletedContext(nil); len(duplicate) != 0 {
		t.Fatalf("completion context delivered twice: %v", duplicate)
	}
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

func TestManagerWaitRetainsTimeoutReceivedInSelect(t *testing.T) {
	m := NewManager(Options{})
	timers := make(chan *foregroundSelectPathTimer, 1)
	m.newWaitTimer = func(time.Duration) waitTimer {
		timer := newForegroundSelectPathTimer()
		timers <- timer
		return timer
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

	waited := make(chan WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, err := m.Wait(context.Background(), started.ID, time.Hour)
		waited <- result
		waitErr <- err
	}()
	timer := <-timers
	select {
	case <-timer.selectRegistered:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground wait did not register its timer select")
	}
	// The timer value is consumed by WaitFor's select. It must remain visible
	// to the next priority-ordered loop iteration.
	timer.c <- time.Now()
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if !result.TimedOut || len(result.Jobs) != 1 || result.Jobs[0].Status != StatusRunning {
			t.Fatalf("timeout result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground wait lost timeout received in select")
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
	for _, field := range []string{`"ids"`, `"until"`, `"first"`, `"all"`} {
		if !strings.Contains(string(tool.Schema()), field) {
			t.Fatalf("wait schema is missing %s: %s", field, tool.Schema())
		}
	}
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
	for _, input := range []string{
		`{"action":"wait","id":"one","ids":["two"]}`,
		`{"action":"wait","ids":[]}`,
		`{"action":"wait","ids":["one","one"]}`,
		`{"action":"wait","until":"later"}`,
	} {
		if _, err := tool.Run(context.Background(), []byte(input)); err == nil {
			t.Fatalf("invalid wait input accepted: %s", input)
		}
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

func TestJobsToolWaitAllSelectedIDs(t *testing.T) {
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
			return tools.BackgroundJobResult{Text: "first result"}, nil
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
			return tools.BackgroundJobResult{Text: "second result"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start second: %v", err)
	}
	<-firstStarted
	<-secondStarted

	input, err := json.Marshal(map[string]any{
		"action": "wait",
		"ids":    []string{second.ID, first.ID},
		"until":  "all",
	})
	if err != nil {
		t.Fatalf("marshal wait input: %v", err)
	}
	waited := make(chan string, 1)
	waitErr := make(chan error, 1)
	go func() {
		output, waitError := NewJobsTool(m).Run(context.Background(), input)
		waited <- output
		waitErr <- waitError
	}()
	close(secondRelease)
	awaitJobDone(t, m, second.ID)
	select {
	case output := <-waited:
		t.Fatalf("wait-all returned after one selected id: %q", output)
	default:
	}
	close(firstRelease)
	select {
	case output := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("background_jobs wait: %v", err)
		}
		firstIndex := strings.Index(output, "id: "+first.ID)
		secondIndex := strings.Index(output, "id: "+second.ID)
		if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
			t.Fatalf("wait output does not preserve launch order:\n%s", output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background_jobs wait-all did not finish")
	}
}

func TestJobsToolWaitPreservesBackgroundOriginal(t *testing.T) {
	m := NewManager(Options{})
	full := strings.Repeat("full output\n", 100)
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{
				Text:         "PASS check (1s; exit 0; 1.2KB output)",
				OriginalText: full,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	awaitJobDone(t, m, started.ID)
	input, err := json.Marshal(map[string]any{"action": "wait", "id": started.ID})
	if err != nil {
		t.Fatalf("marshal wait: %v", err)
	}
	result, err := NewJobsTool(m).RunResult(context.Background(), input)
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}
	if !strings.Contains(result.Text, "PASS check") ||
		!strings.Contains(result.OriginalText, strings.TrimSpace(full)) ||
		result.Text == result.OriginalText {
		t.Fatalf("background_jobs result = %+v", result)
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

// TestManagerExposesProgressOnJob verifies the opaque progress closure supplied
// with a background job request is stored on the job and surfaced through the
// snapshot immediately at start (before the run completes) and after it
// finishes. The manager treats progress as an opaque `any`; a sentinel closure
// stands in for the agent-typed closure the delegate tool builds in production.
func TestManagerExposesProgressOnJob(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	progress := func() int { return 42 } // sentinel; the manager must not introspect it
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:          "delegate",
		Description:   "inspect",
		Agent:         "explore",
		WaitForPrompt: true,
		Progress:      progress,
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

	// Mid-run the snapshot carries the live progress closure set at start. The
	// manager treats progress as opaque, so verify identity by invoking the
	// sentinel closure rather than comparing function values directly (which Go
	// forbids except against nil).
	snap, ok := m.Get(started.ID)
	if !ok || snap.Status != StatusRunning {
		t.Fatalf("running snapshot = %+v ok=%v", snap, ok)
	}
	if got := sentinelProgressValue(snap.Progress); got != 42 {
		t.Fatalf("mid-run snapshot progress = %v (invoked), want 42", snap.Progress)
	}

	close(release)
	awaitJobDone(t, m, started.ID)

	// After completion the snapshot still carries the progress closure.
	snap, ok = m.Get(started.ID)
	if !ok || snap.Status != StatusCompleted {
		t.Fatalf("completed snapshot = %+v ok=%v", snap, ok)
	}
	if got := sentinelProgressValue(snap.Progress); got != 42 {
		t.Fatalf("completed snapshot progress = %v (invoked), want 42", snap.Progress)
	}
}

// TestManagerJobResultProgressOverridesStartProgress verifies a Run closure may
// replace the progress closure via BackgroundJobResult.Progress, which the
// manager stores back onto the job so the final snapshot reflects it.
func TestManagerJobResultProgressOverridesStartProgress(t *testing.T) {
	m := NewManager(Options{})
	startProgress := func() int { return 1 }
	resultProgress := func() int { return 2 }
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:     "delegate",
		Progress: startProgress,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Text: "done", Progress: resultProgress}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	awaitJobDone(t, m, started.ID)
	snap, _ := m.Get(started.ID)
	// The result closure must override the start closure. Verify by invoking it
	// rather than comparing function values directly.
	if got := sentinelProgressValue(snap.Progress); got != 2 {
		t.Fatalf("final snapshot progress = %v (invoked), want 2", snap.Progress)
	}
}

// sentinelProgressValue type-asserts an opaque progress `any` back to the
// sentinel func() int used by these tests and invokes it, mirroring how the
// renderer consumes the production func() agent.DelegateProgressSnapshot. It
// returns -1 (a value no sentinel produces) when the assertion fails, so a
// mismatched or nil closure is reported as a wrong value rather than a panic.
func sentinelProgressValue(progress any) int {
	fn, ok := progress.(func() int)
	if !ok || fn == nil {
		return -1
	}
	return fn()
}

// selectPathWaitTimer lets a test place a timer value specifically in the
// observer's blocking select rather than its preceding non-blocking probe. That
// catches a one-shot timer receive being lost between loop iterations.
type selectPathWaitTimer struct {
	c                   chan time.Time
	stopped             chan struct{}
	observerTop         chan struct{}
	allowObserverSelect chan struct{}
	observerSelect      chan struct{}
	mu                  sync.Mutex
	calls               int
	stops               int
}

func newSelectPathWaitTimer() *selectPathWaitTimer {
	return &selectPathWaitTimer{
		c:                   make(chan time.Time, 1),
		stopped:             make(chan struct{}),
		observerTop:         make(chan struct{}),
		allowObserverSelect: make(chan struct{}),
		observerSelect:      make(chan struct{}),
	}
}

func (t *selectPathWaitTimer) C() <-chan time.Time {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	switch call {
	case 4:
		// The foreground wait has made its initial probe, blocking-select
		// registration, and post-steer probe. Pause the detached observer at
		// its own initial probe so the next C call is its blocking select.
		close(t.observerTop)
		<-t.allowObserverSelect
	case 5:
		close(t.observerSelect)
	}
	return t.c
}

func (t *selectPathWaitTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stops++
	if t.stops == 1 {
		close(t.stopped)
	}
	return true
}

func (t *selectPathWaitTimer) StopCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stops
}

// controlledWaitTimer is a plain manually-fired timer for lifecycle tests.
type controlledWaitTimer struct {
	c       chan time.Time
	stopped chan struct{}
	mu      sync.Mutex
	stops   int
}

func newControlledWaitTimer() *controlledWaitTimer {
	return &controlledWaitTimer{c: make(chan time.Time, 1), stopped: make(chan struct{})}
}

func (t *controlledWaitTimer) C() <-chan time.Time { return t.c }

func (t *controlledWaitTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stops++
	if t.stops == 1 {
		close(t.stopped)
	}
	return true
}

func (t *controlledWaitTimer) StopCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stops
}

// foregroundSelectPathTimer makes the second C call (the outer select after its
// initial non-blocking probe) externally visible.
type foregroundSelectPathTimer struct {
	c                chan time.Time
	selectRegistered chan struct{}
	mu               sync.Mutex
	calls            int
}

func newForegroundSelectPathTimer() *foregroundSelectPathTimer {
	return &foregroundSelectPathTimer{
		c:                make(chan time.Time, 1),
		selectRegistered: make(chan struct{}),
	}
}

func (t *foregroundSelectPathTimer) C() <-chan time.Time {
	t.mu.Lock()
	t.calls++
	if t.calls == 2 {
		close(t.selectRegistered)
	}
	t.mu.Unlock()
	return t.c
}

func (*foregroundSelectPathTimer) Stop() bool { return true }

func TestManagerWaitDetachesOnAcceptedSteerAndDeliversOnce(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{Text: "detached result"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun

	entered := make(chan struct{})
	waited := make(chan WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, err := m.Wait(&doneObservedContext{Context: context.Background(), observed: entered}, started.ID, time.Minute)
		waited <- result
		waitErr <- err
	}()
	<-entered
	m.NotifyAcceptedSteer()

	var detached WaitResult
	select {
	case detached = <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted steer did not detach wait")
	}
	if !detached.Detached || detached.WaitID == "" {
		t.Fatalf("detach acknowledgement = %+v", detached)
	}
	if snap, ok := m.Get(started.ID); !ok || snap.Status != StatusRunning {
		t.Fatalf("detaching changed selected job: %+v, ok=%v", snap, ok)
	}

	close(release)
	awaitJobDone(t, m, started.ID)
	select {
	case <-m.DetachedWaitReady():
	case <-time.After(2 * time.Second):
		t.Fatal("detached completion did not signal readiness")
	}
	if !m.DetachedWaitPending() {
		t.Fatal("detached completion should remain pending")
	}
	peek := m.PeekCompletedContext()
	if len(peek) != 1 || !strings.Contains(peek[0], "[detached background wait "+detached.WaitID+"]") || !strings.Contains(peek[0], "detached result") {
		t.Fatalf("peeked detached context = %v", peek)
	}
	drained := m.DrainCompletedContext(nil)
	if len(drained) != 1 || drained[0] != peek[0] {
		t.Fatalf("drained detached context = %v, want %v", drained, peek)
	}
	if duplicate := m.DrainCompletedContext(nil); len(duplicate) != 0 {
		t.Fatalf("detached context delivered twice: %v", duplicate)
	}
	if m.DetachedWaitPending() {
		t.Fatal("detached pending state survived drain")
	}
}

func TestManagerDetachedWaitTransfersOriginalTimer(t *testing.T) {
	m := NewManager(Options{})
	timers := make(chan *selectPathWaitTimer, 1)
	m.newWaitTimer = func(time.Duration) waitTimer {
		timer := newSelectPathWaitTimer()
		timers <- timer
		return timer
	}
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
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

	entered := make(chan struct{})
	waited := make(chan WaitResult, 1)
	go func() {
		result, err := m.Wait(&doneObservedContext{Context: context.Background(), observed: entered}, started.ID, time.Hour)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		waited <- result
	}()
	timer := <-timers
	<-entered
	m.NotifyAcceptedSteer()
	select {
	case result := <-waited:
		if !result.Detached {
			t.Fatalf("wait result = %+v, want detached", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not detach")
	}
	if calls := timer.StopCalls(); calls != 0 {
		t.Fatalf("foreground wait stopped transferred timer %d times", calls)
	}
	select {
	case <-timer.observerTop:
	case <-time.After(2 * time.Second):
		t.Fatal("detached observer did not reach its timer probe")
	}
	close(timer.allowObserverSelect)
	select {
	case <-timer.observerSelect:
	case <-time.After(2 * time.Second):
		t.Fatal("detached observer did not register its timer select")
	}
	// This is now consumed by observeDetachedWait's select, not its next
	// non-blocking probe. The observer must retain that one-shot timeout.
	timer.c <- time.Now()
	select {
	case <-m.DetachedWaitReady():
	case <-time.After(2 * time.Second):
		t.Fatal("transferred timer did not resolve detached wait")
	}
	contexts := m.DrainCompletedContext(nil)
	if len(contexts) != 1 || !strings.Contains(contexts[0], "wait timed out after 1h0m0s") {
		t.Fatalf("timeout detached context = %v", contexts)
	}
	select {
	case <-timer.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not stop transferred timer")
	}
	if calls := timer.StopCalls(); calls != 1 {
		t.Fatalf("timer stop calls = %d, want one", calls)
	}
	close(release)
	awaitJobDone(t, m, started.ID)
}

func TestManagerWaitCompletionWinsAcceptedSteer(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{Text: "completed first"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun
	entered := make(chan struct{})
	waited := make(chan WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, err := m.Wait(&doneObservedContext{Context: context.Background(), observed: entered}, started.ID, time.Minute)
		waited <- result
		waitErr <- err
	}()
	<-entered
	close(release)
	awaitJobDone(t, m, started.ID)
	m.NotifyAcceptedSteer()
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if result.Detached || len(result.Jobs) != 1 || result.Jobs[0].Status != StatusCompleted {
			t.Fatalf("completion should win accepted steer: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return completed result")
	}
	if m.DetachedWaitPending() {
		t.Fatal("completion race published a detached outcome")
	}
}

func TestManagerOverlappingDetachedWaitsSuppressOrdinaryContext(t *testing.T) {
	m := NewManager(Options{})
	startedRun := make(chan struct{})
	release := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{Text: "shared completion"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun

	entered := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	waited := make(chan WaitResult, 2)
	for _, observed := range entered {
		go func(observed chan struct{}) {
			result, err := m.Wait(&doneObservedContext{Context: context.Background(), observed: observed}, started.ID, time.Minute)
			if err != nil {
				t.Errorf("Wait: %v", err)
			}
			waited <- result
		}(observed)
	}
	<-entered[0]
	<-entered[1]
	m.NotifyAcceptedSteer()
	first, second := <-waited, <-waited
	if !first.Detached || !second.Detached || first.WaitID == second.WaitID {
		t.Fatalf("overlapping detach acknowledgements = %+v / %+v", first, second)
	}
	close(release)
	awaitJobDone(t, m, started.ID)
	var contexts []string
	for len(contexts) < 2 {
		select {
		case <-m.DetachedWaitReady():
			contexts = append(contexts, m.DrainCompletedContext(nil)...)
		case <-time.After(2 * time.Second):
			t.Fatal("overlapping detached waits did not resolve")
		}
	}
	if len(contexts) != 2 {
		t.Fatalf("contexts = %d, want two detached aggregates: %v", len(contexts), contexts)
	}
	for _, context := range contexts {
		if !strings.Contains(context, "[detached background wait ") || !strings.Contains(context, "shared completion") {
			t.Fatalf("unexpected detached aggregate: %q", context)
		}
	}
	if duplicate := m.DrainCompletedContext(nil); len(duplicate) != 0 {
		t.Fatalf("ordinary completion duplicated detached aggregates: %v", duplicate)
	}
}

func TestManagerDetachedWaitContextUsesForegroundPreparation(t *testing.T) {
	m := NewManager(Options{})
	registry := &tools.Registry{}
	registry.SetResultLimits(1000, 1000)
	registry.SetToolResultLimits("background_jobs", 48, 1000)
	m.SetResultPreparer(registry.PrepareResultWithOriginal)
	startedRun := make(chan struct{})
	release := make(chan struct{})
	full := strings.Repeat("complete detached output\n", 20)
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{Text: "compact detached output", OriginalText: full}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun
	entered := make(chan struct{})
	waited := make(chan WaitResult, 1)
	go func() {
		result, err := m.Wait(&doneObservedContext{Context: context.Background(), observed: entered}, started.ID, time.Minute)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		waited <- result
	}()
	<-entered
	m.NotifyAcceptedSteer()
	<-waited
	close(release)
	awaitJobDone(t, m, started.ID)
	select {
	case <-m.DetachedWaitReady():
	case <-time.After(2 * time.Second):
		t.Fatal("detached wait did not resolve")
	}
	archiver := &recordingArchiver{}
	contexts := m.DrainCompletedContext(archiver)
	if len(contexts) != 1 || !strings.Contains(contexts[0], toolresult.ArchivedHintMarker) {
		t.Fatalf("prepared detached context = %v", contexts)
	}
	if !archiver.result.Truncated || archiver.result.ForID == "" || !strings.HasPrefix(archiver.result.ForID, "background_wait_") || !strings.Contains(archiver.result.OriginalText, "complete detached output") {
		t.Fatalf("detached archive result = %+v", archiver.result)
	}
}

func TestManagerClearDropsDetachedWaitObserver(t *testing.T) {
	m := NewManager(Options{})
	timers := make(chan *controlledWaitTimer, 1)
	m.newWaitTimer = func(time.Duration) waitTimer {
		timer := newControlledWaitTimer()
		timers <- timer
		return timer
	}
	startedRun := make(chan struct{})
	release := make(chan struct{})
	runnerDone := make(chan struct{})
	started, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			close(runnerDone)
			return tools.BackgroundJobResult{Text: "must not leak"}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	<-startedRun
	entered := make(chan struct{})
	waited := make(chan WaitResult, 1)
	go func() {
		result, err := m.Wait(&doneObservedContext{Context: context.Background(), observed: entered}, started.ID, time.Hour)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		waited <- result
	}()
	timer := <-timers
	<-entered
	m.NotifyAcceptedSteer()
	if result := <-waited; !result.Detached {
		t.Fatalf("wait result = %+v, want detached", result)
	}
	m.Clear()
	select {
	case <-timer.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Clear did not abort detached observer")
	}
	close(release)
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("background runner did not finish")
	}
	if m.DetachedWaitPending() {
		t.Fatal("cleared manager retained detached outcome")
	}
	select {
	case <-m.DetachedWaitReady():
		t.Fatal("cleared manager readiness channel remained signaled")
	default:
	}
}
