// Package sysprompt builds the system prompt: static agentic-coding instructions
// plus an environment-context block (cwd, os, date, git summary), with
// composition options for appending runtime sections or dropping the env block
// (design §8.5).
package sysprompt

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"harness/prompts"
)

// Options controls system-prompt composition (design §8.5 flags). StaticPrompt
// replaces the built-in static instructions when non-empty; NoEnv drops the env
// block entirely.
// UserAgentsMD and ProjectAgentsMD carry AGENTS.md contents discovered from the
// user's home and working directory; when non-empty they are appended after the
// env block, giving the model user-level and project-specific instructions
// automatically. SkillsCatalog is an optional section listing available agent
// skills for progressive disclosure. AgentPrompt carries the active agent's
// instructions and is appended as the final section, so an agent definition
// layers on top of every other customization.
type Options struct {
	StaticPrompt    string // override for the built-in static instructions (optional)
	NoEnv           bool   // drop the env-context block
	UserAgentsMD    string // contents of ~/.agents/AGENTS.md (optional)
	ProjectAgentsMD string // contents of AGENTS.md from the working directory (optional)
	SkillsCatalog   string // available skills catalog (optional, from skills discovery)
	AgentPrompt     string // agent instructions, appended as the final section (optional)
	Env             EnvOptions
}

// Build composes the full system prompt per design §8.5: static instructions,
// then a blank-line separator and the env block (unless NoEnv), then user,
// project, skills, and agent sections when present.
func Build(opts Options) string {
	instructions := prompts.System()
	if opts.StaticPrompt != "" {
		instructions = opts.StaticPrompt
	}

	parts := []string{instructions}
	if !opts.NoEnv {
		parts = append(parts, EnvContext(opts.Env))
	}
	if opts.UserAgentsMD != "" {
		parts = append(parts, opts.UserAgentsMD)
	}
	if opts.ProjectAgentsMD != "" {
		parts = append(parts, opts.ProjectAgentsMD)
	}
	if opts.SkillsCatalog != "" {
		parts = append(parts, opts.SkillsCatalog)
	}
	// The agent section comes last so agent instructions layer on top of
	// every other customization.
	if opts.AgentPrompt != "" {
		parts = append(parts, opts.AgentPrompt)
	}
	return strings.Join(parts, "\n\n")
}

// EnvOptions parameterizes the env block for testability: Dir is the working
// directory whose git status is summarized (default process cwd via ""), Now
// supplies the date (default time.Now).
type EnvOptions struct {
	Dir string
	Now func() time.Time
}

// EnvContext renders the compact environment-context block (design §8.5).
func EnvContext(opts EnvOptions) string {
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	var b strings.Builder
	b.WriteString("Environment:\n")
	fmt.Fprintf(&b, "cwd: %s\n", dir)
	fmt.Fprintf(&b, "os: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "date: %s\n", now().Format("2006-01-02"))
	fmt.Fprintf(&b, "git: %s", gitSummary(dir))
	return b.String()
}

// gitSummary returns the branch and modified/untracked counts, or
// "(not a git repository)" when dir is not in a work tree (design §8.5).
func gitSummary(dir string) string {
	branch, ok := gitBranch(dir)
	if !ok {
		return "(not a git repository)"
	}

	modified, untracked := gitStatusCounts(dir)
	if branch == "" {
		branch = "(detached)"
	}
	return fmt.Sprintf("branch=%s, %d modified, %d untracked", branch, modified, untracked)
}

// gitBranch runs `git branch --show-current`; ok is false when the command
// fails (no git, or not a repository).
func gitBranch(dir string) (string, bool) {
	out, err := runGit(dir, "branch", "--show-current")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// gitStatusCounts parses `git status --porcelain`: untracked lines start with
// "??"; everything else is a tracked file with staged and/or unstaged changes,
// counted as modified.
func gitStatusCounts(dir string) (modified, untracked int) {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		if strings.HasPrefix(line, "??") {
			untracked++
		} else {
			modified++
		}
	}
	return modified, untracked
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir, "--no-pager"}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}
