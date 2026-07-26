package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

const (
	ripgrepDefaultMaxColumns  = "1024"
	ripgrepDefaultMaxFilesize = "10M"
)

type ripgrep struct {
	program    string
	background BackgroundJobStarter
}

func ripgrepProgram() (string, bool) {
	program, err := exec.LookPath("rg")
	if err != nil {
		return "", false
	}
	return program, true
}

func newRipgrep(bg BackgroundJobStarter) (ripgrep, bool) {
	program, ok := ripgrepProgram()
	if !ok {
		return ripgrep{}, false
	}
	return ripgrep{program: program, background: bg}, true
}

// RipgrepAvailable reports whether the optional rg tool can be registered from
// the current PATH.
func RipgrepAvailable() bool {
	_, ok := ripgrepProgram()
	return ok
}

func (ripgrep) Name() string { return "rg" }

func (ripgrep) Description() string {
	return "Run raw rg for broad repository discovery, combined patterns, filenames, counts, native flags, or background searches. Once a target is known and surrounding source is needed, use search_context instead of rg followed by read_file. Input is an object; args must be an array of strings, not a string."
}

func (r ripgrep) Schema() json.RawMessage {
	if r.background != nil {
		return json.RawMessage(searchCommandBackgroundSchema)
	}
	return json.RawMessage(searchCommandSchema)
}

func (ripgrep) ReadOnly(json.RawMessage) bool { return true }

func (r ripgrep) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if hasBackgroundFlag(input) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if r.background == nil {
			return "", fmt.Errorf("background manager is not initialized")
		}
		args, err := decodeSearchCommandArgs(input)
		if err != nil {
			return "", err
		}
		if err := validateRipgrepArgs(args.Args); err != nil {
			return "", err
		}
		desc := "rg"
		if len(args.Args) > 0 {
			desc = "rg " + strings.Join(args.Args, " ")
		}
		args.Args = guardRipgrepArgs(args.Args)
		prog := r.program
		resourceKey, err := DefaultBackgroundResource(args.Cwd)
		if err != nil {
			return "", err
		}
		info, err := r.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "rg",
			Description: desc,
			ResourceKey: resourceKey,
			Access:      BackgroundAccessReadOnly,
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				out, err := runProgram(ctx, prog, args, "rg", true)
				return BackgroundJobResult{Text: out}, err
			},
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(
			"background job %s started (resource: %s, access: %s)",
			info.ID,
			resourceKey,
			BackgroundAccessReadOnly,
		), nil
	}
	args, err := decodeSearchCommandArgs(input)
	if err != nil {
		return "", err
	}
	if err := validateRipgrepArgs(args.Args); err != nil {
		return "", err
	}
	args.Args = guardRipgrepArgs(args.Args)
	return runProgram(ctx, r.program, args, "rg", true)
}

func validateRipgrepArgs(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			return nil
		}
		if arg == "--replace" || strings.HasPrefix(arg, "--replace=") {
			continue
		}
		if arg == "-r" || isRipgrepShortReplaceArg(arg) {
			return fmt.Errorf("rg does not use grep-style -r for recursion; rg recurses by default. For replacement output, use --replace explicitly.")
		}
	}
	return nil
}

func isRipgrepShortReplaceArg(arg string) bool {
	if len(arg) < 3 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	return strings.Contains(arg[1:], "r")
}

func guardRipgrepArgs(args []string) []string {
	if ripgrepGuardBypass(args) {
		return args
	}

	haveColumns := false
	haveFilesize := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "-M" || strings.HasPrefix(arg, "-M") && len(arg) > len("-M") ||
			arg == "--max-columns" || strings.HasPrefix(arg, "--max-columns=") {
			haveColumns = true
		}
		if arg == "--max-filesize" || strings.HasPrefix(arg, "--max-filesize=") {
			haveFilesize = true
		}
	}

	prefix := make([]string, 0, 3)
	if !haveColumns {
		prefix = append(prefix, "--max-columns="+ripgrepDefaultMaxColumns, "--max-columns-preview")
	}
	if !haveFilesize {
		prefix = append(prefix, "--max-filesize="+ripgrepDefaultMaxFilesize)
	}
	if len(prefix) == 0 {
		return args
	}
	out := make([]string, 0, len(prefix)+len(args))
	out = append(out, prefix...)
	out = append(out, args...)
	return out
}

func ripgrepGuardBypass(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		switch arg {
		case "--help", "-h", "--version", "-V", "--files", "--type-list", "--json":
			return true
		}
	}
	return false
}
