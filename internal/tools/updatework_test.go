package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"harness/internal/workstate"
)

func TestUpdateWorkSchemaExposesOnlyPlanAndProgressModes(t *testing.T) {
	tool := NewUpdateWork(workstate.NewStore(nil))
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
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
	if !slices.Contains(schema.Required, "base_revision_id") {
		t.Fatalf("required = %v, want base_revision_id", schema.Required)
	}
}

func TestUpdateWorkPlansAndAdvancesCanonicalWork(t *testing.T) {
	store := workstate.NewStore(nil)
	root, err := store.NewWork("Implement the feature", "user")
	if err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateWork(store)
	planInput := json.RawMessage(`{
		"mode":"plan",
		"work_id":"` + root.WorkID + `",
		"base_revision_id":"` + root.RevisionID + `",
		"plan":{
			"title":"Feature",
			"state":"ready",
			"active_step_id":"change",
			"nodes":[{
				"id":"change",
				"type":"step",
				"title":"Change code",
				"kind":"change",
				"actions":["edit the implementation"],
				"exit_criteria":["the implementation is complete"]
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

	state := store.Snapshot()
	progressInput := json.RawMessage(`{
		"mode":"progress",
		"work_id":"` + state.WorkID + `",
		"base_revision_id":"` + state.RevisionID + `",
		"progress":{
			"step_id":"change",
			"evidence":[{"path":"feature.go","summary":"implementation changed"}]
		}
	}`)
	if _, err := tool.Run(context.Background(), progressInput); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got.Nodes[0].Evidence) != 1 {
		t.Fatalf("work state = %+v", got)
	}
}

func TestUpdateWorkRejectsStaleWorkAndRevision(t *testing.T) {
	store := workstate.NewStore(nil)
	root, err := store.NewWork("Current task", "user")
	if err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateWork(store)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"mode":"progress","work_id":"old","progress":{"work_status":"active"}}`)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale work error = %v", err)
	}
	input := json.RawMessage(`{
		"mode":"plan",
		"work_id":"` + root.WorkID + `",
		"base_revision_id":"old",
		"plan":{"state":"draft","nodes":[]}
	}`)
	if _, err := tool.Run(context.Background(), input); err == nil || !strings.Contains(err.Error(), "stale work revision") {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestUpdateWorkRequiresRevisionForEveryMode(t *testing.T) {
	store := workstate.NewStore(nil)
	root, err := store.NewWork("Current task", "user")
	if err != nil {
		t.Fatal(err)
	}
	tool := NewUpdateWork(store)
	input := json.RawMessage(`{
		"mode":"plan",
		"work_id":"` + root.WorkID + `",
		"plan":{"state":"draft","nodes":[]}
	}`)
	if _, err := tool.Run(context.Background(), input); err == nil || !strings.Contains(err.Error(), "requires base_revision_id") {
		t.Fatalf("missing revision error = %v", err)
	}
}

func TestUpdateWorkActivityExcludesAdministrativeAndEvidenceRequestUpdates(t *testing.T) {
	tool := NewUpdateWork(workstate.NewStore(nil))
	tests := []struct {
		name     string
		input    string
		progress bool
	}{
		{name: "plan", input: `{"mode":"plan","plan":{"state":"draft","nodes":[]}}`},
		{name: "activate", input: `{"mode":"progress","progress":{"status":"in_progress"}}`},
		{name: "next step", input: `{"mode":"progress","progress":{"next_step_id":"change"}}`},
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
