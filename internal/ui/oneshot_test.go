package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/background"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/skills"
	"harness/internal/todo"
	"harness/internal/tools"
)

type releaseAfterFirstProvider struct {
	*llmtest.FakeProvider
	release chan struct{}
	once    sync.Once
}

func (p *releaseAfterFirstProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	seq := p.FakeProvider.Stream(ctx, req)
	return func(yield func(llm.StreamEvent, error) bool) {
		defer p.once.Do(func() { close(p.release) })
		for event, err := range seq {
			if !yield(event, err) {
				return
			}
		}
	}
}

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

func TestOneShotExplicitReasoningSummaryStaysOnStderr(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			reasoningSummary("Checking defaults"),
			textDelta("the answer"),
		},
		Stop: llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Reasoning = llm.ReasoningConfig{Summary: "auto"}
	app.Agent.SetReasoning(app.Reasoning)
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

func TestOneShotBangPromptIsLiteral(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RunShellCommand = func(command string) error {
		t.Fatalf("one-shot bang prompt should not run shell command %q", command)
		return nil
	}

	if code := OneShot(app, "!echo foo"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("one-shot should send one model request, got %d", len(fp.Requests))
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != "!echo foo" {
		t.Fatalf("prompt = %q, want literal bang prompt", got)
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

func TestOneShotAtImageReferenceAttachesImage(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	path := writeUIImageNamed(t, "screen shot.png")
	prompt := `describe @"` + path + `"`

	if code := OneShot(app, prompt); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}

	content := fp.Requests[0].Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("content = %d, want image + text", len(content))
	}
	if content[0].Kind != llm.BlockImage || content[0].ImageDetail != "auto" || content[0].ImageMediaType != "image/png" {
		t.Fatalf("first block = %+v", content[0])
	}
	if content[1].Kind != llm.BlockText || content[1].Text != prompt {
		t.Fatalf("second block = %+v, want preserved prompt %q", content[1], prompt)
	}
	if !strings.Contains(errw.String(), "[image attached: screen shot.png image/png") {
		t.Fatalf("missing image attachment notice: %q", errw.String())
	}
}

func TestOneShotSkillMentionAddsRequestContext(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"commit": {
			Name:        "commit",
			Description: "Create a git commit",
			Location:    "/skills/commit/SKILL.md",
		},
	}

	if code := OneShot(app, "please use $commit"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != "please use $commit" {
		t.Fatalf("user prompt should be preserved, got %q", got)
	}
	got := strings.Join(req.RequestContext, "\n\n")
	for _, want := range []string{
		"[explicit skill mentions]",
		"- commit: Create a git commit",
		"path: /skills/commit/SKILL.md",
		"read the full SKILL.md",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("request context missing %q:\n%s", want, got)
		}
	}
}

func TestOneShotStandaloneUnknownSkillSkipsProvider(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	}

	if code := OneShot(app, "$missing"); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), `unknown skill "missing"`) {
		t.Fatalf("missing unknown skill notice, errw=%q", errw.String())
	}
}

func TestOneShotEscapedSkillMentionSendsLiteralDollar(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	}

	if code := OneShot(app, "$$commit"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != "$commit" {
		t.Fatalf("escaped prompt = %q, want %q", got, "$commit")
	}
	if got := strings.Join(req.RequestContext, "\n\n"); strings.Contains(got, "[explicit skill mentions]") {
		t.Fatalf("escaped prompt should not add skill context:\n%s", got)
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

func TestOneShotWaitsForBackgroundDelegateSynthesizesAndCountsUsage(t *testing.T) {
	var out, errw bytes.Buffer
	release := make(chan struct{})
	manager := background.NewManager(background.Options{})
	_, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind:          "delegate",
		Description:   "inspect",
		Agent:         "explore",
		WaitForPrompt: true,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			<-release
			return tools.BackgroundJobResult{
				Text:  "child found the accounting path",
				Usage: llm.Usage{InputTokens: 70, OutputTokens: 30},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("StartBackgroundJob: %v", err)
	}
	fp := &releaseAfterFirstProvider{
		FakeProvider: llmtest.New("fake",
			llmtest.Step{
				Events: []llm.StreamEvent{textDelta("premature parent answer")},
				Stop:   llm.StopEndTurn,
				Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
			},
			llmtest.Step{
				Events: []llm.StreamEvent{textDelta("synthesized child result")},
				Stop:   llm.StopEndTurn,
				Usage:  llm.Usage{InputTokens: 20, OutputTokens: 4},
			},
		),
		release: release,
	}
	app := newTestApp(t, &out, &errw, fp.FakeProvider)
	app.Agent.SetProvider(fp)
	app.Background = manager

	if code := OneShot(app, "finish the work"); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want synthesis request after join", len(fp.Requests))
	}
	if got := strings.Join(fp.Requests[1].RequestContext, "\n"); !strings.Contains(got, "child found the accounting path") {
		t.Fatalf("synthesis request context = %q, want child result", got)
	}
	if !strings.Contains(out.String(), "synthesized child result") {
		t.Fatalf("stdout = %q, want synthesized final response", out.String())
	}
	if app.usage.InputTokens != 100 || app.usage.OutputTokens != 36 {
		t.Fatalf("session usage = %+v, want provider 30/6 + background delegate 70/30", app.usage)
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

func TestOneShotOmitsTodoContextAfterCompletedUpdateTodos(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolStep("update_todos", `{"todos":[{"content":"explore","status":"completed"},{"content":"summarize","status":"completed"}]}`, "call_todo")},
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
	secondContext := strings.Join(fp.Requests[1].RequestContext, "\n\n")
	if secondContext != "" {
		t.Fatalf("completed todo list should not be request context:\n%s", secondContext)
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
		"[turn: 1 waiting]",
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

func TestOneShotCompatibilityDiagnosticDisplayedLoggedAndRecordedSafely(t *testing.T) {
	payload := uiOnePixelPNG
	diagnostic := &llm.APIErrorDiagnostic{
		Stage:           llm.APIErrorStageUpstreamHTTP,
		ProxyInstanceID: "proxy-a",
		ProxyRequestID:  77,
		TargetID:        "openai:vision",
		TraceID:         "trace-abc",
		Compatibility: &llm.CompatibilityDiagnostic{
			Category:    llm.CompatibilityCategoryMultimodalToolResultRejected,
			Reason:      "image_unsupported",
			Confidence:  llm.CompatibilityConfidenceLikely,
			Remediation: "Use an image-capable target.",
		},
		MultimodalShape: &llm.MultimodalRequestShape{ImageCount: 1, ImagePayloadsSHA256: "safe-fingerprint"},
	}
	fp := llmtest.New("fake", llmtest.Step{Err: &llm.APIError{
		StatusCode: 400,
		Code:       "invalid_request",
		Message:    "sanitized image rejection",
		Diagnostic: diagnostic,
	}})
	var out, errw, diagnostics bytes.Buffer
	app := newTestApp(t, &out, &errw, fp)
	app.DiagnosticLogger = slog.New(slog.NewJSONHandler(&diagnostics, nil))
	app.PendingImages = []inputimage.Loaded{{Block: llm.ContentBlock{
		Kind:           llm.BlockImage,
		ImageMediaType: "image/png",
		ImageData:      payload,
	}}}

	if code := OneShot(app, "describe it"); code != ExitRuntime {
		t.Fatalf("OneShot exit = %d, want %d", code, ExitRuntime)
	}
	if len(fp.Requests) != 1 || len(fp.Requests[0].Messages) == 0 || fp.Requests[0].Messages[0].Content[0].ImageData != payload {
		t.Fatalf("provider did not receive expected image request: %+v", fp.Requests)
	}
	for _, want := range []string{"model compatibility:", "openai:vision", llm.CompatibilityCategoryMultimodalToolResultRejected, "Use an image-capable target.", "proxy request 77", "trace trace-abc"} {
		if !strings.Contains(errw.String(), want) {
			t.Fatalf("stderr %q missing %q", errw.String(), want)
		}
	}
	if strings.Count(errw.String(), "model compatibility:") != 1 {
		t.Fatalf("compatibility notice count in stderr = %d: %s", strings.Count(errw.String(), "model compatibility:"), errw.String())
	}
	logText := diagnostics.String()
	for _, want := range []string{`"msg":"model compatibility diagnostic"`, `"prompt":1`, `"turn":1`, `"attempt":1`, `"proxy_instance_id":"proxy-a"`, `"proxy_request_id":77`, `"trace_id":"trace-abc"`, `"api_message":"sanitized image rejection"`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("diagnostic log %q missing %q", logText, want)
		}
	}
	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw event log: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"notice"`) || !strings.Contains(string(raw), "proxy request 77") {
		t.Fatalf("raw events missing diagnostic notice: %s", raw)
	}
	for name, text := range map[string]string{"stdout": out.String(), "stderr": errw.String(), "diagnostics": logText, "raw events": string(raw)} {
		if strings.Contains(text, payload) {
			t.Fatalf("%s leaked image payload", name)
		}
	}
}

func TestOneShotModelAPIIssuePersistsOutsideConversationState(t *testing.T) {
	const providerMessage = "quota window temporarily exhausted"
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{
				State:          llm.ModelRequestAccepted,
				ProxyRequestID: 301,
				TargetID:       "alibaba-token-plan:glm-5.2",
			}},
			{Kind: llm.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{
				State:          llm.ModelRequestUpstreamAttemptFailed,
				Outcome:        llm.ModelRequestOutcomeRetrying,
				ProxyRequestID: 301,
				Attempt:        1,
				MaxAttempts:    5,
				StatusCode:     429,
				Message:        providerMessage,
				RetryAfterMS:   500,
				RetryDelayMS:   500,
			}},
			{Kind: llm.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{
				State:          llm.ModelRequestRetryScheduled,
				ProxyRequestID: 301,
				Attempt:        1,
				StatusCode:     429,
				RetryDelayMS:   500,
			}},
			textDelta("recovered"),
		},
		Stop: llm.StopEndTurn,
	})
	var out, errw, diagnostics bytes.Buffer
	app := newTestApp(t, &out, &errw, fp)
	app.DiagnosticLogger = slog.New(slog.NewJSONHandler(&diagnostics, nil))

	if code := OneShot(app, "go"); code != ExitOK {
		t.Fatalf("OneShot exit = %d, stderr=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), providerMessage) || !strings.Contains(errw.String(), "proxy request 301") {
		t.Fatalf("stderr missing provider issue: %q", errw.String())
	}
	if !strings.Contains(diagnostics.String(), `"msg":"model API issue"`) || !strings.Contains(diagnostics.String(), `"api_message":"`+providerMessage+`"`) {
		t.Fatalf("diagnostics missing structured provider issue: %s", diagnostics.String())
	}
	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw event log: %v", err)
	}
	if strings.Count(string(raw), `"type":"model_request"`) != 3 || !strings.Contains(string(raw), providerMessage) || !strings.Contains(string(raw), `"proxy_request_id":301`) {
		t.Fatalf("raw lifecycle events = %s", raw)
	}
	loaded, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	state, err := json.Marshal(loaded.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), providerMessage) || strings.Contains(string(state), "model_request") {
		t.Fatalf("model telemetry leaked into conversation state: %s", state)
	}
}

func TestREPLCompatibilityDiagnosticDisplayedOnce(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Err: &llm.APIError{
		StatusCode: 400,
		Message:    "sanitized rejection",
		Diagnostic: &llm.APIErrorDiagnostic{
			ProxyRequestID: 9,
			TargetID:       "openai:vision",
			Compatibility: &llm.CompatibilityDiagnostic{
				Category:    llm.CompatibilityCategoryMultimodalToolResultRejected,
				Remediation: "Choose another target.",
			},
		},
	}})
	var out, errw, diagnostics bytes.Buffer
	app := newTestApp(t, &out, &errw, fp)
	app.DiagnosticLogger = slog.New(slog.NewJSONHandler(&diagnostics, nil))
	app.runPrompt("describe")
	if strings.Count(errw.String(), "model compatibility:") != 1 || !strings.Contains(errw.String(), "proxy request 9") {
		t.Fatalf("REPL stderr = %q", errw.String())
	}
	if strings.Count(diagnostics.String(), `"msg":"model compatibility diagnostic"`) != 1 {
		t.Fatalf("REPL diagnostics = %q", diagnostics.String())
	}
}

func TestOneShotQuietSuppressesCompatibilityNoticeButPersistsDiagnostic(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Err: &llm.APIError{
		StatusCode: 400,
		Message:    "sanitized rejection",
		Diagnostic: &llm.APIErrorDiagnostic{
			ProxyRequestID: 10,
			TargetID:       "openai:vision",
			Compatibility: &llm.CompatibilityDiagnostic{
				Category:    llm.CompatibilityCategoryMultimodalToolResultRejected,
				Remediation: "Choose another target.",
			},
		},
	}})
	var out, errw, diagnostics bytes.Buffer
	app := newTestApp(t, &out, &errw, fp)
	app.Renderer = NewRenderer(&out, &errw, RenderOptions{Model: app.Model, Quiet: true, SuppressUsage: true})
	app.DiagnosticLogger = slog.New(slog.NewJSONHandler(&diagnostics, nil))

	if code := OneShot(app, "describe"); code != ExitRuntime {
		t.Fatalf("OneShot exit = %d, want %d", code, ExitRuntime)
	}
	if strings.Contains(errw.String(), "model compatibility:") || strings.Contains(errw.String(), "model compatibility diagnostic") {
		t.Fatalf("quiet stderr contains compatibility output: %q", errw.String())
	}
	if !strings.Contains(errw.String(), "[error: model API 400: sanitized rejection") {
		t.Fatalf("quiet stderr omitted terminal model error: %q", errw.String())
	}
	if strings.Count(diagnostics.String(), `"msg":"model compatibility diagnostic"`) != 1 {
		t.Fatalf("quiet diagnostics = %q", diagnostics.String())
	}
}

func TestOneShotProviderErrorExit1(t *testing.T) {
	var out, errw bytes.Buffer
	// A plain (non-API, non-cancel) error is retryable, so it must persist
	// across the whole per-turn budget (1 + 2 retries) to surface as exit 1.
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

func TestOneShotForceExitDoesNotWaitForStuckProvider(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{Block: func(context.Context) {
		close(started)
		<-release
	}})
	var out, errw lockedBuffer
	app := newTestApp(t, &out, &errw, fp)
	force := make(chan struct{}, 1)
	app.ForceExit = force

	done := make(chan int, 1)
	go func() { done <- OneShot(app, "go") }()
	<-started
	force <- struct{}{}
	select {
	case code := <-done:
		if code != ExitInterrupt {
			t.Fatalf("force exit = %d, want %d", code, ExitInterrupt)
		}
	case <-time.After(time.Second):
		t.Fatal("one-shot force exit waited for stuck provider")
	}
	close(release)
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[prompt:") }, "released one-shot prompt cleanup")
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
