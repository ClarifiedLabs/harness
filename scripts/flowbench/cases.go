package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const targetSHA = "8f76b0b0fb7751a8f7b067fa7f88e4df564f9560"

const todoBugOld = "if allCompleted(items) {"
const todoBugNew = "if !allCompleted(items) {"

type benchmarkCase struct {
	Name                 string
	Prompt               string
	Setup                func(string) error
	Score                func(scoreInput) score
	PrimaryMetric        string
	MinimumReductionPct  float64
	TargetTokenSavingPct float64
}

type scoreInput struct {
	Stdout        string
	Worktree      string
	GoCache       string
	FixtureBefore string
	FixtureAfter  string
	Metrics       metrics
}

type score struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons,omitempty"`
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
				"identify any mismatch, and describe the exact fix, documentation, and verification that should be implemented. Use update_todos to track the investigation.",
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
			Prompt: "Work directly; do not delegate or edit. Start go test -race -count=1 ./... as a background run_command. While it runs, compare AGENTS.md, README.md, " +
				"and docs/smoke.md and report whether their verification commands agree. Do not finish until the background test completes; report its exact exit status. Avoid repeated polling.",
			Setup:                setupClean,
			Score:                scoreBackgroundWait,
			PrimaryMetric:        "background_polls",
			MinimumReductionPct:  50,
			TargetTokenSavingPct: 8,
		},
	}
	out := make(map[string]benchmarkCase, len(cases))
	for _, c := range cases {
		out[c.Name] = c
	}
	return out
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
	if !strings.Contains(lower, "completion") && !strings.Contains(lower, "all todos") {
		result.Reasons = append(result.Reasons, "output did not explain the todo completion-cue risk")
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
