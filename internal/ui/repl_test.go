package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/goal"
	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/plan"
	"harness/internal/runstream"
	"harness/internal/session"
	"harness/internal/skills"
	"harness/internal/todo"
	"harness/internal/tools"
)

const uiOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func writeUIImage(t *testing.T) string {
	t.Helper()
	return writeUIImageNamed(t, "screen.png")
}

func writeUIImageNamed(t *testing.T, name string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(uiOnePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func loadUIImage(t *testing.T, detail string) inputimage.Loaded {
	t.Helper()
	loaded, err := inputimage.Load(inputimage.Attachment{Path: writeUIImage(t), Detail: detail})
	if err != nil {
		t.Fatalf("load image: %v", err)
	}
	return loaded
}

func gitAvailableForPromptTest(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

func scratchPromptRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitForPromptTest(t, dir, "init", "-q", "-b", "main")
	gitForPromptTest(t, dir, "config", "user.email", "test@example.com")
	gitForPromptTest(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	gitForPromptTest(t, dir, "add", "file.txt")
	gitForPromptTest(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func gitForPromptTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func textDelta(s string) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventTextDelta, Text: s}
}

func reasoningSummary(s string) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventReasoningSummary, Text: s}
}

// testWriter is the buffer contract newTestApp/liveTestApp need: an io.Writer
// the renderer writes to, plus String() for assertions. Both *bytes.Buffer and
// *lockedBuffer satisfy it, so race-sensitive tests can swap in the locked
// variant without touching the helpers' other callers.
type testWriter interface {
	io.Writer
	String() string
}

// lockedBuffer is a mutex-guarded bytes.Buffer. The during-prompt-input tests poll
// rendered output (via waitFor/String) from the test goroutine while turn
// goroutines write the renderer's out/errw concurrently; a bare *bytes.Buffer
// makes that an unsynchronized access that trips `go test -race`. Locking both
// Write and String lets the race detector exercise the goroutine interleavings.
// Production code guards its writers with its own mutex — this only closes the
// test-harness validation gap.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestApp(t *testing.T, out, errw testWriter, fp *llmtest.FakeProvider) *App {
	t.Helper()
	stateDir := t.TempDir()
	a := agent.New(fp, tools.Default(), agent.Options{Model: "claude-opus-4-8"})
	a.SetSystem("you are a test")
	a.SetSleep(func(time.Duration) {}) // no real time in tests
	r := NewRenderer(out, errw, RenderOptions{Model: "claude-opus-4-8", ToolStream: true})
	return &App{
		Agent:         a,
		Renderer:      r,
		Out:           out,
		Errw:          errw,
		Provider:      "anthropic",
		Model:         "claude-opus-4-8",
		RegistryModel: "anthropic:claude-opus-4-8",
		BaseURL:       "https://api.anthropic.com/v1",
		Registry: llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
			"anthropic:claude-opus-4-8": {InputModalities: []string{"text", "image"}},
		}),
		System:      "you are a test",
		ImageDetail: "auto",
		AgentName:   "auto",
		SessionPath: filepath.Join(stateDir, "session"),
		StateDir:    stateDir,
	}
}

func TestTurnCompletePricesTurnAgainstRegistry(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {Price: llm.Price{Input: 10, Output: 20}},
	})
	sink := newREPLSink(app.Renderer, app, 1)

	sink.TurnComplete(agent.TurnUsage{
		Turn:  1,
		Usage: llm.Usage{InputTokens: 100_000, OutputTokens: 10_000},
	})

	if got := errw.String(); !strings.Contains(got, "· $1.200") {
		t.Fatalf("turn line should show the registry-priced cost, got %q", got)
	}
}

func TestTurnCompleteKeepsProviderCost(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	// A registry price that disagrees with the proxy price must not override it.
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {Price: llm.Price{Input: 10, Output: 20}},
	})
	sink := newREPLSink(app.Renderer, app, 1)

	sink.TurnComplete(agent.TurnUsage{
		Turn:  1,
		Usage: llm.Usage{InputTokens: 100_000, OutputTokens: 10_000, CostUSD: 0.5, CostKnown: true},
	})

	got := errw.String()
	if !strings.Contains(got, "· $0.500") {
		t.Fatalf("turn line should show the provider-priced cost, got %q", got)
	}
	if strings.Contains(got, "$1.200") {
		t.Fatalf("registry price must not override the provider cost, got %q", got)
	}
}

func TestTurnCompleteUnpricedModelOmitsCost(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp) // test registry has no price
	sink := newREPLSink(app.Renderer, app, 1)

	sink.TurnComplete(agent.TurnUsage{
		Turn:  1,
		Usage: llm.Usage{InputTokens: 100_000, OutputTokens: 10_000},
	})

	if got := errw.String(); strings.Contains(got, "$") {
		t.Fatalf("unpriced model must not print a dollar figure, got %q", got)
	}
}

func TestRetentionTelemetryIsPersistedWithoutEnteringTranscript(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	sink := newREPLSink(app.Renderer, app, 3)
	sink.RetentionApplied(agent.RetentionEvent{
		Policy:              "pressure_epoch",
		Trigger:             "context_pressure",
		BlocksTrimmed:       2,
		BytesBefore:         30_000,
		BytesAfter:          5_000,
		ContextTokensBefore: 8_000,
		ContextTokensAfter:  2_000,
		ResponseStateReset:  true,
	})

	data, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read replay: %v", err)
	}
	var event session.Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if event.Type != session.EventRetention || event.Prompt != 3 || event.Turn != 1 || event.Retention == nil {
		t.Fatalf("retention event = %+v", event)
	}
	if event.Retention.BlocksTrimmed != 2 || !event.Retention.ResponseStateReset || event.Retention.NextRequestStateful {
		t.Fatalf("retention snapshot = %+v", event.Retention)
	}
	if len(app.Agent.Transcript()) != 0 {
		t.Fatalf("retention telemetry entered transcript: %+v", app.Agent.Transcript())
	}
}

func TestPromptCheckpointRecoversToolDispatchBeforeResult(t *testing.T) {
	var out, errw lockedBuffer
	tool := &blockingTool{name: "mutate", ran: make(chan struct{}), release: make(chan struct{})}
	fp := llmtest.New("responses", llmtest.Step{
		Events:     []llm.StreamEvent{toolStep("mutate", `{}`, "call-1")},
		Stop:       llm.StopToolUse,
		Usage:      llm.Usage{InputTokens: 7, OutputTokens: 2},
		ResponseID: "resp-1",
	})
	app := newTestApp(t, &out, &errw, fp)
	registry := tools.Default()
	registry.Register(tool)
	app.Agent = agent.New(fp, registry, agent.Options{
		Model:             "claude-opus-4-8",
		ResponsesStateful: true,
	})
	app.Agent.SetSystem(app.System)
	app.Todos = todo.NewStore()
	app.Todos.Replace([]todo.Item{{Content: "mutate once", Status: "in_progress"}})
	app.Plans = plan.NewStore()
	app.Plans.Replace([]plan.Plan{{Title: "Safe mutation", Body: "Run once", Path: "plans/safe.md"}})

	promptID := app.beginPrompt("change it", nil)
	app.Renderer.StartPromptRun()
	done := make(chan error, 1)
	go func() {
		done <- app.Agent.RunPromptContentWithContext(
			context.Background(),
			"change it",
			nil,
			nil,
			promptID,
			newREPLSink(app.Renderer, app, promptID),
		)
	}()
	select {
	case <-tool.ran:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}

	recovered, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatalf("Load active tool dispatch: %v", err)
	}
	if recovered.Recovery == nil || recovered.Recovery.Phase != string(agent.PromptCheckpointToolDispatch) {
		t.Fatalf("recovery = %+v", recovered.Recovery)
	}
	if err := llm.ValidateTranscript(recovered.Messages); err != nil {
		t.Fatalf("recovered transcript: %v", err)
	}
	if len(recovered.Messages) != 3 {
		t.Fatalf("messages = %d, want prompt/tool-use/interrupted result", len(recovered.Messages))
	}
	result := recovered.Messages[2].Content[0]
	if !result.ResultError || result.ResultText != "interrupted" || result.ResultForID != "call-1" {
		t.Fatalf("interrupted result = %+v", result)
	}
	if recovered.ResponseState == nil || recovered.ResponseState.PreviousResponseID != "resp-1" {
		t.Fatalf("response state = %+v", recovered.ResponseState)
	}
	if recovered.Usage.InputTokens != 7 || len(recovered.Todos) != 1 || len(recovered.Plans) != 1 {
		t.Fatalf("durable state = usage %+v todos %+v plans %+v", recovered.Usage, recovered.Todos, recovered.Plans)
	}

	close(tool.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPrompt: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunPrompt did not finish")
	}
}

func TestClosedTurnCheckpointIsDurableBeforeNextModelResponse(t *testing.T) {
	var out, errw lockedBuffer
	nextRequest := make(chan struct{})
	release := make(chan struct{})
	fp := llmtest.New("responses",
		llmtest.Step{
			Events:     []llm.StreamEvent{toolStep("update_todos", `{"todos":[{"content":"verified","status":"completed"}]}`, "todo-1")},
			Stop:       llm.StopToolUse,
			Usage:      llm.Usage{InputTokens: 9, OutputTokens: 3},
			ResponseID: "resp-1",
		},
		llmtest.Step{
			Block: func(context.Context) {
				close(nextRequest)
				<-release
			},
			Stop: llm.StopEndTurn,
		},
	)
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	registry := tools.Default()
	registry.Register(todo.NewTool(store))
	app.Agent = agent.New(fp, registry, agent.Options{
		Model:             "claude-opus-4-8",
		ResponsesStateful: true,
	})
	app.Agent.SetSystem(app.System)
	app.Todos = store

	ctx, cancel := context.WithCancel(context.Background())
	promptID := app.beginPrompt("track it", nil)
	app.Renderer.StartPromptRun()
	done := make(chan error, 1)
	go func() {
		done <- app.Agent.RunPromptContentWithContext(
			ctx,
			"track it",
			nil,
			nil,
			promptID,
			newREPLSink(app.Renderer, app, promptID),
		)
	}()
	select {
	case <-nextRequest:
	case <-time.After(time.Second):
		t.Fatal("second model request did not start")
	}

	recovered, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatalf("Load closed checkpoint: %v", err)
	}
	if err := llm.ValidateTranscript(recovered.Messages); err != nil {
		t.Fatalf("closed transcript: %v", err)
	}
	if len(recovered.Messages) != 3 {
		t.Fatalf("messages = %d, want one complete tool turn", len(recovered.Messages))
	}
	if recovered.Usage.InputTokens != 9 || recovered.Usage.OutputTokens != 3 {
		t.Fatalf("checkpoint usage = %+v", recovered.Usage)
	}
	if len(recovered.Todos) != 1 || recovered.Todos[0].Status != "completed" {
		t.Fatalf("checkpoint todos = %+v", recovered.Todos)
	}
	if recovered.ResponseState == nil || recovered.ResponseState.PreviousResponseID != "resp-1" || recovered.ResponseState.AnchorMessages != 2 {
		t.Fatalf("checkpoint response state = %+v", recovered.ResponseState)
	}

	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunPrompt error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunPrompt did not stop")
	}
}

func TestDirectHookDiagnosticsPersistAndMirror(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	cfg, err := hooks.DecodeEventMap([]byte(`{
		"SessionStart":[{"hooks":[{"name":"startup-policy","type":"command","command":"printf '{}'"}]}],
		"UserPromptSubmit":[{"hooks":[{"name":"prompt-policy","type":"command","command":"printf '{}'"}]}]
	}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	app.Hooks = &hooks.Runner{Config: cfg}
	var stream bytes.Buffer
	writer := runstream.NewWriter(&stream, runstream.RunStart{
		Mode: runstream.ModeInteractive, SessionID: "test-session", Provider: "fake", Model: "fake",
	}, &errw)
	app.RunStream = writer

	app.RunSessionStartHook("startup")
	result := app.runPromptSubmitHook(context.Background(), "hello", 7)
	if result.Block || len(result.Diagnostics) != 1 {
		t.Fatalf("prompt hook result = %+v", result)
	}

	data, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read hook diagnostics: %v", err)
	}
	var persisted []session.Event
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var event session.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode hook diagnostic: %v", err)
		}
		persisted = append(persisted, event)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted hook diagnostics = %+v, want 2", persisted)
	}
	if persisted[0].Type != session.EventHookDiagnostic || persisted[0].Prompt != 0 || persisted[0].HookDiagnostic == nil || persisted[0].HookDiagnostic.Event != "SessionStart" || persisted[0].HookDiagnostic.Handler != "startup-policy" {
		t.Fatalf("session-start diagnostic = %+v; all=%+v", persisted[0], persisted)
	}
	if persisted[1].Type != session.EventHookDiagnostic || persisted[1].Prompt != 7 || persisted[1].HookDiagnostic == nil || persisted[1].HookDiagnostic.Event != "UserPromptSubmit" || persisted[1].HookDiagnostic.Handler != "prompt-policy" {
		t.Fatalf("prompt diagnostic = %+v", persisted[1])
	}
	writer.Close(runstream.RunEnd{})
	mirrored := linesOfType(decodeRunStreamLines(t, stream.String()), string(session.EventHookDiagnostic))
	if len(mirrored) != 2 {
		t.Fatalf("mirrored hook diagnostics = %v; stream=%s", mirrored, stream.String())
	}
}

func TestOneShotPromptHookBlockSkipsTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("should not run")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	cfg, err := hooks.DecodeEventMap([]byte(`{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf '{\"decision\":\"block\",\"reason\":\"secret\"}'"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	app.Hooks = &hooks.Runner{Config: cfg}

	code := OneShot(app, "do it")
	if code != ExitRuntime {
		t.Fatalf("OneShot exit = %d, want %d", code, ExitRuntime)
	}
	if app.PromptNumber != 0 {
		t.Fatalf("prompt = %d, want 0", app.PromptNumber)
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider was called despite prompt block: %d requests", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "[prompt blocked: secret]") {
		t.Fatalf("stderr missing prompt block notice:\n%s", errw.String())
	}
}

func TestREPLHelpPromptExit(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("the answer")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 10, OutputTokens: 3},
	})
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("/help\nwhat is 2+2?\n/exit\n")
	code := Run(in, app, nil)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(errw.String(), "/help") || !strings.Contains(errw.String(), "/exit") {
		t.Errorf("/help should list commands, errw=%q", errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Errorf("agent should be invoked once for the single prompt, got %d requests", fp.RequestCount())
	}
	if !strings.Contains(out.String(), "the answer") {
		t.Errorf("assistant text should reach stdout, out=%q", out.String())
	}
}

func TestREPLCommandDocumentationMatchesVocabulary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "usage.md"))
	if err != nil {
		t.Fatalf("read usage documentation: %v", err)
	}
	section := string(data)
	start := strings.Index(section, "## REPL Commands")
	if start < 0 {
		t.Fatal("docs/usage.md missing REPL Commands section")
	}
	section = section[start:]
	tableStart := strings.Index(section, "| command | effect |")
	if tableStart < 0 {
		t.Fatal("docs/usage.md missing REPL command table")
	}
	section = section[tableStart:]
	tableEnd := strings.Index(section, "\n\n")
	if tableEnd < 0 {
		t.Fatal("docs/usage.md REPL command table has no end")
	}
	documented := replCommandTableSet(section[:tableEnd])
	help := replCommandSet(helpText)
	known := make(map[string]bool, len(knownCommands))
	for _, command := range knownCommands {
		known[command] = true
		if !help[command] {
			t.Errorf("REPL help text missing known command %q", command)
		}
		if !documented[command] {
			t.Errorf("docs/usage.md REPL command table missing known command %q", command)
		}
	}
	for command := range help {
		if !known[command] {
			t.Errorf("REPL help text documents unknown command %q", command)
		}
	}
	for command := range documented {
		if !known[command] {
			t.Errorf("docs/usage.md REPL command table documents unknown command %q", command)
		}
	}
}

func replCommandSet(text string) map[string]bool {
	commands := map[string]bool{}
	for _, command := range regexp.MustCompile(`/[a-z]+`).FindAllString(text, -1) {
		commands[command] = true
	}
	return commands
}

func replCommandTableSet(table string) map[string]bool {
	var commandCells strings.Builder
	for _, row := range strings.Split(table, "\n") {
		cells := strings.Split(row, "|")
		if len(cells) >= 3 {
			commandCells.WriteString(cells[1])
			commandCells.WriteByte('\n')
		}
	}
	return replCommandSet(commandCells.String())
}

func TestREPLSeparatesSubmittedPromptFromModelResponse(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		usePromptEditor bool
	}{
		{name: "plain reader", input: "hi\n/exit\n"},
		{name: "prompt editor", input: "hi\r/exit\r", usePromptEditor: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var term lockedBuffer
			fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("answer")}, Stop: llm.StopEndTurn})
			app := newTestApp(t, &term, &term, fp)
			app.Renderer = NewRenderer(&term, &term, RenderOptions{Model: "claude-opus-4-8", Quiet: true, SuppressUsage: true})

			code := run(strings.NewReader(tt.input), app, nil, tt.usePromptEditor)
			if code != ExitOK {
				t.Fatalf("exit code = %d, want 0; terminal=%q", code, term.String())
			}
			got := term.String()
			// The separator rule must appear between the prompt and the model answer,
			// replacing the old double-blank-line separator.
			if !strings.Contains(got, submittedPromptRule+"\nanswer") {
				t.Fatalf("terminal output should separate prompt from model response with the rule line; got %q", got)
			}
			if strings.Contains(got, "\n\n") {
				t.Fatalf("terminal output should not contain double blank lines; got %q", got)
			}
		})
	}
}

func TestREPLRecordsTurnTimingEvents(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }

	code := Run(strings.NewReader("hi\n/exit\n"), app, nil)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	data, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read replay log: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `"type":"turn_attempt_start"`) {
		t.Fatalf("missing turn_attempt_start event:\n%s", got)
	}
	if !strings.Contains(got, `"type":"turn_attempt_usage"`) || !strings.Contains(got, `"type":"turn_complete"`) {
		t.Fatalf("missing turn attempt usage/completion events:\n%s", got)
	}
	if !strings.Contains(got, `"context"`) {
		t.Fatalf("missing context snapshot:\n%s", got)
	}
}

func TestREPLReasoningSummaryHiddenByDefault(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			reasoningSummary("Hidden by default"),
			textDelta("done"),
		},
		Stop: llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	code := Run(strings.NewReader("hi\n/exit\n"), app, nil)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if strings.Contains(out.String(), "Hidden by default") || strings.Contains(errw.String(), "Hidden by default") {
		t.Fatalf("reasoning summary should be hidden by default; stdout=%q stderr=%q", out.String(), errw.String())
	}
	if !strings.Contains(out.String(), "done") {
		t.Fatalf("assistant text missing from stdout:\n%s", out.String())
	}
	data, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read replay log: %v", err)
	}
	if strings.Contains(string(data), `"type":"reasoning_summary"`) {
		t.Fatalf("hidden reasoning summary should not be recorded for replay:\n%s", data)
	}
}

func TestREPLReasoningSummaryRendersAsFirstClassOutputWhenExplicit(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			reasoningSummary("Exploring context\nChecking files"),
			textDelta("done"),
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

	code := Run(strings.NewReader("hi\n/exit\n"), app, nil)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	gotOut := out.String()
	for _, want := range []string{
		"[12:00:00 reasoning]\n",
		"  Exploring context\n",
		"  Checking files\n",
		"done",
	} {
		if !strings.Contains(gotOut, want) {
			t.Fatalf("stdout missing %q:\n%s", want, gotOut)
		}
	}
	if strings.Contains(errw.String(), "[12:00:00 reasoning]") {
		t.Fatalf("interactive reasoning summary should not render as stderr notice:\n%s", errw.String())
	}
	data, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read replay log: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"type":"reasoning_summary"`) || !strings.Contains(raw, `Exploring context\nChecking files`) {
		t.Fatalf("replay log missing semantic reasoning summary event:\n%s", raw)
	}
}

func TestREPLDefaultPromptShowsAgent(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)

	if code := Run(strings.NewReader("/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := errw.String(); !strings.Contains(got, "[auto] > ") {
		t.Fatalf("default prompt should show active agent, got %q", got)
	}
}

func TestREPLPromptUpdatesAfterAgentSwitch(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{agent}> "
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{
			Name:   name,
			Tools:  tools.Default(),
			System: "you are a test",
		}, nil
	}

	if code := Run(strings.NewReader("/agent plan\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "auto> ") || !strings.Contains(got, "plan> ") {
		t.Fatalf("prompt should re-render after agent switch, got %q", got)
	}
}

func TestREPLPromptRendersReasoning(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{reasoning}> "
	app.Reasoning = llm.ReasoningConfig{Profile: "high"}

	if code := Run(strings.NewReader("/reasoning none\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "high> ") || !strings.Contains(got, "none> ") {
		t.Fatalf("prompt should show current reasoning before and after changes, got %q", got)
	}
	if strings.Contains(got, "profile=high> ") || strings.Contains(got, "profile=none> ") {
		t.Fatalf("prompt should show reasoning profile without profile= prefix, got %q", got)
	}
}

func TestREPLPromptRendersHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Skipf("hostname unavailable: %v", err)
	}

	shortHostname := strings.SplitN(hostname, ".", 2)[0]

	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{hostname} {hostname:long}> "

	if code := Run(strings.NewReader("/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := shortHostname + " " + hostname + "> "
	if got := errw.String(); !strings.Contains(got, want) {
		t.Fatalf("prompt should show short and long hostnames %q, got %q", want, got)
	}
}

func TestREPLPromptRendersViModeLabel(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{vimode}> "
	app.PromptEditMode = "vi"

	// Drive the prompt editor in vi mode: type "hi", Esc into normal mode, 'i'
	// back to insert, then Enter to submit. The {vimode} label should render
	// INSERT at idle (the editor starts a read in insert mode), flip to NORMAL
	// after Esc, and back to INSERT after 'i'.
	if code := run(strings.NewReader("hi\x1bi\r/exit\n"), app, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	if !strings.Contains(got, "INSERT> ") {
		t.Fatalf("prompt should show INSERT label, got %q", got)
	}
	if !strings.Contains(got, "NORMAL> ") {
		t.Fatalf("prompt should show NORMAL label after Esc, got %q", got)
	}
}

func TestREPLPromptViModeEmptyInEmacsMode(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{vimode}> "
	app.PromptEditMode = "emacs"

	if code := run(strings.NewReader("hi\r/exit\n"), app, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	if strings.Contains(got, "INSERT") || strings.Contains(got, "NORMAL") {
		t.Fatalf("vimode label should be empty in emacs mode, got %q", got)
	}
	if !strings.Contains(got, "> ") {
		t.Fatalf("prompt should still render the trailing > in emacs mode, got %q", got)
	}
}

func TestREPLPromptViModeEmptyWithoutPromptEditor(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{vimode}> "
	app.PromptEditMode = "vi"

	if code := Run(strings.NewReader("/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	if strings.Contains(got, "INSERT") || strings.Contains(got, "NORMAL") {
		t.Fatalf("vimode label should be empty without the raw prompt editor, got %q", got)
	}
	if !strings.Contains(got, "> ") {
		t.Fatalf("prompt should still render the trailing > without the raw prompt editor, got %q", got)
	}
}

func TestREPLPromptRendersGitBranchEachPrompt(t *testing.T) {
	gitAvailableForPromptTest(t)
	dir := scratchPromptRepo(t)
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Prompt = "{git_branch}> "
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		gitForPromptTest(t, dir, "checkout", "-q", "-b", "feature/prompt")
		return AgentSelection{
			Name:   name,
			Tools:  tools.Default(),
			System: "you are a test",
		}, nil
	}

	if code := Run(strings.NewReader("/agent plan\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "main> ") || !strings.Contains(got, "feature/prompt> ") {
		t.Fatalf("prompt should re-read git branch each prompt, got %q", got)
	}
}

func TestREPLSavesSessionAfterTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 5, OutputTokens: 1},
	})
	app := newTestApp(t, &out, &errw, fp)
	path := app.SessionPath

	in := strings.NewReader("hello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session should be saved to %s: %v", path, err)
	}
	data, _ := os.ReadFile(filepath.Join(path, "tree.ndjson"))
	if !strings.Contains(string(data), "hello") {
		t.Errorf("saved session should contain the user prompt, got %s", data)
	}
}

func TestREPLImageCommandAttachesNextPrompt(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	path := writeUIImage(t)

	in := strings.NewReader("/image --detail high " + path + "\ndescribe it\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if fp.RequestCount() != 1 {
		t.Fatalf("requests = %d, want 1", fp.RequestCount())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("content = %d, want image + text", len(content))
	}
	if content[0].Kind != llm.BlockImage || content[0].ImageDetail != "high" || content[0].ImageMediaType != "image/png" {
		t.Fatalf("first block = %+v", content[0])
	}
	if content[1].Kind != llm.BlockText || content[1].Text != "describe it" {
		t.Fatalf("second block = %+v", content[1])
	}
	if !strings.Contains(errw.String(), "[image attached: screen.png image/png") {
		t.Fatalf("missing image attachment notice: %q", errw.String())
	}
}

func TestREPLImageCommandSkipsTextOnlyModel(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Provider = "openai"
	app.Model = "gpt-5.5"
	app.RegistryModel = "openai:gpt-5.5"
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"openai:gpt-5.5": {InputModalities: []string{"text"}},
	})
	path := writeUIImage(t)

	in := strings.NewReader("/image " + path + "\ndescribe it\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if fp.RequestCount() != 1 {
		t.Fatalf("requests = %d, want 1", fp.RequestCount())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 1 || content[0].Kind != llm.BlockText || content[0].Text != "describe it" {
		t.Fatalf("content = %+v, want only text", content)
	}
	if !strings.Contains(errw.String(), "[image skipped: model openai:gpt-5.5 does not support image input]") {
		t.Fatalf("missing image skipped warning: %q", errw.String())
	}
	if strings.Contains(errw.String(), "[image attached:") {
		t.Fatalf("image should not have been attached: %q", errw.String())
	}
}

func TestREPLPromptAtImageReferenceAttachesImage(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	path := writeUIImageNamed(t, "screen shot.png")
	prompt := `describe @"` + path + `"`

	in := strings.NewReader(prompt + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if fp.RequestCount() != 1 {
		t.Fatalf("requests = %d, want 1", fp.RequestCount())
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

func TestREPLPromptAtImageReferenceSkipsTextOnlyModel(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Provider = "openai"
	app.Model = "gpt-5.5"
	app.RegistryModel = "openai:gpt-5.5"
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"openai:gpt-5.5": {InputModalities: []string{"text"}},
	})
	path := writeUIImage(t)
	prompt := "describe @" + path

	if code := Run(strings.NewReader(prompt+"\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if fp.RequestCount() != 1 {
		t.Fatalf("requests = %d, want 1", fp.RequestCount())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 1 || content[0].Kind != llm.BlockText || content[0].Text != prompt {
		t.Fatalf("content = %+v, want only preserved text", content)
	}
	if !strings.Contains(errw.String(), "[image skipped: model openai:gpt-5.5 does not support image input]") {
		t.Fatalf("missing image skipped warning: %q", errw.String())
	}
	if strings.Contains(errw.String(), "[image attached:") {
		t.Fatalf("image should not have been attached: %q", errw.String())
	}
}

func TestREPLPromptAtNonImageReferenceStaysTextOnly(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	notes := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notes, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	prompt := "inspect @" + notes

	if code := Run(strings.NewReader(prompt+"\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	content := fp.Requests[0].Messages[0].Content
	if len(content) != 1 || content[0].Kind != llm.BlockText || content[0].Text != prompt {
		t.Fatalf("content = %+v, want only preserved text", content)
	}
	if strings.Contains(errw.String(), "[image failed:") || strings.Contains(errw.String(), "[image attached:") {
		t.Fatalf("non-image reference should not produce image notices: %q", errw.String())
	}
}

func TestREPLPastedAtImageReferenceIsLiteral(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	path := writeUIImage(t)
	prompt := "describe @" + path

	in := strings.NewReader(bracketedPasteStart + prompt + bracketedPasteEnd + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if fp.RequestCount() != 1 {
		t.Fatalf("requests = %d, want 1", fp.RequestCount())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 1 || content[0].Kind != llm.BlockText || content[0].Text != prompt {
		t.Fatalf("content = %+v, want pasted text only", content)
	}
	if strings.Contains(errw.String(), "[image attached:") || strings.Contains(errw.String(), "[image failed:") {
		t.Fatalf("pasted prompt should not auto-attach images: %q", errw.String())
	}
}

func TestREPLClearResetsAndRotates(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("one")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("two")}, Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	origPath := app.SessionPath
	origProxySessionID := app.Agent.ProxySessionID()
	origCacheAffinityID := app.Agent.CacheAffinityID()

	in := strings.NewReader("first prompt\n/clear\nsecond prompt\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// After /clear the transcript holds only the second turn (user+assistant).
	msgs := app.Agent.Transcript()
	if err := llm.ValidateTranscript(msgs); err != nil {
		t.Fatalf("transcript invalid after clear: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("after /clear transcript should hold only the second turn, got %d messages", len(msgs))
	}
	if msgs[0].Content[0].Text != "second prompt" {
		t.Errorf("transcript should start at the post-clear prompt, got %q", msgs[0].Content[0].Text)
	}

	// /clear rotates to a fresh session path.
	if app.SessionPath == origPath {
		t.Errorf("/clear should rotate to a fresh session file, still %s", origPath)
	}
	if app.Agent.ProxySessionID() == origProxySessionID || app.Agent.CacheAffinityID() == origCacheAffinityID {
		t.Errorf("/clear should rotate continuation and cache ids, got proxy=%q cache=%q", app.Agent.ProxySessionID(), app.Agent.CacheAffinityID())
	}
	if !strings.Contains(errw.String(), "/clear") && !strings.Contains(errw.String(), "cleared") {
		t.Errorf("/clear should acknowledge, errw=%q", errw.String())
	}
}

func TestREPLUnknownCommand(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("/bogus\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(errw.String(), "/bogus") || !strings.Contains(strings.ToLower(errw.String()), "unknown") {
		t.Errorf("unknown command should be reported, errw=%q", errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Errorf("unknown command must not invoke the agent, got %d requests", fp.RequestCount())
	}
}

func TestREPLUnknownCommandSuggestsClosest(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)

	if code := Run(strings.NewReader("/modl\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(errw.String(), "did you mean /model?") {
		t.Errorf("near-miss command should suggest /model, errw=%q", errw.String())
	}
}

func TestSuggestCommand(t *testing.T) {
	cases := map[string]string{
		"/modl":     "/model",  // edit distance 1
		"/usag":     "/usage",  // shared prefix
		"/efort":    "/effort", // transposition/missing letter
		"/compactt": "/compact",
		"/xyzzy":    "", // too far from anything
		"/":         "", // too short
	}
	for in, want := range cases {
		if got := suggestCommand(in); got != want {
			t.Errorf("suggestCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestREPLViCommand(t *testing.T) {
	var modeChange string
	var savedModes []string
	app := &App{
		Errw:              new(bytes.Buffer),
		Out:               new(bytes.Buffer),
		PromptEditMode:    "emacs",
		SetPromptEditMode: func(m string) { modeChange = m },
		SaveReplEditMode:  func(m string) error { savedModes = append(savedModes, m); return nil },
	}

	// /vi on
	app.command("/vi on", nil)
	if app.PromptEditMode != "vi" {
		t.Errorf("/vi on: PromptEditMode = %q, want vi", app.PromptEditMode)
	}
	if modeChange != "vi" {
		t.Errorf("/vi on: SetPromptEditMode called with %q, want vi", modeChange)
	}
	if len(savedModes) != 1 || savedModes[0] != "vi" {
		t.Errorf("/vi on: SaveReplEditMode called with %v, want [vi]", savedModes)
	}

	// /vi off
	modeChange = ""
	app.command("/vi off", nil)
	if app.PromptEditMode != "emacs" {
		t.Errorf("/vi off: PromptEditMode = %q, want emacs", app.PromptEditMode)
	}
	if modeChange != "emacs" {
		t.Errorf("/vi off: SetPromptEditMode called with %q, want emacs", modeChange)
	}
	if len(savedModes) != 2 || savedModes[1] != "emacs" {
		t.Errorf("/vi off: SaveReplEditMode called with %v, want appended emacs", savedModes)
	}

	// /vi vi (alias for on)
	modeChange = ""
	app.command("/vi vi", nil)
	if app.PromptEditMode != "vi" {
		t.Errorf("/vi vi: PromptEditMode = %q, want vi", app.PromptEditMode)
	}
	if modeChange != "vi" {
		t.Errorf("/vi vi: SetPromptEditMode called with %q, want vi", modeChange)
	}

	// /vi vim (alias for on)
	modeChange = ""
	app.command("/vi vim", nil)
	if app.PromptEditMode != "vi" {
		t.Errorf("/vi vim: PromptEditMode = %q, want vi", app.PromptEditMode)
	}
	if modeChange != "vi" {
		t.Errorf("/vi vim: SetPromptEditMode called with %q, want vi", modeChange)
	}

	// /vi emacs (alias for off)
	modeChange = ""
	app.command("/vi emacs", nil)
	if app.PromptEditMode != "emacs" {
		t.Errorf("/vi emacs: PromptEditMode = %q, want emacs", app.PromptEditMode)
	}
	if modeChange != "emacs" {
		t.Errorf("/vi emacs: SetPromptEditMode called with %q, want emacs", modeChange)
	}

	// /vi alone (status)
	errw := app.Errw.(*bytes.Buffer)
	errw.Reset()
	app.command("/vi", nil)
	if !strings.Contains(errw.String(), "emacs") {
		t.Errorf("/vi (status): expected current mode in output, got %q", errw.String())
	}

	// /vi status
	errw.Reset()
	app.command("/vi status", nil)
	if !strings.Contains(errw.String(), "emacs") {
		t.Errorf("/vi status: expected current mode in output, got %q", errw.String())
	}

	// /vi bogus
	errw.Reset()
	app.command("/vi bogus", nil)
	if !strings.Contains(strings.ToLower(errw.String()), "unknown") {
		t.Errorf("/vi bogus: expected error, got %q", errw.String())
	}
}

func TestREPLLiteralSlashEscape(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("//not-a-command\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("// escape should send a prompt, got %d requests", fp.RequestCount())
	}
	// The leading slash is restored; the doubled slash is the escape.
	sent := app.Agent.Transcript()[0].Content[0].Text
	if sent != "/not-a-command" {
		t.Errorf("escaped prompt = %q, want %q", sent, "/not-a-command")
	}
}

func TestREPLInteractiveBangRunsLocalShellOnly(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	var events []string
	app.BeforeEditor = func() { events = append(events, "before") }
	app.AfterEditor = func() { events = append(events, "after") }
	app.RunShellCommand = func(command string) error {
		events = append(events, "run:"+command)
		fmt.Fprintln(app.Errw, "foo")
		return nil
	}

	if code := run(strings.NewReader("!echo foo\r/exit\r"), app, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("bang command should not invoke provider, got %d requests", fp.RequestCount())
	}
	if got := strings.Join(events, ","); got != "before,run:echo foo,after" {
		t.Fatalf("shell handoff events = %q", got)
	}
	if !strings.Contains(errw.String(), "foo\n") {
		t.Fatalf("shell output missing from REPL output: %q", errw.String())
	}
}

func TestREPLBangIsLiteralWithoutInteractivePromptEditor(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RunShellCommand = func(command string) error {
		t.Fatalf("non-interactive bang should not run shell command %q", command)
		return nil
	}

	if code := Run(strings.NewReader("!echo foo\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != "!echo foo" {
		t.Fatalf("prompt = %q, want literal bang prompt", got)
	}
}

func TestREPLInteractiveDoubleBangEscapesLiteralBang(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RunShellCommand = func(command string) error {
		t.Fatalf("escaped bang should not run shell command %q", command)
		return nil
	}

	if code := run(strings.NewReader("!!hello\r/exit\r"), app, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != "!hello" {
		t.Fatalf("prompt = %q, want !hello", got)
	}
}

func TestREPLBracketedPasteSubmittedAsSingleLiteralPrompt(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	pasted := "/exit is pasted text\nsecond line\nthird line"
	in := strings.NewReader(bracketedPasteStart + pasted + bracketedPasteEnd + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("bracketed paste should send one prompt, got %d requests", fp.RequestCount())
	}
	sent := app.Agent.Transcript()[0].Content[0].Text
	if sent != pasted {
		t.Errorf("pasted prompt = %q, want %q", sent, pasted)
	}
}

func TestREPLPastedBangStaysLiteral(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RunShellCommand = func(command string) error {
		t.Fatalf("pasted bang should not run shell command %q", command)
		return nil
	}

	pasted := "!echo foo"
	in := strings.NewReader(bracketedPasteStart + pasted + bracketedPasteEnd + "\r/exit\r")
	if code := run(in, app, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != pasted {
		t.Fatalf("prompt = %q, want pasted literal bang prompt", got)
	}
}

func TestREPLTypedSkillMentionAddsRequestContext(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	commit := testSkill(t, "commit", "Create a git commit", "REPL SKILL BODY")
	app.Skills = map[string]skills.Skill{
		"commit": commit,
	}

	if code := Run(strings.NewReader("please use $commit\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != "please use $commit" {
		t.Fatalf("user prompt should be preserved, got %q", got)
	}
	got := strings.Join(req.RequestContext, "\n\n")
	if !strings.Contains(got, "[active skill instructions]") ||
		!strings.Contains(got, "source: "+commit.Location) ||
		!strings.Contains(got, "REPL SKILL BODY") {
		t.Fatalf("request context missing skill instructions:\n%s", got)
	}
}

func TestREPLPromptEditorTabCompletesSkillMention(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	commit := testSkill(t, "commit", "Create a git commit", "TAB SKILL BODY")
	app.Skills = map[string]skills.Skill{
		"commit": commit,
	}

	if code := run(strings.NewReader("please $com\tnow\r/exit\r"), app, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != "please $commit now" {
		t.Fatalf("tab-completed prompt = %q, want %q", got, "please $commit now")
	}
	if got := strings.Join(req.RequestContext, "\n\n"); !strings.Contains(got, "[active skill instructions]") ||
		!strings.Contains(got, "TAB SKILL BODY") {
		t.Fatalf("tab-completed prompt should add skill context:\n%s", got)
	}
}

func TestREPLTypedEscapedSkillMentionStillScansLaterMentions(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	review := testSkill(t, "review", "Review code", "REVIEW SKILL BODY")
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
		"review": review,
	}

	if code := Run(strings.NewReader("$$commit and use $review\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != "$commit and use $review" {
		t.Fatalf("user prompt should unescape only the escaped dollar, got %q", got)
	}
	got := strings.Join(req.RequestContext, "\n\n")
	if strings.Contains(got, "name: commit") {
		t.Fatalf("escaped skill mention should not add commit context:\n%s", got)
	}
	if !strings.Contains(got, "source: "+review.Location) || !strings.Contains(got, "REVIEW SKILL BODY") {
		t.Fatalf("later skill mention should add review context:\n%s", got)
	}
}

func TestREPLPastedSkillMentionStaysLiteral(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	}

	pasted := "please use $commit"
	in := strings.NewReader(bracketedPasteStart + pasted + bracketedPasteEnd + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != pasted {
		t.Fatalf("pasted prompt = %q, want %q", got, pasted)
	}
	if got := strings.Join(req.RequestContext, "\n\n"); strings.Contains(got, "[active skill instructions]") {
		t.Fatalf("pasted prompt should not add skill context:\n%s", got)
	}
}

func TestREPLStandaloneUnknownSkillSkipsProvider(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	}

	if code := Run(strings.NewReader("$missing\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider requests = %d, want 0", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), `unknown skill "missing"`) {
		t.Fatalf("missing unknown skill notice, errw=%q", errw.String())
	}
}

func TestREPLAcceptsPromptLongerThanScannerLimit(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	prompt := strings.Repeat("x", 4*1024*1024+1)
	in := strings.NewReader(prompt + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("long prompt should send one request, got %d", fp.RequestCount())
	}
	sent := app.Agent.Transcript()[0].Content[0].Text
	if sent != prompt {
		t.Fatalf("long prompt length = %d, want %d", len(sent), len(prompt))
	}
}

func TestREPLUsageCumulative(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("a")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("b")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 200, OutputTokens: 20}},
	)
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("p1\np2\n/usage\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	// Cumulative: 300 in / 30 out across both turns.
	if !strings.Contains(got, "300") || !strings.Contains(got, "30 out") {
		t.Errorf("/usage should show cumulative tokens, errw=%q", got)
	}
}

func TestREPLExitPrintsUsageSummary(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("a")},
			Stop:   llm.StopEndTurn,
			Usage: llm.Usage{
				InputTokens:      100,
				CacheReadTokens:  30,
				CacheWriteTokens: 20,
				OutputTokens:     10,
				ReasoningTokens:  4,
			},
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("p1\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	want := "[session summary: 100 input / 30 cached input / 10 output / 4 reasoning / 20 cache write · 0 compactions]\nresume with: harness -resume " + app.SessionPath
	if !strings.Contains(got, want) {
		t.Errorf("exit should print usage summary and resume hint %q, errw=%q", want, got)
	}
}

func TestREPLToolsCommandListsBuiltInMCPAndDisabledTools(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	reg := tools.Default()
	reg.Register(mcpRefreshTool{name: "mcp__search__lookup"})
	reg.Register(mcpRefreshTool{name: "mcp__files__read"})
	app.Agent.SetTools(reg)
	app.DisabledTools = []tools.DisabledTool{
		{Name: "rg", Reason: `"rg" binary not found`},
	}

	in := strings.NewReader("/tools\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("/tools should not invoke the agent, got %d requests", fp.RequestCount())
	}
	got := errw.String()
	for _, want := range []string{
		"built-in tools:",
		"mcp tools:",
		"  [files]",
		"    mcp__files__read  refreshed tool",
		"  [search]",
		"    mcp__search__lookup  refreshed tool",
		"disabled tools:",
		`  rg  ("rg" binary not found)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/tools output missing %q:\n%s", want, got)
		}
	}
	readFileCol := toolSummaryDescriptionColumn(t, got, "read_file", "Read one file with optional offset/limit, or batch paths[]; returns line-numbered content.")
	listDirCol := toolSummaryDescriptionColumn(t, got, "list_dir", "List one directory with an optional base-name glob; non-recursive.")
	if readFileCol != listDirCol {
		t.Errorf("built-in description separators not aligned:\n%s", got)
	}
}

func TestREPLSkillsCommandAlignsAndWrapsDescriptions(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SummaryWidth = func() int { return 42 }
	app.Skills = map[string]skills.Skill{
		"alpha": {
			Name:        "alpha",
			Description: "short description",
			Scope:       skills.ScopeProject,
		},
		"beta-long": {
			Name:        "beta-long",
			Description: "one two three four five six seven",
			Scope:       skills.ScopeProject,
		},
	}

	if code := Run(strings.NewReader("/skills\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	for _, want := range []string{
		"local skills:",
		"  $alpha      short description",
		"  $beta-long  one two three four five six",
		"              seven",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/skills output missing %q:\n%s", want, got)
		}
	}
}

func toolSummaryDescriptionColumn(t *testing.T, summary, name, wantDescription string) int {
	t.Helper()
	for _, line := range strings.Split(summary, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != name {
			continue
		}
		trimmed := strings.TrimSpace(line)
		gotDescription := strings.TrimSpace(strings.TrimPrefix(trimmed, name))
		if gotDescription != wantDescription {
			t.Fatalf("summary description for %q = %q, want %q:\n%s", name, gotDescription, wantDescription, summary)
		}
		return strings.Index(line, wantDescription)
	}
	t.Fatalf("summary missing tool %q:\n%s", name, summary)
	return -1
}

func TestREPLUsageLineSeedsFromSavedUsage(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("a")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 50, OutputTokens: 5}},
	)
	app := newTestApp(t, &out, &errw, fp)
	app.SetUsage(session.UsageTotals{Usage: llm.Usage{InputTokens: 300, OutputTokens: 30}})

	in := strings.NewReader("p1\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "50 (350) in") {
		t.Errorf("usage line should include seeded input total, errw=%q", got)
	}
	if !strings.Contains(got, "5 (35) out") {
		t.Errorf("usage line should include seeded output total, errw=%q", got)
	}
}

func TestREPLClearResetsUsageLineCumulative(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("a")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("b")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 200, OutputTokens: 20}},
	)
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("p1\n/clear\np2\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "100 (100) in") {
		t.Errorf("first turn should show its own cumulative input, errw=%q", got)
	}
	if !strings.Contains(got, "200 (200) in") {
		t.Errorf("post-clear turn should reset cumulative input, errw=%q", got)
	}
	if strings.Contains(got, "200 (300) in") {
		t.Errorf("post-clear turn leaked pre-clear input total, errw=%q", got)
	}
	// /clear echoes the outgoing totals before zeroing them (r26).
	if !strings.Contains(got, "cleared session") || !strings.Contains(got, "100 input") {
		t.Errorf("/clear should echo the discarded session totals, errw=%q", got)
	}
}

func TestREPLUsageLineIncludesCompactUsage(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 9100, OutputTokens: 400}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("after compact")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}},
	)
	app := newTestApp(t, &out, &errw, fp)

	var seed []llm.Message
	for i := 0; i < 10; i++ {
		label := string(rune('a' + i))
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " a"}}},
		)
	}
	app.Agent.SetTranscript(seed)

	in := strings.NewReader("/compact\np1\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "100 (9.2k) in") {
		t.Errorf("post-compact turn should include compact input usage in cumulative total, errw=%q", got)
	}
	if !strings.Contains(got, "10 (410) out") {
		t.Errorf("post-compact turn should include compact output usage in cumulative total, errw=%q", got)
	}
	if strings.Contains(got, "compactions 0") || strings.Contains(got, "(1 total)") {
		t.Errorf("prompt summary should omit zero prompt and single session compaction counts, errw=%q", got)
	}
}

func TestREPLCompactCommand(t *testing.T) {
	var out, errw bytes.Buffer
	// The only model call here is the summary call /compact triggers.
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 9100, OutputTokens: 400}},
	)
	app := newTestApp(t, &out, &errw, fp)

	// Seed enough whole turns that there is something older than the last eight
	// to summarize.
	var seed []llm.Message
	for i := 0; i < 10; i++ {
		label := string(rune('a' + i))
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " a"}}},
		)
	}
	app.Agent.SetTranscript(seed)

	in := strings.NewReader("/compact\n/usage\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	msgs := app.Agent.Transcript()
	if err := llm.ValidateTranscript(msgs); err != nil {
		t.Fatalf("transcript invalid after /compact: %v", err)
	}
	if len(msgs) != 16 {
		t.Fatalf("/compact should collapse to checkpoint + last 8 turns, got %d", len(msgs))
	}
	got := errw.String()
	if !strings.Contains(got, "compacted") {
		t.Errorf("/compact should print a compaction report, errw=%q", got)
	}
	// The summary call's tokens must fold into the cumulative session totals.
	if !strings.Contains(got, "9100") || !strings.Contains(got, "400 out") {
		t.Errorf("/usage should include the summary call usage after /compact, errw=%q", got)
	}
	if !strings.Contains(got, "1 compaction") {
		t.Errorf("/usage and exit summary should include the compaction total, errw=%q", got)
	}
	// The summary call was actually issued (the only model call here).
	if fp.RequestCount() != 1 {
		t.Errorf("/compact should issue exactly the summary call, got %d requests", fp.RequestCount())
	}
}

func TestREPLCompactCommandPassesOneShotFocus(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	var seed []llm.Message
	for i := 0; i < 10; i++ {
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "a"}}},
		)
	}
	app.Agent.SetTranscript(seed)
	if code := Run(strings.NewReader("/compact preserve API names\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(fp.Requests[0].System, "preserve API names") {
		t.Fatalf("summary system missing focus: %q", fp.Requests[0].System)
	}
	meta := app.Agent.Transcript()[0].Compaction
	if meta == nil || meta.Focus != "preserve API names" {
		t.Fatalf("checkpoint focus = %+v", meta)
	}
}

func TestREPLCompactShowsLiveProgressWhileSummaryBlocked(t *testing.T) {
	var out, errw lockedBuffer
	inSummary := make(chan struct{})
	releaseSummary := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSummary) }) }
	defer release()

	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 9100, OutputTokens: 400},
		Block: func(context.Context) {
			close(inSummary)
			<-releaseSummary
		},
	})
	app := liveTestApp(t, &out, &errw, fp)

	var seed []llm.Message
	for i := 0; i < 10; i++ {
		label := string(rune('a' + i))
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " a"}}},
		)
	}
	app.Agent.SetTranscript(seed)

	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(strings.NewReader("/compact\n/exit\n"), app, nil) }()

	select {
	case <-inSummary:
	case <-time.After(time.Second):
		t.Fatal("compaction summary call did not start")
	}
	if got := errw.String(); !strings.Contains(got, "[context: compacting · 0s]") {
		t.Fatalf("live compaction status was not visible while the summary call blocked: %q", got)
	}
	if got := out.String(); got != "" {
		t.Fatalf("compaction progress must leave stdout untouched, got %q", got)
	}

	release()
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ExitOK, errw.String())
	}
	if got := errw.String(); !strings.Contains(got, "[compacted:") {
		t.Fatalf("completed /compact should retain its durable report, errw=%q", got)
	}
}

func TestREPLIdleCompactionAppliesStableSnapshot(t *testing.T) {
	var out, errw lockedBuffer
	inSummary := make(chan struct{})
	releaseSummary := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("IDLE SUMMARY")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 100, OutputTokens: 12},
		Block: func(context.Context) {
			close(inSummary)
			<-releaseSummary
		},
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Agent.SetModel("claude-opus-4-8", 10_000)
	app.Agent.SetTranscript(compactionSeed())
	app.Agent.SetCompactionArchiver(func(_ context.Context, archive agent.CompactionArchive) (string, error) {
		ref, err := session.SaveCompaction(app.SessionPath, session.Compaction{
			Time:          time.Now(),
			Summary:       archive.Summary,
			Usage:         archive.Usage,
			Messages:      archive.Messages,
			Focus:         archive.Focus,
			ReadFiles:     archive.ReadFiles,
			ModifiedFiles: archive.ModifiedFiles,
		})
		if err != nil {
			return "", err
		}
		if err := app.PrepareCompaction(app.Agent.Transcript(), len(archive.Messages), archive.Summary, ref, archive.TokensBefore, archive.Focus, archive.ReadFiles, archive.ModifiedFiles); err != nil {
			return "", err
		}
		return ref, nil
	})
	app.IdleCompactionAfter = time.Minute
	app.IdleCompactionTriggerPercent = 1
	idleTimer := make(chan time.Time, 1)
	timerCalls := 0
	app.idleCompactionAfter = func(time.Duration) <-chan time.Time {
		timerCalls++
		if timerCalls == 1 {
			return idleTimer
		}
		return nil
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()
	idleTimer <- time.Now()
	select {
	case <-inSummary:
	case <-time.After(time.Second):
		t.Fatal("idle compaction did not start")
	}
	close(releaseSummary)
	waitFor(t, func() bool {
		return strings.Contains(errw.String(), "[idle compacted:")
	}, "idle compaction application")

	writePipe(t, pw, "/usage\n/exit\n")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	got := app.Agent.Transcript()
	if len(got) != 16 || got[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("idle transcript = %d messages, want checkpoint + 8 turns", len(got))
	}
	if !strings.Contains(got[0].Content[0].Text, "compactions/0001.input.json") {
		t.Fatalf("idle checkpoint missing archive reference: %q", got[0].Content[0].Text)
	}
	if text := errw.String(); !strings.Contains(text, "100 in") || !strings.Contains(text, "12 out") {
		t.Fatalf("idle maintenance usage missing from /usage:\n%s", text)
	} else if !strings.Contains(text, "1 compaction") {
		t.Fatalf("applied idle compaction missing from /usage:\n%s", text)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want one idle summary", fp.RequestCount())
	}
	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read idle telemetry: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"type":"idle_compaction"`)) || !bytes.Contains(raw, []byte(`"outcome":"applied"`)) {
		t.Fatalf("applied idle telemetry missing:\n%s", raw)
	}
}

func TestREPLIdleCompactionDiscardsLateResultAfterInput(t *testing.T) {
	var out, errw lockedBuffer
	inSummary := make(chan struct{})
	releaseSummary := make(chan struct{})
	partialUsage := llm.Usage{InputTokens: 75, OutputTokens: 5}
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				textDelta("STALE SUMMARY"),
				{Kind: llm.EventUsage, Usage: &partialUsage},
			},
			Stop: llm.StopEndTurn,
			Block: func(context.Context) {
				close(inSummary)
				<-releaseSummary
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("new answer")},
			Stop:   llm.StopEndTurn,
		},
	)
	app := newTestApp(t, &out, &errw, fp)
	app.Agent.SetModel("claude-opus-4-8", 10_000)
	app.Agent.SetTranscript(compactionSeed())
	app.IdleCompactionAfter = time.Minute
	app.IdleCompactionTriggerPercent = 1
	idleTimer := make(chan time.Time, 1)
	timerCalls := 0
	app.idleCompactionAfter = func(time.Duration) <-chan time.Time {
		timerCalls++
		if timerCalls == 1 {
			return idleTimer
		}
		return nil
	}
	promptFinished := make(chan struct{}, 1)
	app.OnPromptFinished = func() { promptFinished <- struct{}{} }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()
	idleTimer <- time.Now()
	select {
	case <-inSummary:
	case <-time.After(time.Second):
		t.Fatal("idle compaction did not start")
	}

	writePipe(t, pw, "continue now\n")
	select {
	case <-promptFinished:
	case <-time.After(time.Second):
		t.Fatal("submitted prompt waited for idle compaction")
	}
	close(releaseSummary)
	maintenancePath := filepath.Join(app.SessionPath, "raw.ndjson")
	waitFor(t, func() bool {
		data, _ := os.ReadFile(maintenancePath)
		return bytes.Contains(data, []byte(`"type":"idle_compaction"`)) &&
			bytes.Contains(data, []byte(`"outcome":"discarded"`))
	}, "discarded idle usage accounting")

	writePipe(t, pw, "/usage\n/exit\n")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	got := app.Agent.Transcript()
	if len(got) != 22 || got[0].Origin == llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("late idle result changed transcript: %d messages, first origin %q", len(got), got[0].Origin)
	}
	if text := transcriptTextForTest(got); !strings.Contains(text, "continue now") || !strings.Contains(text, "new answer") || strings.Contains(text, "STALE SUMMARY") {
		t.Fatalf("transcript after stale idle result:\n%s", text)
	}
	if text := errw.String(); !strings.Contains(text, "75 in") || !strings.Contains(text, "5 out") {
		t.Fatalf("discarded idle usage missing from /usage:\n%s", text)
	}
	raw, err := os.ReadFile(maintenancePath)
	if err != nil {
		t.Fatalf("read idle telemetry: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"purpose":"idle_compaction"`)) {
		t.Fatalf("discarded idle maintenance usage missing:\n%s", raw)
	}
}

func TestREPLIdleCompactionFinishDuringActivePrompt(t *testing.T) {
	var out, errw lockedBuffer
	inSummary := make(chan struct{})
	releaseSummary := make(chan struct{})
	promptStarted := make(chan struct{})
	releasePrompt := make(chan struct{})
	idleUsage := llm.Usage{InputTokens: 75, OutputTokens: 5}
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				textDelta("STALE SUMMARY"),
				{Kind: llm.EventUsage, Usage: &idleUsage},
			},
			Stop: llm.StopEndTurn,
			Block: func(context.Context) {
				close(inSummary)
				<-releaseSummary
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "grep-1",
				ToolName:  "grep",
				ToolInput: json.RawMessage(`{"pattern":"x"}`),
			}},
			Stop: llm.StopToolUse,
			Block: func(context.Context) {
				close(promptStarted)
				<-releasePrompt
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("prompt answer")},
			Stop:   llm.StopEndTurn,
		},
	)
	app := newTestApp(t, &out, &errw, fp)
	app.Agent.SetModel("claude-opus-4-8", 10_000)
	app.Agent.SetTranscript(compactionSeed())
	app.IdleCompactionAfter = time.Minute
	app.IdleCompactionTriggerPercent = 1
	idleTimer := make(chan time.Time, 1)
	timerCalls := 0
	app.idleCompactionAfter = func(time.Duration) <-chan time.Time {
		timerCalls++
		if timerCalls == 1 {
			return idleTimer
		}
		return nil
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()
	idleTimer <- time.Now()
	select {
	case <-inSummary:
	case <-time.After(time.Second):
		t.Fatal("idle compaction did not start")
	}

	writePipe(t, pw, "keep working\n")
	select {
	case <-promptStarted:
	case <-time.After(time.Second):
		t.Fatal("submitted prompt did not start")
	}
	// Release the worker while the prompt is mid-run, then release the model.
	// The agent closes the tool-use turn with a PromptCheckpoint that reads the
	// app's usage maps from the prompt goroutine at the same time the REPL
	// receives the idle result in the active branch, whose accounting must be
	// queued and drained after the run — not applied concurrently with it.
	close(releaseSummary)
	close(releasePrompt)

	writePipe(t, pw, "/usage\n/exit\n")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if got := app.Agent.Transcript(); len(got) != 24 || got[0].Origin == llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("late idle result changed transcript: %d messages", len(got))
	}
	if text := errw.String(); !strings.Contains(text, "75 in") || !strings.Contains(text, "5 out") {
		t.Fatalf("discarded idle usage missing from /usage:\n%s", text)
	}
	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read idle telemetry: %v", err)
	}
	if got := bytes.Count(raw, []byte(`"purpose":"idle_compaction"`)); got != 1 {
		t.Fatalf("idle maintenance usage records = %d, want exactly 1:\n%s", got, raw)
	}
	if got := bytes.Count(raw, []byte(`"outcome":"discarded"`)); got != 1 {
		t.Fatalf("discarded idle events = %d, want exactly 1:\n%s", got, raw)
	}
}

func TestREPLExitDoesNotWaitForIdleCompaction(t *testing.T) {
	var out, errw lockedBuffer
	inSummary := make(chan struct{})
	releaseSummary := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("UNUSED SUMMARY")},
		Stop:   llm.StopEndTurn,
		Block: func(context.Context) {
			close(inSummary)
			<-releaseSummary
		},
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Agent.SetModel("claude-opus-4-8", 10_000)
	app.Agent.SetTranscript(compactionSeed())
	app.IdleCompactionAfter = time.Minute
	app.IdleCompactionTriggerPercent = 1
	idleTimer := make(chan time.Time, 1)
	timerCalls := 0
	app.idleCompactionAfter = func(time.Duration) <-chan time.Time {
		timerCalls++
		if timerCalls == 1 {
			return idleTimer
		}
		return nil
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()
	idleTimer <- time.Now()
	select {
	case <-inSummary:
	case <-time.After(time.Second):
		t.Fatal("idle compaction did not start")
	}
	writePipe(t, pw, "/exit\n")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	close(releaseSummary)

	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read idle telemetry: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"type":"idle_compaction"`)) || !bytes.Contains(raw, []byte(`"outcome":"discarded"`)) {
		t.Fatalf("exit-discard telemetry missing:\n%s", raw)
	}
	if got := app.Agent.Transcript(); len(got) != 20 || got[0].Origin == llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("exit applied in-flight idle candidate: %d messages", len(got))
	}
}

func compactionSeed() []llm.Message {
	var seed []llm.Message
	for i := range 10 {
		label := string(rune('a' + i))
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " a"}}},
		)
	}
	return seed
}

func transcriptTextForTest(messages []llm.Message) string {
	var text []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockText {
				text = append(text, block.Text)
			}
		}
	}
	return strings.Join(text, "\n")
}

func TestREPLModelCommand(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AvailableModels = []string{"gpt-5.5", "claude-opus-4-8"}

	in := strings.NewReader("/model\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	if !strings.Contains(got, "anthropic") || !strings.Contains(got, "claude-opus-4-8") || !strings.Contains(got, "api.anthropic.com") {
		t.Errorf("/model should print provider/model/base-url, errw=%q", got)
	}
	if !strings.Contains(got, "available models:") || !strings.Contains(got, "gpt-5.5") {
		t.Errorf("/model should list available models, errw=%q", got)
	}
}

type replTestModelPick struct {
	id   string
	name string
}

func (m replTestModelPick) PickerID() string   { return m.id }
func (m replTestModelPick) PickerName() string { return m.name }

func TestREPLAuxiliaryPromptsKeepContextLabelsWithViModePrompt(t *testing.T) {
	var out, errw bytes.Buffer
	initial := llmtest.New("initial")
	switched := llmtest.New("switched")
	app := newTestApp(t, &out, &errw, initial)
	app.Prompt = "MAIN {vimode:short}> "
	app.PromptEditMode = "vi"
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"openai:gpt-5.5": {
			Reasoning: &llm.ReasoningInfo{
				Supported: true,
				Options: []llm.ReasoningOption{{
					Type:   "effort",
					Values: []string{"minimal", "low", "medium", "high", "xhigh", "max"},
				}},
			},
		},
	})
	app.PickModel = func(pio PickerIO) (string, error) {
		model, err := Pick(pio.ReadLine, pio.Writer, PickerOptions[replTestModelPick]{
			Items: []replTestModelPick{{
				id:   "openai:gpt-5.5",
				name: "gpt-5.5",
			}},
			PageSize: pio.PageSize,
			Prompt:   "Model target (number/id, /search, n/p, q): ",
			Kind:     "model target",
		})
		if err != nil {
			return "", err
		}
		return model.id, nil
	}
	var switchedReasoning llm.ReasoningConfig
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		if model != "openai:gpt-5.5" {
			t.Fatalf("switch model = %q, want openai:gpt-5.5", model)
		}
		switchedReasoning = reasoning
		return ModelSelection{
			Provider:      "openai",
			Model:         "gpt-5.5",
			RegistryModel: model,
			BaseURL:       "https://api.openai.com/v1",
			Runtime:       switched,
			Reasoning:     reasoning,
		}, nil
	}
	app.PromptDefaultModelSave = true
	app.SaveDefaultModel = func(provider, model string, reasoning llm.ReasoningConfig) error {
		t.Fatal("SaveDefaultModel should not be called after declining the prompt")
		return nil
	}

	if code := run(strings.NewReader("/model\r1\rhigh\rn\r/exit\r"), app, nil, true); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ExitOK, errw.String())
	}
	if switchedReasoning.Profile != "high" {
		t.Fatalf("switch reasoning profile = %q, want high", switchedReasoning.Profile)
	}
	got := errw.String()
	for _, want := range []string{
		"MAIN I> ",
		"Model target (number/id, /search, n/p, q): ",
		"Reasoning profile (default/none/minimal/low/medium/high/xhigh/max",
		"Save openai:gpt-5.5 as the default model? (y/N): ",
		"model switched",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Invalid reasoning profile") {
		t.Fatalf("auxiliary prompts consumed the scripted flow incorrectly:\n%s", got)
	}
}

func TestREPLModelCommandSwitchesNextTurn(t *testing.T) {
	var out, errw bytes.Buffer
	initial := llmtest.New("initial")
	switched := llmtest.New("switched", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("switched reply")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, initial)
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		if model != "gpt-5.5" {
			t.Fatalf("switch model = %q, want gpt-5.5", model)
		}
		return ModelSelection{
			Provider:  "openai",
			Model:     model,
			BaseURL:   "https://api.openai.com/v1",
			Runtime:   switched,
			Reasoning: reasoning,
		}, nil
	}

	in := strings.NewReader("/model gpt-5.5\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(initial.Requests) != 0 {
		t.Fatalf("initial provider should not receive the post-switch turn, got %d requests", len(initial.Requests))
	}
	if len(switched.Requests) != 1 {
		t.Fatalf("switched provider requests = %d, want 1", len(switched.Requests))
	}
	if switched.Requests[0].Model != "gpt-5.5" {
		t.Fatalf("post-switch request model = %q, want gpt-5.5", switched.Requests[0].Model)
	}
	if app.Provider != "openai" || app.Model != "gpt-5.5" {
		t.Fatalf("app provider/model = %s/%s, want openai/gpt-5.5", app.Provider, app.Model)
	}
	if !strings.Contains(errw.String(), "model switched") {
		t.Errorf("switch should be acknowledged, errw=%q", errw.String())
	}
}

func TestREPLEffortCommandAliasesReasoningProfile(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RegistryModel = "anthropic:claude-opus-4-8"
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {
			Reasoning: &llm.ReasoningInfo{
				Supported: true,
			},
		},
	})

	in := strings.NewReader("/effort\n/effort high\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	for _, want := range []string{
		"available controls for anthropic:claude-opus-4-8:",
		"profile: default, none, minimal, low, medium, high, xhigh, max",
		"[reasoning: profile=high]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/effort output missing %q:\n%s", want, got)
		}
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.Profile != "high" {
		t.Fatalf("request profile = %q, want high", fp.Requests[0].Reasoning.Profile)
	}
}

func TestREPLReasoningCommandSendsExplicitNone(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RegistryModel = "openrouter:z-ai/glm-5.1"
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"openrouter:z-ai/glm-5.1": {
			Reasoning: &llm.ReasoningInfo{
				Supported: true,
			},
		},
	})

	in := strings.NewReader("/reasoning none\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "[reasoning: profile=none]") {
		t.Fatalf("reasoning none should be acknowledged, errw=%q", errw.String())
	}
	if fp.Requests[0].Reasoning.Profile != "none" {
		t.Fatalf("request profile = %q, want none", fp.Requests[0].Reasoning.Profile)
	}
}

func TestREPLFastCommandSwitchesSiblingTargetsWithoutRotatingSession(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("one")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("two")}, Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	baseTarget := "anthropic:claude-opus-4-8"
	fastTarget := baseTarget + ":fast"
	app.Provider = baseTarget
	app.Model = baseTarget
	app.RegistryModel = baseTarget
	app.BaseTargetID = baseTarget
	app.FastTargetID = fastTarget
	app.Agent.SetModel(baseTarget, 0)
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		selection := ModelSelection{
			Provider:      model,
			Model:         model,
			RegistryModel: model,
			BaseURL:       "proxy",
			Runtime:       fp,
			Reasoning:     reasoning,
			BaseTargetID:  baseTarget,
			FastTargetID:  fastTarget,
			ReasoningSet:  true,
		}
		switch model {
		case baseTarget:
			return selection, nil
		case fastTarget:
			selection.Variant = "fast"
			return selection, nil
		default:
			return ModelSelection{}, fmt.Errorf("unknown model %q", model)
		}
	}

	input := strings.NewReader("/fast\none\n/fast status\n/fast\ntwo\n/exit\n")
	if code := Run(input, app, nil); code != 0 {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("provider requests = %d, want 2", fp.RequestCount())
	}
	if fp.Requests[0].Model != fastTarget || fp.Requests[1].Model != baseTarget {
		t.Fatalf("request models = %q then %q, want fast then base", fp.Requests[0].Model, fp.Requests[1].Model)
	}
	if fp.Requests[0].ProxySessionID == "" || fp.Requests[0].ProxySessionID != fp.Requests[1].ProxySessionID {
		t.Fatalf("proxy session IDs = %q then %q, want preserved across sibling variants", fp.Requests[0].ProxySessionID, fp.Requests[1].ProxySessionID)
	}
	for _, want := range []string{"[fast mode: on]", "[fast mode: off]"} {
		if !strings.Contains(errw.String(), want) {
			t.Fatalf("output missing %q: %s", want, errw.String())
		}
	}
}

func TestSwitchModelPreservesValidContinuationAcrossFastVariants(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("responses")
	app := newTestApp(t, &out, &errw, fp)
	baseTarget := "openai:gpt-5.5"
	fastTarget := baseTarget + ":fast"
	app.Provider = baseTarget
	app.Model = baseTarget
	app.RegistryModel = baseTarget
	app.BaseTargetID = baseTarget
	app.FastTargetID = fastTarget
	app.Agent.SetModel(baseTarget, 0)
	app.Agent.SetResponsesStateful(true)
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("hello"), uiAsstMsg("answer")})
	digest, err := llm.FingerprintMessages(app.Agent.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	app.Agent.SetResponseState(&llm.ResponseState{
		PreviousResponseID: "resp-base",
		AnchorMessages:     len(app.Agent.Transcript()),
		AnchorDigest:       digest,
	})
	proxySessionID := app.Agent.ProxySessionID()
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		base := baseTarget
		if model == "other:model" {
			base = model
		}
		return ModelSelection{
			Provider:          model,
			Model:             model,
			RegistryModel:     model,
			Runtime:           fp,
			BaseTargetID:      base,
			FastTargetID:      fastTarget,
			ResponsesStateful: true,
			ReasoningSet:      true,
			Reasoning:         reasoning,
		}, nil
	}

	if !app.switchModel(fastTarget, llm.ReasoningConfig{}) {
		t.Fatalf("fast switch failed: %s", errw.String())
	}
	if state := app.Agent.ResponseState(); state == nil || state.PreviousResponseID != "resp-base" || state.AnchorDigest != digest {
		t.Fatalf("fast state = %+v", state)
	}
	if app.Agent.ProxySessionID() != proxySessionID {
		t.Fatal("fast sibling switch rotated proxy session")
	}
	if !app.switchModel(baseTarget, llm.ReasoningConfig{}) {
		t.Fatal("base switch failed")
	}
	if state := app.Agent.ResponseState(); state == nil || state.PreviousResponseID != "resp-base" {
		t.Fatalf("base state = %+v", state)
	}
	if !app.switchModel("other:model", llm.ReasoningConfig{}) {
		t.Fatal("true model switch failed")
	}
	if app.Agent.ResponseState() != nil || app.Agent.ProxySessionID() == proxySessionID {
		t.Fatalf("true model switch retained continuation: state=%+v proxy=%q", app.Agent.ResponseState(), app.Agent.ProxySessionID())
	}
}

func TestSwitchModelDoesNotRestoreMismatchedContinuationDigest(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("responses")
	app := newTestApp(t, &out, &errw, fp)
	app.BaseTargetID = "openai:gpt-5.5"
	app.Agent.SetResponsesStateful(true)
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("hello"), uiAsstMsg("answer")})
	app.Agent.SetResponseState(&llm.ResponseState{
		PreviousResponseID: "resp-bad",
		AnchorMessages:     len(app.Agent.Transcript()),
		AnchorDigest:       strings.Repeat("0", 64),
	})
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		return ModelSelection{
			Provider:          model,
			Model:             model,
			RegistryModel:     model,
			Runtime:           fp,
			BaseTargetID:      "openai:gpt-5.5",
			ResponsesStateful: true,
			ReasoningSet:      true,
		}, nil
	}
	if !app.switchModel("openai:gpt-5.5:fast", llm.ReasoningConfig{}) {
		t.Fatal("switch failed")
	}
	if state := app.Agent.ResponseState(); state != nil {
		t.Fatalf("mismatched state restored: %+v", state)
	}
}

func TestREPLFastCommandReportsUnavailableForCurrentModel(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))

	if code := Run(strings.NewReader("/fast\n/fast status\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.Count(errw.String(), "[fast mode unavailable for current model]"); got != 2 {
		t.Fatalf("unavailable notices = %d, want 2; output=%q", got, errw.String())
	}
}

func TestREPLEffortCommandRejectsInvalidProfileForCurrentModel(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RegistryModel = "anthropic:claude-opus-4-8"
	app.Reasoning = llm.ReasoningConfig{Profile: "medium"}
	app.Agent.SetReasoning(app.Reasoning)
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {
			Reasoning: &llm.ReasoningInfo{
				Supported: true,
			},
		},
	})

	in := strings.NewReader("/effort ultra\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), `invalid profile "ultra"`) {
		t.Fatalf("invalid profile should be reported, errw=%q", errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.Profile != "medium" {
		t.Fatalf("request profile = %q, want unchanged medium", fp.Requests[0].Reasoning.Profile)
	}
}

func TestREPLAgentCommandLists(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AgentName = "plan"
	app.AvailableAgents = []AgentSummary{
		{Name: "auto", Description: "Default agent"},
		{Name: "independent", Description: "Work independently"},
		{Name: "plan", Description: "Plan changes", Model: "anthropic:claude-opus-4-8", Delegatable: true},
		{Name: "style", Description: "Style review", Model: "openai:gpt-5.5", Delegatable: true},
	}

	in := strings.NewReader("/agent\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	for _, name := range []string{"auto", "independent", "plan"} {
		if !strings.Contains(got, name) {
			t.Errorf("/agent should list %q, errw=%q", name, got)
		}
	}
	if !strings.Contains(got, "plan (current)") {
		t.Errorf("/agent should mark the current agent, errw=%q", got)
	}
	if !strings.Contains(got, "Plan changes") {
		t.Errorf("/agent should include descriptions, errw=%q", got)
	}
	for _, want := range []string{
		"current agent: plan [anthropic:claude-opus-4-8]",
		"auto            [inherit current] Default agent",
		"independent     [inherit current] Work independently",
		"plan (current)  [anthropic:claude-opus-4-8] [delegatable] Plan changes",
		"style           [openai:gpt-5.5] [delegatable] Style review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/agent output missing %q, errw=%q", want, got)
		}
	}
	for _, notWant := range []string{
		"auto            [inherit current] [delegatable]",
		"independent     [inherit current] [delegatable]",
	} {
		if strings.Contains(got, notWant) {
			t.Errorf("/agent output should not mark row delegatable with %q, errw=%q", notWant, got)
		}
	}
}

func TestREPLAgentCommandAlignsAndWrapsDescriptions(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AgentName = "auto"
	app.SummaryWidth = func() int { return 54 }
	app.AvailableAgents = []AgentSummary{
		{Name: "auto", Description: "one two three four five six"},
		{Name: "review", Description: "short", Model: "openai:gpt-5.5"},
	}

	if code := Run(strings.NewReader("/agent\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	for _, want := range []string{
		"  auto (current)  [inherit current] one two three four",
		"                  five six",
		"  review          [openai:gpt-5.5] short",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/agent output missing %q:\n%s", want, got)
		}
	}
}

func TestREPLAgentCommandSwitchesNextTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {
			Price: llm.Price{Input: 0.43, Output: 0.87, CacheRead: 0.004},
		},
	})
	app.Reasoning = llm.ReasoningConfig{Profile: "max"}
	app.Agent.SetReasoning(app.Reasoning)
	catalog, _ := tools.CatalogWithOptions(tools.Options{})
	planTools, err := catalog.Subset([]string{"read_file", "grep"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		if name != "plan" {
			t.Fatalf("switch agent = %q, want plan", name)
		}
		return AgentSelection{
			Name:          "plan",
			Tools:         planTools,
			System:        "PLAN AGENT PROMPT",
			Provider:      "anthropic",
			Model:         "claude-opus-4-8",
			RegistryModel: "anthropic:claude-opus-4-8",
			Runtime:       fp,
			BaseURL:       "proxy",
		}, nil
	}

	in := strings.NewReader("/agent plan\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if app.AgentName != "plan" {
		t.Errorf("app.AgentName = %q, want plan", app.AgentName)
	}
	if app.System != "PLAN AGENT PROMPT" {
		t.Errorf("app.System should update so saves capture it, got %q", app.System)
	}
	if !strings.Contains(errw.String(), "agent switched: plan") ||
		!strings.Contains(errw.String(), "model: claude-opus-4-8  reasoning: profile=max  pricing: in=$0.43/M out=$0.87/M cache-read=$0/M") {
		t.Errorf("switch should be acknowledged, errw=%q", errw.String())
	}
	// The post-switch turn must advertise only the plan tool set.
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	names := make([]string, len(fp.Requests[0].Tools))
	for i, s := range fp.Requests[0].Tools {
		names[i] = s.Name
	}
	if len(names) != 2 || names[0] != "read_file" || names[1] != "grep" {
		t.Errorf("post-switch request should advertise [read_file grep], got %v", names)
	}
}

func TestREPLAgentSwitchRotatesProxySessionID(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("first")}, Stop: llm.StopEndTurn, ResponseID: "resp_1"},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second")}, Stop: llm.StopEndTurn, ResponseID: "resp_2"},
	)
	app := newTestApp(t, &out, &errw, fp)
	app.Agent.SetResponsesStateful(true)
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		if name != "plan" {
			t.Fatalf("switch agent = %q, want plan", name)
		}
		return AgentSelection{
			Name:              "plan",
			Tools:             tools.Default(),
			System:            "PLAN AGENT PROMPT",
			Provider:          app.Provider,
			Model:             app.Model,
			RegistryModel:     app.RegistryModel,
			Runtime:           fp,
			ResponsesStateful: true,
		}, nil
	}

	if code := Run(strings.NewReader("first\n/agent plan\nsecond\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("provider requests = %d, want 2", fp.RequestCount())
	}
	firstSession := fp.Requests[0].ProxySessionID
	secondSession := fp.Requests[1].ProxySessionID
	if firstSession == "" || secondSession == "" || firstSession == secondSession {
		t.Fatalf("proxy session ids = %q then %q, want rotation on agent switch", firstSession, secondSession)
	}
	if firstCache, secondCache := fp.Requests[0].CacheAffinityID, fp.Requests[1].CacheAffinityID; firstCache == "" || firstCache != secondCache {
		t.Fatalf("cache affinity ids = %q then %q, want preservation on agent switch", firstCache, secondCache)
	}
	if fp.Requests[1].PreviousResponseID != "" {
		t.Fatalf("post-switch request previous_response_id = %q, want fresh context", fp.Requests[1].PreviousResponseID)
	}
}

func TestREPLModeAliasSwitchesAgent(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "sys", Provider: "anthropic", Model: "claude-opus-4-8", Runtime: fp}, nil
	}

	if code := Run(strings.NewReader("/mode plan\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if app.AgentName != "plan" {
		t.Fatalf("/mode alias did not switch agent, got %q", app.AgentName)
	}
}

func TestREPLPlanAliasDirectlySwitchesAgent(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "sys", Provider: "anthropic", Model: "claude-opus-4-8", Runtime: fp}, nil
	}

	if code := Run(strings.NewReader("/plan\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if app.AgentName != "plan" {
		t.Fatalf("/plan alias did not switch to plan agent, got %q", app.AgentName)
	}
}

func TestREPLAutoAliasDirectlySwitchesAgent(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "sys", Provider: "anthropic", Model: "claude-opus-4-8", Runtime: fp}, nil
	}

	if code := Run(strings.NewReader("/auto\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if app.AgentName != "auto" {
		t.Fatalf("/auto alias did not switch to auto agent, got %q", app.AgentName)
	}
}

func TestREPLAgentCommandWarnsWhenProviderOrModelChanges(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "sys", Provider: "openai", Model: "gpt-5.5", Runtime: fp}, nil
	}

	if code := Run(strings.NewReader("/agent review\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(errw.String(), "may start without prompt cache") {
		t.Fatalf("expected cache warning, errw=%q", errw.String())
	}
}

func TestREPLAgentCommandUnknownReportsError(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AgentName = "auto"
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{}, errors.New(`unknown agent "bogus" (available: auto, plan)`)
	}

	in := strings.NewReader("/agent bogus\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(errw.String(), "agent switch failed") {
		t.Errorf("unknown agent should report failure, errw=%q", errw.String())
	}
	if app.AgentName != "auto" {
		t.Errorf("failed switch should not change the agent, got %q", app.AgentName)
	}
}

type shiftTabDelayRequest struct {
	delay time.Duration
	fire  chan time.Time
}

func nextShiftTabDelay(t *testing.T, requests <-chan shiftTabDelayRequest) shiftTabDelayRequest {
	t.Helper()
	select {
	case req := <-requests:
		if req.delay != shiftTabPrewarmDebounce {
			t.Fatalf("Shift-Tab delay = %v, want %v", req.delay, shiftTabPrewarmDebounce)
		}
		return req
	case <-time.After(time.Second):
		t.Fatal("Shift-Tab prewarm was not scheduled")
	}
	return shiftTabDelayRequest{}
}

func TestNextAgentNameCyclesWrapsAndRecovers(t *testing.T) {
	app := &App{
		AvailableAgents: []AgentSummary{{Name: "auto"}, {Name: "explore"}, {Name: "plan"}},
		SwitchAgent:     func(string) (AgentSelection, error) { return AgentSelection{}, nil },
	}
	for _, tt := range []struct {
		current string
		want    string
		ok      bool
	}{
		{current: "auto", want: "explore", ok: true},
		{current: "explore", want: "plan", ok: true},
		{current: "plan", want: "auto", ok: true},
		{current: "missing", want: "auto", ok: true},
	} {
		app.AgentName = tt.current
		got, ok := app.nextAgentName()
		if got != tt.want || ok != tt.ok {
			t.Errorf("nextAgentName(%q) = %q, %v; want %q, %v", tt.current, got, ok, tt.want, tt.ok)
		}
	}

	app.AvailableAgents = nil
	if got, ok := app.nextAgentName(); ok || got != "" {
		t.Fatalf("zero agents = %q, %v; want no-op", got, ok)
	}
	app.AvailableAgents = []AgentSummary{{Name: "auto"}}
	app.AgentName = "auto"
	if got, ok := app.nextAgentName(); ok || got != "" {
		t.Fatalf("one current agent = %q, %v; want no-op", got, ok)
	}
	app.AgentName = "missing"
	if got, ok := app.nextAgentName(); !ok || got != "auto" {
		t.Fatalf("one recovery agent = %q, %v; want auto", got, ok)
	}
	app.SwitchAgent = nil
	if got, ok := app.nextAgentName(); ok || got != "" {
		t.Fatalf("missing switch callback = %q, %v; want no-op", got, ok)
	}
}

func TestREPLShiftTabCyclesAgentsAndDebouncesFinalPrewarm(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AvailableAgents = []AgentSummary{{Name: "auto"}, {Name: "explore"}, {Name: "plan"}}

	catalog, _ := tools.CatalogWithOptions(tools.Options{})
	toolSets := make(map[string]*tools.Registry)
	for name, names := range map[string][]string{
		"auto":    {"read_file"},
		"explore": {"grep"},
		"plan":    {"read_file", "grep"},
	} {
		registry, err := catalog.Subset(names)
		if err != nil {
			t.Fatalf("%s tool subset: %v", name, err)
		}
		toolSets[name] = registry
	}
	var switched []string
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		switched = append(switched, name)
		return AgentSelection{
			Name:          name,
			Tools:         toolSets[name],
			System:        strings.ToUpper(name) + " SYSTEM",
			Provider:      "anthropic",
			Model:         "claude-opus-4-8", // deliberately identical for every agent
			RegistryModel: "anthropic:claude-opus-4-8",
			BaseURL:       app.BaseURL,
			Runtime:       fp,
		}, nil
	}

	delayRequests := make(chan shiftTabDelayRequest, 4)
	app.shiftTabPrewarmAfter = func(delay time.Duration) <-chan time.Time {
		fire := make(chan time.Time, 1)
		delayRequests <- shiftTabDelayRequest{delay: delay, fire: fire}
		return fire
	}
	type warmSnapshot struct {
		agent   string
		request llm.Request
	}
	warmed := make(chan warmSnapshot, 4)
	app.Prewarm = func() {
		warmed <- warmSnapshot{agent: app.AgentName, request: app.Agent.ContextRequest()}
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	writePipe(t, pw, "draft\x1b[Z")
	first := nextShiftTabDelay(t, delayRequests)
	if app.AgentName != "explore" || app.System != "EXPLORE SYSTEM" {
		t.Fatalf("first cycle selected agent=%q system=%q, want explore", app.AgentName, app.System)
	}
	if req := app.Agent.ContextRequest(); len(req.Tools) != 1 || req.Tools[0].Name != "grep" {
		t.Fatalf("first cycle tools = %+v, want grep", req.Tools)
	}

	writePipe(t, pw, "\x1b[9;2u")
	_ = nextShiftTabDelay(t, delayRequests)
	if app.AgentName != "plan" {
		t.Fatalf("second cycle selected %q, want plan", app.AgentName)
	}
	first.fire <- time.Now() // stale after the second schedule; must do nothing

	writePipe(t, pw, "\x1b[Z")
	latest := nextShiftTabDelay(t, delayRequests)
	if app.AgentName != "auto" {
		t.Fatalf("wrapped cycle selected %q, want auto", app.AgentName)
	}
	select {
	case got := <-warmed:
		t.Fatalf("rapid cycling prewarmed intermediate agent: %+v", got)
	default:
	}

	latest.fire <- time.Now()
	var got warmSnapshot
	select {
	case got = <-warmed:
	case <-time.After(time.Second):
		t.Fatal("latest Shift-Tab timer did not prewarm")
	}
	if got.agent != "auto" || got.request.Model != "claude-opus-4-8" || got.request.System != "AUTO SYSTEM" {
		t.Fatalf("final prewarm snapshot = agent=%q model=%q system=%q", got.agent, got.request.Model, got.request.System)
	}
	if len(got.request.Tools) != 1 || got.request.Tools[0].Name != "read_file" {
		t.Fatalf("final prewarm tools = %+v, want read_file", got.request.Tools)
	}

	writePipe(t, pw, "\x7f\x7f\x7f\x7f\x7f/exit\r")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Join(switched, ",") != "explore,plan,auto" {
		t.Fatalf("switch order = %v, want lexical cycle with wrap", switched)
	}
	for _, name := range switched {
		if !strings.Contains(errw.String(), "[agent switched: "+name+"]") {
			t.Errorf("missing switch notice for %s: %q", name, errw.String())
		}
		if !strings.Contains(errw.String(), "["+name+"] > draft") {
			t.Errorf("next prompt did not retain draft for agent %s: %q", name, errw.String())
		}
	}
	if got := strings.Count(errw.String(), "model: claude-opus-4-8"); got < 3 {
		t.Fatalf("provider/model lines = %d, want one per switch: %q", got, errw.String())
	}
	select {
	case extra := <-warmed:
		t.Fatalf("more than one prewarm: %+v", extra)
	default:
	}
}

func TestREPLShiftTabFailedSwitchRetainsDraft(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.AvailableAgents = []AgentSummary{{Name: "auto"}, {Name: "plan"}}
	app.SwitchAgent = func(string) (AgentSelection, error) { return AgentSelection{}, errors.New("switch broke") }
	prewarms := 0
	app.Prewarm = func() { prewarms++ }

	if code := run(strings.NewReader("draft\x1b[Z\r"), app, nil, true); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if app.AgentName != "auto" || prewarms != 0 {
		t.Fatalf("failed cycle left agent=%q prewarms=%d, want auto and zero", app.AgentName, prewarms)
	}
	if !strings.Contains(errw.String(), "[agent switch failed: switch broke]") {
		t.Fatalf("missing switch failure notice: %q", errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want retained draft submitted once", fp.RequestCount())
	}
	messages := fp.Requests[0].Messages
	if len(messages) == 0 || len(messages[len(messages)-1].Content) == 0 || messages[len(messages)-1].Content[0].Text != "draft" {
		t.Fatalf("submitted messages = %+v, want retained draft", messages)
	}
}

func TestREPLShiftTabPendingPrewarmCancelledByExplicitAgentSwitch(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AvailableAgents = []AgentSummary{{Name: "auto"}, {Name: "explore"}, {Name: "plan"}}
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: strings.ToUpper(name), Provider: app.Provider, Model: app.Model, RegistryModel: app.RegistryModel, Runtime: fp}, nil
	}
	delayRequests := make(chan shiftTabDelayRequest, 2)
	app.shiftTabPrewarmAfter = func(delay time.Duration) <-chan time.Time {
		fire := make(chan time.Time, 1)
		delayRequests <- shiftTabDelayRequest{delay: delay, fire: fire}
		return fire
	}
	warmed := make(chan string, 2)
	app.Prewarm = func() { warmed <- app.AgentName }

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	writePipe(t, pw, "\x1b[Z")
	pending := nextShiftTabDelay(t, delayRequests)
	// The explicit /agent switch supersedes the stale Shift-Tab timer and rides
	// the same debounce: a new timer is scheduled for the settled selection.
	writePipe(t, pw, "/agent plan\r")
	latest := nextShiftTabDelay(t, delayRequests)
	pending.fire <- time.Now() // stale; must do nothing
	select {
	case name := <-warmed:
		t.Fatalf("stale Shift-Tab timer prewarmed %q", name)
	case <-time.After(50 * time.Millisecond):
	}
	latest.fire <- time.Now()
	select {
	case name := <-warmed:
		if name != "plan" {
			t.Fatalf("explicit switch prewarmed %q, want plan", name)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit /agent debounce did not prewarm")
	}
	writePipe(t, pw, "/exit\r")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	select {
	case name := <-warmed:
		t.Fatalf("duplicate prewarm for %q", name)
	default:
	}
}

// TestPrewarmDefersWhilePromptActive pins the mid-prompt gate: warm-up
// requests made while a prompt runs (agent/model switch, /compact) coalesce
// into one warm-up fired at prompt completion, since a running prompt
// refreshes the cache prefix every turn.
func TestPrewarmDefersWhilePromptActive(t *testing.T) {
	app := &App{}
	warms := 0
	app.Prewarm = func() { warms++ }

	app.promptActive = true
	app.prewarm()
	app.prewarm() // coalesces; no flag explosion
	if warms != 0 {
		t.Fatalf("prewarm fired %d times mid-prompt, want deferred", warms)
	}
	if !app.pendingPrewarm {
		t.Fatal("mid-prompt prewarm did not set the pending flag")
	}

	app.promptActive = false
	app.releasePendingPrewarm()
	if warms != 1 || app.pendingPrewarm {
		t.Fatalf("release fired %d warm-ups (pending=%v), want exactly one", warms, app.pendingPrewarm)
	}
	app.releasePendingPrewarm()
	if warms != 1 {
		t.Fatalf("second release fired again (%d), want once", warms)
	}

	// An exit with a pending warm-up drops it: nothing fires without release.
	app.promptActive = true
	app.prewarm()
	app.promptActive = false
	if warms != 1 {
		t.Fatalf("dropped pending prewarm fired (%d), want still one", warms)
	}
}

// TestREPLRapidAgentSwitchesPrewarmOnlySettledSelection drives three rapid
// /agent switches and asserts exactly one debounced prewarm, for the final
// agent — the /agent path rides the same debounce as Shift-Tab cycling.
func TestREPLRapidAgentSwitchesPrewarmOnlySettledSelection(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AvailableAgents = []AgentSummary{{Name: "auto"}, {Name: "explore"}, {Name: "plan"}}
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: strings.ToUpper(name), Provider: app.Provider, Model: app.Model, RegistryModel: app.RegistryModel, Runtime: fp}, nil
	}
	delayRequests := make(chan shiftTabDelayRequest, 4)
	app.shiftTabPrewarmAfter = func(delay time.Duration) <-chan time.Time {
		fire := make(chan time.Time, 1)
		delayRequests <- shiftTabDelayRequest{delay: delay, fire: fire}
		return fire
	}
	warmed := make(chan string, 2)
	app.Prewarm = func() { warmed <- app.AgentName }

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	writePipe(t, pw, "/agent explore\r")
	first := nextShiftTabDelay(t, delayRequests)
	writePipe(t, pw, "/agent plan\r")
	second := nextShiftTabDelay(t, delayRequests)
	writePipe(t, pw, "/agent auto\r")
	latest := nextShiftTabDelay(t, delayRequests)
	first.fire <- time.Now()  // stale
	second.fire <- time.Now() // stale
	latest.fire <- time.Now()
	select {
	case name := <-warmed:
		if name != "auto" {
			t.Fatalf("prewarmed %q, want the settled agent auto", name)
		}
	case <-time.After(time.Second):
		t.Fatal("settled selection was not prewarmed")
	}
	writePipe(t, pw, "/exit\r")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	select {
	case name := <-warmed:
		t.Fatalf("rapid /agent switches prewarmed more than once: %q", name)
	default:
	}
}

// TestREPLCompactPrewarmsImmediatelyWhenIdle keeps /compact's immediate
// warm-up: compaction rewrites the prefix and no prompt is running to refresh
// it, so the debounce would only add cold-cache latency.
func TestREPLCompactPrewarmsImmediatelyWhenIdle(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")}, Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	// Seed enough whole turns that /compact has something older to summarize.
	var seed []llm.Message
	for i := 0; i < 10; i++ {
		label := string(rune('a' + i))
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " a"}}},
		)
	}
	app.Agent.SetTranscript(seed)
	warms := make(chan struct{}, 1)
	app.Prewarm = func() { warms <- struct{}{} }
	select {
	case <-warms:
		t.Fatal("prewarm fired before /compact")
	default:
	}

	if code := run(strings.NewReader("/compact\r/exit\r"), app, nil, true); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	select {
	case <-warms:
	default:
		t.Fatal("/compact while idle did not prewarm immediately")
	}
}

func TestREPLShiftTabPendingPrewarmCancelledByRealPrompt(t *testing.T) {
	var out, errw lockedBuffer
	started := make(chan struct{})
	release := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
		Block: func(context.Context) {
			close(started)
			<-release
		},
	})
	app := newTestApp(t, &out, &errw, fp)
	app.AvailableAgents = []AgentSummary{{Name: "auto"}, {Name: "plan"}}
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "PLAN", Provider: app.Provider, Model: app.Model, RegistryModel: app.RegistryModel, Runtime: fp}, nil
	}
	delayRequests := make(chan shiftTabDelayRequest, 1)
	app.shiftTabPrewarmAfter = func(delay time.Duration) <-chan time.Time {
		fire := make(chan time.Time, 1)
		delayRequests <- shiftTabDelayRequest{delay: delay, fire: fire}
		return fire
	}
	warmed := make(chan struct{}, 1)
	app.Prewarm = func() { warmed <- struct{}{} }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()
	writePipe(t, pw, "hello\x1b[Z")
	pending := nextShiftTabDelay(t, delayRequests)
	writePipe(t, pw, "\r")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("real prompt did not start")
	}
	pending.fire <- time.Now()
	close(release)
	_ = pw.Close()
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	select {
	case <-warmed:
		t.Fatal("submitted real prompt did not cancel pending Shift-Tab prewarm")
	default:
	}
}

func TestREPLSaveToPath(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	alt := filepath.Join(t.TempDir(), "alt.json")

	in := strings.NewReader("hello\n/save " + alt + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(alt); err != nil {
		t.Fatalf("/save <file> should write to the given path: %v", err)
	}
}

func TestREPLContextDumpsCurrentRequest(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("hello\n/context\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("/context should not invoke the model, got %d requests", fp.RequestCount())
	}
	got := errw.String()
	for _, want := range []string{
		`"model": "claude-opus-4-8"`,
		`"system": "you are a test"`,
		`"messages": [`,
		`"text": "hello"`,
		`"tools": [`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/context output missing %s:\n%s", want, got)
		}
	}
}

func TestREPLContextSavesToFile(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	path := filepath.Join(t.TempDir(), "nested", "context.json")

	in := strings.NewReader("hello\n/context " + path + "\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("/context <file> should write the given path: %v", err)
	}
	var req llm.Request
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("context file should be JSON llm.Request: %v\n%s", err, data)
	}
	if req.Model != "claude-opus-4-8" || req.System != "you are a test" {
		t.Fatalf("context request = model %q system %q", req.Model, req.System)
	}
	if len(req.Messages) != 2 || req.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("context messages = %+v", req.Messages)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("/context <file> should not invoke the model, got %d requests", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "[context saved "+path+"]") {
		t.Errorf("/context <file> should acknowledge save, errw=%q", errw.String())
	}
}

func TestREPLContextIncludesTodoRequestContext(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	store.Replace([]todo.Item{
		{Content: "explore", Status: todo.StatusCompleted},
		{Content: "implement", Status: todo.StatusInProgress},
	})
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store

	if code := Run(strings.NewReader("/context\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("/context should not invoke the model, got %d requests", fp.RequestCount())
	}
	got := errw.String()
	for _, want := range []string{
		`[todo]\n1/2 complete`,
		`[~] implement`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/context output missing %q:\n%s", want, got)
		}
	}
}

func TestREPLInjectsRestoredTodoContextOnce(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Stop: llm.StopEndTurn},
		llmtest.Step{Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	store.Restore([]todo.Item{{Content: "explore", Status: todo.StatusInProgress}})
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store

	if code := Run(strings.NewReader("first\nsecond\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	// Restored unresolved work is injected once on the first request.
	if got := strings.Join(fp.Requests[0].RequestContext, "\n"); !strings.Contains(got, "[~] explore") {
		t.Fatalf("first request context = %q, want the todo reminder", got)
	}
	// The reminder is one-shot; the second request relies on transcript state.
	if got := strings.Join(fp.Requests[1].RequestContext, "\n"); strings.Contains(got, "[todo]") {
		t.Fatalf("second request context = %q, want the recovery reminder consumed", got)
	}
}

func TestREPLPrintsTodoStatusAfterUpdateTodosBeforeUsageAndPrompt(t *testing.T) {
	var out, setupErrw bytes.Buffer
	status := "Todos (1/2 done):\n  [x] explore\n  [~] test"
	errw := newSignalBuffer("\x00")
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolStep("update_todos", `{"todos":[{"content":"explore","status":"completed"},{"content":"test","status":"in_progress"}]}`, "call_todo")},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
		},
	)
	app := newTestApp(t, &out, &setupErrw, fp)
	app.Errw = errw
	app.Renderer = NewRenderer(&out, errw, RenderOptions{Model: "claude-opus-4-8", ToolStream: true})
	store := todo.NewStore()
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()

	writePipe(t, pw, "work\n")
	waitFor(t, func() bool {
		got := errw.String()
		return strings.Count(got, "[auto] > ") >= 2 && strings.Contains(got, "[turn:")
	}, "turn completion line and next prompt")
	writePipe(t, pw, "/exit\n")
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("REPL did not exit after /exit")
	}

	got := errw.String()
	statusIndex := strings.Index(got, status)
	if statusIndex < 0 {
		t.Fatalf("todo status was not printed after update_todos:\n%s", got)
	}
	toolResultIndex := strings.Index(got, "[update_todos]")
	if toolResultIndex < 0 {
		t.Fatalf("update_todos tool result was not rendered:\n%s", got)
	}
	nextModelIndex := strings.Index(got, "[turn: 2 waiting]")
	if nextModelIndex < 0 {
		t.Fatalf("second turn was not rendered:\n%s", got)
	}
	if !(toolResultIndex < statusIndex && statusIndex < nextModelIndex) {
		t.Fatalf("todo status should print immediately after update_todos and before the next turn:\n%s", got)
	}

	promptStatusIndex := strings.LastIndex(got, status)
	if promptStatusIndex == statusIndex {
		t.Fatalf("todo status should also be printed before the next prompt:\n%s", got)
	}
	afterPromptStatus := got[promptStatusIndex+len(status):]
	usageIndex := strings.Index(afterPromptStatus, "[prompt:")
	if usageIndex < 0 {
		t.Fatalf("usage line should follow the prompt todo status:\n%s", got)
	}
	promptIndex := strings.Index(afterPromptStatus, "[auto] > ")
	if promptIndex < 0 {
		t.Fatalf("usage line should be followed by the next REPL prompt:\n%s", got)
	}
	if usageIndex > promptIndex {
		t.Fatalf("usage line should be the last status line before the next REPL prompt:\n%s", got)
	}
	if strings.Contains(afterPromptStatus[usageIndex:promptIndex], "Todos (") {
		t.Fatalf("todo status should not be printed between the usage line and next prompt:\n%s", got)
	}
}

func TestREPLSkipsTodoPromptStatusWhenToolUnavailable(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	store.Replace([]todo.Item{
		{Content: "hidden", Status: todo.StatusInProgress},
	})
	app.Todos = store

	if code := Run(strings.NewReader("/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := errw.String(); strings.Contains(got, "Todos (") || strings.Contains(got, "hidden") {
		t.Fatalf("todo status should not print when the visible agent lacks update_todos:\n%s", got)
	}
}

func TestREPLPrintsLifecycleAwarePlanStatusBeforeUsageAndPrompt(t *testing.T) {
	for _, tt := range []struct {
		name        string
		seed        *plan.Plan
		record      bool
		wantState   plan.DisplayState
		wantLabel   string
		wantUpdated bool
	}{
		{name: "newly recorded", record: true, wantState: plan.DisplayRecorded, wantLabel: "Plan recorded:"},
		{
			name:        "updated",
			seed:        &plan.Plan{Title: "Existing", Path: "/tmp/0001-existing.plan.md"},
			record:      true,
			wantState:   plan.DisplayUpdated,
			wantLabel:   "Plan updated:",
			wantUpdated: true,
		},
		{
			name:      "unchanged current",
			seed:      &plan.Plan{Title: "Resumed", Path: "/tmp/0001-resumed.plan.md"},
			wantState: plan.DisplayCurrent,
			wantLabel: "Plan:",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, setupErrw bytes.Buffer
			errw := newSignalBuffer("\x00")
			var fp *llmtest.FakeProvider
			if tt.record {
				fp = llmtest.New("fake",
					llmtest.Step{
						Events: []llm.StreamEvent{toolStep("record_plan", `{"title":"Add widget","plan":"Step one."}`, "call_plan")},
						Stop:   llm.StopToolUse,
					},
					llmtest.Step{
						Events: []llm.StreamEvent{textDelta("done")},
						Stop:   llm.StopEndTurn,
					},
				)
			} else {
				fp = llmtest.New("fake", llmtest.Step{
					Events: []llm.StreamEvent{textDelta("done")},
					Stop:   llm.StopEndTurn,
				})
			}
			app := newTestApp(t, &out, &setupErrw, fp)
			app.Errw = errw
			app.Renderer = NewRenderer(&out, errw, RenderOptions{Model: "claude-opus-4-8", ToolStream: true})
			store := plan.NewStore()
			if tt.seed != nil {
				store.Add(*tt.seed)
			}
			reg := tools.Default()
			reg.Register(plan.NewTool(store, func() string { return app.SessionPath }))
			app.Agent.SetTools(reg)
			app.Plans = store

			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			codeCh := make(chan int, 1)
			go func() { codeCh <- Run(pr, app, nil) }()

			writePipe(t, pw, "work\n")
			waitFor(t, func() bool {
				got := errw.String()
				return strings.Count(got, "[auto] > ") >= 2 && strings.Contains(got, "[turn:")
			}, "turn completion line and next prompt")
			writePipe(t, pw, "/exit\n")
			select {
			case code := <-codeCh:
				if code != 0 {
					t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
				}
			case <-time.After(time.Second):
				t.Fatal("REPL did not exit after /exit")
			}

			got := errw.String()
			status := plan.RenderLatest(store.Snapshot(), tt.wantState)
			if status == "" || !strings.HasPrefix(status, tt.wantLabel) {
				t.Fatalf("plan status = %q, want label %q", status, tt.wantLabel)
			}
			if count := strings.Count(got, status); count != 2 {
				t.Fatalf("plan status should print at exactly two display boundaries, got %d:\n%s", count, got)
			}
			statusIndex := strings.Index(got, status)
			if statusIndex < 0 {
				t.Fatalf("plan status was not printed:\n%s", got)
			}
			if tt.record {
				toolResultIndex := strings.Index(got, "[record_plan]")
				if toolResultIndex < 0 {
					t.Fatalf("record_plan tool result was not rendered:\n%s", got)
				}
				nextModelIndex := strings.Index(got, "[turn: 2 waiting]")
				if nextModelIndex < 0 {
					t.Fatalf("second turn was not rendered:\n%s", got)
				}
				if !(toolResultIndex < statusIndex && statusIndex < nextModelIndex) {
					t.Fatalf("plan status should print immediately after record_plan and before the next turn:\n%s", got)
				}
			}
			if tt.wantUpdated && (tt.seed == nil || strings.Contains(status, tt.seed.Path)) {
				t.Fatalf("updated status should name the newly appended plan, got %q", status)
			}

			promptStatusIndex := strings.LastIndex(got, status)
			if promptStatusIndex == statusIndex {
				t.Fatalf("plan status should also be printed before the next prompt:\n%s", got)
			}
			afterPromptStatus := got[promptStatusIndex+len(status):]
			usageIndex := strings.Index(afterPromptStatus, "[prompt:")
			if usageIndex < 0 {
				t.Fatalf("usage line should follow the prompt plan status:\n%s", got)
			}
			promptIndex := strings.Index(afterPromptStatus, "[auto] > ")
			if promptIndex < 0 {
				t.Fatalf("usage line should be followed by the next REPL prompt:\n%s", got)
			}
			if usageIndex > promptIndex {
				t.Fatalf("usage line should be the last status line before the next REPL prompt:\n%s", got)
			}
			if strings.Contains(afterPromptStatus[usageIndex:promptIndex], status) {
				t.Fatalf("plan status should not be printed between the usage line and next prompt:\n%s", got)
			}
		})
	}
}

func TestREPLSinkPlanDisplayState(t *testing.T) {
	for _, tt := range []struct {
		name     string
		initial  int
		appended int
		want     plan.DisplayState
	}{
		{name: "unchanged existing plan", initial: 1, want: plan.DisplayCurrent},
		{name: "first plan", appended: 1, want: plan.DisplayRecorded},
		{name: "new version of existing plan", initial: 1, appended: 1, want: plan.DisplayUpdated},
		{name: "multiple plans in one prompt", appended: 2, want: plan.DisplayUpdated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := plan.NewStore()
			for range tt.initial {
				store.Add(plan.Plan{})
			}
			sink := newREPLSink(nil, &App{Plans: store}, 1)
			for range tt.appended {
				store.Add(plan.Plan{})
			}
			if got := sink.planDisplayState(); got != tt.want {
				t.Fatalf("planDisplayState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestREPLSkipsPlanPromptStatusWhenToolUnavailable(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "hidden", Path: "/tmp/hidden.plan.md"})
	app.Plans = store

	if code := Run(strings.NewReader("/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	for _, hidden := range []string{"Plan:", "Plan recorded:", "Plan updated:", "/tmp/hidden.plan.md"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("plan status should not print when the visible agent lacks record_plan:\n%s", got)
		}
	}
}

func TestREPLBackgroundCommandListsNoJobs(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Background = background.NewManager(background.Options{})

	if code := Run(strings.NewReader("/background\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("/background should not invoke the model, got %d requests", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "[background: no jobs]") {
		t.Fatalf("/background output = %q", errw.String())
	}
}

func TestREPLEOFSavesAndExitsZero(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	// No trailing /exit: stream ends (EOF) after one prompt.
	in := strings.NewReader("hello\n")
	if code := Run(in, app, nil); code != 0 {
		t.Errorf("^D/EOF should exit 0, got %d", code)
	}
	if _, err := os.Stat(app.SessionPath); err != nil {
		t.Errorf("EOF should save the session: %v", err)
	}
}

func TestREPLProviderErrorReported(t *testing.T) {
	var out, errw bytes.Buffer
	// A plain (non-API, non-cancel) error is retryable, so it must persist
	// across the whole per-turn budget (1 + 2 retries) to surface to errw.
	fail := llmtest.Step{Err: errContext("boom")}
	fp := llmtest.New("fake", fail, fail, fail)
	app := newTestApp(t, &out, &errw, fp)

	in := strings.NewReader("hello\n/exit\n")
	// A turn error in the REPL is reported but does not end the session.
	if code := Run(in, app, nil); code != 0 {
		t.Errorf("REPL should survive a turn error and exit 0 via /exit, got %d", code)
	}
	if !strings.Contains(strings.ToLower(errw.String()), "error") {
		t.Errorf("turn error should be reported to errw, got %q", errw.String())
	}
}

func TestREPLEscapeEscapeCancelsActivePrompt(t *testing.T) {
	var out, errw bytes.Buffer
	inPrompt := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 5, OutputTokens: 1},
		Block: func(ctx context.Context) {
			close(inPrompt)
			<-ctx.Done()
		},
	})
	app := newTestApp(t, &out, &errw, fp)
	exitRequested := make(chan struct{}, 1)
	app.Interrupt = agent.NewInterruptWatcher(make(chan os.Signal), app.clock(), func() {
		exitRequested <- struct{}{}
	})

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()

	writePipe(t, pw, "first\n")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "\x1b\x1b/exit\n")
	_ = pw.Close()

	code := waitRun(t, codeCh)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "[cancelled]") {
		t.Fatalf("Esc-Esc should render cancellation, errw=%q", errw.String())
	}
	select {
	case <-exitRequested:
		t.Fatal("Esc-Esc must cancel the prompt without requesting process exit")
	default:
	}
}

func TestREPLDoubleEscapeStillCancelsAfterSubmittedSteer(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Block: func(ctx context.Context) {
			close(inPrompt)
			<-ctx.Done()
		},
	})
	app := liveTestApp(t, &out, &errw, fp)
	app.Steer = func(agent.SteerInput) bool { return true }
	app.Interrupt = agent.NewInterruptWatcher(make(chan os.Signal), app.clock(), func() {})

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "redirect\r")
	writePipe(t, pw, "\x1b\x1b")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[cancelled]") }, "prompt cancelled after steer")
	_ = pw.Close()
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
}

func TestREPLSecondControlCForceExitsProviderIgnoringCancellation(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	releaseProvider := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Block: func(context.Context) {
			close(inPrompt)
			<-releaseProvider
		},
	})
	app := liveTestApp(t, &out, &errw, fp)
	sig := make(chan os.Signal, 2)
	exit := make(chan struct{}, 1)
	app.Interrupt = agent.NewInterruptWatcher(sig, app.clock(), func() { exit <- struct{}{} })
	stop := app.Interrupt.Start()
	defer stop()

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, exit, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	sig <- os.Interrupt
	waitFor(t, func() bool { return strings.Contains(errw.String(), "cancelling") }, "graceful cancellation status")
	sig <- os.Interrupt
	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("force exit code = %d, want %d", code, ExitInterrupt)
	}

	_ = pw.Close()
	close(releaseProvider)
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(app.SessionPath, "state.json"))
		return err == nil
	}, "released prompt goroutine cleanup")
}

func TestREPLReaderConsumesBufferedEscapeSequenceTail(t *testing.T) {
	rr := newREPLReader(strings.NewReader("\x1b[Asecond\n"), io.Discard, false, "")
	rr.setEscapeLineEnd(true)

	input, ok, err := rr.read(replReadRequest{})
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok || input.text != "second" {
		t.Fatalf("input = %+v ok=%v, want second prompt", input, ok)
	}
}

func TestREPLReaderShiftTabPreservesDraftClassificationWithoutHistory(t *testing.T) {
	rr := newREPLReader(strings.NewReader("\x1b[Z\x1b[9;2u\r"), io.Discard, true, "")
	req := replReadRequest{
		prompt:             "> ",
		promptEditor:       true,
		replPrompt:         true,
		prefill:            "/exit",
		prefillModelPrompt: true,
		prefillPasted:      true,
	}

	for i := 0; i < 2; i++ {
		input, ok, err := rr.read(req)
		if err != nil || !ok {
			t.Fatalf("cycle read %d ok=%v err=%v", i, ok, err)
		}
		if !input.cycleAgent || input.text != "/exit" || !input.modelPrompt || !input.pasted {
			t.Fatalf("cycle read %d = %+v, want classified /exit draft", i, input)
		}
		if len(rr.editor.history) != 0 {
			t.Fatalf("cycle read %d committed history: %v", i, rr.editor.history)
		}
		req.prefill = input.text
		req.prefillModelPrompt = input.modelPrompt
		req.prefillPasted = input.pasted
	}

	input, ok, err := rr.read(req)
	if err != nil || !ok {
		t.Fatalf("submit read ok=%v err=%v", ok, err)
	}
	if input.cycleAgent || input.text != "/exit" || !input.modelPrompt || !input.pasted {
		t.Fatalf("submitted input = %+v, want classified /exit prompt", input)
	}
}

func TestREPLReaderShiftTabIsInertOutsideIdleMainPrompt(t *testing.T) {
	rr := newREPLReader(strings.NewReader("draft\x1b[Z\r"), io.Discard, true, "")
	input, ok, err := rr.read(replReadRequest{prompt: "choice: ", promptEditor: true})
	if err != nil || !ok {
		t.Fatalf("auxiliary read ok=%v err=%v", ok, err)
	}
	if input.cycleAgent || input.text != "draft" {
		t.Fatalf("auxiliary Shift-Tab = %+v, want inert draft", input)
	}

	during := newDuringPromptTestReader("draft\x1b[Z")
	for range len("draft") {
		if _, _, err := pumpDuringPromptKey(during); err != nil {
			t.Fatalf("type during prompt: %v", err)
		}
	}
	input, done, err := pumpDuringPromptKey(during)
	if err != nil {
		t.Fatalf("during-prompt Shift-Tab: %v", err)
	}
	if done || input.cycleAgent || string(during.promptState.buf) != "draft" {
		t.Fatalf("during-prompt Shift-Tab = %+v done=%v buf=%q, want inert draft", input, done, string(during.promptState.buf))
	}
}

func TestREPLReaderAuxiliaryPromptSkipsHistory(t *testing.T) {
	// The main idle prompt (replPrompt: true) records submitted lines to history,
	// but auxiliary menu prompts (e.g. /model selections, save confirmations) must
	// not, so transient answers like "44" or "y" never pollute up-arrow recall.
	rr := newREPLReader(strings.NewReader("real-command\r44\ry\r"), io.Discard, true, "")
	var recorded []string
	rr.editor.onNewHistory = func(entry string) { recorded = append(recorded, entry) }

	if _, ok, err := rr.read(replReadRequest{prompt: "> ", promptEditor: true, replPrompt: true}); err != nil || !ok {
		t.Fatalf("idle read ok=%v err=%v", ok, err)
	}
	if _, ok, err := rr.read(replReadRequest{prompt: "model? ", promptEditor: true}); err != nil || !ok {
		t.Fatalf("menu read 1 ok=%v err=%v", ok, err)
	}
	if _, ok, err := rr.read(replReadRequest{prompt: "save? ", promptEditor: true}); err != nil || !ok {
		t.Fatalf("menu read 2 ok=%v err=%v", ok, err)
	}

	if len(recorded) != 1 || recorded[0] != "real-command" {
		t.Fatalf("history recorded = %v, want only [real-command]", recorded)
	}
	// The editor's recallable history must also exclude the menu answers.
	if got := rr.editor.history; len(got) != 1 || got[0] != "real-command" {
		t.Fatalf("editor history = %v, want [real-command]", got)
	}
}

func TestREPLReaderMarksSplitEscapeSequenceTail(t *testing.T) {
	rr := newREPLReader(strings.NewReader("[A\x1b"), io.Discard, false, "")
	rr.setEscapeLineEnd(true)

	input, ok, err := rr.read(replReadRequest{})
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok || !input.escapeTail || input.text != "[A" {
		t.Fatalf("input = %+v ok=%v, want split escape tail", input, ok)
	}
}

func TestREPLScrollEscapeDuringActiveTurnDoesNotQueuePrompt(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inPrompt)
				<-releaseTurn
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("second answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 6, OutputTokens: 2},
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()

	writePipe(t, pw, "first\n")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "\x1b[A")
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Count(errw.String(), "> ") >= 2 }, "prompt after first turn")
	if fp.RequestCount() != 1 {
		t.Fatalf("scroll escape should not queue a prompt, got %d requests", fp.RequestCount())
	}

	writePipe(t, pw, "second\n/exit\n")
	_ = pw.Close()
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("provider requests = %d, want 2", fp.RequestCount())
	}
	var prompts []string
	for _, msg := range app.Agent.Transcript() {
		if msg.Role == llm.RoleUser && len(msg.Content) == 1 && msg.Content[0].Kind == llm.BlockText {
			prompts = append(prompts, msg.Content[0].Text)
		}
	}
	if strings.Join(prompts, "|") != "first|second" {
		t.Fatalf("user prompts = %q, want first|second", strings.Join(prompts, "|"))
	}
}

// Non-interactive (piped) input keeps the auto-submitting type-ahead drain: a
// script that pipes several lines runs each as a turn. The during-prompt-input
// deposit behavior (never auto-submit, deposit as editable prefill) applies only
// to the interactive prompt-editor path — see
// TestREPLDuringPromptInputDepositedOnCompletionNotAutoSubmitted.
func TestREPLTypeaheadDuringActiveTurnRunsAfterTurn(t *testing.T) {
	var out, errw bytes.Buffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inPrompt)
				<-releaseTurn
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("second answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 6, OutputTokens: 2},
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()

	writePipe(t, pw, "first\n")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "second\n/exit\n")
	_ = pw.Close()
	close(releaseTurn)

	code := waitRun(t, codeCh)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("typeahead prompt should run after the blocked turn, got %d requests", fp.RequestCount())
	}
	var prompts []string
	for _, msg := range app.Agent.Transcript() {
		if msg.Role == llm.RoleUser && len(msg.Content) == 1 && msg.Content[0].Kind == llm.BlockText {
			prompts = append(prompts, msg.Content[0].Text)
		}
	}
	if strings.Join(prompts, "|") != "first|second" {
		t.Fatalf("user prompts = %q, want first|second", strings.Join(prompts, "|"))
	}
}

func TestREPLTypeaheadDuringActiveTurnQueuesWhenSteerConfigured(t *testing.T) {
	var out, errw bytes.Buffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inPrompt)
				<-releaseTurn
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("second answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 6, OutputTokens: 2},
		},
	)
	app := newTestApp(t, &out, &errw, fp)
	ag := agent.New(fp, tools.Default(), agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	app.Steer = func(input agent.SteerInput) bool { return ag.SteerContent(input) }
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(pr, app, nil) }()

	writePipe(t, pw, "first\n")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "second\n/exit\n")
	_ = pw.Close()
	close(releaseTurn)

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("non-interactive typeahead should queue despite configured steering, got %d requests", fp.RequestCount())
	}
	if got := transcriptPrompts(app); got != "first|second" {
		t.Fatalf("prompts = %q, want first|second", got)
	}
}

func TestREPLPromptEditorPrintsPromptAfterTurnWithPendingActiveRead(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inPrompt)
				<-releaseTurn
			},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("second answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 6, OutputTokens: 2},
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	close(releaseTurn)
	waitFor(t, func() bool {
		s := errw.String()
		return strings.Contains(s, "[turn:") && strings.Count(s, "> ") >= 2
	}, "prompt after first turn")

	// The post-turn prompt is the raw line editor, so Enter is \r (the canonical
	// \n fallback is gone now that during-prompt input is captured raw).
	writePipe(t, pw, "second\r")
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request")
	waitFor(t, func() bool { return strings.Count(errw.String(), "> ") >= 3 }, "prompt after second turn")
	writePipe(t, pw, "/exit\r")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("provider requests = %d, want 2", fp.RequestCount())
	}
}

// TestREPLInputReadErrorWarned covers the lint fix: a non-EOF read error from
// stdin must be surfaced (warned to errw) rather than silently treated as a clean
// end of input. The scanner stops on the error; Run reports it and exits 0
// (there is nothing more to read, but the user should know why).
func TestREPLInputReadErrorWarned(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	in := &erroringReader{data: []byte("hello\n"), err: errContext("disk gone")}
	code := Run(in, app, nil)
	if code != ExitOK {
		t.Fatalf("read error should still exit 0, got %d; errw=%q", code, errw.String())
	}
	got := errw.String()
	if !strings.Contains(strings.ToLower(got), "input") || !strings.Contains(got, "disk gone") {
		t.Errorf("input read error should be warned to errw, got %q", got)
	}
	// The session is still saved on this exit path.
	if _, err := os.Stat(app.SessionPath); err != nil {
		t.Errorf("read-error exit should save the session: %v", err)
	}
}

// unsavablePath returns a SessionPath whose parent is a regular file, so
// session.Save's os.MkdirAll fails — a deterministic stand-in for the ordinary
// disk-full / read-only / permission faults that make an automatic save fail.
func unsavablePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// blocker is a file, so MkdirAll(blocker/sub) cannot create the parent.
	return filepath.Join(blocker, "sub", "session")
}

// TestREPLAutoSaveFailureWarned is the regression test for after-every-turn
// auto-save errors being silently swallowed (design §11/§12: a visible failure
// beats silent data loss). A failed save must warn to errw, not vanish.
func TestREPLAutoSaveFailureWarned(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = unsavablePath(t)

	// One prompt then /exit; the after-turn auto-save fails first.
	in := strings.NewReader("hello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("REPL should still exit 0, got %d; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "save failed") {
		t.Errorf("failed auto-save must warn to errw, got %q", errw.String())
	}
}

// TestREPLCompactSaveFailureWarned covers the /compact save path, the sixth
// automatic-save site: after a forced compaction the collapsed transcript must
// be saved, and a failed save must warn rather than leave a stale file silently.
func TestREPLCompactSaveFailureWarned(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}},
	)
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = unsavablePath(t)

	var seed []llm.Message
	for i := 0; i < 10; i++ {
		label := string(rune('a' + i))
		seed = append(seed,
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " q"}}},
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: label + " a"}}},
		)
	}
	app.Agent.SetTranscript(seed)

	// /compact compacts and saves; the save fails and must warn. The failure does
	// not abort the REPL.
	in := strings.NewReader("/compact\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("REPL should exit 0 on EOF, got %d; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "save failed") {
		t.Errorf("failed /compact save must warn to errw, got %q", errw.String())
	}
}

// TestREPLExitSaveFailureWarned covers the /exit save path: if the final save
// fails, the user must be told the on-disk session is stale rather than exiting
// as if it were saved.
func TestREPLExitSaveFailureWarned(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = unsavablePath(t)

	in := strings.NewReader("/exit\n") // no turn; only the /exit save runs
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("/exit should exit 0, got %d; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "save failed") {
		t.Errorf("failed /exit save must warn to errw, got %q", errw.String())
	}
}

// TestREPLEOFSaveFailureWarned covers the EOF (^D) exit-save path.
func TestREPLEOFSaveFailureWarned(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = unsavablePath(t)

	in := strings.NewReader("") // immediate EOF, no prompt
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("EOF should exit 0, got %d; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "save failed") {
		t.Errorf("failed EOF save must warn to errw, got %q", errw.String())
	}
}

// erroringReader returns its data once, then a non-EOF error (not io.EOF), so the
// scanner stops with a real read error rather than clean end-of-input.
type erroringReader struct {
	data []byte
	off  int
	err  error
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

type signalBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	needle string
	seen   chan struct{}
}

func newSignalBuffer(needle string) *signalBuffer {
	return &signalBuffer{needle: needle, seen: make(chan struct{})}
}

func (b *signalBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	seen := strings.Contains(b.buf.String(), b.needle)
	b.mu.Unlock()
	if seen {
		select {
		case <-b.seen:
		default:
			close(b.seen)
		}
	}
	return n, err
}

func (b *signalBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writePipe(t *testing.T, w *io.PipeWriter, s string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte(s))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipe write %q: %v", s, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("pipe write %q timed out", s)
	}
}

func waitRun(t *testing.T, codeCh <-chan int) int {
	t.Helper()
	select {
	case code := <-codeCh:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	return 0
}

func waitFor(t *testing.T, ok func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

// errContext is a sentinel non-cancellation error for provider-error tests.
type errContextT string

func (e errContextT) Error() string { return string(e) }
func errContext(s string) error     { return errContextT(s) }

// The terminal reset must go to /dev/tty (and only when one exists), never to
// Errw: a piped or redirected stderr must receive no escape sequences. This
// regression-tests the removal of the \033c (RIS) write before the first
// prompt, which also cleared the user's screen and scrollback.
func TestREPLWritesNoEscapeSequencesToErrw(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)

	code := Run(strings.NewReader("/exit\n"), app, nil)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if s := errw.String(); strings.ContainsRune(s, '\x1b') {
		t.Errorf("errw contains escape bytes: %q", s)
	}
}

// mcpRefreshTool is a minimal Tool used to prove the RefreshMCP hook's returned
// registry was applied to the agent before the turn.
type mcpRefreshTool struct{ name string }

func (m mcpRefreshTool) Name() string                  { return m.name }
func (m mcpRefreshTool) Description() string           { return "refreshed tool" }
func (m mcpRefreshTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (m mcpRefreshTool) ReadOnly(json.RawMessage) bool { return false }
func (m mcpRefreshTool) Run(context.Context, json.RawMessage) (string, error) {
	return "ok", nil
}

// TestREPLRefreshMCPAppliedBeforeTurn asserts the REPL consults RefreshMCP at
// the idle-prompt boundary, swaps in the returned tools (visible in the next
// request's advertised tool list), and renders the notice.
func TestREPLRefreshMCPAppliedBeforeTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.AgentName = "auto"

	refreshed := &tools.Registry{}
	refreshed.Register(mcpRefreshTool{name: "mcp__test__fresh"})

	var gotAgent string
	calls := 0
	app.RefreshMCP = func(ctx context.Context, agent string) (*tools.Registry, string) {
		calls++
		gotAgent = agent
		return refreshed, "[mcp: tool list updated; 1 tools]"
	}

	if code := Run(strings.NewReader("hello\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit = %d, want 0; errw=%q", code, errw.String())
	}
	if calls != 1 {
		t.Errorf("RefreshMCP called %d times, want 1", calls)
	}
	if gotAgent != "auto" {
		t.Errorf("RefreshMCP agent = %q, want auto", gotAgent)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("want 1 request, got %d", fp.RequestCount())
	}
	var advertised bool
	for _, ts := range fp.Requests[0].Tools {
		if ts.Name == "mcp__test__fresh" {
			advertised = true
		}
	}
	if !advertised {
		t.Errorf("refreshed tool not advertised to the model: %+v", fp.Requests[0].Tools)
	}
	if !strings.Contains(errw.String(), "tool list updated") {
		t.Errorf("refresh notice not rendered: %q", errw.String())
	}
}

// TestREPLRefreshMCPNoChangeKeepsTools confirms a nil-registry hook result is a
// no-op: the turn still runs and no notice is rendered.
func TestREPLRefreshMCPNoChangeKeepsTools(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	called := false
	app.RefreshMCP = func(context.Context, string) (*tools.Registry, string) {
		called = true
		return nil, ""
	}
	if code := Run(strings.NewReader("hi\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !called {
		t.Errorf("RefreshMCP should still be consulted")
	}
	if strings.Contains(errw.String(), "tool list updated") {
		t.Errorf("no notice expected on no-change, got %q", errw.String())
	}
}

func TestAddUsageBucketsPerModel(t *testing.T) {
	app := &App{Provider: "anthropic", Model: "opus", RegistryModel: "opus"}
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.1, CostKnown: true}, Compactions: 2})
	app.Provider, app.Model, app.RegistryModel = "openai", "gpt", "gpt"
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 30, OutputTokens: 5, CostUSD: 0.2, CostKnown: true}})

	if len(app.usageByModel) != 2 {
		t.Fatalf("want 2 model buckets, got %d: %+v", len(app.usageByModel), app.usageByModel)
	}
	if app.usageByModel["opus"].InputTokens != 100 {
		t.Errorf("opus bucket = %+v", app.usageByModel["opus"])
	}
	if app.usageByModel["gpt"].OutputTokens != 5 {
		t.Errorf("gpt bucket = %+v", app.usageByModel["gpt"])
	}
	if app.usage.InputTokens != 130 || app.usage.OutputTokens != 15 {
		t.Errorf("aggregate = %+v, want 130/15", app.usage)
	}
	report := app.usageReport("session")
	for _, want := range []string{"opus", "gpt", "total"} {
		if !strings.Contains(report, want) {
			t.Errorf("multi-model report missing %q: %s", want, report)
		}
	}
	if !strings.HasSuffix(report, "\n  total · 2 compactions · $0.3000]") {
		t.Errorf("multi-model report should place compactions before total cost: %s", report)
	}
}

func TestQueuedMaintenanceUsageIsAccountedWithoutCreatingTurn(t *testing.T) {
	app := &App{
		Provider:      "anthropic",
		Model:         "opus",
		RegistryModel: "opus",
		SessionPath:   t.TempDir(),
	}
	app.QueueMaintenanceUsageForModel("opus", agent.MaintenanceUsage{
		Purpose: "prewarm",
		Usage:   llm.Usage{InputTokens: 12, OutputTokens: 1},
	})
	app.RegistryModel = "gpt"

	if got := app.usageSummary(); !strings.Contains(got, "12 input") || !strings.Contains(got, "1 output") {
		t.Fatalf("usage summary did not drain queued prewarm usage: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"maintenance_usage"`) || !strings.Contains(log, `"purpose":"prewarm"`) {
		t.Fatalf("prewarm was not recorded as maintenance: %s", log)
	}
	if strings.Contains(log, `"type":"turn_complete"`) || strings.Contains(log, `"type":"turn_attempt_usage"`) {
		t.Fatalf("prewarm must not create a conversational turn: %s", log)
	}
	if got := app.usageByModel["opus"]; got.InputTokens != 12 || got.OutputTokens != 1 {
		t.Fatalf("prewarm usage moved off its initiating model: %+v", app.usageByModel)
	}
	if got := app.usageByModel["gpt"]; got != (session.UsageTotals{}) {
		t.Fatalf("prewarm usage was charged to the later active model: %+v", app.usageByModel)
	}
}

func TestUsageReportSingleModelIncludesCompactions(t *testing.T) {
	app := &App{Provider: "anthropic", Model: "opus", RegistryModel: "opus"}
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 30, OutputTokens: 10, ReasoningTokens: 4, CacheWriteTokens: 20, CostUSD: 0.5, CostKnown: true}, Compactions: 2})
	got := app.usageReport("session summary")
	want := "[session summary: 100 input / 30 cached input / 10 output / 4 reasoning / 20 cache write · 2 compactions · $0.5000]"
	if got != want {
		t.Errorf("single-model report = %q, want %q", got, want)
	}
}

func uiUserMsg(s string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: s}}}
}

func uiAsstMsg(s string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: s}}}
}

func TestTreeCommandBranchesBeforeSelectedPromptAndPrefillsIt(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	seed := []llm.Message{
		{Role: llm.RoleUser, Time: at, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
		{Role: llm.RoleAssistant, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first answer"}}},
		{Role: llm.RoleUser, Time: at.Add(time.Minute), Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
		{Role: llm.RoleAssistant, Time: at.Add(time.Minute), Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second answer"}}},
	}
	app.Agent.SetTranscript(seed)
	if err := app.ensureSessionTree(); err != nil {
		t.Fatalf("ensureSessionTree: %v", err)
	}
	selected := app.SessionTree.Entries[2]
	read := func(string) (string, error) { return "n", nil }
	result := app.command("/tree "+selected.ID, read)
	if !result.prefillSet || result.prefill != "second" {
		t.Fatalf("prefill = %q/%v, want second/true", result.prefill, result.prefillSet)
	}
	text := transcriptTextForUI(app.Agent.Transcript())
	if strings.Contains(text, "second answer") || !strings.Contains(text, "working directory was not reverted") {
		t.Fatalf("branched transcript = %q", text)
	}
	loaded, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Tree.Entries) != 5 || loaded.ActiveLeaf != app.SessionTree.ActiveLeaf {
		t.Fatalf("saved tree entries/leaf = %d/%s", len(loaded.Tree.Entries), loaded.ActiveLeaf)
	}
}

func TestTreeCommandDefaultSummaryUsesBranchPurpose(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("left-branch summary")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 12, OutputTokens: 3},
	})
	app := newTestApp(t, &out, &errw, fp)
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	seed := []llm.Message{
		{Role: llm.RoleUser, Time: at, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
		{Role: llm.RoleAssistant, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first answer"}}},
		{Role: llm.RoleUser, Time: at.Add(time.Minute), Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
		{Role: llm.RoleAssistant, Time: at.Add(time.Minute), Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second answer"}}},
	}
	app.Agent.SetTranscript(seed)
	if err := app.ensureSessionTree(); err != nil {
		t.Fatalf("ensureSessionTree: %v", err)
	}
	selected := app.SessionTree.Entries[2]
	app.command("/tree "+selected.ID, func(string) (string, error) { return "d", nil })
	if fp.RequestCount() != 1 {
		t.Fatalf("summary requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Purpose != llm.RequestPurposeBranchSummary {
		t.Fatalf("summary purpose = %q", fp.Requests[0].Purpose)
	}
	if got := transcriptTextForUI(app.Agent.Transcript()); !strings.Contains(got, "left-branch summary") {
		t.Fatalf("branch summary missing from context: %q", got)
	}
	if app.usage.InputTokens != 12 || app.usage.OutputTokens != 3 {
		t.Fatalf("branch summary usage not accounted: %+v", app.usage)
	}
}

func TestForkCommandCreatesChildSessionWithFreshUsage(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	app.Now = func() time.Time { return at.Add(2 * time.Minute) }
	seed := []llm.Message{
		{Role: llm.RoleUser, Time: at, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
		{Role: llm.RoleAssistant, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first answer"}}},
		{Role: llm.RoleUser, Time: at.Add(time.Minute), Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
		{Role: llm.RoleAssistant, Time: at.Add(time.Minute), Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second answer"}}},
	}
	app.Agent.SetTranscript(seed)
	app.SetUsage(session.UsageTotals{Usage: llm.Usage{InputTokens: 100}})
	if err := app.ensureSessionTree(); err != nil {
		t.Fatalf("ensureSessionTree: %v", err)
	}
	parentID := app.SessionTree.Header.ID
	originalPath := app.SessionPath
	selected := app.SessionTree.Entries[2]
	result := app.command("/fork "+selected.ID, func(string) (string, error) { return "n", nil })
	if !result.prefillSet || result.prefill != "second" {
		t.Fatalf("fork prefill = %q/%v", result.prefill, result.prefillSet)
	}
	if app.SessionPath == originalPath || app.usage.InputTokens != 0 {
		t.Fatalf("fork path/usage = %q/%+v", app.SessionPath, app.usage)
	}
	loaded, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}
	if loaded.ParentSession != parentID || loaded.Usage.InputTokens != 0 {
		t.Fatalf("child lineage/usage = %q/%+v", loaded.ParentSession, loaded.Usage)
	}
	if got := transcriptTextForUI(loaded.Messages); strings.Contains(got, "second answer") || !strings.Contains(got, "first answer") {
		t.Fatalf("forked context = %q", got)
	}
}

func transcriptTextForUI(messages []llm.Message) string {
	var parts []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockText {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func TestHandoffToImplementationReseedsContext(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = filepath.Join(t.TempDir(), "session")
	app.Plans = plan.NewStore()
	app.Todos = todo.NewStore()
	app.Todos.Replace([]todo.Item{{Content: "planning step", Status: "in_progress"}})
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it"), uiAsstMsg("here is the design")})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl system"}, nil
	}

	app.handoffToImplementation(plan.HandoffRequest{
		Agent:    "auto",
		PlanPath: "/sess/plans/0001.plan.md",
		Brief:    "tests run with go test",
		Message:  "preserve the public API",
	})

	msgs := app.Agent.Transcript()
	if len(msgs) != 1 {
		t.Fatalf("want a single seeded message, got %d", len(msgs))
	}
	if err := llm.ValidateTranscript(msgs); err != nil {
		t.Fatalf("seeded transcript invalid: %v", err)
	}
	seed := msgs[0].Content[0].Text
	for _, want := range []string{"Implementation handoff", "/sess/plans/0001.plan.md", "tests run with go test", "Additional input from the user", "preserve the public API"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed missing %q: %q", want, seed)
		}
	}
	if app.AgentName != "auto" {
		t.Errorf("agent not switched: %q", app.AgentName)
	}
	if len(app.Todos.Snapshot()) != 0 {
		t.Error("planning todos should be cleared on handoff")
	}
	if entries, _ := os.ReadDir(filepath.Join(app.SessionPath, "compactions")); len(entries) == 0 {
		t.Error("planning transcript not archived under compactions/")
	}
}

func TestHandoffToImplementationAbortsWhenModelSwitchFails(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it"), uiAsstMsg("here is the design")})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl system"}, nil
	}
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		return ModelSelection{}, errors.New("bad model")
	}

	app.handoffToImplementation(plan.HandoffRequest{
		Agent:    "auto",
		Model:    "missing-model",
		PlanPath: "/sess/plans/0001.plan.md",
		Brief:    "tests run with go test",
	})

	msgs := app.Agent.Transcript()
	if len(msgs) != 2 || msgs[0].Content[0].Text != "design it" || msgs[1].Content[0].Text != "here is the design" {
		t.Fatalf("failed model switch should keep planning transcript, got %+v", msgs)
	}
	if !strings.Contains(errw.String(), "model switch failed") {
		t.Fatalf("stderr missing model switch failure:\n%s", errw.String())
	}
	if _, err := os.Stat(filepath.Join(app.SessionPath, "compactions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed model switch should not archive or reseed, stat err=%v", err)
	}
}

func TestHandoffToImplementationAbortsWhenArchiveFails(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	if err := os.WriteFile(app.SessionPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("make bad session path: %v", err)
	}
	app.Todos = todo.NewStore()
	app.Todos.Replace([]todo.Item{{Content: "planning step", Status: "in_progress"}})
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it"), uiAsstMsg("here is the design")})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl system"}, nil
	}

	app.handoffToImplementation(plan.HandoffRequest{
		Agent:    "auto",
		PlanPath: "/sess/plans/0001.plan.md",
		Brief:    "tests run with go test",
	})

	msgs := app.Agent.Transcript()
	if len(msgs) != 2 || msgs[0].Content[0].Text != "design it" || msgs[1].Content[0].Text != "here is the design" {
		t.Fatalf("archive failure should keep planning transcript, got %+v", msgs)
	}
	if len(app.Todos.Snapshot()) != 1 {
		t.Fatal("archive failure should not clear planning todos")
	}
	if !strings.Contains(errw.String(), "archive failed") {
		t.Fatalf("stderr missing archive failure:\n%s", errw.String())
	}
}

func TestParseHandoffCommandOptions(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    handoffCommandOptions
		wantErr bool
	}{
		{name: "empty"},
		{name: "message", input: "keep  the public API", want: handoffCommandOptions{Message: "keep  the public API"}},
		{
			name:  "all fields",
			input: "-a independent -m openai:gpt-5.5 run the migration first",
			want: handoffCommandOptions{
				Agent:   "independent",
				Model:   "openai:gpt-5.5",
				Message: "run the migration first",
			},
		},
		{name: "dash message", input: "-a auto -- - preserve this wording", want: handoffCommandOptions{Agent: "auto", Message: "- preserve this wording"}},
		{name: "unknown option", input: "-x value", wantErr: true},
		{name: "missing agent", input: "-a", wantErr: true},
		{name: "missing model before option", input: "-m -a auto", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHandoffCommandOptions(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseHandoffCommandOptions(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("parseHandoffCommandOptions(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

func TestHandoffCommandRequiresRecordedPlan(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Plans = plan.NewStore()
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default()}, nil
	}
	called := false
	app.handoffCommand("", func(string) (string, error) { called = true; return "y", nil })
	if called {
		t.Error("should not prompt for approval without a recorded plan")
	}
	if !strings.Contains(errw.String(), "no recorded plan") {
		t.Errorf("expected a no-plan message, got %q", errw.String())
	}
}

func TestHandoffCommandCancelledOnNo(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = filepath.Join(t.TempDir(), "session")
	app.Plans = plan.NewStore()
	app.Handoff = plan.NewPending()
	app.Handoff.Request(plan.HandoffRequest{Brief: "ctx", PlanPath: "/p/0001.plan.md"})
	switched := false
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		switched = true
		return AgentSelection{Name: name, Tools: tools.Default()}, nil
	}
	app.handoffCommand("", func(string) (string, error) { return "n", nil })
	if switched {
		t.Error("declining the prompt should not switch agents")
	}
	if !strings.Contains(errw.String(), "handoff cancelled") {
		t.Errorf("expected cancellation message, got %q", errw.String())
	}
}

func TestHandoffCommandAppliesOptionsAndSeedsUserMessage(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = filepath.Join(t.TempDir(), "session")
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it")})
	app.Handoff = plan.NewPending()
	app.Handoff.Request(plan.HandoffRequest{Brief: "planning context", PlanPath: "/p/0001.plan.md"})
	var agentTarget, modelTarget, approval string
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		agentTarget = name
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}
	app.SwitchModel = func(model string, reasoning llm.ReasoningConfig) (ModelSelection, error) {
		modelTarget = model
		return ModelSelection{Model: model, Runtime: fp}, nil
	}

	if !app.handoffCommand("-a independent -m cheap-model preserve the public API", func(prompt string) (string, error) {
		approval = prompt
		return "y", nil
	}) {
		t.Fatal("handoffCommand should approve the handoff")
	}
	if agentTarget != "independent" || modelTarget != "cheap-model" {
		t.Errorf("targets = agent %q model %q, want independent/cheap-model", agentTarget, modelTarget)
	}
	if !strings.Contains(approval, `using model "cheap-model"`) {
		t.Errorf("approval prompt should name the model override: %q", approval)
	}
	seed := transcriptTextForUI(app.Agent.Transcript())
	for _, want := range []string{"planning context", "Additional input from the user", "preserve the public API"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed missing %q: %q", want, seed)
		}
	}
	if got := errw.String(); !strings.Contains(got, "Handoff brief:\nplanning context") {
		t.Errorf("handoff brief was not displayed:\n%s", got)
	}
}

func TestHandoffCommandRendersMarkdownBriefWithoutChangingSource(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Renderer = NewRenderer(&out, &errw, RenderOptions{
		Markdown: true,
		Width:    func() int { return 24 },
	})
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it")})
	app.Handoff = plan.NewPending()
	const brief = "**Verify behavior** with [docs](https://example.com/design).\n\n- alpha beta gamma delta epsilon zeta eta theta"
	app.Handoff.Request(plan.HandoffRequest{Brief: brief, PlanPath: "/p/0001.plan.md"})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}

	if !app.handoffCommand("", func(string) (string, error) { return "y", nil }) {
		t.Fatal("handoffCommand should approve the handoff")
	}

	display := errw.String()
	for _, want := range []string{
		"Handoff brief:\nVerify behavior",
		"docs\n<https://example.com/design>",
		"- alpha beta gamma delta\n  epsilon zeta eta theta",
	} {
		if !strings.Contains(display, want) {
			t.Errorf("rendered handoff brief missing %q:\n%s", want, display)
		}
	}
	if strings.Contains(display, "**Verify behavior**") {
		t.Errorf("display retained raw emphasis delimiters:\n%s", display)
	}
	if strings.Contains(out.String(), "Handoff brief:") {
		t.Errorf("handoff brief should remain on stderr, stdout = %q", out.String())
	}
	if seed := transcriptTextForUI(app.Agent.Transcript()); !strings.Contains(seed, brief) {
		t.Errorf("seed did not retain original Markdown source: %q", seed)
	}
	archived, err := os.ReadFile(filepath.Join(app.SessionPath, "compactions", "0001.summary.md"))
	if err != nil {
		t.Fatalf("read archived handoff brief: %v", err)
	}
	if got := string(archived); got != brief {
		t.Errorf("archived handoff brief = %q, want original %q", got, brief)
	}
}

func TestHandoffCommandDisplaysRawBriefWithoutRenderer(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Renderer = nil
	app.Handoff = plan.NewPending()
	app.Handoff.Request(plan.HandoffRequest{Brief: "**raw brief**", PlanPath: "/p/0001.plan.md"})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default()}, nil
	}

	app.handoffCommand("", func(string) (string, error) { return "n", nil })

	if got := errw.String(); !strings.Contains(got, "Handoff brief:\n**raw brief**\n") {
		t.Errorf("handoff brief should be displayed raw without a renderer:\n%s", got)
	}
}

func TestHandoffCommandDisplaysGeneratedBrief(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("generated planning brief")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Plans = plan.NewStore()
	app.Plans.Add(plan.Plan{Path: "/p/0001.plan.md"})
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it")})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default()}, nil
	}

	app.handoffCommand("manual implementation guidance", func(string) (string, error) { return "n", nil })

	if got := errw.String(); !strings.Contains(got, "Handoff brief:\ngenerated planning brief") {
		t.Errorf("generated handoff brief was not displayed:\n%s", got)
	}
}

func TestHandoffCommandShowsProgressWhileGeneratingBrief(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 7, 22, 11, 30, 0, 0, time.Local)
	var app *App
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("generated planning brief")},
		Stop:   llm.StopEndTurn,
		Block: func(context.Context) {
			now = now.Add(6 * time.Second)
			app.Renderer.tick()
		},
	})
	app = newTestApp(t, &out, &errw, fp)
	app.Renderer = liveRenderer(&out, &errw, func() time.Time { return now })
	defer app.Renderer.StopProgress()
	app.Plans = plan.NewStore()
	app.Plans.Add(plan.Plan{Path: "/p/0001.plan.md"})
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("design it")})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default()}, nil
	}

	app.handoffCommand("", func(string) (string, error) { return "n", nil })

	got := errw.String()
	if !strings.Contains(got, "[handoff: generating brief · 6s]") {
		t.Errorf("generated handoff brief did not show elapsed progress:\n%s", got)
	}
	if !strings.Contains(got, "\r\x1b[2KHandoff brief:\ngenerated planning brief") {
		t.Errorf("handoff progress was not erased before displaying the brief:\n%s", got)
	}
}

func TestHandoffCommandApproveUsesPendingAndDefaultAgent(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = filepath.Join(t.TempDir(), "session")
	app.HandoffAgent = "auto"
	app.Plans = plan.NewStore()
	app.Todos = todo.NewStore()
	app.Agent.SetTranscript([]llm.Message{uiUserMsg("x")})
	app.Handoff = plan.NewPending()
	app.Handoff.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
	var target string
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		target = name
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}
	app.handoffCommand("", func(string) (string, error) { return "y", nil })
	if target != "auto" {
		t.Errorf("handoff target = %q, want auto (default)", target)
	}
	got := app.Agent.Transcript()
	if len(got) != 1 || !strings.Contains(got[0].Content[0].Text, "/p/0001.plan.md") {
		t.Errorf("transcript not reseeded with the plan pointer: %+v", got)
	}
}

func TestREPLHandoffCommandApprovalStartsImplementationTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("implemented")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = filepath.Join(t.TempDir(), "session")
	app.Handoff = plan.NewPending()
	app.Handoff.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}

	if code := Run(strings.NewReader("/handoff\ny\n/exit\n"), app, nil); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ExitOK, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("implementation turn requests = %d, want 1", fp.RequestCount())
	}
	if got := transcriptPrompts(app); !strings.Contains(got, implementationStartPrompt) {
		t.Fatalf("implementation prompt missing from transcript prompts %q", got)
	}
	if !strings.Contains(out.String(), "implemented") {
		t.Fatalf("implementation response missing from stdout: %q", out.String())
	}
}

func TestREPLAutoHandoffApprovalStartsImplementationAfterPlanTurn(t *testing.T) {
	var out, errw lockedBuffer
	pending := plan.NewPending()
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("plan ready")},
			Stop:   llm.StopEndTurn,
			Block: func(ctx context.Context) {
				pending.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
				close(inPrompt)
				<-releaseTurn
			},
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("implemented")}, Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	app.SessionPath = filepath.Join(t.TempDir(), "session")
	app.Handoff = pending
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, false) }()

	writePipe(t, pw, "make a plan\n")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("plan turn did not start")
	}
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Contains(errw.String(), "Hand off to") }, "handoff approval prompt")
	writePipe(t, pw, "y\n/exit\n")

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ExitOK, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("model requests = %d, want plan + implementation", fp.RequestCount())
	}
	if got := transcriptPrompts(app); !strings.Contains(got, implementationStartPrompt) {
		t.Fatalf("implementation prompt missing from transcript prompts %q", got)
	}
	if got := errw.String(); !strings.Contains(got, "Handoff brief:\nenv: go test") {
		t.Fatalf("tool-requested handoff brief was not displayed: %q", got)
	}
}

func TestREPLAutoHandoffDeclineDoesNotStartImplementation(t *testing.T) {
	var out, errw bytes.Buffer
	pending := plan.NewPending()
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("plan ready")},
		Stop:   llm.StopEndTurn,
		Block: func(ctx context.Context) {
			pending.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
		},
	})
	app := newTestApp(t, &out, &errw, fp)
	app.Handoff = pending
	switched := false
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		switched = true
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}

	if code := Run(strings.NewReader("make a plan\nn\n/exit\n"), app, nil); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ExitOK, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("model requests = %d, want only the plan turn", fp.RequestCount())
	}
	if switched {
		t.Fatal("declined handoff should not switch agents")
	}
	if strings.Contains(transcriptPrompts(app), implementationStartPrompt) {
		t.Fatalf("declined handoff should not submit implementation prompt: %q", transcriptPrompts(app))
	}
}

func TestREPLHandoffFailureDoesNotStartImplementationTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("should not run")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Handoff = plan.NewPending()
	app.Handoff.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{}, errors.New("no such agent")
	}

	if code := Run(strings.NewReader("/handoff\ny\n/exit\n"), app, nil); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ExitOK, errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("implementation turn should not start after handoff failure, got %d requests", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "handoff failed") {
		t.Fatalf("stderr missing handoff failure: %q", errw.String())
	}
}

// liveTestApp is newTestApp with a renderer that enables the live wait counter
// and during-prompt input line, so the typed buffer renders to errw in tests.
func liveTestApp(t *testing.T, out, errw testWriter, fp *llmtest.FakeProvider) *App {
	t.Helper()
	app := newTestApp(t, out, errw, fp)
	app.Renderer = NewRenderer(out, errw, RenderOptions{Model: "claude-opus-4-8", ToolStream: true, LiveStatus: true})
	return app
}

// The cancelableReader's pump eagerly drains the fd, so a split escape
// sequence's tail can sit in its Go-side buffers where WaitReadable(fd) can no
// longer see it. buffered() must report exactly those undelivered bytes so the
// escape-readiness probe finds them.
func TestCancelableReaderBufferedTracksPendingBytes(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	cr := newCancelableReader(pr)

	if got := cr.buffered(); got != 0 {
		t.Fatalf("buffered() = %d on a fresh reader, want 0", got)
	}
	writePipe(t, pw, "\x1b[A") // a 3-byte arrow-key escape sequence
	waitFor(t, func() bool { return cr.buffered() == 3 }, "pump buffers the 3 escape bytes")

	// Reading the ESC leaves the [A tail buffered — invisible to a drained fd.
	one := make([]byte, 1)
	if n, err := cr.Read(one); err != nil || n != 1 {
		t.Fatalf("Read = %d, %v; want 1, nil", n, err)
	}
	if got := cr.buffered(); got != 2 {
		t.Fatalf("buffered() = %d after reading 1 of 3, want 2", got)
	}
	rest := make([]byte, 8)
	if n, err := cr.Read(rest); err != nil || n != 2 {
		t.Fatalf("Read remainder = %d, %v; want 2, nil", n, err)
	}
	if got := cr.buffered(); got != 0 {
		t.Fatalf("buffered() = %d after draining, want 0", got)
	}
}

// Readiness must consult the cancelableReader's Go-side buffers, not only the
// raw fd: when the pump has pre-drained an escape sequence's tail off the fd, a
// WaitReadable probe reports not-readable, so escapeSequenceAvailable would
// otherwise mis-read the sequence as a bare Esc.
func TestEscapeSequenceAvailableConsultsCancelableBuffer(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	cr := newCancelableReader(pr)
	e := newPromptLineEditor(cr, io.Discard)
	// Mirror the production wiring: the fd probe is stubbed not-readable (the pump
	// already drained the fd), so readiness must come from the Go-side buffer.
	e.escapeSequenceReady = func(time.Duration) bool { return cr.buffered() > 0 }

	if e.escapeSequenceAvailable() {
		t.Fatal("no buffered bytes and fd not readable -> escape sequence must be unavailable")
	}
	writePipe(t, pw, "[A") // the tail the pump pre-drained off the fd
	waitFor(t, func() bool { return cr.buffered() == 2 }, "pump buffers the escape tail")
	if !e.escapeSequenceAvailable() {
		t.Fatal("a buffered escape tail must make the sequence available despite a drained fd")
	}
}

func transcriptPrompts(app *App) string {
	var prompts []string
	for _, msg := range app.Agent.Transcript() {
		if msg.Role == llm.RoleUser && len(msg.Content) == 1 && msg.Content[0].Kind == llm.BlockText {
			prompts = append(prompts, msg.Content[0].Text)
		}
	}
	return strings.Join(prompts, "|")
}

// dumpTranscript renders a transcript for test failure messages.
func dumpTranscript(msgs []llm.Message) string {
	b, _ := json.MarshalIndent(msgs, "", "  ")
	return string(b)
}

// During-prompt typed input submitted with Enter is queued and automatically runs
// as the next prompt after the current prompt completes.
func TestREPLDuringPromptInputQueuedOnEnter(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		// No events on the blocking step: the model is in its initial wait, so
		// the live counter stays active and the typed buffer renders on it.
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	// Type during the prompt and press Enter. It renders live but the queued prompt
	// waits until the in-flight prompt finishes.
	writePipe(t, pw, "draft\r")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> draft") }, "live input line")
	if fp.RequestCount() != 1 {
		t.Fatalf("queued during-prompt input must wait for the active prompt; got %d requests", fp.RequestCount())
	}
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from queued during-prompt input")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|draft" {
		t.Fatalf("prompts = %q, want first|draft (during-prompt Enter queues next prompt)", got)
	}
}

// blockingTool is a fake tool whose Run blocks on release until the test signals
// it, so a during-prompt steer can be queued while the loop is between tool
// dispatch and the next model request.
type blockingTool struct {
	name    string
	ran     chan struct{}
	release chan struct{}
}

func (t *blockingTool) Name() string                  { return t.name }
func (t *blockingTool) Description() string           { return "blocks" }
func (t *blockingTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t *blockingTool) ReadOnly(json.RawMessage) bool { return false }
func (t *blockingTool) Run(context.Context, json.RawMessage) (string, error) {
	close(t.ran)
	<-t.release
	return "ok", nil
}

// With steering enabled, a prompt submitted during a tool-calling turn is
// injected as the next intermediate model round's input (a RoleUser message the
// second model request sees), rather than queued for the next prompt.
func TestREPLDuringPromptSteerInjectsBeforeNextModelRound(t *testing.T) {
	var out, errw lockedBuffer
	releaseTurn := make(chan struct{})
	toolRan := make(chan struct{})
	tool := &blockingTool{name: "probe", ran: toolRan, release: releaseTurn}
	// Step 1: call the tool (StopToolUse). The tool's Run blocks on
	// releaseTurn until the test steers, gating the loop between tool dispatch
	// and the next model request.
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				Index:     0,
				ToolID:    "call_1",
				ToolName:  "probe",
				ToolInput: json.RawMessage(`{}`),
			}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	// Enable steering: rebuild the agent with Steer on and wire app.Steer.
	reg := tools.Default()
	reg.Register(tool)
	ag := agent.New(fp, reg, agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	steerQueued := make(chan struct{})
	app.Steer = func(input agent.SteerInput) bool {
		accepted := ag.SteerContent(input)
		if accepted {
			close(steerQueued)
		}
		return accepted
	}
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")

	// The model step streams the tool call, then the agent dispatches it. Wait
	// for the tool to run (the loop is now between tool dispatch and the next
	// model request), then type a steer and submit it with Enter.
	select {
	case <-toolRan:
	case <-time.After(time.Second):
		t.Fatal("tool did not run")
	}
	writePipe(t, pw, "redirect now\r")

	// A successful pipe write only means the reader consumed the bytes. Wait for
	// the REPL loop to prepare and queue the steer before letting the tool return,
	// so the loop's next drainSteer is guaranteed to see it (otherwise the steer
	// races the drain and is recovered as a leftover next prompt instead).
	select {
	case <-steerQueued:
	case <-time.After(time.Second):
		t.Fatal("steer was not queued")
	}

	// The steer must land before promptDone: the second model request fires while
	// the turn is still active and carries the steered text.
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from steered input")
	var sawSteer bool
	for _, m := range fp.Requests[1].Messages {
		for _, b := range m.Content {
			if b.Kind == llm.BlockText && b.Text == "redirect now" {
				sawSteer = true
			}
		}
	}
	if !sawSteer {
		t.Errorf("second model request did not include the steered text")
	}
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	// The steered text is a mid-prompt RoleUser message, not a separate next-prompt
	// prompt: it sits between the tool_result and the final assistant message.
	// (With steering off, "redirect now" would instead be the next prompt,
	// appearing after the final assistant message.)
	msgs := app.Agent.Transcript()
	var steerIdx, finalAsstIdx int = -1, -1
	for i, m := range msgs {
		if m.Role == llm.RoleUser && len(m.Content) == 1 && m.Content[0].Kind == llm.BlockText && m.Content[0].Text == "redirect now" {
			steerIdx = i
		}
		if m.Role == llm.RoleAssistant && len(m.Content) == 1 && m.Content[0].Kind == llm.BlockText && strings.Contains(m.Content[0].Text, "second answer") {
			finalAsstIdx = i
		}
	}
	if steerIdx == -1 {
		t.Fatalf("steer message not found in transcript:\n%s", dumpTranscript(msgs))
	}
	if finalAsstIdx == -1 {
		t.Fatalf("final assistant message not found in transcript:\n%s", dumpTranscript(msgs))
	}
	if steerIdx > finalAsstIdx {
		t.Fatalf("steer at index %d should precede final assistant at %d (in-prompt, not next prompt):\n%s", steerIdx, finalAsstIdx, dumpTranscript(msgs))
	}
}

func TestREPLDuringPromptSteerCarriesSkillContext(t *testing.T) {
	var out, errw lockedBuffer
	releaseTurn := make(chan struct{})
	toolRan := make(chan struct{})
	tool := &blockingTool{name: "probe", ran: toolRan, release: releaseTurn}
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				Index:     0,
				ToolID:    "call_1",
				ToolName:  "probe",
				ToolInput: json.RawMessage(`{}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	steer := testSkill(t, "steer", "steer skill", "STEER SKILL BODY")
	app.Skills = map[string]skills.Skill{
		"steer": steer,
	}
	reg := tools.Default()
	reg.Register(tool)
	ag := agent.New(fp, reg, agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	steerQueued := make(chan struct{})
	app.Steer = func(input agent.SteerInput) bool {
		accepted := ag.SteerContent(input)
		if accepted {
			close(steerQueued)
		}
		return accepted
	}
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-toolRan:
	case <-time.After(time.Second):
		t.Fatal("tool did not run")
	}
	writePipe(t, pw, "redirect with $steer\r")
	// A successful pipe write only means the reader consumed the bytes. Wait for
	// the REPL loop to prepare and queue the steer before letting the tool return.
	select {
	case <-steerQueued:
	case <-time.After(time.Second):
		t.Fatal("steer was not queued")
	}
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from steered input")
	_ = pw.Close()
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}

	var sawSkillContext bool
	for _, item := range fp.Requests[1].RequestContext {
		if strings.Contains(item, "STEER SKILL BODY") {
			sawSkillContext = true
		}
	}
	if !sawSkillContext {
		t.Fatalf("second request context = %v, want steer skill context", fp.Requests[1].RequestContext)
	}
}

func TestREPLDuringPromptSteerPromptHookBlockSkipsInjection(t *testing.T) {
	var out, errw lockedBuffer
	releaseTurn := make(chan struct{})
	toolRan := make(chan struct{})
	tool := &blockingTool{name: "probe", ran: toolRan, release: releaseTurn}
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				Index:     0,
				ToolID:    "call_1",
				ToolName:  "probe",
				ToolInput: json.RawMessage(`{}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	cfg, err := hooks.DecodeEventMap([]byte(`{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"if grep -q 'blocked steer'; then printf '{\"decision\":\"block\",\"reason\":\"blocked steer\"}'; else printf '{}'; fi"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	app.Hooks = &hooks.Runner{Config: cfg}
	reg := tools.Default()
	reg.Register(tool)
	ag := agent.New(fp, reg, agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	app.Steer = func(input agent.SteerInput) bool { return ag.SteerContent(input) }
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-toolRan:
	case <-time.After(time.Second):
		t.Fatal("tool did not run")
	}
	writePipe(t, pw, "blocked steer\r")
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request after blocked steer")
	_ = pw.Close()
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "[prompt blocked: blocked steer]") {
		t.Fatalf("stderr missing prompt block notice:\n%s", errw.String())
	}
	for _, req := range fp.Requests {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Kind == llm.BlockText && b.Text == "blocked steer" {
					t.Fatalf("blocked steer was sent to provider in request: %+v", req)
				}
			}
		}
	}
}

// With steering disabled (app.Steer nil), during-prompt Enter keeps the legacy
// behavior: input is queued and runs as the next prompt after the prompt ends.
// This test also exercises the EOF handling: after the queued prompt runs, EOF
// on the input pipe must cause a clean exit (not a deadlock on the orphaned
// readReq channel).
func TestREPLDuringPromptNoSteerQueuesForNextTurn(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp) // app.Steer left nil -> steering off

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	writePipe(t, pw, "draft\r")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> draft") }, "live input line")
	if fp.RequestCount() != 1 {
		t.Fatalf("queued during-prompt input must wait for the active prompt; got %d requests", fp.RequestCount())
	}
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from queued during-prompt input")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|draft" {
		t.Fatalf("prompts = %q, want first|draft (no-steer queues next prompt)", got)
	}
}

// A steer submitted during a prompt whose final turn has no tool round (StopEndTurn)
// is never injected after the final turn; it must be recovered at promptDone and run as the
// next prompt so the input is not silently lost.
func TestREPLDuringPromptSteerRecoveredWhenTurnEndsWithoutToolRound(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	ag := agent.New(fp, tools.Default(), agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	app.Steer = func(input agent.SteerInput) bool { return ag.SteerContent(input) }
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	// Submit a steer while the (tool-less) turn is still running. There is no
	// tool round, so the loop cannot inject it; it must be recovered at promptDone.
	writePipe(t, pw, "redirect\r")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> redirect") }, "live input line")
	if fp.RequestCount() != 1 {
		t.Fatalf("steer must not start a new request after the final turn; got %d requests", fp.RequestCount())
	}
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from recovered steer")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	// The recovered steer ran as the next prompt (not lost).
	if got := transcriptPrompts(app); got != "first|redirect" {
		t.Fatalf("prompts = %q, want first|redirect (recovered steer ran as next prompt)", got)
	}
}

func TestREPLDuringPromptRecoveredLiteralSlashSteerDoesNotRunCommand(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	ag := agent.New(fp, tools.Default(), agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	app.Steer = func(input agent.SteerInput) bool { return ag.SteerContent(input) }
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	writePipe(t, pw, "//exit\r")
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from recovered literal slash steer")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|/exit" {
		t.Fatalf("prompts = %q, want first|/exit (literal slash steer must not run /exit)", got)
	}
}

func TestREPLRejectedSteerAdmissionRunsPreparedInputNext(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{Stop: llm.StopEndTurn, Block: func(context.Context) { close(inPrompt); <-releaseTurn }},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("queued answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	rejected := make(chan struct{})
	app.Steer = func(agent.SteerInput) bool {
		close(rejected)
		return false
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "redirect\r")
	select {
	case <-rejected:
	case <-time.After(time.Second):
		t.Fatal("steer admission was not attempted")
	}
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "request from prepared queued input")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|redirect" {
		t.Fatalf("prompts = %q, want rejected steer retained as next prompt", got)
	}
}

func TestREPLSteerFallbackKeepsSubmissionOrder(t *testing.T) {
	var out, errw lockedBuffer
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{Stop: llm.StopEndTurn, Block: func(context.Context) { close(inPrompt); <-releaseTurn }},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("alpha answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("bravo answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	ag := agent.New(fp, tools.Default(), agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	rejected := make(chan struct{})
	var once sync.Once
	app.Steer = func(input agent.SteerInput) bool {
		select {
		case <-rejected:
			return ag.SteerContent(input)
		default:
			once.Do(func() { close(rejected) })
			return false // alpha: behave like a full steer queue
		}
	}
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }
	delivered := make(chan struct{}, 8)
	app.onInputDelivered = func() { delivered <- struct{}{} }

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	// alpha queues when steering refuses it; bravo submits after, so the
	// steer gate must queue it behind alpha rather than letting it steer and
	// recover first at completion.
	writePipe(t, pw, "alpha\r")
	select {
	case <-rejected:
	case <-time.After(time.Second):
		t.Fatal("steer admission was not attempted")
	}
	drain := len(delivered)
	writePipe(t, pw, "bravo\r")
	writePipe(t, pw, "/boguscommand\r")
	// The command's delivery proves the run loop received bravo first (the
	// input channel holds one result), so bravo is processed while the turn
	// is still blocked.
	waitFor(t, func() bool { return len(delivered) >= drain+2 }, "bravo and command delivered")
	close(releaseTurn)
	waitFor(t, func() bool { return fp.RequestCount() == 3 }, "queued prompts ran")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|alpha|bravo" {
		t.Fatalf("prompts = %q, want submission order first|alpha|bravo", got)
	}
}

// steerDuringPrompt classifies a during-prompt input as model-bound (steer it) or
// not (queue it). This pins the prefix rules without driving a full REPL.
func TestSteerDuringPromptClassification(t *testing.T) {
	cases := []struct {
		name      string
		input     replInput
		wantSteer bool
		wantText  string // text that should reach Steer; ignored when !wantSteer
	}{
		{name: "plain", input: replInput{text: "hi", interactive: true}, wantSteer: true, wantText: "hi"},
		{name: "empty", input: replInput{text: "", interactive: true}, wantSteer: false},
		{name: "pasted", input: replInput{text: "blob", pasted: true}, wantSteer: true, wantText: "blob"},
		{name: "bang-bang strips one", input: replInput{text: "!!cmd", interactive: true}, wantSteer: true, wantText: "!cmd"},
		{name: "slash-slash strips one", input: replInput{text: "//path", interactive: true}, wantSteer: true, wantText: "/path"},
		{name: "shell escape queued", input: replInput{text: "!ls", interactive: true}, wantSteer: false},
		{name: "command queued", input: replInput{text: "/help", interactive: true}, wantSteer: false},
		{name: "edit queued", input: replInput{text: "x", edit: true}, wantSteer: false},
		{name: "interrupt queued", input: replInput{text: "", interrupt: true}, wantSteer: false},
		{name: "escape queued", input: replInput{text: "", escape: true}, wantSteer: false},
		{name: "deposit queued", input: replInput{text: "buf", deposit: true}, wantSteer: false},
		{name: "non-interactive plain queued", input: replInput{text: "hi"}, wantSteer: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var called bool
			app := &App{Steer: func(input agent.SteerInput) bool { called = true; got = input.Text; return true }}
			handled, queued := app.steerDuringPrompt(tc.input)
			if handled != tc.wantSteer {
				t.Fatalf("steerDuringPrompt handled = %v, want %v", handled, tc.wantSteer)
			}
			if queued != nil {
				t.Fatalf("steerDuringPrompt queued = %+v after accepted steering", queued)
			}
			if tc.wantSteer {
				if !called {
					t.Fatalf("Steer not called for model-bound input")
				}
				if got != tc.wantText {
					t.Errorf("steered text = %q, want %q", got, tc.wantText)
				}
			}
		})
	}

	// nil Steer disables steering: everything is queued (returns false).
	app := &App{}
	if handled, queued := app.steerDuringPrompt(replInput{text: "hi", interactive: true}); handled || queued != nil {
		t.Fatalf("steerDuringPrompt = (%v, %+v), want (false, nil) when Steer is nil", handled, queued)
	}

	// A full bounded steer queue must return the already-prepared model input,
	// including literal-prefix normalization, rather than asking the idle loop to
	// prepare and run the raw input again.
	app.Steer = func(agent.SteerInput) bool { return false }
	handled, queued := app.steerDuringPrompt(replInput{text: "//path", interactive: true})
	if !handled || queued == nil || queued.Text != "/path" {
		t.Fatalf("rejected steer = (%v, %+v), want prepared literal /path queued", handled, queued)
	}
}

// Partial during-prompt input that is not submitted with Enter is still deposited
// into the next prompt as editable prefill and requires a manual Enter.
func TestREPLDuringPromptPartialInputDepositedOnCompletion(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	// Type without pressing Enter; the partial buffer is deposited as prefill.
	writePipe(t, pw, "draft")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> draft") }, "live input line")
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[turn:") }, "turn 1 usage line")
	if fp.RequestCount() != 1 {
		t.Fatalf("partial during-prompt input must not auto-submit; got %d requests", fp.RequestCount())
	}

	writePipe(t, pw, "\r")
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from deposited prefill")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|draft" {
		t.Fatalf("prompts = %q, want first|draft (partial text deposited then submitted manually)", got)
	}
}

// On interrupt (double-Esc) the typed-so-far (unsubmitted) buffer is still
// deposited, and the prompt is cancelled (Esc-Esc still interrupts).
func TestREPLDuringPromptInputDepositedOnInterrupt(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inPrompt := make(chan struct{})
	fp := llmtest.New("fake",
		// No events: the model is in its initial wait so the live input line is
		// active when the user types and double-Esc cancels.
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 1},
			Block: func(ctx context.Context) { close(inPrompt); <-ctx.Done() },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("resumed answer")}, Stop: llm.StopEndTurn},
	)
	app := liveTestApp(t, &out, &errw, fp)
	app.Interrupt = agent.NewInterruptWatcher(make(chan os.Signal), app.clock(), func() {})

	pr, pw := io.Pipe()
	defer pr.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(pr, app, nil, true) }()

	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "initial prompt")
	writePipe(t, pw, "first\r")
	select {
	case <-inPrompt:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	// Type without Enter, then double-Esc to interrupt. The typed text survives as a deposit.
	writePipe(t, pw, "wip")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> wip") }, "live input line")
	writePipe(t, pw, "\x1b\x1b")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[cancelled]") }, "prompt cancelled")

	if fp.RequestCount() != 1 {
		t.Fatalf("interrupt must not start a new turn; got %d requests", fp.RequestCount())
	}

	// The interrupted-turn buffer is deposited; submitting it runs it.
	writePipe(t, pw, "\r")
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from deposited prefill after interrupt")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|wip" {
		t.Fatalf("prompts = %q, want first|wip (typed text deposited on interrupt)", got)
	}
}

// The during-prompt capture shares the promptLineEditor's editing grammar via
// handleKey(duringPrompt=true): printable runes, inserts, and deletes act at the
// cursor, and Ctrl-A/E/B/F and the Delete escape sequence move it. Keystrokes
// are fed through the editor's reader so multi-byte escape sequences decode
// exactly as they do in the live REPL (one pump decodes a whole \x1b[3~). This
// asserts both the buffer and the cursor (mirrored on the live onPromptInput
// callback) after each keystroke.
func TestREPLDuringPromptCursorEditing(t *testing.T) {
	// Whole input is preloaded; each pump reads one rune (an escape sequence's
	// tail is consumed by readEscape within the single pump that read the Esc).
	const input = "abc\x02\x02X\x01\x7f\x1b[3~\x05\x7f\x06\nyz\x01\x1b[3~"
	rr := newDuringPromptTestReader(input)
	var emittedBuf string
	var emittedCursor int
	rr.onPromptInput = func(buf string, cursor int) { emittedBuf, emittedCursor = buf, cursor }

	steps := []struct {
		buf    string
		cursor int
	}{
		{"a", 1},      // type a
		{"ab", 2},     // type b
		{"abc", 3},    // type c
		{"abc", 2},    // Ctrl-B -> between b and c
		{"abc", 1},    // Ctrl-B -> between a and b
		{"aXbc", 2},   // insert X mid-buffer
		{"aXbc", 0},   // Ctrl-A home
		{"aXbc", 0},   // backspace no-op at start
		{"Xbc", 0},    // Delete forward: rune AT cursor
		{"Xbc", 3},    // Ctrl-E end
		{"Xb", 2},     // backspace: rune BEFORE cursor
		{"Xb", 2},     // Ctrl-F right no-op at end
		{"Xb\n", 3},   // raw LF inserts a newline (Shift-Enter path)
		{"Xb\ny", 4},  // type y
		{"Xb\nyz", 5}, // type z
		{"Xb\nyz", 0}, // Ctrl-A home
		{"b\nyz", 0},  // delete forward
	}
	for i, want := range steps {
		in, done, err := pumpDuringPromptKey(rr)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if done {
			t.Fatalf("step %d: handleKey returned done=%+v during a prompt", i, in)
		}
		if string(rr.promptState.buf) != want.buf || rr.promptState.cursor != want.cursor {
			t.Fatalf("step %d: buf=%q cursor=%d, want buf=%q cursor=%d", i, string(rr.promptState.buf), rr.promptState.cursor, want.buf, want.cursor)
		}
		if emittedBuf != want.buf || emittedCursor != want.cursor {
			t.Fatalf("step %d: emitted buf=%q cursor=%d, want buf=%q cursor=%d", i, emittedBuf, emittedCursor, want.buf, want.cursor)
		}
	}

	// Deposit returns the full buffer (newline preserved) and resets buffer+cursor.
	dep := rr.depositPromptBuffer()
	if !dep.deposit || dep.text != "b\nyz" {
		t.Fatalf("deposit = %+v, want text %q deposit=true", dep, "b\nyz")
	}
	if len(rr.promptState.buf) != 0 || rr.promptState.cursor != 0 {
		t.Fatalf("after deposit buf=%q cursor=%d, want empty buffer and cursor 0", string(rr.promptState.buf), rr.promptState.cursor)
	}
}

// Wide and multi-byte runes must move the cursor by whole runes, and a stale
// cursor past the (now shorter) buffer is clamped rather than panicking.
func TestREPLDuringPromptCursorWideRunesAndClamp(t *testing.T) {
	rr := newDuringPromptTestReader("aé漢\x02\x7f\x7f")
	// Type a, é, 漢 (3 pumps).
	for i := 0; i < 3; i++ {
		if _, _, err := pumpDuringPromptKey(rr); err != nil {
			t.Fatalf("type %d: %v", i, err)
		}
	}
	if string(rr.promptState.buf) != "aé漢" || rr.promptState.cursor != 3 {
		t.Fatalf("buf=%q cursor=%d, want aé漢 / 3", string(rr.promptState.buf), rr.promptState.cursor)
	}
	if _, _, err := pumpDuringPromptKey(rr); err != nil { // Ctrl-B -> between é and 漢
		t.Fatalf("ctrl-b: %v", err)
	}
	if _, _, err := pumpDuringPromptKey(rr); err != nil { // backspace deletes é
		t.Fatalf("backspace: %v", err)
	}
	if string(rr.promptState.buf) != "a漢" || rr.promptState.cursor != 1 {
		t.Fatalf("buf=%q cursor=%d, want a漢 / 1", string(rr.promptState.buf), rr.promptState.cursor)
	}
	// A stale out-of-range cursor is clamped to the buffer end on the next edit.
	rr.promptState.cursor = 99
	if _, _, err := pumpDuringPromptKey(rr); err != nil { // clamped backspace deletes 漢
		t.Fatalf("backspace: %v", err)
	}
	if string(rr.promptState.buf) != "a" || rr.promptState.cursor != 1 {
		t.Fatalf("buf=%q cursor=%d after clamped backspace, want a / 1", string(rr.promptState.buf), rr.promptState.cursor)
	}
}

// During a prompt the shared editor runs in vi mode too: Esc enters normal mode,
// motions (h/l) and x delete work, and Enter queues the buffer as the next prompt.
// A second Esc is the cancel gesture.
func TestREPLDuringPromptViMode(t *testing.T) {
	// type "abc", Esc -> normal, h h (move left twice), x (delete at cursor),
	// l (right), i (insert), type "Z".
	rr := newDuringPromptTestReader("abc\x1bhhxliZ")
	rr.editor.setEditMode(string(promptEditModeVi))
	rr.beginPromptCapture() // re-seed vi state for the new edit mode

	steps := []struct {
		buf    string
		cursor int
	}{
		{"a", 1},   // a
		{"ab", 2},  // b
		{"abc", 3}, // c (insert mode, cursor at end)
		{"abc", 2}, // Esc -> normal (cursor backs to 2, on 'c')
		{"abc", 1}, // h -> on 'b'
		{"abc", 0}, // h -> on 'a'
		{"bc", 0},  // x deletes 'a' (cursor stays 0, on 'b')
		{"bc", 1},  // l -> on 'c'
		{"bc", 1},  // i -> insert mode at cursor 1
		{"bZc", 2}, // type Z
	}
	for i, want := range steps {
		if _, _, err := pumpDuringPromptKey(rr); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if string(rr.promptState.buf) != want.buf || rr.promptState.cursor != want.cursor {
			t.Fatalf("step %d: buf=%q cursor=%d, want buf=%q cursor=%d", i, string(rr.promptState.buf), rr.promptState.cursor, want.buf, want.cursor)
		}
	}

	// Enter during a prompt submits the current buffer as queued input, even in vi
	// insert mode.
	rr2 := newDuringPromptTestReader("ab\r")
	rr2.editor.setEditMode(string(promptEditModeVi))
	rr2.beginPromptCapture()
	for i := 0; i < 2; i++ {
		if _, _, err := pumpDuringPromptKey(rr2); err != nil {
			t.Fatalf("type %d: %v", i, err)
		}
	}
	in, done, err := pumpDuringPromptKey(rr2) // Enter
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	if !done || in.text != "ab" || !in.interactive {
		t.Fatalf("Enter during a prompt = %+v done=%v, want queued interactive input %q", in, done, "ab")
	}
	if string(rr2.promptState.buf) != "" {
		t.Fatalf("buf=%q, want empty after queued submit", string(rr2.promptState.buf))
	}

	// Shift-Enter/raw LF still inserts a newline for multiline during-prompt prompts.
	rr3 := newDuringPromptTestReader("ab\n")
	rr3.editor.setEditMode(string(promptEditModeVi))
	rr3.beginPromptCapture()
	for i := 0; i < 2; i++ {
		if _, _, err := pumpDuringPromptKey(rr3); err != nil {
			t.Fatalf("type before LF %d: %v", i, err)
		}
	}
	in, done, err = pumpDuringPromptKey(rr3) // LF
	if err != nil {
		t.Fatalf("lf: %v", err)
	}
	if done {
		t.Fatalf("LF during a prompt must insert, not submit; got done=%+v", in)
	}
	if string(rr3.promptState.buf) != "ab\n" {
		t.Fatalf("buf=%q, want ab\\n (LF inserts newline)", string(rr3.promptState.buf))
	}
}

// In vi mode during a prompt, the first Esc both enters normal mode and surfaces
// to the run loop, so the usual double-Esc cancel detector sees two presses (not
// three).
func TestREPLDuringPromptViDoubleEscapeSurfacesBothPresses(t *testing.T) {
	rr := newDuringPromptTestReader("\x1b\x1b")
	rr.editor.setEditMode(string(promptEditModeVi))
	rr.beginPromptCapture()

	first, done, err := pumpDuringPromptKey(rr)
	if err != nil {
		t.Fatalf("first Esc: %v", err)
	}
	if !done || !first.escape {
		t.Fatalf("first Esc = %+v done=%v, want surfaced escape gesture", first, done)
	}
	if rr.promptVi.mode != viModeNormal {
		t.Fatalf("first Esc should still enter vi normal mode, got mode=%v", rr.promptVi.mode)
	}

	var presses escapePresses
	now := time.Unix(100, 0)
	if presses.press(now) {
		t.Fatal("first surfaced Esc should not cancel yet")
	}
	second, done, err := pumpDuringPromptKey(rr)
	if err != nil {
		t.Fatalf("second Esc: %v", err)
	}
	if !done || !second.escape {
		t.Fatalf("second Esc = %+v done=%v, want surfaced escape gesture", second, done)
	}
	if !presses.press(now.Add(10 * time.Millisecond)) {
		t.Fatal("two surfaced Esc gestures within the window should cancel")
	}
}

// During a prompt up/down arrows recall history just like the idle prompt: the
// recalled line replaces the buffer (and is deposited as editable prefill, never
// auto-submitted).
func TestREPLDuringPromptHistoryRecall(t *testing.T) {
	rr := newDuringPromptTestReader("\x1b[A\x1b[A\x1b[B")
	rr.editor.SetInitialHistory([]string{"old1", "old2"})
	rr.beginPromptCapture() // re-seed history anchor for the editor's history

	// First pump: empty buffer (nothing typed yet) — up recalls the last entry.
	if _, _, err := pumpDuringPromptKey(rr); err != nil {
		t.Fatalf("up1: %v", err)
	}
	if string(rr.promptState.buf) != "old2" {
		t.Fatalf("after first up buf=%q, want old2", string(rr.promptState.buf))
	}
	// Second up: previous entry.
	if _, _, err := pumpDuringPromptKey(rr); err != nil {
		t.Fatalf("up2: %v", err)
	}
	if string(rr.promptState.buf) != "old1" {
		t.Fatalf("after second up buf=%q, want old1", string(rr.promptState.buf))
	}
	// Down: back to old2.
	if _, _, err := pumpDuringPromptKey(rr); err != nil {
		t.Fatalf("down: %v", err)
	}
	if string(rr.promptState.buf) != "old2" {
		t.Fatalf("after down buf=%q, want old2", string(rr.promptState.buf))
	}
}

// Ctrl-G during a prompt returns an edit request (done) carrying the typed buffer
// so the run loop can open $EDITOR on it; the during-prompt state is cleared.
func TestREPLDuringPromptCtrlGRequestsEdit(t *testing.T) {
	rr := newDuringPromptTestReader("wip\x07")
	for i := 0; i < 3; i++ {
		if _, _, err := pumpDuringPromptKey(rr); err != nil {
			t.Fatalf("type %d: %v", i, err)
		}
	}
	in, done, err := pumpDuringPromptKey(rr) // Ctrl-G (\x07 = lineTermEdit)
	if err != nil {
		t.Fatalf("ctrl-g: %v", err)
	}
	if !done || !in.edit || in.text != "wip" {
		t.Fatalf("ctrl-g = %+v done=%v, want edit request text %q", in, done, "wip")
	}
	// handleKey hands the buffer text to the run loop; the run loop (readDuringPrompt)
	// clears the during-prompt state. The buffer is intact here so the text is not
	// lost before the editor opens on it.
	if string(rr.promptState.buf) != "wip" {
		t.Fatalf("after ctrl-g buf=%q, want wip (intact until the run loop clears)", string(rr.promptState.buf))
	}
}

// newDuringPromptTestReader builds a replReader whose editor reads from a shared
// bufio.Reader seeded with input, mirroring the live read loop where readDuringPrompt
// reads a rune and hands it to handleKey (which reads escape-sequence tails from
// the same reader).
func newDuringPromptTestReader(input string) *replReader {
	r := bufio.NewReader(strings.NewReader(input))
	ed := newPromptLineEditorWithReader(r, io.Discard)
	rr := &replReader{r: r, editor: ed}
	rr.beginPromptCapture()
	return rr
}

// pumpDuringPromptKey reads one rune from the shared reader and dispatches it
// through the during-prompt handleKey path, emitting the status-line update on
// redraw. It returns the resulting input/done flag (done=false for ordinary
// edits; done=true only for interrupt/escape/edit/EOF gestures).
func pumpDuringPromptKey(rr *replReader) (replInput, bool, error) {
	r, _, err := rr.r.ReadRune()
	if err != nil {
		return replInput{}, false, err
	}
	res, err := rr.editor.handleKey(&rr.promptVi, rr.promptState, &rr.promptHistory, "", r, true)
	if err != nil {
		return replInput{}, false, err
	}
	if res.redraw {
		rr.emitPromptInput()
	}
	return res.input, res.done, nil
}

func TestContextDumpShowsTodoSemanticContextWithoutPendingReminder(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store
	store.Replace([]todo.Item{{Content: "explore", Status: todo.StatusInProgress}})

	// /context answers "what context does the model have" for the user, so it
	// renders the semantic recovery view even when no injection is pending.
	req := app.contextRequest()
	if got := strings.Join(req.RequestContext, "\n"); !strings.Contains(got, "[~] explore") {
		t.Fatalf("/context request context = %q, want the todo reminder", got)
	}

	// The display path must not consume a recovery reminder real requests need.
	store.Replace([]todo.Item{
		{Content: "explore", Status: todo.StatusCompleted},
		{Content: "test", Status: todo.StatusInProgress},
	})
	store.RequireRequestContext()
	app.contextRequest()
	if store.PendingRequestContext() == "" {
		t.Fatal("/context consumed the todo recovery reminder")
	}
}

func TestAccumulatingSinkPeekDoesNotConsumeTodoRecovery(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store
	store.Restore([]todo.Item{{Content: "explore", Status: todo.StatusInProgress}})

	sink := &accumulatingSink{r: app.Renderer, app: app}
	for i := range 2 {
		got := strings.Join(sink.PeekRequestContext(), "\n")
		if !strings.Contains(got, "[~] explore") {
			t.Fatalf("peek %d = %q, want the todo reminder (peeks must not consume it)", i+1, got)
		}
	}
	// Attaching is still a preview; only the final send boundary consumes the
	// one-shot reminder.
	if got := strings.Join(sink.RequestContext(), "\n"); !strings.Contains(got, "[~] explore") {
		t.Fatalf("RequestContext = %q, want the todo reminder", got)
	}
	if got := store.PendingRequestContext(); got == "" {
		t.Fatal("RequestContext consumed the reminder before a model request")
	}
	sink.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("after the send boundary, PendingRequestContext = %q, want empty", got)
	}

	store.Replace([]todo.Item{{Content: "implement", Status: todo.StatusInProgress}})
	sink.TranscriptRewritten()
	if got := strings.Join(sink.RequestContext(), "\n"); !strings.Contains(got, "[~] implement") {
		t.Fatalf("post-rewrite RequestContext = %q, want immediate recovery reminder", got)
	}
	sink.TurnAttemptStart(2, 1, agent.ContextEstimate{})
}

func TestAccumulatingSinkAdvancesTodoStaleReminder(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	store := todo.NewStore()
	reg := tools.Default()
	reg.Register(todo.NewTool(store))
	app.Agent.SetTools(reg)
	app.Todos = store
	store.Replace([]todo.Item{{Content: "implement", Status: todo.StatusInProgress}})

	sink := &accumulatingSink{r: app.Renderer, app: app}
	for request := 1; request < 12; request++ {
		if got := sink.RequestContext(); len(got) != 0 {
			t.Fatalf("request %d context = %q, want none", request, got)
		}
		sink.TurnAttemptStart(request, 1, agent.ContextEstimate{})
		sink.TurnAttemptStart(request, 2, agent.ContextEstimate{})
	}
	if got := strings.Join(sink.RequestContext(), "\n"); !strings.Contains(got, "[~] implement") {
		t.Fatalf("request 12 context = %q, want stale todo reminder", got)
	}
	sink.TurnAttemptStart(12, 1, agent.ContextEstimate{})
}

func newTestAppWithGoal(t *testing.T, out, errw testWriter, fp *llmtest.FakeProvider) *App {
	t.Helper()
	app := newTestApp(t, out, errw, fp)
	store := goal.NewStore()
	reg := tools.Default()
	reg.Register(goal.NewCreateTool(store, true))
	reg.Register(goal.NewUpdateTool(store, true))
	app.Agent.SetTools(reg)
	app.Goal = store
	app.GoalAutoContinue = true
	app.GoalMaxContinuations = 25
	return app
}

func TestREPLGoalCommandSetsObjective(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	app := newTestAppWithGoal(t, &out, &errw, fp)

	code := Run(strings.NewReader("/goal refactor the parser\n/exit\n"), app, nil)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "[goal set: refactor the parser]") {
		t.Fatalf("missing goal set notice:\n%s", errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; !strings.Contains(got, "Continue working toward the active session goal") || !strings.Contains(got, "<objective>refactor the parser</objective>") {
		t.Fatalf("first prompt = %q, want rendered goal continuation", got)
	}
	ctx := strings.Join(req.RequestContext, "\n\n")
	if !strings.Contains(ctx, "refactor the parser") || !strings.Contains(ctx, "<goal status=\"active\">") {
		t.Fatalf("missing goal reminder in request context:\n%s", ctx)
	}
}

func TestREPLGoalAutoContinuesUntilComplete(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("working")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{toolStep("update_goal", `{"status":"complete"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	finished := make(chan struct{}, 2)
	app.OnPromptFinished = func() { finished <- struct{}{} }
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	_, _ = fmt.Fprintln(writer, "/goal refactor the parser")
	<-finished // objective turn
	<-finished // autonomous continuation, including update_goal + final response
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	code := <-codeCh

	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("requests = %d, want 3 (initial + continuation + post-tool)", len(fp.Requests))
	}
	if got := app.Goal.Status(); got != goal.StatusComplete {
		t.Fatalf("goal status = %q, want complete", got)
	}
	cont := fp.Requests[1].Messages[len(fp.Requests[1].Messages)-1].Content[0].Text
	if !strings.Contains(cont, "Continue working toward the active session goal") {
		t.Fatalf("continuation prompt missing: %q", cont)
	}
}

func TestREPLGoalMaxContinuationsCapPauses(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("working")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("still working")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	app.GoalMaxContinuations = 1
	finished := make(chan struct{}, 2)
	app.OnPromptFinished = func() { finished <- struct{}{} }
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	_, _ = fmt.Fprintln(writer, "/goal refactor the parser")
	<-finished // objective turn
	<-finished // the one allowed continuation
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	code := <-codeCh

	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want 2 (initial + one continuation)", len(fp.Requests))
	}
	if got := app.Goal.Status(); got != goal.StatusPaused {
		t.Fatalf("goal status = %q, want paused", got)
	}
	if !strings.Contains(errw.String(), "goal paused after 1 continuation") {
		t.Fatalf("missing cap pause notice:\n%s", errw.String())
	}
}

func TestREPLGoalClear(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	app := newTestAppWithGoal(t, &out, &errw, fp)

	code := Run(strings.NewReader("/goal refactor the parser\n/goal clear\n/exit\n"), app, nil)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if app.Goal.Snapshot() != nil {
		t.Fatal("goal not cleared")
	}
	if !strings.Contains(errw.String(), "goal cleared") {
		t.Fatalf("missing clear notice:\n%s", errw.String())
	}
}

func TestGoalCommandShowPauseResumeAndExactSubcommands(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("ship it"); err != nil {
		t.Fatal(err)
	}
	app.goalCommand("")
	if got := errw.String(); !strings.Contains(got, "goal: active") || !strings.Contains(got, "objective: ship it") || !strings.Contains(got, "continuations: 0/25") {
		t.Fatalf("show output = %q", got)
	}
	app.goalCommand("pause")
	if app.Goal.Status() != goal.StatusPaused {
		t.Fatalf("status after pause = %q", app.Goal.Status())
	}
	app.Goal.BumpContinuations()
	resume := app.goalCommand("resume")
	if app.Goal.Status() != goal.StatusActive || app.Goal.Continuations() != 0 {
		t.Fatalf("resume state = %+v", app.Goal.Snapshot())
	}
	if !strings.Contains(resume.prompt, "Continue working toward the active session goal") {
		t.Fatalf("resume prompt = %q", resume.prompt)
	}
	result := app.goalCommand("clear the backlog")
	if app.Goal.Objective() != "clear the backlog" || !strings.Contains(result.prompt, "<objective>clear the backlog</objective>") {
		t.Fatalf("exact-match subcommand handling failed: state=%+v result=%+v", app.Goal.Snapshot(), result)
	}
}

func TestGoalCommandRejectsResumeWhileActive(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("keep working"); err != nil {
		t.Fatal(err)
	}
	app.Goal.BumpContinuations()

	result := app.goalCommand("resume")
	if result.prompt != "" {
		t.Fatalf("active resume prompt = %q, want none", result.prompt)
	}
	if !app.Goal.Active() || app.Goal.Continuations() != 1 {
		t.Fatalf("active goal changed after resume: %+v", app.Goal.Snapshot())
	}
	if !strings.Contains(errw.String(), "goal is already active") {
		t.Fatalf("active resume error missing: %q", errw.String())
	}
}

func TestContextRequestIncludesActiveGoalReminder(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("keep the context visible"); err != nil {
		t.Fatal(err)
	}

	ctx := strings.Join(app.contextRequest().RequestContext, "\n")
	if !strings.Contains(ctx, `<goal status="active">`) || !strings.Contains(ctx, "keep the context visible") {
		t.Fatalf("/context omitted active goal reminder: %q", ctx)
	}
}

func TestGoalReminderDisabledOutsideAutonomousREPL(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("do not leak into one-shot requests"); err != nil {
		t.Fatal(err)
	}
	app.GoalAutoContinue = false
	if got := app.goalRequestContext(); got != "" {
		t.Fatalf("non-autonomous goal context = %q, want empty", got)
	}
}

func TestGoalIdleTransitionsPersistImmediately(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("persist transitions"); err != nil {
		t.Fatal(err)
	}
	app.goalCommand("pause")
	loaded, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal == nil || loaded.Goal.Status != goal.StatusPaused {
		t.Fatalf("persisted pause = %+v", loaded.Goal)
	}

	app.goalCommand("resume")
	loaded, err = session.Load(app.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal == nil || loaded.Goal.Status != goal.StatusActive || loaded.Goal.Continuations != 0 {
		t.Fatalf("persisted resume = %+v", loaded.Goal)
	}

	app.goalCommand("clear")
	loaded, err = session.Load(app.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal != nil {
		t.Fatalf("persisted clear = %+v, want nil", loaded.Goal)
	}
}

func TestGoalContinuationCapPersistsPauseImmediately(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	app.GoalMaxContinuations = 1
	if err := app.Goal.Set("persist cap"); err != nil {
		t.Fatal(err)
	}
	app.Goal.BumpContinuations()
	if !app.pauseGoalAtContinuationCap() {
		t.Fatal("cap did not pause goal")
	}
	loaded, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal == nil || loaded.Goal.Status != goal.StatusPaused || loaded.Goal.Continuations != 1 {
		t.Fatalf("persisted cap state = %+v", loaded.Goal)
	}
}

func TestClearResetsGoal(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("ship it"); err != nil {
		t.Fatal(err)
	}
	app.clear()
	if app.Goal.Snapshot() != nil {
		t.Fatalf("goal survived /clear: %+v", app.Goal.Snapshot())
	}
}

func TestREPLRestoredActiveGoalContinuesAtFirstIdleBoundary(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolStep("update_goal", `{"status":"complete"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	if err := app.Goal.Set("finish restored work"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{}, 1)
	app.OnPromptFinished = func() { finished <- struct{}{} }
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	<-finished
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want continuation tool round + final round", len(fp.Requests))
	}
	prompt := fp.Requests[0].Messages[0].Content[0].Text
	if !strings.Contains(prompt, "Continue working toward the active session goal") || !strings.Contains(prompt, "finish restored work") {
		t.Fatalf("restored continuation prompt = %q", prompt)
	}
	if app.Goal.Status() != goal.StatusComplete {
		t.Fatalf("restored goal status = %q, want complete", app.Goal.Status())
	}
}

func TestREPLGoalContinuesAfterSwitchingBackToCapableAgent(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolStep("update_goal", `{"status":"complete"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	if err := app.Goal.Set("finish after switching back"); err != nil {
		t.Fatal(err)
	}
	app.Agent.SetTools(tools.Default())
	app.AgentName = "plan"
	autoTools := tools.Default()
	autoTools.Register(goal.NewCreateTool(app.Goal, true))
	autoTools.Register(goal.NewUpdateTool(app.Goal, true))
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		if name != "auto" {
			return AgentSelection{}, fmt.Errorf("unknown agent %q", name)
		}
		return AgentSelection{Name: "auto", Tools: autoTools, System: app.System}, nil
	}

	if code := Run(strings.NewReader("/agent auto\n/exit\n"), app, nil); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("requests = %d, want continuation tool round + final round", fp.RequestCount())
	}
	prompt := fp.Requests[0].Messages[0].Content[0].Text
	if !strings.Contains(prompt, "finish after switching back") {
		t.Fatalf("continuation prompt = %q", prompt)
	}
	if app.Goal.Status() != goal.StatusComplete {
		t.Fatalf("goal status = %q, want complete", app.Goal.Status())
	}
}

func TestBackgroundGoalCreationWakesIdleREPLAndPersists(t *testing.T) {
	var out, errw lockedBuffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolStep("update_goal", `{"status":"complete"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	finished := make(chan struct{}, 1)
	app.OnPromptFinished = func() { finished <- struct{}{} }
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "idle prompt")

	if err := app.Goal.Create("wake the root loop"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("background goal did not wake idle REPL")
	}
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 || app.Goal.Status() != goal.StatusComplete {
		t.Fatalf("background goal result: requests=%d state=%+v", fp.RequestCount(), app.Goal.Snapshot())
	}
	loaded, err := session.Load(app.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal == nil || loaded.Goal.Status != goal.StatusComplete || loaded.Goal.Objective != "wake the root loop" {
		t.Fatalf("persisted background goal = %+v", loaded.Goal)
	}
}

func TestBackgroundGoalCreationPreservesIdleEditorDraft(t *testing.T) {
	var out, errw lockedBuffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("user prompt done")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{toolStep("update_goal", `{"status":"complete"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("goal done")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	finished := make(chan struct{}, 2)
	app.OnPromptFinished = func() { finished <- struct{}{} }
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(reader, app, nil, true) }()
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> ") }, "idle editor prompt")

	writePipe(t, writer, "unsent draft")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> unsent draft") }, "typed idle draft")
	if err := app.Goal.Create("wake without losing input"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return strings.Count(errw.String(), "> unsent draft") >= 2
	}, "deposited draft redisplay")

	if got := fp.RequestCount(); got != 0 {
		t.Fatalf("goal started over an unsent draft: requests=%d", got)
	}
	select {
	case code := <-codeCh:
		t.Fatalf("REPL exited while preserving draft: code=%d stderr=%q", code, errw.String())
	default:
	}

	writePipe(t, writer, "\r")
	<-finished // submitted user draft
	<-finished // autonomous continuation, including update_goal
	writePipe(t, writer, "/exit\r")
	_ = writer.Close()
	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 3 {
		t.Fatalf("requests = %d, want user prompt plus goal tool/final rounds", fp.RequestCount())
	}
	prompts := transcriptPrompts(app)
	if !strings.HasPrefix(prompts, "unsent draft|") || !strings.Contains(prompts, "wake without losing input") {
		t.Fatalf("prompt order = %q, want preserved draft before goal continuation", prompts)
	}
}

func TestGoalPromptAdmissionSkipsGoalCompletedDuringRefresh(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestAppWithGoal(t, &out, &errw, fp)
	if err := app.Goal.Set("finish before admission"); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{})
	app.RefreshMCP = func(context.Context, string) (*tools.Registry, string) {
		if err := app.Goal.MarkStatus(goal.StatusComplete); err != nil {
			t.Errorf("MarkStatus: %v", err)
		}
		close(refreshed)
		return nil, ""
	}
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	<-refreshed
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()

	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 0 || app.Goal.Continuations() != 0 {
		t.Fatalf("stale goal prompt ran: requests=%d continuations=%d", fp.RequestCount(), app.Goal.Continuations())
	}
	if app.Goal.Status() != goal.StatusComplete {
		t.Fatalf("goal status = %q, want complete", app.Goal.Status())
	}
}

func TestGoalContinuationSkippedWhenRefreshRemovesGoalTool(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestAppWithGoal(t, &out, &errw, fp)
	if err := app.Goal.Set("wait for a capable agent"); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{})
	app.RefreshMCP = func(context.Context, string) (*tools.Registry, string) {
		close(refreshed)
		return tools.Default(), ""
	}
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	<-refreshed
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()

	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 0 || app.Goal.Continuations() != 0 {
		t.Fatalf("incapable continuation ran: requests=%d continuations=%d", fp.RequestCount(), app.Goal.Continuations())
	}
	if app.Goal.Status() != goal.StatusActive {
		t.Fatalf("goal status = %q, want active", app.Goal.Status())
	}
}

func TestGoalPromptInterruptedDuringRefreshPausesGoal(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("pause before admission"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	app.RefreshMCP = func(ctx context.Context, _ string) (*tools.Registry, string) {
		close(started)
		<-ctx.Done()
		return nil, ""
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	exit := make(chan struct{})
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, exit) }()
	<-started
	close(exit)

	if code := <-codeCh; code != ExitInterrupt {
		t.Fatalf("exit code = %d, want interrupt; errw=%q", code, errw.String())
	}
	if app.Goal.Status() != goal.StatusPaused {
		t.Fatalf("goal status = %q, want paused", app.Goal.Status())
	}
	if !strings.Contains(errw.String(), "[goal paused; /goal resume to continue]") {
		t.Fatalf("missing pause notice: %q", errw.String())
	}
}

func TestREPLInterruptedGoalPromptPausesGoal(t *testing.T) {
	var out, errw lockedBuffer
	started := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Stop: llm.StopEndTurn,
		Block: func(ctx context.Context) {
			close(started)
			<-ctx.Done()
		},
	})
	app := newTestAppWithGoal(t, &out, &errw, fp)
	app.Interrupt = agent.NewInterruptWatcher(nil, time.Now, func() {})
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	_, _ = fmt.Fprintln(writer, "/goal keep working")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal prompt did not start")
	}
	app.Interrupt.CancelPrompt()
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[goal paused; /goal resume to continue]") }, "goal pause notice")
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if app.Goal.Status() != goal.StatusPaused {
		t.Fatalf("goal status = %q, want paused", app.Goal.Status())
	}
}

func TestInterruptedPromptPausesGoalCreatedDuringPrompt(t *testing.T) {
	var out, errw lockedBuffer
	started := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolStep("create_goal", `{"objective":"created in flight"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{
			Stop: llm.StopEndTurn,
			Block: func(ctx context.Context) {
				close(started)
				<-ctx.Done()
			},
		},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	app.Interrupt = agent.NewInterruptWatcher(nil, time.Now, func() {})
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	_, _ = fmt.Fprintln(writer, "start an autonomous task")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("post-create model round did not start")
	}
	app.Interrupt.CancelPrompt()
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[goal paused; /goal resume to continue]") }, "created goal pause notice")
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	state := app.Goal.Snapshot()
	if state == nil || state.Objective != "created in flight" || state.Status != goal.StatusPaused {
		t.Fatalf("created goal after interruption = %+v, want paused", state)
	}
}

func TestInterruptedGoalPromptDoesNotPauseReplacementGoal(t *testing.T) {
	var out, errw lockedBuffer
	started := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Stop: llm.StopEndTurn,
		Block: func(ctx context.Context) {
			close(started)
			<-ctx.Done()
		},
	})
	app := newTestAppWithGoal(t, &out, &errw, fp)
	app.Interrupt = agent.NewInterruptWatcher(nil, time.Now, func() {})
	if err := app.Goal.Set("old objective"); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal prompt did not start")
	}
	_, _ = fmt.Fprintln(writer, "/exit")
	if err := app.Goal.Set("replacement objective"); err != nil {
		t.Fatal(err)
	}
	app.Interrupt.CancelPrompt()
	_ = writer.Close()

	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	state := app.Goal.Snapshot()
	if state == nil || state.Status != goal.StatusActive || state.Objective != "replacement objective" || state.Continuations != 0 {
		t.Fatalf("replacement goal changed by stale interruption: %+v", state)
	}
	if strings.Contains(errw.String(), "[goal paused; /goal resume to continue]") {
		t.Fatalf("stale interruption printed pause notice: %q", errw.String())
	}
}

func TestGoalPromptInterruptedDuringSubmitHookPausesGoal(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("pause during hook"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-started")
	command := fmt.Sprintf("printf started > %q; sleep 600", marker)
	raw, err := json.Marshal(map[string]any{
		"UserPromptSubmit": []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hooks.DecodeEventMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	app.Hooks = &hooks.Runner{Config: cfg}
	exit := make(chan struct{})
	reader, writer := io.Pipe()
	defer writer.Close()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, exit) }()
	waitFor(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, "prompt-submit hook start")
	close(exit)

	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("exit code = %d, want interrupt; errw=%q", code, errw.String())
	}
	if app.Goal.Status() != goal.StatusPaused || app.Goal.Continuations() != 0 {
		t.Fatalf("interrupted hook goal = %+v", app.Goal.Snapshot())
	}
	if app.PromptNumber != 0 {
		t.Fatalf("interrupted hook recorded %d prompts, want 0", app.PromptNumber)
	}
}

func TestGoalDeadlineDoesNotPause(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("survive deadline"); err != nil {
		t.Fatal(err)
	}
	revision, active := app.Goal.ActiveRevisionSnapshot()
	app.goalOnPromptEnd(context.Background(), context.DeadlineExceeded, revision, active)
	if got := app.Goal.Status(); got != goal.StatusActive {
		t.Fatalf("goal status = %q after deadline, want active", got)
	}
	if app.lastPromptInterrupted {
		t.Fatal("deadline was classified as user interruption")
	}
}

func TestRejectedGoalContinuationPausesWithoutConsumingCap(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestAppWithGoal(t, &out, &errw, fp)
	cfg, err := hooks.DecodeEventMap([]byte(`{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf '{\"decision\":\"block\",\"reason\":\"policy\"}'"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	app.Hooks = &hooks.Runner{Config: cfg}
	if err := app.Goal.Set("blocked by hook"); err != nil {
		t.Fatal(err)
	}
	if code := Run(strings.NewReader("/exit\n"), app, nil); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := app.Goal.Status(); got != goal.StatusPaused {
		t.Fatalf("goal status = %q, want paused", got)
	}
	if got := app.Goal.Continuations(); got != 0 {
		t.Fatalf("continuations = %d, want rejected prompt not consumed", got)
	}
	if fp.RequestCount() != 0 || len(app.Agent.Transcript()) != 0 {
		t.Fatalf("rejected goal prompt reached agent: requests=%d transcript=%+v", fp.RequestCount(), app.Agent.Transcript())
	}
	if !strings.Contains(errw.String(), "goal paused because its continuation prompt was rejected") {
		t.Fatalf("missing rejected-continuation pause notice: %q", errw.String())
	}
}

func TestRejectedGoalCommandPromptPausesGoal(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestAppWithGoal(t, &out, &errw, fp)
	cfg, err := hooks.DecodeEventMap([]byte(`{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf '{\"decision\":\"block\",\"reason\":\"policy\"}'"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	app.Hooks = &hooks.Runner{Config: cfg}

	if code := Run(strings.NewReader("/goal blocked directly\n/exit\n"), app, nil); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if app.Goal.Status() != goal.StatusPaused {
		t.Fatalf("goal status = %q, want paused", app.Goal.Status())
	}
	if app.Goal.Continuations() != 0 || fp.RequestCount() != 0 {
		t.Fatalf("rejected direct goal ran: continuations=%d requests=%d", app.Goal.Continuations(), fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "goal paused because its continuation prompt was rejected") {
		t.Fatalf("missing rejected goal prompt notice: %q", errw.String())
	}
}

func TestREPLQueuedUserInputWinsOverGoalContinuation(t *testing.T) {
	var out, errw bytes.Buffer
	started := make(chan struct{})
	release := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop: llm.StopEndTurn,
			Block: func(context.Context) {
				close(started)
				<-release
			},
		},
		llmtest.Step{Events: []llm.StreamEvent{toolStep("update_goal", `{"status":"complete"}`, "call_1")}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	app := newTestAppWithGoal(t, &out, &errw, fp)
	finished := make(chan struct{}, 2)
	app.OnPromptFinished = func() { finished <- struct{}{} }
	delivered := make(chan struct{}, 3)
	app.onInputDelivered = func() { delivered <- struct{}{} }
	reader, writer := io.Pipe()
	codeCh := make(chan int, 1)
	go func() { codeCh <- Run(reader, app, nil) }()
	_, _ = fmt.Fprintln(writer, "/goal original objective")
	<-delivered
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal prompt did not start")
	}
	_, _ = fmt.Fprintln(writer, "user input wins")
	<-delivered // prove reader publication precedes prompt completion
	close(release)
	<-finished
	<-finished
	_, _ = fmt.Fprintln(writer, "/exit")
	_ = writer.Close()
	if code := <-codeCh; code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("requests = %d, want initial + user tool round + final", len(fp.Requests))
	}
	second := fp.Requests[1].Messages
	if got := second[len(second)-1].Content[0].Text; got != "user input wins" {
		t.Fatalf("second prompt = %q, want queued user input", got)
	}
}

func TestGoalReminderSurvivesTranscriptRewrite(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestAppWithGoal(t, &out, &errw, llmtest.New("fake"))
	if err := app.Goal.Set("preserve this objective"); err != nil {
		t.Fatal(err)
	}
	sink := newAccumulatingSink(app.Renderer, app, 1)
	before := strings.Join(sink.RequestContext(), "\n")
	if !strings.Contains(before, "preserve this objective") {
		t.Fatalf("initial goal context = %q", before)
	}
	sink.TranscriptRewritten()
	after := strings.Join(sink.PeekRequestContext(), "\n")
	if !strings.Contains(after, "preserve this objective") {
		t.Fatalf("post-rewrite goal context = %q", after)
	}
}
