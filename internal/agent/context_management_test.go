package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/taskcontext"
	"harness/internal/tools"
)

func notesTestAgent(t *testing.T, provider llm.Provider, opts Options) (*Agent, *tools.Registry, string, *session.Tree) {
	t.Helper()
	dir := t.TempDir()
	registry := &tools.Registry{}
	taskcontext.New(func() string { return dir }).Register(registry)
	a := newAgent(provider, registry, opts)
	tree := session.NewTree(time.Now(), "", "", "")
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		ref, err := session.SaveCompaction(dir, session.Compaction{Messages: archive.Messages, Summary: archive.Summary, SummarySource: archive.SummarySource})
		if err != nil {
			return "", err
		}
		return ref, tree.PrepareCompaction(a.Transcript(), len(archive.Messages), archive.Summary, ref, archive.TokensBefore, "", nil, nil)
	})
	return a, registry, dir, tree
}

func TestNotesRefreshArchivesEvidenceAndPreservesInstructionsAcrossWindows(t *testing.T) {
	p := &nativeCompactionProvider{FakeProvider: llmtest.New("fake")}
	a, r, dir, tree := notesTestAgent(t, p, Options{NativeCompaction: true, ReasoningReplayDomain: "test", ContextWindow: 100_000})
	a.SetSystem("System stays outside history.")
	steer := userText("Keep compatibility with the old cache.")
	steer.Origin = llm.MessageOriginSteer
	prompt := userText("Fix the invalidation race.")
	prompt.Origin = llm.MessageOriginPrompt
	evidence := "FAIL TestInvalidation: observed stale generation 17"
	a.SetTranscript([]llm.Message{prompt, asstToolUse("test", "shell", `{"command":"go test"}`), toolResult("test", evidence+strings.Repeat(" details", 2000)), steer})
	notes, _ := r.Lookup("task_notes")
	if _, err := notes.Run(context.Background(), json.RawMessage(`{"text":"Goal: fix cache. First fix failed. Next: inspect invalidation."}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
			t.Fatal(err)
		}
		if len(a.Transcript()) != 1 {
			t.Fatal("old conversational turns remained in active context")
		}
		checkpoint := a.Transcript()[0]
		if len(checkpoint.Compaction.UserInstructions) != 2 {
			t.Fatal("lost or duplicated original instructions")
		}
		if err := llm.ValidateTranscript(a.Transcript()); err != nil {
			t.Fatal(err)
		}
		if err := tree.SyncTranscript(a.Transcript()); err != nil {
			t.Fatal(err)
		}
		if err := tree.Save(dir); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			a.transcript = append(a.transcript, asstText(strings.Repeat("further work ", 1000)))
		}
	}
	if p.RequestCount() != 0 || len(p.requests) != 0 {
		t.Fatal("notes reset invoked local or native summarization")
	}
	search, _ := r.Lookup("history_search")
	result, err := search.Run(context.Background(), json.RawMessage(`{"query":"observed stale generation 17"}`))
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Hits []struct {
			ID     string
			Window string `json:"window_id"`
		}
	}
	if err := json.Unmarshal([]byte(result), &found); err != nil || len(found.Hits) == 0 {
		t.Fatalf("original failed test not searchable: %s %v", result, err)
	}
	read, _ := r.Lookup("history_read")
	input, _ := json.Marshal(map[string]any{"id": found.Hits[0].ID})
	result, err = read.Run(context.Background(), input)
	if err != nil || !strings.Contains(result, evidence) {
		t.Fatalf("stable ID lookup: %s %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "compactions", "0002.input.json")); err != nil {
		t.Fatal(err)
	}
}

func TestContextBudgetRemindsBeforeBufferedReset(t *testing.T) {
	a, _, _, _ := notesTestAgent(t, llmtest.New("fake"), Options{ContextWindow: 100_000, CompactInputTokens: 30_000})
	if a.overThreshold(30_000) {
		t.Fatal("reset before model can save notes")
	}
	if !a.overThreshold(40_000) {
		t.Fatal("buffer exhaustion did not force reset")
	}
	a.SetTranscript([]llm.Message{userText("continue"), asstText(strings.Repeat("x", 27_000*4))})
	if got := a.contextManagementContext(); !strings.Contains(got, "context_window_reminder") || !strings.Contains(got, "save notes") {
		t.Fatalf("missing reminder: %s", got)
	}
	a.SetTranscript([]llm.Message{userText("continue"), asstText(strings.Repeat("x", 31_000*4))})
	if got := a.contextManagementContext(); !strings.Contains(got, "exhausted") {
		t.Fatalf("missing handoff: %s", got)
	}
	if _, ok, err := a.PrepareIdleCompaction(1); ok || err != nil {
		t.Fatalf("notes mode started speculative summarization: %v %v", ok, err)
	}
}

func TestContextRefreshFailureLeavesTranscriptAndContinuationIntact(t *testing.T) {
	a, _, _, _ := notesTestAgent(t, llmtest.New("fake"), Options{})
	a.SetTranscript([]llm.Message{userText("task"), asstText(strings.Repeat("prior evidence", 1000))})
	before := cloneMessages(a.Transcript())
	epoch := a.ResponseStateEpoch()
	want := errors.New("archive unavailable")
	a.SetCompactionArchiver(func(context.Context, CompactionArchive) (string, error) { return "", want })
	if _, err := a.Compact(context.Background(), &recordSink{}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(before, a.Transcript()) || epoch != a.ResponseStateEpoch() {
		t.Fatal("failed archive changed live state")
	}
}

func TestNewContextRunsThroughCompactionAndClearsMeasuredAnchor(t *testing.T) {
	p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "refresh", "new_context", `{}`)}, Stop: llm.StopToolUse, Usage: llm.Usage{InputTokens: 20_000, OutputTokens: 20}}, summaryStep("continued", 500, 10))
	a, _, _, _ := notesTestAgent(t, p, Options{ContextWindow: 100_000})
	a.SetTranscript([]llm.Message{userText("Original task"), asstText(strings.Repeat("old detail ", 5000))})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "Continue the same task", sink); err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 2 {
		t.Fatalf("requests = %d", len(p.Requests))
	}
	if a.compactions != 1 {
		t.Fatalf("context reset not counted: %d", a.compactions)
	}
	next := p.Requests[1]
	if next.EstimatedInputTokens >= 20_000 || next.PreviousResponseID != "" {
		t.Fatalf("stale measurement or continuation survived: %+v", next)
	}
	if !strings.Contains(strings.Join(next.RequestContext, "\n"), "history_read") {
		t.Fatal("recovery instructions missing")
	}
	if err := llm.ValidateTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
}

func TestNotesResetRunsExistingCompactionHooks(t *testing.T) {
	for _, trigger := range []string{"manual", "auto"} {
		t.Run(trigger, func(t *testing.T) {
			dir := t.TempDir()
			pre, post := filepath.Join(dir, "pre.json"), filepath.Join(dir, "post.json")
			body, _ := json.Marshal(map[string]any{
				"PreCompact":  []any{map[string]any{"matcher": trigger, "hooks": []any{map[string]any{"type": "command", "command": "cat > " + pre}}}},
				"PostCompact": []any{map[string]any{"matcher": trigger, "hooks": []any{map[string]any{"type": "command", "command": "cat > " + post}}}},
			})
			cfg, err := hooks.DecodeEventMap(body)
			if err != nil {
				t.Fatal(err)
			}
			a, registry, _, _ := notesTestAgent(t, llmtest.New("fake"), Options{Hooks: &hooks.Runner{Config: cfg}})
			a.SetTranscript([]llm.Message{userText("original task"), asstText(strings.Repeat("evidence ", 1000))})
			if trigger == "manual" {
				_, err = a.Compact(context.Background(), &recordSink{})
			} else {
				tool, _ := registry.Lookup("new_context")
				if _, err := tool.Run(context.Background(), json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
				_, err = a.applyContextEpoch(context.Background(), &recordSink{})
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{pre, post} {
				data, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(data), `"trigger":"`+trigger+`"`) {
					t.Fatalf("hook did not run: %s %v", data, err)
				}
			}
		})
	}
}

func TestContextToolsDisappearImmediatelyOnProviderSwitch(t *testing.T) {
	dir := t.TempDir()
	enabled := true
	m := taskcontext.New(func() string { return dir })
	m.SetEnabled(func() bool { return enabled })
	r := &tools.Registry{}
	m.Register(r)
	a := newAgent(llmtest.New("codex"), r, Options{})
	if len(a.ToolSpecs()) == 0 {
		t.Fatal("missing initial tools")
	}
	enabled = false
	a.SetProvider(llmtest.New("api"))
	if len(a.ToolSpecs()) != 0 {
		t.Fatal("tool specs survived provider switch")
	}
	if got := a.contextManagementContext(); got != "" {
		t.Fatal("notes guidance survived provider switch")
	}
	enabled = true
	a.SetProvider(llmtest.New("codex"))
	if len(a.ToolSpecs()) != len(taskcontext.Names) {
		t.Fatal("tools did not return on switching back")
	}
}

func TestContextReminderUsesMeasuredUsageWithinOnePrompt(t *testing.T) {
	p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "budget", "get_context_remaining", `{}`)}, Stop: llm.StopToolUse, Usage: llm.Usage{InputTokens: 75_000, OutputTokens: 20}}, summaryStep("done", 76000, 10))
	a, _, _, _ := notesTestAgent(t, p, Options{ContextWindow: 100_000})
	if err := a.RunPrompt(context.Background(), "continue", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 2 {
		t.Fatalf("requests=%d", len(p.Requests))
	}
	if got := strings.Join(p.Requests[1].RequestContext, "\n"); !strings.Contains(got, "context_window_reminder") {
		t.Fatalf("provider-reported pressure did not reach model: %s", got)
	}
}
