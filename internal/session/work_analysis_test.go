package session

import (
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/workstate"
)

func TestAnalyzeWorkReportsBoundedLifecycleMetrics(t *testing.T) {
	dir := t.TempDir()
	store := workstate.NewStore(func() string { return dir })
	root, err := store.NewWork("SECRET objective body", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(workstate.PlanUpdate{
		BaseRevisionID: root.RevisionID,
		Title:          "Plan",
		PlanState:      workstate.PlanReady,
		Nodes: []workstate.NodeDefinition{{
			ID: "change", Type: workstate.NodeStep, Title: "Change", Kind: workstate.KindChange,
			Actions: []string{"edit"}, ExitCriteria: []string{"done"},
		}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Progress(workstate.ProgressUpdate{BaseRevisionID: planned.RevisionID, StepID: "change", Status: workstate.StatusInProgress}, "model")
	if err != nil {
		t.Fatal(err)
	}
	withResult, err := store.AddResults("change", []workstate.Result{{Kind: workstate.ResultChange, Path: "secret.go"}}, "host")
	if err != nil {
		t.Fatal(err)
	}
	resultID := withResult.Nodes[0].Results[0].ID
	if _, err := store.Progress(workstate.ProgressUpdate{
		BaseRevisionID: withResult.RevisionID, StepID: "change", Status: workstate.StatusCompleted,
		Summary: "done", ResultIDs: []string{resultID},
	}, "model"); err != nil {
		t.Fatal(err)
	}

	events := []Event{
		{Type: EventTurnProgress, WorkID: root.WorkID, WorkStepID: "change", TurnProgress: &TurnProgressSnapshot{Activity: map[string]int{"inspect": 1}}},
		{Type: EventTurnProgress, WorkID: root.WorkID, WorkStepID: "change", TurnProgress: &TurnProgressSnapshot{Activity: map[string]int{"inspect": 1}}},
		{Type: EventTurnProgress, WorkID: root.WorkID, WorkStepID: "change", TurnProgress: &TurnProgressSnapshot{SuccessfulMutation: true}},
	}
	analysis, err := analyzeWork(dir, active.CreatedAt.AddDate(1, 0, 0), events)
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.Available || analysis.WorkItems != 1 || analysis.Structured != 1 || analysis.Completed != 1 || analysis.MutationResults != 1 || analysis.StepTransitions < 2 {
		t.Fatalf("analysis = %+v", analysis)
	}
	if analysis.AttributedSteps != 1 || analysis.AttributedToolTurns != 3 || analysis.InspectionOperations != 2 || analysis.MutationTurns != 1 || analysis.TurnsToFirstMutation.Samples != 1 || analysis.TurnsToFirstMutation.Median != 3 {
		t.Fatalf("step analysis = %+v", analysis)
	}
	if strings.Contains(strings.TrimSpace(toJSON(t, analysis)), "SECRET") || strings.Contains(toJSON(t, analysis), "secret.go") {
		t.Fatalf("analysis leaked work content: %s", toJSON(t, analysis))
	}
}

func toJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
