package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func stageCalls(inputs ...string) []llm.ToolCall {
	calls := make([]llm.ToolCall, len(inputs))
	for i, input := range inputs {
		calls[i] = llm.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "probe", Input: json.RawMessage(input)}
	}
	return calls
}

func TestPlanCallSuppressionSuppressesDuplicatesWithinStage(t *testing.T) {
	calls := stageCalls(`{"path":"a"}`, `{"path":"a"}`, ` {"path":"a"} `, `{"path":"b"}`)
	_, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	suppressed, duplicates, overLimit := planCallSuppression(calls, stages)
	if duplicates != 2 || overLimit != 0 {
		t.Fatalf("duplicates=%d overLimit=%d, want 2/0", duplicates, overLimit)
	}
	if len(suppressed) != 2 {
		t.Fatalf("suppressed = %v, want indexes 1 and 2", suppressed)
	}
	for _, i := range []int{1, 2} {
		if !strings.Contains(suppressed[i], "duplicates an identical call") {
			t.Fatalf("suppressed[%d] = %q, want duplicate guard text", i, suppressed[i])
		}
	}
}

func TestPlanCallSuppressionAllowsStagedReruns(t *testing.T) {
	calls := stageCalls(
		`{"_stage":1,"path":"a"}`,
		`{"_stage":2,"path":"a"}`,
		`{"_stage":3,"path":"a"}`,
	)
	execution, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	suppressed, duplicates, overLimit := planCallSuppression(execution, stages)
	if len(suppressed) != 0 || duplicates != 0 || overLimit != 0 {
		t.Fatalf("staged re-runs suppressed: %v (dups=%d over=%d)", suppressed, duplicates, overLimit)
	}
}

func TestPlanCallSuppressionEnforcesTurnLimit(t *testing.T) {
	inputs := make([]string, 0, maxDispatchedCallsPerTurn+2)
	for i := 0; i < maxDispatchedCallsPerTurn+2; i++ {
		inputs = append(inputs, fmt.Sprintf(`{"path":"f%d"}`, i))
	}
	calls := stageCalls(inputs...)
	_, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	suppressed, duplicates, overLimit := planCallSuppression(calls, stages)
	if duplicates != 0 || overLimit != 2 {
		t.Fatalf("duplicates=%d overLimit=%d, want 0/2", duplicates, overLimit)
	}
	if len(suppressed) != 2 {
		t.Fatalf("suppressed = %v, want the last two indexes", suppressed)
	}
	for _, i := range []int{maxDispatchedCallsPerTurn, maxDispatchedCallsPerTurn + 1} {
		if !strings.Contains(suppressed[i], "dispatch limit") {
			t.Fatalf("suppressed[%d] = %q, want over-limit guard text", i, suppressed[i])
		}
	}
}

func TestPlanCallSuppressionDuplicatesDoNotConsumeBudget(t *testing.T) {
	// maxDispatchedCallsPerTurn-1 duplicates of one call plus two distinct
	// calls: only the first duplicate-group member and the distinct calls
	// dispatch, so nothing hits the turn limit.
	inputs := make([]string, 0, maxDispatchedCallsPerTurn+1)
	for i := 0; i < maxDispatchedCallsPerTurn-1; i++ {
		inputs = append(inputs, `{"path":"same"}`)
	}
	inputs = append(inputs, `{"path":"x"}`, `{"path":"y"}`)
	calls := stageCalls(inputs...)
	_, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	suppressed, duplicates, overLimit := planCallSuppression(calls, stages)
	if duplicates != maxDispatchedCallsPerTurn-2 || overLimit != 0 {
		t.Fatalf("duplicates=%d overLimit=%d, want %d/0", duplicates, overLimit, maxDispatchedCallsPerTurn-2)
	}
	if len(suppressed) != maxDispatchedCallsPerTurn-2 {
		t.Fatalf("suppressed count = %d, want %d", len(suppressed), maxDispatchedCallsPerTurn-2)
	}
}

func TestPlanCallSuppressionSkipsNonDispatchCalls(t *testing.T) {
	invalid := llm.ToolCall{
		ID:                "bad1",
		Name:              "probe",
		Input:             llm.InvalidToolInputObject(fmt.Errorf("unexpected EOF")),
		InvalidInputError: "unexpected EOF",
	}
	bad2 := invalid
	bad2.ID = "bad2"
	hosted := llm.ToolCall{ID: "hosted1", Name: kimiWebSearchToolName, Input: json.RawMessage(`{"query":"x"}`)}
	hosted2 := llm.ToolCall{ID: "hosted2", Name: kimiWebSearchToolName, Input: json.RawMessage(`{"query":"x"}`)}
	calls := []llm.ToolCall{invalid, bad2, hosted, hosted2}
	_, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	suppressed, duplicates, overLimit := planCallSuppression(calls, stages)
	if len(suppressed) != 0 || duplicates != 0 || overLimit != 0 {
		t.Fatalf("non-dispatch calls suppressed: %v (dups=%d over=%d)", suppressed, duplicates, overLimit)
	}
}

func TestDispatchCallsSuppressesDuplicatesAndPreservesOrder(t *testing.T) {
	probe := &recordTool{name: "probe", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(probe)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	sink := &recordSink{}
	calls := stageCalls(`{"path":"a"}`, `{"path":"b"}`, `{"path":"a"}`, `{"path":"a"}`)
	blocks, _, _ := a.dispatchCalls(context.Background(), calls, 1, 1, sink)

	if len(probe.inputs) != 2 {
		t.Fatalf("tool ran %d times, want 2 (one per distinct input): %v", len(probe.inputs), probe.inputs)
	}
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(blocks))
	}
	for i, block := range blocks {
		if block.ResultForID != calls[i].ID {
			t.Fatalf("block %d ForID = %q, want %q", i, block.ResultForID, calls[i].ID)
		}
	}
	if blocks[0].ResultError || blocks[1].ResultError {
		t.Fatalf("distinct calls errored: %+v %+v", blocks[0], blocks[1])
	}
	for _, i := range []int{2, 3} {
		if !blocks[i].ResultError || !strings.Contains(blocks[i].ResultText, "duplicates an identical call") {
			t.Fatalf("block %d = %+v, want duplicate guard error", i, blocks[i])
		}
	}
	if got := idsFromCalls(sink.starts); !slices.Equal(got, []string{"call_0", "call_1", "call_2", "call_3"}) {
		t.Fatalf("ToolStart order = %v", got)
	}
	if got := idsFromResults(sink.results); !slices.Equal(got, []string{"call_0", "call_1", "call_2", "call_3"}) {
		t.Fatalf("ToolResult order = %v", got)
	}
	if len(sink.notices) != 1 || !strings.Contains(sink.notices[0], "suppressed 2 duplicate and 0 over-limit") {
		t.Fatalf("notices = %v, want one suppression summary", sink.notices)
	}
}

func TestDispatchCallsEnforcesTurnLimit(t *testing.T) {
	probe := &recordTool{name: "probe", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(probe)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	sink := &recordSink{}
	inputs := make([]string, 0, maxDispatchedCallsPerTurn+2)
	for i := 0; i < maxDispatchedCallsPerTurn+2; i++ {
		inputs = append(inputs, fmt.Sprintf(`{"path":"f%d"}`, i))
	}
	calls := stageCalls(inputs...)
	blocks, _, _ := a.dispatchCalls(context.Background(), calls, 1, 1, sink)

	if len(probe.inputs) != maxDispatchedCallsPerTurn {
		t.Fatalf("tool ran %d times, want %d", len(probe.inputs), maxDispatchedCallsPerTurn)
	}
	if len(blocks) != maxDispatchedCallsPerTurn+2 {
		t.Fatalf("blocks = %d, want %d", len(blocks), maxDispatchedCallsPerTurn+2)
	}
	for i := maxDispatchedCallsPerTurn; i < len(blocks); i++ {
		if !blocks[i].ResultError || !strings.Contains(blocks[i].ResultText, "dispatch limit") {
			t.Fatalf("block %d = %+v, want over-limit guard error", i, blocks[i])
		}
	}
	if len(sink.notices) != 1 || !strings.Contains(sink.notices[0], "0 duplicate and 2 over-limit") {
		t.Fatalf("notices = %v, want over-limit summary", sink.notices)
	}
}

func TestDispatchCallsStagedRerunNotSuppressed(t *testing.T) {
	probe := &recordTool{name: "probe", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(probe)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	calls := stageCalls(`{"_stage":1,"path":"a"}`, `{"_stage":2,"path":"a"}`)
	blocks, _, _ := a.dispatchCalls(context.Background(), calls, 1, 1, &recordSink{})
	if len(probe.inputs) != 2 {
		t.Fatalf("tool ran %d times, want 2 staged re-runs", len(probe.inputs))
	}
	for i, block := range blocks {
		if block.ResultError {
			t.Fatalf("block %d errored: %+v", i, block)
		}
	}
}

func TestCrossStageMutationDependenciesSkipSuppressed(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&mutationRecordTool{
		recordTool: &recordTool{name: "mut", run: func(context.Context, json.RawMessage) (string, error) {
			return "ok", nil
		}},
		paths: func(json.RawMessage) ([]string, error) { return []string{"x.txt"}, nil },
	})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	calls := stageCalls(
		`{"_stage":1,"path":"x.txt"}`,
		`{"_stage":1,"path":"x.txt"}`,
		`{"_stage":2,"path":"x.txt"}`,
	)
	for i := range calls {
		calls[i].Name = "mut"
	}
	execution, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	suppressed, _, _ := planCallSuppression(execution, stages)
	if _, ok := suppressed[1]; !ok {
		t.Fatalf("call 1 not suppressed: %v", suppressed)
	}
	deps := a.crossStageMutationDependencies(execution, stages, suppressed)
	// The stage-2 call must depend on the real stage-1 writer (index 0), not
	// the suppressed duplicate (index 1) that never produces side effects.
	if len(deps[2]) != 1 || deps[2][0] != 0 {
		t.Fatalf("deps[2] = %v, want [0]", deps[2])
	}
	if len(deps[1]) != 0 {
		t.Fatalf("suppressed call gained dependencies: %v", deps[1])
	}
}

func TestDispatchParallelBatchBoundsConcurrency(t *testing.T) {
	var current, maxSeen atomic.Int32
	probe := &recordTool{name: "probe", readOnly: true, run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		now := current.Add(1)
		for {
			peak := maxSeen.Load()
			if now <= peak || maxSeen.CompareAndSwap(peak, now) {
				break
			}
		}
		defer current.Add(-1)
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
		}
		return "ok", nil
	}}
	reg := &tools.Registry{}
	reg.Register(probe)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	inputs := make([]string, 0, 2*maxConcurrentToolRuns)
	for i := 0; i < 2*maxConcurrentToolRuns; i++ {
		inputs = append(inputs, fmt.Sprintf(`{"path":"f%d"}`, i))
	}
	calls := stageCalls(inputs...)
	blocks, _, _ := a.dispatchCalls(context.Background(), calls, 1, 1, &recordSink{})
	for i, block := range blocks {
		if block.ResultError {
			t.Fatalf("block %d errored: %+v", i, block)
		}
	}
	if got := int(maxSeen.Load()); got > maxConcurrentToolRuns {
		t.Fatalf("observed %d concurrent tool runs, want <= %d", got, maxConcurrentToolRuns)
	}
	if len(probe.inputs) != 2*maxConcurrentToolRuns {
		t.Fatalf("tool ran %d times, want %d", len(probe.inputs), 2*maxConcurrentToolRuns)
	}
}

// TestRunPromptSuppressesDegenerateDuplicateBatch is the regression test for
// the session-exhaustion incident: a model response that repeats one identical
// call hundreds of times must dispatch it once, not hundreds of times.
func TestRunPromptSuppressesDegenerateDuplicateBatch(t *testing.T) {
	probe := &recordTool{name: "probe", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		return "PASS", nil
	}}
	reg := &tools.Registry{}
	reg.Register(probe)
	const duplicates = 300
	events := make([]llm.StreamEvent, 0, duplicates)
	for i := 0; i < duplicates; i++ {
		events = append(events, toolDone(i, fmt.Sprintf("call_%d", i), "probe", `{"command":"go test ./..."}`))
	}
	fp := llmtest.New("fake",
		llmtest.Step{Events: events, Stop: llm.StopToolUse},
		textStep("done"),
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "run the tests", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(probe.inputs) != 1 {
		t.Fatalf("tool ran %d times, want exactly 1 dispatch", len(probe.inputs))
	}
	if len(sink.results) != duplicates {
		t.Fatalf("results = %d, want %d (every call gets a result)", len(sink.results), duplicates)
	}
	blocked := 0
	for _, r := range sink.results[1:] {
		if r.IsError && r.ErrorKind == llm.ToolErrorBlocked && strings.Contains(r.Text, "duplicates an identical call") {
			blocked++
		}
	}
	if blocked != duplicates-1 {
		t.Fatalf("blocked = %d, want %d", blocked, duplicates-1)
	}
	mustValid(t, a.Transcript())
}
