package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

const gitReadonlySchema = `{
  "type": "object",
  "properties": {
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Arguments after \"git\"; the first must be an allowed restricted subcommand. Must be a JSON array of strings, e.g. [\"log\",\"--oneline\"], not a string or JSON-encoded array."
    },
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."}
  },
  "required": ["args"]
}`

// gitReadonlySubcommands is the git_readonly allowlist. bisect is included
// even though it checks out commits (moves HEAD): it is repository
// exploration, accepted by design.
var gitReadonlySubcommands = []string{"bisect", "blame", "diff", "grep", "log", "show", "status"}

type gitReadonly struct {
	program string
}

func newGitReadonly() (gitReadonly, bool) {
	program, ok := gitProgram()
	if !ok {
		return gitReadonly{}, false
	}
	return gitReadonly{program: program}, true
}

func (gitReadonly) Name() string { return "git_readonly" }

func (gitReadonly) Description() string {
	return `Run a restricted git command: status, log, diff, show, grep, blame, or bisect (bisect checks out commits; run/view/visualize are rejected). Provide a JSON object with args as an array of strings starting with the subcommand, e.g. {"args":["log","--oneline"]}; do not pass args as a string or JSON-encoded array. No shell; no pager.`
}

func (gitReadonly) Schema() json.RawMessage { return json.RawMessage(gitReadonlySchema) }

func (gitReadonly) ReadOnly(json.RawMessage) bool { return true }

func (g gitReadonly) Run(ctx context.Context, input json.RawMessage) (string, error) {
	gi, err := decodeGitArgs(input)
	if err != nil {
		return "", err
	}
	if len(gi.Args) == 0 {
		return "", badArgs("args is required and must be a non-empty array")
	}
	// The first argument must be a bare allowlisted subcommand. Global git
	// options (-c, -C, --git-dir, --exec-path, --paginate, ...) precede the
	// subcommand, so requiring a non-flag first argument blocks every global
	// option injection.
	sub := gi.Args[0]
	if strings.HasPrefix(sub, "-") || !slices.Contains(gitReadonlySubcommands, sub) {
		return "", badArgs("first argument must be one of: %s", strings.Join(gitReadonlySubcommands, ", "))
	}
	// bisect run/view/visualize execute a command or launch a viewer, escaping
	// the read-only boundary; the other bisect operations only move HEAD. The
	// operation is always the token right after "bisect".
	if sub == "bisect" && len(gi.Args) > 1 {
		switch gi.Args[1] {
		case "run", "view", "visualize":
			return "", badArgs("git_readonly does not allow %q (it runs commands or launches a viewer)", "bisect "+gi.Args[1])
		}
	}
	// A few subcommand-local flags still write files or launch programs even on
	// read-only subcommands; reject them so the boundary holds. Ordinary flags
	// pass through.
	for _, a := range gi.Args[1:] {
		if disallowedReadonlyFlag(a) {
			return "", badArgs("flag %q is not allowed in git_readonly", a)
		}
	}
	// git grep's -O/--open-files-in-pager opens matches in a pager (an arbitrary
	// program). -O can hide inside a clustered short-flag group, e.g. -inO<pager>,
	// so the long-form check above is not enough.
	if sub == "grep" {
		for _, a := range gi.Args[1:] {
			if shortFlagOpensPager(a) {
				return "", badArgs("flag %q opens a pager and is not allowed in git_readonly", a)
			}
		}
	}
	return runGitArgs(ctx, g.program, gi)
}

// disallowedReadonlyFlag reports whether a subcommand-local flag can write a
// file or launch a program in long form: --output/--output-directory (diff,
// log, show write to a file) and --open-files-in-pager (grep opens a pager).
// Clustered short-form -O is handled separately by shortFlagOpensPager.
func disallowedReadonlyFlag(arg string) bool {
	switch {
	case arg == "--output" || strings.HasPrefix(arg, "--output="):
		return true
	case arg == "--output-directory" || strings.HasPrefix(arg, "--output-directory="):
		return true
	case arg == "--open-files-in-pager" || strings.HasPrefix(arg, "--open-files-in-pager="):
		return true
	default:
		return false
	}
}

// shortFlagOpensPager reports whether arg is a git grep short-flag cluster
// containing -O (open-files-in-pager). It scans the cluster left to right and
// stops at the first value-taking short flag, whose remaining characters are a
// value rather than more flags — so an "O" inside e.g. -e<pattern> (a literal
// search for text containing O) is correctly not treated as the pager flag.
func shortFlagOpensPager(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	for _, c := range arg[1:] {
		switch c {
		case 'O':
			return true
		// git grep short flags that consume a value (attached or following):
		// after one of these the rest of the cluster is its value, not flags.
		case 'e', 'f', 'm', 'A', 'B', 'C':
			return false
		}
	}
	return false
}
