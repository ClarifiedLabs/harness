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
	stagnationRecoveryFixture       = ".flowbench-stagnation-recovery"
	stagnationRecoveryCandidateFile = "candidate.txt"
	stagnationRecoveryEvidenceDir   = "evidence"
	stagnationRecoveryEvidenceFile  = "strategy.txt"
	stagnationRecoveryStateFile     = "phase.txt"
	stagnationRecoveryCommand       = "harness-flowbench-stagnation-recovery-evaluate"
	stagnationRecoveryHandler       = "stagnation-recovery"
	stagnationRecoveryPhaseCount    = 4
)

const stagnationRecoveryInitial = "strategy=repeat\ncheckpoint=unchanged\n"
const stagnationRecoveryFinal = "strategy=alternate-17\ncheckpoint=unchanged\n"
const stagnationRecoveryEvidence = `The repeated strategy is not making progress.
Edit only .flowbench-stagnation-recovery/candidate.txt.
Change its first line to exactly:
strategy=alternate-17
Preserve the second line and every other file exactly.
`

const stagnationRecoveryConfigTemplate = `{
  "stagnation_nudge": %t,
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "name": "` + stagnationRecoveryHandler + `",
            "command": "` + stagnationRecoveryCommand + `",
            "timeout_seconds": 120
          }
        ]
      }
    ]
  }
}`

func stagnationRecoveryCase() benchmarkCase {
	phases := make([]benchmarkPhase, 0, stagnationRecoveryPhaseCount)
	for phase := 1; phase <= stagnationRecoveryPhaseCount; phase++ {
		phases = append(phases, benchmarkPhase{Prompt: stagnationRecoveryPrompt(phase)})
	}
	return benchmarkCase{
		Name:                  "stagnation_recovery",
		Prompt:                phases[0].Prompt,
		SecondPrompt:          phases[1].Prompt,
		RestartBetweenPrompts: true,
		RestartPhases:         phases,
		Setup:                 setupStagnationRecovery,
		Score:                 scoreStagnationRecovery,
		PrimaryMetric:         "stagnation_recovery_failures",
		Baseline: benchmarkVariant{
			Config: fmt.Sprintf(stagnationRecoveryConfigTemplate, false), Helper: true,
		},
		Candidate: benchmarkVariant{
			Config: fmt.Sprintf(stagnationRecoveryConfigTemplate, true), Helper: true,
		},
		HelperCommand:      stagnationRecoveryCommand,
		Acceptance:         acceptanceStagnationRecovery,
		MaxTokenRegression: 100,
		MaxTurnRegression:  100,
	}
}

func stagnationRecoveryPrompt(phase int) string {
	return fmt.Sprintf(`Recovery phase %d/%d. Reply exactly READY without calling tools, inspecting files, or changing the workspace. If the automatic Stop evaluator rejects this response, reply exactly READY again without tools unless its corrective turn also contains the exact marker [host strategy reset]. Only when that marker is present, follow the reset: read the evaluator's referenced evidence and the named candidate file, make the smallest exact change the evidence requires, verify it, and then finish. Do not inspect or change anything before the marker, and do not bypass the evaluator.`, phase, stagnationRecoveryPhaseCount)
}

func setupStagnationRecovery(dir string) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	root := filepath.Join(dir, stagnationRecoveryFixture)
	if err := os.MkdirAll(filepath.Join(root, stagnationRecoveryEvidenceDir), 0o755); err != nil {
		return err
	}
	for path, content := range map[string]string{
		stagnationRecoveryCandidateFile: stagnationRecoveryInitial,
		stagnationRecoveryStateFile:     "0\n",
		filepath.Join(stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile): stagnationRecoveryEvidence,
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func scoreStagnationRecovery(in scoreInput) score {
	var reasons []string
	root := filepath.Join(in.Worktree, stagnationRecoveryFixture)
	if content, err := os.ReadFile(filepath.Join(root, stagnationRecoveryCandidateFile)); err != nil || string(content) != stagnationRecoveryFinal {
		reasons = append(reasons, "candidate did not apply the exact evidence-backed strategy change")
	}
	if content, err := os.ReadFile(filepath.Join(root, stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile)); err != nil || string(content) != stagnationRecoveryEvidence {
		reasons = append(reasons, "strategy evidence was modified")
	}
	if content, err := os.ReadFile(filepath.Join(root, stagnationRecoveryStateFile)); err != nil || string(content) != strconv.Itoa(stagnationRecoveryPhaseCount)+"\n" {
		reasons = append(reasons, "recovery evaluator did not complete all four phases")
	}
	if in.Metrics.RecoveryEvaluatorResults != stagnationRecoveryPhaseCount ||
		in.Metrics.RecoveryEvaluatorRejections != stagnationRecoveryPhaseCount-1 ||
		in.Metrics.RecoveryEvaluatorAccepts != 1 {
		reasons = append(reasons, "typed recovery lifecycle is not three rejections followed by one acceptance")
	}
	if err := requireOnlyStagnationRecoveryFixture(in.Worktree); err != nil {
		reasons = append(reasons, err.Error())
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func requireOnlyStagnationRecoveryFixture(worktree string) error {
	status, err := gitOutput(worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	want := []string{
		"?? " + filepath.ToSlash(filepath.Join(stagnationRecoveryFixture, stagnationRecoveryCandidateFile)),
		"?? " + filepath.ToSlash(filepath.Join(stagnationRecoveryFixture, stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile)),
		"?? " + filepath.ToSlash(filepath.Join(stagnationRecoveryFixture, stagnationRecoveryStateFile)),
	}
	got := strings.Split(strings.TrimSpace(status), "\n")
	if len(got) != len(want) {
		return fmt.Errorf("unexpected workspace change: %s", strings.TrimSpace(status))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("unexpected workspace change: %s", strings.TrimSpace(status))
		}
	}
	return nil
}

type stagnationRecoveryOutput struct {
	Accepted              bool    `json:"accepted"`
	Score                 float64 `json:"score"`
	ScoreDirection        string  `json:"score_direction"`
	Candidate             string  `json:"candidate"`
	RemainingRequirements int     `json:"remaining_requirements"`
	EvidenceRef           string  `json:"evidence_ref,omitempty"`
	Reason                string  `json:"reason,omitempty"`
}

func runStagnationRecoveryEvaluator(cwd string, stdin io.Reader, stdout io.Writer) int {
	var payload stagnationStopPayload
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&payload); err != nil || !payload.CanBlock {
		return 0
	}
	if payload.PromptID < 1 || payload.PromptID > stagnationRecoveryPhaseCount {
		return 2
	}
	root := filepath.Join(cwd, stagnationRecoveryFixture)
	statePath := filepath.Join(root, stagnationRecoveryStateFile)
	previous, err := os.ReadFile(statePath)
	if err != nil || strings.TrimSpace(string(previous)) != strconv.Itoa(payload.PromptID-1) {
		return 2
	}
	if err := os.WriteFile(statePath, []byte(strconv.Itoa(payload.PromptID)+"\n"), 0o600); err != nil {
		return 2
	}
	candidate, err := os.ReadFile(filepath.Join(root, stagnationRecoveryCandidateFile))
	if err != nil {
		return 2
	}
	accepted := payload.PromptID == stagnationRecoveryPhaseCount && string(candidate) == stagnationRecoveryFinal
	out := stagnationRecoveryOutput{
		Accepted:              accepted,
		ScoreDirection:        "maximize",
		Candidate:             "strategy-repeat",
		RemainingRequirements: 1,
		EvidenceRef:           filepath.ToSlash(filepath.Join(stagnationRecoveryFixture, stagnationRecoveryEvidenceDir, stagnationRecoveryEvidenceFile)),
	}
	if accepted {
		out.Score = 1
		out.Candidate = "strategy-alternate-17"
		out.RemainingRequirements = 0
		out.EvidenceRef = ""
	} else {
		out.Reason = "The candidate strategy did not improve. Reply exactly READY without tools unless this corrective turn also includes [host strategy reset]."
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 2
	}
	return 0
}
