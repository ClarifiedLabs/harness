package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness/internal/lineage"
)

const (
	defaultStackFixture         = ".flowbench-default-stack"
	defaultStackEvidenceDir     = "evidence"
	defaultStackMilestoneFile   = "MILESTONE.md"
	defaultStackLegacyPhaseFile = "milestone.txt"
	defaultStackVerifierCommand = "harness-flowbench-default-stack-evaluate"
	defaultStackEvaluatorName   = "default-stack-evaluator"
	defaultStackAttempts        = 3
	defaultStackMinimumTokens   = 1_000_000
)

var defaultStackPhaseCount = len(defaultStackMilestones) * defaultStackAttempts

type defaultStackMilestone struct {
	name     string
	contract string
	score    float64
}

var defaultStackMilestones = []defaultStackMilestone{
	{name: "kv-core", contract: defaultStackKVCoreContract, score: 16},
	{name: "kv-durability", contract: defaultStackKVDurabilityContract, score: 33},
	{name: "planner-core", contract: defaultStackPlannerCoreContract, score: 50},
	{name: "planner-durability", contract: defaultStackPlannerDurabilityContract, score: 66},
	{name: "framing-core", contract: defaultStackFramingCoreContract, score: 83},
	{name: "framing-log", contract: defaultStackFramingLogContract, score: 100},
}

func defaultStackMarathonCase() benchmarkCase {
	phases := make([]benchmarkPhase, 0, defaultStackPhaseCount)
	for milestoneIndex := range defaultStackMilestones {
		milestoneIndex := milestoneIndex
		for attempt := 1; attempt <= defaultStackAttempts; attempt++ {
			phase := benchmarkPhase{Prompt: defaultStackPrompt(milestoneIndex, attempt)}
			if attempt == defaultStackAttempts {
				phase.After = func(worktree string) error {
					return advanceDefaultStackMilestone(worktree, milestoneIndex)
				}
				phase.CompactAfter = milestoneIndex+1 < len(defaultStackMilestones)
			}
			phases = append(phases, phase)
		}
	}
	return benchmarkCase{
		Name:                  "default_stack_marathon",
		Prompt:                phases[0].Prompt,
		SecondPrompt:          phases[defaultStackAttempts].Prompt,
		RestartBetweenPrompts: true,
		RestartPhases:         phases,
		Setup:                 setupDefaultStackMarathon,
		Score:                 scoreDefaultStackMarathon,
		PrimaryMetric:         "evaluator_turns_to_best",
		MinimumReductionPct:   0,
		Baseline: benchmarkVariant{
			Agent: "auto", Config: makeDefaultStackConfig(false), Helper: true,
		},
		Candidate: benchmarkVariant{
			Agent: "auto", Config: makeDefaultStackConfig(true), Helper: true,
		},
		HelperCommand:      defaultStackVerifierCommand,
		MaxTokenRegression: 35,
		MaxTurnRegression:  35,
		MinimumRunTokens:   defaultStackMinimumTokens,
		RunTimeout:         4 * time.Hour,
	}
}

func makeDefaultStackConfig(stagnationNudge bool) string {
	return fmt.Sprintf(`{
  "stagnation_nudge": %t,
  "context_window": 32768,
  "compact_trigger_percent": 60,
  "compact_target_percent": 40,
  "compact_keep_turns": 1,
  "compact_keep_tokens": 1024,
  "compact_summary_max_tokens": 1200,
  "responses_stateful": false,
  "retention_policy": "disabled",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "name": "%s",
            "command": "%s",
            "timeout_seconds": 180
          }
        ]
      }
    ]
  }
}`, stagnationNudge, defaultStackEvaluatorName, defaultStackVerifierCommand)
}

func defaultStackEvaluatorAssetLeak(tool string, raw json.RawMessage) bool {
	switch tool {
	case "read", "edit", "write", "view_image":
		path := normalizeFixturePath(toolInputPath(raw))
		return defaultStackHiddenAssetReference(path) || filepath.Base(path) == defaultStackVerifierCommand ||
			strings.Contains(path, ".oracle") || strings.Contains(path, "scripts/flowbench/default_stack")
	case "shell":
		input, ok := parseShellMetricInput(raw)
		if !ok {
			text := string(raw)
			return strings.Contains(text, defaultStackVerifierCommand) || strings.Contains(text, ".oracle")
		}
		if defaultStackShellLeaks(input.Command, input.Argv) {
			return true
		}
		for _, step := range input.Steps {
			if defaultStackShellLeaks(step.Command, step.Argv) {
				return true
			}
		}
	}
	return false
}

func defaultStackShellLeaks(command string, argv []string) bool {
	text := command + " " + strings.Join(argv, " ")
	return defaultStackHiddenAssetReference(text) || strings.Contains(text, defaultStackVerifierCommand) ||
		strings.Contains(text, ".oracle") || strings.Contains(text, "scripts/flowbench/default_stack")
}

func defaultStackHiddenAssetReference(text string) bool {
	return strings.Contains(text, "zz_flowbench_hidden") || strings.Contains(text, "TestOracle")
}

func defaultStackPrompt(milestoneIndex, attempt int) string {
	milestone := defaultStackMilestones[milestoneIndex]
	prefix := fmt.Sprintf("Long-horizon project milestone %d/%d (%s), attempt %d/%d.", milestoneIndex+1, len(defaultStackMilestones), milestone.name, attempt, defaultStackAttempts)
	if attempt == 1 {
		return prefix + " Work directly; do not delegate or commit. Read .flowbench-default-stack/MILESTONE.md and the relevant existing Go sources. MILESTONE.md and milestone state are host-owned and read-only; never edit or recreate them. Implement the complete current milestone while preserving every earlier accepted behavior. Add or update focused public tests when useful. Run gofmt on changed Go files and `cd .flowbench-default-stack && go test ./...`, then finish so the opaque evaluator can run hidden behavioral tests. Do not invoke, locate, or inspect the evaluator command or hidden assets, and do not change files outside .flowbench-default-stack."
	}
	if attempt == 2 {
		return prefix + " Continue the same implementation in a fresh resumed process. Re-read the host-owned, read-only MILESTONE.md and inspect the current code and visible test results rather than assuming the prior attempt is complete; never edit or recreate milestone state. Diagnose and repair every remaining requirement, run gofmt and `cd .flowbench-default-stack && go test ./...`, then finish. If the opaque evaluator rejects, use only its referenced evidence and diagnostics for the bounded corrective turn. Do not delegate, commit, inspect hidden assets, or change files outside .flowbench-default-stack."
	}
	return prefix + " This is the final recovery and validation process for the current milestone. Work directly, inspect the actual code, and close every remaining contract gap. MILESTONE.md and milestone state remain host-owned and read-only; never edit or recreate them. Run gofmt and `cd .flowbench-default-stack && go test ./...`, then finish. If the opaque evaluator rejects, use its referenced evidence immediately; the host will advance only if the hidden behavioral oracle passes after this process. Do not delegate, commit, inspect hidden assets, or change files outside .flowbench-default-stack."
}

func setupDefaultStackMarathon(dir string) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	root := filepath.Join(dir, defaultStackFixture)
	if err := os.MkdirAll(filepath.Join(root, defaultStackEvidenceDir), 0o755); err != nil {
		return err
	}
	paths := make([]string, 0, len(defaultStackInitialFiles))
	for path := range defaultStackInitialFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(defaultStackInitialFiles[path]), 0o644); err != nil {
			return err
		}
	}
	if err := writeDefaultStackMilestone(root, 0); err != nil {
		return err
	}
	return nil
}

func writeDefaultStackMilestone(root string, index int) error {
	if index < 0 || index >= len(defaultStackMilestones) {
		return fmt.Errorf("default-stack milestone index %d is out of range", index)
	}
	return os.WriteFile(filepath.Join(root, defaultStackMilestoneFile), []byte(defaultStackMilestones[index].contract), 0o644)
}

func advanceDefaultStackMilestone(worktree string, index int) error {
	passed, diagnostics := runDefaultStackTestSuite(worktree, index)
	if !passed {
		return fmt.Errorf("default-stack milestone %d (%s) failed before transition: %s", index+1, defaultStackMilestones[index].name, diagnostics)
	}
	if index+1 == len(defaultStackMilestones) {
		return nil
	}
	return writeDefaultStackMilestone(filepath.Join(worktree, defaultStackFixture), index+1)
}

func runDefaultStackVerifier(cwd string, stdin io.Reader, stdout io.Writer) int {
	if stopCannotBlock(stdin) {
		return 0
	}
	index, err := readDefaultStackMilestone(cwd)
	if err != nil {
		return 2
	}
	candidate, err := defaultStackCandidateID(cwd)
	if err != nil {
		return 2
	}
	passed, diagnostics := runDefaultStackTestSuite(cwd, index)
	milestone := defaultStackMilestones[index]
	scoreValue := 0.0
	if index > 0 {
		scoreValue = defaultStackMilestones[index-1].score
	}
	remaining := 1
	if passed {
		scoreValue = milestone.score
		remaining = 0
	}
	evidence := writeDefaultStackEvidence(cwd, candidate, index, passed, diagnostics)
	reason := ""
	if !passed {
		reason = fmt.Sprintf("Milestone %d/%d (%s) remains rejected at score %.0f/100. Read %s, repair the complete current contract, rerun visible tests, and finish.", index+1, len(defaultStackMilestones), milestone.name, scoreValue, evidence)
	}
	writeEvaluatorJSON(stdout, evaluatorOutput{
		Accepted: passed, Score: &scoreValue, ScoreDirection: lineage.ScoreDirectionMaximize,
		Candidate: candidate, RemainingRequirements: &remaining, EvidenceRef: evidence, Reason: reason,
	})
	return 0
}

func readDefaultStackMilestone(cwd string) (int, error) {
	data, err := os.ReadFile(filepath.Join(cwd, defaultStackFixture, defaultStackMilestoneFile))
	if err != nil {
		return 0, err
	}
	for index, milestone := range defaultStackMilestones {
		if string(data) == milestone.contract {
			return index, nil
		}
	}
	return 0, fmt.Errorf("default-stack milestone contract does not match a host phase")
}

func runDefaultStackTestSuite(cwd string, milestoneIndex int) (bool, string) {
	root := filepath.Join(cwd, defaultStackFixture)
	temp, err := os.MkdirTemp("", "harness-flowbench-default-stack-oracle-*")
	if err != nil {
		return false, err.Error()
	}
	defer os.RemoveAll(temp)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel == defaultStackEvidenceDir {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source %s is a symlink", filepath.ToSlash(rel))
		}
		if rel != "go.mod" && (!strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		destination := filepath.Join(temp, rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o600)
	})
	if err != nil {
		return false, err.Error()
	}
	for rel, body := range defaultStackHiddenTests(milestoneIndex) {
		path := filepath.Join(temp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err.Error()
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return false, err.Error()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "./...")
	cmd.Dir = temp
	cmd.Env = setEnv(os.Environ(), "GOWORK", "off")
	cmd.Env = setEnv(cmd.Env, "GOPROXY", "off")
	cmd.Env = setEnv(cmd.Env, "GOSUMDB", "off")
	out, runErr := cmd.CombinedOutput()
	diagnostics := strings.ReplaceAll(string(out), temp, "<oracle>")
	diagnostics = boundedDefaultStackDiagnostics(diagnostics)
	if ctx.Err() != nil {
		return false, "hidden behavioral tests timed out"
	}
	return runErr == nil, diagnostics
}

func boundedDefaultStackDiagnostics(value string) string {
	value = strings.TrimSpace(value)
	const limit = 8000
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n[diagnostics truncated]"
}

func defaultStackCandidateID(cwd string) (string, error) {
	root := filepath.Join(cwd, defaultStackFixture)
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && entry.Name() == defaultStackEvidenceDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "go.mod" || strings.HasSuffix(rel, ".go") {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s\x00%d\x00", path, len(data))
		b.Write(data)
	}
	return "sha256:" + digestString(b.String())[:16], nil
}

func writeDefaultStackEvidence(cwd, candidate string, milestoneIndex int, accepted bool, diagnostics string) string {
	milestone := defaultStackMilestones[milestoneIndex]
	state := "rejected"
	if accepted {
		state = "accepted"
	}
	name := strings.TrimPrefix(candidate, "sha256:")
	ref := filepath.ToSlash(filepath.Join(defaultStackFixture, defaultStackEvidenceDir, fmt.Sprintf("%02d-%s-%s-%s.txt", milestoneIndex+1, milestone.name, state, name)))
	var b strings.Builder
	fmt.Fprintf(&b, "DEFAULT STACK MILESTONE %d/%d: %s\nCandidate: %s\nAccepted: %t\n\n%s", milestoneIndex+1, len(defaultStackMilestones), milestone.name, candidate, accepted, milestone.contract)
	if diagnostics != "" {
		fmt.Fprintf(&b, "\nHIDDEN BEHAVIORAL TEST DIAGNOSTICS\n%s\n", diagnostics)
	}
	_ = os.WriteFile(filepath.Join(cwd, filepath.FromSlash(ref)), []byte(b.String()), 0o600)
	return ref
}

func scoreDefaultStackMarathon(in scoreInput) score {
	var reasons []string
	finalIndex := len(defaultStackMilestones) - 1
	passed, diagnostics := runDefaultStackTestSuite(in.Worktree, finalIndex)
	if !passed {
		reasons = append(reasons, "final multi-package behavioral oracle failed: "+diagnostics)
	}
	index, err := readDefaultStackMilestone(in.Worktree)
	if err != nil || index != finalIndex {
		reasons = append(reasons, "fixture is not in the final framing-log milestone")
	}
	contract, err := os.ReadFile(filepath.Join(in.Worktree, defaultStackFixture, defaultStackMilestoneFile))
	if err != nil || string(contract) != defaultStackMilestones[finalIndex].contract {
		reasons = append(reasons, "final milestone contract was modified")
	}
	if in.Metrics.DefaultStackEvaluatorAssetLeaks > 0 {
		reasons = append(reasons, "model accessed the default-stack evaluator command or a hidden evaluator asset")
	}
	if in.Metrics.DefaultStackControlMutations > 0 {
		reasons = append(reasons, "model modified host-owned default-stack milestone control state")
	}
	if in.Metrics.DefaultStackEvaluatorResults != defaultStackPhaseCount {
		reasons = append(reasons, fmt.Sprintf("typed evaluator recorded %d results, want %d", in.Metrics.DefaultStackEvaluatorResults, defaultStackPhaseCount))
	}
	if !in.Metrics.EvaluatorBestScoreAvailable || in.Metrics.EvaluatorBestScore != 100 || !in.Metrics.EvaluatorAcceptedAfterResume {
		reasons = append(reasons, "typed evaluator did not accept a score-100 candidate after resume")
	}
	if !validDefaultStackProgression(in.Metrics.EvaluatorScoreProgression) {
		reasons = append(reasons, "evaluator scores are incomplete, decreasing, or do not end at 100")
	}
	if in.Variant == "baseline" && in.Metrics.StagnationNudgeEvents != 0 {
		reasons = append(reasons, "baseline unexpectedly recorded a stagnation nudge")
	}
	if in.Variant == "candidate" && in.Metrics.StagnationNudgeEvents > 1 {
		reasons = append(reasons, "candidate recorded more than the one bounded stagnation nudge")
	}
	if in.Metrics.Compactions < len(defaultStackMilestones)-1 || in.Metrics.Prompts < defaultStackPhaseCount {
		reasons = append(reasons, "long-horizon compaction/resume coverage is incomplete")
	}
	if err := requireOnlyDefaultStackFixture(in.Worktree); err != nil {
		reasons = append(reasons, err.Error())
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

type evaluatorOutput struct {
	Accepted              bool     `json:"accepted"`
	Score                 *float64 `json:"score,omitempty"`
	ScoreDirection        string   `json:"score_direction,omitempty"`
	Candidate             string   `json:"candidate,omitempty"`
	RemainingRequirements *int     `json:"remaining_requirements,omitempty"`
	EvidenceRef           string   `json:"evidence_ref,omitempty"`
	Reason                string   `json:"reason,omitempty"`
}

func writeEvaluatorJSON(w io.Writer, out evaluatorOutput) {
	_ = json.NewEncoder(w).Encode(out)
}

func stopCannotBlock(r io.Reader) bool {
	if r == nil {
		return false
	}
	data, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	if len(bytes.TrimSpace(data)) == 0 {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return false
	}
	canBlock, present := payload["can_block"].(bool)
	return present && !canBlock
}

func defaultStackControlMutation(paths []string) bool {
	controls := []string{
		filepath.ToSlash(filepath.Join(defaultStackFixture, defaultStackMilestoneFile)),
		filepath.ToSlash(filepath.Join(defaultStackFixture, defaultStackLegacyPhaseFile)),
	}
	for _, raw := range paths {
		clean := filepath.ToSlash(filepath.Clean(raw))
		for _, control := range controls {
			if clean == control || strings.HasSuffix(clean, "/"+control) {
				return true
			}
		}
	}
	return false
}

func validDefaultStackProgression(scores []float64) bool {
	if len(scores) != defaultStackPhaseCount || len(scores) == 0 || scores[len(scores)-1] != 100 {
		return false
	}
	for i := 1; i < len(scores); i++ {
		if scores[i] < scores[i-1] {
			return false
		}
	}
	return true
}

func requireOnlyDefaultStackFixture(worktree string) error {
	status, err := gitOutput(worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	prefix := "?? " + defaultStackFixture + "/"
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if line != "" && !strings.HasPrefix(line, prefix) {
			return fmt.Errorf("unexpected workspace change: %s", line)
		}
	}
	root := filepath.Join(worktree, defaultStackFixture)
	for path := range defaultStackInitialFiles {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("required project file %s is missing or not regular", path)
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, path)
			return fmt.Errorf("unexpected project symlink %s", filepath.ToSlash(rel))
		}
		return nil
	})
}
