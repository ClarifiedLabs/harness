package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

const gitSchema = `{
  "type": "object",
  "properties": {
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Arguments after \"git\". Must be a JSON array of strings, e.g. [\"status\",\"--porcelain\"], not a string or JSON-encoded array."
    },
    "workflow": {
      "type": "string",
      "enum": ["workspace_summary"],
      "description": "Run a compact read-only workspace survey. Mutually exclusive with args."
    },
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."}
  }
}`

type gitTool struct {
	program string
}

func gitProgram() (string, bool) {
	program, err := exec.LookPath("git")
	if err != nil {
		return "", false
	}
	return program, true
}

func newGitTool() (gitTool, bool) {
	program, ok := gitProgram()
	if !ok {
		return gitTool{}, false
	}
	return gitTool{program: program}, true
}

// GitAvailable reports whether the optional git-backed tools can be registered
// from the current PATH.
func GitAvailable() bool {
	_, ok := gitProgram()
	return ok
}

func (gitTool) Name() string { return "git" }

func (gitTool) Description() string {
	return "Run git without a shell or pager. Input is an object: use workflow workspace_summary for branch, status, diff sizes, and whitespace checks; otherwise args must be an array of strings, not a string."
}

func (gitTool) Schema() json.RawMessage { return json.RawMessage(gitSchema) }

func (gitTool) ReadOnly(input json.RawMessage) bool {
	gi, err := decodeGitArgs(input)
	if err != nil {
		return false
	}
	if gi.Workflow != "" {
		return gi.Workflow == gitWorkflowWorkspaceSummary && len(gi.Args) == 0
	}
	if len(gi.Args) == 0 {
		return false
	}
	return gitArgsReadOnly(gi.Args)
}

func (g gitTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	gi, err := decodeGitArgs(input)
	if err != nil {
		return "", err
	}
	switch {
	case gi.Workflow != "" && len(gi.Args) > 0:
		return "", badArgs("provide args or workflow, not both")
	case gi.Workflow == gitWorkflowWorkspaceSummary:
		return g.workspaceSummary(ctx, gi.Cwd)
	case gi.Workflow != "":
		return "", badArgs("unknown workflow %q", gi.Workflow)
	case len(gi.Args) == 0:
		return "", badArgs("args or workflow is required")
	}

	return runGitArgs(ctx, g.program, gi)
}

// gitInput carries the decoded input for both git and git_readonly tools.
type gitInput struct {
	Args     []string `json:"args"`
	Workflow string   `json:"workflow"`
	Cwd      string   `json:"cwd"`
}

const gitWorkflowWorkspaceSummary = "workspace_summary"

func decodeGitArgs(input json.RawMessage) (gitInput, error) {
	// Bare array still works for args-only calls.
	var bare []string
	if err := json.Unmarshal(input, &bare); err == nil && bare != nil {
		return gitInput{Args: bare}, nil
	}

	var gi gitInput
	if err := json.Unmarshal(input, &gi); err != nil {
		return gitInput{}, err
	}
	return gi, nil
}

func gitArgsReadOnly(args []string) bool {
	sub := args[0]
	if strings.HasPrefix(sub, "-") || !slices.Contains(gitReadOnlyParallelSubcommands, sub) {
		return false
	}
	for _, a := range args[1:] {
		if disallowedReadonlyFlag(a) {
			return false
		}
	}
	if sub == "grep" {
		for _, a := range args[1:] {
			if shortFlagOpensPager(a) {
				return false
			}
		}
	}
	return true
}

var gitReadOnlyParallelSubcommands = []string{"blame", "diff", "grep", "log", "show", "status"}

// runGitArgs executes git with userArgs and formats the combined output plus
// the exit-code marker; shared by git and git_readonly.
func runGitArgs(ctx context.Context, program string, input gitInput) (string, error) {
	if err := validateCwd(input.Cwd); err != nil {
		return "", err
	}
	cmd := buildGitCommand(ctx, program, input.Args)
	cmd.Dir = input.Cwd

	out, err := runProcess(ctx, cmd, 0)
	if err != nil {
		return "", fmt.Errorf("failed to run git: %w", err)
	}
	return out, nil
}

func (g gitTool) workspaceSummary(ctx context.Context, cwd string) (string, error) {
	if err := validateCwd(cwd); err != nil {
		return "", err
	}
	status, err := runGitWorkflowCommand(ctx, g.program, cwd, "status", "--porcelain=v1", "--branch")
	if err != nil {
		return "", err
	}
	if !status.success() {
		return "", fmt.Errorf("git workspace_summary status failed (%s): %s", status.receiptStatus(), strings.TrimSpace(status.Output))
	}

	head, err := runGitWorkflowCommand(ctx, g.program, cwd, "log", "-1", "--oneline")
	if err != nil {
		return "", err
	}
	unstagedStat, err := runGitWorkflowCommand(ctx, g.program, cwd, "diff", "--stat", "--")
	if err != nil {
		return "", err
	}
	stagedStat, err := runGitWorkflowCommand(ctx, g.program, cwd, "diff", "--cached", "--stat", "--")
	if err != nil {
		return "", err
	}
	unstagedCheck, err := runGitWorkflowCommand(ctx, g.program, cwd, "diff", "--check", "--")
	if err != nil {
		return "", err
	}
	stagedCheck, err := runGitWorkflowCommand(ctx, g.program, cwd, "diff", "--cached", "--check", "--")
	if err != nil {
		return "", err
	}
	for _, check := range []struct {
		name   string
		result processResult
	}{
		{name: "unstaged diffstat", result: unstagedStat},
		{name: "staged diffstat", result: stagedStat},
	} {
		if !check.result.success() {
			return "", fmt.Errorf("git workspace_summary %s failed (%s): %s", check.name, check.result.receiptStatus(), strings.TrimSpace(check.result.Output))
		}
	}

	var b strings.Builder
	b.WriteString("branch/status:\n")
	b.WriteString(strings.TrimSpace(status.Output))
	b.WriteByte('\n')
	b.WriteString("head:\n")
	switch {
	case head.success():
		b.WriteString(strings.TrimSpace(head.Output))
	case strings.Contains(status.Output, "No commits yet") || strings.Contains(status.Output, "Initial commit"):
		b.WriteString("(unborn; no commits yet)")
	default:
		fmt.Fprintf(&b, "(unavailable: %s", head.receiptStatus())
		if detail := strings.TrimSpace(head.Output); detail != "" {
			fmt.Fprintf(&b, ": %s", detail)
		}
		b.WriteByte(')')
	}
	b.WriteByte('\n')
	writeGitSummarySection(&b, "unstaged diffstat", unstagedStat.Output)
	writeGitSummarySection(&b, "staged diffstat", stagedStat.Output)

	unstagedWhitespace := strings.TrimSpace(unstagedCheck.Output)
	stagedWhitespace := strings.TrimSpace(stagedCheck.Output)
	if unstagedWhitespace == "" && stagedWhitespace == "" && unstagedCheck.success() && stagedCheck.success() {
		b.WriteString("whitespace: clean\n")
	} else {
		if unstagedWhitespace != "" {
			writeGitSummarySection(&b, "unstaged whitespace errors", unstagedWhitespace)
		} else if !unstagedCheck.success() {
			writeGitSummarySection(&b, "unstaged whitespace check", unstagedCheck.receiptStatus())
		}
		if stagedWhitespace != "" {
			writeGitSummarySection(&b, "staged whitespace errors", stagedWhitespace)
		} else if !stagedCheck.success() {
			writeGitSummarySection(&b, "staged whitespace check", stagedCheck.receiptStatus())
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func runGitWorkflowCommand(ctx context.Context, program, cwd string, args ...string) (processResult, error) {
	cmd := buildGitCommand(ctx, program, args)
	cmd.Dir = cwd
	result, err := runProcessDetailed(ctx, cmd, 0)
	if err != nil {
		return processResult{}, fmt.Errorf("failed to run git %s: %w", strings.Join(args, " "), err)
	}
	return result, nil
}

func writeGitSummarySection(b *strings.Builder, label, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	fmt.Fprintf(b, "%s:\n%s\n", label, text)
}

// buildGitCommand assembles the git invocation without running it: --no-pager
// is injected as the first argument (no interactive pager), and
// GIT_TERMINAL_PROMPT=0 is added to the inherited environment so credential
// prompts fail fast instead of hanging on a missing TTY (design §9.9). Exposing
// the *exec.Cmd is the env-inspection seam tests rely on.
func buildGitCommand(ctx context.Context, program string, userArgs []string) *exec.Cmd {
	if program == "" {
		program = "git"
	}
	argv := append([]string{"--no-pager"}, userArgs...)
	cmd := exec.CommandContext(ctx, program, argv...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}
