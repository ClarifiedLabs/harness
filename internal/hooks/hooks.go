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
	"sync"
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
	defaultTimeoutSeconds      = 120
	maxTimeoutSeconds          = 600
	defaultMaxTimeouts         = 3
	maxMaxTimeouts             = 100
	defaultCooldownSeconds     = 60
	maxTimeoutCooldownSeconds  = 3600
	maxConsecutiveTimeoutCount = 1_000_000
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
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	Command                string `json:"command"`
	CommandWindows         string `json:"command_windows,omitempty"`
	TimeoutSeconds         int    `json:"timeout_seconds,omitempty"`
	StatusMessage          string `json:"status_message,omitempty"`
	MaxConsecutiveTimeouts *int   `json:"max_consecutive_timeouts,omitempty"`
	TimeoutCooldownSeconds int    `json:"timeout_cooldown_seconds,omitempty"`
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
	Diagnostics       []Diagnostic
}

// DiagnosticOutcome is a bounded hook execution outcome.
type DiagnosticOutcome string

const (
	OutcomeSuccess     DiagnosticOutcome = "success"
	OutcomeTimeout     DiagnosticOutcome = "timeout"
	OutcomeCircuitOpen DiagnosticOutcome = "circuit_open"
	OutcomeCanceled    DiagnosticOutcome = "canceled"
	OutcomeStartFailed DiagnosticOutcome = "start_failed"
	OutcomeExitNonzero DiagnosticOutcome = "exit_nonzero"
	OutcomeParseFailed DiagnosticOutcome = "parse_failed"
)

// Diagnostic contains bounded operational metadata only. It never includes the
// command, input payload, stdout, or stderr.
type Diagnostic struct {
	Event               Event
	Handler             string
	Target              string
	ToolID              string
	TimeoutSeconds      int
	Elapsed             time.Duration
	ConsecutiveTimeouts int
	Outcome             DiagnosticOutcome
	CircuitOpen         bool
	CircuitOpenUntil    time.Time
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

// MarshalJSON renders only configured events, suitable for config projections.
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
// config.Config projections and tests, but callers that need wrapper support
// should use DecodeFile.
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
	h.Name = strings.TrimSpace(h.Name)
	if len(h.Name) > 128 {
		return fmt.Errorf("name must be <= 128 bytes")
	}
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
	if h.MaxConsecutiveTimeouts != nil {
		if *h.MaxConsecutiveTimeouts < 0 {
			return fmt.Errorf("max_consecutive_timeouts must be >= 0")
		}
		if *h.MaxConsecutiveTimeouts > maxMaxTimeouts {
			return fmt.Errorf("max_consecutive_timeouts must be <= %d", maxMaxTimeouts)
		}
	}
	if h.TimeoutCooldownSeconds == 0 {
		h.TimeoutCooldownSeconds = defaultCooldownSeconds
	}
	if h.TimeoutCooldownSeconds < 0 {
		return fmt.Errorf("timeout_cooldown_seconds must be >= 0")
	}
	if h.TimeoutCooldownSeconds > maxTimeoutCooldownSeconds {
		return fmt.Errorf("timeout_cooldown_seconds must be <= %d", maxTimeoutCooldownSeconds)
	}
	return nil
}

// UnmarshalJSON accepts harness snake_case fields and Codex camelCase aliases.
func (h *Handler) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type                  string `json:"type"`
		Name                  string `json:"name"`
		Command               string `json:"command"`
		CommandWindows        string `json:"command_windows"`
		CommandWindowsAlias   string `json:"commandWindows"`
		TimeoutSeconds        *int   `json:"timeout_seconds"`
		TimeoutAlias          *int   `json:"timeout"`
		StatusMessage         string `json:"status_message"`
		StatusMessageAlias    string `json:"statusMessage"`
		MaxTimeouts           *int   `json:"max_consecutive_timeouts"`
		CooldownSeconds       *int   `json:"timeout_cooldown_seconds"`
		UnsupportedAsync      *bool  `json:"async"`
		UnsupportedPromptName string `json:"prompt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.Type = strings.TrimSpace(raw.Type)
	h.Name = raw.Name
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
	if raw.MaxTimeouts != nil {
		maxTimeouts := *raw.MaxTimeouts
		h.MaxConsecutiveTimeouts = &maxTimeouts
	}
	if raw.CooldownSeconds != nil {
		h.TimeoutCooldownSeconds = *raw.CooldownSeconds
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

	mu       sync.Mutex
	breakers map[string]*timeoutBreaker
	now      func() time.Time
	execute  func(context.Context, Handler, string, []byte) commandResult
}

type timeoutBreaker struct {
	consecutive int
	openUntil   time.Time
	probeActive bool
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
	if ctx.Err() != nil {
		return out
	}
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
	toolID, _ := payload["tool_use_id"].(string)

	for groupIndex, group := range groups {
		if !eventIgnoresMatcher(event) && !group.matches(target) {
			continue
		}
		for hookIndex, hook := range group.Hooks {
			if ctx.Err() != nil {
				return out
			}
			identity := handlerIdentity(event, groupIndex, hookIndex, hook.Name)
			stateKey := fmt.Sprintf("%s/%d/%d", event, groupIndex, hookIndex)
			now := r.clock()()
			run, count, openUntil := r.beforeHook(stateKey, hook, now)
			if !run {
				out.Notices = append(out.Notices, fmt.Sprintf("[hook %s handler %s circuit open until %s; skipped]", event, identity, openUntil.UTC().Format(time.RFC3339)))
				out.Diagnostics = append(out.Diagnostics, Diagnostic{
					Event: event, Handler: identity, Target: target, ToolID: toolID,
					TimeoutSeconds: hook.TimeoutSeconds, ConsecutiveTimeouts: count,
					Outcome: OutcomeCircuitOpen, CircuitOpen: true, CircuitOpenUntil: openUntil,
				})
				continue
			}
			if hook.StatusMessage != "" {
				out.Notices = append(out.Notices, "[hook: "+hook.StatusMessage+"]")
			}
			started := r.clock()()
			cmdResult := r.commandRunner()(ctx, hook, r.CWD, stdin)
			finished := r.clock()()
			elapsed := finished.Sub(started)
			if elapsed < 0 {
				elapsed = 0
			}
			outcome := commandDiagnosticOutcome(cmdResult)
			count, circuitOpen, openUntil := r.afterHook(stateKey, hook, outcome, finished)
			diagnostic := Diagnostic{
				Event: event, Handler: identity, Target: target, ToolID: toolID,
				TimeoutSeconds: hook.TimeoutSeconds, Elapsed: elapsed,
				ConsecutiveTimeouts: count, Outcome: outcome,
				CircuitOpen: circuitOpen, CircuitOpenUntil: openUntil,
			}
			out.Diagnostics = append(out.Diagnostics, diagnostic)
			if ctx.Err() != nil {
				return out
			}
			if cmdResult.TimedOut {
				message := fmt.Sprintf("[hook %s handler %s timed out after %ds; continuing", event, identity, hook.TimeoutSeconds)
				if circuitOpen {
					message += "; circuit open until " + openUntil.UTC().Format(time.RFC3339)
				}
				out.Notices = append(out.Notices, message+"]")
				continue
			}
			parsed := parseCommandOutput(cmdResult)
			out.Block = out.Block || parsed.Block
			out.Reasons = append(out.Reasons, parsed.Reasons...)
			out.AdditionalContext = append(out.AdditionalContext, parsed.AdditionalContext...)
			out.Notices = append(out.Notices, parsed.Notices...)
		}
	}
	return out
}

func handlerIdentity(event Event, groupIndex, hookIndex int, name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return fmt.Sprintf("%s/%d/%d", event, groupIndex, hookIndex)
}

func (r *Runner) clock() func() time.Time {
	if r.now != nil {
		return r.now
	}
	return time.Now
}

func (r *Runner) commandRunner() func(context.Context, Handler, string, []byte) commandResult {
	if r.execute != nil {
		return r.execute
	}
	return runCommand
}

func maxTimeouts(hook Handler) int {
	if hook.MaxConsecutiveTimeouts == nil {
		return defaultMaxTimeouts
	}
	return *hook.MaxConsecutiveTimeouts
}

func (r *Runner) beforeHook(identity string, hook Handler, now time.Time) (run bool, count int, openUntil time.Time) {
	if maxTimeouts(hook) == 0 {
		return true, 0, time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breakers == nil {
		r.breakers = make(map[string]*timeoutBreaker)
	}
	state := r.breakers[identity]
	if state == nil {
		state = &timeoutBreaker{}
		r.breakers[identity] = state
	}
	if !state.openUntil.IsZero() && now.Before(state.openUntil) {
		return false, state.consecutive, state.openUntil
	}
	if !state.openUntil.IsZero() && state.consecutive >= maxTimeouts(hook) {
		if state.probeActive {
			return false, state.consecutive, state.openUntil
		}
		state.probeActive = true
	}
	return true, state.consecutive, state.openUntil
}

func (r *Runner) afterHook(identity string, hook Handler, outcome DiagnosticOutcome, now time.Time) (count int, circuitOpen bool, openUntil time.Time) {
	threshold := maxTimeouts(hook)
	if threshold == 0 {
		return 0, false, time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breakers == nil {
		r.breakers = make(map[string]*timeoutBreaker)
	}
	state := r.breakers[identity]
	if state == nil {
		state = &timeoutBreaker{}
		r.breakers[identity] = state
	}
	state.probeActive = false
	switch outcome {
	case OutcomeTimeout:
		if state.consecutive < maxConsecutiveTimeoutCount {
			state.consecutive++
		}
		if state.consecutive >= threshold {
			state.openUntil = now.Add(time.Duration(hook.TimeoutCooldownSeconds) * time.Second)
		}
	default:
		state.consecutive = 0
		state.openUntil = time.Time{}
	}
	return state.consecutive, !state.openUntil.IsZero() && now.Before(state.openUntil), state.openUntil
}

func commandDiagnosticOutcome(result commandResult) DiagnosticOutcome {
	switch {
	case result.StartErr != nil:
		return OutcomeStartFailed
	case result.TimedOut:
		return OutcomeTimeout
	case result.Canceled:
		return OutcomeCanceled
	case result.Code != 0:
		return OutcomeExitNonzero
	case malformedJSONObject(result.Stdout):
		return OutcomeParseFailed
	default:
		return OutcomeSuccess
	}
}

func malformedJSONObject(stdout string) bool {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" || !strings.HasPrefix(stdout, "{") {
		return false
	}
	var value hookOutput
	return json.Unmarshal([]byte(stdout), &value) != nil
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
	if err := ctx.Err(); err != nil {
		return commandResult{
			Code:     -1,
			TimedOut: errors.Is(err, context.DeadlineExceeded),
			Canceled: errors.Is(err, context.Canceled),
		}
	}
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
	if err := runCtx.Err(); err != nil {
		return commandResult{
			Code:     -1,
			TimedOut: errors.Is(err, context.DeadlineExceeded),
			Canceled: errors.Is(err, context.Canceled),
		}
	}

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
