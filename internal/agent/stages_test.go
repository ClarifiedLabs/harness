package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestPlanToolStagesAcceptsOutOfOrderLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []int // zero means omitted, not an explicit zero
		order  []int
		stages []callStage
	}{
		{"interleaved", []int{1, 2, 1}, []int{0, 2, 1}, []callStage{{0, 2}, {2, 3}}},
		{"descending with inheritance", []int{7, 0, 1, 0, 3, 0}, []int{2, 3, 4, 5, 0, 1}, []callStage{{0, 2}, {2, 4}, {4, 6}}},
		{"initial default", []int{0, 5, 0, 1, 0}, []int{0, 3, 4, 1, 2}, []callStage{{0, 3}, {3, 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := make([]string, len(tc.labels))
			for i, label := range tc.labels {
				inputs[i] = fmt.Sprintf(`{"path":"%d"}`, i)
				if label != 0 {
					inputs[i] = fmt.Sprintf(`{"path":"%d","_stage":%d}`, i, label)
				}
			}
			calls := stageCalls(inputs...)
			execution, stages, order, err := planToolStages(calls)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(order, tc.order) || !slices.Equal(stages, tc.stages) {
				t.Fatalf("order=%v stages=%v, want %v %v", order, stages, tc.order, tc.stages)
			}
			for i, original := range order {
				if execution[i].ID != calls[original].ID || string(execution[i].Input) != fmt.Sprintf(`{"path":"%d"}`, original) {
					t.Fatalf("execution[%d] = %+v", i, execution[i])
				}
			}
			// Post-dispatch guards still pair calls with emission-ordered results.
			emitted := executionToolCalls(calls)
			for i, call := range calls {
				if string(call.Input) != inputs[i] {
					t.Fatalf("raw call %d changed: %s", i, call.Input)
				}
				if emitted[i].ID != call.ID || string(emitted[i].Input) != fmt.Sprintf(`{"path":"%d"}`, i) {
					t.Fatalf("emission order/input lost at %d: %+v", i, emitted[i])
				}
			}
		})
	}
}

func TestOutOfOrderStagesPreserveMutationAndTranscriptOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	calls := []llm.ToolCall{
		{ID: "last-write", Name: "write", Input: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"final","_stage":3}`, path))},
		{ID: "first-write", Name: "write", Input: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"first","_stage":1}`, path))},
		{ID: "edit", Name: "edit", Input: json.RawMessage(fmt.Sprintf(`{"files":[{"path":%q,"edits":[{"oldText":"first","newText":"second"}]}],"_stage":1}`, path))},
		{ID: "read", Name: "read", Input: json.RawMessage(fmt.Sprintf(`{"path":%q,"_stage":2}`, path))},
	}
	events := make([]llm.StreamEvent, len(calls))
	for i, call := range calls {
		events[i] = toolDone(i, call.ID, call.Name, string(call.Input))
	}
	fp := llmtest.New("fake", llmtest.Step{Events: events, Stop: llm.StopToolUse}, summaryStep("done", 10, 1))
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "apply", sink); err != nil {
		t.Fatal(err)
	}
	mustValid(t, a.Transcript())
	wantExecution := []string{"first-write", "edit", "read", "last-write"}
	if !slices.Equal(idsFromCalls(sink.starts), wantExecution) || !slices.Equal(idsFromResults(sink.results), wantExecution) {
		t.Fatalf("execution order: starts=%v results=%v", idsFromCalls(sink.starts), idsFromResults(sink.results))
	}
	messages := a.Transcript()
	for i, call := range calls {
		use, result := messages[1].Content[i], messages[2].Content[i]
		if use.ToolUseID != call.ID || string(use.ToolInput) != string(call.Input) {
			t.Fatalf("raw tool call %d changed: %+v", i, use)
		}
		if result.ResultForID != call.ID || result.ResultError {
			t.Fatalf("result %d = %+v", i, result)
		}
	}
	if got := messages[2].Content[3].ResultText; !strings.Contains(got, "second") || strings.Contains(got, "final") {
		t.Fatalf("stage-2 read did not observe stage-1 edit: %q", got)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "final" {
		t.Fatalf("stage-3 write = %q, %v", got, err)
	}
}

func TestOutOfOrderStagesKeepMixedResultsPairedForProgress(t *testing.T) {
	dir := t.TempDir()
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{
		toolDone(0, "read", "read", fmt.Sprintf(`{"path":%q,"_stage":2}`, filepath.Join(dir, "missing.txt"))),
		toolDone(1, "write", "write", fmt.Sprintf(`{"path":%q,"content":"ok","_stage":1}`, filepath.Join(dir, "created.txt"))),
	}, Stop: llm.StopToolUse}, summaryStep("done", 10, 1))
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "apply", sink); err != nil {
		t.Fatal(err)
	}
	mustValid(t, a.Transcript())
	results := a.Transcript()[2].Content
	if len(results) != 2 || results[0].ResultForID != "read" || !results[0].ResultError || results[1].ResultForID != "write" || results[1].ResultError {
		t.Fatalf("mixed transcript results = %+v", results)
	}
	if len(sink.progress) != 1 {
		t.Fatalf("progress = %+v", sink.progress)
	}
	progress := sink.progress[0]
	if progress.ErrorCount != 1 || !progress.SuccessfulMutation || !progress.ExplicitProgress || progress.Activity.Inspect != 1 || progress.Activity.Mutate != 1 {
		t.Fatalf("call/result classifications were mismatched: %+v", progress)
	}
}

func TestOutOfOrderStagesDispatchLimitPreservesResultPositions(t *testing.T) {
	probe := &recordTool{name: "probe", run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	reg := &tools.Registry{}
	reg.Register(probe)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	inputs := []string{`{"_stage":2,"path":"later"}`}
	for i := 0; i < maxDispatchedCallsPerTurn; i++ {
		inputs = append(inputs, fmt.Sprintf(`{"_stage":1,"path":"%d"}`, i))
	}
	calls := stageCalls(inputs...)
	blocks, _, _ := a.dispatchCalls(context.Background(), calls, 1, 1, &recordSink{})
	if len(probe.inputs) != maxDispatchedCallsPerTurn {
		t.Fatalf("runs=%d, want %d", len(probe.inputs), maxDispatchedCallsPerTurn)
	}
	for i, block := range blocks {
		if block.ResultForID != calls[i].ID || block.ResultError != (i == 0) {
			t.Fatalf("result %d = %+v", i, block)
		}
	}
	if !strings.Contains(blocks[0].ResultText, "dispatch limit") {
		t.Fatalf("later-stage result = %+v", blocks[0])
	}
}

func TestOutOfOrderStagesSuppressNonadjacentDuplicates(t *testing.T) {
	probe := &recordTool{name: "probe", run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	reg := &tools.Registry{}
	reg.Register(probe)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	calls := stageCalls(`{"_stage":1,"path":"a"}`, `{"_stage":2,"path":"a"}`, `{"_stage":1,"path":"a"}`)
	blocks, _, _ := a.dispatchCalls(context.Background(), calls, 1, 1, &recordSink{})
	if len(probe.inputs) != 2 {
		t.Fatalf("runs=%d, want one per stage", len(probe.inputs))
	}
	for i, block := range blocks {
		if block.ResultForID != calls[i].ID || block.ResultError != (i == 2) {
			t.Fatalf("result %d = %+v", i, block)
		}
	}
	if !strings.Contains(blocks[2].ResultText, "duplicates an identical call") {
		t.Fatalf("duplicate result = %+v", blocks[2])
	}
}
