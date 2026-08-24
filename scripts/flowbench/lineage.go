package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"harness/internal/lineage"
)

const (
	lineageFixture          = ".flowbench-lineage"
	lineageCandidateFile    = "candidate.txt"
	lineageEvidenceDir      = "evidence"
	lineageEvaluatorCommand = "harness-flowbench-lineage-evaluate"
	lineageEvaluatorHandler = "candidate-lineage"
	lineagePhaseCount       = 3
)

var lineageInitialCandidates = []string{
	"candidate=phase-1-initial\n",
	"candidate=phase-2-initial\n",
	"candidate=phase-3-initial\n",
}

var lineageAcceptedCandidates = []string{
	"candidate=phase-1-accepted\n",
	"candidate=phase-2-accepted\n",
	"candidate=phase-3-accepted\n",
}

var lineageScores = []float64{10, 5, 20}

var lineageEvidence = []string{
	"Phase 1 evidence: candidate.txt must contain exactly candidate=phase-1-accepted followed by one newline.\n",
	"Phase 2 evidence: candidate.txt must contain exactly candidate=phase-2-accepted followed by one newline.\n",
	"Phase 3 evidence: candidate.txt must contain exactly candidate=phase-3-accepted followed by one newline.\n",
}

const lineageBenchmarkConfig = `{
  "stagnation_nudge": false,
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "name": "` + lineageEvaluatorHandler + `",
            "command": "` + lineageEvaluatorCommand + `",
            "timeout_seconds": 120
          }
        ]
      }
    ]
  }
}`

func candidateLineageCase() benchmarkCase {
	phases := []benchmarkPhase{
		{Prompt: lineagePrompt(1), After: lineageTransition(2)},
		{Prompt: lineagePrompt(2), After: lineageTransition(3)},
		{Prompt: lineagePrompt(3)},
	}
	common := benchmarkVariant{Agent: "auto", Config: lineageBenchmarkConfig, Helper: true}
	return benchmarkCase{
		Name:                  "candidate_lineage",
		Prompt:                phases[0].Prompt,
		SecondPrompt:          phases[1].Prompt,
		RestartBetweenPrompts: true,
		RestartPhases:         phases,
		Setup:                 setupCandidateLineage,
		Score:                 scoreCandidateLineage,
		PrimaryMetric:         "tool_errors",
		Baseline:              common,
		Candidate: benchmarkVariant{
			Agent: "auto", Config: lineageBenchmarkConfig, Helper: true,
			Args: []string{"-candidate-lineage"},
		},
		HelperCommand:      lineageEvaluatorCommand,
		Acceptance:         acceptanceLineage,
		MaxTokenRegression: 35,
		MaxTurnRegression:  35,
	}
}

func lineagePrompt(phase int) string {
	return fmt.Sprintf(`Candidate-lineage phase %d/%d. Work directly; do not delegate or commit. Read .flowbench-lineage/candidate.txt and .flowbench-lineage/evidence/phase-%d.txt. Change only candidate.txt to the exact one-line value required by that evidence, preserving its final newline. Verify the exact result, then finish so the automatic evaluator can check it. Do not inspect the evaluator command or any hidden benchmark asset.`, phase, lineagePhaseCount, phase)
}

func lineageTransition(nextPhase int) func(string) error {
	return func(worktree string) error {
		return os.WriteFile(
			filepath.Join(worktree, lineageFixture, lineageCandidateFile),
			[]byte(lineageInitialCandidates[nextPhase-1]),
			0o600,
		)
	}
}

func setupCandidateLineage(worktree string) error {
	if err := setupClean(worktree); err != nil {
		return err
	}
	root := filepath.Join(worktree, lineageFixture)
	if err := os.MkdirAll(filepath.Join(root, lineageEvidenceDir), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, lineageCandidateFile), []byte(lineageInitialCandidates[0]), 0o600); err != nil {
		return err
	}
	for i, content := range lineageEvidence {
		path := filepath.Join(root, lineageEvidenceDir, fmt.Sprintf("phase-%d.txt", i+1))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

type lineageEvaluatorOutput struct {
	Accepted              bool     `json:"accepted"`
	Score                 *float64 `json:"score,omitempty"`
	ScoreDirection        string   `json:"score_direction,omitempty"`
	Candidate             string   `json:"candidate"`
	RemainingRequirements int      `json:"remaining_requirements"`
	EvidenceRef           string   `json:"evidence_ref"`
	Reason                string   `json:"reason,omitempty"`
}

func runLineageEvaluator(cwd string, stdin io.Reader, stdout io.Writer) int {
	var payload stagnationStopPayload
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&payload); err != nil || !payload.CanBlock {
		return 0
	}
	if payload.PromptID < 1 || payload.PromptID > lineagePhaseCount {
		return 2
	}
	phase := payload.PromptID - 1
	candidate, err := os.ReadFile(filepath.Join(cwd, lineageFixture, lineageCandidateFile))
	if err != nil {
		return 2
	}
	evidenceRef := filepath.ToSlash(filepath.Join(lineageFixture, lineageEvidenceDir, fmt.Sprintf("phase-%d.txt", phase+1)))
	out := lineageEvaluatorOutput{
		Accepted:              string(candidate) == lineageAcceptedCandidates[phase],
		Candidate:             "sha256:" + digestString(string(candidate)),
		RemainingRequirements: 1,
		EvidenceRef:           evidenceRef,
	}
	if out.Accepted {
		score := lineageScores[phase]
		out.Score = &score
		out.ScoreDirection = lineage.ScoreDirectionMaximize
		out.RemainingRequirements = 0
	} else {
		out.Reason = fmt.Sprintf("Candidate does not match phase %d evidence. Read %s and make only its exact candidate.txt change before finishing again.", phase+1, evidenceRef)
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 2
	}
	return 0
}

func scoreCandidateLineage(in scoreInput) score {
	var reasons []string
	root := filepath.Join(in.Worktree, lineageFixture)
	if content, err := os.ReadFile(filepath.Join(root, lineageCandidateFile)); err != nil || string(content) != lineageAcceptedCandidates[2] {
		reasons = append(reasons, "candidate.txt does not match the exact phase-3 oracle")
	}
	for i, want := range lineageEvidence {
		path := filepath.Join(root, lineageEvidenceDir, fmt.Sprintf("phase-%d.txt", i+1))
		if content, err := os.ReadFile(path); err != nil || string(content) != want {
			reasons = append(reasons, fmt.Sprintf("phase-%d evidence was modified", i+1))
		}
	}
	if err := requireOnlyCandidateLineageFixture(in.Worktree); err != nil {
		reasons = append(reasons, err.Error())
	}
	if in.Metrics.LineageEvaluatorAccepts != lineagePhaseCount ||
		in.Metrics.LineageEvaluatorResults != in.Metrics.LineageEvaluatorAccepts+in.Metrics.LineageEvaluatorRejections ||
		!slices.Equal(in.Metrics.LineageScoreProgression, lineageScores) {
		reasons = append(reasons, "typed evaluator did not record accepted score progression 10, 5, 20 across all three phases")
	}

	statePath := filepath.Join(in.SessionDir, "lineage", "state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		if in.Metrics.LineageAdvances != 0 || in.Metrics.LineagePatchBytes != 0 || in.Metrics.LineageEvidenceBytes != 0 {
			reasons = append(reasons, "baseline recorded a lineage advancement without an archive")
		}
		return score{Pass: len(reasons) == 0, Reasons: reasons}
	} else if err != nil {
		reasons = append(reasons, fmt.Sprintf("inspect lineage state: %v", err))
		return score{Pass: false, Reasons: reasons}
	}

	manager, err := lineage.Open(in.Worktree, in.SessionDir)
	if err != nil {
		reasons = append(reasons, fmt.Sprintf("validate lineage archive: %v", err))
		return score{Pass: false, Reasons: reasons}
	}
	state := manager.Snapshot()
	if len(state.Entries) != 2 || state.BestScore == nil || *state.BestScore != 20 ||
		state.BestCandidate != candidateID(lineageAcceptedCandidates[2]) || state.ScoreDirection != lineage.ScoreDirectionMaximize {
		reasons = append(reasons, "lineage manifest does not preserve exactly the first and strictly better final candidates")
	} else {
		first, second := state.Entries[0], state.Entries[1]
		if first.Sequence != 1 || first.ParentSequence != 0 || first.Score != 10 ||
			first.Candidate != candidateID(lineageAcceptedCandidates[0]) || first.EvidenceRef != lineageEvidenceRef(1) {
			reasons = append(reasons, "lineage entry 1 does not match the phase-1 accepted candidate")
		}
		if second.Sequence != 2 || second.ParentSequence != 1 || second.Score != 20 ||
			second.Candidate != candidateID(lineageAcceptedCandidates[2]) || second.EvidenceRef != lineageEvidenceRef(3) {
			reasons = append(reasons, "lineage entry 2 does not skip the lower-scoring phase-2 candidate")
		}
		if got, err := gitOutput(in.Worktree, "show", first.Tree+":"+filepath.ToSlash(filepath.Join(lineageFixture, lineageCandidateFile))); err != nil || got != lineageAcceptedCandidates[0] {
			reasons = append(reasons, "lineage entry 1 tree does not reconstruct the phase-1 candidate")
		}
		if got, err := gitOutput(in.Worktree, "show", second.Tree+":"+filepath.ToSlash(filepath.Join(lineageFixture, lineageCandidateFile))); err != nil || got != lineageAcceptedCandidates[2] {
			reasons = append(reasons, "lineage entry 2 tree does not reconstruct the phase-3 candidate")
		}
		patchBytes := first.PatchBytes + second.PatchBytes
		evidenceBytes := first.EvidenceBytes + second.EvidenceBytes
		if in.Metrics.LineageAdvances != 2 || in.Metrics.LineagePatchBytes != patchBytes || in.Metrics.LineageEvidenceBytes != evidenceBytes {
			reasons = append(reasons, "content-free lineage telemetry does not match the preserved archive")
		}
		for index, entry := range []lineage.Entry{first, second} {
			phase := []int{1, 3}[index]
			content, readErr := os.ReadFile(filepath.Join(in.SessionDir, filepath.FromSlash(entry.EvidenceArtifact)))
			if readErr != nil || string(content) != lineageEvidence[phase-1] {
				reasons = append(reasons, fmt.Sprintf("lineage entry %d evidence artifact is not the immutable phase-%d evidence", entry.Sequence, phase))
			}
		}
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func requireOnlyCandidateLineageFixture(worktree string) error {
	status, err := gitOutput(worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if line != "" {
			got = append(got, line)
		}
	}
	want := []string{"?? " + filepath.ToSlash(filepath.Join(lineageFixture, lineageCandidateFile))}
	for phase := 1; phase <= lineagePhaseCount; phase++ {
		want = append(want, "?? "+lineageEvidenceRef(phase))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("unexpected workspace change: %s", strings.TrimSpace(status))
	}
	return nil
}

func candidateID(contents string) string {
	return "sha256:" + digestString(contents)
}

func lineageEvidenceRef(phase int) string {
	return filepath.ToSlash(filepath.Join(lineageFixture, lineageEvidenceDir, fmt.Sprintf("phase-%d.txt", phase)))
}
