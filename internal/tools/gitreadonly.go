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

// gitReadonlySubcommands is an audited query-only allowlist. Commands with
// mixed read/write modes (for example branch, config, remote, reflog,
// submodule, tag, and worktree) stay out even when some invocations are safe.
var gitReadonlySubcommands = []string{
	"blame",
	"cat-file",
	"check-attr",
	"check-ignore",
	"check-mailmap",
	"check-ref-format",
	"cherry",
	"count-objects",
	"describe",
	"diff",
	"diff-files",
	"diff-index",
	"diff-tree",
	"for-each-ref",
	"grep",
	"log",
	"ls-files",
	"ls-tree",
	"merge-base",
	"name-rev",
	"range-diff",
	"rev-list",
	"rev-parse",
	"shortlog",
	"show",
	"show-branch",
	"show-ref",
	"status",
}

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
	return "Run read-only git queries (status, diff, log, rev-parse, merge-base, …) without a shell or pager; args is a string array."
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
	if err := validateGitReadonlyArgs(gi.Args); err != nil {
		return "", err
	}
	return runGitReadonlyArgs(ctx, g.program, gi)
}

func validateGitReadonlyArgs(args []string) error {
	if len(args) == 0 {
		return badArgs("args is required and must be a non-empty array")
	}
	// The first argument must be a bare allowlisted subcommand. Global git
	// options (-c, -C, --git-dir, --exec-path, --paginate, ...) precede the
	// subcommand, so requiring a non-flag first argument blocks every global
	// option injection.
	sub := args[0]
	if strings.HasPrefix(sub, "-") || !slices.Contains(gitReadonlySubcommands, sub) {
		return badArgs("first argument must be one of: %s", strings.Join(gitReadonlySubcommands, ", "))
	}
	// A few subcommand-local flags still write files or launch programs even on
	// query subcommands. Signature pretty formats can invoke a configured GPG
	// program, so inspect --format/--pretty values as well as direct flags.
	for i, arg := range args[1:] {
		if disallowedReadonlyFlag(arg) {
			return badArgs("flag %q is not allowed in git_readonly", arg)
		}
		if signatureFormatFlag(arg, args[1:], i) {
			return badArgs("signature format %q is not allowed in git_readonly", arg)
		}
	}
	// git grep's -O/--open-files-in-pager opens matches in a pager (an arbitrary
	// program). -O can hide inside a clustered short-flag group, e.g. -inO<pager>,
	// so the long-form check above is not enough.
	if sub == "grep" {
		for _, arg := range args[1:] {
			if shortFlagOpensPager(arg) {
				return badArgs("flag %q opens a pager and is not allowed in git_readonly", arg)
			}
		}
	}
	return nil
}

// disallowedReadonlyFlag reports whether a subcommand-local flag can write a
// file or launch a program in long form. Clustered grep -O is handled
// separately by shortFlagOpensPager.
func disallowedReadonlyFlag(arg string) bool {
	switch {
	case arg == "--output" || strings.HasPrefix(arg, "--output="):
		return true
	case arg == "--output-directory" || strings.HasPrefix(arg, "--output-directory="):
		return true
	case arg == "--open-files-in-pager" || strings.HasPrefix(arg, "--open-files-in-pager="):
		return true
	case arg == "--ext-diff" || strings.HasPrefix(arg, "--ext-diff="):
		return true
	case arg == "--textconv" || strings.HasPrefix(arg, "--textconv="):
		return true
	case arg == "--filters" || strings.HasPrefix(arg, "--filters="):
		return true
	case arg == "--show-signature" || strings.HasPrefix(arg, "--show-signature="):
		return true
	default:
		return false
	}
}

func signatureFormatFlag(arg string, args []string, index int) bool {
	for _, prefix := range []string{"--format=", "--pretty="} {
		if strings.HasPrefix(arg, prefix) {
			return strings.Contains(strings.TrimPrefix(arg, prefix), "%G")
		}
	}
	if (arg == "--format" || arg == "--pretty") && index+1 < len(args) {
		return strings.Contains(args[index+1], "%G")
	}
	return false
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
