package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
      "enum": ["workspace_summary", "commit"],
      "description": "Run a structured workspace survey or explicit-path commit. Mutually exclusive with args."
    },
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "paths": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "maxItems": 100,
      "description": "Exact repository-relative file or directory paths for workflow commit; a directory stages and commits everything beneath it; '.', globs, and pathspec magic are rejected."
    },
    "message": {"type": "string", "description": "Conventional commit message for workflow commit."}
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
	return "Run git without a shell or pager. Input is an object: use workspace_summary for compact status or workflow commit with an explicit paths[] list and message to stage, check, commit, and report in one call; otherwise args must be an array of strings, not a string."
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
	case gi.Workflow == gitWorkflowCommit:
		return g.commitPaths(ctx, gi)
	case gi.Workflow != "":
		return "", badArgs("unknown workflow %q", gi.Workflow)
	case len(gi.Args) == 0:
		return "", badArgs("args or workflow is required")
	}

	if gitArgsReadOnly(gi.Args) {
		return runGitReadonlyArgs(ctx, g.program, gi)
	}
	return runGitArgs(ctx, g.program, gi)
}

// gitInput carries the decoded input for both git and git_readonly tools.
type gitInput struct {
	Args     []string `json:"args"`
	Workflow string   `json:"workflow"`
	Cwd      string   `json:"cwd"`
	Paths    []string `json:"paths"`
	Message  string   `json:"message"`
}

const gitWorkflowWorkspaceSummary = "workspace_summary"
const gitWorkflowCommit = "commit"

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
	if gi.Workflow != gitWorkflowCommit && (len(gi.Paths) > 0 || strings.TrimSpace(gi.Message) != "") {
		return gitInput{}, badArgs("paths and message require workflow commit")
	}
	return gi, nil
}

func gitArgsReadOnly(args []string) bool {
	return validateGitReadonlyArgs(args) == nil
}

var gitReadOnlyParallelSubcommands = gitReadonlySubcommands

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

func runGitReadonlyArgs(ctx context.Context, program string, input gitInput) (string, error) {
	if err := validateCwd(input.Cwd); err != nil {
		return "", err
	}
	cmd := buildGitReadonlyCommand(ctx, program, input.Args)
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

func (g gitTool) commitPaths(ctx context.Context, input gitInput) (string, error) {
	if err := validateCwd(input.Cwd); err != nil {
		return "", err
	}
	if strings.TrimSpace(input.Message) == "" {
		return "", badArgs("message is required for workflow commit")
	}
	if len(input.Paths) == 0 {
		return "", badArgs("paths is required for workflow commit")
	}
	if len(input.Paths) > 100 {
		return "", badArgs("paths must contain at most 100 items")
	}
	seen := map[string]bool{}
	for i, path := range input.Paths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		clean := filepath.ToSlash(filepath.Clean(path))
		switch {
		case path == "":
			return "", badArgs("paths[%d] must not be empty", i)
		case filepath.IsAbs(path):
			return "", badArgs("paths[%d] must be repository-relative", i)
		case clean != path || path == "." || path == ".." || strings.HasPrefix(path, "../") || strings.HasSuffix(path, "/"):
			return "", badArgs("paths[%d] must name an explicit file or directory, not %q", i, path)
		case strings.ContainsAny(path, "*?[") || strings.HasPrefix(path, ":("):
			return "", badArgs("paths[%d] must not contain pathspec magic or globs", i)
		case seen[path]:
			return "", badArgs("paths[%d] duplicates %q", i, path)
		}
		seen[path] = true
		input.Paths[i] = path
	}

	addArgs := append([]string{"add", "--"}, input.Paths...)
	add, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, addArgs...)
	if err != nil {
		return "", err
	}
	if !add.success() {
		return "", fmt.Errorf("git commit workflow add failed (%s): %s", add.receiptStatus(), strings.TrimSpace(add.Output))
	}

	checkArgs := append([]string{"diff", "--cached", "--check", "--"}, input.Paths...)
	check, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, checkArgs...)
	if err != nil {
		return "", err
	}
	if !check.success() {
		return "", fmt.Errorf("git commit workflow whitespace check failed (%s): %s", check.receiptStatus(), strings.TrimSpace(check.Output))
	}

	quietArgs := append([]string{"diff", "--cached", "--quiet", "--"}, input.Paths...)
	quiet, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, quietArgs...)
	if err != nil {
		return "", err
	}
	if quiet.ExitCode == 0 {
		return "", badArgs("no staged changes in the requested paths")
	}
	if quiet.ExitCode != 1 {
		return "", fmt.Errorf("git commit workflow diff failed (%s): %s", quiet.receiptStatus(), strings.TrimSpace(quiet.Output))
	}

	commitArgs := []string{"commit", "-m", input.Message, "--"}
	commitArgs = append(commitArgs, input.Paths...)
	commit, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, commitArgs...)
	if err != nil {
		return "", err
	}
	if !commit.success() {
		return "", fmt.Errorf("git commit workflow commit failed (%s): %s", commit.receiptStatus(), strings.TrimSpace(commit.Output))
	}

	head, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, "log", "-1", "--format=%h %s")
	if err != nil {
		return "", err
	}
	files, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, "show", "--format=", "--name-only", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := runGitWorkflowCommand(ctx, g.program, input.Cwd, "status", "--short")
	if err != nil {
		return "", err
	}
	for _, check := range []struct {
		label  string
		result processResult
	}{
		{label: "head", result: head},
		{label: "files", result: files},
		{label: "status", result: status},
	} {
		if !check.result.success() {
			return "", fmt.Errorf("git commit workflow %s failed (%s): %s", check.label, check.result.receiptStatus(), strings.TrimSpace(check.result.Output))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "committed: %s\n", strings.TrimSpace(head.Output))
	writeGitSummarySection(&b, "files", files.Output)
	if remaining := strings.TrimSpace(status.Output); remaining == "" {
		b.WriteString("workspace: clean\n")
	} else {
		writeGitSummarySection(&b, "remaining workspace", remaining)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func runGitWorkflowCommand(ctx context.Context, program, cwd string, args ...string) (processResult, error) {
	var cmd *exec.Cmd
	if gitArgsReadOnly(args) {
		cmd = buildGitReadonlyCommand(ctx, program, args)
	} else {
		cmd = buildGitCommand(ctx, program, args)
	}
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

// buildGitReadonlyCommand additionally suppresses optional index writes,
// filesystem-monitor hooks, external diff drivers, and text-conversion
// programs. The allowlist validator separately rejects flags that can write
// files or explicitly request an external helper.
func buildGitReadonlyCommand(ctx context.Context, program string, userArgs []string) *exec.Cmd {
	if program == "" {
		program = "git"
	}
	hardened := hardenReadonlyGitArgs(userArgs)
	argv := append([]string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false"}, hardened...)
	cmd := exec.CommandContext(ctx, program, argv...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

func hardenReadonlyGitArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	hardened := []string{args[0]}
	switch args[0] {
	case "diff", "diff-files", "diff-index", "diff-tree", "log", "show":
		hardened = append(hardened, "--no-ext-diff", "--no-textconv")
	case "grep":
		hardened = append(hardened, "--no-textconv")
	}
	return append(hardened, args[1:]...)
}
