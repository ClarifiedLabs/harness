// Package hooks loads and runs command hooks around selected harness lifecycle
// events. It is deliberately provider- and agent-neutral: callers supply event
// payload fields as plain JSON-compatible values.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Event names the lifecycle point a hook group is attached to.
type Event string

const (
	SessionStart     Event = "SessionStart"
	UserPromptSubmit Event = "UserPromptSubmit"
	PreToolUse       Event = "PreToolUse"
	PostToolUse      Event = "PostToolUse"
	PreCompact       Event = "PreCompact"
	PostCompact      Event = "PostCompact"
	Stop             Event = "Stop"
)

var eventOrder = []Event{
	SessionStart,
	UserPromptSubmit,
	PreToolUse,
	PostToolUse,
	PreCompact,
	PostCompact,
	Stop,
}

var validEvents = func() map[Event]bool {
	m := make(map[Event]bool, len(eventOrder))
	for _, ev := range eventOrder {
		m[ev] = true
	}
	return m
}()

const (
	defaultTimeoutSeconds = 120
	maxTimeoutSeconds     = 600
)

var hookTimeoutUnit = time.Second

// Config is the fully decoded hook set. Groups are additive and preserve the
// order in which inline config and hook_configs files were loaded.
type Config struct {
	events map[Event][]Group
}

// Group is one matcher group under an event.
type Group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []Handler `json:"hooks"`

	matchAll bool
	matcher  *regexp.Regexp
}

// Handler is one command hook. Only type "command" is supported in v1.
type Handler struct {
	Type           string `json:"type"`
	Command        string `json:"command"`
	CommandWindows string `json:"command_windows,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	StatusMessage  string `json:"status_message,omitempty"`
}

// Payload carries event-specific fields. Runner adds common fields before
// sending JSON to hook commands.
type Payload map[string]any

// Result is the aggregate output from all matching command hooks.
type Result struct {
	Block             bool
	Reasons           []string
	AdditionalContext []string
	Notices           []string
}

// Reason returns all block reasons in deterministic execution order.
func (r Result) Reason() string { return joinNonEmpty(r.Reasons, "\n") }

// Context returns all additional context in deterministic execution order.
func (r Result) Context() string { return joinNonEmpty(r.AdditionalContext, "\n\n") }

// Empty reports whether no hooks are configured.
func (c Config) Empty() bool {
	for _, groups := range c.events {
		if len(groups) > 0 {
			return false
		}
	}
	return true
}

// Groups returns the configured groups for event.
func (c Config) Groups(event Event) []Group {
	if c.events == nil {
		return nil
	}
	return c.events[event]
}

// Append appends another config's groups to c.
func (c *Config) Append(other Config) {
	if other.Empty() {
		return
	}
	if c.events == nil {
		c.events = make(map[Event][]Group)
	}
	for _, ev := range eventOrder {
		if groups := other.events[ev]; len(groups) > 0 {
			c.events[ev] = append(c.events[ev], groups...)
		}
	}
}

// MarshalJSON renders only configured events, suitable for --show-config.
func (c Config) MarshalJSON() ([]byte, error) {
	out := make(map[string][]Group)
	for _, ev := range eventOrder {
		if groups := c.events[ev]; len(groups) > 0 {
			out[string(ev)] = groups
		}
	}
	if len(out) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes an event map. It exists so Config can be embedded in
// config.Config for show-config output and tests, but callers that need wrapper
// support should use DecodeFile.
func (c *Config) UnmarshalJSON(data []byte) error {
	cfg, err := DecodeEventMap(data)
	if err != nil {
		return err
	}
	*c = cfg
	return nil
}

// DecodeFile decodes a hook config file. Files may contain {"hooks": {...}} or
// a bare event map.
func DecodeFile(data []byte) (Config, error) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Config{}, err
	}
	if raw, ok := wrapper["hooks"]; ok {
		return DecodeEventMap(raw)
	}
	return DecodeEventMap(data)
}

// DecodeEventMap decodes the value of a top-level "hooks" object.
func DecodeEventMap(data []byte) (Config, error) {
	if len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return Config{}, nil
	}
	var raw map[string][]Group
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}
	cfg := Config{events: make(map[Event][]Group)}
	for name, groups := range raw {
		ev := Event(name)
		if !validEvents[ev] {
			return Config{}, fmt.Errorf("unknown hook event %q", name)
		}
		for i := range groups {
			if err := groups[i].validate(); err != nil {
				return Config{}, fmt.Errorf("%s[%d]: %w", ev, i, err)
			}
		}
		cfg.events[ev] = append(cfg.events[ev], groups...)
	}
	return cfg, nil
}

// LoadFile reads one hook config file, resolving relative paths against baseDir.
func LoadFile(baseDir, file string) (Config, error) {
	path := resolvePath(baseDir, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := DecodeFile(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// LoadFiles reads and appends every hook config file in order.
func LoadFiles(baseDir string, files []string) (Config, error) {
	var out Config
	for _, file := range files {
		if strings.TrimSpace(file) == "" {
			return Config{}, fmt.Errorf("hook_configs contains an empty path")
		}
		cfg, err := LoadFile(baseDir, file)
		if err != nil {
			return Config{}, err
		}
		out.Append(cfg)
	}
	return out, nil
}

func resolvePath(baseDir, file string) string {
	if filepath.IsAbs(file) || baseDir == "" {
		return file
	}
	return filepath.Join(baseDir, file)
}

func (g *Group) validate() error {
	matcher := strings.TrimSpace(g.Matcher)
	switch matcher {
	case "", "*":
		g.matchAll = true
	default:
		re, err := regexp.Compile(matcher)
		if err != nil {
			return fmt.Errorf("matcher: %w", err)
		}
		g.matcher = re
	}
	if len(g.Hooks) == 0 {
		return fmt.Errorf("hooks must contain at least one handler")
	}
	for i := range g.Hooks {
		if err := g.Hooks[i].validate(); err != nil {
			return fmt.Errorf("hooks[%d]: %w", i, err)
		}
	}
	return nil
}

func (g Group) matches(target string) bool {
	if g.matchAll {
		return true
	}
	if g.matcher == nil {
		return false
	}
	return g.matcher.MatchString(target)
}

func (h *Handler) validate() error {
	if h.Type == "" {
		h.Type = "command"
	}
	if h.Type != "command" {
		return fmt.Errorf("unsupported hook type %q", h.Type)
	}
	if strings.TrimSpace(h.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if h.TimeoutSeconds == 0 {
		h.TimeoutSeconds = defaultTimeoutSeconds
	}
	if h.TimeoutSeconds < 0 {
		return fmt.Errorf("timeout_seconds must be >= 0")
	}
	if h.TimeoutSeconds > maxTimeoutSeconds {
		return fmt.Errorf("timeout_seconds must be <= %d", maxTimeoutSeconds)
	}
	return nil
}

// UnmarshalJSON accepts harness snake_case fields and Codex camelCase aliases.
func (h *Handler) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type                  string `json:"type"`
		Command               string `json:"command"`
		CommandWindows        string `json:"command_windows"`
		CommandWindowsAlias   string `json:"commandWindows"`
		TimeoutSeconds        *int   `json:"timeout_seconds"`
		TimeoutAlias          *int   `json:"timeout"`
		StatusMessage         string `json:"status_message"`
		StatusMessageAlias    string `json:"statusMessage"`
		UnsupportedAsync      *bool  `json:"async"`
		UnsupportedPromptName string `json:"prompt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.Type = strings.TrimSpace(raw.Type)
	h.Command = raw.Command
	h.CommandWindows = raw.CommandWindows
	if h.CommandWindows == "" {
		h.CommandWindows = raw.CommandWindowsAlias
	}
	if raw.TimeoutSeconds != nil {
		h.TimeoutSeconds = *raw.TimeoutSeconds
	} else if raw.TimeoutAlias != nil {
		h.TimeoutSeconds = *raw.TimeoutAlias
	}
	h.StatusMessage = raw.StatusMessage
	if h.StatusMessage == "" {
		h.StatusMessage = raw.StatusMessageAlias
	}
	return nil
}

// Runner executes hooks for a resolved config.
type Runner struct {
	Config         Config
	CWD            string
	SessionID      string
	TranscriptPath string
	Model          string
}

// Empty reports whether the runner has no configured hooks.
func (r *Runner) Empty() bool {
	return r == nil || r.Config.Empty()
}

// SetSession updates session identifiers included in future hook payloads.
func (r *Runner) SetSession(path string) {
	if r == nil {
		return
	}
	r.SessionID = path
	r.TranscriptPath = path
}

// SetModel updates the model included in future hook payloads.
func (r *Runner) SetModel(model string) {
	if r != nil {
		r.Model = model
	}
}

// HasEvent reports whether event has any configured groups.
func (r *Runner) HasEvent(event Event) bool {
	return !r.Empty() && len(r.Config.Groups(event)) > 0
}

// Run executes every matching hook for event. For events whose matcher is not
// meaningful in v1, pass an empty target and all groups will run.
func (r *Runner) Run(ctx context.Context, event Event, target string, payload Payload) Result {
	var out Result
	if r.Empty() {
		return out
	}
	groups := r.Config.Groups(event)
	if len(groups) == 0 {
		return out
	}
	input, err := r.input(event, payload)
	if err != nil {
		out.Notices = append(out.Notices, fmt.Sprintf("[hook %s skipped: %v]", event, err))
		return out
	}
	stdin, err := json.Marshal(input)
	if err != nil {
		out.Notices = append(out.Notices, fmt.Sprintf("[hook %s skipped: %v]", event, err))
		return out
	}
	stdin = append(stdin, '\n')

	for _, group := range groups {
		if !eventIgnoresMatcher(event) && !group.matches(target) {
			continue
		}
		for _, hook := range group.Hooks {
			if hook.StatusMessage != "" {
				out.Notices = append(out.Notices, "[hook: "+hook.StatusMessage+"]")
			}
			cmdResult := runCommand(ctx, hook, r.CWD, stdin)
			parsed := parseCommandOutput(cmdResult)
			out.Block = out.Block || parsed.Block
			out.Reasons = append(out.Reasons, parsed.Reasons...)
			out.AdditionalContext = append(out.AdditionalContext, parsed.AdditionalContext...)
			out.Notices = append(out.Notices, parsed.Notices...)
		}
	}
	return out
}

func (r *Runner) input(event Event, payload Payload) (map[string]any, error) {
	m := map[string]any{
		"session_id":      r.SessionID,
		"transcript_path": r.TranscriptPath,
		"cwd":             r.CWD,
		"hook_event_name": string(event),
		"model":           r.Model,
		"permission_mode": "default",
	}
	for k, v := range payload {
		m[k] = v
	}
	return m, nil
}

func eventIgnoresMatcher(event Event) bool {
	return event == UserPromptSubmit || event == Stop
}

type commandResult struct {
	Stdout   string
	Stderr   string
	Code     int
	StartErr error
	TimedOut bool
	Canceled bool
}

func runCommand(ctx context.Context, hook Handler, cwd string, stdin []byte) commandResult {
	command := hook.Command
	if runtime.GOOS == "windows" && hook.CommandWindows != "" {
		command = hook.CommandWindows
	}
	cmd := shellCommand(command)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	timeout := time.Duration(hook.TimeoutSeconds) * hookTimeoutUnit
	if timeout <= 0 {
		timeout = time.Duration(defaultTimeoutSeconds) * hookTimeoutUnit
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return commandResult{StartErr: err, Code: -1}
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			killGroup(cmd.Process.Pid)
		case <-done:
		}
	}()
	waitErr := cmd.Wait()
	close(done)

	res := commandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Code:   exitCode(waitErr),
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.Code = -1
	} else if errors.Is(runCtx.Err(), context.Canceled) {
		res.Canceled = true
		res.Code = -1
	}
	return res
}

func shellCommand(line string) *exec.Cmd {
	if _, err := exec.LookPath("bash"); err == nil {
		return exec.Command("bash", "-lc", line) // nosemgrep: dangerous-exec-command
	}
	return exec.Command("sh", "-c", line) // nosemgrep: dangerous-exec-command
}

func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

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

type hookOutput struct {
	Decision           string `json:"decision"`
	Continue           *bool  `json:"continue"`
	Reason             string `json:"reason"`
	HookSpecificOutput struct {
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
	UpdatedInput any `json:"updatedInput"`
}

func parseCommandOutput(cmd commandResult) Result {
	var out Result
	if cmd.StartErr != nil {
		out.Notices = append(out.Notices, fmt.Sprintf("[hook failed to start: %v]", cmd.StartErr))
		return out
	}
	stdout := strings.TrimSpace(cmd.Stdout)
	stderr := strings.TrimSpace(cmd.Stderr)
	if cmd.TimedOut {
		out.Notices = append(out.Notices, "[hook timed out; continuing]")
		return out
	}
	if cmd.Canceled {
		out.Notices = append(out.Notices, "[hook cancelled]")
		return out
	}

	parsed, parsedJSON := parseJSONOutput(stdout)
	if parsedJSON {
		if parsed.UpdatedInput != nil {
			out.Notices = append(out.Notices, "[hook updatedInput ignored: unsupported in harness v1]")
		}
		if ctx := strings.TrimSpace(parsed.HookSpecificOutput.AdditionalContext); ctx != "" {
			out.AdditionalContext = append(out.AdditionalContext, ctx)
		}
		if strings.EqualFold(parsed.Decision, "block") || strings.EqualFold(parsed.Decision, "deny") ||
			(parsed.Continue != nil && !*parsed.Continue) {
			out.Block = true
			if reason := strings.TrimSpace(parsed.Reason); reason != "" {
				out.Reasons = append(out.Reasons, reason)
			}
		}
	} else if stdout != "" && cmd.Code == 0 {
		out.AdditionalContext = append(out.AdditionalContext, stdout)
	}

	if cmd.Code == 2 {
		out.Block = true
		if len(out.Reasons) == 0 {
			reason := joinNonEmpty([]string{stdout, stderr}, "\n")
			if reason == "" {
				reason = "hook command exited with code 2"
			}
			out.Reasons = append(out.Reasons, reason)
		}
		return out
	}
	if cmd.Code != 0 {
		msg := fmt.Sprintf("[hook exited with code %d; continuing", cmd.Code)
		if stderr != "" {
			msg += ": " + firstLine(stderr)
		}
		out.Notices = append(out.Notices, msg+"]")
	}
	return out
}

func parseJSONOutput(stdout string) (hookOutput, bool) {
	var out hookOutput
	if stdout == "" || !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		return out, false
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return hookOutput{}, false
	}
	return out, true
}

func joinNonEmpty(parts []string, sep string) string {
	var kept []string
	for _, part := range parts {
		if s := strings.TrimSpace(part); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, sep)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
