package trajectory

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEvaluationProjectionAssignsIDsAndTracksImprovement(t *testing.T) {
	zero := 0.0
	remaining := 2
	state := ApplyEvaluation(State{}, EvaluationInput{
		Handler: "verify", Accepted: false, Score: &zero,
		RemainingRequirements: &remaining, Prompt: 3, Turn: 4,
	})
	if state.Schema != SchemaVersion || state.Epoch != 1 || state.ObjectiveRef != "prompt:3" || state.TotalEvaluations != 1 || state.RejectedEvaluations != 1 {
		t.Fatalf("rejected state = %+v", state)
	}
	if state.CurrentCandidateID != "candidate-e1-000001" || state.MissingCandidateIDs != 1 || state.MissingEvidenceRefs != 1 || state.EvaluationsSinceImprovement != 1 {
		t.Fatalf("fallback state = %+v", state)
	}
	if len(state.Evaluations) != 1 || state.Evaluations[0].ID != "eval-e1-000001" || state.Evaluations[0].Score == nil || *state.Evaluations[0].Score != 0 || state.Evaluations[0].FailureClass != "evaluator_rejected" {
		t.Fatalf("evaluation = %+v", state.Evaluations)
	}

	score := 0.75
	state = ApplyEvaluation(state, EvaluationInput{
		Handler: "verify", Accepted: true, Score: &score, Candidate: "sha256:accepted",
		EvidenceRef: "artifacts/verify.log", Prompt: 3, Turn: 5,
	})
	if state.BestAcceptedCandidateID != "sha256:accepted" || state.BestAcceptedScore == nil || *state.BestAcceptedScore != 0.75 ||
		state.LastImprovementEvaluationID != "eval-e1-000002" || state.EvaluationsSinceImprovement != 0 || state.AcceptedEvaluations != 1 {
		t.Fatalf("accepted state = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{
		Handler: "verify", Accepted: false, Candidate: "sha256:rejected", EvidenceRef: "artifacts/rejected.log",
	})
	if state.CurrentCandidateID != "sha256:rejected" || state.BestAcceptedCandidateID != "sha256:accepted" || state.LastImprovementEvaluationID != "eval-e1-000002" || state.EvaluationsSinceImprovement != 1 {
		t.Fatalf("rejection replaced accepted best: %+v", state)
	}
}

func TestOrderedScoreStagnationTracksBestAndResetsOnImprovement(t *testing.T) {
	state := State{}
	apply := func(score float64, accepted bool, candidate string) {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "verify", Accepted: accepted, Score: &score,
			ScoreDirection: ScoreDirectionMaximize, Candidate: candidate,
		})
	}
	apply(0, false, "candidate:a")
	if state.NoImprovementStreak != 0 || state.StagnationBaselines != 1 {
		t.Fatalf("initial score baseline = %+v", state)
	}
	apply(50, false, "candidate:b")
	apply(50, false, "candidate:c")
	apply(40, false, "candidate:d")
	if state.NoImprovementStreak != 2 || state.MaxNoImprovementStreak != 2 || state.StagnationImprovements != 1 || state.StagnationPlateaus != 1 || state.StagnationRegressions != 1 {
		t.Fatalf("ordered plateau/regression = %+v", state)
	}
	apply(60, false, "candidate:e")
	if state.NoImprovementStreak != 0 || state.StagnationBestScore == nil || *state.StagnationBestScore != 60 || state.StagnationImprovements != 2 {
		t.Fatalf("ordered improvement = %+v", state)
	}
	apply(55, true, "candidate:accepted")
	if state.NoImprovementStreak != 0 || state.StagnationImprovements != 3 || state.StagnationBestScore == nil || *state.StagnationBestScore != 60 {
		t.Fatalf("acceptance did not reset without replacing best score = %+v", state)
	}
}

func TestMinimizedScoreAndRemainingRequirementsDriveStagnation(t *testing.T) {
	state := State{}
	for i, score := range []float64{10, 8, 8, 9} {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "latency", Score: &score, ScoreDirection: ScoreDirectionMinimize,
			Candidate: fmt.Sprintf("candidate:%d", i),
		})
	}
	if state.StagnationBestScore == nil || *state.StagnationBestScore != 8 || state.NoImprovementStreak != 2 || state.StagnationImprovements != 1 || state.StagnationPlateaus != 1 || state.StagnationRegressions != 1 {
		t.Fatalf("minimized score state = %+v", state)
	}

	state = ApplyBranch(state)
	for i, remaining := range []int{5, 3, 3, 4} {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "requirements", RemainingRequirements: &remaining,
			Candidate: fmt.Sprintf("remaining:%d", i),
		})
	}
	if state.StagnationBestRemaining == nil || *state.StagnationBestRemaining != 3 || state.NoImprovementStreak != 2 || state.MaxNoImprovementStreak != 2 || state.StagnationImprovements != 2 || state.StagnationPlateaus != 2 || state.StagnationRegressions != 2 {
		t.Fatalf("remaining-requirement state = %+v", state)
	}
}

func TestUnorderedNewCandidatesRemainIndeterminate(t *testing.T) {
	state := ApplyEvaluation(State{}, EvaluationInput{Handler: "verify", Candidate: "candidate:a"})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Candidate: "candidate:b"})
	if state.NoImprovementStreak != 0 || state.StagnationIndeterminate != 1 {
		t.Fatalf("new unscored candidate created stagnation = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Candidate: "candidate:b"})
	if state.NoImprovementStreak != 1 || state.StagnationPlateaus != 1 {
		t.Fatalf("repeated candidate was not a plateau = %+v", state)
	}

	score10 := 10.0
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &score10, Candidate: "candidate:c"})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &score10, Candidate: "candidate:d"})
	if state.NoImprovementStreak != 2 || state.UnorderedScoreEvaluations != 2 || state.StagnationPlateaus != 2 {
		t.Fatalf("unchanged unordered score = %+v", state)
	}
	score9 := 9.0
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &score9, Candidate: "candidate:d"})
	if state.NoImprovementStreak != 2 || state.StagnationIndeterminate != 3 {
		t.Fatalf("changed unordered score was classified = %+v", state)
	}
}

func TestStagnationLaneChangesResetActiveStreak(t *testing.T) {
	zero := 0.0
	state := ApplyEvaluation(State{}, EvaluationInput{Handler: "one", Score: &zero, ScoreDirection: ScoreDirectionMaximize, Candidate: "a"})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "one", Score: &zero, ScoreDirection: ScoreDirectionMaximize, Candidate: "b"})
	if state.NoImprovementStreak != 1 {
		t.Fatalf("setup streak = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{Handler: "two", Score: &zero, ScoreDirection: ScoreDirectionMaximize, Candidate: "c"})
	if state.NoImprovementStreak != 0 || state.StagnationLaneResets != 1 || state.MaxNoImprovementStreak != 1 {
		t.Fatalf("handler lane reset = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{Handler: "two", Score: &zero, ScoreDirection: ScoreDirectionMinimize, Candidate: "d"})
	if state.NoImprovementStreak != 0 || state.StagnationLaneResets != 2 {
		t.Fatalf("direction lane reset = %+v", state)
	}
}

func TestStagnationNudgeIsOneShotPerEvaluatorLane(t *testing.T) {
	zero, one := 0.0, 1.0
	state := State{}
	for _, candidate := range []string{"a", "b", "c"} {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "score", Score: &zero, ScoreDirection: ScoreDirectionMaximize, Candidate: candidate,
		})
	}
	if state.NoImprovementStreak != 2 || !CanStagnationNudge(state, 2) || CanStagnationNudge(state, 0) {
		t.Fatalf("nudge threshold state = %+v", state)
	}
	beforeTransitions := state.Transitions
	state = ApplyStagnationNudge(state)
	if !state.StagnationNudgeIssued || state.StagnationNudges != 1 || state.Transitions != beforeTransitions+1 || state.LastTransition != TransitionNudge || CanStagnationNudge(state, 2) {
		t.Fatalf("first nudge state = %+v", state)
	}
	if duplicate := ApplyStagnationNudge(state); duplicate.StagnationNudges != 1 || duplicate.Transitions != state.Transitions {
		t.Fatalf("duplicate nudge changed state = %+v", duplicate)
	}

	state = ApplyEvaluation(state, EvaluationInput{
		Handler: "score", Score: &one, ScoreDirection: ScoreDirectionMaximize, Candidate: "improved",
	})
	state = ApplyEvaluation(state, EvaluationInput{
		Handler: "score", Score: &one, ScoreDirection: ScoreDirectionMaximize, Candidate: "d",
	})
	state = ApplyEvaluation(state, EvaluationInput{
		Handler: "score", Score: &one, ScoreDirection: ScoreDirectionMaximize, Candidate: "e",
	})
	if state.NoImprovementStreak != 2 || !state.StagnationNudgeIssued || CanStagnationNudge(state, 2) {
		t.Fatalf("same lane re-armed after improvement = %+v", state)
	}

	state = ApplyEvaluation(state, EvaluationInput{
		Handler: "latency", Score: &one, ScoreDirection: ScoreDirectionMinimize, Candidate: "f",
	})
	if state.StagnationNudgeIssued || state.NoImprovementStreak != 0 {
		t.Fatalf("new lane did not re-arm = %+v", state)
	}
	for _, candidate := range []string{"g", "h"} {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "latency", Score: &one, ScoreDirection: ScoreDirectionMinimize, Candidate: candidate,
		})
	}
	if !CanStagnationNudge(state, 2) {
		t.Fatalf("second lane did not reach threshold = %+v", state)
	}
	state = ApplyStagnationNudge(state)
	state = ApplyBranch(state)
	if state.StagnationNudgeIssued || state.StagnationNudges != 2 {
		t.Fatalf("branch nudge state = %+v", state)
	}
}

func TestTrackerStagnationNudgeUsesDefensiveState(t *testing.T) {
	zero := 0.0
	tracker := NewTracker(nil)
	for _, candidate := range []string{"a", "b", "c"} {
		tracker.ObserveEvaluation(EvaluationInput{
			Handler: "verify", Score: &zero, ScoreDirection: ScoreDirectionMaximize, Candidate: candidate,
		})
	}
	if !tracker.CanStagnationNudge(2) || !tracker.ObserveStagnationNudge() || tracker.ObserveStagnationNudge() {
		t.Fatalf("tracker nudge state = %+v", tracker.Snapshot())
	}
	if got := tracker.Snapshot(); got.StagnationNudges != 1 || !got.StagnationNudgeIssued {
		t.Fatalf("tracker snapshot = %+v", got)
	}
}

func TestMutationProjectionMeasuresUnconfirmedPaths(t *testing.T) {
	state := ApplyModifiedPaths(State{}, []string{"a.go", "b.go", "a.go"})
	if len(state.ModifiedPaths) != 2 || state.MutationPathObservations != 3 || state.UnconfirmedMutationPaths != 2 {
		t.Fatalf("mutation state = %+v", state)
	}
	state = ApplyConfirmedPaths(state, []string{"a.go", "generated.go"})
	if len(state.ModifiedPaths) != 3 || len(state.ConfirmedModifiedPaths) != 2 || state.DiffPathConfirmations != 2 || state.UnconfirmedMutationPaths != 1 {
		t.Fatalf("confirmed state = %+v", state)
	}
	state = ApplyModifiedPaths(state, []string{"bad\npath"})
	if state.InvalidModifiedPaths != 1 {
		t.Fatalf("invalid path metrics = %+v", state)
	}
}

func TestBranchResetsActiveEpochButKeepsAggregateMetrics(t *testing.T) {
	state := ApplyModifiedPaths(State{}, []string{"a.go"})
	state = ApplyEvaluation(state, EvaluationInput{Accepted: false, Prompt: 1})
	beforeTransitions := state.Transitions
	state = ApplyBranch(state)
	if state.Epoch != 2 || state.BranchResets != 1 || state.Transitions != beforeTransitions+1 || state.TotalEvaluations != 1 {
		t.Fatalf("branch metrics = %+v", state)
	}
	if state.ObjectiveRef != "" || state.CurrentCandidateID != "" || state.BestAcceptedCandidateID != "" || len(state.Evaluations) != 0 || len(state.ModifiedPaths) != 0 || state.UnconfirmedMutationPaths != 0 || state.StagnationHandler != "" || state.NoImprovementStreak != 0 {
		t.Fatalf("branch retained active state = %+v", state)
	}
}

func TestProjectionBoundsHistoryPathsAndEncodedSize(t *testing.T) {
	state := State{}
	evidence := strings.Repeat("e", MaxEvidenceRefBytes)
	for i := 0; i < MaxEvaluations+5; i++ {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: strings.Repeat("h", MaxHandlerBytes), Candidate: fmt.Sprintf("%03d-%s", i, strings.Repeat("c", MaxCandidateBytes)),
			EvidenceRef: evidence, Prompt: 1,
		})
	}
	paths := make([]string, MaxModifiedPaths+7)
	for i := range paths {
		paths[i] = fmt.Sprintf("path-%03d-%s", i, strings.Repeat("p", MaxPathBytes-16))
	}
	state = ApplyModifiedPaths(state, paths)
	state = ApplyConfirmedPaths(state, paths[:MaxModifiedPaths])
	if len(state.Evaluations) != MaxEvaluations || state.DroppedEvaluations != 5 {
		t.Fatalf("evaluation bounds = len %d dropped %d", len(state.Evaluations), state.DroppedEvaluations)
	}
	if len(state.ModifiedPaths) != MaxModifiedPaths || state.DroppedModifiedPaths != 7 {
		t.Fatalf("path bounds = len %d dropped %d", len(state.ModifiedPaths), state.DroppedModifiedPaths)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 64<<10 {
		t.Fatalf("bounded projection encoded to %d bytes, want <= 64 KiB", len(data))
	}
	oversized := State{
		StagnationHandler:      "verify",
		NoImprovementStreak:    MaxNoImprovementStreak + 1,
		MaxNoImprovementStreak: MaxNoImprovementStreak + 2,
	}
	normalized := Normalize(&oversized)
	if normalized.NoImprovementStreak != MaxNoImprovementStreak || normalized.MaxNoImprovementStreak != MaxNoImprovementStreak {
		t.Fatalf("stagnation bounds = %+v", normalized)
	}
}

func TestTrackerSnapshotIsDefensive(t *testing.T) {
	tracker := NewTracker(nil)
	tracker.ObserveEvaluation(EvaluationInput{Candidate: "one", Accepted: true})
	first := tracker.Snapshot()
	first.Evaluations[0].CandidateID = "mutated"
	first.ModifiedPaths = append(first.ModifiedPaths, "injected")
	second := tracker.Snapshot()
	if second.Evaluations[0].CandidateID != "one" || len(second.ModifiedPaths) != 0 {
		t.Fatalf("tracker snapshot aliased state: %+v", second)
	}
}
