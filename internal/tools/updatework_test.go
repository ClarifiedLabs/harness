package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/workstate"
)

func TestUpdateWorkSchemaExposesOnlyPlanAndProgressModes(t *testing.T) {
	tool := NewUpdateWork(workstate.NewStore(nil))
	var schema struct {
		Properties map[string]struct {
			Enum       []string                   `json:"enum"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	got := schema.Properties["mode"].Enum
	if len(got) != 2 || got[0] != "plan" || got[1] != "progress" {
		t.Fatalf("mode enum = %v, want [plan progress]", got)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "mode" {
		t.Fatalf("required = %v, want [mode]", schema.Required)
	}
	for _, name := range []string{"work_id", "base_revision_id"} {
		if _, ok := schema.Properties[name]; ok {
			t.Fatalf("schema unexpectedly exposes %q", name)
		}
	}
	for _, name := range []string{"step_id", "next_step_id", "evidence_ids", "result_ids"} {
		if _, ok := schema.Properties["progress"].Properties[name]; ok {
			t.Fatalf("progress schema unexpectedly exposes %q", name)
		}
	}
	for _, name := range []string{"nodes", "state", "active_step_id"} {
		if _, ok := schema.Properties["plan"].Properties[name]; ok {
			t.Fatalf("plan schema unexpectedly exposes %q", name)
		}
	}
	for _, name := range []string{"steps", "questions"} {
		if _, ok := schema.Properties["plan"].Properties[name]; !ok {
			t.Fatalf("plan schema is missing %q", name)
		}
	}
	var steps struct {
		Items struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"items"`
		MinItems int `json:"minItems"`
	}
	if err := json.Unmarshal(schema.Properties["plan"].Properties["steps"], &steps); err != nil {
		t.Fatal(err)
	}
	if steps.MinItems != 1 || len(steps.Items.Required) != 1 || steps.Items.Required[0] != "title" {
		t.Fatalf("steps schema = %+v", steps)
	}
	for _, name := range []string{"id", "parent_id", "type", "status", "active"} {
		if _, ok := steps.Items.Properties[name]; ok {
			t.Fatalf("step schema unexpectedly exposes %q", name)
		}
	}
}

func TestUpdateWorkPlansAndAdvancesCanonicalWork(t *testing.T) {
	dir := t.TempDir()
	store := workstate.NewStore(func() string { return dir })
	_, err := store.NewWork("Implement the feature", "user")
	if err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateWork(store)
	planInput := json.RawMessage(`{
		"mode":"plan",
		"plan":{
			"title":"Feature",
			"steps":[{
				"title":"Change code",
				"kind":"change",
				"description":"edit the implementation",
				"done_when":"the implementation is complete"
			}]
		}
	}`)
	receipt, err := tool.Run(context.Background(), planInput)
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

	progressInput := json.RawMessage(`{
		"mode":"progress",
		"progress":{
			"evidence":[{"path":"feature.go","summary":"implementation changed"}]
		}
	}`)
	if _, err := tool.Run(context.Background(), progressInput); err != nil {
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
	tool := NewUpdateWork(store)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"plan",
		"work_id":"ignored-old-work",
		"base_revision_id":"ignored-old-revision",
		"plan":{"steps":[{"title":"Inspect","kind":"discover"}]}
	}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddEvidence("", []workstate.EvidenceInput{{Path: "host.go", Summary: "host advanced the revision"}}, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"progress",
		"base_revision_id":"ignored-stale-revision",
		"progress":{"status":"completed","summary":"inspection complete"}
	}`)); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); got.Lifecycle != workstate.LifecycleCompleted || workStepByTitle(got, "Inspect").Status != workstate.StatusCompleted {
		t.Fatalf("work state = %+v", got)
	}
}

func TestUpdateWorkPromotesImplicitEvidenceAndAdvancesRunnableSteps(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Implement and verify", "user"); err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateWork(store)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"progress",
		"progress":{"evidence":[{"path":"existing.go","summary":"located the implementation"}]}
	}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"plan",
		"plan":{"steps":[
			{"title":"Change","kind":"change","description":"edit","done_when":"changed"},
			{"title":"Inspect verification","kind":"discover","description":"inspect","done_when":"verified"}
		]}
	}`)); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); activeStepTitle(got) != "Change" {
		t.Fatalf("plan did not select first runnable step: %+v", got)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"progress",
		"progress":{"status":"completed","summary":"implementation changed"}
	}`)); err != nil {
		t.Fatal(err)
	}
	state := store.Snapshot()
	changed := workStepByTitle(state, "Change")
	if changed == nil || changed.Status != workstate.StatusCompleted || activeStepTitle(state) != "Inspect verification" || len(changed.Evidence) != 1 {
		t.Fatalf("state after first completion = %+v", state)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"progress",
		"progress":{"status":"completed","summary":"verification inspected","workspace_reconciled":true,"evidence":[{"path":"existing.go","summary":"verified the implementation"}]}
	}`)); err != nil {
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
	tool := NewUpdateWork(store)
	input := json.RawMessage(`{
		"mode":"plan",
		"plan":{
			"questions":["Which package owns this behavior?"],
			"steps":[{"title":"Inspect ownership"},{"title":"Implement change"},{"title":"Verify behavior"}]
		}
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
	if _, err := tool.Run(context.Background(), json.RawMessage(`{
		"mode":"plan",
		"plan":{"steps":[{"title":"Inspect ownership"},{"title":"Implement change"},{"title":"Verify behavior"}]}
	}`)); err != nil {
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
	tool := NewUpdateWork(store)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"mode":"plan","plan":{"steps":[{"title":"Implement"}]}}`)); err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"mode":"plan","plan":{"steps":[]}}`)); err == nil || !strings.Contains(err.Error(), "at least one step") {
		t.Fatalf("empty plan error = %v", err)
	}
	after := store.Snapshot()
	if activeStepTitle(after) != "Implement" || after.RevisionID != before.RevisionID {
		t.Fatalf("empty plan changed state: before=%+v after=%+v", before, after)
	}
}

func TestUpdateWorkActivityExcludesAdministrativeAndEvidenceRequestUpdates(t *testing.T) {
	tool := NewUpdateWork(workstate.NewStore(nil))
	tests := []struct {
		name     string
		input    string
		progress bool
	}{
		{name: "plan", input: `{"mode":"plan","plan":{"steps":[{"title":"Implement"}]}}`},
		{name: "activate", input: `{"mode":"progress","progress":{"status":"in_progress"}}`},
		{name: "evidence request", input: `{"mode":"progress","progress":{"needs_evidence":{"question":"which package?"}}}`},
		{name: "evidence", input: `{"mode":"progress","progress":{"evidence":[{"path":"x.go","summary":"found"}]}}`, progress: true},
		{name: "complete", input: `{"mode":"progress","progress":{"status":"completed"}}`, progress: true},
		{name: "blocked", input: `{"mode":"progress","progress":{"work_status":"blocked"}}`, progress: true},
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
