// Package delegate implements configured child-agent execution. It lives
// outside internal/tools to avoid a tools -> agent import cycle: child-agent
// tools start agents, and agent already dispatches through tools.
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/todo"
	"harness/internal/tools"
)

const DefaultMaxTurns = 20

const delegateToolName = "delegate"
const updateTodosToolName = "update_todos"

var childSeq atomic.Uint64

// Runtime is the parent agent state a delegate call needs to start a child.
type Runtime struct {
	Provider          llm.Provider
	ProviderName      string
	Model             string
	ContextWindow     int
	Registry          *llm.Registry
	Reasoning         llm.ReasoningConfig
	ResponsesStateful bool
	System            string
	Agent             string
	ToolNames         []string
	SessionPath       string
	ParentChildID     string
}

// Launch is the fully resolved child-agent runtime for one delegate call.
type Launch struct {
	Provider          llm.Provider
	ProviderName      string
	Model             string
	ContextWindow     int
	Registry          *llm.Registry
	Reasoning         llm.ReasoningConfig
	ResponsesStateful bool
	System            string
	Agent             string
	Tools             *tools.Registry
}

// AgentCandidate is a configured agent that may be delegated to when its tools
// are a subset of the calling agent's current tools.
type AgentCandidate struct {
	Name      string
	ToolNames []string
}

// State stores the current runtime snapshot. Main updates it on startup and
// after /model or /agent switches; delegate calls read it when they begin.
type State struct {
	mu      sync.RWMutex
	runtime Runtime
}

func NewState(runtime Runtime) *State {
	return &State{runtime: cloneRuntime(runtime)}
}

func (s *State) Set(runtime Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = cloneRuntime(runtime)
}

func (s *State) Snapshot() Runtime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRuntime(s.runtime)
}

// Options configures the delegate tool.
type Options struct {
	MaxTurns                  int
	CompactKeepTurns          int
	CompactSummaryMaxTokens   int
	CompactToolResultMaxBytes int
	AgentCandidates           func(Runtime) []AgentCandidate
	Now                       func() time.Time
}

// RunRequest is one child-agent launch request.
type RunRequest struct {
	Kind       string
	Task       string
	Agent      string
	MaxTurns   *int
	ChildID    string
	Background bool

	// Tools optionally scopes the child to a subset of the resolved agent's
	// tools. Empty means the full set (back-compatible). Each name is validated
	// against the resolved launch tools; an unknown name is a hard error so a
	// typo fails fast rather than silently shrinking the child's surface.
	Tools []string
}

// RunResult is the complete outcome of one child-agent run.
type RunResult struct {
	Report         string
	Usage          llm.Usage
	ModelTurns     int
	ChildID        string
	TranscriptPath string
	Agent          string
	ProviderName   string
	Model          string
	SaveError      error
}

// RuntimeRebinder is implemented by tools whose behavior depends on the
// immediate parent agent runtime. Child registries use it to bind recursive
// delegate-like tools to the child instead of the original parent.
type RuntimeRebinder interface {
	RebindRuntime(snapshot func() Runtime) tools.Tool
}

// Runner starts configured child agents. It is shared by synchronous delegate
// and background delegation.
type Runner struct {
	snapshot func() Runtime
	resolve  func(Runtime, string) (Launch, error)
	opts     Options
}

func NewRunner(snapshot func() Runtime, resolve func(Runtime, string) (Launch, error), opts Options) *Runner {
	return &Runner{snapshot: snapshot, resolve: resolve, opts: opts}
}

func (r *Runner) Rebind(snapshot func() Runtime) *Runner {
	if r == nil {
		return nil
	}
	next := *r
	next.snapshot = snapshot
	return &next
}

func (r *Runner) Schema() json.RawMessage {
	var agents []string
	var toolNames []string
	if r != nil && r.snapshot != nil {
		runtime := r.snapshot()
		toolNames = runtime.ToolNames
		if r.opts.AgentCandidates != nil {
			agents = DelegatableAgentNames(runtime.ToolNames, r.opts.AgentCandidates(runtime))
		}
	}
	return schema(agents, toolNames)
}

// Tool is a model-callable configured-agent launcher.
type Tool struct {
	runner     *Runner
	background tools.BackgroundJobStarter
}

func New(snapshot func() Runtime, resolve func(Runtime, string) (Launch, error), opts Options) *Tool {
	return NewTool(NewRunner(snapshot, resolve, opts))
}

func NewTool(runner *Runner, background ...tools.BackgroundJobStarter) *Tool {
	var starter tools.BackgroundJobStarter
	if len(background) > 0 {
		starter = background[0]
	}
	return &Tool{runner: runner, background: starter}
}

func (*Tool) Name() string { return "delegate" }

func (*Tool) Description() string {
	return "Run a configured delegate agent on a self-contained task and return its final report. Provide a JSON object matching the schema."
}

func (t *Tool) Schema() json.RawMessage {
	if t == nil || t.runner == nil {
		return schema(nil, nil)
	}
	return t.runner.Schema()
}

func (*Tool) ReadOnly(json.RawMessage) bool { return false }

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunMetered(ctx, input)
	return result.Text, err
}

func (t *Tool) RunMetered(ctx context.Context, input json.RawMessage) (tools.MeteredResult, error) {
	if t == nil || t.runner == nil {
		return tools.MeteredResult{}, fmt.Errorf("delegate runner is not initialized")
	}
	req, err := DecodeRunRequest(input, "delegate")
	if err != nil {
		return tools.MeteredResult{}, err
	}
	if req.Background {
		if err := ctx.Err(); err != nil {
			return tools.MeteredResult{}, err
		}
		if t.background == nil {
			return tools.MeteredResult{}, fmt.Errorf("background manager is not initialized")
		}
		info, err := t.background.StartBackgroundJob(tools.BackgroundJobRequest{
			Kind:        "delegate",
			Description: req.Task,
			Run: func(ctx context.Context, childID string) (tools.BackgroundJobResult, error) {
				req.Background = false
				req.ChildID = childID
				result, err := t.runner.Run(ctx, req)
				return tools.BackgroundJobResult{
					Text:           result.Report,
					TranscriptPath: result.TranscriptPath,
				}, err
			},
		})
		if err != nil {
			return tools.MeteredResult{}, err
		}
		return tools.MeteredResult{Text: fmt.Sprintf("background job %s started", info.ID)}, nil
	}
	result, err := t.runner.Run(ctx, req)
	if err != nil {
		return tools.MeteredResult{Usage: result.Usage}, err
	}
	return tools.MeteredResult{Text: result.Report, Usage: result.Usage}, nil
}

func (t *Tool) RebindRuntime(snapshot func() Runtime) tools.Tool {
	if t == nil || t.runner == nil {
		return NewTool(nil)
	}
	return NewTool(t.runner.Rebind(snapshot), t.background)
}

func DecodeRunRequest(input json.RawMessage, kind string) (RunRequest, error) {
	var args struct {
		Task       string   `json:"task"`
		Agent      string   `json:"agent"`
		MaxTurns   *int     `json:"max_turns"`
		Background bool     `json:"background"`
		Tools      []string `json:"tools"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return RunRequest{}, err
	}
	task := strings.TrimSpace(args.Task)
	if task == "" {
		return RunRequest{}, fmt.Errorf("task is required")
	}
	if kind == "" {
		kind = "delegate"
	}
	return RunRequest{
		Kind:       kind,
		Task:       task,
		Agent:      strings.TrimSpace(args.Agent),
		MaxTurns:   args.MaxTurns,
		Background: args.Background,
		Tools:      cleanToolNames(args.Tools),
	}, nil
}

// cleanToolNames trims and drops blank entries, preserving order and dropping
// duplicates. It returns nil when nothing remains so an empty/whitespace-only
// list resolves to the full tool set.
func cleanToolNames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if r == nil {
		return RunResult{}, fmt.Errorf("delegate runner is not initialized")
	}
	if req.Kind == "" {
		req.Kind = "delegate"
	}
	if r.snapshot == nil {
		return RunResult{}, fmt.Errorf("delegate runtime is not initialized")
	}
	maxTurns, err := r.maxTurns(req.MaxTurns)
	if err != nil {
		return RunResult{}, err
	}

	runtime := r.snapshot()
	if runtime.Provider == nil {
		return RunResult{}, fmt.Errorf("delegate runtime is not initialized")
	}
	if r.resolve == nil {
		return RunResult{}, fmt.Errorf("delegate resolver is not initialized")
	}
	launch, err := r.resolve(runtime, req.Agent)
	if err != nil {
		return RunResult{}, err
	}
	if launch.Provider == nil {
		return RunResult{}, fmt.Errorf("delegate provider is not initialized")
	}
	if launch.Tools == nil {
		return RunResult{}, fmt.Errorf("delegate tool registry is not initialized")
	}

	// Resolve the child's tool surface: the full set by default, or the validated
	// requested subset. Validating before any session metadata is written fails a
	// typo'd name fast without leaving a stranded "running" child record.
	toolNames := launch.Tools.Names()
	if len(req.Tools) > 0 {
		if missing := MissingTools(req.Tools, toolNames); len(missing) > 0 {
			return RunResult{}, fmt.Errorf("delegate: unknown tools requested: %s (available: %s)",
				strings.Join(missing, ", "), strings.Join(toolNames, ", "))
		}
		toolNames = slices.Clone(req.Tools)
	}

	childID := strings.TrimSpace(req.ChildID)
	if childID == "" {
		childID = nextChildID(req.Kind)
	}
	now := r.now()
	childDir, saveErr := r.saveChildMeta(runtime, launch, childID, req, "running", now, now, agent.TurnUsage{}, nil, 0)

	childTodos := todo.NewStore()
	hasTodoTool := slices.Contains(toolNames, updateTodosToolName)
	childTools, err := r.childTools(runtime, launch, childID, childTodos, toolNames)
	if err != nil {
		return RunResult{ChildID: childID, TranscriptPath: childDir, SaveError: saveErr}, err
	}
	child := agent.New(launch.Provider, childTools, agent.Options{
		MaxTurns:                  maxTurns,
		Model:                     launch.Model,
		ContextWindow:             launch.ContextWindow,
		Registry:                  launch.Registry,
		Reasoning:                 launch.Reasoning,
		ResponsesStateful:         launch.ResponsesStateful,
		CompactKeepTurns:          r.opts.CompactKeepTurns,
		CompactSummaryMaxTokens:   r.opts.CompactSummaryMaxTokens,
		CompactToolResultMaxBytes: r.opts.CompactToolResultMaxBytes,
		Now:                       r.opts.Now,
	})
	child.SetSystem(launch.System)

	sink := newChildSink(childDir, childTodos, hasTodoTool)
	sink.User(req.Task)
	runErr := child.RunTurn(ctx, req.Task, sink)
	usage := sink.usage
	status := "completed"
	errText := ""
	if runErr != nil {
		status = "failed"
		errText = runErr.Error()
	}
	if err := r.saveChildSession(runtime, launch, childID, req, child, childTodos, usage, status, errText, now); err != nil && saveErr == nil {
		saveErr = err
	}
	if runErr != nil {
		return RunResult{
			Usage:          usage.Usage,
			ModelTurns:     usage.ModelTurns,
			ChildID:        childID,
			TranscriptPath: childDir,
			Agent:          launch.Agent,
			ProviderName:   launch.ProviderName,
			Model:          launch.Model,
			SaveError:      saveErr,
		}, runErr
	}
	report := strings.TrimSpace(lastAssistantText(child.Transcript()))
	if report == "" {
		report = "(delegate completed without a final text response)"
	}
	report += fmt.Sprintf("\n\n[delegate: %s, %d input tokens, %d output tokens",
		modelTurnPhrase(usage.ModelTurns), usage.Usage.InputTokens, usage.Usage.OutputTokens)
	if childDir != "" {
		report += fmt.Sprintf(", transcript %s", childDir)
	}
	if saveErr != nil {
		report += fmt.Sprintf(", transcript save failed: %v", saveErr)
	}
	report += "]"
	return RunResult{
		Report:         report,
		Usage:          usage.Usage,
		ModelTurns:     usage.ModelTurns,
		ChildID:        childID,
		TranscriptPath: childDir,
		Agent:          launch.Agent,
		ProviderName:   launch.ProviderName,
		Model:          launch.Model,
		SaveError:      saveErr,
	}, nil
}

func (r *Runner) childTools(parent Runtime, launch Launch, childID string, todos *todo.Store, names []string) (*tools.Registry, error) {
	if names == nil {
		names = launch.Tools.Names()
	}
	childTools, err := launch.Tools.Subset(names)
	if err != nil {
		return nil, err
	}
	if slices.Contains(names, updateTodosToolName) {
		childTools.Register(todo.NewTool(todos))
	}
	childRuntime := Runtime{
		Provider:      launch.Provider,
		ProviderName:  launch.ProviderName,
		Model:         launch.Model,
		ContextWindow: launch.ContextWindow,
		Registry:      launch.Registry,
		Reasoning:     launch.Reasoning,
		System:        launch.System,
		Agent:         launch.Agent,
		ToolNames:     names,
		SessionPath:   parent.SessionPath,
		ParentChildID: childID,
	}
	childState := NewState(childRuntime)
	for _, name := range childTools.Names() {
		tool, ok := childTools.Lookup(name)
		if !ok {
			continue
		}
		if rebinder, ok := tool.(RuntimeRebinder); ok {
			childTools.Register(rebinder.RebindRuntime(childState.Snapshot))
		}
	}
	return childTools, nil
}

func (r *Runner) maxTurns(requested *int) (int, error) {
	cap := r.opts.MaxTurns
	if cap <= 0 {
		cap = DefaultMaxTurns
	}
	if requested == nil {
		return cap, nil
	}
	if *requested <= 0 {
		return 0, fmt.Errorf("max_turns must be positive")
	}
	if *requested > cap {
		return cap, nil
	}
	return *requested, nil
}

func (r *Runner) now() time.Time {
	if r != nil && r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

func modelTurnPhrase(n int) string {
	if n == 1 {
		return "1 model turn"
	}
	return fmt.Sprintf("%d model turns", n)
}

func lastAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		var parts []string
		for _, b := range msgs[i].Content {
			if b.Kind == llm.BlockText && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func cloneRuntime(runtime Runtime) Runtime {
	runtime.ToolNames = slices.Clone(runtime.ToolNames)
	return runtime
}

// MissingTools returns required tool names that are not available, preserving
// the required order and de-duplicating repeated names.
func MissingTools(required, available []string) []string {
	have := make(map[string]bool, len(available))
	for _, name := range available {
		have[name] = true
	}
	seen := make(map[string]bool)
	var missing []string
	for _, name := range required {
		if have[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	return missing
}

// DelegatableAgentNames returns the agent names whose tools are a subset of
// available, preserving the candidate order.
func DelegatableAgentNames(available []string, candidates []AgentCandidate) []string {
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Name == "" {
			continue
		}
		if len(MissingTools(candidate.ToolNames, available)) == 0 {
			names = append(names, candidate.Name)
		}
	}
	return names
}

func schema(agents, toolNames []string) json.RawMessage {
	agent := map[string]any{
		"type":        "string",
		"description": "Optional configured agent name to run. When omitted, uses the current active agent.",
	}
	if len(agents) > 0 {
		agent["enum"] = agents
	}
	items := map[string]any{"type": "string"}
	if len(toolNames) > 0 {
		items["enum"] = toolNames
	}
	body := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "The self-contained task for the delegate agent.",
			},
			"agent": agent,
			"max_turns": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Optional model-turn cap for this delegate call. Values above the configured cap are reduced to the cap.",
			},
			"background": map[string]any{
				"type":        "boolean",
				"description": "When true, start the delegate as a process-local background job and return a job id immediately.",
			},
			"tools": map[string]any{
				"type":        "array",
				"items":       items,
				"description": "Optional subset of the agent's tools to expose to the child, by exact name, to cut the child's per-turn schema overhead. When omitted, the child gets the agent's full tool set. Every name must be one of the agent's available tools.",
			},
		},
		"required": []string{"task"},
	}
	b, _ := json.Marshal(body)
	return b
}

func nextChildID(kind string) string {
	prefix := "child"
	switch strings.TrimSpace(kind) {
	case "delegate":
		prefix = "delegate"
	}
	return fmt.Sprintf("%s_%s_%06d", prefix, time.Now().UTC().Format("20060102T150405Z"), childSeq.Add(1))
}

func (r *Runner) saveChildMeta(parent Runtime, launch Launch, childID string, req RunRequest, status string, created, updated time.Time, usage agent.TurnUsage, runErr error, messageCount int) (string, error) {
	if parent.SessionPath == "" {
		return "", nil
	}
	meta := session.ChildMeta{
		ID:           childID,
		ParentID:     parent.ParentChildID,
		Kind:         req.Kind,
		Agent:        launch.Agent,
		Provider:     launch.ProviderName,
		Model:        launch.Model,
		Status:       status,
		TaskPreview:  preview(req.Task, 240),
		Created:      created,
		Updated:      updated,
		Usage:        usage.Usage,
		MessageCount: messageCount,
	}
	if runErr != nil {
		meta.Error = runErr.Error()
	}
	return session.SaveChildMeta(parent.SessionPath, meta)
}

func (r *Runner) saveChildSession(parent Runtime, launch Launch, childID string, req RunRequest, child *agent.Agent, todos *todo.Store, usage agent.TurnUsage, status, errText string, created time.Time) error {
	if parent.SessionPath == "" {
		return nil
	}
	updated := r.now()
	childDir := session.ChildSessionDir(parent.SessionPath, childID)
	var cost float64
	if launch.Registry != nil {
		cost, _ = launch.Registry.Cost(launch.Model, usage.Usage)
	}
	if err := (session.Session{
		Version:       session.Version,
		Provider:      launch.ProviderName,
		Model:         launch.Model,
		Created:       created,
		Updated:       updated,
		System:        launch.System,
		Agent:         launch.Agent,
		Turn:          1,
		Messages:      child.Transcript(),
		ResponseState: child.ResponseState(),
		Todos:         todos.Snapshot(),
		Usage:         session.UsageTotals{Usage: usage.Usage, CostUSD: cost},
	}).Save(childDir); err != nil {
		return err
	}
	var runErr error
	if errText != "" {
		runErr = fmt.Errorf("%s", errText)
	}
	_, err := r.saveChildMeta(parent, launch, childID, req, status, created, updated, usage, runErr, len(child.Transcript()))
	return err
}

func preview(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

type childSink struct {
	usage       agent.TurnUsage
	sessionDir  string
	todos       *todo.Store
	todoContext bool
	pending     map[string]llm.ToolCall
}

func newChildSink(sessionDir string, todos *todo.Store, todoContext bool) *childSink {
	return &childSink{sessionDir: sessionDir, todos: todos, todoContext: todoContext, pending: make(map[string]llm.ToolCall)}
}

func (s *childSink) User(text string) {
	s.append(session.Event{Type: session.EventUser, Turn: 1, Text: text})
}

func (s *childSink) TextDelta(text string) {
	s.append(session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: text})
}

func (s *childSink) ReasoningSummary(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.append(session.Event{Type: session.EventReasoningSummary, Turn: 1, Text: text})
}

func (*childSink) ModelTurnStart(int, int, agent.ContextEstimate) {}

func (s *childSink) ModelTurnComplete(u agent.ModelTurnUsage) {
	usage := u.Usage
	s.append(session.Event{Type: session.EventModelTurnUsage, Turn: 1, Usage: &usage, ModelTurns: u.ModelTurn})
}

func (*childSink) ToolUseStart(llm.ToolCall) {}

func (*childSink) ToolUseDelta(int, string) {}

func (s *childSink) ToolStart(call llm.ToolCall) {
	s.pending[call.ID] = call
	s.append(session.Event{Type: session.EventToolStart, Turn: 1, ToolID: call.ID, Tool: call.Name, Input: call.Input})
}

func (s *childSink) ToolResult(result llm.ToolResult) {
	call := s.pending[result.ForID]
	delete(s.pending, result.ForID)
	display := fmt.Sprintf("[tool: %s completed]", call.Name)
	if result.IsError {
		display = fmt.Sprintf("[tool: %s error: %s]", call.Name, preview(firstLine(result.Text), 120))
	}
	s.append(session.Event{Type: session.EventToolResult, Turn: 1, ToolID: result.ForID, Tool: call.Name, Display: display})
}

func (s *childSink) ArchiveToolResult(result llm.ToolResult) (agent.ToolResultArchive, error) {
	ref, err := session.SaveToolResultArtifact(s.sessionDir, 1, result)
	if err != nil || ref == "" {
		return agent.ToolResultArchive{}, err
	}
	return agent.ToolResultArchive{
		DisplayPath: ref,
		ModelPath:   filepath.Join(s.sessionDir, ref),
	}, nil
}

func (s *childSink) Notice(msg string) {
	s.append(session.Event{Type: session.EventNotice, Turn: 1, Display: msg})
}

func (s *childSink) RequestContext() []string {
	if s.todos == nil || !s.todoContext {
		return nil
	}
	return []string{todo.RequestContext(s.todos.Snapshot())}
}

func (s *childSink) TurnComplete(usage agent.TurnUsage) {
	s.usage = usage
	u := usage.Usage
	s.append(session.Event{Type: session.EventTurnUsage, Turn: 1, Usage: &u, ModelTurns: usage.ModelTurns})
}

func (s *childSink) append(ev session.Event) {
	if s.sessionDir == "" {
		return
	}
	_ = session.AppendEvent(s.sessionDir, ev)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
