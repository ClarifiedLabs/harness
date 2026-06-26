package ui

import (
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
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/plan"
	"harness/internal/session"
	"harness/internal/skills"
	"harness/internal/todo"
	"harness/internal/tools"
)

const uiOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func writeUIImage(t *testing.T) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(uiOnePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "screen.png")
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

// lockedBuffer is a mutex-guarded bytes.Buffer. The during-turn-input tests poll
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
	if app.Turn != 0 {
		t.Fatalf("turn = %d, want 0", app.Turn)
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

func TestREPLRecordsModelTurnTimingEvents(t *testing.T) {
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
	if !strings.Contains(got, `"type":"model_turn_start"`) {
		t.Fatalf("missing model_turn_start event:\n%s", got)
	}
	if !strings.Contains(got, `"type":"model_turn_usage"`) {
		t.Fatalf("missing model_turn_usage event:\n%s", got)
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
	data, _ := os.ReadFile(filepath.Join(path, "state.json"))
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

func TestREPLClearResetsAndRotates(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("one")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("two")}, Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	origPath := app.SessionPath

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
	app := &App{
		Errw:              new(bytes.Buffer),
		Out:               new(bytes.Buffer),
		PromptEditMode:    "emacs",
		SetPromptEditMode: func(m string) { modeChange = m },
	}

	// /vi on
	app.command("/vi on", nil)
	if app.PromptEditMode != "vi" {
		t.Errorf("/vi on: PromptEditMode = %q, want vi", app.PromptEditMode)
	}
	if modeChange != "vi" {
		t.Errorf("/vi on: SetPromptEditMode called with %q, want vi", modeChange)
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
	app.Skills = map[string]skills.Skill{
		"commit": {
			Name:        "commit",
			Description: "Create a git commit",
			Location:    "/skills/commit/SKILL.md",
		},
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
	if !strings.Contains(got, "[explicit skill mentions]") ||
		!strings.Contains(got, "path: /skills/commit/SKILL.md") ||
		!strings.Contains(got, "read the full SKILL.md") {
		t.Fatalf("request context missing skill instructions:\n%s", got)
	}
}

func TestREPLTypedEscapedSkillMentionStillScansLaterMentions(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
		"review": {Name: "review", Description: "Review code", Location: "/skills/review/SKILL.md"},
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
	if strings.Contains(got, "path: /skills/commit/SKILL.md") {
		t.Fatalf("escaped skill mention should not add commit context:\n%s", got)
	}
	if !strings.Contains(got, "path: /skills/review/SKILL.md") {
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
	if got := strings.Join(req.RequestContext, "\n\n"); strings.Contains(got, "[explicit skill mentions]") {
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
	want := "[session summary: 100 input / 30 cached input / 10 output / 4 reasoning / 20 cache write]\nresume with: harness -resume " + app.SessionPath
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
		"  read_file    Read a file from disk.",
		"  list_dir     List directory entries",
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
	if col := toolSummarySeparatorColumn(t, got, "read_file"); col != toolSummarySeparatorColumn(t, got, "list_dir") {
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

func toolSummarySeparatorColumn(t *testing.T, summary, name string) int {
	t.Helper()
	for _, line := range strings.Split(summary, "\n") {
		if strings.Contains(line, name) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				t.Fatalf("summary line for %q has no description:\n%s", name, summary)
			}
			return strings.Index(line, fields[1])
		}
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
}

func TestREPLCompactCommand(t *testing.T) {
	var out, errw bytes.Buffer
	// The only model call here is the summary call /compact triggers.
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("CANNED SUMMARY")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 9100, OutputTokens: 400}},
	)
	app := newTestApp(t, &out, &errw, fp)

	// Seed enough whole turns that there is something older than the last four
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
	if len(msgs) != 1+8 {
		t.Fatalf("/compact should collapse to summary + last 4 turns (9 msgs), got %d", len(msgs))
	}
	got := errw.String()
	if !strings.Contains(got, "compacted") {
		t.Errorf("/compact should print a compaction report, errw=%q", got)
	}
	// The summary call's tokens must fold into the cumulative session totals.
	if !strings.Contains(got, "9100") || !strings.Contains(got, "400 out") {
		t.Errorf("/usage should include the summary call usage after /compact, errw=%q", got)
	}
	// The summary call was actually issued (the only model call here).
	if fp.RequestCount() != 1 {
		t.Errorf("/compact should issue exactly the summary call, got %d requests", fp.RequestCount())
	}
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

func TestREPLEffortCommandListsAndSwitchesNextTurn(t *testing.T) {
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
				Options:   []llm.ReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
			},
		},
	})

	in := strings.NewReader("/effort\n/effort high\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	for _, want := range []string{
		"available efforts for anthropic:claude-opus-4-8:",
		"provider default (current)",
		"high",
		"[reasoning effort: high]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/effort output missing %q:\n%s", want, got)
		}
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.Effort != "high" {
		t.Fatalf("request effort = %q, want high", fp.Requests[0].Reasoning.Effort)
	}
}

func TestREPLEffortCommandSendsExplicitNone(t *testing.T) {
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
				Options:   []llm.ReasoningOption{{Type: "effort", Values: []string{"none", "minimal", "low", "medium", "high", "xhigh"}}},
			},
		},
	})

	in := strings.NewReader("/effort none\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.Effort != "none" {
		t.Fatalf("request effort = %q, want none", fp.Requests[0].Reasoning.Effort)
	}
}

func TestREPLReasoningCommandSetsBudgetTokens(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RegistryModel = "anthropic:claude-opus-4-8"
	minBudget, maxBudget := 1024, 4096
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {
			Reasoning: &llm.ReasoningInfo{
				Supported: true,
				Options: []llm.ReasoningOption{
					{Type: "budget_tokens", Min: &minBudget, Max: &maxBudget},
				},
			},
		},
	})

	in := strings.NewReader("/reasoning\n/reasoning budget 2048\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	for _, want := range []string{
		"available controls for anthropic:claude-opus-4-8:",
		"budget_tokens: 1024..4096",
		"[reasoning: budget_tokens=2048]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/reasoning output missing %q:\n%s", want, got)
		}
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.BudgetTokens == nil || *fp.Requests[0].Reasoning.BudgetTokens != 2048 {
		t.Fatalf("request budget_tokens = %v, want 2048", fp.Requests[0].Reasoning.BudgetTokens)
	}
}

func TestREPLReasoningCommandSetsToggle(t *testing.T) {
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
				Options:   []llm.ReasoningOption{{Type: "toggle"}},
			},
		},
	})

	in := strings.NewReader("/reasoning off\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "[reasoning: enabled=false]") {
		t.Fatalf("toggle should be acknowledged, errw=%q", errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.Enabled == nil || *fp.Requests[0].Reasoning.Enabled {
		t.Fatalf("request enabled = %v, want false", fp.Requests[0].Reasoning.Enabled)
	}
}

func TestREPLEffortCommandRejectsInvalidLevelForCurrentModel(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.RegistryModel = "anthropic:claude-opus-4-8"
	app.Reasoning = llm.ReasoningConfig{Effort: "medium"}
	app.Agent.SetReasoning(app.Reasoning)
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {
			Reasoning: &llm.ReasoningInfo{
				Supported: true,
				Options:   []llm.ReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
			},
		},
	})

	in := strings.NewReader("/effort xhigh\nhello\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), `does not support reasoning effort "xhigh"`) {
		t.Fatalf("invalid effort should be reported, errw=%q", errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	if fp.Requests[0].Reasoning.Effort != "medium" {
		t.Fatalf("request effort = %q, want unchanged medium", fp.Requests[0].Reasoning.Effort)
	}
}

func TestREPLAgentCommandLists(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.AgentName = "plan"
	app.AvailableAgents = []AgentSummary{
		{Name: "auto", Description: "Default agent"},
		{Name: "independent", Description: "Work independently", Provider: "openai"},
		{Name: "plan", Description: "Plan changes", Provider: "anthropic", Model: "claude-opus-4-8", Delegatable: true},
		{Name: "style", Description: "Style review", Model: "gpt-5.5", Delegatable: true},
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
		"current agent: plan [anthropic/claude-opus-4-8]",
		"auto            [inherit current] Default agent",
		"independent     [openai/inherit current model] Work independently",
		"plan (current)  [anthropic/claude-opus-4-8] [delegatable] Plan changes",
		"style           [inherit provider/gpt-5.5] [delegatable] Style review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/agent output missing %q, errw=%q", want, got)
		}
	}
	for _, notWant := range []string{
		"auto            [inherit current] [delegatable]",
		"independent     [openai/inherit current model] [delegatable]",
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
		{Name: "review", Description: "short", Provider: "openai", Model: "gpt-5.5"},
	}

	if code := Run(strings.NewReader("/agent\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := errw.String()
	for _, want := range []string{
		"  auto (current)  [inherit current] one two three four",
		"                  five six",
		"  review          [openai/gpt-5.5] short",
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
	app.Reasoning = llm.ReasoningConfig{Effort: "max"}
	app.Agent.SetReasoning(app.Reasoning)
	catalog, _ := tools.CatalogWithOptions(tools.Options{SearchTools: tools.SearchToolsGrep})
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
		!strings.Contains(errw.String(), "provider: anthropic  model: claude-opus-4-8  reasoning: effort=max  pricing: in=$0.43/M out=$0.87/M cache-read=$0/M") {
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
		{Content: "implement", Status: todo.StatusInProgress, ActiveForm: "Implementing"},
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
		`[todo]\nTodos (1/2 done):`,
		`[x] explore`,
		`[~] Implementing`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/context output missing %q:\n%s", want, got)
		}
	}
}

func TestREPLPrintsTodoStatusAfterUpdateTodosAndBeforePrompt(t *testing.T) {
	var out, setupErrw bytes.Buffer
	status := "Todos (1/2 done):\n  [x] explore\n  [~] Testing"
	errw := newSignalBuffer(status + "\n[auto] > ")
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolStep("update_todos", `{"todos":[{"content":"explore","status":"completed"},{"content":"test","status":"in_progress","active_form":"Testing"}]}`, "call_todo")},
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
	select {
	case <-errw.seen:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for todo status before the prompt:\n%s", errw.String())
	}
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
	nextModelIndex := strings.Index(got, "[model: turn 2 waiting]")
	if nextModelIndex < 0 {
		t.Fatalf("second model turn was not rendered:\n%s", got)
	}
	if !(toolResultIndex < statusIndex && statusIndex < nextModelIndex) {
		t.Fatalf("todo status should print immediately after update_todos and before the next model turn:\n%s", got)
	}

	promptStatusIndex := strings.LastIndex(got, status)
	if promptStatusIndex == statusIndex {
		t.Fatalf("todo status should also be printed before the next prompt:\n%s", got)
	}
	if promptIndex := strings.Index(got[promptStatusIndex+len(status):], "> "); promptIndex < 0 {
		t.Fatalf("todo status should be followed by the next REPL prompt:\n%s", got)
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
	// across the whole per-model-turn budget (1 + 2 retries) to surface to errw.
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

func TestREPLEscapeEscapeCancelsActiveTurn(t *testing.T) {
	var out, errw bytes.Buffer
	inTurn := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 5, OutputTokens: 1},
		Block: func(ctx context.Context) {
			close(inTurn)
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
	case <-inTurn:
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
		t.Fatal("Esc-Esc must cancel the turn without requesting process exit")
	default:
	}
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
	inTurn := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inTurn)
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
	case <-inTurn:
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
// script that pipes several lines runs each as a turn. The during-turn-input
// deposit behavior (never auto-submit, deposit as editable prefill) applies only
// to the interactive prompt-editor path — see
// TestREPLDuringTurnInputDepositedOnCompletionNotAutoSubmitted.
func TestREPLTypeaheadDuringActiveTurnRunsAfterTurn(t *testing.T) {
	var out, errw bytes.Buffer
	inTurn := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inTurn)
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
	case <-inTurn:
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

func TestREPLPromptEditorPrintsPromptAfterTurnWithPendingActiveRead(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inTurn := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) {
				close(inTurn)
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
	case <-inTurn:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	close(releaseTurn)
	waitFor(t, func() bool {
		s := errw.String()
		return strings.Contains(s, "[turn:") && strings.Count(s, "> ") >= 2
	}, "prompt after first turn")

	// The post-turn prompt is the raw line editor, so Enter is \r (the canonical
	// \n fallback is gone now that during-turn input is captured raw).
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
	app.addUsage(agent.TurnUsage{Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}})
	app.Provider, app.Model, app.RegistryModel = "openai", "gpt", "gpt"
	app.addUsage(agent.TurnUsage{Usage: llm.Usage{InputTokens: 30, OutputTokens: 5}})

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
}

func TestUsageReportSingleModelMatchesLegacyFormat(t *testing.T) {
	app := &App{Provider: "anthropic", Model: "opus", RegistryModel: "opus"}
	app.addUsage(agent.TurnUsage{Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 30, OutputTokens: 10, ReasoningTokens: 4, CacheWriteTokens: 20}})
	got := app.usageReport("session summary")
	want := "[session summary: 100 input / 30 cached input / 10 output / 4 reasoning / 20 cache write]"
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

	app.handoffToImplementation(plan.HandoffRequest{Agent: "auto", PlanPath: "/sess/plans/0001.plan.md", Brief: "tests run with go test"})

	msgs := app.Agent.Transcript()
	if len(msgs) != 1 {
		t.Fatalf("want a single seeded message, got %d", len(msgs))
	}
	if err := llm.ValidateTranscript(msgs); err != nil {
		t.Fatalf("seeded transcript invalid: %v", err)
	}
	seed := msgs[0].Content[0].Text
	for _, want := range []string{"Implementation handoff", "/sess/plans/0001.plan.md", "tests run with go test"} {
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
	inTurn := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("plan ready")},
			Stop:   llm.StopEndTurn,
			Block: func(ctx context.Context) {
				pending.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
				close(inTurn)
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
	case <-inTurn:
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
// and during-turn input line, so the typed buffer renders to errw in tests.
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

// During-turn typed input is NEVER auto-submitted (rule 1): Enter inserts a
// newline into the buffer, and on completion the buffer is deposited into the
// next prompt as editable, pre-filled text submitted manually (rule 2).
func TestREPLDuringTurnInputDepositedOnCompletionNotAutoSubmitted(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inTurn := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		// No events on the blocking step: the model is in its initial wait, so
		// the live counter stays active and the typed buffer renders on it.
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) { close(inTurn); <-releaseTurn },
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
	case <-inTurn:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	// Type during the turn, with Enter mid-word: it must render live and the
	// Enter must insert a newline, never start a turn.
	writePipe(t, pw, "dr\raft")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> dr aft") }, "live input line with sanitized newline")
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[turn:") }, "turn 1 usage line")

	if fp.RequestCount() != 1 {
		t.Fatalf("during-turn input must not auto-submit; got %d requests", fp.RequestCount())
	}

	// The deposited buffer is editable prefill; submitting it manually runs it,
	// and it preserves the newline that Enter inserted.
	writePipe(t, pw, "\r")
	waitFor(t, func() bool { return fp.RequestCount() == 2 }, "second request from deposited prefill")
	_ = pw.Close()

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d; errw=%q", code, errw.String())
	}
	if got := transcriptPrompts(app); got != "first|dr\naft" {
		t.Fatalf("prompts = %q, want %q (deposited text, newline preserved, submitted manually)", got, "first|dr\naft")
	}
}

// On interrupt (double-Esc) the typed-so-far buffer is still deposited (rule 2),
// and the turn is cancelled (Esc-Esc still interrupts).
func TestREPLDuringTurnInputDepositedOnInterrupt(t *testing.T) {
	var out, errw lockedBuffer // concurrent renderer writes vs waitFor reads
	inTurn := make(chan struct{})
	fp := llmtest.New("fake",
		// No events: the model is in its initial wait so the live input line is
		// active when the user types and double-Esc cancels.
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 1},
			Block: func(ctx context.Context) { close(inTurn); <-ctx.Done() },
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
	case <-inTurn:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	// Type, then double-Esc to interrupt. The typed text survives as a deposit.
	writePipe(t, pw, "wip")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "> wip") }, "live input line")
	writePipe(t, pw, "\x1b\x1b")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[cancelled]") }, "turn cancelled")

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

// The during-turn capture is a single-line editor: printable runes, inserts,
// and deletes act at turnCursor, and Left/Right/Home/End move it. This exercises
// the full edit grammar and asserts both the buffer and the cursor (mirrored on
// the live onTurnInput callback) after each operation.
func TestREPLDuringTurnCursorEditing(t *testing.T) {
	rr := &replReader{}
	var emittedBuf string
	var emittedCursor int
	rr.onTurnInput = func(buf string, cursor int) { emittedBuf, emittedCursor = buf, cursor }

	check := func(wantBuf string, wantCursor int) {
		t.Helper()
		if string(rr.turnBuf) != wantBuf || rr.turnCursor != wantCursor {
			t.Fatalf("buf=%q cursor=%d, want buf=%q cursor=%d", string(rr.turnBuf), rr.turnCursor, wantBuf, wantCursor)
		}
		if emittedBuf != wantBuf || emittedCursor != wantCursor {
			t.Fatalf("emitted buf=%q cursor=%d, want buf=%q cursor=%d", emittedBuf, emittedCursor, wantBuf, wantCursor)
		}
	}

	rr.turnInsertRunes([]rune("abc")) // type "abc"
	check("abc", 3)
	rr.applyTurnAction(lineEditLeft, "") // -> between b and c
	check("abc", 2)
	rr.applyTurnAction(lineEditLeft, "") // -> between a and b
	check("abc", 1)
	rr.turnInsertRunes([]rune("X")) // insert mid-buffer
	check("aXbc", 2)
	rr.applyTurnAction(lineEditHome, "")
	check("aXbc", 0)
	rr.applyTurnAction(lineEditBackspace, "") // no-op at start
	check("aXbc", 0)
	rr.applyTurnAction(lineEditDelete, "") // delete rune AT cursor
	check("Xbc", 0)
	rr.applyTurnAction(lineEditEnd, "")
	check("Xbc", 3)
	rr.applyTurnAction(lineEditBackspace, "") // delete rune BEFORE cursor
	check("Xb", 2)
	rr.applyTurnAction(lineEditRight, "") // no-op at end
	check("Xb", 2)
	rr.applyTurnAction(lineEditInsertNewline, "") // Enter inserts a newline, never submits
	check("Xb\n", 3)
	rr.applyTurnAction(lineEditInsertText, "yz") // pasted/CSI-u text inserts at cursor
	check("Xb\nyz", 5)
	rr.applyTurnAction(lineEditHome, "")
	check("Xb\nyz", 0)
	rr.applyTurnAction(lineEditDelete, "")
	check("b\nyz", 0)

	// Deposit returns the full buffer (newline preserved) and resets buffer+cursor.
	dep := rr.depositTurnBuffer()
	if !dep.deposit || dep.text != "b\nyz" {
		t.Fatalf("deposit = %+v, want text %q deposit=true", dep, "b\nyz")
	}
	if len(rr.turnBuf) != 0 || rr.turnCursor != 0 {
		t.Fatalf("after deposit buf=%q cursor=%d, want empty buffer and cursor 0", string(rr.turnBuf), rr.turnCursor)
	}
}

// Wide and multi-byte runes must move the cursor by whole runes, and a stale
// cursor past the (now shorter) buffer is clamped rather than panicking.
func TestREPLDuringTurnCursorWideRunesAndClamp(t *testing.T) {
	rr := &replReader{}
	rr.turnInsertRunes([]rune("aé漢")) // 1-byte, 2-byte, 3-byte runes
	if string(rr.turnBuf) != "aé漢" || rr.turnCursor != 3 {
		t.Fatalf("buf=%q cursor=%d, want aé漢 / 3", string(rr.turnBuf), rr.turnCursor)
	}
	rr.applyTurnAction(lineEditLeft, "") // between é and 漢
	rr.applyTurnAction(lineEditBackspace, "")
	if string(rr.turnBuf) != "a漢" || rr.turnCursor != 1 {
		t.Fatalf("buf=%q cursor=%d, want a漢 / 1", string(rr.turnBuf), rr.turnCursor)
	}
	// A stale out-of-range cursor is clamped to the buffer end on the next edit.
	rr.turnCursor = 99
	rr.applyTurnAction(lineEditBackspace, "")
	if string(rr.turnBuf) != "a" || rr.turnCursor != 1 {
		t.Fatalf("buf=%q cursor=%d after clamped backspace, want a / 1", string(rr.turnBuf), rr.turnCursor)
	}
}

func TestEffortMenuFallsBackToDefaults(t *testing.T) {
	catalog := &llm.ReasoningInfo{Supported: true, Options: []llm.ReasoningOption{{Type: "effort", Values: []string{"low", "high"}}}}
	if v, fromCatalog := effortMenu(catalog); !fromCatalog || strings.Join(v, ",") != "low,high" {
		t.Errorf("catalog efforts = %v (catalog=%v), want low,high from catalog", v, fromCatalog)
	}
	// Provider-defined (supported, no enumerated options): offer the default
	// menu (r61).
	noLevels := &llm.ReasoningInfo{Supported: true}
	if v, fromCatalog := effortMenu(noLevels); fromCatalog || strings.Join(v, ",") != "none,minimal,low,medium,high,xhigh,max" {
		t.Errorf("fallback efforts = %v (catalog=%v), want the default menu", v, fromCatalog)
	}
	// Supported via a non-effort control (toggle): effort is not accepted, so no
	// menu is offered.
	toggleOnly := &llm.ReasoningInfo{Supported: true, Options: []llm.ReasoningOption{{Type: "toggle"}}}
	if v, _ := effortMenu(toggleOnly); len(v) != 0 {
		t.Errorf("toggle-only efforts = %v, want none", v)
	}
}
