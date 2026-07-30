package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/plan"
	"harness/internal/runstream"
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
	app.Steer = func(input agent.SteerInput) {
		ag.SteerContent(input)
		once.Do(func() { close(steered) })
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

func TestRunJSONSteerRecoveredAsNextPrompt(t *testing.T) {
	inPrompt := make(chan struct{})
	releaseTurn := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Stop:  llm.StopEndTurn,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 2},
			Block: func(context.Context) { close(inPrompt); <-releaseTurn },
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("recovered answer")}, Stop: llm.StopEndTurn},
	)
	app, stream, _, w := newJSONRunApp(t, fp)
	steerAgent(app, fp, nil)

	pw, codeCh := runJSONPipe(t, app)
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"first\"}\n")
	select {
	case <-inPrompt:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	// Steering lands after the last model request of the prompt: it must be
	// recovered as the next prompt, matching TTY REPL semantics.
	writePipe(t, pw, "{\"type\":\"prompt\",\"text\":\"redirect later\"}\n")
	close(releaseTurn)
	waitFor(t, func() bool { return strings.Count(stream.String(), "\"type\":\"prompt_end\"") >= 2 }, "recovered prompt_end")
	writePipe(t, pw, "{\"type\":\"shutdown\"}\n")
	code := <-codeCh
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})

	lines := decodeRunStreamLines(t, stream.String())
	if got := countType(lines, "prompt_end"); got != 2 {
		t.Fatalf("prompt_end count = %d, want 2 (steer recovered as next prompt); types=%v",
			got, streamTypes(lines))
	}
	users := linesOfType(lines, "user")
	if len(users) != 2 || users[1]["text"] != "redirect later" {
		t.Fatalf("user events = %v, want the recovered steer as the second prompt", users)
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
			"{\"type\":\"bogus\"}\n"+
			"{\"type\":\"prompt\"}\n"+
			"{\"type\":\"approval_response\",\"id\":\"h9\"}\n"+
			"{\"type\":\"approval_response\",\"id\":\"h9\",\"approve\":true}\n"+
			"{\"type\":\"prompt\",\"id\":\"ok1\",\"text\":\"hi\"}\n"+
			"{\"type\":\"shutdown\"}\n"), app)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	w.Close(runstream.RunEnd{ExitCode: code})
	lines := decodeRunStreamLines(t, stream.String())
	if got := countType(lines, "input_error"); got != 5 {
		t.Fatalf("input_error count = %d, want 5 (malformed, unknown type, missing text, missing approve, no pending approval); lines=%v",
			got, streamTypes(lines))
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

// newHandoffJSONApp wires the handoff machinery: a shared Pending holder, a
// successful agent-switch stub, and the JSON run stream.
func newHandoffJSONApp(t *testing.T, fp *llmtest.FakeProvider, pending *plan.Pending) (*App, *lockedBuffer, *lockedBuffer, *runstream.Writer) {
	t.Helper()
	app, stream, errw, w := newJSONRunApp(t, fp)
	app.Handoff = pending
	app.SwitchAgent = func(name string) (AgentSelection, error) {
		return AgentSelection{Name: name, Tools: tools.Default(), System: "impl"}, nil
	}
	return app, stream, errw, w
}

func handoffPlanStep(pending *plan.Pending) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{textDelta("plan ready")},
		Stop:   llm.StopEndTurn,
		Block: func(context.Context) {
			pending.Request(plan.HandoffRequest{Brief: "env: go test", PlanPath: "/p/0001.plan.md"})
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
	pending := plan.NewPending()
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
	if approval["kind"] != "implementation_handoff" || approval["brief"] != "env: go test" ||
		approval["plan_path"] != "/p/0001.plan.md" || approval["agent"] != "auto" || approval["id"] == nil {
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
	pending := plan.NewPending()
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

func TestRunJSONUnknownApprovalIDRejected(t *testing.T) {
	pending := plan.NewPending()
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
	pending := plan.NewPending()
	fp := llmtest.New("fake", handoffPlanStep(pending))
	app, stream, errw, w := newJSONRunApp(t, fp)
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
