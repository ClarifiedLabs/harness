package workstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlanProgressAndArtifactShareCanonicalState(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(func() string { return dir })
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { now = now.Add(time.Second); return now })

	root, err := store.NewWork("Implement structured work", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		Title:          "Structured work",
		PlanState:      PlanReady,
		Nodes: []NodeDefinition{
			{ID: "phase", Type: NodePhase, Title: "Implementation"},
			{ID: "change", ParentID: "phase", Type: NodeStep, Title: "Change code", Kind: KindChange, Actions: []string{"edit"}, ExitCriteria: []string{"code changed"}},
		},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if planned.ArtifactPath == "" {
		t.Fatal("ready plan did not create artifact")
	}
	artifact, err := os.ReadFile(filepath.Join(dir, planned.ArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifact), "Change code") {
		t.Fatalf("artifact = %q", artifact)
	}

	active, err := store.Progress(ProgressUpdate{StepID: "change", Status: StatusInProgress}, "model")
	if err != nil {
		t.Fatal(err)
	}
	withEvidence, err := store.Progress(ProgressUpdate{
		StepID:   "change",
		Evidence: []EvidenceInput{{Path: "x.go", Summary: "implementation changed"}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	evidenceID := withEvidence.Nodes[1].Evidence[0].ID
	completed, err := store.Progress(ProgressUpdate{
		StepID: "change", Status: StatusCompleted, Summary: "implemented", EvidenceIDs: []string{evidenceID},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if active.ActiveStepID != "change" || completed.Lifecycle != LifecycleCompleted || completed.Nodes[1].Status != StatusCompleted {
		t.Fatalf("active=%+v completed=%+v", active, completed)
	}
	if got := RenderStatus(completed); !strings.Contains(got, "1/1 complete") {
		t.Fatalf("status = %q", got)
	}
}

func TestDecisionGateRequiresNamedEvidenceBatch(t *testing.T) {
	store := NewStore(nil)
	if _, err := store.NewWork("Investigate", "user"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < DecisionThreshold; i++ {
		if _, err := store.RecordInspectionTurn(true, false); err != nil {
			t.Fatal(err)
		}
	}
	if !store.DecisionRequired() {
		t.Fatal("decision gate did not trip")
	}
	state, err := store.Progress(ProgressUpdate{NeedsEvidence: &EvidenceRequest{Question: "Which package owns the state?", Targets: []string{"internal"}}}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if state.Gate.DecisionRequired || state.Gate.AllowanceTurns != EvidenceTurnAllowance {
		t.Fatalf("gate after request = %+v", state.Gate)
	}
	for i := 0; i < EvidenceTurnAllowance; i++ {
		state, err = store.RecordInspectionTurn(true, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !state.Gate.DecisionRequired {
		t.Fatalf("gate after allowance = %+v", state.Gate)
	}
}

func TestMeaningfulProgressClearsEvidenceAllowance(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		PlanState:      PlanDraft,
		Nodes:          []NodeDefinition{{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Progress(ProgressUpdate{BaseRevisionID: planned.RevisionID, NeedsEvidence: &EvidenceRequest{Question: "Which package owns this?"}}, "model")
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.Progress(ProgressUpdate{
		BaseRevisionID: state.RevisionID,
		StepID:         "inspect",
		Evidence:       []EvidenceInput{{Path: "internal/workstate", Summary: "ownership established"}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !gateEmpty(state.Gate) {
		t.Fatalf("gate after progress = %+v", state.Gate)
	}
}

func TestAdministrativeProgressDoesNotClearDecisionGate(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		PlanState:      PlanDraft,
		Nodes:          []NodeDefinition{{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	state := planned
	for range DecisionThreshold {
		state, err = store.RecordInspectionTurn(true, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err = store.Progress(ProgressUpdate{
		BaseRevisionID: state.RevisionID,
		StepID:         "inspect",
		Status:         StatusInProgress,
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Gate.DecisionRequired {
		t.Fatalf("administrative activation cleared gate: %+v", state.Gate)
	}
	state, err = store.SetPlan(PlanUpdate{
		BaseRevisionID: state.RevisionID,
		PlanState:      PlanDraft,
		Nodes: []NodeDefinition{
			{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover},
			{ID: "change", Type: NodeStep, Title: "Change", Kind: KindChange},
		},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !gateEmpty(state.Gate) {
		t.Fatalf("structural plan revision did not clear gate: %+v", state.Gate)
	}
}

func TestCompletedStepRequiresExplicitCitations(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Implement", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		PlanState:      PlanDraft,
		Nodes:          []NodeDefinition{{ID: "change", Type: NodeStep, Title: "Change", Kind: KindChange}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	withEvidence, err := store.Progress(ProgressUpdate{
		BaseRevisionID: planned.RevisionID,
		StepID:         "change",
		Evidence:       []EvidenceInput{{Path: "change.go", Summary: "change made"}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Progress(ProgressUpdate{
		BaseRevisionID: withEvidence.RevisionID,
		StepID:         "change",
		Status:         StatusCompleted,
		Summary:        "done",
	}, "model")
	if err == nil || !strings.Contains(err.Error(), "citations") {
		t.Fatalf("completion error = %v", err)
	}
}

func TestResumeRetainsLineageAndClearsLifecycleDetails(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Keep working", "user")
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := store.Progress(ProgressUpdate{
		BaseRevisionID: root.RevisionID,
		WorkStatus:     LifecycleBlocked,
		Summary:        "waiting on a decision",
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Resume("user")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.WorkID != root.WorkID || resumed.RevisionID == blocked.RevisionID || resumed.Lifecycle != LifecycleActive || len(resumed.Blockers) != 0 {
		t.Fatalf("resumed work = %+v", resumed)
	}
}

func TestBranchCheckoutMakesEvidenceStaleAndRequiresReconciliation(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Branch work", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		Title:          "Branch",
		PlanState:      PlanDraft,
		Nodes:          []NodeDefinition{{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover, Actions: []string{"read"}, ExitCriteria: []string{"understood"}}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Progress(ProgressUpdate{StepID: "inspect", Status: StatusInProgress}, "model"); err != nil {
		t.Fatal(err)
	}
	withEvidence, err := store.Progress(ProgressUpdate{StepID: "inspect", Evidence: []EvidenceInput{{Path: "a.go", Summary: "old evidence"}}}, "model")
	if err != nil {
		t.Fatal(err)
	}
	checkedOut, err := store.Checkout(withEvidence.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if !checkedOut.WorkspaceUnverified || !checkedOut.Nodes[0].Evidence[0].Stale {
		t.Fatalf("checkout = %+v", checkedOut)
	}
	if _, err := store.Progress(ProgressUpdate{StepID: "inspect", Status: StatusCompleted, Summary: "done", EvidenceIDs: []string{checkedOut.Nodes[0].Evidence[0].ID}}, "model"); err == nil || !strings.Contains(err.Error(), "reconciled") {
		t.Fatalf("completion error = %v", err)
	}
	if planned.RevisionID == "" {
		t.Fatal("planned revision missing")
	}
}

func TestRevisionLogReloadAndDigestValidation(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(func() string { return dir })
	state, err := store.NewWork("Persist me", "user")
	if err != nil {
		t.Fatal(err)
	}
	restored := NewStore(func() string { return dir })
	if err := restored.LoadLog(dir); err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(state); err != nil {
		t.Fatal(err)
	}
	if err := restored.ValidateCurrentRevision(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.ndjson"), append(mustRead(t, filepath.Join(dir, "work.ndjson")), []byte("{\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	withTail := NewStore(func() string { return dir })
	if err := withTail.LoadLog(dir); err != nil {
		t.Fatalf("incomplete final line should be ignored: %v", err)
	}
}

func TestParentMustPrecedeChildAndOnlyOptionalMaySkip(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Validate", "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		PlanState:      PlanDraft,
		Nodes: []NodeDefinition{
			{ID: "child", ParentID: "parent", Type: NodeStep, Title: "Child", Kind: KindChange},
			{ID: "parent", Type: NodePhase, Title: "Parent"},
		},
	}, "model")
	if err == nil || !strings.Contains(err.Error(), "must precede") {
		t.Fatalf("plan error = %v", err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		PlanState:      PlanDraft,
		Nodes:          []NodeDefinition{{ID: "required", Type: NodeStep, Title: "Required", Kind: KindChange}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Progress(ProgressUpdate{StepID: "required", Status: StatusSkipped, Summary: "not needed"}, "model"); err == nil {
		t.Fatal("required step was skipped")
	}
	if planned.Nodes[0].Status != StatusPending {
		t.Fatalf("planned node = %+v", planned.Nodes[0])
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
