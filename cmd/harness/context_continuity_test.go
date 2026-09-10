package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/plan"
	"harness/internal/session"
	"harness/internal/taskcontext"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/internal/ui"
)

const (
	continuityAstra    = "openai-codex:gpt-6-astra"
	continuityClaude   = "anthropic:claude-opus-4-8"
	continuityEvidence = "Original evidence: generation 17 must survive the context reset."
	continuityNote     = "Checkpoint: generation guard implemented; regression verification remains."
	continuityDraft    = "## Implementation\nPreserve generation evidence and verify the regression."
	continuityTodo     = "Verify the generation regression"
)

func continuityToolStep(t *testing.T, id, name string, input any) llmtest.Step {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: id, ToolName: name, ToolInput: data}}, Stop: llm.StopToolUse}
}

func continuityConfig(t *testing.T, mode string) string {
	t.Helper()
	return writeMainConfig(t, `{"context_management":"`+mode+`","mcp":{"enable":false,"local":{"enable":false}},"lsp":{"enable":false},"stagnation_nudge":false}`)
}

func continuityAddAstra(proxy *fakeModelProxy) {
	proxy.catalog.Targets = append(proxy.catalog.Targets, protocol.Target{ID: continuityAstra, ProviderLabel: "openai-codex", ModelLabel: "gpt-6-astra", ContextWindow: 100000})
}

func continuityRun(t *testing.T, args []string, fp *llmtest.FakeProvider, input string) {
	t.Helper()
	env, _, stderr, _, proxy := fakeProviderEnvWithProxy(t, args, fp, input)
	env.stdinPiped = true
	continuityAddAstra(proxy)
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("run = %d: %s", code, stderr.String())
	}
	for i, req := range fp.Requests {
		if err := llm.ValidateTranscript(req.Messages); err != nil {
			t.Fatalf("request %d invalid: %v", i, err)
		}
	}
}

func continuityTools(t *testing.T, req llm.Request, memory, reset bool) {
	t.Helper()
	for _, name := range taskcontext.Names {
		want := memory
		if name == "new_context" {
			want = reset
		}
		if got := slices.Contains(toolNames(req), name); got != want {
			t.Errorf("%s exposed=%v, want %v", name, got, want)
		}
	}
}

func continuityContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %.3000s", want, text)
		}
	}
}

func continuityResult(t *testing.T, req llm.Request, id string) string {
	t.Helper()
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == id {
				if block.ResultError {
					t.Fatalf("tool %s failed: %s", id, block.ResultText)
				}
				return block.ResultText
			}
		}
	}
	t.Fatalf("missing tool result %q", id)
	return ""
}

func continuityLoad(t *testing.T, dir string) session.Session {
	t.Helper()
	s, err := session.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateTranscript(s.Messages); err != nil {
		t.Fatal(err)
	}
	return s
}

func continuityRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Select an original canonical entry, not the reset's summary of that entry.
func continuityEvidenceID(t *testing.T, dir string) string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(continuityRead(t, filepath.Join(dir, "tree.ndjson"))))
	for scanner.Scan() {
		var entry session.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		for _, msg := range entry.Messages {
			for _, block := range msg.Content {
				if block.Text == continuityEvidence {
					return entry.ID
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("original evidence missing from canonical history")
	return ""
}

func TestContextContinuityCLIAstraResetThenClaudeResume(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := filepath.Join(t.TempDir(), "astra-session")
	cfg := continuityConfig(t, "auto")
	first := continuityToolStep(t, "checkpoint", "task_notes", map[string]any{"text": continuityNote})
	// A refresh must have actual work to reclaim, not just a tiny bookkeeping
	// transcript smaller than its bounded recovery hints.
	first.Events = append([]llm.StreamEvent{{Kind: llm.EventTextDelta, Text: strings.Repeat("Inspected generation-check evidence. ", 200)}}, first.Events...)
	fp := llmtest.New("fake", first,
		continuityToolStep(t, "draft", "task_notes", map[string]any{"path": "draft.md", "text": continuityDraft}),
		continuityToolStep(t, "todo", "update_todos", map[string]any{"todos": []todo.Item{{Step: continuityTodo, Status: todo.StatusPending}}}),
		continuityToolStep(t, "publish", "record_plan", map[string]any{"title": "Generation repair", "path": "draft.md"}),
		continuityToolStep(t, "reset", "new_context", map[string]any{}), okStep())
	continuityRun(t, []string{"-config", cfg, "-model", continuityAstra, "-session", dir, "-p", continuityEvidence}, fp, "")
	if len(fp.Requests) != 6 {
		t.Fatalf("requests=%d, want six tool/answer rounds without a model summary", len(fp.Requests))
	}
	continuityTools(t, fp.Requests[0], true, true)
	for i, id := range []string{"checkpoint", "draft", "todo", "publish"} {
		continuityResult(t, fp.Requests[i+1], id)
	}
	last := fp.Requests[5]
	if len(last.Messages) != 1 || last.Messages[0].Compaction == nil || last.Messages[0].Compaction.SummarySource != "task_notes" {
		t.Fatalf("reset did not replace transcript from notes: %+v", last.Messages)
	}
	stored := continuityLoad(t, dir)
	if stored.Plan == nil || stored.Plan.Body != continuityDraft {
		t.Fatalf("record_plan(path) not persisted: %+v", stored.Plan)
	}
	if !reflect.DeepEqual(stored.Todos, []todo.Item{{Step: continuityTodo, Status: todo.StatusPending}}) {
		t.Fatalf("saved TODOs: %+v", stored.Todos)
	}
	if got := continuityRead(t, stored.Plan.Path); got != plan.Render(*stored.Plan) {
		t.Fatalf("published artifact=%q", got)
	}
	if got := continuityRead(t, filepath.Join(dir, "notes", "draft.md")); got != continuityDraft {
		t.Fatalf("publication changed draft: %q", got)
	}
	evidenceID := continuityEvidenceID(t, dir)

	// Each run constructs an entirely new CLI/root/manager. No in-memory state
	// from Astra can explain the tools or recovery context on Claude.
	t.Run("auto", func(t *testing.T) {
		resumed := llmtest.New("fake",
			continuityToolStep(t, "recover-note", "task_notes", map[string]any{"action": "read"}),
			continuityToolStep(t, "find-evidence", "history_search", map[string]any{"query": continuityEvidence}),
			continuityToolStep(t, "read-evidence", "history_read", map[string]any{"id": evidenceID}),
			continuityToolStep(t, "list-history", "history_list", map[string]any{"limit": 2}), okStep())
		continuityRun(t, []string{"-config", cfg, "-model", continuityClaude, "-resume", dir, "-p", "Continue the remaining verification."}, resumed, "")
		if len(resumed.Requests) != 5 {
			t.Fatalf("requests=%d", len(resumed.Requests))
		}
		for _, req := range resumed.Requests {
			continuityTools(t, req, true, false)
		}
		hint := strings.Join(resumed.Requests[0].RequestContext, "\n")
		continuityContains(t, hint, "ordinary compaction", continuityNote, continuityTodo, stored.Plan.Path, filepath.Join(dir, "task-notes.md"), filepath.Join(dir, "tree.ndjson"), "history_read", "update_todos")
		if strings.Contains(hint, "Experimental context management") {
			t.Fatal("Claude received Astra automatic-reset guidance")
		}
		continuityContains(t, continuityResult(t, resumed.Requests[1], "recover-note"), continuityNote)
		continuityContains(t, continuityResult(t, resumed.Requests[2], "find-evidence"), evidenceID, continuityEvidence)
		continuityContains(t, continuityResult(t, resumed.Requests[3], "read-evidence"), evidenceID, continuityEvidence)
		continuityContains(t, continuityResult(t, resumed.Requests[4], "list-history"), "hits", "id")
	})
	t.Run("off", func(t *testing.T) {
		resumed := llmtest.New("fake",
			continuityToolStep(t, "read-note", "read", map[string]any{"path": filepath.Join(dir, "task-notes.md")}),
			continuityToolStep(t, "read-plan", "read", map[string]any{"path": stored.Plan.Path}),
			continuityToolStep(t, "read-tree", "read", map[string]any{"path": filepath.Join(dir, "tree.ndjson")}), okStep())
		continuityRun(t, []string{"-config", continuityConfig(t, "off"), "-model", continuityClaude, "-resume", dir, "-p", "Recover using ordinary reads."}, resumed, "")
		if len(resumed.Requests) != 4 {
			t.Fatalf("requests=%d", len(resumed.Requests))
		}
		for _, req := range resumed.Requests {
			continuityTools(t, req, false, false)
		}
		hint := strings.Join(resumed.Requests[0].RequestContext, "\n")
		continuityContains(t, hint, "standard read tool", continuityNote, continuityTodo, stored.Plan.Path, filepath.Join(dir, "task-notes.md"), filepath.Join(dir, "notes"), filepath.Join(dir, "tree.ndjson"))
		continuityContains(t, continuityResult(t, resumed.Requests[1], "read-note"), continuityNote)
		continuityContains(t, continuityResult(t, resumed.Requests[2], "read-plan"), "Implementation", "Preserve generation evidence")
		continuityContains(t, continuityResult(t, resumed.Requests[3], "read-tree"), continuityEvidence)
	})
}

// Non-lifecycle cases use a real saved session and Manager-produced notes/state
// rather than repeating the six-round CLI setup or hand-writing state JSON.
func continuityFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	manager := taskcontext.New(func() string { return dir })
	manager.SetPolicy(func() taskcontext.Policy { return taskcontext.Policy{Reset: true, Identity: continuityAstra} })
	registry := &tools.Registry{}
	manager.Register(registry)
	notes, ok := registry.Lookup("task_notes")
	if !ok {
		t.Fatal("task_notes not registered")
	}
	for _, input := range []map[string]any{{"text": continuityNote}, {"path": "draft.md", "text": continuityDraft}} {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := notes.Run(context.Background(), data); err != nil {
			t.Fatal(err)
		}
	}
	plans := plan.NewStore()
	publisher := plan.NewTool(plans, func() string { return dir }).WithNoteReader(taskcontext.ReadNote)
	if _, err := publisher.Run(context.Background(), json.RawMessage(`{"title":"Generation repair","path":"draft.md"}`)); err != nil {
		t.Fatal(err)
	}
	published, ok := plans.Latest()
	if !ok {
		t.Fatal("fixture plan missing")
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	s := session.Session{Provider: continuityAstra, Model: continuityAstra, Agent: "auto", Created: now, Updated: now,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: continuityEvidence}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Checkpoint saved."}}}},
		Plan:     &published, Todos: []todo.Item{{Step: continuityTodo, Status: todo.StatusPending}}}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	manager.CommitContextRequest()
	return dir
}

type continuityFile struct {
	Content  string
	Mode     fs.FileMode
	Modified time.Time
}

func continuitySnapshot(t *testing.T, dir string) map[string]continuityFile {
	t.Helper()
	files := map[string]continuityFile{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// CLI resume legitimately acquires/updates the process ownership lock;
		// all durable task/session files must remain byte-for-byte untouched.
		if entry.IsDir() || entry.Name() == "session.lock" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[rel] = continuityFile{continuityRead(t, path), info.Mode(), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestContextContinuityCLIResumeCloneIsIndependent(t *testing.T) {
	t.Chdir(t.TempDir())
	source := continuityFixture(t)
	before := continuitySnapshot(t, source)
	clone := filepath.Join(t.TempDir(), "clone")
	const addition = "\nClone-only verification decision."
	fp := llmtest.New("fake", continuityToolStep(t, "clone-note", "task_notes", map[string]any{"action": "append", "text": addition}), okStep())
	continuityRun(t, []string{"-config", continuityConfig(t, "auto"), "-model", continuityClaude, "-resume", source, "-session", clone, "-p", "Continue independently."}, fp, "")
	if len(fp.Requests) != 2 {
		t.Fatalf("requests=%d", len(fp.Requests))
	}
	continuityTools(t, fp.Requests[0], true, false)
	continuityContains(t, strings.Join(fp.Requests[0].RequestContext, "\n"), continuityNote, filepath.Join(clone, "task-notes.md"))
	continuityResult(t, fp.Requests[1], "clone-note")
	if got := continuityRead(t, filepath.Join(clone, "task-notes.md")); got != continuityNote+addition {
		t.Fatalf("clone note=%q", got)
	}
	if got := continuityRead(t, filepath.Join(clone, "notes", "draft.md")); got != continuityDraft {
		t.Fatalf("clone draft=%q", got)
	}
	statePath := filepath.Join(clone, "context-state.json")
	var state struct {
		Activated      bool `json:"activated"`
		NeedsReconcile bool `json:"needs_reconcile"`
		Reset          bool `json:"reset"`
	}
	if err := json.Unmarshal([]byte(continuityRead(t, statePath)), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Activated || !state.NeedsReconcile || state.Reset {
		t.Fatalf("clone did not independently transition to ordinary mode: %+v", state)
	}
	if reflect.DeepEqual(before["context-state.json"].Content, continuityRead(t, statePath)) {
		t.Fatal("clone context state did not transition")
	}
	if after := continuitySnapshot(t, source); !reflect.DeepEqual(before, after) {
		t.Fatal("resume clone modified source durable files")
	}
	continuityLoad(t, clone)
}

// Copied notes alone can reactivate memory. This case additionally proves that
// the persisted reconciliation requirement, which notes cannot encode, is copied.
func TestContextContinuityCLIClonePreservesReconciliation(t *testing.T) {
	t.Chdir(t.TempDir())
	source := continuityFixture(t)
	manager := taskcontext.New(func() string { return source })
	policy := taskcontext.Policy{Identity: continuityClaude}
	manager.SetPolicy(func() taskcontext.Policy { return policy })
	if err := manager.ContextPrepare(); err != nil {
		t.Fatal(err)
	}
	policy = taskcontext.Policy{Reset: true, Identity: continuityAstra}
	if err := manager.ContextPrepare(); err != nil {
		t.Fatal(err)
	}
	before := continuitySnapshot(t, source)
	clone := filepath.Join(t.TempDir(), "clone")
	fp := llmtest.New("fake", okStep())
	continuityRun(t, []string{"-config", continuityConfig(t, "auto"), "-model", continuityAstra, "-resume", source, "-session", clone, "-p", "Recover before resetting."}, fp, "")
	if len(fp.Requests) != 1 {
		t.Fatalf("requests=%d", len(fp.Requests))
	}
	continuityTools(t, fp.Requests[0], true, true)
	continuityContains(t, strings.Join(fp.Requests[0].RequestContext, "\n"), "blocked until reconciliation", continuityNote)
	var state struct {
		NeedsReconcile bool `json:"needs_reconcile"`
	}
	if err := json.Unmarshal([]byte(continuityRead(t, filepath.Join(clone, "context-state.json"))), &state); err != nil {
		t.Fatal(err)
	}
	if !state.NeedsReconcile {
		t.Fatal("clone forgot source reconciliation requirement")
	}
	if after := continuitySnapshot(t, source); !reflect.DeepEqual(before, after) {
		t.Fatal("clone modified source durable files")
	}
}

func TestContextContinuityCLIFreshClaudeAndClearDoNotLeakMemory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Run("fresh", func(t *testing.T) {
		fp := llmtest.New("fake", okStep())
		continuityRun(t, []string{"-config", continuityConfig(t, "auto"), "-model", continuityClaude, "-p", "Fresh work."}, fp, "")
		if len(fp.Requests) != 1 {
			t.Fatalf("requests=%d", len(fp.Requests))
		}
		continuityTools(t, fp.Requests[0], false, false)
		if hint := strings.Join(fp.Requests[0].RequestContext, "\n"); strings.Contains(hint, "Context strategy:") || strings.Contains(hint, continuityNote) {
			t.Fatalf("fresh session received recovery: %s", hint)
		}
	})
	t.Run("clear", func(t *testing.T) {
		source := continuityFixture(t)
		fp := llmtest.New("fake", okStep(), okStep())
		continuityRun(t, []string{"-config", continuityConfig(t, "auto"), "-model", continuityClaude, "-resume", source}, fp, "Recover current work.\n/clear\nUnrelated fresh work.\n/exit\n")
		if len(fp.Requests) != 2 {
			t.Fatalf("requests=%d", len(fp.Requests))
		}
		continuityTools(t, fp.Requests[0], true, false)
		continuityTools(t, fp.Requests[1], false, false)
		hint := strings.Join(fp.Requests[1].RequestContext, "\n")
		for _, stale := range []string{continuityNote, continuityTodo, "Latest published plan:", "Context strategy:"} {
			if strings.Contains(hint, stale) {
				t.Errorf("/clear leaked %q: %s", stale, hint)
			}
		}
		if len(fp.Requests[1].Messages) != 1 {
			t.Fatalf("/clear retained transcript: %+v", fp.Requests[1].Messages)
		}
	})
}

func TestContextContinuityCLIDebugRequestIsReadOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, mode := range []string{"new", "clone", "clone-ordinary"} {
		t.Run(mode, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "must-not-exist")
			args := []string{"--debug-request", "-config", continuityConfig(t, "auto"), "-model", continuityAstra, "-session", dest, "-p", "Inspect without writing."}
			var source string
			var before map[string]continuityFile
			if mode != "new" {
				source = continuityFixture(t)
				before = continuitySnapshot(t, source)
				args = append(args, "-resume", source)
				if mode == "clone-ordinary" {
					args = append(args, "-model", continuityClaude)
				}
			}
			fp := llmtest.New("fake")
			env, out, stderr, getenv, proxy := fakeProviderEnvWithProxy(t, args, fp, "")
			continuityAddAstra(proxy)
			homeBefore := continuitySnapshot(t, getenv("HOME"))
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("run=%d: %s", code, stderr.String())
			}
			if len(fp.Requests) != 0 || len(proxy.requests) != 0 {
				t.Fatal("debug request streamed a model call")
			}
			var dumped debugRequestOutput
			if err := json.Unmarshal(out.Bytes(), &dumped); err != nil {
				t.Fatalf("debug JSON: %v: %s", err, out.String())
			}
			continuityTools(t, dumped.Request, true, mode != "clone-ordinary")
			if !dumped.PromptIncluded {
				t.Fatal("debug request omitted prompt")
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("debug request created destination: stat error=%v", err)
			}
			if after := continuitySnapshot(t, getenv("HOME")); !reflect.DeepEqual(homeBefore, after) {
				t.Error("debug request created state files outside explicit session path")
			}
			if source != "" {
				continuityContains(t, strings.Join(dumped.Request.RequestContext, "\n"), continuityNote, continuityTodo, "Latest published plan:", filepath.Join(source, "task-notes.md"))
				if after := continuitySnapshot(t, source); !reflect.DeepEqual(before, after) {
					t.Error("debug clone modified source durable state or notes")
				}
			}
		})
	}
}

func TestContextContinuityACPRootRecordPlanFromNotePath(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	fp := llmtest.New("fake",
		continuityToolStep(t, "draft", "task_notes", map[string]any{"path": "draft.md", "text": continuityDraft}),
		continuityToolStep(t, "publish", "record_plan", map[string]any{"title": "Generation repair", "path": "draft.md"}), okStep())
	env, _, stderr, _, proxy := fakeProviderEnvWithProxy(t, []string{"acp", "serve", "-config", continuityConfig(t, "auto"), "-model", continuityAstra}, fp, "")
	continuityAddAstra(proxy)
	invocation, err := commandCatalog(env).Parse(env.args)
	if err != nil {
		t.Fatal(err)
	}
	factory := &acpRootFactory{env: env, flags: invocation.Flags, logger: slog.New(slog.NewTextHandler(stderr, nil)), launchCWD: workspace}
	t.Cleanup(func() { factory.close(context.Background()) })
	root, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := root.Prompt(context.Background(), "Publish the saved draft.", &recordACPUpdates{}); err != nil || reason != acp.StopReasonEndTurn {
		t.Fatalf("Prompt=%s, %v", reason, err)
	}
	if err := root.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("requests=%d", len(fp.Requests))
	}
	continuityResult(t, fp.Requests[1], "draft")
	result := continuityResult(t, fp.Requests[2], "publish")
	const prefix = "recorded plan: "
	if !strings.HasPrefix(result, prefix) {
		t.Fatalf("publication result=%q", result)
	}
	path := strings.TrimSpace(strings.TrimPrefix(result, prefix))
	if got := continuityRead(t, path); got != plan.Render(plan.Plan{Title: "Generation repair", Body: continuityDraft}) {
		t.Fatalf("ACP published artifact=%q", got)
	}
	stored := continuityLoad(t, filepath.Dir(filepath.Dir(path)))
	if stored.Plan == nil || stored.Plan.Path != path || stored.Plan.Body != continuityDraft {
		t.Fatalf("ACP saved plan=%+v", stored.Plan)
	}
}
