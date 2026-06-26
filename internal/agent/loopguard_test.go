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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	if got := countUserMessagesContaining(a.Transcript(), "identical results"); got != 0 {
		t.Errorf("repetition steer fired %d times on changing results, want 0:\n%s", got, dump(a.Transcript()))
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
}

// TestTurnTokenBudgetStops verifies the per-turn token budget halts a tool loop
// with a notice once cumulative tokens cross the ceiling, without an extra
// (paid) wind-down request.
func TestTurnTokenBudgetStops(t *testing.T) {
	tool := &recordTool{name: "probe", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	// Each model turn reports 60 tokens; budget 100 -> stop after the 2nd turn.
	step := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "probe", `{}`)},
		Stop:   llm.StopToolUse,
		Usage:  llm.Usage{InputTokens: 60},
	}
	fp := llmtest.New("fake", step, step, step, step)
	a := newAgent(fp, reg, Options{MaxTurns: 0, MaxTurnTokens: 100})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2 (budget stops the loop, no wind-down)", len(fp.Requests))
	}
	var sawBudget bool
	for _, msg := range sink.notices {
		if strings.Contains(msg, "turn token budget 100 exceeded") {
			sawBudget = true
		}
	}
	if !sawBudget {
		t.Errorf("expected turn token budget notice, notices=%v", sink.notices)
	}
}

// TestZeroTokenBudgetUnlimited verifies MaxTurnTokens == 0 imposes no ceiling.
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
	a := newAgent(fp, reg, Options{MaxTurns: 0, MaxTurnTokens: 0})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	for _, msg := range sink.notices {
		if strings.Contains(msg, "token budget") {
			t.Errorf("unlimited budget should not emit a budget notice, got %q", msg)
		}
	}
}

// TestPromptCostBudgetStops verifies the per-turn USD cost budget halts a tool
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2 (cost budget stops the loop)", len(fp.Requests))
	}
	var sawBudget bool
	for _, msg := range sink.notices {
		if strings.Contains(msg, "turn cost budget") {
			sawBudget = true
		}
	}
	if !sawBudget {
		t.Errorf("expected turn cost budget notice, notices=%v", sink.notices)
	}
}

// TestPromptCostBudgetUnpricedModelNeverFires verifies the cost budget cannot
// fire for a model without catalog pricing (Cost reports known=false): it
// degrades to no ceiling rather than stopping arbitrarily.
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
