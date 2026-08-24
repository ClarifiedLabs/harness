// Package trajectory maintains the bounded host-owned state needed to detect
// repeated evaluator stagnation. Raw session events, rather than this runtime
// projection, are authoritative across process boundaries.
package trajectory

import (
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaVersion            = 4
	MaxCandidateBytes        = 256
	MaxHandlerBytes          = 128
	MaxRemainingRequirements = 1_000_000
	MaxNoImprovementStreak   = 1_000_000
)

const (
	ScoreDirectionMaximize = "maximize"
	ScoreDirectionMinimize = "minimize"
)

// EvaluatorObservation is the one bounded prior observation retained for
// equal-score and repeated-rejected-candidate comparisons. It is deliberately
// not an evaluator history.
type EvaluatorObservation struct {
	Accepted    bool     `json:"accepted,omitempty"`
	Score       *float64 `json:"score,omitempty"`
	CandidateID string   `json:"candidate_id,omitempty"`
}

// State contains only the active stagnation-control lane and aggregate counters
// produced by the current trajectory schema. Raw evaluator_result,
// stagnation_nudge, and branch events are the durable source of truth.
type State struct {
	Schema                   int                   `json:"schema"`
	Epoch                    int                   `json:"epoch,omitempty"`
	StagnationHandler        string                `json:"stagnation_handler,omitempty"`
	StagnationScoreDirection string                `json:"stagnation_score_direction,omitempty"`
	StagnationBestScore      *float64              `json:"stagnation_best_score,omitempty"`
	StagnationBestRemaining  *int                  `json:"stagnation_best_remaining_requirements,omitempty"`
	PreviousObservation      *EvaluatorObservation `json:"previous_observation,omitempty"`
	NoImprovementStreak      int                   `json:"no_improvement_streak,omitempty"`
	MaxNoImprovementStreak   int                   `json:"max_no_improvement_streak,omitempty"`
	StagnationNudgeIssued    bool                  `json:"stagnation_nudge_issued,omitempty"`

	TotalEvaluations          int `json:"total_evaluations,omitempty"`
	AcceptedEvaluations       int `json:"accepted_evaluations,omitempty"`
	RejectedEvaluations       int `json:"rejected_evaluations,omitempty"`
	StagnationBaselines       int `json:"stagnation_baselines,omitempty"`
	StagnationImprovements    int `json:"stagnation_improvements,omitempty"`
	StagnationPlateaus        int `json:"stagnation_plateaus,omitempty"`
	StagnationRegressions     int `json:"stagnation_regressions,omitempty"`
	StagnationIndeterminate   int `json:"stagnation_indeterminate,omitempty"`
	UnorderedScoreEvaluations int `json:"unordered_score_evaluations,omitempty"`
	StagnationLaneResets      int `json:"stagnation_lane_resets,omitempty"`
	BranchResets              int `json:"branch_resets,omitempty"`
	StagnationNudges          int `json:"stagnation_nudges,omitempty"`
}

// EvaluationInput is one host-observed evaluator event. Candidate is retained
// only in the bounded previous observation needed for repeated-candidate
// comparison.
type EvaluationInput struct {
	Handler               string
	Accepted              bool
	Score                 *float64
	ScoreDirection        string
	Candidate             string
	RemainingRequirements *int
}

// Tracker serializes live policy updates and returns defensive snapshots.
type Tracker struct {
	mu       sync.RWMutex
	state    State
	disabled bool
}

func NewTracker(initial *State) *Tracker {
	return &Tracker{state: Normalize(initial)}
}

// NewDisabledTracker returns a tracker that preserves the normal caller shape
// but cannot advance or issue policy. It is used when resumed raw telemetry is
// unavailable or malformed, so an unverifiable cached state cannot act.
func NewDisabledTracker() *Tracker {
	return &Tracker{state: Normalize(nil), disabled: true}
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
	if t.disabled {
		return
	}
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
	return !t.disabled && CanStagnationNudge(t.state, threshold)
}

// ObserveStagnationNudge advances policy only after the canonical trigger event
// has been appended by the caller.
func (t *Tracker) ObserveStagnationNudge() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled {
		return false
	}
	next := ApplyStagnationNudge(t.state)
	if next.StagnationNudges == t.state.StagnationNudges {
		return false
	}
	t.state = next
	return true
}

func (t *Tracker) ResetForBranch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled {
		return
	}
	t.state = ApplyBranch(t.state)
}

// Normalize returns a bounded defensive copy. State from an older non-zero
// schema is not a policy cache: it fails closed to fresh state and must instead
// be reconstructed from canonical raw events.
func Normalize(initial *State) State {
	state := State{Schema: SchemaVersion}
	if initial == nil || (initial.Schema != 0 && initial.Schema != SchemaVersion) {
		return state
	}
	state = cloneState(*initial)
	state.Schema = SchemaVersion
	state.Epoch = nonNegative(state.Epoch)
	state.StagnationHandler = boundedSingleLine(state.StagnationHandler, MaxHandlerBytes)
	state.StagnationScoreDirection = normalizeScoreDirection(state.StagnationScoreDirection)
	state.NoImprovementStreak = min(nonNegative(state.NoImprovementStreak), MaxNoImprovementStreak)
	state.MaxNoImprovementStreak = min(nonNegative(state.MaxNoImprovementStreak), MaxNoImprovementStreak)
	state.MaxNoImprovementStreak = max(state.MaxNoImprovementStreak, state.NoImprovementStreak)
	state.TotalEvaluations = nonNegative(state.TotalEvaluations)
	state.AcceptedEvaluations = nonNegative(state.AcceptedEvaluations)
	state.RejectedEvaluations = nonNegative(state.RejectedEvaluations)
	state.StagnationBaselines = nonNegative(state.StagnationBaselines)
	state.StagnationImprovements = nonNegative(state.StagnationImprovements)
	state.StagnationPlateaus = nonNegative(state.StagnationPlateaus)
	state.StagnationRegressions = nonNegative(state.StagnationRegressions)
	state.StagnationIndeterminate = nonNegative(state.StagnationIndeterminate)
	state.UnorderedScoreEvaluations = nonNegative(state.UnorderedScoreEvaluations)
	state.StagnationLaneResets = nonNegative(state.StagnationLaneResets)
	state.BranchResets = nonNegative(state.BranchResets)
	state.StagnationNudges = nonNegative(state.StagnationNudges)

	if state.StagnationBestScore != nil && state.StagnationScoreDirection != "" && validScore(*state.StagnationBestScore) {
		score := *state.StagnationBestScore
		state.StagnationBestScore = &score
	} else {
		state.StagnationBestScore = nil
	}
	if state.StagnationBestRemaining != nil && validRemaining(*state.StagnationBestRemaining) {
		remaining := *state.StagnationBestRemaining
		state.StagnationBestRemaining = &remaining
	} else {
		state.StagnationBestRemaining = nil
	}
	state.PreviousObservation = normalizeObservation(state.PreviousObservation)
	if state.StagnationHandler == "" {
		clearActiveLane(&state)
	}
	return state
}

func ApplyEvaluation(current State, input EvaluationInput) State {
	state := Normalize(&current)
	ensureEpoch(&state)
	state.TotalEvaluations++
	if input.Accepted {
		state.AcceptedEvaluations++
	} else {
		state.RejectedEvaluations++
	}

	handler := boundedSingleLine(input.Handler, MaxHandlerBytes)
	if handler == "" {
		handler = "unknown"
	}
	direction := normalizeScoreDirection(input.ScoreDirection)
	observation := &EvaluatorObservation{
		Accepted:    input.Accepted,
		CandidateID: boundedSingleLine(input.Candidate, MaxCandidateBytes),
	}
	if input.Score != nil && validScore(*input.Score) {
		score := *input.Score
		observation.Score = &score
	} else {
		direction = ""
	}
	var remaining *int
	if input.RemainingRequirements != nil && validRemaining(*input.RemainingRequirements) {
		value := *input.RemainingRequirements
		remaining = &value
	}

	laneReset := state.StagnationHandler == "" || state.StagnationHandler != handler
	if !laneReset && direction != "" && direction != state.StagnationScoreDirection {
		laneReset = true
	}
	if laneReset {
		if state.StagnationHandler != "" {
			state.StagnationLaneResets++
		}
		clearActiveLane(&state)
		state.StagnationHandler = handler
		state.StagnationScoreDirection = direction
	}

	if observation.Score != nil && direction == "" {
		state.UnorderedScoreEvaluations++
	}
	orderedScore := observation.Score != nil && direction != "" && direction == state.StagnationScoreDirection
	outcome := stagnationIndeterminate
	switch {
	case observation.Accepted:
		outcome = stagnationImprovement
	case laneReset:
		outcome = stagnationBaseline
	case orderedScore:
		if state.StagnationBestScore == nil {
			outcome = stagnationBaseline
		} else {
			outcome = compareOrderedScore(*observation.Score, *state.StagnationBestScore, direction)
		}
	case remaining != nil:
		if state.StagnationBestRemaining == nil {
			outcome = stagnationBaseline
		} else {
			switch {
			case *remaining < *state.StagnationBestRemaining:
				outcome = stagnationImprovement
			case *remaining == *state.StagnationBestRemaining:
				outcome = stagnationPlateau
			default:
				outcome = stagnationRegression
			}
		}
	case equalPreviousScore(state.PreviousObservation, observation):
		outcome = stagnationPlateau
	case repeatedRejectedCandidate(state.PreviousObservation, observation):
		outcome = stagnationPlateau
	}

	if orderedScore && (state.StagnationBestScore == nil || scoreImproves(*observation.Score, *state.StagnationBestScore, direction)) {
		score := *observation.Score
		state.StagnationBestScore = &score
	}
	if remaining != nil && (state.StagnationBestRemaining == nil || *remaining < *state.StagnationBestRemaining) {
		value := *remaining
		state.StagnationBestRemaining = &value
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
	state.PreviousObservation = observation
	return state
}

const (
	stagnationBaseline      = "baseline"
	stagnationImprovement   = "improvement"
	stagnationPlateau       = "plateau"
	stagnationRegression    = "regression"
	stagnationIndeterminate = "indeterminate"
)

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

// ApplyStagnationNudge records an already-persisted intervention. Replay does
// not re-evaluate the runtime threshold stored in the event telemetry.
func ApplyStagnationNudge(current State) State {
	state := Normalize(&current)
	if state.StagnationHandler == "" || state.StagnationNudgeIssued {
		return state
	}
	state.StagnationNudgeIssued = true
	state.StagnationNudges++
	return state
}

func ApplyBranch(current State) State {
	state := Normalize(&current)
	state.Epoch++
	if state.Epoch <= 0 {
		state.Epoch = 1
	}
	clearActiveLane(&state)
	state.BranchResets++
	return state
}

func clearActiveLane(state *State) {
	state.StagnationHandler = ""
	state.StagnationScoreDirection = ""
	state.StagnationBestScore = nil
	state.StagnationBestRemaining = nil
	state.PreviousObservation = nil
	state.NoImprovementStreak = 0
	state.StagnationNudgeIssued = false
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

func equalPreviousScore(previous, current *EvaluatorObservation) bool {
	return previous != nil && current != nil &&
		previous.Score != nil && current.Score != nil &&
		*previous.Score == *current.Score
}

func repeatedRejectedCandidate(previous, current *EvaluatorObservation) bool {
	if previous == nil || current == nil || previous.Accepted || current.Accepted || current.CandidateID == "" {
		return false
	}
	if current.Score != nil && previous.Score != nil && *current.Score != *previous.Score {
		return false
	}
	return previous.CandidateID == current.CandidateID
}

func normalizeObservation(observation *EvaluatorObservation) *EvaluatorObservation {
	if observation == nil {
		return nil
	}
	out := &EvaluatorObservation{
		Accepted:    observation.Accepted,
		CandidateID: boundedSingleLine(observation.CandidateID, MaxCandidateBytes),
	}
	if observation.Score != nil && validScore(*observation.Score) {
		score := *observation.Score
		out.Score = &score
	}
	return out
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

func validRemaining(remaining int) bool {
	return remaining >= 0 && remaining <= MaxRemainingRequirements
}

func ensureEpoch(state *State) {
	if state.Epoch <= 0 {
		state.Epoch = 1
	}
}

func cloneState(state State) State {
	if state.StagnationBestScore != nil {
		score := *state.StagnationBestScore
		state.StagnationBestScore = &score
	}
	if state.StagnationBestRemaining != nil {
		remaining := *state.StagnationBestRemaining
		state.StagnationBestRemaining = &remaining
	}
	state.PreviousObservation = normalizeObservation(state.PreviousObservation)
	return state
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
