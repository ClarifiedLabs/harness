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
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."}
  },
  "required": ["args"]
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
	return `Run a git command. Provide a JSON object with args as an array of strings, e.g. {"args":["status","--porcelain"]}; do not pass args as a string or JSON-encoded array. No shell; no pager.`
}

func (gitTool) Schema() json.RawMessage { return json.RawMessage(gitSchema) }

func (gitTool) ReadOnly(input json.RawMessage) bool {
	gi, err := decodeGitArgs(input)
	if err != nil || len(gi.Args) == 0 {
		return false
	}
	return gitArgsReadOnly(gi.Args)
}

func (g gitTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	gi, err := decodeGitArgs(input)
	if err != nil {
		return "", err
	}
	if len(gi.Args) == 0 {
		return "", badArgs("args is required and must be a non-empty array")
	}

	return runGitArgs(ctx, g.program, gi)
}

// gitInput carries the decoded input for both git and git_readonly tools.
type gitInput struct {
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
}

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
