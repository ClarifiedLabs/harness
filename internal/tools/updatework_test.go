package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/workstate"
)

func TestWorkToolsExposeSplitBoundedSchemas(t *testing.T) {
	planTool := NewSetWorkPlan(workstate.NewStore(nil))
	var schema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(planTool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "steps" || schema.AdditionalProperties {
		t.Fatalf("plan schema = %+v", schema)
	}
	for _, name := range []string{"mode", "plan", "progress", "work_id", "base_revision_id", "nodes", "active_step_id"} {
		if _, ok := schema.Properties[name]; ok {
			t.Fatalf("plan schema unexpectedly exposes %q", name)
		}
	}
	for _, name := range []string{"steps", "questions"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("plan schema is missing %q", name)
		}
	}
	var steps struct {
		Items struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"items"`
		MinItems int `json:"minItems"`
		MaxItems int `json:"maxItems"`
	}
	if err := json.Unmarshal(schema.Properties["steps"], &steps); err != nil {
		t.Fatal(err)
	}
	if steps.MinItems != 1 || steps.MaxItems != 64 || len(steps.Items.Required) != 1 || steps.Items.Required[0] != "title" {
		t.Fatalf("steps schema = %+v", steps)
	}
	for _, name := range []string{"id", "parent_id", "type", "status", "active"} {
		if _, ok := steps.Items.Properties[name]; ok {
			t.Fatalf("step schema unexpectedly exposes %q", name)
		}
	}

	progressTool := NewUpdateWork(workstate.NewStore(nil))
	var progressSchema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(progressTool.Schema(), &progressSchema); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"status", "summary", "evidence", "work_status"} {
		if _, ok := progressSchema.Properties[name]; !ok {
			t.Fatalf("progress schema is missing %q", name)
		}
	}
	for _, name := range []string{"mode", "progress", "plan", "needs_evidence", "step_id", "next_step_id", "evidence_ids", "result_ids"} {
		if _, ok := progressSchema.Properties[name]; ok {
			t.Fatalf("progress schema unexpectedly exposes %q", name)
		}
	}
	if progressSchema.AdditionalProperties {
		t.Fatal("progress schema permits unknown fields")
	}
	var workStatus struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(progressSchema.Properties["work_status"], &workStatus); err != nil {
		t.Fatal(err)
	}
	if slicesContain(workStatus.Enum, "completed") || len(progressSchema.Required) != 1 || progressSchema.Required[0] != "summary" {
		t.Fatalf("work_status schema = %+v", workStatus)
	}
	registry := &Registry{}
	registry.Register(progressTool)
	if specs := registry.Specs(); len(specs) != 1 || strings.Contains(string(specs[0].Parameters), `"description"`) || strings.Contains(string(specs[0].Parameters), `"maxLength"`) || !strings.Contains(specs[0].Description, "Complete one step at a time") {
		t.Fatalf("model-facing progress schema is not compact: %+v", specs)
	}
}

func TestUpdateWorkPlansAndAdvancesCanonicalWork(t *testing.T) {
	dir := t.TempDir()
	store := workstate.NewStore(func() string { return dir })
	_, err := store.NewWork("Implement the feature", "user")
	if err != nil {
		t.Fatal(err)
	}
	planTool := NewSetWorkPlan(store)
	progressTool := NewUpdateWork(store)
	planInput := json.RawMessage(`{
		"title":"Feature",
		"steps":[{
			"title":"Change code",
			"kind":"change",
			"description":"edit the implementation",
			"done_when":"the implementation is complete"
		}]
	}`)
	receipt, err := planTool.Run(context.Background(), planInput)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt, "active: Change code") {
		t.Fatalf("plan receipt = %q", receipt)
	}
	planned := store.Snapshot()
	if planned.PlanState != workstate.PlanReady || planned.ArtifactPath == "" || strings.Contains(receipt, planned.ArtifactPath) {
		t.Fatalf("plan state/receipt = %+v / %q", planned, receipt)
	}

	progressInput := json.RawMessage(`{"summary":"recorded the implementation change","evidence":[{"path":"feature.go","summary":"implementation changed"}]}`)
	if _, err := progressTool.Run(context.Background(), progressInput); err != nil {
		t.Fatal(err)
	}
	if got := workStepByTitle(store.Snapshot(), "Change code"); got == nil || len(got.Evidence) != 1 {
		t.Fatalf("work state = %+v", got)
	}
}

func TestUpdateWorkBindsToCurrentWorkAcrossHostRevisions(t *testing.T) {
	store := workstate.NewStore(nil)
	_, err := store.NewWork("Current task", "user")
	if err != nil {
		t.Fatal(err)
	}
	planTool := NewSetWorkPlan(store)
	progressTool := NewUpdateWork(store)
	if _, err := planTool.Run(context.Background(), json.RawMessage(`{"steps":[{"title":"Inspect","kind":"discover"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddEvidence("", []workstate.EvidenceInput{{Path: "host.go", Summary: "host advanced the revision"}}, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := progressTool.Run(context.Background(), json.RawMessage(`{"status":"completed","summary":"inspection complete"}`)); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); got.Lifecycle != workstate.LifecycleCompleted || workStepByTitle(got, "Inspect").Status != workstate.StatusCompleted {
		t.Fatalf("work state = %+v", got)
	}
}

func TestUpdateWorkRejectsModelManagedWorkCompletion(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Review telemetry", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetWorkPlan(store).Run(context.Background(), json.RawMessage(`{"steps":[{"title":"Inspect"},{"title":"Report"}]}`)); err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	registry := &Registry{}
	registry.Register(NewUpdateWork(store))
	result := registry.Dispatch(context.Background(), llm.ToolCall{
		ID:    "work-complete",
		Name:  "update_work",
		Input: json.RawMessage(`{"status":"completed","work_status":"completed","summary":"all done","evidence":[{"path":"internal/tools/runcommand.go","summary":"reviewed"}]}`),
	})
	if !result.IsError || result.ErrorKind != llm.ToolErrorInvalidArgs || !strings.Contains(result.Text, "Harness will complete work automatically") {
		t.Fatalf("result = %+v", result)
	}
	after := store.Snapshot()
	if after.RevisionID != before.RevisionID || after.Lifecycle != workstate.LifecycleActive || activeStepTitle(after) != "Inspect" {
		t.Fatalf("invalid lifecycle completion changed state: before=%+v after=%+v", before, after)
	}
}

func TestUpdateWorkEnforcesTextLimitsOutsideModelSchema(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Review telemetry", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetWorkPlan(store).Run(context.Background(), json.RawMessage(`{"steps":[{"title":"Inspect"}]}`)); err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateWork(store)
	tooLongSummary, _ := json.Marshal(map[string]any{"summary": strings.Repeat("s", 2049)})
	if _, err := tool.Run(context.Background(), tooLongSummary); err == nil || !strings.Contains(err.Error(), "summary exceeds 2048 runes") {
		t.Fatalf("summary limit error = %v", err)
	}
	tooLongSymbol, _ := json.Marshal(map[string]any{
		"summary":  "record source evidence",
		"evidence": []map[string]any{{"path": "internal/tools/runcommand.go", "symbol": strings.Repeat("s", 1025), "summary": "located outcome decoder"}},
	})
	if _, err := tool.Run(context.Background(), tooLongSymbol); err == nil || !strings.Contains(err.Error(), "evidence symbol exceeds 1024 runes") {
		t.Fatalf("symbol limit error = %v", err)
	}
}

func TestUpdateWorkPromotesImplicitEvidenceAndAdvancesRunnableSteps(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Implement and verify", "user"); err != nil {
		t.Fatal(err)
	}
	planTool := NewSetWorkPlan(store)
	progressTool := NewUpdateWork(store)
	if _, err := progressTool.Run(context.Background(), json.RawMessage(`{"summary":"located the implementation","evidence":[{"path":"existing.go","summary":"located the implementation"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := planTool.Run(context.Background(), json.RawMessage(`{"steps":[
			{"title":"Change","kind":"change","description":"edit","done_when":"changed"},
			{"title":"Inspect verification","kind":"discover","description":"inspect","done_when":"verified"}
		]
	}`)); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); activeStepTitle(got) != "Change" {
		t.Fatalf("plan did not select first runnable step: %+v", got)
	}
	if _, err := progressTool.Run(context.Background(), json.RawMessage(`{"status":"completed","summary":"implementation changed"}`)); err != nil {
		t.Fatal(err)
	}
	state := store.Snapshot()
	changed := workStepByTitle(state, "Change")
	if changed == nil || changed.Status != workstate.StatusCompleted || activeStepTitle(state) != "Inspect verification" || len(changed.Evidence) != 1 {
		t.Fatalf("state after first completion = %+v", state)
	}
	if _, err := progressTool.Run(context.Background(), json.RawMessage(`{"status":"completed","summary":"verification inspected","workspace_reconciled":true,"evidence":[{"path":"existing.go","summary":"verified the implementation"}]}`)); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); got.Lifecycle != workstate.LifecycleCompleted || workStepByTitle(got, "Inspect verification").Status != workstate.StatusCompleted {
		t.Fatalf("final work state = %+v", got)
	}
}

func TestUpdateWorkInfersDraftPlanAndStableHostStructure(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Plan the change", "user"); err != nil {
		t.Fatal(err)
	}
	tool := NewSetWorkPlan(store)
	input := json.RawMessage(`{
		"questions":["Which package owns this behavior?"],
		"steps":[{"title":"Inspect ownership"},{"title":"Implement change"},{"title":"Verify behavior"}]
	}`)
	if _, err := tool.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	first := store.Snapshot()
	if first.PlanState != workstate.PlanDraft || first.ActiveStepID != "" || len(first.Questions) != 1 {
		t.Fatalf("draft plan = %+v", first)
	}
	for _, title := range []string{"Inspect ownership", "Implement change", "Verify behavior"} {
		step := workStepByTitle(first, title)
		if step == nil || step.ID == "" || step.ParentID == "" || len(step.Actions) != 1 || len(step.ExitCriteria) != 1 {
			t.Fatalf("host-normalized step %q = %+v", title, step)
		}
	}
	inspectID := workStepByTitle(first, "Inspect ownership").ID
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"steps":[{"title":"Inspect ownership"},{"title":"Implement change"},{"title":"Verify behavior"}]}`)); err != nil {
		t.Fatal(err)
	}
	second := store.Snapshot()
	if second.PlanState != workstate.PlanReady || workStepByTitle(second, "Inspect ownership").ID != inspectID || activeStepTitle(second) != "Inspect ownership" {
		t.Fatalf("ready plan = %+v", second)
	}
}

func TestUpdateWorkRejectsEmptyPlanWithoutReplacingCurrentStructure(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Keep the plan", "user"); err != nil {
		t.Fatal(err)
	}
	tool := NewSetWorkPlan(store)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"steps":[{"title":"Implement"}]}`)); err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"steps":[]}`)); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("empty plan error = %v", err)
	}
	after := store.Snapshot()
	if activeStepTitle(after) != "Implement" || after.RevisionID != before.RevisionID {
		t.Fatalf("empty plan changed state: before=%+v after=%+v", before, after)
	}
}

func TestUpdateWorkActivityExcludesAdministrativeUpdates(t *testing.T) {
	tool := NewUpdateWork(workstate.NewStore(nil))
	tests := []struct {
		name     string
		input    string
		progress bool
	}{
		{name: "activate", input: `{"status":"in_progress"}`},
		{name: "summary only", input: `{"summary":"starting"}`},
		{name: "evidence", input: `{"evidence":[{"path":"x.go","summary":"found"}]}`, progress: true},
		{name: "complete", input: `{"status":"completed"}`, progress: true},
		{name: "blocked", input: `{"work_status":"blocked"}`, progress: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			activity := tool.(ActivityReporter).Activity(json.RawMessage(tc.input))
			if activity.ExplicitProgress != tc.progress {
				t.Fatalf("ExplicitProgress = %v, want %v", activity.ExplicitProgress, tc.progress)
			}
		})
	}
}

func workStepByTitle(state *workstate.State, title string) *workstate.Node {
	if state == nil {
		return nil
	}
	for i := range state.Nodes {
		if state.Nodes[i].Type == workstate.NodeStep && state.Nodes[i].Title == title {
			return &state.Nodes[i]
		}
	}
	return nil
}

func activeStepTitle(state *workstate.State) string {
	if state == nil {
		return ""
	}
	for _, node := range state.Nodes {
		if node.ID == state.ActiveStepID {
			return node.Title
		}
	}
	return ""
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
