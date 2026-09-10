package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestPlanToolStagesRecordsEmissionAndResolution(t *testing.T) {
	calls := stageCalls(`{}`, `{"_stage":3}`, `{"_stage":1}`, `{}`)
	execution, _, order, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	wantStage := []int{1, 3, 1, 1}
	wantEmitted := []string{"", "3", "1", ""}
	for i, original := range order {
		stage := execution[i].Stage
		if stage == nil || stage.EmissionIndex != original+1 || stage.Resolved != wantStage[original] || string(stage.Emitted) != wantEmitted[original] || stage.BatchRejected {
			t.Fatalf("execution[%d] stage = %+v", i, stage)
		}
		if calls[original].Stage != nil {
			t.Fatalf("raw call mutated: %+v", calls[original])
		}
	}
}

func TestPlanToolStagesRecordsInvalidValues(t *testing.T) {
	for _, value := range []string{`null`, `0`, `-1`, `1.5`, `"later"`, `true`, `{}`, `[]`, `99999999999999999999999`} {
		t.Run(value, func(t *testing.T) {
			calls := stageCalls(`{"_stage":2}`, fmt.Sprintf(`{"_stage":%s}`, value), `{}`)
			execution, _, _, err := planToolStages(calls)
			if err == nil {
				t.Fatal("accepted invalid stage")
			}
			for i, call := range execution {
				if call.Stage == nil || !call.Stage.BatchRejected || call.Stage.EmissionIndex != i+1 {
					t.Fatalf("call %d missing rejection metadata: %+v", i, call.Stage)
				}
				if calls[i].Stage != nil {
					t.Fatal("mutated emitted call")
				}
			}
			if bad := execution[1].Stage; string(bad.Emitted) != value || bad.Resolved != 0 {
				t.Fatalf("invalid stage = %+v", bad)
			}
			if execution[0].Stage.Resolved != 2 || execution[2].Stage.Resolved != 2 || len(execution[2].Stage.Emitted) != 0 {
				t.Fatalf("valid labels/inheritance lost: %+v", execution)
			}
		})
	}
}

func TestPostToolUseHookClearsReplacedErrorDetails(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "probe", run: func(context.Context, json.RawMessage) (string, error) {
		return "", tools.WithKind(&tools.BackgroundLeaseConflictError{BlockingJobID: "blocker"}, llm.ToolErrorLeaseConflict)
	}})
	runner := testHookRunner(t, `{"PostToolUse":[{"hooks":[{"type":"command","command":"printf '{\"continue\":false,\"reason\":\"redacted\"}'"}]}]}`)
	a := newAgent(llmtest.New("fake"), reg, Options{Hooks: runner})
	result, _ := a.dispatchOne(context.Background(), llm.ToolCall{ID: "call", Name: "probe", Input: json.RawMessage(`{}`)}, 1, 1, &recordSink{})
	if result.Text != "redacted" || !result.IsError || result.ErrorKind != llm.ToolErrorHookBlocked || result.ErrorDetails != nil {
		t.Fatalf("hook replacement retained stale diagnostics: %+v", result)
	}
}

func TestSafeToolResultForSinkOwnsErrorDetails(t *testing.T) {
	result := llm.ToolResult{ErrorDetails: &llm.ToolErrorDetails{LeaseConflict: &llm.LeaseConflictDetails{BlockingJobID: "original"}}}
	clone := safeToolResultForSink(result)
	clone.ErrorDetails.LeaseConflict.BlockingJobID = "changed"
	if result.ErrorDetails.LeaseConflict.BlockingJobID != "original" {
		t.Fatal("sink can mutate source error details")
	}
	block := resultBlock(result, "delegate")
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ErrorDetails", "error_details", "Stage", "tool_stage"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("diagnostics leaked into content: %s", encoded)
		}
	}
}
