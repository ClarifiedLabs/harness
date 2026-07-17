package background

import (
	"context"
	"strings"
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
