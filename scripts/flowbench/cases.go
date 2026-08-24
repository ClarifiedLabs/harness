package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const targetSHA = "8f76b0b0fb7751a8f7b067fa7f88e4df564f9560"

const todoBugOld = "if active > 1 {"
const todoBugNew = "if active < 1 {"

type benchmarkCase struct {
	Name                  string
	Prompt                string
	Setup                 func(string) error
	Score                 func(scoreInput) score
	PrimaryMetric         string
	MinimumReductionPct   float64
	TargetTokenSavingPct  float64
	SecondPrompt          string
	BetweenPrompts        func(string) error
	RestartBetweenPrompts bool
	RestartPhases         []benchmarkPhase
	Baseline              benchmarkVariant
	Candidate             benchmarkVariant
	HelperCommand         string
	Acceptance            acceptanceMode
	MaxTokenRegression    float64
	MaxTurnRegression     float64
	MinimumRunTokens      int
	RunTimeout            time.Duration
}

// benchmarkPhase is one prompt process in a restart-backed benchmark sequence.
// CompactAfter runs a prompt-free compaction control process before After and
// before the next prompt process resumes the same session.
type benchmarkPhase struct {
	Prompt       string
	CompactAfter bool
	After        func(string) error
}

type benchmarkVariant struct {
	Agent  string
	Config string
	Helper bool
	Args   []string
}

type acceptanceMode string

const (
	acceptanceEfficiency         acceptanceMode = ""
	acceptanceStagnation         acceptanceMode = "stagnation"
	acceptanceStagnationRecovery acceptanceMode = "stagnation_recovery"
	acceptanceLineage            acceptanceMode = "candidate_lineage"
)

const defaultBenchmarkConfig = "{}\n"

func (c benchmarkCase) variant(name string) benchmarkVariant {
	var variant benchmarkVariant
	switch name {
	case "baseline":
		variant = c.Baseline
	case "candidate":
		variant = c.Candidate
	}
	if strings.TrimSpace(variant.Agent) == "" {
		variant.Agent = "independent"
	}
	if strings.TrimSpace(variant.Config) == "" {
		variant.Config = defaultBenchmarkConfig
	} else if !strings.HasSuffix(variant.Config, "\n") {
		variant.Config += "\n"
	}
	return variant
}

func (c benchmarkCase) hasCustomVariants() bool {
	return c.Baseline.Agent != "" || c.Baseline.Config != "" || c.Baseline.Helper ||
		len(c.Baseline.Args) > 0 || c.Candidate.Agent != "" || c.Candidate.Config != "" ||
		c.Candidate.Helper || len(c.Candidate.Args) > 0
}

type scoreInput struct {
	Variant       string
	Stdout        string
	Worktree      string
	SessionDir    string
	GoCache       string
	FixtureBefore string
	FixtureAfter  string
	Metrics       metrics
}

type score struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons,omitempty"`
}

func evaluateCase(c benchmarkCase, in scoreInput) (metrics, score) {
	result := c.Score(in)
	return classifyEffectiveToolErrors(c.Name, in.Metrics, result), result
}

func evaluateArchivedCase(c benchmarkCase, in scoreInput, recorded score) (metrics, score) {
	if c.Name == "stagnation_detection" || c.Name == "stagnation_recovery" {
		return classifyEffectiveToolErrors(c.Name, in.Metrics, recorded), recorded
	}
	if c.Name == "candidate_lineage" {
		return classifyEffectiveToolErrors(c.Name, in.Metrics, recorded), recorded
	}
	if strings.HasPrefix(c.Name, "edit_") {
		return classifyEffectiveToolErrors(c.Name, in.Metrics, recorded), recorded
	}
	return evaluateCase(c, in)
}

func classifyEffectiveToolErrors(caseName string, m metrics, result score) metrics {
	m.EffectiveToolErrors = m.ToolErrors + m.NestedToolErrors
	m.EffectiveToolErrorsAvailable = true
	if caseName == "edit_drift_recovery" && result.Pass && m.TimelyRecoveredEditMisses > 0 {
		m.EffectiveToolErrors = max(0, m.EffectiveToolErrors-m.TimelyRecoveredEditMisses)
	}
	return m
}

func allCases() map[string]benchmarkCase {
	cases := []benchmarkCase{
		{
			Name: "search_context",
			Prompt: "Work directly; do not delegate or edit. Trace, end to end, how automatic live retention decides which tool-result bodies stay verbatim, " +
				"when older results are trimmed or archived, how compaction reuses that machinery, and how the model recovers omitted output. Cite exact files and symbols.",
			Setup:                setupClean,
			Score:                scoreSearchContext,
			PrimaryMetric:        "rg_to_read_transitions",
			MinimumReductionPct:  50,
			TargetTokenSavingPct: 15,
		},
		{
			Name: "command_steps",
			Prompt: "Work directly; do not delegate or commit. The current worktree contains a one-line bug in internal/todo that makes its existing tests fail. " +
				"Diagnose and fix only that bug. Before reporting, run gofmt on every changed Go file, go test ./internal/todo, go vet ./internal/todo, and go test ./.... Report each result.",
			Setup:                setupTodoBug,
			Score:                scoreCommandSteps,
			PrimaryMetric:        "command_to_command_transitions",
			MinimumReductionPct:  50,
			TargetTokenSavingPct: 10,
		},
		{
			Name: "todo_coissue",
			Prompt: "Work directly and perform a read-only investigation: do not delegate, call editing tools, or modify files. Diagnose whether " +
				"compact_tool_result_max_bytes consistently controls both compaction and live retention. Trace the setting from configuration to every use site, " +
				"identify any mismatch, and describe the exact fix, documentation, and verification that should be implemented. Keep an update_todos checklist without dedicating a turn to bookkeeping.",
			Setup:                setupClean,
			Score:                scoreTodoCoissue,
			PrimaryMetric:        "avoidable_todo_only_turns",
			MinimumReductionPct:  50,
			TargetTokenSavingPct: 8,
		},
		{
			Name: "git_workspace_summary",
			Prompt: "Work directly; do not delegate, edit, stage, or commit. Review the current uncommitted workspace. Report branch and HEAD, distinguish staged, unstaged, " +
				"and untracked paths, quantify staged and unstaged diff size, identify all whitespace errors, and inspect the patch enough to explain its functional risk and whether it is ready to commit.",
			Setup:                setupGitWorkspace,
			Score:                scoreGitWorkspace,
			PrimaryMetric:        "git_calls",
			MinimumReductionPct:  50,
			TargetTokenSavingPct: 8,
		},
		{
			Name: "background_wait",
			Prompt: "Work directly; do not delegate or edit. Start go test -race -count=1 ./... as a background shell. While it runs, compare AGENTS.md, README.md, " +
				"and docs/smoke.md and report whether their verification commands agree. Do not finish until the background test completes; report its exact exit status. Avoid repeated polling.",
			Setup:                setupClean,
			Score:                scoreBackgroundWait,
			PrimaryMetric:        "background_polls",
			MinimumReductionPct:  50,
			TargetTokenSavingPct: 8,
		},
		stagnationDetectionCase(),
		stagnationRecoveryCase(),
		candidateLineageCase(),
		defaultStackMarathonCase(),
		{
			Name: "edit_precision",
			Prompt: "Work directly; do not delegate or commit. Use the edit tool to make exactly the five requested replacements in .flowbench-tool-accuracy/edit-precision.txt: " +
				"alpha pending->ready, beta pending->blocked, gamma 1->2, delta off->on, and epsilon old->new. Preserve every other byte, including sentinel punctuation and trailing spaces. Re-read to verify.",
			Setup: setupEditPrecision, Score: scoreEditPrecision,
			PrimaryMetric: "tool_errors", MinimumReductionPct: 0,
		},
		{
			Name:           "edit_drift_recovery",
			Prompt:         "Work directly; do not delegate or edit yet. Read .flowbench-tool-accuracy/edit-drift.txt and plan an exact edit that changes enabled=false to enabled=true only in the service beta block. Finish after describing the planned edit.",
			SecondPrompt:   "The workspace may have changed since your plan. Now use the edit tool to make the requested beta-only change, re-reading as needed, and verify the exact result. Do not change anything else.",
			BetweenPrompts: applyEditDrift,
			Setup:          setupEditDrift, Score: scoreEditDrift,
			PrimaryMetric: "tool_errors", MinimumReductionPct: 0,
		},
		{
			Name:   "known_path_batching",
			Prompt: knownPathBatchingPrompt(),
			Setup:  setupKnownPathBatching, Score: scoreKnownPathBatching,
			PrimaryMetric: "tool_errors", MinimumReductionPct: 0,
		},
		{
			Name: "unknown_path_discovery",
			Prompt: "Work directly; do not delegate or edit. Files are somewhere under .flowbench-tool-accuracy/discovery, but their paths are unknown. " +
				"Use shell with rg --files or find to discover all matching paths before reading any file, then read the first and last discovered files together. Independent repository lookups may be issued together in one turn. Report Discover01 and Discover18.",
			Setup: setupUnknownPathDiscovery, Score: scoreUnknownPathDiscovery,
			PrimaryMetric: "tool_errors", MinimumReductionPct: 0,
		},
	}
	for _, count := range []int{2, 8, 18, 36, 72} {
		count := count
		cases = append(cases, benchmarkCase{
			Name:          fmt.Sprintf("read_scale_%03d", count),
			Prompt:        readScalePrompt(count),
			Setup:         func(dir string) error { return setupReadScale(dir, count) },
			Score:         func(in scoreInput) score { return scoreReadScale(in, count) },
			PrimaryMetric: "tool_errors", MinimumReductionPct: 0,
		})
	}
	out := make(map[string]benchmarkCase, len(cases))
	for _, c := range cases {
		out[c.Name] = c
	}
	return out
}

const toolAccuracyFixture = ".flowbench-tool-accuracy"

const editPrecisionBefore = "alpha: pending\nbeta: pending\ngamma: 1\ndelta: off\nepsilon: old\nsentinel: “keep—exactly”  \n"
const editPrecisionAfter = "alpha: ready\nbeta: blocked\ngamma: 2\ndelta: on\nepsilon: new\nsentinel: “keep—exactly”  \n"

func setupEditPrecision(dir string) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, toolAccuracyFixture)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "edit-precision.txt"), []byte(editPrecisionBefore), 0o644)
}

const editDriftBefore = "service alpha\nowner: platform\nenabled=false\n\nservice beta\nowner: platform\nenabled=false\n"
const editDriftChanged = "service alpha\nowner: platform\nenabled=false\n\nservice beta\nowner: runtime\nenabled=false\n"
const editDriftAfter = "service alpha\nowner: platform\nenabled=false\n\nservice beta\nowner: runtime\nenabled=true\n"

func setupEditDrift(dir string) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, toolAccuracyFixture)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "edit-drift.txt"), []byte(editDriftBefore), 0o644)
}

func applyEditDrift(dir string) error {
	path := filepath.Join(dir, toolAccuracyFixture, "edit-drift.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != editDriftBefore {
		return fmt.Errorf("phase one changed drift fixture")
	}
	return os.WriteFile(path, []byte(editDriftChanged), 0o644)
}

func knownPathBatchingPrompt() string {
	paths := contractFixturePaths("known", "contract-%02d.txt")
	return "Work directly; do not delegate or edit. Read these known paths efficiently: " + strings.Join(paths, ", ") + ". " +
		"Use argv-form rg calls through shell to search the .flowbench-tool-accuracy/known directory for the literal strings Widget( and State{ (escape the punctuation in the regular expressions) plus the regular expression Marker[0-9]+. Independent repository lookups may be issued together in one turn. " +
		"For the requested step outputs, use exactly one shell call with output_mode full and two steps, in order: argv [\"printf\", \"STEP_ALPHA\\n\"] then argv [\"printf\", \"STEP_BETA\\n\"]. Report Marker01, Marker18, and both step outputs."
}

func setupKnownPathBatching(dir string) error {
	return setupContractFiles(dir, "known", "contract-%02d.txt", "Marker%02d Widget( State{\n")
}

func setupUnknownPathDiscovery(dir string) error {
	return setupContractFiles(dir, "discovery", "shard-%02d-hidden.txt", "Discover%02d\n")
}

func readScalePrompt(count int) string {
	paths := readScaleFixturePaths(count)
	return fmt.Sprintf("Work directly; do not delegate or edit. Use read, not shell or cat, to read every one of these %d known files: %s. ", count, strings.Join(paths, ", ")) +
		"Read efficiently; independent reads may be coissued in one turn. Report " + strings.Join(readScaleMarkers(count), ", ") + "."
}

func setupReadScale(dir string, count int) error {
	return setupNumberedFiles(dir, readScaleSubdir(count), "item-%03d.txt", "Scale%03d\n", count)
}

func setupContractFiles(dir, subdir, nameFormat, bodyFormat string) error {
	return setupNumberedFiles(dir, subdir, nameFormat, bodyFormat, 18)
}

func setupNumberedFiles(dir, subdir, nameFormat, bodyFormat string, count int) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	root := filepath.Join(dir, toolAccuracyFixture, subdir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for i := 1; i <= count; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf(nameFormat, i)), []byte(fmt.Sprintf(bodyFormat, i)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func scoreEditPrecision(in scoreInput) score {
	var reasons []string
	data, err := os.ReadFile(filepath.Join(in.Worktree, toolAccuracyFixture, "edit-precision.txt"))
	if err != nil || string(data) != editPrecisionAfter {
		reasons = append(reasons, "edit precision fixture does not match the exact oracle")
	}
	if in.Metrics.ToolCalls["edit"] == 0 {
		reasons = append(reasons, "edit tool was not exercised")
	}
	if err := requireOnlyUntrackedFixture(in.Worktree, filepath.Join(toolAccuracyFixture, "edit-precision.txt")); err != nil {
		reasons = append(reasons, err.Error())
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func scoreEditDrift(in scoreInput) score {
	var reasons []string
	data, err := os.ReadFile(filepath.Join(in.Worktree, toolAccuracyFixture, "edit-drift.txt"))
	if err != nil || string(data) != editDriftAfter {
		reasons = append(reasons, "drift fixture does not match the exact oracle")
	}
	if in.Metrics.ToolCalls["edit"] == 0 {
		reasons = append(reasons, "edit tool was not exercised")
	}
	if !in.Metrics.ReadDriftAfterPhaseOne {
		reasons = append(reasons, "drifted path was not re-read in phase two")
	}
	if err := requireOnlyUntrackedFixture(in.Worktree, filepath.Join(toolAccuracyFixture, "edit-drift.txt")); err != nil {
		reasons = append(reasons, err.Error())
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func requireOnlyUntrackedFixture(worktree string, paths ...string) error {
	status, err := gitOutput(worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(paths))
	for _, path := range paths {
		want["?? "+filepath.ToSlash(path)] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if line == "" {
			continue
		}
		if !want[line] {
			return fmt.Errorf("unexpected workspace change: %s", line)
		}
		delete(want, line)
	}
	if len(want) != 0 {
		return fmt.Errorf("expected fixture path is missing from workspace status")
	}
	return nil
}

func scoreKnownPathBatching(in scoreInput) score {
	result := requireOutput(in.Stdout, "Marker01", "Marker18", "STEP_ALPHA", "STEP_BETA")
	if !coversFixturePaths(in.Metrics.SuccessfulReadPaths, contractFixturePaths("known", "contract-%02d.txt")) {
		result.Reasons = append(result.Reasons, "not all known paths were read successfully")
	}
	if in.Metrics.ExactKnownPathSearches < 3 {
		result.Reasons = append(result.Reasons, "the two literal searches and one regex search were not completed successfully in the known directory")
	}
	if in.Metrics.ExactKnownPathCommands != 1 {
		result.Reasons = append(result.Reasons, "the requested output steps were not one successful exact full-output argv batch")
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "known-path fixture was modified")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func scoreUnknownPathDiscovery(in scoreInput) score {
	result := requireOutput(in.Stdout, "Discover01", "Discover18")
	if !in.Metrics.DiscoveryBeforeRead || in.Metrics.ReadBeforeDiscovery {
		result.Reasons = append(result.Reasons, "paths were not successfully discovered before reads")
	}
	paths := contractFixturePaths("discovery", "shard-%02d-hidden.txt")
	if !coversFixturePaths(in.Metrics.SuccessfulReadPaths, []string{paths[0], paths[len(paths)-1]}) {
		result.Reasons = append(result.Reasons, "the first and last discovered paths were not read successfully")
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "discovery fixture was modified")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func scoreReadScale(in scoreInput, count int) score {
	result := requireOutput(in.Stdout, readScaleMarkers(count)...)
	if !coversFixturePaths(in.Metrics.SuccessfulDirectReadPaths, readScaleFixturePaths(count)) {
		result.Reasons = append(result.Reasons, "not all scale paths were read successfully through read")
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "read-scale fixture was modified")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func readScaleMarkers(count int) []string {
	markers := []string{"Scale001"}
	if middle := count / 2; middle > 1 {
		markers = append(markers, fmt.Sprintf("Scale%03d", middle))
	}
	if count > 1 {
		markers = append(markers, fmt.Sprintf("Scale%03d", count))
	}
	return markers
}

func readScaleSubdir(count int) string {
	return fmt.Sprintf("read-scale-%03d", count)
}

func readScaleFixturePaths(count int) []string {
	return numberedFixturePaths(readScaleSubdir(count), "item-%03d.txt", count)
}

func contractFixturePaths(subdir, nameFormat string) []string {
	return numberedFixturePaths(subdir, nameFormat, 18)
}

func numberedFixturePaths(subdir, nameFormat string, count int) []string {
	paths := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		paths = append(paths, fmt.Sprintf("%s/%s/%s", toolAccuracyFixture, subdir, fmt.Sprintf(nameFormat, i)))
	}
	return paths
}

func sameFixturePaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[string]int, len(got))
	for _, path := range got {
		counts[normalizeFixturePath(path)]++
	}
	for _, path := range want {
		if counts[path] != 1 {
			return false
		}
	}
	return true
}

func coversFixturePaths(got, want []string) bool {
	seen := make(map[string]bool, len(got))
	for _, path := range got {
		seen[normalizeFixturePath(path)] = true
	}
	for _, path := range want {
		if !seen[path] {
			return false
		}
	}
	return true
}

func setupClean(dir string) error {
	status, err := gitOutput(dir, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("target worktree is not clean before setup:\n%s", status)
	}
	return nil
}

func setupTodoBug(dir string) error {
	if err := setupClean(dir); err != nil {
		return err
	}
	return replaceOnce(filepath.Join(dir, "internal", "todo", "todo.go"), todoBugOld, todoBugNew)
}

func setupGitWorkspace(dir string) error {
	if err := setupTodoBug(dir); err != nil {
		return err
	}
	if _, err := gitOutput(dir, "add", "--", "internal/todo/todo.go"); err != nil {
		return err
	}
	readme := filepath.Join(dir, "README.md")
	data, err := os.ReadFile(readme)
	if err != nil {
		return err
	}
	data = append(data, []byte("\nFlowbench workspace note.  \n")...)
	if err := os.WriteFile(readme, data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "flowbench-note.txt"), []byte("untracked flowbench note\n"), 0o644)
}

func replaceOnce(path, old, new string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if bytes.Count(data, []byte(old)) != 1 {
		return fmt.Errorf("%s: expected exactly one %q", path, old)
	}
	return os.WriteFile(path, bytes.Replace(data, []byte(old), []byte(new), 1), 0o644)
}

func scoreSearchContext(in scoreInput) score {
	result := requireOutput(in.Stdout,
		"applyRetention",
		"keepBoundary",
		"trimToolResultBlock",
		"readOnlyResultIDsIn",
		"archive",
	)
	for _, alternatives := range [][]string{
		{"internal/agent/retention.go", "retention.go"},
		{"internal/agent/compact.go", "compact.go"},
	} {
		if !containsAnyFold(in.Stdout, alternatives...) {
			result.Reasons = append(result.Reasons, "output missing "+strings.Join(alternatives, " or "))
		}
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "model changed the prepared workspace")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func scoreCommandSteps(in scoreInput) score {
	var reasons []string
	status, err := gitOutput(in.Worktree, "status", "--porcelain=v1")
	if err != nil {
		reasons = append(reasons, err.Error())
	} else if strings.TrimSpace(status) != "" {
		reasons = append(reasons, "worktree is not clean after the fix")
	}
	for _, required := range []string{
		"gofmt",
		"go test ./internal/todo",
		"go vet ./internal/todo",
		"go test ./...",
	} {
		if !strings.Contains(in.Metrics.CommandText, required) {
			reasons = append(reasons, "model did not invoke "+required)
		}
	}
	if len(reasons) == 0 {
		for _, argv := range [][]string{
			{"test", "./internal/todo"},
			{"vet", "./internal/todo"},
			{"test", "./..."},
		} {
			cmd := exec.Command("go", argv...)
			cmd.Dir = in.Worktree
			cmd.Env = benchmarkEnv(in.GoCache)
			if out, err := cmd.CombinedOutput(); err != nil {
				reasons = append(reasons, fmt.Sprintf("post-run go %s failed: %v\n%s", strings.Join(argv, " "), err, out))
				break
			}
		}
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func scoreTodoCoissue(in scoreInput) score {
	result := requireOutput(in.Stdout,
		"compact_tool_result_max_bytes",
		"internal/config",
		"cmd/harness",
		"CompactToolResultMaxBytes",
		"toolResultMaxBytes",
		"retention.go",
	)
	lower := strings.ToLower(in.Stdout)
	if !strings.Contains(lower, "4096") && !strings.Contains(lower, "4 kib") && !strings.Contains(lower, "4kib") {
		result.Reasons = append(result.Reasons, "output missing the 4096-byte (4 KiB) default")
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "model changed the prepared workspace")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func scoreGitWorkspace(in scoreInput) score {
	result := requireOutput(in.Stdout,
		"internal/todo/todo.go",
		"README.md",
		"flowbench-note.txt",
		"staged",
		"unstaged",
		"untracked",
		"whitespace",
	)
	lower := strings.ToLower(in.Stdout)
	if !strings.Contains(lower, "validation") && !strings.Contains(lower, "in_progress") {
		result.Reasons = append(result.Reasons, "output did not explain the TODO validation risk")
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "model changed the prepared workspace")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func scoreBackgroundWait(in scoreInput) score {
	result := requireOutput(in.Stdout, "AGENTS.md", "README.md")
	if !containsAnyFold(in.Stdout, "docs/smoke.md", "smoke.md") {
		result.Reasons = append(result.Reasons, "output missing docs/smoke.md or smoke.md")
	}
	lower := strings.ToLower(in.Stdout)
	if (!strings.Contains(lower, "exit code") && !strings.Contains(lower, "exit status")) || !strings.Contains(lower, "0") {
		result.Reasons = append(result.Reasons, "output did not report successful exit code 0")
	}
	if in.FixtureBefore != in.FixtureAfter {
		result.Reasons = append(result.Reasons, "model changed the clean workspace")
	}
	if !in.Metrics.StartedRaceSuite {
		result.Reasons = append(result.Reasons, "model did not start the required race suite in the background")
	}
	result.Pass = len(result.Reasons) == 0
	return result
}

func requireOutput(out string, needles ...string) score {
	var reasons []string
	for _, needle := range needles {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(needle)) {
			reasons = append(reasons, "output missing "+needle)
		}
	}
	return score{Pass: len(reasons) == 0, Reasons: reasons}
}

func containsAnyFold(out string, alternatives ...string) bool {
	lower := strings.ToLower(out)
	for _, alternative := range alternatives {
		if strings.Contains(lower, strings.ToLower(alternative)) {
			return true
		}
	}
	return false
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}
