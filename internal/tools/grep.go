package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// grepMaxLineLen caps the width of a matched line in grep's output. Host grep
// has no portable --max-columns equivalent, so the clamp happens in-process: a
// match in a minified/JSON/lock line otherwise produces one enormous line the
// central line cap cannot help (it counts lines, not columns).
const grepMaxLineLen = 1024

type grep struct {
	background BackgroundJobStarter
	// preferRG steers the model toward rg when both are registered (search_tools=both).
	preferRG bool
}

func (grep) Name() string { return "grep" }

func (t grep) Description() string {
	desc := `Run the host grep command directly. Provide a JSON object with args as an array of strings, e.g. {"args":["-R","-n","TODO","."]}; do not pass args as a string or JSON-encoded array. No shell; binary files are skipped (-I) unless you set a binary policy or pass --; overlong matched lines are clamped in output. Returns combined stdout+stderr and the exit code, or returns a background job id immediately when background is true.`
	if t.preferRG {
		desc += " Prefer rg when available; it is registered alongside this tool and is the faster default."
	}
	return desc
}

func (t grep) Schema() json.RawMessage {
	if t.background != nil {
		return json.RawMessage(searchCommandBackgroundSchema)
	}
	return json.RawMessage(searchCommandSchema)
}

func (grep) ReadOnly(json.RawMessage) bool { return true }

func (t grep) Run(ctx context.Context, input json.RawMessage) (string, error) {
	args, err := decodeSearchCommandArgs(input)
	if err != nil {
		return "", err
	}
	// Validate before guarding: injecting -I would otherwise mask an empty-args call.
	if len(args.Args) == 0 {
		return "", badArgs("args is required and must be a non-empty array")
	}

	if hasBackgroundFlag(input) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if t.background == nil {
			return "", fmt.Errorf("background manager is not initialized")
		}
		desc := "grep"
		if len(args.Args) > 0 {
			desc = "grep " + strings.Join(args.Args, " ")
		}
		args.Args = guardGrepArgs(args.Args)
		info, err := t.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "grep",
			Description: desc,
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				out, err := runProgram(ctx, "grep", args, "grep", true)
				return BackgroundJobResult{Text: clampLongGrepLines(out)}, err
			},
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("background job %s started", info.ID), nil
	}

	args.Args = guardGrepArgs(args.Args)
	out, err := runProgram(ctx, "grep", args, "grep", true)
	if err != nil {
		return "", err
	}
	return clampLongGrepLines(out), nil
}

// guardGrepArgs prepends -I (skip binary files) unless the caller already set a
// binary policy (-I, -a/--text, --binary-files=...) or the args are a help/
// version invocation. The scan for a binary policy stops at "--" (everything
// after it is a pattern/path, not a flag); -I is still injected before "--".
func guardGrepArgs(args []string) []string {
	if grepGuardBypass(args) {
		return args
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if grepHasBinaryPolicy(arg) {
			return args
		}
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, "-I")
	return append(out, args...)
}

// grepGuardBypass reports invocations where injecting -I is pointless (help and
// version). The "--" terminator ends the scan: after it, tokens are operands.
func grepGuardBypass(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		switch arg {
		case "--help", "--version", "-V":
			return true
		}
	}
	return false
}

// grepHasBinaryPolicy reports whether arg already chooses how binary files are
// handled, so the -I guard must not override the caller's intent. Only exact
// tokens are matched; a clustered -a/-I is rare and at worst makes the guard a
// harmless no-op.
func grepHasBinaryPolicy(arg string) bool {
	switch arg {
	case "-I", "-a", "--text":
		return true
	}
	return arg == "--binary-files" || strings.HasPrefix(arg, "--binary-files=")
}

// clampLongGrepLines clamps every output line wider than grepMaxLineLen to that
// width plus a short marker, so a match inside a minified/JSON/lock line cannot
// flood the result. Short lines (including the "[exit code: N]" trailer) pass
// through unchanged.
func clampLongGrepLines(s string) string {
	if len(s) <= grepMaxLineLen && !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	clamped := false
	for i, ln := range lines {
		if len(ln) > grepMaxLineLen {
			lines[i] = clampGrepLine(ln, grepMaxLineLen)
			clamped = true
		}
	}
	if !clamped {
		return s
	}
	return strings.Join(lines, "\n")
}

func clampGrepLine(ln string, maxLen int) string {
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(ln[cut]) {
		cut--
	}
	omitted := len(ln) - cut
	return fmt.Sprintf("%s… [%d chars clamped]", ln[:cut], omitted)
}
