package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"harness/internal/trajectory"
)

// ReconstructTrajectory deterministically derives runtime stagnation policy
// from the only canonical transition events: evaluator results, delivered
// stagnation nudges, and conversation branches. Tool telemetry is invisible to
// policy reconstruction.
func ReconstructTrajectory(events []Event) trajectory.State {
	state := trajectory.Normalize(nil)
	for _, event := range events {
		state = applyTrajectoryEvent(state, event)
	}
	return state
}

func applyTrajectoryEvent(state trajectory.State, event Event) trajectory.State {
	switch event.Type {
	case EventBranch:
		return trajectory.ApplyBranch(state)
	case EventEvaluatorResult:
		if result := event.EvaluatorResult; result != nil {
			return trajectory.ApplyEvaluation(state, trajectory.EvaluationInput{
				Handler:               result.Handler,
				Accepted:              result.Accepted,
				Score:                 result.Score,
				ScoreDirection:        result.ScoreDirection,
				Candidate:             result.Candidate,
				RemainingRequirements: result.RemainingRequirements,
			})
		}
	case EventStagnationNudge:
		return trajectory.ApplyStagnationNudge(state)
	}
	return state
}

func loadTrajectoryProjection(dir string) (*trajectory.State, bool, error) {
	f, err := os.Open(filepath.Join(dir, eventLog))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, followReadBufferSize), maxReplayRecordSize)
	state := trajectory.Normalize(nil)
	observed := false
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, false, fmt.Errorf("session: trajectory replay decode: %w", err)
		}
		if err := validateTrajectoryEvent(state, event); err != nil {
			return nil, false, err
		}
		if isTrajectoryEvent(event) {
			observed = true
			state = applyTrajectoryEvent(state, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return &state, observed, nil
}

func validateTrajectoryEvent(state trajectory.State, event Event) error {
	switch event.Type {
	case EventEvaluatorResult:
		if event.EvaluatorResult == nil {
			return fmt.Errorf("session: trajectory replay evaluator_result is missing payload")
		}
	case EventStagnationNudge:
		nudge := event.StagnationNudge
		if nudge == nil {
			return fmt.Errorf("session: trajectory replay stagnation_nudge is missing payload")
		}
		if nudge.Threshold <= 0 || nudge.Streak != state.NoImprovementStreak || nudge.Streak < nudge.Threshold || !trajectory.CanStagnationNudge(state, nudge.Threshold) {
			return fmt.Errorf("session: trajectory replay stagnation_nudge payload is inconsistent with evaluator state")
		}
	}
	return nil
}

func isTrajectoryEvent(event Event) bool {
	switch event.Type {
	case EventBranch, EventEvaluatorResult, EventStagnationNudge:
		return true
	default:
		return false
	}
}

// reconcileTrajectory treats raw.ndjson as the only policy source. Missing or
// malformed replay data disables policy for this process rather than trusting
// a cached state.json projection or a partial replay. A valid policy-free stream
// remains eligible to establish a fresh lane from future evaluator events.
func reconcileTrajectory(dir string, saved *Session) {
	if saved == nil {
		return
	}
	saved.Trajectory = nil
	saved.TrajectoryPolicyDisabled = false
	replayed, observed, err := loadTrajectoryProjection(dir)
	if err != nil {
		saved.TrajectoryPolicyDisabled = true
		return
	}
	if observed {
		saved.Trajectory = replayed
	}
}
