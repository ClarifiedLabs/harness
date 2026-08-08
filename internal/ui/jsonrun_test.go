package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/handoff"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/runstream"
	"harness/internal/skills"
	"harness/internal/tools"
)

// newJSONRunApp builds a test app wired for interactive JSON mode: a run
// stream writer on a locked buffer and the app sharing it.
func newJSONRunApp(t *testing.T, fp *llmtest.FakeProvider) (*App, *lockedBuffer, *lockedBuffer, *runstream.Writer) {
	t.Helper()
	var out, errw lockedBuffer
	app := newTestApp(t, &out, &errw, fp)
	stream := &lockedBuffer{}
	w := runstream.NewWriter(stream, runstream.RunStart{
		Mode: runstream.ModeInteractive, SessionID: "test-session", Provider: "fake", Model: "fake",
	}, &errw)
	app.RunStream = w
	return app, stream, &errw, w
}

// steerAgent rebuilds the app's agent with steering enabled and wires the
// app-level steer hooks, mirroring the main.go interactive wiring. The
// returned channel closes when steered input reaches the agent.
func steerAgent(app *App, fp *llmtest.FakeProvider, reg *tools.Registry) chan struct{} {
	if reg == nil {
		reg = tools.Default()
	}
	ag := agent.New(fp, reg, agent.Options{Model: "claude-opus-4-8", Steer: true})
	ag.SetSystem("you are a test")
	ag.SetSleep(func(time.Duration) {})
	app.Agent = ag
	steered := make(chan struct{})
	var once sync.Once
	app.Steer = func(input agent.SteerInput) bool {
		accepted := ag.SteerContent(input)
		if accepted {
			once.Do(func() { close(steered) })
		}
		return accepted
	}
	app.DrainSteer = func() agent.SteerInput { return ag.DrainSteerContent() }
	return steered
}

func runJSONPipe(t *testing.T, app *App) (*io.PipeWriter, chan int) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pr.Close() })
	codeCh := make(chan int, 1)
	go func() { codeCh <- RunJSON(pr, app) }()
	return pw, codeCh
}

func promptSubmitHookRunner(t *testing.T, command string) *hooks.Runner {
	t.Helper()
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
	return &hooks.Runner{Config: cfg}
}

// waitEntryContext signals when Manager.Wait starts selecting. It gives UI
// integration tests the same handoff point as an accepted in-prompt steer.
type waitEntryContext struct {
	context.Context
	once    sync.Once
	entered chan<- struct{}
}

func (c *waitEntryContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func streamTypes(lines []map[string]any) []string {
	var types []string
	for _, line := range lines {
		typ, _ := line["type"].(string)
		types = append(types, typ)
	}
	return types
}

func countType(lines []map[string]any, want string) int {
	n := 0
	for _, line := range lines {
		if line["type"] == want {
			n++
		}
	}
	return n
}

func linesOfType(lines []map[string]any, want string) []map[string]any {
	var out []map[string]any
	for _, line := range lines {
		if line["type"] == want {
			out = append(out, line)
		}
	}
	return out
}

func TestRunJSONTwoPrompts(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("first answer")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 5, OutputTokens: 2}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 6, OutputTokens: 2}},
	)
	app, stream, _, w := newJSONRunApp(t, fp)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"prompt_end\"") }, "first prompt_end")
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"second\"}\n")
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "second prompt_end")
	// Stdin EOF == shutdown.
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	lines := decodeRunStreamLines(t, stream.String())
	// The second prompt message either queues or steers+recovers; either way
	// both prompts run to completion in order.
	if got := countType(lines, "prompt_end"); got != 2 {
		t.Fatalf("prompt_end count = %d, want 2; types=%v", got, streamTypes(lines))
	}
	starts := linesOfType(lines, "prompt_start")
	ends := linesOfType(lines, "prompt_end")
	if len(starts) != 2 {
		t.Fatalf("prompt_start count = %d, want 2; types=%v", len(starts), streamTypes(lines))
	}
	if starts[0]["id"] != "p1" || starts[0]["text"] != "first" || starts[0]["prompt"] != float64(1) {
		t.Fatalf("first prompt_start = %v", starts[0])
	}
	if starts[1]["text"] != "second" || starts[1]["prompt"] != float64(2) {
		t.Fatalf("second prompt_start = %v", starts[1])
	}
	if ends[0]["final_text"] != "first answer" || ends[1]["final_text"] != "second answer" {
		t.Fatalf("prompt_end final texts = %v / %v", ends[0]["final_text"], ends[1]["final_text"])
	}
	users := linesOfType(lines, "user")
	if len(users) != 2 || users[0]["text"] != "first" || users[1]["text"] != "second" {
		t.Fatalf("user events = %v", users)
	}
}

func TestRunJSONSteerInjectsBeforeNextModelRound(t *testing.T) {
	releaseTool := make(chan struct{})
	tool := &blockingTool{name: "probe", ran: make(chan struct{}), release: releaseTool}
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
		llmtest.Step{Events: []llm.StreamEvent{textDelta("steered answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	reg := tools.Default()
	reg.Register(tool)
	steered := steerAgent(app, fp, reg)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"first\"}\n")
	select {
	case <-tool.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not run")
	}
	// The loop is between tool dispatch and the next model request: a bare
	// prompt message now steers into the running prompt.
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"redirect now\"}\n")
	select {
	case <-steered:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt message was not steered into the running prompt")
	}
	close(releaseTool)
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"prompt_end\"") }, "steered prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	if len(fp.Requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(fp.Requests))
	}
	last := fp.Requests[1].Messages[len(fp.Requests[1].Messages)-1]
	var found bool
	for _, b := range last.Content {
		if b.Kind == llm.BlockText && strings.Contains(b.Text, "redirect now") {
			found = true
		}
	}
	if !found || last.Role != llm.RoleUser {
		t.Fatalf("steered input missing from second request's final message: %+v", last)
	}
	lines := decodeRunStreamLines(t, stream.String())
	if got := countType(lines, "prompt_end"); got != 1 {
		t.Fatalf("steered prompt should not start a second prompt: %v", streamTypes(lines))
	}
}

func TestRunJSONSteersRecoveredSeparatelyWithCorrelationIDs(t *testing.T) {
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("first recovered answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second recovered answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	steerAgent(app, fp, nil)
	baseSteer := app.Steer
	accepted := make(chan string, 2)
	app.Steer = func(input agent.SteerInput) bool {
		ok := baseSteer(input)
		if ok {
			accepted <- input.CorrelationID
		}
		return ok
	}

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"first\",\"text\":\"first\"}\n")
	select {
	case <-inPrompt:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	// Both inputs land after the prompt's last model request. Recovery must keep
	// them separate and retain their protocol IDs instead of combining them.
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"s1\",\"text\":\"redirect one\"}\n")
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"s2\",\"text\":\"redirect two\"}\n")
	for _, want := range []string{"s1", "s2"} {
		select {
		case got := <-accepted:
			if got != want {
				t.Fatalf("accepted steer ID = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("steer %q was not accepted", want)
		}
	}
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 3 }, "recovered prompt_end events")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	lines := decodeRunStreamLines(t, stream.String())
	if got := countType(lines, "prompt_end"); got != 3 {
		t.Fatalf("prompt_end count = %d, want 3; types=%v", got, streamTypes(lines))
	}
	users := linesOfType(lines, "user")
	if len(users) != 3 || users[1]["text"] != "redirect one" || users[2]["text"] != "redirect two" {
		t.Fatalf("user events = %v, want two separate recovered prompts", users)
	}
	starts := linesOfType(lines, "prompt_start")
	if len(starts) != 3 || starts[1]["id"] != "s1" || starts[2]["id"] != "s2" {
		t.Fatalf("prompt_start IDs = %v, want recovered IDs s1 then s2", starts)
	}
}

func TestRunJSONInterruptCancelsPrompt(t *testing.T) {
	inPrompt := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(ctx context.Context) { close(inPrompt); <-ctx.Done() },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("after interrupt")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	select {
	case <-inPrompt:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "{\"type\":\"interrupt\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"prompt_end\"") }, "cancelled prompt_end")
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"second\"}\n")
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "second prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	lines := decodeRunStreamLines(t, stream.String())
	ends := linesOfType(lines, "prompt_end")
	if len(ends) != 2 {
		t.Fatalf("prompt_end count = %d, want 2; types=%v", len(ends), streamTypes(lines))
	}
	if ends[0]["termination_reason"] != "cancelled" || ends[0]["exit_code"] != float64(ExitInterrupt) {
		t.Fatalf("interrupted prompt_end = %v", ends[0])
	}
	if ends[1]["final_text"] != "after interrupt" {
		t.Fatalf("second prompt_end = %v, want the session to keep running", ends[1])
	}
}

func TestRunJSONShutdownMidPromptCancelsAndExitsZero(t *testing.T) {
	inPrompt := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Stop:  llm.StopEndTurn,
		Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
		Block: func(ctx context.Context) { close(inPrompt); <-ctx.Done() },
	})
	app, stream, _, w := newJSONRunApp(t, fp)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"first\"}\n")
	select {
	case <-inPrompt:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	select {
	case code := <-codeCh:
		if code != ExitOK {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel the active prompt")
	}
	w.Close(runstream.RunEnd{ExitCode: 0})
	lines := decodeRunStreamLines(t, stream.String())
	ends := linesOfType(lines, "prompt_end")
	if len(ends) != 1 || ends[0]["termination_reason"] != "cancelled" {
		t.Fatalf("prompt_end = %v, want one cancelled", ends)
	}
}

func TestRunJSONBadInputNeverKillsSession(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok answer")},
		Stop:   llm.StopEndTurn,
	})
	app, stream, _, w := newJSONRunApp(t, fp)

	code := RunJSON(strings.NewReader(
		"garbage\n"+
			"{\"type\":\"bogus\",\"id\":\"badtype\"}\n"+
			"{\"type\":\"prompt\",\"id\":\"empty\"}\n"+
			"{\"type\":\"approval_response\",\"id\":\"h9\"}\n"+
			"{\"type\":\"approval_response\",\"id\":\"h9\",\"approve\":true}\n"+
			"{\"type\":\"prompt\",\"id\":\"ok1\",\"text\":\"hi\"}\n"+
			"{\"type\":\"shutdown\"}\n"), app)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	lines := decodeRunStreamLines(t, stream.String())
	errs := linesOfType(lines, "input_error")
	if len(errs) != 5 {
		t.Fatalf("input_error count = %d, want 5 (malformed, unknown type, missing text, missing approve, no pending approval); lines=%v",
			len(errs), streamTypes(lines))
	}
	wantIDs := []any{nil, "badtype", "empty", "h9", "h9"}
	for i, want := range wantIDs {
		if errs[i]["id"] != want {
			t.Fatalf("input_error %d ID = %v, want %v; errors=%v", i, errs[i]["id"], want, errs)
		}
	}
	var sawPrompt bool
	for _, line := range linesOfType(lines, "prompt_start") {
		if line["id"] == "ok1" {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("valid prompt after bad input did not run: %v", streamTypes(lines))
	}
}

func TestRunJSONRejectedSteerAdmissionQueuesPreparedPrompt(t *testing.T) {
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{Stop: llm.StopEndTurn, Block: func(context.Context) { close(inPrompt); <-releaseTurn }},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("queued answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	steerAgent(app, fp, nil)
	rejected := make(chan struct{})
	app.Steer = func(agent.SteerInput) bool {
		close(rejected)
		return false
	}

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	select {
	case <-inPrompt:
	case <-time.After(2 * time.Second):
		t.Fatal("first prompt did not start")
	}
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"must not drop\"}\n")
	select {
	case <-rejected:
	case <-time.After(2 * time.Second):
		t.Fatal("steer admission was not attempted")
	}
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "queued prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	lines := decodeRunStreamLines(t, stream.String())
	starts := linesOfType(lines, "prompt_start")
	if len(starts) != 2 || starts[1]["id"] != "p2" || starts[1]["text"] != "must not drop" {
		t.Fatalf("prompt starts = %v, want rejected steer queued as p2", starts)
	}
}

func TestRunJSONCancelledPromptDoesNotReusePriorFinalText(t *testing.T) {
	secondStarted := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("old answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Block: func(ctx context.Context) { close(secondStarted); <-ctx.Done() }},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 1 }, "first prompt_end")
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"second\"}\n")
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second prompt did not start")
	}
	writePipe(t, pw, "{\"type\":\"interrupt\"}\n")
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "cancelled prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	w.Close(runstream.RunEnd{ExitCode: code})

	ends := linesOfType(decodeRunStreamLines(t, stream.String()), "prompt_end")
	if len(ends) != 2 || ends[0]["final_text"] != "old answer" {
		t.Fatalf("prompt ends = %v", ends)
	}
	if got, exists := ends[1]["final_text"]; exists && got != "" {
		t.Fatalf("cancelled prompt final_text = %v, want empty rather than prior answer; end=%v", got, ends[1])
	}
}

func TestRunJSONForceExitCancelsPromptSubmitHook(t *testing.T) {
	fp := llmtest.New("fake")
	app, stream, _, w := newJSONRunApp(t, fp)
	marker := filepath.Join(t.TempDir(), "hook-started")
	app.Hooks = promptSubmitHookRunner(t, fmt.Sprintf("printf started > %q; sleep 600", marker))
	forceExit := make(chan struct{})
	app.ForceExit = forceExit
	var forceOnce sync.Once
	stopForceExit := func() { forceOnce.Do(func() { close(forceExit) }) }
	t.Cleanup(stopForceExit)

	pw, codeCh := runJSONPipe(t, app)
	t.Cleanup(func() { _ = pw.Close() })
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	waitFor(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, "JSON prompt-submit hook start")
	stopForceExit()

	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("force exit during prompt-submit hook = %d, want %d", code, ExitInterrupt)
	}
	if err := w.Close(runstream.RunEnd{ExitCode: ExitInterrupt}); err != nil {
		t.Fatalf("close run stream: %v", err)
	}
	if app.PromptNumber != 0 {
		t.Fatalf("interrupted hook recorded %d prompts, want 0", app.PromptNumber)
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider turns = %d, want 0", fp.RequestCount())
	}
	if strings.Contains(stream.String(), `"type":"prompt_start"`) {
		t.Fatalf("stream unexpectedly admitted prompt: %s", stream.String())
	}
}

func TestRunJSONForceExitCancelsSteerPromptSubmitHook(t *testing.T) {
	providerStarted := make(chan struct{})
	releaseProvider := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{Block: func(context.Context) {
		close(providerStarted)
		<-releaseProvider
	}})
	app, stream, _, w := newJSONRunApp(t, fp)
	steerAgent(app, fp, nil)
	marker := filepath.Join(t.TempDir(), "steer-hook-started")
	app.Hooks = promptSubmitHookRunner(t, fmt.Sprintf("if grep -q '\"prompt\":\"steer\"'; then printf started > %q; sleep 600; else printf '{}'; fi", marker))
	forceExit := make(chan struct{})
	app.ForceExit = forceExit
	var forceOnce, releaseOnce sync.Once
	stopForceExit := func() { forceOnce.Do(func() { close(forceExit) }) }
	release := func() { releaseOnce.Do(func() { close(releaseProvider) }) }
	t.Cleanup(func() {
		stopForceExit()
		release()
	})

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(pr)}
	codeCh := make(chan int, 1)
	go func() { codeCh <- d.run() }()
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		t.Fatal("initial prompt did not reach provider")
	}
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"s1\",\"text\":\"steer\"}\n")
	waitFor(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, "JSON steer prompt-submit hook start")
	stopForceExit()

	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("force exit during steer hook = %d, want %d", code, ExitInterrupt)
	}
	release()
	select {
	case <-d.done:
	case <-time.After(time.Second):
		t.Fatal("initial prompt did not finish after provider release")
	}
	if err := w.Close(runstream.RunEnd{ExitCode: ExitInterrupt}); err != nil {
		t.Fatalf("close run stream: %v", err)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider turns = %d, want only the initial prompt", fp.RequestCount())
	}
	if strings.Contains(stream.String(), `"id":"s1"`) {
		t.Fatalf("stream unexpectedly admitted steered prompt: %s", stream.String())
	}
}

func TestRunJSONForceExitCancelsInitialMCPRefresh(t *testing.T) {
	fp := llmtest.New("fake")
	app, _, _, w := newJSONRunApp(t, fp)
	forceExit := make(chan struct{}, 1)
	app.ForceExit = forceExit
	refreshStarted := make(chan struct{})
	app.RefreshMCP = func(ctx context.Context, _ string) (*tools.Registry, string) {
		close(refreshStarted)
		<-ctx.Done()
		return nil, ""
	}

	done := make(chan int, 1)
	go func() { done <- RunJSON(strings.NewReader(""), app) }()
	<-refreshStarted
	forceExit <- struct{}{}
	select {
	case code := <-done:
		if code != ExitInterrupt {
			t.Fatalf("force exit during initial MCP refresh = %d, want %d", code, ExitInterrupt)
		}
	case <-time.After(time.Second):
		t.Fatal("force exit did not cancel initial MCP refresh")
	}
	if err := w.Close(runstream.RunEnd{ExitCode: ExitInterrupt}); err != nil {
		t.Fatalf("close run stream: %v", err)
	}
}

func TestRunJSONDetachedWaitCompletionStartsContinuation(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("continuation answer")},
		Stop:   llm.StopEndTurn,
	})
	app, stream, _, w := newJSONRunApp(t, fp)
	manager := background.NewManager(background.Options{})
	app.Background = manager

	startedRun := make(chan struct{})
	release := make(chan struct{})
	job, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			close(startedRun)
			<-release
			return tools.BackgroundJobResult{Text: "completed detached work"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	<-startedRun

	entered := make(chan struct{})
	waited := make(chan background.WaitResult, 1)
	waitErr := make(chan error, 1)
	go func() {
		result, waitErrResult := manager.Wait(&waitEntryContext{Context: context.Background(), entered: entered}, job.ID, time.Minute)
		waited <- result
		waitErr <- waitErrResult
	}()
	<-entered
	manager.NotifyAcceptedSteer()
	select {
	case result := <-waited:
		if err := <-waitErr; err != nil {
			t.Fatalf("detach wait: %v", err)
		}
		if !result.Detached {
			t.Fatalf("wait result = %+v, want detached", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted steer did not detach wait")
	}

	pw, codeCh := runJSONPipe(t, app)
	close(release)
	waitFor(t, func() bool { return strings.Contains(stream.String(), `"cause":"detached_background_wait"`) }, "detached continuation prompt_start")
	waitFor(t, func() bool { return fp.RequestCount() == 1 }, "detached continuation model request")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := waitRun(t, codeCh)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	if fp.RequestCount() != 1 {
		t.Fatalf("model requests = %d, want one continuation", fp.RequestCount())
	}
	if got := strings.Join(fp.Requests[0].RequestContext, "\n"); !strings.Contains(got, "[detached background wait ") || !strings.Contains(got, "completed detached work") {
		t.Fatalf("continuation request context = %q", got)
	}
	lines := decodeRunStreamLines(t, stream.String())
	starts := linesOfType(lines, "prompt_start")
	ends := linesOfType(lines, "prompt_end")
	if len(starts) != 1 || starts[0]["cause"] != "detached_background_wait" || starts[0]["id"] != nil {
		t.Fatalf("continuation prompt_start = %v", starts)
	}
	if len(ends) != 1 || ends[0]["cause"] != "detached_background_wait" || ends[0]["final_text"] != "continuation answer" {
		t.Fatalf("continuation prompt_end = %v", ends)
	}
}

func TestRunJSONInitialBoundaryRefreshesMCPAndPersistsNotice(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn})
	app, stream, _, w := newJSONRunApp(t, fp)
	app.AgentName = "auto"
	manager := background.NewManager(background.Options{})
	job, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{
		Kind: "shell",
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{Text: "background result"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	if _, err := manager.Wait(context.Background(), job.ID, time.Second); err != nil {
		t.Fatalf("wait for background job: %v", err)
	}
	app.Background = manager
	refreshed := &tools.Registry{}
	refreshed.Register(mcpRefreshTool{name: "mcp__test__initial"})
	calls := 0
	const notice = "[mcp: tool list updated; 1 tools]"
	app.RefreshMCP = func(context.Context, string) (*tools.Registry, string) {
		calls++
		return refreshed, notice
	}

	code := RunJSON(strings.NewReader("{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"hello\"}\n"), app)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	if calls != 1 {
		t.Fatalf("RefreshMCP calls = %d, want one initial boundary", calls)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	var advertised bool
	for _, tool := range fp.Requests[0].Tools {
		advertised = advertised || tool.Name == "mcp__test__initial"
	}
	if !advertised {
		t.Fatalf("initially refreshed tool was not advertised: %+v", fp.Requests[0].Tools)
	}
	lines := decodeRunStreamLines(t, stream.String())
	var noticeIndex, promptIndex = -1, -1
	for i, line := range lines {
		switch line["type"] {
		case "notice":
			if line["text"] == notice {
				noticeIndex = i
			}
		case "prompt_start":
			promptIndex = i
		}
	}
	if noticeIndex < 0 || promptIndex < 0 || noticeIndex > promptIndex {
		t.Fatalf("initial MCP notice must precede prompt_start; types=%v", streamTypes(lines))
	}
	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw.ndjson: %v", err)
	}
	if !strings.Contains(string(raw), notice) || !strings.Contains(string(raw), "[background:") {
		t.Fatalf("raw.ndjson omitted streamed boundary notices:\n%s", raw)
	}
}

func TestRunJSONPromptPreparationRejectionReturnsCorrelatedInputError(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("valid answer")}, Stop: llm.StopEndTurn})
	app, stream, _, w := newJSONRunApp(t, fp)
	app.Skills = map[string]skills.Skill{
		"known": {Name: "known", Location: "/skills/known/SKILL.md"},
	}
	code := RunJSON(strings.NewReader(
		"{\"type\":\"prompt\",\"id\":\"bad\",\"text\":\"$missing\"}\n"+
			"{\"type\":\"prompt\",\"id\":\"good\",\"text\":\"hello\"}\n"), app)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	lines := decodeRunStreamLines(t, stream.String())
	errs := linesOfType(lines, "input_error")
	if len(errs) != 1 || errs[0]["id"] != "bad" || !strings.Contains(errs[0]["message"].(string), "skill resolution") {
		t.Fatalf("input errors = %v, want correlated preparation rejection", errs)
	}
	starts := linesOfType(lines, "prompt_start")
	if len(starts) != 1 || starts[0]["id"] != "good" {
		t.Fatalf("prompt starts = %v, want only accepted prompt", starts)
	}
}

// newHandoffJSONApp wires the handoff machinery: a shared Pending holder, a
// successful agent-switch stub, and the JSON run stream.
func newHandoffJSONApp(t *testing.T, fp *llmtest.FakeProvider, pending *handoff.Pending) (*App, *lockedBuffer, *lockedBuffer, *runstream.Writer) {
	t.Helper()
	app, stream, errw, w := newJSONRunApp(t, fp)
	readyPlanForApp(t, app, "Implement structured handoff")
	app.Handoff = pending
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}
	return app, stream, errw, w
}

func handoffPlanStep(pending *handoff.Pending) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{textDelta("plan ready")},
		Stop:   llm.StopEndTurn,
		Block: func(context.Context) {
			pending.Request(handoff.Request{PlanPath: "/p/0001.plan.md"})
		},
	}
}

func TestRunJSONPromptWithUnknownAgentRejected(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("still here")},
		Stop:   llm.StopEndTurn,
	})
	app, stream, _, w := newJSONRunApp(t, fp)
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{}, fmt.Errorf("unknown agent %q", name)
	}

	code := RunJSON(strings.NewReader(
		"{\"type\":\"prompt\",\"agent\":\"bogus\",\"text\":\"hi\"}\n"+
			"{\"type\":\"prompt\",\"text\":\"hello\"}\n"), app)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	lines := decodeRunStreamLines(t, stream.String())
	errs := linesOfType(lines, "input_error")
	if len(errs) != 1 || !strings.Contains(errs[0]["message"].(string), "agent switch failed") {
		t.Fatalf("input_error events = %v", errs)
	}
	if got := countType(lines, "prompt_start"); got != 1 {
		t.Fatalf("prompt_start count = %d, want 1 (the rejected prompt never ran); types=%v", got, streamTypes(lines))
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("model requests = %d, want 1 (bogus agent made no request)", fp.RequestCount())
	}
}

func TestRunJSONHandoffApprovalStartsImplementation(t *testing.T) {
	pending := handoff.NewPending()
	fp := llmtest.New("fake",
		handoffPlanStep(pending),
		llmtest.Step{Events: []llm.StreamEvent{textDelta("implemented")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newHandoffJSONApp(t, fp, pending)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"make a plan\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"approval_request\"") }, "approval request")

	lines := decodeRunStreamLines(t, stream.String())
	types := streamTypes(lines)
	if types[len(types)-1] != "approval_request" {
		t.Fatalf("approval_request should follow the first prompt_end: %v", types)
	}
	approvals := linesOfType(lines, "approval_request")
	if len(approvals) != 1 {
		t.Fatalf("approval_request count = %d, want 1", len(approvals))
	}
	approval := approvals[0]
	latest, ok := app.Plans.Latest()
	if !ok {
		t.Fatal("test app has no recorded plan")
	}
	if approval["kind"] != "implementation_handoff" ||
		approval["plan_path"] != latest.Path || approval["agent"] != "auto" || approval["id"] == nil || approval["work_id"] != nil || approval["revision_id"] != nil {
		t.Fatalf("approval_request = %v", approval)
	}
	id, _ := approval["id"].(string)
	writePipe(t, pw, "{\"type\":\"approval_response\",\"id\":\""+id+"\",\"approve\":true}\n")
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "implementation prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	if got := transcriptPrompts(app); !strings.Contains(got, implementationStartPrompt) {
		t.Fatalf("implementation prompt missing from transcript prompts %q", got)
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("model requests = %d, want plan + implementation", fp.RequestCount())
	}
	lines = decodeRunStreamLines(t, stream.String())
	var sawImplStart bool
	for _, line := range linesOfType(lines, "prompt_start") {
		if line["text"] == implementationStartPrompt {
			sawImplStart = true
		}
	}
	if !sawImplStart {
		t.Fatalf("no prompt_start for the implementation prompt: %v", streamTypes(lines))
	}
}

func TestRunJSONHandoffApprovalDeclined(t *testing.T) {
	pending := handoff.NewPending()
	fp := llmtest.New("fake",
		handoffPlanStep(pending),
		llmtest.Step{Events: []llm.StreamEvent{textDelta("later answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, errw, w := newHandoffJSONApp(t, fp, pending)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"make a plan\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"approval_request\"") }, "approval request")
	// A prompt while approval is pending is rejected.
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"wait\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "approval pending") }, "pending-approval input error")
	writePipe(t, pw, "{\"type\":\"approval_response\",\"id\":\"h1\",\"approve\":false}\n")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[handoff cancelled]") }, "cancellation notice")
	if strings.Contains(stream.String(), implementationStartPrompt) {
		t.Fatal("declined handoff should not start the implementation prompt")
	}
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"after decline\"}\n")
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "post-decline prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	if strings.Contains(transcriptPrompts(app), implementationStartPrompt) {
		t.Fatalf("declined handoff should not submit implementation prompt: %q", transcriptPrompts(app))
	}
}

func TestRunJSONInterruptDuringApprovalCancelsHandoff(t *testing.T) {
	pending := handoff.NewPending()
	fp := llmtest.New("fake", handoffPlanStep(pending))
	app, stream, errw, w := newHandoffJSONApp(t, fp, pending)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"make a plan\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"approval_request\"") }, "approval request")

	// Interrupt with a pending approval cancels the handoff and exits 130,
	// mirroring the TTY approval Ctrl-C path; it never auto-approves.
	writePipe(t, pw, "{\"type\":\"interrupt\"}\n")
	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("exit code = %d, want %d (130)", code, ExitInterrupt)
	}
	w.Close(runstream.RunEnd{ExitCode: ExitInterrupt})
	if !strings.Contains(errw.String(), "[handoff cancelled]") {
		t.Fatalf("missing cancellation notice: %q", errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("model requests = %d, want plan only (no auto-approved implementation)", fp.RequestCount())
	}
	if strings.Contains(transcriptPrompts(app), implementationStartPrompt) {
		t.Fatalf("interrupted handoff should not submit implementation prompt: %q", transcriptPrompts(app))
	}
}

func TestRunJSONEOFAfterCompletionDeclinesPendingApprovalAndDrainsQueue(t *testing.T) {
	pending := handoff.NewPending()
	fp := llmtest.New("fake",
		handoffPlanStep(pending),
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, errw, w := newHandoffJSONApp(t, fp, pending)
	// Hold the first prompt's completion so the second prompt message is
	// buffered at the completion boundary and queues behind the approval.
	finishing := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	app.OnPromptFinished = func() { once.Do(func() { close(finishing); <-proceed }) }

	pr, pw := io.Pipe()
	defer pr.Close()
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(pr)}
	codeCh := make(chan int, 1)
	go func() { codeCh <- d.run() }()

	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"make a plan\"}\n")
	<-finishing
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"second\"}\n")
	waitFor(t, func() bool { return len(d.msgs) >= 1 }, "second prompt buffered at completion")
	close(proceed)
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"approval_request\"") }, "approval request")

	// Stdin EOF with a pending approval declines the handoff (never
	// auto-approves) and drains the queued prompt before exiting 0.
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: ExitOK})
	if !strings.Contains(errw.String(), "[handoff cancelled]") {
		t.Fatalf("missing decline notice: %q", errw.String())
	}
	lines := decodeRunStreamLines(t, stream.String())
	starts := linesOfType(lines, "prompt_start")
	if len(starts) != 2 || starts[0]["id"] != "p1" || starts[1]["id"] != "p2" {
		t.Fatalf("prompt starts = %v, want p1 then the drained p2", starts)
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("model requests = %d, want plan + drained prompt (no implementation)", fp.RequestCount())
	}
	if strings.Contains(transcriptPrompts(app), implementationStartPrompt) {
		t.Fatalf("EOF-declined handoff should not submit implementation prompt: %q", transcriptPrompts(app))
	}
}

func TestRunJSONShutdownAtCompletionDiscardsQueuedPrompt(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("first answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	finishing := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	app.OnPromptFinished = func() { once.Do(func() { close(finishing); <-proceed }) }

	pr, pw := io.Pipe()
	defer pr.Close()
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(pr)}
	codeCh := make(chan int, 1)
	go func() { codeCh <- d.run() }()

	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	<-finishing
	// Shutdown races prompt completion: it must be handled before the
	// boundary can start the queued second prompt.
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"second\"}\n")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	waitFor(t, func() bool { return len(d.msgs) >= 2 }, "prompt+shutdown buffered at completion")
	close(proceed)

	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: ExitOK})
	lines := decodeRunStreamLines(t, stream.String())
	starts := linesOfType(lines, "prompt_start")
	if len(starts) != 1 || starts[0]["id"] != "p1" {
		t.Fatalf("prompt starts = %v, want p1 only (shutdown discards the queued prompt)", starts)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("model requests = %d, want 1 (queued prompt never ran)", fp.RequestCount())
	}
}

func TestRunJSONInterruptAtCompletionExits130InsteadOfStealingNextPrompt(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("first answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	finishing := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	app.OnPromptFinished = func() { once.Do(func() { close(finishing); <-proceed }) }

	pr, pw := io.Pipe()
	defer pr.Close()
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(pr)}
	codeCh := make(chan int, 1)
	go func() { codeCh <- d.run() }()

	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	<-finishing
	// Interrupt races prompt completion: the prompt it targeted is already
	// finished, so it stops the run instead of cancelling the next prompt.
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p2\",\"text\":\"second\"}\n")
	writePipe(t, pw, "{\"type\":\"interrupt\"}\n")
	waitFor(t, func() bool { return len(d.msgs) >= 2 }, "prompt+interrupt buffered at completion")
	close(proceed)

	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("exit code = %d, want %d (130)", code, ExitInterrupt)
	}
	w.Close(runstream.RunEnd{ExitCode: ExitInterrupt})
	lines := decodeRunStreamLines(t, stream.String())
	starts := linesOfType(lines, "prompt_start")
	if len(starts) != 1 || starts[0]["id"] != "p1" {
		t.Fatalf("prompt starts = %v, want p1 only (interrupt must not steal the next prompt)", starts)
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("model requests = %d, want 1 (queued prompt never ran)", fp.RequestCount())
	}
}

func TestRunJSONSteerFallbackKeepsSubmissionOrder(t *testing.T) {
	release := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{Stop: llm.StopEndTurn, Block: func(context.Context) { <-release }},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("alpha answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("bravo answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	steerAgent(app, fp, nil)
	base := app.Steer
	first := true
	app.Steer = func(input agent.SteerInput) bool {
		if first {
			first = false
			return false // alpha: behave like a full steer queue
		}
		return base(input)
	}

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"prompt_start\"") }, "first prompt_start")
	// alpha queues when steering refuses it; bravo submits after, so the
	// steer gate must queue it behind alpha rather than letting it steer and
	// recover first at completion.
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"a\",\"text\":\"alpha\"}\n")
	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"b\",\"text\":\"bravo\"}\n")
	writePipe(t, pw, "{bogus\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"input_error\"") }, "barrier input_error")
	close(release)
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 3 }, "all prompt ends")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	if code := waitRun(t, codeCh); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: ExitOK})

	if fp.RequestCount() != 3 {
		t.Fatalf("model requests = %d, want first + alpha + bravo", fp.RequestCount())
	}
	lastUserText := func(r llm.Request) string {
		for i := len(r.Messages) - 1; i >= 0; i-- {
			m := r.Messages[i]
			if m.Role == llm.RoleUser && len(m.Content) > 0 {
				return m.Content[len(m.Content)-1].Text
			}
		}
		return ""
	}
	if got := lastUserText(fp.Requests[1]); got != "alpha" {
		t.Fatalf("second request prompt = %q, want alpha (submitted first)", got)
	}
	if got := lastUserText(fp.Requests[2]); got != "bravo" {
		t.Fatalf("third request prompt = %q, want bravo (submitted second)", got)
	}
}

func TestRunJSONForceExitAbortsBlockedInputReader(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("first answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	forceExit := make(chan struct{})
	app.ForceExit = forceExit
	finishing := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	app.OnPromptFinished = func() { once.Do(func() { close(finishing); <-proceed }) }

	pr, pw := io.Pipe()
	defer pr.Close()
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(pr)}
	codeCh := make(chan int, 1)
	go func() { codeCh <- d.run() }()

	writePipe(t, pw, "{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"first\"}\n")
	<-finishing
	// Overfill the input buffer while the run loop is parked in prompt
	// completion: the reader blocks on send with the buffer full.
	var buf strings.Builder
	for i := 0; i < 40; i++ {
		buf.WriteString("{\"type\":\"prompt\",\"text\":\"extra\"}\n")
	}
	writePipe(t, pw, buf.String())
	waitFor(t, func() bool { return len(d.msgs) == cap(d.msgs) }, "input buffer full")
	close(forceExit)
	close(proceed)

	if code := waitRun(t, codeCh); code != ExitInterrupt {
		t.Fatalf("exit code = %d, want %d (130)", code, ExitInterrupt)
	}
	w.Close(runstream.RunEnd{ExitCode: ExitInterrupt})
	_ = stream
	// The reader goroutine must abort its blocked send on force-exit and
	// close the message channel instead of leaking.
	waitFor(t, func() bool {
		select {
		case _, ok := <-d.msgs:
			return !ok
		default:
			return false
		}
	}, "input reader aborted after force-exit")
}

func TestRunJSONUnknownApprovalIDRejected(t *testing.T) {
	pending := handoff.NewPending()
	fp := llmtest.New("fake", handoffPlanStep(pending))
	app, stream, errw, w := newHandoffJSONApp(t, fp, pending)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"make a plan\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"approval_request\"") }, "approval request")
	writePipe(t, pw, "{\"type\":\"approval_response\",\"id\":\"nope\",\"approve\":true}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "unknown approval id") }, "unknown-id input error")
	// The pending approval survives a wrong id.
	writePipe(t, pw, "{\"type\":\"approval_response\",\"id\":\"h1\",\"approve\":false}\n")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[handoff cancelled]") }, "cancellation notice")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	if got := countType(decodeRunStreamLines(t, stream.String()), "prompt_start"); got != 1 {
		t.Fatalf("prompt_start count = %d, want 1 (no implementation prompt)", got)
	}
}

func TestRunJSONHandoffSwitchFailureSurfaces(t *testing.T) {
	pending := handoff.NewPending()
	fp := llmtest.New("fake", handoffPlanStep(pending))
	app, stream, errw, w := newJSONRunApp(t, fp)
	readyPlanForApp(t, app, "Implement structured handoff")
	app.Handoff = pending
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{}, errors.New("no such agent")
	}

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"make a plan\"}\n")
	waitFor(t, func() bool { return strings.Contains(stream.String(), "\"type\":\"approval_request\"") }, "approval request")
	writePipe(t, pw, "{\"type\":\"approval_response\",\"id\":\"h1\",\"approve\":true}\n")
	waitFor(t, func() bool { return strings.Contains(errw.String(), "[handoff failed:") }, "handoff failure notice")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	if strings.Contains(transcriptPrompts(app), implementationStartPrompt) {
		t.Fatal("failed handoff must not start the implementation prompt")
	}
}
