package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/todo"
	"harness/internal/tools"
)

func TestOneShotAssistantTextOnStdoutNoiseOnStderr(t *testing.T) {
	var out, errw bytes.Buffer
	tool := toolStep("read_file", `{"path":"a.go"}`, "c1")
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("reading file "), tool},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 4},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("the answer is 42")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 6},
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	code := OneShot(app, "do it")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "the answer is 42") {
		t.Errorf("assistant text should be on stdout, out=%q", out.String())
	}
	if strings.Contains(out.String(), "[read_file]") || strings.Contains(out.String(), "[turn:") {
		t.Errorf("tool summaries and usage must not pollute stdout, out=%q", out.String())
	}
	if !strings.Contains(errw.String(), "[read_file]") {
		t.Errorf("tool summary should be on stderr, errw=%q", errw.String())
	}
	if !strings.Contains(errw.String(), "[turn:") {
		t.Errorf("usage line should be on stderr, errw=%q", errw.String())
	}
}

func TestOneShotReasoningSummaryStaysOnStderr(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			reasoningSummary("Checking defaults"),
			textDelta("the answer"),
		},
		Stop: llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Renderer = NewRenderer(&out, &errw, RenderOptions{
		Model:           "claude-opus-4-8",
		ToolStream:      true,
		Now:             func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) },
		TimestampLayout: TimestampShortLayout,
	})

	code := OneShot(app, "do it")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(out.String(), "[reasoning]") || strings.Contains(out.String(), "Checking defaults") {
		t.Fatalf("one-shot reasoning summary should not write stdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the answer") {
		t.Fatalf("assistant answer missing from stdout:\n%s", out.String())
	}
	if !strings.Contains(errw.String(), "[12:00:00 reasoning]\n  Checking defaults\n") {
		t.Fatalf("one-shot reasoning summary should render to stderr:\n%s", errw.String())
	}
}

func TestOneShotSavesSessionAndRunsOneTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	if code := OneShot(app, "go"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(fp.Requests) != 1 {
		t.Errorf("one-shot should run exactly one turn, got %d requests", len(fp.Requests))
	}
	if _, err := os.Stat(app.SessionPath); err != nil {
		t.Errorf("one-shot should save the session: %v", err)
	}
}

func TestOneShotSendsPendingImage(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.PendingImages = append(app.PendingImages, loadUIImage(t, "original"))

	if code := OneShot(app, "describe it"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	content := fp.Requests[0].Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("content = %d, want image + text", len(content))
	}
	if content[0].Kind != llm.BlockImage || content[0].ImageDetail != "original" {
		t.Fatalf("first block = %+v", content[0])
	}
	if content[1].Kind != llm.BlockText || content[1].Text != "describe it" {
		t.Fatalf("second block = %+v", content[1])
	}
}

func TestOneShotAddsTodoRequestContextWhenToolAvailable(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store

	if code := OneShot(app, "work on it"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	msgs := req.Messages
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want only user prompt in transcript messages: %+v", len(msgs), msgs)
	}
	if got := msgs[0].Content[0].Text; got != "work on it" {
		t.Fatalf("first message = %q, want prompt", got)
	}
	got := strings.Join(req.RequestContext, "\n\n")
	if got != "" {
		t.Errorf("empty todo list should not add request context, got:\n%s", got)
	}
}

func TestOneShotDoesNotPrintTodoPromptStatus(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	store.Replace([]todo.Item{
		{Content: "explore", Status: todo.StatusCompleted},
		{Content: "test", Status: todo.StatusInProgress, ActiveForm: "Testing"},
	})
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store

	if code := OneShot(app, "work on it"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if got := errw.String(); strings.Contains(got, "Todos (1/2 done):") || strings.Contains(got, "[~] Testing") {
		t.Fatalf("one-shot mode should not print the interactive todo prompt status:\n%s", got)
	}
}

func TestOneShotSkipsTodoContextWhenToolUnavailable(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Todos = todo.NewStore()

	if code := OneShot(app, "work on it"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	msgs := fp.Requests[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want only user prompt: %+v", len(msgs), msgs)
	}
	if strings.Contains(msgs[0].Content[0].Text, "[todo]") {
		t.Fatalf("todo context should not be injected when update_todos is unavailable: %+v", msgs)
	}
}

func TestOneShotRefreshesTodoContextAfterUpdateTodos(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolStep("update_todos", `{"todos":[{"content":"explore","status":"completed"},{"content":"test","status":"in_progress","active_form":"Testing"}]}`, "call_todo")},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store

	if code := OneShot(app, "work on it"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	firstContext := strings.Join(fp.Requests[0].RequestContext, "\n\n")
	if firstContext != "" {
		t.Fatalf("first request should have no empty-list reminder:\n%s", firstContext)
	}
	secondContext := strings.Join(fp.Requests[1].RequestContext, "\n\n")
	for _, want := range []string{"Todos (1/2 done):", "[x] explore", "[~] Testing"} {
		if !strings.Contains(secondContext, want) {
			t.Errorf("second request context missing %q:\n%s", want, secondContext)
		}
	}
	if strings.Contains(secondContext, "No active todo list") {
		t.Fatalf("second request should not reuse stale empty-list reminder:\n%s", secondContext)
	}
}

func TestOneShotShowsToolCallProgressOnStderrOnly(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				{Kind: llm.EventToolCallStart, Index: 0, ToolID: "call_1", ToolName: "list_dir"},
				{Kind: llm.EventToolCallDelta, Index: 0, ArgsDelta: `{"path":`},
				{Kind: llm.EventToolCallDelta, Index: 0, ArgsDelta: `"."}`},
				toolStep("list_dir", `{"path":"."}`, "call_1"),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	if code := OneShot(app, "inspect"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if got := out.String(); got != "done\n" {
		t.Fatalf("stdout = %q, want only assistant answer", got)
	}
	got := errw.String()
	for _, want := range []string{
		"[model: turn 1 waiting]",
		"[tool-call: list_dir id=call_1]",
		"[tool: list_dir started path=.]",
		"[list_dir] path=.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[tool-call args]") {
		t.Errorf("stderr should not dump raw tool-call args:\n%s", got)
	}
}

// TestOneShotSaveFailureWarned is the regression test for the one-shot save
// error being silently swallowed: OneShot used to return ExitOK and print
// nothing when the session save failed, losing the transcript with no signal.
// A failed save must warn to errw (design §11/§12 — visible failure beats silent
// data loss).
func TestOneShotSaveFailureWarned(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = unsavablePath(t)

	// The turn itself succeeds; only the save fails. Exit code is unchanged (the
	// turn ran), but the failure must be surfaced.
	if code := OneShot(app, "go"); code != ExitOK {
		t.Fatalf("turn succeeded, exit code should be 0, got %d; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "save failed") {
		t.Errorf("failed one-shot save must warn to errw, got %q", errw.String())
	}
}

func TestOneShotProviderErrorExit1(t *testing.T) {
	var out, errw bytes.Buffer
	// A plain (non-API, non-cancel) error is retryable, so it must persist
	// across the whole per-model-turn budget (1 + 2 retries) to surface as exit 1.
	fail := llmtest.Step{Err: errContext("upstream 500")}
	fp := llmtest.New("fake", fail, fail, fail)
	app := newTestApp(t, &out, &errw, fp)

	code := OneShot(app, "go")
	if code != 1 {
		t.Errorf("provider error should exit 1, got %d", code)
	}
	if !strings.Contains(strings.ToLower(errw.String()), "error") {
		t.Errorf("error should be reported to stderr, errw=%q", errw.String())
	}
}

func TestBuildPromptDash(t *testing.T) {
	got, err := BuildPrompt("-", strings.NewReader("from stdin"), true)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if got != "from stdin" {
		t.Errorf("`-p -` should read the whole prompt from stdin, got %q", got)
	}
}

func TestBuildPromptFlagAndStdinConcatenate(t *testing.T) {
	got, err := BuildPrompt("summarize:", strings.NewReader("the notes"), true)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if got != "summarize:\nthe notes" {
		t.Errorf("flag text then stdin should concatenate, got %q", got)
	}
}

func TestBuildPromptFlagOnlyWhenNoStdin(t *testing.T) {
	got, err := BuildPrompt("just the flag", nil, false)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if got != "just the flag" {
		t.Errorf("flag-only prompt should pass through, got %q", got)
	}
}

// toolStep builds a complete tool-call Done event for one-shot tests.
func toolStep(name, input, id string) llm.StreamEvent {
	return llm.StreamEvent{
		Kind:      llm.EventToolCallDone,
		ToolID:    id,
		ToolName:  name,
		ToolInput: []byte(input),
	}
}
