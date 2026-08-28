package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"harness/internal/llm"
	"harness/internal/sessionrec"
	"harness/internal/tools"
)

// Concise shell result lines (design §10) project the interactive one-line shell
// summary a human actually reads: the name/command, success or failure with the
// exit code, step progress, and background-job identity. It is a live-only
// projection gated like ConciseReads; sessionrec.ToolResultLine stays the
// canonical detailed form used by recording, replay, and verbose display, so
// raw.ndjson and ordinary replay are unchanged.

const (
	// shellStepLabelLimit bounds one inline step label; step names and commands
	// are authored text and can be arbitrarily long.
	shellStepLabelLimit = 40
	// shellCommandLineLimit bounds the single top-level command/argv label.
	shellCommandLineLimit = 80
	// shellInlineStepLabels is how many step labels render inline before the
	// "+N more" continuation.
	shellInlineStepLabels = 3
)

// shellCallView is the display-only decode of a shell tool call's input JSON.
// Unknown keys are ignored; anything that fails to decode fails closed so the
// canonical detailed summary is retained.
type shellCallView struct {
	name       string
	hasName    bool
	command    string
	hasCommand bool
	steps      []shellStepView
	background bool

	leaseAccess   string
	leaseResource string
	hasLease      bool
}

type shellStepView struct {
	name    string
	command string
}

// decodeShellCallView decodes the model-authored shell input. ok is false when
// the JSON is malformed or the argument shapes cannot be understood, so the
// caller falls back to the detailed summary rather than guessing.
func decodeShellCallView(input json.RawMessage) (shellCallView, bool) {
	var raw struct {
		Name       string          `json:"name"`
		Argv       json.RawMessage `json:"argv"`
		Command    json.RawMessage `json:"command"`
		Steps      []shellRawStep  `json:"steps"`
		Background bool            `json:"background"`
		Lease      *struct {
			Access      string `json:"access"`
			ResourceKey string `json:"resource_key"`
		} `json:"background_lease"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return shellCallView{}, false
	}
	view := shellCallView{
		name:       strings.TrimSpace(raw.Name),
		hasName:    strings.TrimSpace(raw.Name) != "",
		background: raw.Background,
	}
	if view.hasLease = raw.Lease != nil; view.hasLease {
		view.leaseAccess = strings.TrimSpace(raw.Lease.Access)
		view.leaseResource = strings.TrimSpace(raw.Lease.ResourceKey)
	}
	if len(raw.Command) > 0 {
		var command string
		if err := json.Unmarshal(raw.Command, &command); err == nil {
			view.command = strings.TrimSpace(command)
			view.hasCommand = view.command != ""
		} else {
			// Tolerant command: an array of strings joins into one line.
			var argv []string
			if err := json.Unmarshal(raw.Command, &argv); err != nil || len(argv) == 0 {
				return shellCallView{}, false
			}
			view.command = strings.Join(argv, " ")
			view.hasCommand = true
		}
	}
	if len(raw.Argv) > 0 {
		var argv []string
		if err := json.Unmarshal(raw.Argv, &argv); err == nil {
			view.command = strings.Join(argv, " ")
			view.hasCommand = view.command != ""
		} else {
			// Tolerant argv: a JSON-encoded string holding the array.
			var encoded string
			if err := json.Unmarshal(raw.Argv, &encoded); err != nil {
				return shellCallView{}, false
			}
			var parsed []string
			if err := json.Unmarshal([]byte(encoded), &parsed); err != nil || len(parsed) == 0 {
				return shellCallView{}, false
			}
			view.command = strings.Join(parsed, " ")
			view.hasCommand = true
		}
	}
	if len(raw.Steps) > 0 {
		view.steps = make([]shellStepView, 0, len(raw.Steps))
		for _, step := range raw.Steps {
			label := strings.TrimSpace(step.Name)
			command := ""
			switch {
			case len(step.Argv) > 0:
				command = strings.Join(step.Argv, " ")
			case strings.TrimSpace(step.Command) != "":
				command = strings.TrimSpace(step.Command)
			}
			if label == "" {
				label = command
			}
			if label == "" && command == "" {
				return shellCallView{}, false
			}
			view.steps = append(view.steps, shellStepView{name: label, command: command})
		}
	}
	if !view.hasName && !view.hasCommand && len(view.steps) == 0 {
		return shellCallView{}, false
	}
	return view, true
}

type shellRawStep struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Argv    []string `json:"argv"`
}

// shellLabel renders the human label for a decoded shell call. Steps are
// prefixed with their authored count and their labels joined by "; " so
// multi-word commands and flag lists stay visually distinct per step. Names
// pass through shellClipLabel like commands: model-authored text can carry
// newlines or run long, and the projected line must stay single and bounded.
func shellLabel(view shellCallView) string {
	if len(view.steps) > 0 {
		labels := make([]string, 0, min(len(view.steps), shellInlineStepLabels)+1)
		for _, step := range view.steps[:min(len(view.steps), shellInlineStepLabels)] {
			labels = append(labels, shellClipLabel(step.label(), shellStepLabelLimit))
		}
		if rest := len(view.steps) - shellInlineStepLabels; rest > 0 {
			labels = append(labels, fmt.Sprintf("+%d more", rest))
		}
		joined := strings.Join(labels, "; ")
		if view.hasName {
			return fmt.Sprintf("steps=%d %s: %s", len(view.steps), shellClipLabel(view.name, shellCommandLineLimit), joined)
		}
		return fmt.Sprintf("steps=%d %s", len(view.steps), joined)
	}
	if view.hasName {
		return shellClipLabel(view.name, shellCommandLineLimit)
	}
	return shellClipLabel(view.command, shellCommandLineLimit)
}

func (s shellStepView) label() string {
	if s.name != "" {
		return s.name
	}
	return s.command
}

// shellClipLabel truncates a human label on a rune boundary. Unlike
// sessionrec.FormatScalar it never quotes: this is a command line, not a
// key=value value.
func shellClipLabel(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	clipped := s[:max-1]
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return strings.TrimSpace(clipped) + "…"
}

// shellStatusSegment renders the outcome segment from the diagnostics metrics
// shell attaches to every non-error result. ok is false for results that do
// not carry the command-outcome contract (foreign tools, or legacy results
// recorded before the contract), so callers keep the detailed form.
func shellStatusSegment(result llm.ToolResult) (string, bool) {
	outcome, ok := tools.CommandResultOutcome(result)
	if !ok {
		return "", false
	}
	switch outcome {
	case tools.CommandOutcomePassed:
		return "ok", true
	case tools.CommandOutcomeFailed:
		// Timeouts are classified as failures (the process was killed, not
		// skipped), so distinguish them within the branch.
		if result.Metrics[tools.CommandMetricTimedOut] != 0 {
			return "timed out", true
		}
		// Signal-killed and unclassified failures carry a -1 or 0 exit code;
		// a parenthesized "-1" is noise, so only positive codes get the segment.
		if exit := result.Metrics[tools.CommandMetricExitCode]; exit > 0 {
			return fmt.Sprintf("failed (exit %d)", exit), true
		}
		return "failed", true
	default: // CommandOutcomeNotRun: cancelled (or an unclassified skip)
		if result.Metrics[tools.CommandMetricTimedOut] != 0 {
			return "timed out", true
		}
		return "cancelled", true
	}
}

// shellProgressSegment renders partial-run progress for step batches. The count
// already lives in the label's steps=N prefix, so only incomplete runs need it.
func shellProgressSegment(result llm.ToolResult) string {
	skipped := result.Metrics[tools.CommandMetricStepsSkipped]
	if skipped <= 0 {
		return ""
	}
	executed := result.Metrics[tools.CommandMetricStepsExecuted]
	total := result.Metrics[tools.CommandMetricStepsTotal]
	return fmt.Sprintf("ran %d/%d, skipped %d", executed, total, skipped)
}

// conciseShellResultLine renders the live-only concise shell summary. concise
// is true when the projection applies; callers fall back to the canonical
// detailed line otherwise.
func (r *Renderer) conciseShellResultLine(call llm.ToolCall, result llm.ToolResult) (string, bool) {
	if !r.conciseShell || r.verbose || call.Name != "shell" || result.IsError {
		return "", false
	}
	view, ok := decodeShellCallView(call.Input)
	if !ok {
		return "", false
	}
	if result.BackgroundJobID != "" {
		// Launch receipts carry the outcome contract like every non-error
		// shell result; without it this is a foreign or legacy result, so
		// keep the detailed line rather than trusting the job identity alone.
		if _, ok := tools.CommandResultOutcome(result); !ok {
			return "", false
		}
		return fmt.Sprintf("[shell] %s → started", shellBackgroundSegment(view, result.BackgroundJobID, r.cwd)), true
	}
	status, haveStatus := shellStatusSegment(result)
	if !haveStatus {
		// Without the metrics contract there is no trustworthy outcome to
		// project; keep the canonical detailed form rather than a statusless
		// half-projection.
		return "", false
	}
	line := fmt.Sprintf("[shell] %s", shellLabel(view))
	if status != "" {
		line += " · " + status
	}
	if progress := shellProgressSegment(result); progress != "" {
		line += " · " + progress
	}
	return line + " → " + sessionrec.ResultSummary(result), true
}

// shellBackgroundSegment renders the launch line for a backgrounded shell call:
// the job identity plus its lease, with the resource key relativized against
// the session cwd when it lies under it. The result text is the manager's
// launch receipt rather than command output, so the caller replaces the normal
// size summary with "started".
func shellBackgroundSegment(view shellCallView, jobID, cwd string) string {
	if !view.hasLease || (view.leaseAccess == "" && view.leaseResource == "") {
		return fmt.Sprintf("%s · background job %s", shellLabel(view), jobID)
	}
	resource := shellClipLabel(sessionrec.DisplayPath(cwd, view.leaseResource), shellCommandLineLimit)
	switch {
	case view.leaseAccess != "" && resource != "":
		return fmt.Sprintf("%s · background job %s (%s @ %s)", shellLabel(view), jobID, view.leaseAccess, resource)
	case view.leaseAccess != "":
		return fmt.Sprintf("%s · background job %s (%s)", shellLabel(view), jobID, view.leaseAccess)
	default:
		return fmt.Sprintf("%s · background job %s (%s)", shellLabel(view), jobID, resource)
	}
}
