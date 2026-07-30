package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

// failingTool returns a read-only fake that always fails with the same error.
func failingTool(name, errText string) *recordTool {
	return &recordTool{
		name:     name,
		readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			return "", errors.New(errText)
		},
	}
}

// mutatingSuccessTool reports mutated paths so a successful run resets the
// guard map.
type mutatingSuccessTool struct {
	recordTool
}

func (t *mutatingSuccessTool) MutatedPaths(json.RawMessage) ([]string, error) {
	return []string{"x.txt"}, nil
}

func toolCallStep(id, name, input string) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, id, name, input)},
		Stop:   llm.StopToolUse,
	}
}

func textStep(text string) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{textDelta(text)},
		Stop:   llm.StopEndTurn,
	}
}

func TestIdenticalFailureGuardWarnsThenBlocks(t *testing.T) {
	probe := failingTool("probe", "no such file")
	reg := &tools.Registry{}
	reg.Register(probe)
	fp := llmtest.New("fake",
		toolCallStep("call_1", "probe", `{"path":"missing.txt"}`),
		toolCallStep("call_2", "probe", `{"path":"missing.txt"}`),
		toolCallStep("call_3", "probe", `{"path":"missing.txt"}`),
		toolCallStep("call_4", "probe", `{"path":"missing.txt"}`),
		textStep("done"),
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "probe away", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 4 {
		t.Fatalf("results = %d, want 4: %+v", len(sink.results), sink.results)
	}
	if got := sink.results[0].Text; strings.Contains(got, "[loop guard]") {
		t.Fatalf("first failure unexpectedly steered: %q", got)
	}
	if got := sink.results[1].Text; !strings.Contains(got, "[loop guard]") || !strings.Contains(got, "2 times") || !strings.Contains(got, "missing.txt") {
		t.Fatalf("second failure not steered with count and target: %q", got)
	}
	for i, r := range sink.results[2:] {
		if !r.IsError || r.ErrorKind != llm.ToolErrorBlocked {
			t.Fatalf("result %d = kind %q is_error=%v, want blocked error", i+2, r.ErrorKind, r.IsError)
		}
		if !strings.Contains(r.Text, "blocked") || !strings.Contains(r.Text, "missing.txt") {
			t.Fatalf("blocked result %d text = %q", i+2, r.Text)
		}
	}
	// Blocked calls are not dispatched and do not grow the streak.
	if len(probe.inputs) != 2 {
		t.Fatalf("tool ran %d times, want exactly 2 dispatches", len(probe.inputs))
	}
	mustValid(t, a.Transcript())
}

func TestIdenticalFailureGuardIgnoresDifferentErrors(t *testing.T) {
	probe := &recordTool{name: "probe", readOnly: true}
	probe.run = func(context.Context, json.RawMessage) (string, error) {
		return "", fmt.Errorf("boom %d", len(probe.inputs))
	}
	reg := &tools.Registry{}
	reg.Register(probe)
	fp := llmtest.New("fake",
		toolCallStep("call_1", "probe", `{"path":"a.go"}`),
		toolCallStep("call_2", "probe", `{"path":"a.go"}`),
		toolCallStep("call_3", "probe", `{"path":"a.go"}`),
		textStep("done"),
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "probe", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 3 {
		t.Fatalf("results = %d, want 3", len(sink.results))
	}
	for i, r := range sink.results {
		if r.ErrorKind == llm.ToolErrorBlocked || strings.Contains(r.Text, "[loop guard]") {
			t.Fatalf("result %d tripped the guard despite differing errors: %+v", i, r)
		}
	}
	if len(probe.inputs) != 3 {
		t.Fatalf("tool ran %d times, want 3", len(probe.inputs))
	}
}

func TestIdenticalFailureGuardResetsAfterMutatingSuccess(t *testing.T) {
	probe := failingTool("probe", "still broken")
	writer := &mutatingSuccessTool{recordTool{
		name: "writer",
		run: func(context.Context, json.RawMessage) (string, error) {
			return "wrote x.txt", nil
		},
	}}
	reg := &tools.Registry{}
	reg.Register(probe)
	reg.Register(writer)
	fp := llmtest.New("fake",
		toolCallStep("call_1", "probe", `{"path":"a.go"}`),
		toolCallStep("call_2", "probe", `{"path":"a.go"}`), // warn
		toolCallStep("call_3", "writer", `{"path":"x.txt"}`),
		toolCallStep("call_4", "probe", `{"path":"a.go"}`), // streak reset: first failure
		toolCallStep("call_5", "probe", `{"path":"a.go"}`), // warn again
		textStep("done"),
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "probe", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 5 {
		t.Fatalf("results = %d, want 5", len(sink.results))
	}
	for _, i := range []int{1, 4} {
		if got := sink.results[i].Text; !strings.Contains(got, "[loop guard]") {
			t.Fatalf("result %d missing warn: %q", i, got)
		}
	}
	if got := sink.results[3].Text; strings.Contains(got, "[loop guard]") {
		t.Fatalf("first failure after mutating success unexpectedly steered: %q", got)
	}
	for i, r := range sink.results {
		if r.ErrorKind == llm.ToolErrorBlocked {
			t.Fatalf("result %d blocked after mutating success reset: %+v", i, r)
		}
	}
}

func TestIdenticalFailureGuardResetsPerPrompt(t *testing.T) {
	probe := failingTool("probe", "no such file")
	reg := &tools.Registry{}
	reg.Register(probe)
	fp := llmtest.New("fake",
		toolCallStep("call_1", "probe", `{"path":"missing.txt"}`),
		toolCallStep("call_2", "probe", `{"path":"missing.txt"}`),
		textStep("first done"),
		toolCallStep("call_3", "probe", `{"path":"missing.txt"}`),
		textStep("second done"),
	)
	a := newAgent(fp, reg, Options{})
	if err := a.RunPrompt(context.Background(), "first", &recordSink{}); err != nil {
		t.Fatalf("first RunPrompt: %v", err)
	}

	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "second", sink); err != nil {
		t.Fatalf("second RunPrompt: %v", err)
	}
	if len(sink.results) != 1 {
		t.Fatalf("second prompt results = %d, want 1", len(sink.results))
	}
	if got := sink.results[0].Text; strings.Contains(got, "[loop guard]") {
		t.Fatalf("new prompt's first failure unexpectedly steered: %q", got)
	}
}

func TestIdenticalFailureGuardConcurrentBatch(t *testing.T) {
	probe := failingTool("probe", "no such file")
	reg := &tools.Registry{}
	reg.Register(probe)
	input := `{"path":"missing.txt"}`
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "call_1", "probe", input),
				toolDone(1, "call_2", "probe", input),
			},
			Stop: llm.StopToolUse,
		},
		toolCallStep("call_3", "probe", input),
		textStep("done"),
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "probe", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 3 {
		t.Fatalf("results = %d, want 3", len(sink.results))
	}
	// The two concurrent identical calls race through the mutex: one records
	// attempt 1, the other attempt 2, so exactly one carries the warn.
	warns := 0
	for _, r := range sink.results[:2] {
		if !r.IsError {
			t.Fatalf("batch result not an error: %+v", r)
		}
		if strings.Contains(r.Text, "[loop guard]") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("warns across concurrent batch = %d, want exactly 1: %+v", warns, sink.results[:2])
	}
	// The third identical attempt is blocked before dispatch.
	if got := sink.results[2]; !got.IsError || got.ErrorKind != llm.ToolErrorBlocked {
		t.Fatalf("third result = kind %q is_error=%v, want blocked", got.ErrorKind, got.IsError)
	}
	if len(probe.inputs) != 2 {
		t.Fatalf("tool ran %d times, want exactly 2 dispatches", len(probe.inputs))
	}
	mustValid(t, a.Transcript())
}
