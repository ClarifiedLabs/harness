package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"harness/internal/trajectory"
)

// ReconstructTrajectory deterministically derives the shadow projection from
// canonical raw events. ToolDiff remains a legacy/backfill path; new runs also
// record ToolMutation so path observation does not depend on diff rendering.
func ReconstructTrajectory(events []Event) trajectory.State {
	state := trajectory.Normalize(nil)
	for _, event := range events {
		state = applyTrajectoryEvent(state, event)
	}
	return state
}

func applyTrajectoryEvent(state trajectory.State, event Event) trajectory.State {
	switch event.Type {
	case EventTrajectorySeed:
		if event.Trajectory != nil {
			return trajectory.Normalize(event.Trajectory)
		}
	case EventBranch:
		return trajectory.ApplyBranch(state)
	case EventToolMutation:
		if event.ToolMutation != nil {
			return trajectory.ApplyModifiedPaths(state, event.ToolMutation.Paths)
		}
	case EventToolDiff:
		if event.Path != "" {
			return trajectory.ApplyConfirmedPaths(state, []string{event.Path})
		}
	case EventEvaluatorResult:
		if result := event.EvaluatorResult; result != nil {
			return trajectory.ApplyEvaluation(state, trajectory.EvaluationInput{
				Handler:               result.Handler,
				Accepted:              result.Accepted,
				Score:                 result.Score,
				ScoreDirection:        result.ScoreDirection,
				Candidate:             result.Candidate,
				RemainingRequirements: result.RemainingRequirements,
				EvidenceRef:           result.EvidenceRef,
				Prompt:                event.Prompt,
				Turn:                  event.Turn,
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
		observed = observed || isTrajectoryEvent(event)
		state = applyTrajectoryEvent(state, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return &state, observed, nil
}

func isTrajectoryEvent(event Event) bool {
	switch event.Type {
	case EventTrajectorySeed, EventBranch, EventToolMutation, EventToolDiff, EventEvaluatorResult, EventStagnationNudge:
		return true
	default:
		return false
	}
}

// reconcileTrajectory treats raw.ndjson as the canonical transition stream.
// Shadow-state replay failures never make an otherwise healthy session
// unloadable; the last atomically saved projection remains the fallback.
func reconcileTrajectory(dir string, saved *Session) {
	if saved == nil {
		return
	}
	if saved.Trajectory != nil {
		normalized := trajectory.Normalize(saved.Trajectory)
		saved.Trajectory = &normalized
	}
	replayed, observed, err := loadTrajectoryProjection(dir)
	if err == nil && observed {
		saved.Trajectory = replayed
	}
}
