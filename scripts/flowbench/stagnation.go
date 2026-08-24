package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	stagnationFixture          = ".flowbench-stagnation"
	stagnationStateFile        = "phase.txt"
	stagnationEvaluatorCommand = "harness-flowbench-stagnation-evaluate"
	stagnationScoreHandler     = "stagnation-score"
	stagnationLatencyHandler   = "stagnation-latency"
	stagnationPhaseCount       = 12
)

const stagnationBenchmarkConfig = `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "name": "` + stagnationScoreHandler + `",
            "command": "` + stagnationEvaluatorCommand + ` score",
            "timeout_seconds": 120
          },
          {
            "type": "command",
            "name": "` + stagnationLatencyHandler + `",
            "command": "` + stagnationEvaluatorCommand + ` latency",
            "timeout_seconds": 120
          }
        ]
      }
    ]
  }
}`

type stagnationStep struct {
	lane                  string
	accepted              bool
	score                 *float64
	scoreDirection        string
	candidate             string
	remainingRequirements *int
}

var stagnationSteps = []stagnationStep{
	{lane: "score", score: floatPtr(0), scoreDirection: "maximize", candidate: "score-a"},
	{lane: "score", score: floatPtr(0), scoreDirection: "maximize", candidate: "score-b"},
	{lane: "score", score: floatPtr(-1), scoreDirection: "maximize", candidate: "score-c"},
	{lane: "score", score: floatPtr(1), scoreDirection: "maximize", candidate: "score-d"},
	{lane: "score", candidate: "phase-ready", remainingRequirements: intPtr(0)},
	{lane: "score", candidate: "phase-expanded", remainingRequirements: intPtr(4)},
	{lane: "score", candidate: "unknown-cycle"},
	{lane: "score", candidate: "unknown-cycle"},
	{lane: "latency", score: floatPtr(1), scoreDirection: "minimize", candidate: "latency-a"},
	{lane: "latency", score: floatPtr(2), scoreDirection: "minimize", candidate: "latency-b"},
	{lane: "latency", score: floatPtr(0), scoreDirection: "minimize", candidate: "latency-c"},
	{lane: "latency", accepted: true, score: floatPtr(0), scoreDirection: "minimize", candidate: "latency-c", remainingRequirements: intPtr(0)},
}

func stagnationDetectionCase() benchmarkCase {
	phases := make([]benchmarkPhase, 0, stagnationPhaseCount)
	for phase := 1; phase <= stagnationPhaseCount; phase++ {
		phases = append(phases, benchmarkPhase{Prompt: stagnationPrompt(phase)})
	}
	variant := benchmarkVariant{Config: stagnationBenchmarkConfig, Helper: true}
	return benchmarkCase{
		Name:                  "stagnation_detection",
		Prompt:                phases[0].Prompt,
		SecondPrompt:          phases[1].Prompt,
		RestartBetweenPrompts: true,
		RestartPhases:         phases,
		Setup:                 setupStagnationDetection,
		Score:                 scoreStagnationDetection,
		PrimaryMetric:         "trajectory_max_no_improvement_streak",
		Baseline:              variant,
		Candidate:             variant,
		HelperCommand:         stagnationEvaluatorCommand,
		Acceptance:            acceptanceStagnation,
	}
}

func stagnationPrompt(phase int) string {
	return fmt.Sprintf("Synthetic detector phase %d/%d. Reply exactly READY without calling any tool, inspecting any file, or taking any other action. If the automatic Stop evaluator requests one continuation, reply exactly READY again without tools. This run validates host-only telemetry and does not ask you to solve or change anything.", phase, stagnationPhaseCount)
}

func setupStagnationDetection(dir string) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	root := filepath.Join(dir, stagnationFixture)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, stagnationStateFile), []byte("0\n"), 0o600)
}

func scoreStagnationDetection(in scoreInput) score {
	var reasons []string
	state, err := os.ReadFile(filepath.Join(in.Worktree, stagnationFixture, stagnationStateFile))
	if err != nil || string(state) != strconv.Itoa(stagnationPhaseCount)+"\n" {
		reasons = append(reasons, "synthetic evaluator did not complete all 12 phases")
	}
	if in.Metrics.StagnationEvaluatorResults != stagnationPhaseCount || in.Metrics.StagnationEvaluatorRejections != stagnationPhaseCount-1 || in.Metrics.StagnationEvaluatorAccepts != 1 {
		reasons = append(reasons, "typed evaluator lifecycle is not the required 11 rejections followed by one acceptance")
	}
	if totalToolCalls(in.Metrics.ToolCalls) != 0 {
		reasons = append(reasons, "model called a tool during the tool-free detector trace")
	}
	if !candidateStagnationTrace(in.Metrics) && !legacyStagnationTrace(in.Metrics) {
		reasons = append(reasons, "shadow telemetry does not match either the directed candidate or unordered legacy oracle")
	}
	if err := requireOnlyStagnationFixture(in.Worktree); err != nil {
		reasons = append(reasons, err.Error())
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func candidateStagnationTrace(m metrics) bool {
	return m.NoImprovementStreak == 0 &&
		m.MaxNoImprovementStreak == 2 &&
		m.StagnationBaselines == 3 &&
		m.StagnationImprovements == 3 &&
		m.StagnationPlateaus == 2 &&
		m.StagnationRegressions == 3 &&
		m.StagnationIndeterminate == 1 &&
		m.UnorderedScoreEvaluations == 0 &&
		m.StagnationLaneResets == 1
}

func floatPtr(value float64) *float64 { return &value }

func intPtr(value int) *int { return &value }

func legacyStagnationTrace(m metrics) bool {
	return m.NoImprovementStreak == 0 &&
		m.MaxNoImprovementStreak == 2 &&
		m.StagnationBaselines == 3 &&
		m.StagnationImprovements == 1 &&
		m.StagnationPlateaus == 2 &&
		m.StagnationRegressions == 1 &&
		m.StagnationIndeterminate == 5 &&
		m.UnorderedScoreEvaluations == 8 &&
		m.StagnationLaneResets == 1
}

func totalToolCalls(calls map[string]int) int {
	total := 0
	for _, count := range calls {
		total += count
	}
	return total
}

func requireOnlyStagnationFixture(worktree string) error {
	status, err := gitOutput(worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	want := "?? " + filepath.ToSlash(filepath.Join(stagnationFixture, stagnationStateFile))
	if strings.TrimSpace(status) != want {
		return fmt.Errorf("unexpected workspace change: %s", strings.TrimSpace(status))
	}
	return nil
}

type stagnationStopPayload struct {
	PromptID int  `json:"prompt_id"`
	CanBlock bool `json:"can_block"`
}

type stagnationEvaluatorOutput struct {
	Accepted              bool     `json:"accepted"`
	Score                 *float64 `json:"score,omitempty"`
	ScoreDirection        string   `json:"score_direction,omitempty"`
	Candidate             string   `json:"candidate,omitempty"`
	RemainingRequirements *int     `json:"remaining_requirements,omitempty"`
	Reason                string   `json:"reason,omitempty"`
}

// runStagnationEvaluator emits one fixed semantic result per prompt. Two Stop
// handlers call it sequentially, but only the lane assigned to that phase
// responds. The phase comes from the host payload rather than mutable helper
// state, so retries cannot advance the oracle accidentally.
func runStagnationEvaluator(cwd string, args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 1 || (args[0] != "score" && args[0] != "latency") {
		return 2
	}
	var payload stagnationStopPayload
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&payload); err != nil || !payload.CanBlock {
		return 0
	}
	if payload.PromptID < 1 || payload.PromptID > len(stagnationSteps) {
		return 2
	}
	step := stagnationSteps[payload.PromptID-1]
	if step.lane != args[0] {
		return 0
	}
	path := filepath.Join(cwd, stagnationFixture, stagnationStateFile)
	previous, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(previous)) != strconv.Itoa(payload.PromptID-1) {
		return 2
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(payload.PromptID)+"\n"), 0o600); err != nil {
		return 2
	}
	out := stagnationEvaluatorOutput{
		Accepted:              step.accepted,
		Score:                 step.score,
		ScoreDirection:        step.scoreDirection,
		Candidate:             step.candidate,
		RemainingRequirements: step.remainingRequirements,
	}
	if !step.accepted {
		out.Reason = "Synthetic detector sample recorded. Reply exactly READY once more without tools."
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 2
	}
	return 0
}
