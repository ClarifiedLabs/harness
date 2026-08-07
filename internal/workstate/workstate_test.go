package workstate

import (
	"fmt"
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
	if context := RequestContext(planned); strings.Contains(context, planned.ArtifactPath) {
		t.Fatalf("model context exposes internal artifact path: %q", context)
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

func TestDecisionGateAutomaticallyGrantsOneBoundedGrace(t *testing.T) {
	store := NewStore(nil)
	state, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.SetPlan(PlanUpdate{
		BaseRevisionID: state.RevisionID,
		PlanState:      PlanDraft,
		ActiveStepID:   "inspect",
		Nodes:          []NodeDefinition{{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.RecordInspectionOperations(state.ActiveStepID, DecisionOperationThreshold-1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Gate.DecisionRequired {
		t.Fatalf("gate tripped early: %+v", state.Gate)
	}
	state, err = store.RecordInspectionOperations(state.ActiveStepID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if store.DecisionRequired() || state.Gate.GraceOperations != InspectionGraceOperations || !state.Gate.GraceUsed {
		t.Fatalf("automatic grace was not started: %+v", state.Gate)
	}
	if context := RequestContext(state); !strings.Contains(context, "automatic grace") {
		t.Fatalf("request context = %q", context)
	}
	state, err = store.RecordInspectionOperations(state.ActiveStepID, InspectionGraceOperations)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Gate.DecisionRequired {
		t.Fatalf("gate after grace = %+v", state.Gate)
	}
}

func TestDecisionGateChargesThresholdOvershootAgainstGrace(t *testing.T) {
	store := NewStore(nil)
	state, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.RecordInspectionOperations(InspectionStepID(state), DecisionOperationThreshold+InspectionGraceOperations)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Gate.DecisionRequired || state.Gate.GraceOperations != 0 || !state.Gate.GraceUsed {
		t.Fatalf("oversized inspection batch bypassed hard gate: %+v", state.Gate)
	}
}

func TestSelectedEvidenceDoesNotRenewAutomaticGrace(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID,
		PlanState:      PlanDraft,
		ActiveStepID:   "inspect",
		Nodes:          []NodeDefinition{{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.RecordInspectionOperations(planned.ActiveStepID, DecisionOperationThreshold)
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
	if state.Gate.GraceOperations != InspectionGraceOperations || !state.Gate.GraceUsed {
		t.Fatalf("selected evidence renewed or cleared grace: %+v", state.Gate)
	}
}

func TestChecklistPhaseBoundaryAndHostEvidence(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(func() string { return dir })
	root, err := store.NewWork("ship it", "user")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := store.SetPlan(PlanUpdate{
		BaseRevisionID: root.RevisionID, PlanState: PlanDraft, ActiveStepID: "inspect",
		Nodes: []NodeDefinition{
			{ID: "discover", Type: NodePhase, Title: "Discover"},
			{ID: "inspect", ParentID: "discover", Type: NodeStep, Title: "Inspect", Kind: KindDiscover},
			{ID: "change", Type: NodePhase, Title: "Change"},
			{ID: "edit", ParentID: "change", Type: NodeStep, Title: "Edit", Kind: KindChange},
		},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !CrossesPhase(*planned, "inspect", "edit") || CrossesPhase(*planned, "inspect", "inspect") {
		t.Fatalf("phase boundary classification failed: %+v", planned.Nodes)
	}
	withEvidence, err := store.AddEvidence("inspect", []EvidenceInput{{Kind: EvidenceArtifact, Path: "/tmp/evidence.txt", Summary: "inspection result", ToolCallID: "call-1"}}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if len(withEvidence.Nodes[1].Evidence) != 1 || withEvidence.Nodes[1].Evidence[0].ToolCallID != "call-1" {
		t.Fatalf("host evidence = %+v", withEvidence.Nodes[1].Evidence)
	}
	checklist := RenderChecklist(*withEvidence)
	if !strings.Contains(checklist, "[>] Inspect") || !strings.Contains(checklist, "[ ] Edit") || strings.Contains(checklist, "Discover") {
		t.Fatalf("checklist = %q", checklist)
	}
}

func TestAutomaticEvidenceRollsWithoutEvictingSelectedEvidence(t *testing.T) {
	store := NewStore(nil)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time {
		now = now.Add(time.Second)
		return now
	})
	state, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.Progress(ProgressUpdate{
		StepID: state.ActiveStepID,
		Evidence: []EvidenceInput{{
			Path: "internal/workstate/workstate.go", Summary: "model-selected evidence",
		}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	selectedID := state.Nodes[0].Evidence[0].ID
	for i := 0; i < maxEvidencePerNode; i++ {
		state, err = store.AddEvidence(state.ActiveStepID, []EvidenceInput{{
			Kind: EvidenceArtifact, Path: filepath.Join("artifacts", fmt.Sprintf("receipt-%d", i)),
			Summary: "automatic inspection receipt", ToolCallID: fmt.Sprintf("call-%d", i),
		}}, "host")
		if err != nil {
			t.Fatal(err)
		}
	}
	node := state.Nodes[0]
	if len(node.Evidence) != maxEvidencePerNode {
		t.Fatalf("retained evidence = %d, want %d", len(node.Evidence), maxEvidencePerNode)
	}
	if node.Evidence[0].ID != selectedID || node.Evidence[0].ToolCallID != "" {
		t.Fatalf("selected evidence was evicted: %+v", node.Evidence[0])
	}
	retainedCalls := make(map[string]bool, len(node.Evidence))
	for _, evidence := range node.Evidence {
		retainedCalls[evidence.ToolCallID] = true
	}
	if retainedCalls["call-0"] || !retainedCalls[fmt.Sprintf("call-%d", maxEvidencePerNode-1)] {
		t.Fatalf("automatic receipt window = %+v", retainedCalls)
	}
	state, err = store.AddResults(state.ActiveStepID, []Result{{Kind: ResultDelegate, Detail: "durable selected result"}}, "host")
	if err != nil {
		t.Fatal(err)
	}
	node = state.Nodes[0]
	if len(node.Evidence)+len(node.Results) != maxEvidencePerNode || len(node.Results) != 1 {
		t.Fatalf("rolling observations = %d evidence, %d results", len(node.Evidence), len(node.Results))
	}
	if node.Evidence[0].ID != selectedID {
		t.Fatalf("selected evidence was evicted for result: %+v", node.Evidence)
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
		ActiveStepID:   "inspect",
		Nodes: []NodeDefinition{
			{ID: "inspect", Type: NodeStep, Title: "Inspect", Kind: KindDiscover},
			{ID: "change", Type: NodeStep, Title: "Change", Kind: KindChange},
		},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.RecordInspectionOperations(planned.ActiveStepID, DecisionOperationThreshold+InspectionGraceOperations)
	if err != nil {
		t.Fatal(err)
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
			{ID: "verify", Type: NodeStep, Title: "Verify", Kind: KindVerify},
		},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Gate.DecisionRequired {
		t.Fatalf("same-step plan revision cleared gate: %+v", state.Gate)
	}
	state, err = store.AddResults("inspect", []Result{{Kind: ResultDelegate, Detail: "inspection complete"}}, "host")
	if err != nil {
		t.Fatal(err)
	}
	resultID := state.Nodes[0].Results[0].ID
	state, err = store.Progress(ProgressUpdate{
		BaseRevisionID: state.RevisionID,
		StepID:         "inspect",
		Status:         StatusCompleted,
		Summary:        "inspection complete",
		ResultIDs:      []string{resultID},
		NextStepID:     "change",
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !gateEmpty(state.Gate) {
		t.Fatalf("step transition did not clear gate: %+v", state.Gate)
	}
}

func TestCompletedStepSelectsFreshEvidence(t *testing.T) {
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
	completed, err := store.Progress(ProgressUpdate{
		BaseRevisionID: withEvidence.RevisionID,
		StepID:         "change",
		Status:         StatusCompleted,
		Summary:        "done",
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Nodes[0].CompletionRefs) != 1 || completed.Nodes[0].CompletionRefs[0] != withEvidence.Nodes[0].Evidence[0].ID {
		t.Fatalf("completion refs = %v", completed.Nodes[0].CompletionRefs)
	}
}

func TestImplicitEvidenceContextAndAutoCompletionHideInternalIDs(t *testing.T) {
	store := NewStore(nil)
	root, err := store.NewWork("Investigate", "user")
	if err != nil {
		t.Fatal(err)
	}
	withEvidence, err := store.AddEvidence(InspectionStepID(root), []EvidenceInput{{Path: "internal/example.go", Summary: "located the implementation", ToolCallID: "read-1"}}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if withEvidence.ActiveStepID != implicitStepID || len(withEvidence.Nodes) != 1 || len(withEvidence.Nodes[0].Evidence) != 1 {
		t.Fatalf("implicit evidence state = %+v", withEvidence)
	}
	context := RequestContext(withEvidence)
	for _, internalID := range []string{root.WorkID, withEvidence.RevisionID, implicitStepID, withEvidence.Nodes[0].Evidence[0].ID} {
		if strings.Contains(context, internalID) {
			t.Fatalf("request context exposes internal id %q: %q", internalID, context)
		}
	}
	if _, err := store.RecordInspectionOperations(InspectionStepID(withEvidence), DecisionOperationThreshold+InspectionGraceOperations); err != nil {
		t.Fatal(err)
	}
	completed, err := store.AutoCompleteImplicit("investigation complete")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Lifecycle != LifecycleCompleted || completed.ActiveStepID != "" || completed.Nodes[0].Status != StatusCompleted || len(completed.Nodes[0].CompletionRefs) != 1 || !gateEmpty(completed.Gate) {
		t.Fatalf("completed implicit work = %+v", completed)
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
	if planned.Nodes[0].Status != StatusInProgress || planned.ActiveStepID != "required" {
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
