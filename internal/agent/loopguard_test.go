package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

// countUserMessagesContaining counts transcript user text blocks containing sub.
func countUserMessagesContaining(msgs []llm.Message, sub string) int {
	n := 0
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockText && strings.Contains(b.Text, sub) {
				n++
			}
		}
	}
	return n
}

func toolUseStep() llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "probe", `{}`)},
		Stop:   llm.StopToolUse,
	}
}

func TestTurnBudgetClosureReserve(t *testing.T) {
	tests := []struct {
		maxTurns int
		reserve  int
	}{
		{maxTurns: 0, reserve: 0},
		{maxTurns: 1, reserve: 1},
		{maxTurns: 2, reserve: 1},
		{maxTurns: 3, reserve: 2},
		{maxTurns: 20, reserve: 2},
		{maxTurns: 21, reserve: 3},
		{maxTurns: 250, reserve: 25},
		{maxTurns: 1_000, reserve: 25},
	}
	for _, tt := range tests {
		if got := turnBudgetClosureReserve(tt.maxTurns); got != tt.reserve {
			t.Errorf("turnBudgetClosureReserve(%d) = %d, want %d", tt.maxTurns, got, tt.reserve)
		}
	}
	if !shouldEnterTurnBudgetClosure(3, 1) {
		t.Error("three-turn budget should enter closure after the first completed turn")
	}
	if shouldEnterTurnBudgetClosure(3, 0) {
		t.Error("three-turn budget entered closure before only two turns remained")
	}
}

func TestOrientationGuardSteersToBatching(t *testing.T) {
	var guard turnGuard
	results := []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultText: "ok"}}
	for i := 0; i < orientationSteerThreshold; i++ {
		input := json.RawMessage(fmt.Sprintf(`{"path":"file-%d.go"}`, i))
		guard.recordTools([]llm.ToolCall{{Name: "read_file", Input: input}}, results)
	}
	if got := guard.steerMessage(); !strings.Contains(got, "Coissue") || !strings.Contains(got, "paths[]") {
		t.Fatalf("orientation steer = %q", got)
	}
	if got := guard.steerMessage(); got != "" {
		t.Fatalf("orientation steer repeated: %q", got)
	}
}

func TestOrientationGuardIgnoresBatchedLookups(t *testing.T) {
	for _, call := range []llm.ToolCall{
		{Name: "read_file", Input: json.RawMessage(`{"paths":["a","b"]}`)},
	} {
		if isSingleOrientationTurn([]llm.ToolCall{call}) {
			t.Errorf("%s batched call classified as single orientation lookup", call.Name)
		}
	}
	searches := []llm.ToolCall{
		{Name: "search", Input: json.RawMessage(`{"pattern":"a"}`)},
		{Name: "search", Input: json.RawMessage(`{"pattern":"b"}`)},
	}
	if isSingleOrientationTurn(searches) {
		t.Fatal("coissued searches classified as one sequential orientation lookup")
	}
}

func TestSemanticInspectionStagnationSteersOnceWithoutStopping(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "read_file", readOnly: true})
	var guard turnGuard
	var batching, phase int
	for i := 0; i < semanticSteerThreshold+5; i++ {
		calls := []llm.ToolCall{{Name: "read_file", Input: json.RawMessage(fmt.Sprintf(`{"path":"file-%d.go"}`, i))}}
		results := []llm.ContentBlock{{Kind: llm.BlockToolResult, ToolName: "read_file", ResultText: fmt.Sprintf("evidence-%d", i)}}
		progress := guard.aggregateTurnProgress(reg, i+1, calls, results)
		guard.recordTurn(calls, results, &progress)
		reason, _ := guard.nextSteer()
		switch reason {
		case GuardSteerBatching:
			batching++
		case GuardSteerPhaseTransition:
			phase++
		}
		if guard.shouldBreakErrors() || guard.shouldBreakRepeat() {
			t.Fatal("semantic inspection streak triggered a hard stop")
		}
	}
	if batching != 1 || phase != 1 {
		t.Fatalf("steers: batching=%d phase=%d, want one each", batching, phase)
	}
	if guard.semanticRuns != semanticSteerThreshold+5 {
		t.Fatalf("semantic streak = %d, want %d", guard.semanticRuns, semanticSteerThreshold+5)
	}
}

func TestSemanticProgressSignalsResetInspectionStreak(t *testing.T) {
	reg := tools.Default()
	reg.Register(&recordTool{name: "read_file", readOnly: true})
	reg.Register(&recordTool{name: "edit", readOnly: false})
	var guard turnGuard
	inspect := []llm.ToolCall{{Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)}}
	okResult := []llm.ContentBlock{{Kind: llm.BlockToolResult, ToolName: "read_file", ResultText: "evidence"}}
	for i := 0; i < 4; i++ {
		progress := guard.aggregateTurnProgress(reg, i+1, inspect, okResult)
		guard.recordTurn(inspect, okResult, &progress)
	}

	mutation := []llm.ToolCall{{Name: "edit", Input: json.RawMessage(`{}`)}}
	failedMutation := []llm.ContentBlock{{Kind: llm.BlockToolResult, ToolName: "edit", ResultText: "failed", ResultError: true}}
	progress := guard.aggregateTurnProgress(reg, 5, mutation, failedMutation)
	if progress.SuccessfulMutation || progress.ExplicitProgress {
		t.Fatalf("failed mutation claimed progress: %+v", progress)
	}
	guard.recordTurn(mutation, failedMutation, &progress)
	if guard.semanticRuns != 0 {
		t.Fatalf("mutation attempt left inspection streak at %d", guard.semanticRuns)
	}

	verification := []llm.ToolCall{{Name: "run_command", Input: json.RawMessage(`{"argv":["go","test","./..."]}`)}}
	failedVerification := []llm.ContentBlock{{Kind: llm.BlockToolResult, ToolName: "run_command", ResultText: "tests failed", ResultError: true}}
	progress = guard.aggregateTurnProgress(reg, 6, verification, failedVerification)
	if !progress.VerificationAttempt || progress.SuccessfulVerification || !progress.ExplicitProgress {
		t.Fatalf("failed verification progress = %+v", progress)
	}
	guard.recordTurn(verification, failedVerification, &progress)

	progress = TurnProgress{}
	guard.semanticRuns = 7
	guard.semanticSteered = true
	guard.resetForUserSteer(&progress)
	if guard.semanticRuns != 0 || !progress.UserSteer {
		t.Fatalf("user steer did not reset semantic streak: guard=%+v progress=%+v", guard, progress)
	}
}

func TestBoundedNewEvidenceDetectionIncludesRichContentDigest(t *testing.T) {
	var evidence boundedEvidence
	base := llm.ContentBlock{Kind: llm.BlockToolResult, ToolName: "view_image", ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "first"}}}
	if !evidence.add(toolResultEvidence(base)) || evidence.add(toolResultEvidence(base)) {
		t.Fatal("identical rich result was not recognized as existing evidence")
	}
	base.ResultContent[0].ImageData = "second"
	if !evidence.add(toolResultEvidence(base)) {
		t.Fatal("changed rich image digest was not recognized as new evidence")
	}
	for i := 0; i < maxEvidenceSignatures+20; i++ {
		result := llm.ContentBlock{Kind: llm.BlockToolResult, ToolName: "probe", ResultText: fmt.Sprintf("result-%d", i)}
		evidence.add(toolResultEvidence(result))
	}
	if len(evidence.seen) != maxEvidenceSignatures || len(evidence.order) != maxEvidenceSignatures {
		t.Fatalf("evidence window sizes = %d/%d, want %d", len(evidence.seen), len(evidence.order), maxEvidenceSignatures)
	}
}

func TestSemanticProgressEventDoesNotEnterTranscript(t *testing.T) {
	tool := &recordTool{name: "read_file", readOnly: true, run: func(_ context.Context, input json.RawMessage) (string, error) {
		return string(input), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)
	steps := make([]llmtest.Step, 0, semanticSteerThreshold+1)
	for i := 0; i < semanticSteerThreshold; i++ {
		steps = append(steps, llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, fmt.Sprintf("id-%d", i), "read_file", fmt.Sprintf(`{"path":"file-%d.go"}`, i))},
			Stop:   llm.StopToolUse,
		})
	}
	steps = append(steps, textStep("done"))
	a := newAgent(llmtest.New("fake", steps...), reg, Options{MaxTurns: semanticSteerThreshold + 2})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "inspect", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.progress) != semanticSteerThreshold {
		t.Fatalf("progress events = %d, want %d", len(sink.progress), semanticSteerThreshold)
	}
	if got := countUserMessagesContaining(a.Transcript(), "recent turns have remained in inspection"); got != 1 {
		t.Fatalf("phase-transition steer count = %d, want 1", got)
	}
	if sink.progress[len(sink.progress)-1].SteerReason != GuardSteerPhaseTransition {
		t.Fatalf("last steer reason = %q, want %q", sink.progress[len(sink.progress)-1].SteerReason, GuardSteerPhaseTransition)
	}
}

// TestRepetitionGuardSteersOnce verifies that identical (calls+results) model
// turns trigger exactly one steering nudge, not one per repeat.
func TestRepetitionGuardSteersOnce(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "identical output", nil // same result every call -> repetition
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	steps := make([]llmtest.Step, 6)
	for i := range steps {
		steps[i] = toolUseStep()
	}
	fp := llmtest.New("fake", steps...)
	a := newAgent(fp, reg, Options{MaxTurns: 6})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if got := countUserMessagesContaining(a.Transcript(), "identical results"); got != 1 {
		t.Errorf("repetition steer injected %d times, want exactly 1:\n%s", got, dump(a.Transcript()))
	}
}

// TestRepetitionGuardIgnoresChangingResults verifies that an identical call that
// returns different output each time (polling, a now-passing test) never trips
// the repetition guard.
func TestRepetitionGuardIgnoresChangingResults(t *testing.T) {
	n := 0
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		n++
		return fmt.Sprintf("output %d", n), nil // different result every call
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	steps := make([]llmtest.Step, 5)
	for i := range steps {
		steps[i] = toolUseStep()
	}
	fp := llmtest.New("fake", steps...)
	a := newAgent(fp, reg, Options{MaxTurns: 5})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if got := countUserMessagesContaining(a.Transcript(), "identical results"); got != 0 {
		t.Errorf("repetition steer fired %d times on changing results, want 0:\n%s", got, dump(a.Transcript()))
	}
}

func TestShellPipelineHeadIgnoresQuotedPipes(t *testing.T) {
	tests := []struct {
		command string
		head    string
		piped   bool
	}{
		{command: `go test ./pkg -run TestOne | grep PASS`, head: `go test ./pkg -run TestOne`, piped: true},
		{command: `printf '%s|%s' a b | sed -n '1p'`, head: `printf '%s|%s' a b`, piped: true},
		{command: `printf "a|b" | cat`, head: `printf "a|b"`, piped: true},
		{command: `printf a\|b`, head: `printf a\|b`, piped: false},
		{command: `go test ./pkg`, head: `go test ./pkg`, piped: false},
	}
	for _, tt := range tests {
		head, piped := shellPipelineHead(tt.command)
		if head != tt.head || piped != tt.piped {
			t.Errorf("shellPipelineHead(%q) = (%q, %v), want (%q, %v)", tt.command, head, piped, tt.head, tt.piped)
		}
	}
}

func TestCommandPipelineLoopSteersThenHardStops(t *testing.T) {
	n := 0
	tool := &recordTool{name: "run_command", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		n++
		return fmt.Sprintf("changing output %d", n), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	const base = `go test ./cmd/flow-worker -run TestWorkerRetries -count=1 -v 2>&1`
	steps := make([]llmtest.Step, 0, commandRepeatBreak+1)
	for i := 0; i < commandRepeatBreak; i++ {
		input, err := json.Marshal(map[string]string{
			"command": fmt.Sprintf("%s | sed -n '1,%dp'", base, i+1),
		})
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, fmt.Sprintf("call_%d", i), "run_command", string(input))},
			Stop:   llm.StopToolUse,
		})
	}
	steps = append(steps, textStep("stopping after repeated command"))
	fp := llmtest.New("fake", steps...)
	a := newAgent(fp, reg, Options{MaxTurns: 0})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "investigate the test", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if got := countUserMessagesContaining(a.Transcript(), "same underlying shell command"); got != 1 {
		t.Errorf("command-repeat steer injected %d times, want 1:\n%s", got, dump(a.Transcript()))
	}
	if len(sink.progress) != commandRepeatBreak {
		t.Fatalf("progress events = %d, want %d", len(sink.progress), commandRepeatBreak)
	}
	if got := sink.progress[commandRepeatSteer-1].SteerReason; got != GuardSteerCommandRepeat {
		t.Errorf("steer reason at threshold = %q, want %q", got, GuardSteerCommandRepeat)
	}
	if got := sink.progress[len(sink.progress)-1].CommandRepeatStreak; got != commandRepeatBreak {
		t.Errorf("final command repeat streak = %d, want %d", got, commandRepeatBreak)
	}
	var sawBreak bool
	for _, notice := range sink.notices {
		if strings.Contains(notice, "repeated the same underlying shell command") {
			sawBreak = true
		}
	}
	if !sawBreak {
		t.Errorf("expected command-repeat break notice, notices=%v", sink.notices)
	}
	if len(fp.Requests) != commandRepeatBreak+1 {
		t.Errorf("provider called %d times, want %d (break + summary)", len(fp.Requests), commandRepeatBreak+1)
	}
}

func TestCommandPipelineStreakResetsWhenBaseCommandChanges(t *testing.T) {
	var guard turnGuard
	result := []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultText: "ok"}}
	for i := 0; i < commandRepeatSteer-1; i++ {
		call := llm.ToolCall{Name: "run_command", Input: json.RawMessage(fmt.Sprintf(`{"command":"go test ./pkg | head -%d"}`, i+1))}
		guard.recordTools([]llm.ToolCall{call}, result)
	}
	changed := llm.ToolCall{Name: "run_command", Input: json.RawMessage(`{"command":"go test ./other | head -1"}`)}
	guard.recordTools([]llm.ToolCall{changed}, result)
	if guard.commandRuns != 1 {
		t.Fatalf("command streak after base change = %d, want 1", guard.commandRuns)
	}
	if reason, message := guard.nextSteer(); reason == GuardSteerCommandRepeat || strings.Contains(message, "underlying shell command") {
		t.Fatalf("base command change triggered command-repeat steer: reason=%q message=%q", reason, message)
	}
}

// TestRepeatLoopHardStops verifies that a byte-identical successful repeat loop
// is hard-stopped (not merely steered once) after repeatBreak turns, mirroring
// the error-storm break, so an unlimited-turn run can't spin forever re-issuing
// the same calls with the same results.
func TestRepeatLoopHardStops(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "identical output", nil // same calls + same results every turn -> repeat loop
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	// repeatBreak erroring-free tool turns plus a final tools-disabled summary;
	// MaxTurns unlimited so the repeat breaker (not maxTurns) is what stops it.
	steps := make([]llmtest.Step, repeatBreak)
	for i := range steps {
		steps[i] = toolUseStep()
	}
	steps = append(steps, llmtest.Step{
		Events: []llm.StreamEvent{textDelta("I keep repeating; stopping")},
		Stop:   llm.StopEndTurn,
	})
	fp := llmtest.New("fake", steps...)
	a := newAgent(fp, reg, Options{MaxTurns: 0})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	var sawBreak bool
	for _, msg := range sink.notices {
		if strings.Contains(msg, "identical tool turns repeated") {
			sawBreak = true
		}
	}
	if !sawBreak {
		t.Errorf("expected repeat-loop break notice, notices=%v", sink.notices)
	}
	// Stopped at the break threshold (+1 tools-disabled summary), not spun forever.
	if len(fp.Requests) != repeatBreak+1 {
		t.Errorf("provider called %d times, want %d (break + summary)", len(fp.Requests), repeatBreak+1)
	}
	// The repetition steer still fired exactly once before the hard stop.
	if got := countUserMessagesContaining(a.Transcript(), "identical results"); got != 1 {
		t.Errorf("repetition steer should fire once before the break, got %d", got)
	}
	last := a.Transcript()[len(a.Transcript())-1]
	if last.Role != llm.RoleAssistant || len(last.Content) == 0 || !strings.Contains(last.Content[0].Text, "repeating") {
		t.Errorf("turn should end on the assistant wind-down summary, got %+v", last)
	}
	assertPromptTermination(t, sink, TerminationRepeatGuard)
	if got := sink.promptUsage[0].ClosureTrigger; got != ClosureTriggerRepeatGuard {
		t.Errorf("closure trigger = %q, want repeat_guard", got)
	}
}

// TestErrorStormSteersThenBreaks verifies the consecutive-error backoff: one
// steering nudge at the steer threshold, then a hard stop with a notice at the
// break threshold. Error text varies each call so the error storm is isolated
// from the repetition guard.
func TestErrorStormSteersThenBreaks(t *testing.T) {
	n := 0
	tool := &recordTool{name: "probe", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		n++
		return "", fmt.Errorf("distinct failure %d", n)
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	// Plenty of erroring tool turns plus a final summary; MaxTurns unlimited so
	// the error-storm breaker (not maxTurns) is what stops the run.
	steps := make([]llmtest.Step, errorStormBreak)
	for i := range steps {
		steps[i] = toolUseStep()
	}
	steps = append(steps, llmtest.Step{
		Events: []llm.StreamEvent{textDelta("I am blocked: every call failed")},
		Stop:   llm.StopEndTurn,
	})
	fp := llmtest.New("fake", steps...)
	a := newAgent(fp, reg, Options{MaxTurns: 0})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if got := countUserMessagesContaining(a.Transcript(), "consecutive tool calls have all failed"); got != 1 {
		t.Errorf("error-storm steer injected %d times, want exactly 1", got)
	}
	var sawBreak bool
	for _, msg := range sink.notices {
		if strings.Contains(msg, "consecutive tool turns all failed") {
			sawBreak = true
		}
	}
	if !sawBreak {
		t.Errorf("expected error-storm break notice, notices=%v", sink.notices)
	}
	// The run stopped at the break threshold (+1 tools-disabled summary), not at
	// the unlimited default cap.
	if len(fp.Requests) != errorStormBreak+1 {
		t.Errorf("provider called %d times, want %d (break + summary)", len(fp.Requests), errorStormBreak+1)
	}
	last := a.Transcript()[len(a.Transcript())-1]
	if last.Role != llm.RoleAssistant || len(last.Content) == 0 || !strings.Contains(last.Content[0].Text, "blocked") {
		t.Errorf("turn should end on the assistant wind-down summary, got %+v", last)
	}
	assertPromptTermination(t, sink, TerminationErrorGuard)
	if got := sink.promptUsage[0].ClosureTrigger; got != ClosureTriggerErrorGuard {
		t.Errorf("closure trigger = %q, want error_guard", got)
	}
}

// TestTurnTokenBudgetStops verifies the per-prompt token budget halts a tool loop
// with a notice once cumulative tokens cross the ceiling, without an extra
// (paid) wind-down request.
func TestTurnTokenBudgetStops(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	// Each turn reports 60 tokens; budget 100 -> stop after the 2nd turn.
	step := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "probe", `{}`)},
		Stop:   llm.StopToolUse,
		Usage:  llm.Usage{InputTokens: 60},
	}
	fp := llmtest.New("fake", step, step, step, step)
	a := newAgent(fp, reg, Options{MaxTurns: 0, MaxPromptTokens: 100})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2 (budget stops the loop, no wind-down)", len(fp.Requests))
	}
	var sawBudget bool
	for _, msg := range sink.notices {
		if strings.Contains(msg, "prompt token budget 100 exceeded") {
			sawBudget = true
		}
	}
	if !sawBudget {
		t.Errorf("expected prompt token budget notice, notices=%v", sink.notices)
	}
	assertPromptTermination(t, sink, TerminationTokenLimit)
}

// TestZeroTokenBudgetUnlimited verifies MaxPromptTokens == 0 imposes no ceiling.
func TestZeroTokenBudgetUnlimited(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	toolStep := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "probe", `{}`)},
		Stop:   llm.StopToolUse,
		Usage:  llm.Usage{InputTokens: 1_000_000, CostUSD: 5, CostKnown: true},
	}
	done := llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn}
	fp := llmtest.New("fake", toolStep, toolStep, done)
	a := newAgent(fp, reg, Options{MaxTurns: 0, MaxPromptTokens: 0})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	for _, msg := range sink.notices {
		if strings.Contains(msg, "token budget") {
			t.Errorf("unlimited budget should not emit a budget notice, got %q", msg)
		}
	}
}

// TestPromptCostBudgetStops verifies the per-prompt USD cost budget halts a tool
// loop once cumulative model cost crosses the ceiling, like the token budget but
// in dollars. The default test registry prices claude-opus-4-8 at $5/1M input,
// so 1,000,000 input tokens => $5/turn; budget $8 stops after the 2nd turn.
func TestPromptCostBudgetStops(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	step := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "probe", `{}`)},
		Stop:   llm.StopToolUse,
		Usage:  llm.Usage{InputTokens: 1_000_000, CostUSD: 5, CostKnown: true},
	}
	fp := llmtest.New("fake", step, step, step, step)
	a := newAgent(fp, reg, Options{MaxTurns: 0, Model: "claude-opus-4-8", MaxPromptCostUSD: 8.0})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(sink.completedTurns) != 2 {
		t.Errorf("completed turns = %d, want 2 (cost budget stops the loop); provider calls=%d include maintenance", len(sink.completedTurns), len(fp.Requests))
	}
	var sawBudget bool
	for _, msg := range sink.notices {
		if strings.Contains(msg, "prompt cost budget") {
			sawBudget = true
		}
	}
	if !sawBudget {
		t.Errorf("expected prompt cost budget notice, notices=%v", sink.notices)
	}
	assertPromptTermination(t, sink, TerminationCostLimit)
}

// TestPromptCostBudgetUnpricedModelNeverFires verifies the cost budget cannot
// fire when provider usage has no known cost: it degrades to no ceiling rather
// than stopping arbitrarily.
func TestPromptCostBudgetUnpricedModelNeverFires(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	unpriced := llm.NewRegistry(map[string]llm.ModelInfo{
		"local-model": {ContextWindow: 1_000_000}, // no Price -> Cost known=false
	})
	toolStep := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "probe", `{}`)},
		Stop:   llm.StopToolUse,
		Usage:  llm.Usage{InputTokens: 10_000_000},
	}
	done := llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn}
	fp := llmtest.New("fake", toolStep, toolStep, done)
	a := newAgent(fp, reg, Options{MaxTurns: 0, Model: "local-model", Registry: unpriced, MaxPromptCostUSD: 0.01})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	for _, msg := range sink.notices {
		if strings.Contains(msg, "cost budget") {
			t.Errorf("unpriced model must not trigger a cost budget stop, got %q", msg)
		}
	}
}

// TestCanonicalJSONOrderInsensitive pins the order-insensitive input
// canonicalization that backs the repetition signature.
func TestCanonicalJSONOrderInsensitive(t *testing.T) {
	a := canonicalJSON([]byte(`{"b":1,"a":2}`))
	b := canonicalJSON([]byte(`{"a":2,"b":1}`))
	if a != b {
		t.Errorf("key order should not change the signature: %q vs %q", a, b)
	}
	// Array order is significant.
	if canonicalJSON([]byte(`[1,2]`)) == canonicalJSON([]byte(`[2,1]`)) {
		t.Errorf("array order must remain significant")
	}
}

func TestCallSetSignatureIncludesRichResultImageMetadataAndDigest(t *testing.T) {
	const imageData = "c2Vuc2l0aXZlLWJhc2U2NC1wYXlsb2Fk"
	calls := []llm.ToolCall{{Name: "capture", Input: json.RawMessage(`{"target":"screen"}`)}}
	baseResult := llm.ContentBlock{
		Kind:        llm.BlockToolResult,
		ResultForID: "call",
		ResultText:  "attached",
		ResultContent: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageMediaType: "image/png",
			ImageData:      imageData,
			ImageDetail:    "high",
			ImageWidth:     640,
			ImageHeight:    480,
		}},
	}
	base := callSetSignature(calls, []llm.ContentBlock{baseResult})
	if strings.Contains(base, imageData) {
		t.Fatalf("loop signature contains raw base64: %q", base)
	}
	if got := callSetSignature(calls, []llm.ContentBlock{baseResult}); got != base {
		t.Fatalf("identical rich result signature changed:\n%s\n%s", base, got)
	}

	mutations := map[string]func(*llm.ContentBlock){
		"media type": func(image *llm.ContentBlock) { image.ImageMediaType = "image/jpeg" },
		"detail":     func(image *llm.ContentBlock) { image.ImageDetail = "low" },
		"width":      func(image *llm.ContentBlock) { image.ImageWidth++ },
		"height":     func(image *llm.ContentBlock) { image.ImageHeight++ },
		"image data": func(image *llm.ContentBlock) { image.ImageData += "A" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := baseResult
			changed.ResultContent = append([]llm.ContentBlock(nil), baseResult.ResultContent...)
			mutate(&changed.ResultContent[0])
			if got := callSetSignature(calls, []llm.ContentBlock{changed}); got == base {
				t.Fatalf("%s did not change rich result signature", name)
			}
		})
	}
}

// TestIncrementalValidationCatchesNewAppends verifies r62's incremental
// validator still rejects a corruption introduced after an already-validated
// prefix — only the suffix is re-walked, but new content is always checked.
func TestIncrementalValidationCatchesNewAppends(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{})
	a.SetTranscript([]llm.Message{userText("q"), asstText("a")})
	if err := a.validateTranscript("seed"); err != nil {
		t.Fatalf("seed should be valid: %v", err)
	}
	// A valid whole turn appended after the validated prefix.
	a.transcript = append(a.transcript, userText("q2"), asstToolUse("t1", "read_file", `{}`), toolResult("t1", "ok"))
	if err := a.validateTranscript("valid turn"); err != nil {
		t.Fatalf("valid appended turn rejected: %v", err)
	}
	// An orphan tool_result appended after the prefix must still be caught.
	a.transcript = append(a.transcript, asstText("a2"), toolResult("orphan", "x"))
	if err := a.validateTranscript("orphan"); err == nil {
		t.Fatal("incremental validator missed an orphan tool_result in the suffix")
	}
}

// TestSetTranscriptForcesFullRevalidation verifies a resumed transcript is
// validated in full, not skipped by a stale validated prefix.
func TestSetTranscriptForcesFullRevalidation(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{})
	a.SetTranscript([]llm.Message{userText("q"), asstText("a")})
	if err := a.validateTranscript("first"); err != nil {
		t.Fatalf("first should be valid: %v", err)
	}
	// Resume with an invalid transcript (orphan tool_result at index 0).
	a.SetTranscript([]llm.Message{toolResult("orphan", "x")})
	if err := a.validateTranscript("resumed"); err == nil {
		t.Fatal("resumed invalid transcript should be rejected by a full walk")
	}
}
