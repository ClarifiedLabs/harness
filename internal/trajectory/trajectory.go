// Package trajectory maintains a bounded host-owned projection of candidate
// evaluations and workspace mutations. Callers decide how to persist snapshots
// and whether to opt into the package's bounded read-only request capsule.
package trajectory

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaVersion            = 3
	MaxEvaluations           = 16
	MaxModifiedPaths         = 32
	MaxCandidateBytes        = 256
	MaxEvidenceRefBytes      = 1024
	MaxHandlerBytes          = 128
	MaxPathBytes             = 512
	MaxRemainingRequirements = 1_000_000
	MaxNoImprovementStreak   = 1_000_000
)

const (
	ScoreDirectionMaximize = "maximize"
	ScoreDirectionMinimize = "minimize"
)

const (
	TransitionEvaluation = "evaluation"
	TransitionMutation   = "mutation"
	TransitionBranch     = "branch"
	TransitionNudge      = "stagnation_nudge"
)

// Evaluation is one bounded semantic observation. ID and a fallback candidate
// ID are assigned by the host, never by the model.
type Evaluation struct {
	ID                    string   `json:"id"`
	CandidateID           string   `json:"candidate_id"`
	Handler               string   `json:"handler"`
	Accepted              bool     `json:"accepted"`
	Score                 *float64 `json:"score,omitempty"`
	ScoreDirection        string   `json:"score_direction,omitempty"`
	RemainingRequirements *int     `json:"remaining_requirements,omitempty"`
	EvidenceRef           string   `json:"evidence_ref,omitempty"`
	FailureClass          string   `json:"failure_class,omitempty"`
	Prompt                int      `json:"prompt,omitempty"`
	Turn                  int      `json:"turn,omitempty"`
}

// State is the bounded current trajectory projection persisted in state.json.
// Aggregate counters survive branch resets; candidate/evaluation/path details
// describe only the active epoch.
type State struct {
	Schema                      int          `json:"schema"`
	Epoch                       int          `json:"epoch,omitempty"`
	ObjectiveRef                string       `json:"objective_ref,omitempty"`
	CurrentCandidateID          string       `json:"current_candidate_id,omitempty"`
	BestAcceptedCandidateID     string       `json:"best_accepted_candidate_id,omitempty"`
	BestAcceptedScore           *float64     `json:"best_accepted_score,omitempty"`
	Evaluations                 []Evaluation `json:"evaluations,omitempty"`
	ModifiedPaths               []string     `json:"modified_paths,omitempty"`
	ConfirmedModifiedPaths      []string     `json:"confirmed_modified_paths,omitempty"`
	LastImprovementEvaluationID string       `json:"last_improvement_evaluation_id,omitempty"`
	EvaluationsSinceImprovement int          `json:"evaluations_since_improvement,omitempty"`
	StagnationHandler           string       `json:"stagnation_handler,omitempty"`
	StagnationScoreDirection    string       `json:"stagnation_score_direction,omitempty"`
	StagnationBestScore         *float64     `json:"stagnation_best_score,omitempty"`
	StagnationBestRemaining     *int         `json:"stagnation_best_remaining_requirements,omitempty"`
	NoImprovementStreak         int          `json:"no_improvement_streak,omitempty"`
	MaxNoImprovementStreak      int          `json:"max_no_improvement_streak,omitempty"`
	StagnationBaselines         int          `json:"stagnation_baselines,omitempty"`
	StagnationImprovements      int          `json:"stagnation_improvements,omitempty"`
	StagnationPlateaus          int          `json:"stagnation_plateaus,omitempty"`
	StagnationRegressions       int          `json:"stagnation_regressions,omitempty"`
	StagnationIndeterminate     int          `json:"stagnation_indeterminate,omitempty"`
	UnorderedScoreEvaluations   int          `json:"unordered_score_evaluations,omitempty"`
	StagnationLaneResets        int          `json:"stagnation_lane_resets,omitempty"`
	StagnationNudgeIssued       bool         `json:"stagnation_nudge_issued,omitempty"`
	StagnationNudges            int          `json:"stagnation_nudges,omitempty"`
	EpochEvaluations            int          `json:"epoch_evaluations,omitempty"`
	TotalEvaluations            int          `json:"total_evaluations,omitempty"`
	AcceptedEvaluations         int          `json:"accepted_evaluations,omitempty"`
	RejectedEvaluations         int          `json:"rejected_evaluations,omitempty"`
	Transitions                 int          `json:"transitions,omitempty"`
	BranchResets                int          `json:"branch_resets,omitempty"`
	MissingCandidateIDs         int          `json:"missing_candidate_ids,omitempty"`
	MissingEvidenceRefs         int          `json:"missing_evidence_refs,omitempty"`
	DroppedEvaluations          int          `json:"dropped_evaluations,omitempty"`
	DroppedModifiedPaths        int          `json:"dropped_modified_paths,omitempty"`
	InvalidModifiedPaths        int          `json:"invalid_modified_paths,omitempty"`
	MutationPathObservations    int          `json:"mutation_path_observations,omitempty"`
	DiffPathConfirmations       int          `json:"diff_path_confirmations,omitempty"`
	UnconfirmedMutationPaths    int          `json:"unconfirmed_mutation_paths,omitempty"`
	LastTransition              string       `json:"last_transition,omitempty"`
}

// EvaluationInput is the host-observed evaluator event consumed by ApplyEvaluation.
type EvaluationInput struct {
	Handler               string
	Accepted              bool
	Score                 *float64
	ScoreDirection        string
	Candidate             string
	RemainingRequirements *int
	EvidenceRef           string
	Prompt                int
	Turn                  int
}

// Tracker serializes live projection updates and returns defensive snapshots.
type Tracker struct {
	mu    sync.RWMutex
	state State
}

func NewTracker(initial *State) *Tracker {
	return &Tracker{state: Normalize(initial)}
}

func (t *Tracker) Snapshot() State {
	if t == nil {
		return Normalize(nil)
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneState(t.state)
}

func (t *Tracker) SnapshotPtr() *State {
	state := t.Snapshot()
	return &state
}

func (t *Tracker) ObserveEvaluation(input EvaluationInput) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = ApplyEvaluation(t.state, input)
}

// CanStagnationNudge reports whether the active evaluator lane has reached the
// threshold and has not already received its one bounded nudge.
func (t *Tracker) CanStagnationNudge(threshold int) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return CanStagnationNudge(t.state, threshold)
}

// ObserveStagnationNudge marks the active evaluator lane after the canonical
// trigger event has been persisted.
func (t *Tracker) ObserveStagnationNudge() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	next := ApplyStagnationNudge(t.state)
	if next.StagnationNudges == t.state.StagnationNudges {
		return false
	}
	t.state = next
	return true
}

func (t *Tracker) ObserveModifiedPaths(paths []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = ApplyModifiedPaths(t.state, paths)
}

func (t *Tracker) ConfirmModifiedPaths(paths []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = ApplyConfirmedPaths(t.state, paths)
}

func (t *Tracker) ResetForBranch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = ApplyBranch(t.state)
}

// Replace installs a normalized seed after its canonical transition has been
// persisted. It is used only when a fresh physical session inherits state.
func (t *Tracker) Replace(initial *State) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = Normalize(initial)
}

// Normalize returns a bounded defensive copy suitable for a seed or persisted
// snapshot. Invalid or oversized optional identifiers are omitted.
func Normalize(initial *State) State {
	state := State{Schema: SchemaVersion}
	if initial == nil {
		return state
	}
	state = cloneState(*initial)
	state.Schema = SchemaVersion
	state.Epoch = nonNegative(state.Epoch)
	state.ObjectiveRef = boundedSingleLine(state.ObjectiveRef, MaxCandidateBytes)
	state.CurrentCandidateID = boundedSingleLine(state.CurrentCandidateID, MaxCandidateBytes)
	state.BestAcceptedCandidateID = boundedSingleLine(state.BestAcceptedCandidateID, MaxCandidateBytes)
	state.LastImprovementEvaluationID = boundedSingleLine(state.LastImprovementEvaluationID, MaxCandidateBytes)
	state.LastTransition = boundedSingleLine(state.LastTransition, 32)
	state.EvaluationsSinceImprovement = nonNegative(state.EvaluationsSinceImprovement)
	state.StagnationHandler = boundedSingleLine(state.StagnationHandler, MaxHandlerBytes)
	state.StagnationScoreDirection = normalizeScoreDirection(state.StagnationScoreDirection)
	state.NoImprovementStreak = min(nonNegative(state.NoImprovementStreak), MaxNoImprovementStreak)
	state.MaxNoImprovementStreak = min(nonNegative(state.MaxNoImprovementStreak), MaxNoImprovementStreak)
	state.MaxNoImprovementStreak = max(state.MaxNoImprovementStreak, state.NoImprovementStreak)
	state.StagnationBaselines = nonNegative(state.StagnationBaselines)
	state.StagnationImprovements = nonNegative(state.StagnationImprovements)
	state.StagnationPlateaus = nonNegative(state.StagnationPlateaus)
	state.StagnationRegressions = nonNegative(state.StagnationRegressions)
	state.StagnationIndeterminate = nonNegative(state.StagnationIndeterminate)
	state.UnorderedScoreEvaluations = nonNegative(state.UnorderedScoreEvaluations)
	state.StagnationLaneResets = nonNegative(state.StagnationLaneResets)
	state.StagnationNudges = nonNegative(state.StagnationNudges)
	state.EpochEvaluations = nonNegative(state.EpochEvaluations)
	state.TotalEvaluations = nonNegative(state.TotalEvaluations)
	state.AcceptedEvaluations = nonNegative(state.AcceptedEvaluations)
	state.RejectedEvaluations = nonNegative(state.RejectedEvaluations)
	state.Transitions = nonNegative(state.Transitions)
	state.BranchResets = nonNegative(state.BranchResets)
	state.MissingCandidateIDs = nonNegative(state.MissingCandidateIDs)
	state.MissingEvidenceRefs = nonNegative(state.MissingEvidenceRefs)
	state.DroppedEvaluations = nonNegative(state.DroppedEvaluations)
	state.DroppedModifiedPaths = nonNegative(state.DroppedModifiedPaths)
	state.InvalidModifiedPaths = nonNegative(state.InvalidModifiedPaths)
	state.MutationPathObservations = nonNegative(state.MutationPathObservations)
	state.DiffPathConfirmations = nonNegative(state.DiffPathConfirmations)
	if state.BestAcceptedScore != nil {
		score := *state.BestAcceptedScore
		state.BestAcceptedScore = &score
	}
	if state.StagnationBestScore != nil && state.StagnationScoreDirection != "" && validScore(*state.StagnationBestScore) {
		score := *state.StagnationBestScore
		state.StagnationBestScore = &score
	} else {
		state.StagnationBestScore = nil
	}
	if state.StagnationBestRemaining != nil && *state.StagnationBestRemaining >= 0 && *state.StagnationBestRemaining <= MaxRemainingRequirements {
		remaining := *state.StagnationBestRemaining
		state.StagnationBestRemaining = &remaining
	} else {
		state.StagnationBestRemaining = nil
	}
	if state.StagnationHandler == "" {
		state.StagnationScoreDirection = ""
		state.StagnationBestScore = nil
		state.StagnationBestRemaining = nil
		state.NoImprovementStreak = 0
		state.StagnationNudgeIssued = false
	}

	if len(state.Evaluations) > MaxEvaluations {
		state.DroppedEvaluations += len(state.Evaluations) - MaxEvaluations
		state.Evaluations = state.Evaluations[len(state.Evaluations)-MaxEvaluations:]
	}
	evaluations := make([]Evaluation, 0, len(state.Evaluations))
	for _, evaluation := range state.Evaluations {
		evaluation = normalizeEvaluation(evaluation)
		if evaluation.ID == "" || evaluation.CandidateID == "" {
			continue
		}
		evaluations = append(evaluations, evaluation)
	}
	state.Evaluations = evaluations

	paths := make([]string, 0, min(len(state.ModifiedPaths), MaxModifiedPaths))
	seen := make(map[string]struct{}, len(state.ModifiedPaths))
	for _, path := range state.ModifiedPaths {
		path = boundedSingleLine(path, MaxPathBytes)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		if len(paths) == MaxModifiedPaths {
			state.DroppedModifiedPaths++
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	state.ModifiedPaths = paths
	confirmed := make([]string, 0, min(len(state.ConfirmedModifiedPaths), MaxModifiedPaths))
	modified := make(map[string]struct{}, len(state.ModifiedPaths))
	for _, path := range state.ModifiedPaths {
		modified[path] = struct{}{}
	}
	seen = make(map[string]struct{}, len(state.ConfirmedModifiedPaths))
	for _, path := range state.ConfirmedModifiedPaths {
		path = boundedSingleLine(path, MaxPathBytes)
		if _, ok := modified[path]; !ok || path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		if len(confirmed) == MaxModifiedPaths {
			break
		}
		seen[path] = struct{}{}
		confirmed = append(confirmed, path)
	}
	state.ConfirmedModifiedPaths = confirmed
	state.UnconfirmedMutationPaths = len(state.ModifiedPaths) - len(state.ConfirmedModifiedPaths)
	return state
}

func ApplyEvaluation(current State, input EvaluationInput) State {
	state := Normalize(&current)
	ensureEpoch(&state)
	state.EpochEvaluations++
	state.TotalEvaluations++
	state.Transitions++
	state.LastTransition = TransitionEvaluation

	evaluationID := fmt.Sprintf("eval-e%d-%06d", state.Epoch, state.EpochEvaluations)
	candidate := boundedSingleLine(input.Candidate, MaxCandidateBytes)
	if candidate == "" {
		state.MissingCandidateIDs++
		candidate = fmt.Sprintf("candidate-e%d-%06d", state.Epoch, state.EpochEvaluations)
	}
	evidenceRef := boundedSingleLine(input.EvidenceRef, MaxEvidenceRefBytes)
	if evidenceRef == "" {
		state.MissingEvidenceRefs++
	}
	handler := boundedSingleLine(input.Handler, MaxHandlerBytes)
	if handler == "" {
		handler = "unknown"
	}
	if state.ObjectiveRef == "" {
		if input.Prompt > 0 {
			state.ObjectiveRef = fmt.Sprintf("prompt:%d", input.Prompt)
		} else {
			state.ObjectiveRef = "prompt:unknown"
		}
	}
	evaluation := Evaluation{
		ID:             evaluationID,
		CandidateID:    candidate,
		Handler:        handler,
		Accepted:       input.Accepted,
		ScoreDirection: normalizeScoreDirection(input.ScoreDirection),
		EvidenceRef:    evidenceRef,
		Prompt:         nonNegative(input.Prompt),
		Turn:           nonNegative(input.Turn),
		FailureClass:   "evaluator_rejected",
	}
	if input.Accepted {
		evaluation.FailureClass = ""
		state.AcceptedEvaluations++
	} else {
		state.RejectedEvaluations++
	}
	if input.Score != nil {
		score := *input.Score
		if validScore(score) {
			evaluation.Score = &score
		}
	}
	if evaluation.Score == nil {
		evaluation.ScoreDirection = ""
	}
	if input.RemainingRequirements != nil && *input.RemainingRequirements >= 0 && *input.RemainingRequirements <= MaxRemainingRequirements {
		remaining := *input.RemainingRequirements
		evaluation.RemainingRequirements = &remaining
	}

	state.CurrentCandidateID = candidate
	improved := input.Accepted && candidate != state.BestAcceptedCandidateID
	if improved {
		state.BestAcceptedCandidateID = candidate
		state.BestAcceptedScore = nil
		if evaluation.Score != nil {
			score := *evaluation.Score
			state.BestAcceptedScore = &score
		}
		state.LastImprovementEvaluationID = evaluationID
		state.EvaluationsSinceImprovement = 0
	} else {
		state.EvaluationsSinceImprovement++
		if input.Accepted && candidate == state.BestAcceptedCandidateID && evaluation.Score != nil {
			score := *evaluation.Score
			state.BestAcceptedScore = &score
		}
	}
	applyStagnation(&state, evaluation)
	state.Evaluations = append(state.Evaluations, evaluation)
	if len(state.Evaluations) > MaxEvaluations {
		drop := len(state.Evaluations) - MaxEvaluations
		state.Evaluations = append([]Evaluation(nil), state.Evaluations[drop:]...)
		state.DroppedEvaluations += drop
	}
	return state
}

const (
	stagnationBaseline      = "baseline"
	stagnationImprovement   = "improvement"
	stagnationPlateau       = "plateau"
	stagnationRegression    = "regression"
	stagnationIndeterminate = "indeterminate"
)

// applyStagnation classifies only evidence the host can order safely. An
// accepted result is progress. Rejected scores are comparable only when the
// evaluator explicitly supplies a direction; remaining-requirement counts are
// intrinsically minimized. Equal unordered scores and exact repeated rejected
// candidates are plateaus. A new unscored candidate remains indeterminate and
// cannot create a false no-improvement signal.
func applyStagnation(state *State, evaluation Evaluation) {
	if state == nil {
		return
	}
	direction := normalizeScoreDirection(evaluation.ScoreDirection)
	laneReset := state.StagnationHandler == "" || state.StagnationHandler != evaluation.Handler
	if !laneReset && direction != "" && direction != state.StagnationScoreDirection {
		laneReset = true
	}
	if laneReset {
		if state.StagnationHandler != "" {
			state.StagnationLaneResets++
		}
		state.StagnationHandler = evaluation.Handler
		state.StagnationScoreDirection = direction
		state.StagnationBestScore = nil
		state.StagnationBestRemaining = nil
		state.NoImprovementStreak = 0
		state.StagnationNudgeIssued = false
	}

	if evaluation.Score != nil && direction == "" {
		state.UnorderedScoreEvaluations++
	}
	orderedScore := evaluation.Score != nil && direction != "" && direction == state.StagnationScoreDirection
	outcome := stagnationIndeterminate
	switch {
	case evaluation.Accepted:
		outcome = stagnationImprovement
	case laneReset:
		outcome = stagnationBaseline
	case orderedScore:
		if state.StagnationBestScore == nil {
			outcome = stagnationBaseline
		} else {
			outcome = compareOrderedScore(*evaluation.Score, *state.StagnationBestScore, direction)
		}
	case evaluation.RemainingRequirements != nil:
		if state.StagnationBestRemaining == nil {
			outcome = stagnationBaseline
		} else {
			switch {
			case *evaluation.RemainingRequirements < *state.StagnationBestRemaining:
				outcome = stagnationImprovement
			case *evaluation.RemainingRequirements == *state.StagnationBestRemaining:
				outcome = stagnationPlateau
			default:
				outcome = stagnationRegression
			}
		}
	case equalPreviousScore(state.Evaluations, evaluation):
		outcome = stagnationPlateau
	case repeatedRejectedCandidate(state.Evaluations, evaluation):
		outcome = stagnationPlateau
	}

	if orderedScore && (state.StagnationBestScore == nil || scoreImproves(*evaluation.Score, *state.StagnationBestScore, direction)) {
		score := *evaluation.Score
		state.StagnationBestScore = &score
	}
	if evaluation.RemainingRequirements != nil && (state.StagnationBestRemaining == nil || *evaluation.RemainingRequirements < *state.StagnationBestRemaining) {
		remaining := *evaluation.RemainingRequirements
		state.StagnationBestRemaining = &remaining
	}

	switch outcome {
	case stagnationBaseline:
		state.StagnationBaselines++
		state.NoImprovementStreak = 0
	case stagnationImprovement:
		state.StagnationImprovements++
		state.NoImprovementStreak = 0
	case stagnationPlateau:
		state.StagnationPlateaus++
		state.NoImprovementStreak = min(state.NoImprovementStreak+1, MaxNoImprovementStreak)
	case stagnationRegression:
		state.StagnationRegressions++
		state.NoImprovementStreak = min(state.NoImprovementStreak+1, MaxNoImprovementStreak)
	default:
		state.StagnationIndeterminate++
	}
	state.MaxNoImprovementStreak = max(state.MaxNoImprovementStreak, state.NoImprovementStreak)
}

// CanStagnationNudge is deliberately conservative: only a non-empty active
// evaluator lane at or above a positive threshold can trigger, and each lane
// can trigger at most once until a handler/direction change or branch reset.
func CanStagnationNudge(current State, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	state := Normalize(&current)
	return state.StagnationHandler != "" &&
		state.NoImprovementStreak >= threshold &&
		!state.StagnationNudgeIssued
}

// ApplyStagnationNudge records the canonical intervention fact for replay. The
// runtime threshold belongs to the trigger event; replay applies an event that
// already happened and therefore does not re-evaluate policy.
func ApplyStagnationNudge(current State) State {
	state := Normalize(&current)
	if state.StagnationHandler == "" || state.StagnationNudgeIssued {
		return state
	}
	state.StagnationNudgeIssued = true
	state.StagnationNudges++
	state.Transitions++
	state.LastTransition = TransitionNudge
	return state
}

func compareOrderedScore(score, best float64, direction string) string {
	if score == best {
		return stagnationPlateau
	}
	if scoreImproves(score, best, direction) {
		return stagnationImprovement
	}
	return stagnationRegression
}

func scoreImproves(score, best float64, direction string) bool {
	if direction == ScoreDirectionMinimize {
		return score < best
	}
	return direction == ScoreDirectionMaximize && score > best
}

func equalPreviousScore(evaluations []Evaluation, current Evaluation) bool {
	if current.Score == nil {
		return false
	}
	for i := len(evaluations) - 1; i >= 0; i-- {
		previous := evaluations[i]
		if previous.Handler != current.Handler {
			continue
		}
		return previous.Score != nil && *previous.Score == *current.Score
	}
	return false
}

func repeatedRejectedCandidate(evaluations []Evaluation, current Evaluation) bool {
	if current.Accepted || current.CandidateID == "" {
		return false
	}
	for i := len(evaluations) - 1; i >= 0; i-- {
		previous := evaluations[i]
		if previous.Handler != current.Handler {
			continue
		}
		if current.Score != nil && previous.Score != nil && *current.Score != *previous.Score {
			return false
		}
		return !previous.Accepted && previous.CandidateID == current.CandidateID
	}
	return false
}

func normalizeScoreDirection(direction string) string {
	direction = strings.TrimSpace(direction)
	if direction == ScoreDirectionMaximize || direction == ScoreDirectionMinimize {
		return direction
	}
	return ""
}

func validScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0)
}

func ApplyModifiedPaths(current State, paths []string) State {
	return applyPaths(current, paths, false)
}

func ApplyConfirmedPaths(current State, paths []string) State {
	return applyPaths(current, paths, true)
}

func applyPaths(current State, paths []string, confirmed bool) State {
	state := Normalize(&current)
	ensureEpoch(&state)
	seen := make(map[string]struct{}, len(state.ModifiedPaths))
	for _, path := range state.ModifiedPaths {
		seen[path] = struct{}{}
	}
	changed := false
	for _, path := range paths {
		path = boundedSingleLine(path, MaxPathBytes)
		if path == "" {
			state.InvalidModifiedPaths++
			changed = true
			continue
		}
		if confirmed {
			state.DiffPathConfirmations++
		} else {
			state.MutationPathObservations++
		}
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			if len(state.ModifiedPaths) >= MaxModifiedPaths {
				state.DroppedModifiedPaths++
				changed = true
				continue
			}
			state.ModifiedPaths = append(state.ModifiedPaths, path)
			changed = true
		}
		if confirmed && !contains(state.ConfirmedModifiedPaths, path) {
			state.ConfirmedModifiedPaths = append(state.ConfirmedModifiedPaths, path)
			changed = true
		}
	}
	state.UnconfirmedMutationPaths = len(state.ModifiedPaths) - len(state.ConfirmedModifiedPaths)
	if changed {
		state.Transitions++
		state.LastTransition = TransitionMutation
	}
	return state
}

func ApplyBranch(current State) State {
	state := Normalize(&current)
	state.Epoch++
	if state.Epoch <= 0 {
		state.Epoch = 1
	}
	state.ObjectiveRef = ""
	state.CurrentCandidateID = ""
	state.BestAcceptedCandidateID = ""
	state.BestAcceptedScore = nil
	state.Evaluations = nil
	state.ModifiedPaths = nil
	state.ConfirmedModifiedPaths = nil
	state.UnconfirmedMutationPaths = 0
	state.LastImprovementEvaluationID = ""
	state.EvaluationsSinceImprovement = 0
	state.StagnationHandler = ""
	state.StagnationScoreDirection = ""
	state.StagnationBestScore = nil
	state.StagnationBestRemaining = nil
	state.NoImprovementStreak = 0
	state.StagnationNudgeIssued = false
	state.EpochEvaluations = 0
	state.BranchResets++
	state.Transitions++
	state.LastTransition = TransitionBranch
	return state
}

func ensureEpoch(state *State) {
	if state.Epoch <= 0 {
		state.Epoch = 1
	}
}

func normalizeEvaluation(evaluation Evaluation) Evaluation {
	evaluation.ID = boundedSingleLine(evaluation.ID, MaxCandidateBytes)
	evaluation.CandidateID = boundedSingleLine(evaluation.CandidateID, MaxCandidateBytes)
	evaluation.Handler = boundedSingleLine(evaluation.Handler, MaxHandlerBytes)
	evaluation.ScoreDirection = normalizeScoreDirection(evaluation.ScoreDirection)
	evaluation.EvidenceRef = boundedSingleLine(evaluation.EvidenceRef, MaxEvidenceRefBytes)
	evaluation.FailureClass = boundedSingleLine(evaluation.FailureClass, 64)
	evaluation.Prompt = nonNegative(evaluation.Prompt)
	evaluation.Turn = nonNegative(evaluation.Turn)
	if evaluation.Score != nil {
		score := *evaluation.Score
		if validScore(score) {
			evaluation.Score = &score
		} else {
			evaluation.Score = nil
			evaluation.ScoreDirection = ""
		}
	}
	if evaluation.Score == nil {
		evaluation.ScoreDirection = ""
	}
	if evaluation.RemainingRequirements != nil {
		remaining := *evaluation.RemainingRequirements
		if remaining < 0 || remaining > MaxRemainingRequirements {
			evaluation.RemainingRequirements = nil
		} else {
			evaluation.RemainingRequirements = &remaining
		}
	}
	return evaluation
}

func cloneState(state State) State {
	if state.BestAcceptedScore != nil {
		score := *state.BestAcceptedScore
		state.BestAcceptedScore = &score
	}
	if state.StagnationBestScore != nil {
		score := *state.StagnationBestScore
		state.StagnationBestScore = &score
	}
	if state.StagnationBestRemaining != nil {
		remaining := *state.StagnationBestRemaining
		state.StagnationBestRemaining = &remaining
	}
	state.Evaluations = append([]Evaluation(nil), state.Evaluations...)
	for i := range state.Evaluations {
		state.Evaluations[i] = normalizeEvaluation(state.Evaluations[i])
	}
	state.ModifiedPaths = append([]string(nil), state.ModifiedPaths...)
	state.ConfirmedModifiedPaths = append([]string(nil), state.ConfirmedModifiedPaths...)
	return state
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func boundedSingleLine(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxBytes <= 0 {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
