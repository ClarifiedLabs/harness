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

	"harness/internal/llm"
)

const (
	shellDefaultTimeout           = 120
	shellBackgroundDefaultTimeout = 1200
	shellMaxSteps                 = 16
	shellFailureOutputBytes       = 4096
	shellFailureTailLines         = 40
	shellAutoReceiptBytes         = 8 * 1024
	shellSuccessTailBytes         = 4096
	shellSuccessTailLines         = 40
	shellReceiptLabelBytes        = 160
)

const (
	CommandMetricOutcomeAvailable = "command_outcome_available"
	CommandMetricSucceeded        = "command_succeeded"
	CommandMetricFailed           = "command_failed"
	CommandMetricCancelled        = "command_cancelled"
	CommandMetricTimedOut         = "command_timed_out"
	CommandMetricExitCode         = "command_exit_code"
	CommandMetricWaitComplete     = "command_wait_complete"
	CommandMetricStepsTotal       = "command_steps_total"
	CommandMetricStepsExecuted    = "command_steps_executed"
	CommandMetricStepsFailed      = "command_steps_failed"
	CommandMetricStepsCancelled   = "command_steps_cancelled"
	CommandMetricStepsTimedOut    = "command_steps_timed_out"
	CommandMetricStepsSkipped     = "command_steps_skipped"
)

// CommandOutcome is the structured process outcome carried in diagnostics
// metrics. Process failures intentionally remain ordinary tool results so the
// model can inspect their output; observers use this contract instead of
// guessing from model-visible text.
type CommandOutcome uint8

const (
	CommandOutcomeUnknown CommandOutcome = iota
	CommandOutcomePassed
	CommandOutcomeFailed
	CommandOutcomeNotRun
)

// CommandResultOutcome decodes shell's diagnostics-only outcome. It
// returns false for tools and legacy results that do not carry the contract.
func CommandResultOutcome(result llm.ToolResult) (CommandOutcome, bool) {
	metrics := result.Metrics
	if metrics[CommandMetricOutcomeAvailable] == 0 {
		return CommandOutcomeUnknown, false
	}
	if metrics[CommandMetricFailed] != 0 {
		return CommandOutcomeFailed, true
	}
	if metrics[CommandMetricCancelled] != 0 {
		return CommandOutcomeNotRun, true
	}
	if metrics[CommandMetricSucceeded] != 0 {
		return CommandOutcomePassed, true
	}
	return CommandOutcomeFailed, true
}

const (
	shellOutputAuto    = "auto"
	shellOutputReceipt = "receipt"
	shellOutputFull    = "full"
)

var (
	processTimeoutUnit = time.Second
	processReapGrace   = 500 * time.Millisecond
)

const shellSchemaFmt = `{
  "type": "object",
  "properties": {
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Direct exec with literal arguments; prefer over command."
    },
    "command": {"type": "string", "description": "Use only when shell syntax is needed."},
    "steps": {
      "type": "array",
      "minItems": 1,
      "maxItems": 16,
      "description": "Serial commands; excludes top-level argv/command/stdin.",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Default: step N."},
          "argv": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Direct exec with literal arguments."},
          "command": {"type": "string", "description": "Use only when shell syntax is needed."},
          "stdin": {"type": "string"},
          "cwd": {"type": "string"},
          "timeout_seconds": {"type": "integer"}
        }
      }
    },
    "stop_on_failure": {"type": "boolean", "description": "Default: true."},
    "name": {"type": "string"},
    "output_mode": {"type": "string", "enum": ["auto", "receipt", "full"], "description": "auto/receipt are compact; full returns complete step output."},
    "stdin": {"type": "string"},
    "cwd": {"type": "string", "description": "Default: process cwd."},
    "timeout_seconds": {"type": "integer", "description": "Default: %ds; no maximum."}
  }
}`

const shellBackgroundSchemaFmt = `{
  "type": "object",
  "properties": {
    "argv": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Direct exec with literal arguments; prefer over command."
    },
    "command": {"type": "string", "description": "Use only when shell syntax is needed."},
    "steps": {
      "type": "array",
      "minItems": 1,
      "maxItems": 16,
      "description": "Serial commands; excludes top-level argv/command/stdin.",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Default: step N."},
          "argv": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Direct exec with literal arguments."},
          "command": {"type": "string", "description": "Use only when shell syntax is needed."},
          "stdin": {"type": "string"},
          "cwd": {"type": "string"},
          "timeout_seconds": {"type": "integer"}
        }
      }
    },
    "stop_on_failure": {"type": "boolean", "description": "Default: true."},
    "name": {"type": "string"},
    "output_mode": {"type": "string", "enum": ["auto", "receipt", "full"], "description": "auto/receipt are compact; full returns complete step output."},
    "stdin": {"type": "string"},
    "cwd": {"type": "string", "description": "Default: process cwd."},
    "timeout_seconds": {"type": "integer", "description": "Background default: %ds; no maximum."},
    "background": {"type": "boolean", "description": "Returns a job ID; dependents should wait once via background_jobs."},
    "background_lease": {
      "type": "object",
      "description": "Scheduling only; does not restrict command behavior.",
      "properties": {
        "resource_key": {"type": "string", "description": "Default: canonical cwd."},
        "access": {"type": "string", "enum": ["read_only", "exclusive"], "description": "Default: exclusive; read_only only for non-mutating jobs."}
      },
      "additionalProperties": false
    }
  }
}`

type shell struct {
	background        BackgroundJobStarter
	foregroundTimeout int // seconds; 0 means use constant default
	backgroundTimeout int // seconds; 0 means use constant default
}

func (shell) Name() string { return "shell" }

func (shell) Description() string { return "Run a command or ordered steps; prefer argv." }

func (shell) PreserveSchemaDescriptions() bool { return true }

func (t shell) Schema() json.RawMessage {
	fg := t.foregroundTimeout
	if fg <= 0 {
		fg = shellDefaultTimeout
	}
	bg := t.backgroundTimeout
	if bg <= 0 {
		bg = shellBackgroundDefaultTimeout
	}
	if t.background != nil {
		return json.RawMessage(fmt.Sprintf(shellBackgroundSchemaFmt, bg))
	}
	return json.RawMessage(fmt.Sprintf(shellSchemaFmt, fg))
}

func (shell) ReadOnly(json.RawMessage) bool { return false }

// hasBackgroundFlag reports whether the tool input JSON contains
// "background": true, without decoding the rest of the tool-specific args.
func hasBackgroundFlag(input json.RawMessage) bool {
	var v struct {
		Background bool `json:"background"`
	}
	json.Unmarshal(input, &v)
	return v.Background
}

type shellArgs struct {
	Command         string      `json:"command"`
	Argv            []string    `json:"argv"`
	Steps           []shellStep `json:"steps"`
	StopOnFailure   *bool       `json:"stop_on_failure"`
	Name            string      `json:"name"`
	OutputMode      string      `json:"output_mode"`
	Stdin           string      `json:"stdin"`
	Cwd             string      `json:"cwd"`
	TimeoutSeconds  int         `json:"timeout_seconds"`
	Background      bool        `json:"background"`
	BackgroundLease *shellLease `json:"background_lease"`

	// Backward-compatible input aliases. They deliberately stay out of the
	// model-facing schema because top-level access was easy to misread as a
	// command-safety restriction rather than background scheduling metadata.
	ResourceKey string `json:"resource_key"`
	Access      string `json:"access"`
}

type shellLease struct {
	ResourceKey string `json:"resource_key"`
	Access      string `json:"access"`
}

type shellStep struct {
	Name           string   `json:"name"`
	Command        string   `json:"command"`
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin"`
	Cwd            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (t shell) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t shell) RunResult(ctx context.Context, input json.RawMessage) (RunResult, error) {
	args, err := decodeShellArgs(input)
	if err != nil {
		return RunResult{}, err
	}
	if err := validateShellArgs(args); err != nil {
		return RunResult{}, err
	}
	args.Name = strings.TrimSpace(args.Name)
	outputMode, err := normalizeShellOutputMode(args.OutputMode)
	if err != nil {
		return RunResult{}, err
	}
	args.OutputMode = outputMode
	if args.TimeoutSeconds < 0 {
		return RunResult{}, badArgs("timeout_seconds must be >= 0")
	}
	if err := validateCwd(args.Cwd); err != nil {
		return RunResult{}, err
	}
	if !args.Background && args.TimeoutSeconds == 0 && t.foregroundTimeout > 0 {
		args.TimeoutSeconds = t.foregroundTimeout
	}
	if len(args.Steps) > 0 && !args.Background {
		return shellSteps(ctx, args)
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
				args.TimeoutSeconds = shellBackgroundDefaultTimeout
			}
		}
		defaultResource, err := DefaultBackgroundResource(args.Cwd)
		if err != nil {
			return RunResult{}, err
		}
		resourceKey, access, err := ResolveBackgroundLease(
			args.ResourceKey,
			args.Access,
			defaultResource,
			BackgroundAccessExclusive,
		)
		if err != nil {
			return RunResult{}, err
		}
		info, err := t.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "shell",
			Description: shellDescription(args),
			ResourceKey: resourceKey,
			Access:      access,
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				var result RunResult
				var err error
				if len(args.Steps) > 0 {
					result, err = shellSteps(ctx, args)
				} else {
					result, err = shellTopLevel(ctx, args)
				}
				return BackgroundJobResult{
					Text:         result.Text,
					OriginalText: result.OriginalText,
					Metrics:      result.Metrics,
				}, err
			},
		})
		if err != nil {
			return RunResult{}, err
		}
		return RunResult{
			Text: fmt.Sprintf(
				"background job %s started (resource: %s, access: %s)",
				info.ID,
				resourceKey,
				access,
			),
			BackgroundJobID: info.ID,
		}, nil
	}

	return shellTopLevel(ctx, args)
}

func decodeShellArgs(input json.RawMessage) (shellArgs, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return shellArgs{}, err
	}
	var args shellArgs
	// Tolerant argv: model may send "[\"go\",\"test\"]" string or []string.
	if v, ok := raw["argv"]; ok && len(v) > 0 {
		var argv []string
		if err := json.Unmarshal(v, &argv); err == nil {
			args.Argv = argv
		} else {
			var s string
			if err2 := json.Unmarshal(v, &s); err2 == nil && strings.TrimSpace(s) != "" {
				var parsed []string
				if err3 := json.Unmarshal([]byte(s), &parsed); err3 == nil && len(parsed) > 0 {
					args.Argv = parsed
				} else {
					return shellArgs{}, badArgs("argv: expected an array of strings; got string — send argv as a JSON array, e.g. [\"go\",\"test\"]")
				}
			} else {
				return shellArgs{}, err
			}
		}
		delete(raw, "argv")
	}
	// Tolerant command: model may send ["git","status"] or "git status".
	if v, ok := raw["command"]; ok && len(v) > 0 {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			args.Command = s
		} else {
			var arr []string
			if err2 := json.Unmarshal(v, &arr); err2 == nil && len(arr) > 0 {
				args.Command = strings.Join(arr, " ")
			} else {
				return shellArgs{}, err
			}
		}
		delete(raw, "command")
	}
	// Decode remainder strictly so unknown top-level keys still fail.
	remainder, err := json.Marshal(raw)
	if err != nil {
		return shellArgs{}, err
	}
	if err := json.Unmarshal(remainder, &args); err != nil {
		return shellArgs{}, err
	}
	if args.BackgroundLease != nil {
		if strings.TrimSpace(args.ResourceKey) != "" || strings.TrimSpace(args.Access) != "" {
			return shellArgs{}, badArgs("provide background_lease or legacy top-level resource_key/access, not both")
		}
		args.ResourceKey = args.BackgroundLease.ResourceKey
		args.Access = args.BackgroundLease.Access
	}
	return args, nil
}

// SelfTimeout reports shell's own per-call deadline so its documented
// "no maximum" timeout_seconds is honored even under a shorter dispatch ceiling.
// Background jobs run outside Dispatch (it returns once the job is queued), so
// they report no deadline. See tools.SelfTimeouter.
func (t shell) SelfTimeout(input json.RawMessage) (time.Duration, bool) {
	var args shellArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return 0, false
	}
	if args.Background || args.TimeoutSeconds < 0 {
		return 0, false
	}
	if len(args.Steps) > 0 {
		defaultTimeout := args.TimeoutSeconds
		if defaultTimeout == 0 {
			defaultTimeout = shellDefaultTimeout
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
	if timeout == shellDefaultTimeout && t.foregroundTimeout > 0 {
		timeout = t.foregroundTimeout
	}
	return time.Duration(timeout) * processTimeoutUnit, true
}

func validateShellArgs(args shellArgs) error {
	hasCommand := strings.TrimSpace(args.Command) != ""
	hasArgv := len(args.Argv) > 0
	hasSteps := len(args.Steps) > 0
	hasLease := args.BackgroundLease != nil || strings.TrimSpace(args.ResourceKey) != "" || strings.TrimSpace(args.Access) != ""
	switch {
	case hasSteps && (hasCommand || hasArgv):
		return badArgs("provide steps or a top-level command/argv, not both")
	case hasSteps && args.Stdin != "":
		return badArgs("top-level stdin is unavailable with steps; set stdin on a step")
	case !args.Background && hasLease:
		return badArgs("background_lease requires background:true")
	case len(args.Steps) > shellMaxSteps:
		return badArgs("steps must contain at most %d items", shellMaxSteps)
	case hasCommand && hasArgv:
		return badArgs("provide command or argv, not both")
	case !hasCommand && !hasArgv && !hasSteps:
		return badArgs("command, argv, or steps is required")
	case hasArgv && strings.TrimSpace(args.Argv[0]) == "":
		return badArgs("argv[0] is required")
	}
	for i, step := range args.Steps {
		stepArgs := shellArgs{
			Command:        step.Command,
			Argv:           step.Argv,
			Stdin:          step.Stdin,
			Cwd:            step.Cwd,
			TimeoutSeconds: step.TimeoutSeconds,
		}
		if strings.TrimSpace(step.Name) == "" {
			step.Name = fmt.Sprintf("step %d", i+1)
		}
		if err := validateShellArgs(stepArgs); err != nil {
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

func shellDescription(args shellArgs) string {
	if len(args.Steps) > 0 {
		if name := strings.TrimSpace(args.Name); name != "" {
			return name
		}
		return fmt.Sprintf("%d shell steps", len(args.Steps))
	}
	if len(args.Argv) > 0 {
		return strings.Join(args.Argv, " ")
	}
	return args.Command
}

func shellTopLevel(ctx context.Context, args shellArgs) (RunResult, error) {
	started := time.Now()
	result, err := shellArgsProcess(ctx, args)
	if err != nil {
		return RunResult{}, err
	}
	return formatShellResult(args, result, conciseDuration(time.Since(started))), nil
}

func normalizeShellOutputMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return shellOutputAuto, nil
	}
	switch mode {
	case shellOutputAuto, shellOutputReceipt, shellOutputFull:
		return mode, nil
	default:
		return "", badArgs(`output_mode must be "auto", "receipt", or "full"`)
	}
}

func formatShellResult(args shellArgs, result processResult, elapsed string) RunResult {
	full := formatProcessResult(result)
	metrics := commandProcessMetrics(result)
	if args.OutputMode == shellOutputFull {
		return RunResult{Text: full, Metrics: metrics}
	}
	if result.success() {
		if args.OutputMode == shellOutputAuto && len(result.Output) <= shellAutoReceiptBytes {
			return RunResult{Text: full, Metrics: metrics}
		}
		text, clipped := formatShellReceipt(args, result, elapsed, shellSuccessTailBytes, shellSuccessTailLines)
		original := ""
		// Archive only when something was cut, or when the receipt drops the
		// partial-reap signal that formatProcessResult emits for an incomplete
		// wait (timeoutStatusLine); an unclipped, complete receipt is
		// self-contained and archiving it is pure noise.
		if clipped || !result.WaitComplete {
			original = full
		}
		return RunResult{Text: text, OriginalText: original, Metrics: metrics}
	}

	text, clipped := formatShellReceipt(args, result, elapsed, shellFailureOutputBytes, shellFailureTailLines)
	original := ""
	// As on the success path, keep the original when the receipt drops the
	// partial-reap signal (timeoutStatusLine) for an incomplete wait.
	if clipped || !result.WaitComplete {
		original = full
	}
	return RunResult{Text: text, OriginalText: original, Metrics: metrics}
}

func commandProcessMetrics(result processResult) map[string]int {
	metrics := map[string]int{
		CommandMetricOutcomeAvailable: 1,
		CommandMetricExitCode:         result.ExitCode,
	}
	if result.WaitComplete {
		metrics[CommandMetricWaitComplete] = 1
	}
	if result.success() {
		metrics[CommandMetricSucceeded] = 1
		return metrics
	}
	if result.Status == processCancelled {
		metrics[CommandMetricCancelled] = 1
		return metrics
	}
	metrics[CommandMetricFailed] = 1
	if result.Status == processTimedOut {
		metrics[CommandMetricTimedOut] = 1
	}
	return metrics
}

func formatShellReceipt(args shellArgs, result processResult, elapsed string, tailBytes, tailLines int) (string, bool) {
	status := "PASS"
	if !result.success() {
		status = "FAIL"
	}
	var b strings.Builder
	fmt.Fprintf(
		&b,
		"%s %s (%s; %s; %s output)",
		status,
		shellReceiptLabel(args),
		elapsed,
		result.receiptStatus(),
		HumanBytes(len(result.Output)),
	)
	if len(result.Output) == 0 {
		fmt.Fprintf(&b, "\n[exit code: %d]", result.ExitCode)
		return b.String(), false
	}
	tail, clipped := commandOutputTail(result.Output, tailBytes, tailLines)
	if strings.TrimSpace(tail) == "" {
		fmt.Fprintf(&b, "\n[exit code: %d]", result.ExitCode)
		return b.String(), clipped
	}
	if clipped {
		b.WriteString("\n[showing output tail]\n")
	} else {
		b.WriteString("\noutput:\n")
	}
	b.WriteString(strings.TrimRight(tail, "\n"))
	if clipped {
		fmt.Fprint(&b, "\n[full transcript archived — retry with sed -n or output_mode full if you need hidden lines]")
	}
	fmt.Fprintf(&b, "\n[exit code: %d]", result.ExitCode)
	return b.String(), clipped
}

func shellReceiptLabel(args shellArgs) string {
	label := strings.TrimSpace(args.Name)
	if label == "" {
		label = shellDescription(args)
	}
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		label = "command"
	}
	if len(label) <= shellReceiptLabelBytes {
		return label
	}
	clipped := label[:shellReceiptLabelBytes-len("...")]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return strings.TrimSpace(clipped) + "..."
}

func commandOutputTail(output string, maxBytes, maxLines int) (string, bool) {
	if output == "" {
		return "", false
	}
	start := 0
	clipped := false
	if maxBytes > 0 && len(output) > maxBytes {
		start = len(output) - maxBytes
		clipped = true
		for start < len(output) && !utf8.RuneStart(output[start]) {
			start++
		}
	}
	tail := output[start:]
	if maxLines > 0 {
		lineCount := strings.Count(tail, "\n")
		if !strings.HasSuffix(tail, "\n") {
			lineCount++
		}
		if lineCount > maxLines {
			skip := lineCount - maxLines
			index := 0
			for i := 0; i < skip; i++ {
				newline := strings.IndexByte(tail[index:], '\n')
				if newline < 0 {
					break
				}
				index += newline + 1
			}
			tail = tail[index:]
			clipped = true
		}
	}
	return tail, clipped
}

func shellArgsProcess(ctx context.Context, args shellArgs) (processResult, error) {
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

func shellSteps(ctx context.Context, args shellArgs) (RunResult, error) {
	stopOnFailure := args.StopOnFailure == nil || *args.StopOnFailure
	var receipt strings.Builder
	var transcript strings.Builder
	suppressed := false
	metrics := map[string]int{
		CommandMetricOutcomeAvailable: 1,
		CommandMetricStepsTotal:       len(args.Steps),
	}
	incompleteWait := false
	for i, step := range args.Steps {
		name := strings.TrimSpace(step.Name)
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		resolved := shellArgs{
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
		result, err := shellArgsProcess(ctx, resolved)
		metrics[CommandMetricStepsExecuted]++
		elapsed := conciseDuration(time.Since(started))

		if transcript.Len() > 0 {
			transcript.WriteString("\n\n")
		}
		fmt.Fprintf(&transcript, "==> %s <==\n$ %s\n", name, shellDescription(resolved))
		if err != nil {
			metrics[CommandMetricFailed] = 1
			metrics[CommandMetricStepsFailed]++
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
		if !result.WaitComplete {
			incompleteWait = true
		}
		if result.success() {
			fmt.Fprintf(&receipt, "PASS %s (%s)\n", name, elapsed)
			if strings.TrimSpace(result.Output) != "" {
				suppressed = true
			}
			continue
		}
		if result.Status == processCancelled {
			metrics[CommandMetricCancelled] = 1
			metrics[CommandMetricStepsCancelled]++
		} else {
			metrics[CommandMetricFailed] = 1
			metrics[CommandMetricStepsFailed]++
			if result.Status == processTimedOut {
				metrics[CommandMetricTimedOut] = 1
				metrics[CommandMetricStepsTimedOut]++
			}
		}

		fmt.Fprintf(&receipt, "FAIL %s (%s; %s)\n", name, elapsed, result.receiptStatus())
		if strings.TrimSpace(result.Output) != "" {
			excerpt, clipped := clipCommandOutput(result.Output, shellFailureOutputBytes)
			receipt.WriteString("output:\n")
			receipt.WriteString(strings.TrimRight(excerpt, "\n"))
			receipt.WriteByte('\n')
			if clipped {
				receipt.WriteString("[failure output clipped; inspect the archived full transcript]\n")
				suppressed = true
			}
		}
		if result.Status == processCancelled || stopOnFailure {
			writeSkippedReceipt(&receipt, len(args.Steps)-i-1)
			break
		}
	}
	metrics[CommandMetricStepsSkipped] = len(args.Steps) - metrics[CommandMetricStepsExecuted]
	if metrics[CommandMetricFailed] == 0 && metrics[CommandMetricCancelled] == 0 && metrics[CommandMetricStepsSkipped] == 0 {
		metrics[CommandMetricSucceeded] = 1
	}
	text := strings.TrimRight(receipt.String(), "\n")
	full := strings.TrimRight(transcript.String(), "\n")
	if name := strings.TrimSpace(args.Name); name != "" {
		text = fmt.Sprintf("Batch %s\n%s", name, text)
		full = fmt.Sprintf("== %s ==\n%s", name, full)
	}
	if args.OutputMode == shellOutputFull {
		return RunResult{Text: full, Metrics: metrics}, nil
	}
	original := ""
	if suppressed || incompleteWait {
		original = full
	}
	return RunResult{Text: text, OriginalText: original, Metrics: metrics}, nil
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
	if ctx.Err() != nil {
		return processResult{ExitCode: -1, Status: processCancelled, TimeoutSeconds: timeout, WaitComplete: true}, nil
	}

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
		return shellDefaultTimeout
	}
	return timeoutSeconds
}

// shellCommand builds the *exec.Cmd that runs line under the user's shell.
// Running an arbitrary shell command is shell's documented purpose
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
// without paying the login-shell cost on every shell call.
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
