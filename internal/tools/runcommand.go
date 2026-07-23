package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	runCommandDefaultTimeout           = 120
	runCommandBackgroundDefaultTimeout = 1200
	runCommandMaxSteps                 = 16
	runCommandFailureOutputBytes       = 4096
)

var (
	processTimeoutUnit = time.Second
	processReapGrace   = 500 * time.Millisecond
)

const runCommandSchemaFmt = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command line to execute."},
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Program and arguments to run directly without a shell. Must be a JSON array of strings, e.g. [\"go\",\"test\",\"./...\"], not a shell string or JSON-encoded array. argv[0] is resolved via PATH; remaining items are passed literally."
    },
    "steps": {
      "type": "array",
      "minItems": 1,
      "maxItems": 16,
      "description": "Ordered commands to run serially. Use for related build, format, lint, and test verification. Mutually exclusive with top-level command/argv/stdin and unavailable in background mode.",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Concise receipt label; defaults to step N."},
          "command": {"type": "string", "description": "Shell command line to execute."},
          "argv": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Program and literal arguments to run directly."},
          "stdin": {"type": "string", "description": "Written to this step's standard input."},
          "cwd": {"type": "string", "description": "Working directory override for this step."},
          "timeout_seconds": {"type": "integer", "description": "Timeout override for this step."}
        }
      }
    },
    "stop_on_failure": {"type": "boolean", "description": "Stop after the first non-zero, timed out, cancelled, or unstartable step (default true)."},
    "stdin": {"type": "string", "description": "Written to the command's standard input. Omit for no stdin."},
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "timeout_seconds": {"type": "integer", "description": "Kill the command after this many seconds (default %d; no maximum)."}
  }
}`

const runCommandBackgroundSchemaFmt = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command line to execute."},
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Program and arguments to run directly without a shell. Must be a JSON array of strings, e.g. [\"go\",\"test\",\"./...\"], not a shell string or JSON-encoded array. argv[0] is resolved via PATH; remaining items are passed literally."
    },
    "steps": {
      "type": "array",
      "minItems": 1,
      "maxItems": 16,
      "description": "Ordered foreground commands to run serially. Mutually exclusive with top-level command/argv/stdin and background:true.",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Concise receipt label; defaults to step N."},
          "command": {"type": "string", "description": "Shell command line to execute."},
          "argv": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Program and literal arguments to run directly."},
          "stdin": {"type": "string", "description": "Written to this step's standard input."},
          "cwd": {"type": "string", "description": "Working directory override for this step."},
          "timeout_seconds": {"type": "integer", "description": "Timeout override for this step."}
        }
      }
    },
    "stop_on_failure": {"type": "boolean", "description": "Stop after the first non-zero, timed out, cancelled, or unstartable step (default true)."},
    "stdin": {"type": "string", "description": "Written to the command's standard input. Omit for no stdin."},
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "timeout_seconds": {"type": "integer", "description": "Kill the command after this many seconds (default %d for background; no maximum)."},
    "background": {"type": "boolean", "description": "When true, start the command as a process-local background job and return a job id immediately. If later work depends on completion, call background_jobs action=wait once; do not poll get/list."}
  }
}`

type runCommand struct {
	background        BackgroundJobStarter
	foregroundTimeout int // seconds; 0 means use constant default
	backgroundTimeout int // seconds; 0 means use constant default
}

func (runCommand) Name() string { return "run_command" }

func (runCommand) Description() string {
	return "Run one command or ordered steps. Each uses shell command or argv as an array of strings; steps stop on failure and return compact receipts while archiving verbose output. Background supports one command."
}

func (t runCommand) Schema() json.RawMessage {
	fg := t.foregroundTimeout
	if fg <= 0 {
		fg = runCommandDefaultTimeout
	}
	bg := t.backgroundTimeout
	if bg <= 0 {
		bg = runCommandBackgroundDefaultTimeout
	}
	if t.background != nil {
		return json.RawMessage(fmt.Sprintf(runCommandBackgroundSchemaFmt, bg))
	}
	return json.RawMessage(fmt.Sprintf(runCommandSchemaFmt, fg))
}

func (runCommand) ReadOnly(json.RawMessage) bool { return false }

// hasBackgroundFlag reports whether the tool input JSON contains
// "background": true, without decoding the rest of the tool-specific args.
func hasBackgroundFlag(input json.RawMessage) bool {
	var v struct {
		Background bool `json:"background"`
	}
	json.Unmarshal(input, &v)
	return v.Background
}

type runCommandArgs struct {
	Command        string           `json:"command"`
	Argv           []string         `json:"argv"`
	Steps          []runCommandStep `json:"steps"`
	StopOnFailure  *bool            `json:"stop_on_failure"`
	Stdin          string           `json:"stdin"`
	Cwd            string           `json:"cwd"`
	TimeoutSeconds int              `json:"timeout_seconds"`
	Background     bool             `json:"background"`
}

type runCommandStep struct {
	Name           string   `json:"name"`
	Command        string   `json:"command"`
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin"`
	Cwd            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (t runCommand) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t runCommand) RunResult(ctx context.Context, input json.RawMessage) (RunResult, error) {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return RunResult{}, err
	}
	if err := validateRunCommandArgs(args); err != nil {
		return RunResult{}, err
	}
	if args.TimeoutSeconds < 0 {
		return RunResult{}, badArgs("timeout_seconds must be >= 0")
	}
	if err := validateCwd(args.Cwd); err != nil {
		return RunResult{}, err
	}
	if !args.Background && args.TimeoutSeconds == 0 && t.foregroundTimeout > 0 {
		args.TimeoutSeconds = t.foregroundTimeout
	}
	if len(args.Steps) > 0 {
		return runCommandSteps(ctx, args)
	}
	if args.Background {
		if err := ctx.Err(); err != nil {
			return RunResult{}, err
		}
		if t.background == nil {
			return RunResult{}, fmt.Errorf("background manager is not initialized")
		}
		if args.TimeoutSeconds == 0 {
			if t.backgroundTimeout > 0 {
				args.TimeoutSeconds = t.backgroundTimeout
			} else {
				args.TimeoutSeconds = runCommandBackgroundDefaultTimeout
			}
		}
		info, err := t.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "run_command",
			Description: runCommandDescription(args),
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				out, err := runCommandArgsCommand(ctx, args)
				return BackgroundJobResult{Text: out}, err
			},
		})
		if err != nil {
			return RunResult{}, err
		}
		return RunResult{Text: fmt.Sprintf("background job %s started", info.ID)}, nil
	}

	out, err := runCommandArgsCommand(ctx, args)
	return RunResult{Text: out}, err
}

// SelfTimeout reports run_command's own per-call deadline so its documented
// "no maximum" timeout_seconds is honored even under a shorter dispatch ceiling.
// Background jobs run outside Dispatch (it returns once the job is queued), so
// they report no deadline. See tools.SelfTimeouter.
func (t runCommand) SelfTimeout(input json.RawMessage) (time.Duration, bool) {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return 0, false
	}
	if args.Background || args.TimeoutSeconds < 0 {
		return 0, false
	}
	if len(args.Steps) > 0 {
		defaultTimeout := args.TimeoutSeconds
		if defaultTimeout == 0 {
			defaultTimeout = runCommandDefaultTimeout
			if t.foregroundTimeout > 0 {
				defaultTimeout = t.foregroundTimeout
			}
		}
		var total time.Duration
		for _, step := range args.Steps {
			if step.TimeoutSeconds < 0 {
				return 0, false
			}
			seconds := step.TimeoutSeconds
			if seconds == 0 {
				seconds = defaultTimeout
			}
			duration := time.Duration(seconds) * processTimeoutUnit
			if duration < 0 || total > time.Duration(1<<63-1)-duration {
				return time.Duration(1<<63 - 1), true
			}
			total += duration
		}
		return total, len(args.Steps) > 0
	}
	timeout := resolveProcessTimeoutSeconds(args.TimeoutSeconds)
	if timeout == runCommandDefaultTimeout && t.foregroundTimeout > 0 {
		timeout = t.foregroundTimeout
	}
	return time.Duration(timeout) * processTimeoutUnit, true
}

func validateRunCommandArgs(args runCommandArgs) error {
	hasCommand := strings.TrimSpace(args.Command) != ""
	hasArgv := len(args.Argv) > 0
	hasSteps := len(args.Steps) > 0
	switch {
	case hasSteps && (hasCommand || hasArgv):
		return badArgs("provide steps or a top-level command/argv, not both")
	case hasSteps && args.Stdin != "":
		return badArgs("top-level stdin is unavailable with steps; set stdin on a step")
	case hasSteps && args.Background:
		return badArgs("steps cannot run in the background")
	case len(args.Steps) > runCommandMaxSteps:
		return badArgs("steps must contain at most %d items", runCommandMaxSteps)
	case hasCommand && hasArgv:
		return badArgs("provide command or argv, not both")
	case !hasCommand && !hasArgv && !hasSteps:
		return badArgs("command, argv, or steps is required")
	case hasArgv && strings.TrimSpace(args.Argv[0]) == "":
		return badArgs("argv[0] is required")
	}
	for i, step := range args.Steps {
		stepArgs := runCommandArgs{
			Command:        step.Command,
			Argv:           step.Argv,
			Stdin:          step.Stdin,
			Cwd:            step.Cwd,
			TimeoutSeconds: step.TimeoutSeconds,
		}
		if strings.TrimSpace(step.Name) == "" {
			step.Name = fmt.Sprintf("step %d", i+1)
		}
		if err := validateRunCommandArgs(stepArgs); err != nil {
			return badArgs("steps[%d]: %v", i, err)
		}
		if step.TimeoutSeconds < 0 {
			return badArgs("steps[%d].timeout_seconds must be >= 0", i)
		}
		if err := validateCwd(step.Cwd); err != nil {
			return badArgs("steps[%d].cwd: %v", i, err)
		}
	}
	return nil
}

func runCommandDescription(args runCommandArgs) string {
	if len(args.Argv) > 0 {
		return strings.Join(args.Argv, " ")
	}
	return args.Command
}

func runCommandArgsCommand(ctx context.Context, args runCommandArgs) (string, error) {
	result, err := runCommandArgsProcess(ctx, args)
	if err != nil {
		return "", err
	}
	return formatProcessResult(result), nil
}

func runCommandArgsProcess(ctx context.Context, args runCommandArgs) (processResult, error) {
	if len(args.Argv) == 0 {
		cmd := shellCommand(args.Command)
		cmd.Dir = args.Cwd
		cmd.Env = shellEnv()
		if args.Stdin != "" {
			cmd.Stdin = strings.NewReader(args.Stdin)
		}
		result, err := runProcessDetailed(ctx, cmd, args.TimeoutSeconds)
		if err != nil {
			return processResult{}, fmt.Errorf("failed to start shell: %w", err)
		}
		return result, nil
	}
	cmd := exec.Command(args.Argv[0], args.Argv[1:]...) // nosemgrep: dangerous-exec-command
	cmd.Dir = args.Cwd
	if args.Stdin != "" {
		cmd.Stdin = strings.NewReader(args.Stdin)
	}
	result, err := runProcessDetailed(ctx, cmd, args.TimeoutSeconds)
	if err != nil {
		return processResult{}, fmt.Errorf("%s: %w", args.Argv[0], err)
	}
	return result, nil
}

func runCommandSteps(ctx context.Context, args runCommandArgs) (RunResult, error) {
	stopOnFailure := args.StopOnFailure == nil || *args.StopOnFailure
	var receipt strings.Builder
	var transcript strings.Builder
	suppressed := false
	for i, step := range args.Steps {
		name := strings.TrimSpace(step.Name)
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		resolved := runCommandArgs{
			Command:        step.Command,
			Argv:           append([]string(nil), step.Argv...),
			Stdin:          step.Stdin,
			Cwd:            step.Cwd,
			TimeoutSeconds: step.TimeoutSeconds,
		}
		if resolved.Cwd == "" {
			resolved.Cwd = args.Cwd
		}
		if resolved.TimeoutSeconds == 0 {
			resolved.TimeoutSeconds = args.TimeoutSeconds
		}
		started := time.Now()
		result, err := runCommandArgsProcess(ctx, resolved)
		elapsed := conciseDuration(time.Since(started))

		if transcript.Len() > 0 {
			transcript.WriteString("\n\n")
		}
		fmt.Fprintf(&transcript, "==> %s <==\n$ %s\n", name, runCommandDescription(resolved))
		if err != nil {
			fmt.Fprintf(&transcript, "failed to start: %v", err)
			fmt.Fprintf(&receipt, "FAIL %s (%s; failed to start: %v)\n", name, elapsed, err)
			if stopOnFailure {
				writeSkippedReceipt(&receipt, len(args.Steps)-i-1)
				break
			}
			continue
		}
		full := formatProcessResult(result)
		transcript.WriteString(full)
		if result.success() {
			fmt.Fprintf(&receipt, "PASS %s (%s)\n", name, elapsed)
			if strings.TrimSpace(result.Output) != "" {
				suppressed = true
			}
			continue
		}

		fmt.Fprintf(&receipt, "FAIL %s (%s; %s)\n", name, elapsed, result.receiptStatus())
		if strings.TrimSpace(result.Output) != "" {
			excerpt, clipped := clipCommandOutput(result.Output, runCommandFailureOutputBytes)
			receipt.WriteString("output:\n")
			receipt.WriteString(strings.TrimRight(excerpt, "\n"))
			receipt.WriteByte('\n')
			if clipped {
				receipt.WriteString("[failure output clipped; inspect the archived full transcript]\n")
				suppressed = true
			}
		}
		if stopOnFailure {
			writeSkippedReceipt(&receipt, len(args.Steps)-i-1)
			break
		}
	}
	text := strings.TrimRight(receipt.String(), "\n")
	original := ""
	if suppressed {
		original = strings.TrimRight(transcript.String(), "\n")
	}
	return RunResult{Text: text, OriginalText: original}, nil
}

func writeSkippedReceipt(b *strings.Builder, count int) {
	if count > 0 {
		fmt.Fprintf(b, "SKIP %d remaining step(s)\n", count)
	}
}

func conciseDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "0s"
	}
	return d.Round(time.Millisecond).String()
}

func clipCommandOutput(out string, limit int) (string, bool) {
	if len(out) <= limit {
		return out, false
	}
	clipped := out[:limit]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped, true
}

type programArgs struct {
	Args           []string
	Stdin          string
	Cwd            string
	TimeoutSeconds int
}

func decodeProgramArgs(input json.RawMessage, field string) (programArgs, error) {
	var bare []string
	if err := json.Unmarshal(input, &bare); err == nil && bare != nil {
		return programArgs{Args: bare}, nil
	}

	var raw struct {
		Args           []string `json:"args"`
		Argv           []string `json:"argv"`
		Stdin          string   `json:"stdin"`
		Cwd            string   `json:"cwd"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return programArgs{}, err
	}
	args := raw.Args
	if field == "argv" {
		args = raw.Argv
	}
	return programArgs{Args: args, Stdin: raw.Stdin, Cwd: raw.Cwd, TimeoutSeconds: raw.TimeoutSeconds}, nil
}

func runProgram(ctx context.Context, program string, args programArgs, displayName string, requireArgs bool) (string, error) {
	if requireArgs && len(args.Args) == 0 {
		return "", badArgs("args is required and must be a non-empty array")
	}
	if args.TimeoutSeconds < 0 {
		return "", badArgs("timeout_seconds must be >= 0")
	}
	if err := validateCwd(args.Cwd); err != nil {
		return "", err
	}

	cmd := exec.Command(program, args.Args...) // nosemgrep: dangerous-exec-command
	cmd.Dir = args.Cwd
	if args.Stdin != "" {
		cmd.Stdin = strings.NewReader(args.Stdin)
	}

	out, err := runProcess(ctx, cmd, args.TimeoutSeconds)
	if err != nil {
		return "", fmt.Errorf("%s: %w", displayName, err)
	}
	return out, nil
}

// validateCwd checks the optional cwd argument the exec-style tools share: an
// empty value is fine (inherit the process cwd); a non-empty value must name an
// existing directory.
func validateCwd(cwd string) error {
	if cwd == "" {
		return nil
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd %s is not a directory", cwd)
	}
	return nil
}

// runProcess starts cmd in its own process group/session, enforces the timeout
// (0 means the default; there is no maximum), and returns the combined output
// with the standard "[exit code: N]" trailer. A timeout or context cancellation
// kills the whole group — negative-pid signal reaps children — and is reported
// in-band, not as a tool error (design §9.7, §9.8). After the direct process
// exits, any remaining same-group descendants are also killed so foreground tool
// calls do not leak backgrounded children. The caller wires cmd.Dir/Stdin;
// runProcess owns process setup, combined output capture, the timeout context,
// the kill goroutine, and output formatting. A non-nil error means the process
// failed to start or its output could not be captured; callers wrap it with
// tool-specific context.
type processStatus uint8

const (
	processExited processStatus = iota
	processTimedOut
	processCancelled
)

type processResult struct {
	Output         string
	ExitCode       int
	Status         processStatus
	TimeoutSeconds int
	WaitComplete   bool
}

func (r processResult) success() bool {
	return r.Status == processExited && r.ExitCode == 0
}

func (r processResult) receiptStatus() string {
	switch r.Status {
	case processTimedOut:
		return fmt.Sprintf("timed out after %ds", r.TimeoutSeconds)
	case processCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("exit %d", r.ExitCode)
	}
}

func runProcess(ctx context.Context, cmd *exec.Cmd, timeoutSeconds int) (string, error) {
	result, err := runProcessDetailed(ctx, cmd, timeoutSeconds)
	if err != nil {
		return "", err
	}
	return formatProcessResult(result), nil
}

func runProcessDetailed(ctx context.Context, cmd *exec.Cmd, timeoutSeconds int) (processResult, error) {
	timeout := resolveProcessTimeoutSeconds(timeoutSeconds)

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*processTimeoutUnit)
	defer cancel()

	configureProcessGroup(cmd)

	outFile, err := os.CreateTemp("", "harness-tool-output-*")
	if err != nil {
		return processResult{}, err
	}
	defer os.Remove(outFile.Name())
	defer outFile.Close()
	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		return processResult{}, err
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	ctxErr := runCtx.Err()
	waitComplete := true
	select {
	case waitErr = <-waitDone:
		ctxErr = runCtx.Err()
	case <-runCtx.Done():
		ctxErr = runCtx.Err()
		killGroup(cmd.Process.Pid)
		select {
		case waitErr = <-waitDone:
		case <-time.After(processReapGrace):
			waitComplete = false
		}
	}
	killGroup(cmd.Process.Pid)

	out, err := readProcessOutput(outFile.Name())
	if err != nil {
		return processResult{}, err
	}

	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return processResult{Output: out, ExitCode: -1, Status: processTimedOut, TimeoutSeconds: timeout, WaitComplete: waitComplete}, nil
	} else if errors.Is(ctxErr, context.Canceled) {
		return processResult{Output: out, ExitCode: -1, Status: processCancelled, TimeoutSeconds: timeout, WaitComplete: waitComplete}, nil
	}

	return processResult{Output: out, ExitCode: exitCode(waitErr), Status: processExited, TimeoutSeconds: timeout, WaitComplete: true}, nil
}

func formatProcessResult(result processResult) string {
	switch result.Status {
	case processTimedOut:
		return result.Output + timeoutStatusLine("timed out", fmt.Sprintf("after %ds", result.TimeoutSeconds), result.WaitComplete)
	case processCancelled:
		return result.Output + timeoutStatusLine("cancelled", "", result.WaitComplete)
	default:
		return result.Output + fmt.Sprintf("[exit code: %d]", result.ExitCode)
	}
}

func readProcessOutput(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	out := string(data)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out += "\n"
	}
	return out, nil
}

func timeoutStatusLine(status, detail string, waitComplete bool) string {
	if detail != "" {
		status += " " + detail
	}
	if waitComplete {
		return fmt.Sprintf("[%s; process group killed]\n[exit code: -1]", status)
	}
	return fmt.Sprintf("[%s; process group kill signaled; wait did not finish]\n[exit code: -1]", status)
}

func resolveProcessTimeoutSeconds(timeoutSeconds int) int {
	if timeoutSeconds == 0 {
		return runCommandDefaultTimeout
	}
	return timeoutSeconds
}

// shellCommand builds the *exec.Cmd that runs line under the user's shell.
// Running an arbitrary shell command is run_command's documented purpose
// (design §2 no-sandbox stance, §9.7); the harness is assumed to be launched
// inside an already-sandboxed environment, so there is no command allowlist.
// The shell program name is a static literal in each branch; only the command
// line itself is user-supplied, which is intrinsic to this tool — hence the
// nosemgrep annotations.
//
// A non-login shell (-c, not -lc) is used: sourcing the full login-profile
// chain on every call added ~50-300ms (nvm/rbenv/conda) and could emit banner
// noise into the result. PATH enrichment that the login shell would have done is
// recovered once via shellEnv (see loginShellPATH).
func shellCommand(line string) *exec.Cmd {
	if _, err := exec.LookPath("bash"); err == nil {
		return exec.Command("bash", "-c", line) // nosemgrep: dangerous-exec-command
	}
	return exec.Command("sh", "-c", line) // nosemgrep: dangerous-exec-command
}

// shellEnv returns the process environment with PATH enriched by the
// once-resolved login-shell PATH, so dropping -lc above does not lose toolchain
// directories a login shell would have added (nvm/rbenv/etc.). The current PATH
// keeps precedence; login-only directories are appended.
func shellEnv() []string {
	login := loginShellPATH()
	env := os.Environ()
	if login == "" {
		return env
	}
	current := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			current = v
			break
		}
	}
	return setEnvPATH(env, mergePATH(current, login))
}

// setEnvPATH returns env with its PATH entry replaced by path (appended if
// absent), without mutating the input slice.
func setEnvPATH(env []string, path string) []string {
	out := make([]string, len(env))
	copy(out, env)
	for i, kv := range out {
		if strings.HasPrefix(kv, "PATH=") {
			out[i] = "PATH=" + path
			return out
		}
	}
	return append(out, "PATH="+path)
}

// mergePATH appends login PATH entries not already present in current,
// preserving current's order and precedence.
func mergePATH(current, login string) string {
	switch {
	case login == "":
		return current
	case current == "":
		return login
	}
	seen := make(map[string]bool)
	var parts []string
	add := func(list string) {
		for _, p := range filepath.SplitList(list) {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			parts = append(parts, p)
		}
	}
	add(current)
	add(login)
	return strings.Join(parts, string(filepath.ListSeparator))
}

// loginPATHSentinel brackets the printed PATH so login-profile stdout noise
// before it is discarded when parsing.
const loginPATHSentinel = "__harness_login_path__:"

// loginPATHResolver is a package var so tests can substitute a deterministic
// value instead of spawning a real login shell.
var loginPATHResolver = resolveLoginShellPATH

var (
	loginPATHOnce   sync.Once
	loginPATHCached string
)

// loginShellPATH returns the PATH a login shell exports, resolved at most once
// per process. This recovers the toolchain PATH the dropped -lc would have set
// without paying the login-shell cost on every run_command call.
func loginShellPATH() string {
	loginPATHOnce.Do(func() { loginPATHCached = loginPATHResolver() })
	return loginPATHCached
}

// resolveLoginShellPATH spawns one login shell to print its PATH. On any error
// it returns "" so shellEnv falls back to the inherited environment unchanged.
func resolveLoginShellPATH() string {
	bash, err := exec.LookPath("bash")
	if err != nil {
		return ""
	}
	cmd := exec.Command(bash, "-lc", "printf '%s%s' '"+loginPATHSentinel+"' \"$PATH\"") // nosemgrep: dangerous-exec-command
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseLoginPATHOutput(string(out))
}

// parseLoginPATHOutput extracts the PATH that follows the sentinel, tolerating
// any banner text a login profile printed before it.
func parseLoginPATHOutput(out string) string {
	if i := strings.LastIndex(out, loginPATHSentinel); i >= 0 {
		return strings.TrimSpace(out[i+len(loginPATHSentinel):])
	}
	return ""
}

// killGroup sends SIGKILL to the entire process group led by pid. Setpgid made
// the child a group leader, so its pgid equals its pid; the negative target
// signals every process in the group.
func killGroup(pid int) {
	killProcessGroup(pid)
}

var killProcessGroup = func(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// exitCode extracts a process exit code from cmd.Wait's error: 0 on success, the
// process's own code on a normal non-zero exit, or -1 when it was signalled or
// failed for another reason.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
