package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const runCommandDefaultTimeout = 120

var (
	processTimeoutUnit = time.Second
	processReapGrace   = 500 * time.Millisecond
)

const runCommandSchema = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command line to execute."},
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Program and arguments to run directly without a shell. Must be a JSON array of strings, e.g. [\"go\",\"test\",\"./...\"], not a shell string or JSON-encoded array. argv[0] is resolved via PATH; remaining items are passed literally."
    },
    "stdin": {"type": "string", "description": "Written to the command's standard input. Omit for no stdin."},
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "timeout_seconds": {"type": "integer", "description": "Kill the command after this many seconds (default 120; no maximum)."}
  }
}`

const runCommandBackgroundSchema = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command line to execute."},
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Program and arguments to run directly without a shell. Must be a JSON array of strings, e.g. [\"go\",\"test\",\"./...\"], not a shell string or JSON-encoded array. argv[0] is resolved via PATH; remaining items are passed literally."
    },
    "stdin": {"type": "string", "description": "Written to the command's standard input. Omit for no stdin."},
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "timeout_seconds": {"type": "integer", "description": "Kill the command after this many seconds (default 120; no maximum)."},
    "background": {"type": "boolean", "description": "When true, start the command as a process-local background job and return a job id immediately. Use background_jobs to inspect or cancel it."}
  }
}`

type runCommand struct {
	background BackgroundJobStarter
}

func (runCommand) Name() string { return "run_command" }

func (runCommand) Description() string {
	return "Run a shell command with command or a program directly with argv. Provide exactly one of command or argv. When using argv, pass it as an array of strings, not a shell string or JSON-encoded array. Returns combined stdout+stderr and exit code, or a background job id when background is true."
}

func (t runCommand) Schema() json.RawMessage {
	if t.background != nil {
		return json.RawMessage(runCommandBackgroundSchema)
	}
	return json.RawMessage(runCommandSchema)
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
	Command        string   `json:"command"`
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin"`
	Cwd            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Background     bool     `json:"background"`
}

func (t runCommand) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if err := validateRunCommandArgs(args); err != nil {
		return "", err
	}
	if args.TimeoutSeconds < 0 {
		return "", badArgs("timeout_seconds must be >= 0")
	}
	if err := validateCwd(args.Cwd); err != nil {
		return "", err
	}
	if args.Background {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if t.background == nil {
			return "", fmt.Errorf("background manager is not initialized")
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
			return "", err
		}
		return fmt.Sprintf("background job %s started", info.ID), nil
	}

	return runCommandArgsCommand(ctx, args)
}

func validateRunCommandArgs(args runCommandArgs) error {
	hasCommand := strings.TrimSpace(args.Command) != ""
	hasArgv := len(args.Argv) > 0
	switch {
	case hasCommand && hasArgv:
		return badArgs("provide command or argv, not both")
	case !hasCommand && !hasArgv:
		return badArgs("command or argv is required")
	case hasArgv && strings.TrimSpace(args.Argv[0]) == "":
		return badArgs("argv[0] is required")
	default:
		return nil
	}
}

func runCommandDescription(args runCommandArgs) string {
	if len(args.Argv) > 0 {
		return strings.Join(args.Argv, " ")
	}
	return args.Command
}

func runCommandArgsCommand(ctx context.Context, args runCommandArgs) (string, error) {
	if len(args.Argv) == 0 {
		return runShellCommand(ctx, args)
	}
	programArgs := programArgs{
		Args:           append([]string(nil), args.Argv[1:]...),
		Stdin:          args.Stdin,
		Cwd:            args.Cwd,
		TimeoutSeconds: args.TimeoutSeconds,
	}
	return runProgram(ctx, args.Argv[0], programArgs, args.Argv[0], false)
}

func runShellCommand(ctx context.Context, args runCommandArgs) (string, error) {
	cmd := shellCommand(args.Command)
	cmd.Dir = args.Cwd
	if args.Stdin != "" {
		cmd.Stdin = strings.NewReader(args.Stdin)
	}

	out, err := runProcess(ctx, cmd, args.TimeoutSeconds)
	if err != nil {
		return "", fmt.Errorf("failed to start shell: %w", err)
	}
	return out, nil
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
func runProcess(ctx context.Context, cmd *exec.Cmd, timeoutSeconds int) (string, error) {
	timeout := resolveProcessTimeoutSeconds(timeoutSeconds)

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*processTimeoutUnit)
	defer cancel()

	configureProcessGroup(cmd)

	outFile, err := os.CreateTemp("", "harness-tool-output-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(outFile.Name())
	defer outFile.Close()
	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		return "", err
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
		return "", err
	}

	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return out + timeoutStatusLine("timed out", fmt.Sprintf("after %ds", timeout), waitComplete), nil
	} else if errors.Is(ctxErr, context.Canceled) {
		return out + timeoutStatusLine("cancelled", "", waitComplete), nil
	}

	return out + fmt.Sprintf("[exit code: %d]", exitCode(waitErr)), nil
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
func shellCommand(line string) *exec.Cmd {
	if _, err := exec.LookPath("bash"); err == nil {
		// -l makes the login shell pick up the user's PATH/toolchain.
		return exec.Command("bash", "-lc", line) // nosemgrep: dangerous-exec-command
	}
	return exec.Command("sh", "-c", line) // nosemgrep: dangerous-exec-command
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
