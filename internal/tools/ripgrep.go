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
	return `Run the host rg (ripgrep) command directly. Provide a JSON object with args as an array of strings, e.g. {"args":["-n","TODO","."]}; do not pass args as a string or JSON-encoded array. No shell; normal searches default to --max-columns=1024 --max-columns-preview --max-filesize=10M unless args set those native rg options. Returns combined stdout+stderr and the exit code, or returns a background job id immediately when background is true.`
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
		desc := "rg"
		if len(args.Args) > 0 {
			desc = "rg " + strings.Join(args.Args, " ")
		}
		args.Args = guardRipgrepArgs(args.Args)
		prog := r.program
		info, err := r.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "rg",
			Description: desc,
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				out, err := runProgram(ctx, prog, args, "rg", true)
				return BackgroundJobResult{Text: out}, err
			},
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("background job %s started", info.ID), nil
	}
	args, err := decodeSearchCommandArgs(input)
	if err != nil {
		return "", err
	}
	args.Args = guardRipgrepArgs(args.Args)
	return runProgram(ctx, r.program, args, "rg", true)
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
