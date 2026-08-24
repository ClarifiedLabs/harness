package trajectory

import (
	"strings"
	"testing"
)

func TestEvaluationStateContainsOnlyBoundedStagnationPolicy(t *testing.T) {
	score := 0.5
	state := ApplyEvaluation(State{}, EvaluationInput{
		Handler: " verify ", Accepted: false, Score: &score,
		ScoreDirection: ScoreDirectionMaximize,
		Candidate:      strings.Repeat("c", MaxCandidateBytes+20),
	})
	if state.Schema != SchemaVersion || state.Epoch != 1 || state.TotalEvaluations != 1 || state.RejectedEvaluations != 1 {
		t.Fatalf("evaluation counters = %+v", state)
	}
	if state.StagnationHandler != "verify" || state.StagnationScoreDirection != ScoreDirectionMaximize || state.StagnationBestScore == nil || *state.StagnationBestScore != score {
		t.Fatalf("active lane = %+v", state)
	}
	if state.PreviousObservation == nil || len(state.PreviousObservation.CandidateID) != MaxCandidateBytes {
		t.Fatalf("bounded previous observation = %+v", state.PreviousObservation)
	}
	if state.StagnationBaselines != 1 || state.NoImprovementStreak != 0 {
		t.Fatalf("baseline classification = %+v", state)
	}
}

func TestOrderedScoresAndRemainingRequirementsClassifyStagnation(t *testing.T) {
	state := State{}
	for _, score := range []float64{0, 50, 50, 40, 60} {
		value := score
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "verify", Score: &value, ScoreDirection: ScoreDirectionMaximize,
		})
	}
	if state.StagnationBestScore == nil || *state.StagnationBestScore != 60 || state.NoImprovementStreak != 0 || state.MaxNoImprovementStreak != 2 {
		t.Fatalf("ordered score state = %+v", state)
	}
	if state.StagnationBaselines != 1 || state.StagnationImprovements != 2 || state.StagnationPlateaus != 1 || state.StagnationRegressions != 1 {
		t.Fatalf("ordered score classifications = %+v", state)
	}

	state = ApplyBranch(state)
	for _, count := range []int{5, 3, 3, 4} {
		remaining := count
		state = ApplyEvaluation(state, EvaluationInput{Handler: "requirements", RemainingRequirements: &remaining})
	}
	if state.StagnationBestRemaining == nil || *state.StagnationBestRemaining != 3 || state.NoImprovementStreak != 2 || state.MaxNoImprovementStreak != 2 {
		t.Fatalf("remaining-requirement state = %+v", state)
	}
	if state.StagnationBaselines != 2 || state.StagnationImprovements != 3 || state.StagnationPlateaus != 2 || state.StagnationRegressions != 2 {
		t.Fatalf("aggregate classifications = %+v", state)
	}
}

func TestPreviousObservationSupportsEqualAndRepeatedComparison(t *testing.T) {
	state := ApplyEvaluation(State{}, EvaluationInput{Handler: "verify", Candidate: "candidate:a"})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Candidate: "candidate:b"})
	if state.NoImprovementStreak != 0 || state.StagnationIndeterminate != 1 {
		t.Fatalf("new unscored candidate created stagnation = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Candidate: "candidate:b"})
	if state.NoImprovementStreak != 1 || state.StagnationPlateaus != 1 {
		t.Fatalf("repeated rejected candidate was not a plateau = %+v", state)
	}

	score := 10.0
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &score, Candidate: "candidate:c"})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &score, Candidate: "candidate:d"})
	if state.NoImprovementStreak != 2 || state.UnorderedScoreEvaluations != 2 || state.StagnationPlateaus != 2 {
		t.Fatalf("equal unordered score state = %+v", state)
	}
}

func TestLaneChangesResetOnlyActiveControlState(t *testing.T) {
	zero := 0.0
	state := ApplyEvaluation(State{}, EvaluationInput{Handler: "one", Score: &zero, ScoreDirection: ScoreDirectionMaximize})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "one", Score: &zero, ScoreDirection: ScoreDirectionMaximize})
	state = ApplyStagnationNudge(state)
	if !state.StagnationNudgeIssued || state.NoImprovementStreak != 1 {
		t.Fatalf("setup lane = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{Handler: "two", Score: &zero, ScoreDirection: ScoreDirectionMaximize})
	if state.StagnationNudgeIssued || state.NoImprovementStreak != 0 || state.StagnationLaneResets != 1 || state.PreviousObservation == nil {
		t.Fatalf("handler lane reset = %+v", state)
	}
	state = ApplyEvaluation(state, EvaluationInput{Handler: "two", Score: &zero, ScoreDirection: ScoreDirectionMinimize})
	if state.NoImprovementStreak != 0 || state.StagnationLaneResets != 2 || state.MaxNoImprovementStreak != 1 {
		t.Fatalf("direction lane reset = %+v", state)
	}
}

func TestBaselinePlusTwoSameLaneNonImprovementsAllowsExactlyOneNudge(t *testing.T) {
	zero := 0.0
	state := State{}
	for range 3 {
		state = ApplyEvaluation(state, EvaluationInput{
			Handler: "verify", Score: &zero, ScoreDirection: ScoreDirectionMaximize,
		})
	}
	if state.StagnationBaselines != 1 || state.StagnationPlateaus != 2 || state.NoImprovementStreak != 2 || !CanStagnationNudge(state, 2) {
		t.Fatalf("threshold state = %+v", state)
	}
	state = ApplyStagnationNudge(state)
	if !state.StagnationNudgeIssued || state.StagnationNudges != 1 || CanStagnationNudge(state, 2) {
		t.Fatalf("first nudge state = %+v", state)
	}
	duplicate := ApplyStagnationNudge(state)
	if duplicate.StagnationNudges != 1 {
		t.Fatalf("duplicate nudge changed state = %+v", duplicate)
	}

	one := 1.0
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &one, ScoreDirection: ScoreDirectionMaximize})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &one, ScoreDirection: ScoreDirectionMaximize})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &one, ScoreDirection: ScoreDirectionMaximize})
	if state.NoImprovementStreak != 2 || !state.StagnationNudgeIssued || CanStagnationNudge(state, 2) {
		t.Fatalf("same lane re-armed after improvement = %+v", state)
	}
}

func TestBranchResetsActiveLaneAndPreservesAggregates(t *testing.T) {
	zero := 0.0
	state := ApplyEvaluation(State{}, EvaluationInput{Handler: "verify", Score: &zero, ScoreDirection: ScoreDirectionMaximize})
	state = ApplyEvaluation(state, EvaluationInput{Handler: "verify", Score: &zero, ScoreDirection: ScoreDirectionMaximize})
	state = ApplyStagnationNudge(state)
	state = ApplyBranch(state)
	if state.Epoch != 2 || state.BranchResets != 1 || state.TotalEvaluations != 2 || state.StagnationNudges != 1 {
		t.Fatalf("branch aggregates = %+v", state)
	}
	if state.StagnationHandler != "" || state.StagnationScoreDirection != "" || state.StagnationBestScore != nil || state.PreviousObservation != nil || state.NoImprovementStreak != 0 || state.StagnationNudgeIssued {
		t.Fatalf("branch retained active policy = %+v", state)
	}
}

func TestNormalizeRejectsCachedOlderSchemaAndBoundsCounters(t *testing.T) {
	cached := State{Schema: SchemaVersion - 1, StagnationHandler: "verify", NoImprovementStreak: 9, StagnationNudgeIssued: true}
	if got := Normalize(&cached); got != (State{Schema: SchemaVersion}) {
		t.Fatalf("old cached schema survived normalization: %+v", got)
	}
	oversized := State{
		StagnationHandler:      "verify",
		NoImprovementStreak:    MaxNoImprovementStreak + 1,
		MaxNoImprovementStreak: MaxNoImprovementStreak + 2,
		TotalEvaluations:       -1,
	}
	normalized := Normalize(&oversized)
	if normalized.NoImprovementStreak != MaxNoImprovementStreak || normalized.MaxNoImprovementStreak != MaxNoImprovementStreak || normalized.TotalEvaluations != 0 {
		t.Fatalf("normalized bounds = %+v", normalized)
	}
}

func TestDisabledTrackerNeverAdvancesOrIssuesPolicy(t *testing.T) {
	tracker := NewDisabledTracker()
	for _, candidate := range []string{"one", "two", "three"} {
		tracker.ObserveEvaluation(EvaluationInput{Handler: "verify", Candidate: candidate})
	}
	tracker.ResetForBranch()
	if tracker.CanStagnationNudge(2) || tracker.ObserveStagnationNudge() {
		t.Fatal("disabled tracker issued stagnation policy")
	}
	if got := tracker.Snapshot(); got != (State{Schema: SchemaVersion}) {
		t.Fatalf("disabled tracker advanced state: %+v", got)
	}
}

func TestTrackerSnapshotIsDefensive(t *testing.T) {
	score := 1.0
	tracker := NewTracker(nil)
	tracker.ObserveEvaluation(EvaluationInput{Handler: "verify", Score: &score, ScoreDirection: ScoreDirectionMaximize, Candidate: "one"})
	first := tracker.Snapshot()
	*first.StagnationBestScore = 9
	first.PreviousObservation.CandidateID = "mutated"
	second := tracker.Snapshot()
	if second.StagnationBestScore == nil || *second.StagnationBestScore != 1 || second.PreviousObservation == nil || second.PreviousObservation.CandidateID != "one" {
		t.Fatalf("tracker snapshot aliased state: %+v", second)
	}
}
