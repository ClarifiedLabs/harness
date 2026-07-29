// Package delegate implements configured child-agent execution. It lives
// outside internal/tools to avoid a tools -> agent import cycle: child-agent
// tools start agents, and agent already dispatches through tools.
package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/prompts"
)

const DefaultMaxTurns = 20
const DefaultMaxDepth = 3
const maxAgentDescriptionBytes = 160

const delegateToolName = "delegate"
const updateTodosToolName = "update_todos"

const ModeImplementation = "implementation"
const continuationContextPercent = 60

const (
	continuationModeRetained   = "retained"
	continuationModeCheckpoint = "compact_checkpoint"
)
const continuationFingerprintVersion = 3

var childSeq atomic.Uint64

// Runtime is the parent agent state a delegate call needs to start a child.
type Runtime struct {
	Provider          llm.Provider
	ProviderName      string
	Model             string
	ContextWindow     int
	MaxOutputTokens   int
	Registry          *llm.Registry
	Reasoning         llm.ReasoningConfig
	ServerTools       []llm.ServerTool
	ResponsesStateful bool
	System            string
	Agent             string
	ToolNames         []string
	SessionPath       string
	CacheAffinityID   string
	ParentChildID     string
	Depth             int
	MaxPromptTokens   int
	MaxPromptCostUSD  float64
	Build             session.BuildMetadata
	RuntimeProfile    session.RuntimeProfile
}

// Launch is the fully resolved child-agent runtime for one delegate call.
type Launch struct {
	Provider          llm.Provider
	ProviderName      string
	Model             string
	ContextWindow     int
	MaxOutputTokens   int
	Registry          *llm.Registry
	Reasoning         llm.ReasoningConfig
	ServerTools       []llm.ServerTool
	ResponsesStateful bool
	System            string
	Agent             string
	Tools             *tools.Registry
}

// AgentCandidate is a configured agent that may be delegated to when its tools
// are a subset of the calling agent's current tools.
type AgentCandidate struct {
	Name            string
	Description     string
	ToolNames       []string
	WorkspaceAccess string
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
	MaxDepth                  int
	CompactKeepTurns          int
	CompactKeepTokens         int
	CompactTriggerPercent     int
	CompactTargetPercent      int
	DisableAutoCompaction     bool
	CompactSummaryMaxTokens   int
	CompactToolResultMaxBytes int
	RetentionPolicy           agent.RetentionPolicy
	AgentCandidates           func(Runtime) []AgentCandidate
	ActivityRegistry          *ActivityRegistry
	Now                       func() time.Time
}

// RunRequest is one child-agent launch request.
type RunRequest struct {
	Kind            string
	Mode            string
	Task            string
	Agent           string
	MaxTurns        *int
	ContinueChildID string
	ChildID         string
	Background      bool
	ResourceKey     string
	Access          string

	maxTurnsInherited         bool
	leaseAcquired             bool
	continuationMode          string
	continuationContextBefore int
	continuationContextAfter  int
	continuationContextWindow int
}

// RunResult is the complete outcome of one child-agent run.
type RunResult struct {
	Report            string
	Usage             llm.Usage
	Turns             int
	EffectiveMaxTurns int
	TerminationReason agent.TerminationReason
	Mode              string
	ContinuedFrom     string
	ContinuationMode  string
	ChildID           string
	TranscriptPath    string
	Agent             string
	ProviderName      string
	Model             string
	SaveError         error
	// Progress carries the live-progress closure (func() agent.DelegateProgressSnapshot)
	// for foreground delegates so the parent wait ticker can read child activity while
	// the (synchronous) run is in progress. It is nil for failure-before-run paths.
	Progress any
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
	snapshot         func() Runtime
	resolve          func(Runtime, string) (Launch, error)
	opts             Options
	childToolBuilder func(Runtime, Launch, string, *todo.Store, []string) (*tools.Registry, error)
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
	var agents []AgentCandidate
	if r != nil && r.snapshot != nil && r.opts.AgentCandidates != nil {
		runtime := r.snapshot()
		agents = DelegatableAgentCandidates(runtime.ToolNames, r.opts.AgentCandidates(runtime))
	}
	return schema(agents, r.configuredMaxTurns())
}

// Tool is a model-callable configured-agent launcher.
type Tool struct {
	runner     *Runner
	background tools.BackgroundJobStarter
	// progress stashes live Progress objects keyed by raw input between
	// StartProgress and RunMetered so foreground runs reuse the exact object
	// the renderer's closure reads. Lazily allocated; nil when unused.
	progress *sync.Map
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
	return "Delegate broad exploration or separable work; keep small or tightly coupled tasks local. Launch independent calls together, then synthesize without polling. Read-only background agents share leases automatically; mutating siblings need distinct scope paths."
}

func (t *Tool) Schema() json.RawMessage {
	if t == nil || t.runner == nil {
		return schema(nil, DefaultMaxTurns)
	}
	return t.runner.Schema()
}

// PreserveSchemaDescriptions keeps the dynamic compatible-agent catalog and
// delegation guidance model-facing; registry defaults strip schema descriptions.
func (*Tool) PreserveSchemaDescriptions() bool { return true }

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
		prepared, err := t.runner.prepareRun(req)
		if err != nil {
			return tools.MeteredResult{}, err
		}
		req = prepared.req
		maxTurns := prepared.maxTurns
		if err := ctx.Err(); err != nil {
			return tools.MeteredResult{}, err
		}
		if t.background == nil {
			return tools.MeteredResult{}, fmt.Errorf("background manager is not initialized")
		}
		defaultResource, err := tools.DefaultBackgroundResource("")
		if err != nil {
			return tools.MeteredResult{}, err
		}
		req.ResourceKey, req.Access, err = tools.ResolveBackgroundLease(
			req.ResourceKey,
			req.Access,
			defaultResource,
			tools.BackgroundAccessExclusive,
		)
		if err != nil {
			return tools.MeteredResult{}, err
		}
		req.leaseAcquired = true
		// Create the progress here so its closure is available to the parent wait
		// ticker immediately (the job runs in a goroutine that starts now); the
		// same progress feeds the child sink inside the job's Run closure.
		progress := NewProgress()
		jobAgent := req.Agent
		if prepared.continuation != nil {
			jobAgent = prepared.continuation.meta.Agent
		}
		info, err := t.background.StartBackgroundJob(tools.BackgroundJobRequest{
			Kind:          "delegate",
			Description:   req.Task,
			Agent:         jobAgent,
			ResourceKey:   req.ResourceKey,
			Access:        req.Access,
			WaitForPrompt: true,
			Progress:      progress.Closure(),
			Run: func(ctx context.Context, childID string) (tools.BackgroundJobResult, error) {
				childReq := req
				childReq.Background = false
				childReq.ChildID = childID
				result, err := t.runner.Run(ctx, childReq, progress)
				return tools.BackgroundJobResult{
					Text:           result.Report,
					TranscriptPath: result.TranscriptPath,
					Usage:          result.Usage,
					Progress:       result.Progress,
				}, err
			},
		})
		if err != nil {
			return tools.MeteredResult{}, err
		}
		receipt := fmt.Sprintf("background job %s started (turn budget: %d", info.ID, maxTurns)
		if req.Mode != "" {
			receipt += ", mode: " + req.Mode
		}
		if req.ContinueChildID != "" {
			receipt += ", continues: " + req.ContinueChildID
		}
		receipt += ", scope: " + req.ResourceKey + ", access: " + req.Access
		receipt += ")"
		return tools.MeteredResult{
			Text:     receipt,
			Progress: progress.Closure(),
		}, nil
	}
	// Foreground: the live progress was created by StartProgress (called by the
	// agent just before dispatch) and stashed keyed by input so this Run reuses
	// the very object the renderer's closure reads. Fall back to a fresh progress
	// when no StartProgress ran (e.g. Run called directly outside dispatch).
	progress := t.takeProgress(input)
	result, err := t.runner.Run(ctx, req, progress)
	if err != nil {
		return tools.MeteredResult{Usage: result.Usage, Progress: result.Progress}, err
	}
	return tools.MeteredResult{Text: result.Report, Usage: result.Usage, Progress: result.Progress}, nil
}

// StartProgress implements tools.ProgressStarter. It creates the live progress
// object for an outstanding foreground delegate call and returns its closure so
// the parent wait ticker can read child activity while the (synchronous) run
// blocks. The progress is stashed keyed by the raw input so RunMetered,
// invoked next with the same input, reuses this exact object (not a fresh one);
// RunMetered removes it. Distinct parallel inputs thus never collide.
func (t *Tool) StartProgress(input json.RawMessage) any {
	if t == nil || t.runner == nil {
		return nil
	}
	req, err := DecodeRunRequest(input, "delegate")
	if err != nil || req.Background {
		return nil // background progress is created inline in RunMetered
	}
	progress := NewProgress()
	if t.progress == nil {
		t.progress = &sync.Map{}
	}
	t.progress.Store(string(input), progress)
	return progress.Closure()
}

// takeProgress removes and returns the progress stashed for input by
// StartProgress, or nil when none was stashed (direct Run paths).
func (t *Tool) takeProgress(input json.RawMessage) *Progress {
	if t == nil || t.progress == nil {
		return nil
	}
	v, ok := t.progress.LoadAndDelete(string(input))
	if !ok {
		return nil
	}
	return v.(*Progress)
}

func (t *Tool) RebindRuntime(snapshot func() Runtime) tools.Tool {
	if t == nil || t.runner == nil {
		return NewTool(nil)
	}
	return NewTool(t.runner.Rebind(snapshot), t.background)
}

func DecodeRunRequest(input json.RawMessage, kind string) (RunRequest, error) {
	var args struct {
		Task       string `json:"task"`
		Agent      string `json:"agent"`
		Mode       string `json:"mode"`
		MaxTurns   *int   `json:"max_turns"`
		ContinueID string `json:"continue_child_id"`
		Background bool   `json:"background"`
		Scope      string `json:"scope"`
		Access     string `json:"access"`
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
	mode, err := normalizeMode(args.Mode)
	if err != nil {
		return RunRequest{}, err
	}
	resourceKey := strings.TrimSpace(args.Scope)
	access := strings.TrimSpace(args.Access)
	if !args.Background && (resourceKey != "" || access != "") {
		return RunRequest{}, fmt.Errorf("scope and access require background:true")
	}
	return RunRequest{
		Kind:            kind,
		Mode:            mode,
		Task:            task,
		Agent:           strings.TrimSpace(args.Agent),
		MaxTurns:        args.MaxTurns,
		ContinueChildID: strings.TrimSpace(args.ContinueID),
		Background:      args.Background,
		ResourceKey:     resourceKey,
		Access:          access,
	}, nil
}

// Run executes one child-agent run. progress, when non-nil, is the live
// progress object the child sink updates so a parent wait ticker can read child
// activity while this (synchronous) call blocks; a fresh Progress is created
// when nil for callers that do not surface live progress.
func (r *Runner) Run(ctx context.Context, req RunRequest, progress *Progress) (result RunResult, retErr error) {
	if r == nil {
		return RunResult{}, fmt.Errorf("delegate runner is not initialized")
	}
	if progress == nil {
		progress = NewProgress()
	}
	prepared, err := r.prepareRun(req)
	if err != nil {
		return RunResult{}, err
	}
	req = prepared.req
	runtime := prepared.runtime
	maxTurns := prepared.maxTurns
	continuation := prepared.continuation
	maxDepth := r.maxDepth()
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
	launch.System = childBudgetSystemPrompt(childSystemPrompt(withoutChildBudget(launch.System)), maxTurns)
	if req.Mode == ModeImplementation {
		launch.System = implementationSystemPrompt(launch.System)
	}
	progress.SetAgent(launch.Agent)

	toolNames := launch.Tools.Names()
	if runtime.Depth+1 >= maxDepth {
		toolNames = withoutTool(toolNames, delegateToolName)
	}
	runtimeFingerprint, err := r.runtimeFingerprint(runtime, launch, req, maxTurns, toolNames)
	if err != nil {
		return RunResult{}, err
	}
	if continuation != nil {
		if continuation.meta.Agent != launch.Agent {
			return RunResult{}, continuationIncompatibleError(req.ContinueChildID, "resolved agent does not match the saved agent")
		}
		if continuation.state.System != launch.System {
			return RunResult{}, continuationIncompatibleError(req.ContinueChildID, "saved system prompt does not match the current runtime")
		}
		if continuation.meta.RuntimeFingerprint != runtimeFingerprint {
			return RunResult{}, continuationIncompatibleError(req.ContinueChildID, "provider, model, prompt, tools, or runtime policy changed")
		}
	}

	childID := strings.TrimSpace(req.ChildID)
	if childID == "" {
		childID = nextChildID(req.Kind)
	}
	if childID == req.ContinueChildID {
		return RunResult{}, fmt.Errorf("delegate continuation must use a fresh child id, not %q", childID)
	}
	created := r.now()
	childDir, saveErr := r.saveChildMeta(runtime, launch, childID, req, maxTurns, runtimeFingerprint, session.ChildStatusRunning, created, created, agent.PromptUsage{}, nil, 0)
	result = RunResult{
		ChildID:           childID,
		TranscriptPath:    childDir,
		EffectiveMaxTurns: maxTurns,
		Mode:              req.Mode,
		ContinuedFrom:     req.ContinueChildID,
		SaveError:         saveErr,
	}
	activity := r.opts.ActivityRegistry.Register(ActivityStart{
		ID:             childID,
		ParentID:       runtime.ParentChildID,
		Depth:          runtime.Depth + 1,
		Agent:          launch.Agent,
		TranscriptPath: childDir,
	})

	terminalStatus := session.ChildStatusFailed
	var terminalUsage agent.PromptUsage
	var terminalErr error
	var terminalMessageCount int
	var terminalUpdated time.Time
	var terminalOnce sync.Once
	flushDisplay := func() {}
	finish := func() {
		terminalOnce.Do(func() {
			flushDisplay()
			progress.markFinished()
			if terminalUpdated.IsZero() {
				terminalUpdated = r.now()
			}
			_, err := r.saveChildMeta(runtime, launch, childID, req, maxTurns, runtimeFingerprint, terminalStatus, created, terminalUpdated, terminalUsage, terminalErr, terminalMessageCount)
			result.SaveError = errors.Join(result.SaveError, err)
			activity.Finish(terminalStatus, terminalUsage.Turns)
		})
	}
	defer finish()

	childTodos := todo.NewStore()
	hasTodoTool := slices.Contains(toolNames, updateTodosToolName)
	cacheAffinityID := childCacheAffinityID(runtime.CacheAffinityID, childID)
	if continuation != nil {
		cacheAffinityID = continuation.state.CacheAffinityID
	}
	childTools, err := r.buildChildTools(runtime, launch, childID, childTodos, toolNames, cacheAffinityID)
	if err != nil {
		terminalErr = err
		finish()
		return result, err
	}
	child := agent.New(launch.Provider, childTools, agent.Options{
		MaxTurns:                  maxTurns,
		MaxPromptTokens:           runtime.MaxPromptTokens,
		MaxOutputTokens:           launch.MaxOutputTokens,
		MaxPromptCostUSD:          runtime.MaxPromptCostUSD,
		Model:                     launch.Model,
		ContextWindow:             launch.ContextWindow,
		Registry:                  launch.Registry,
		Reasoning:                 launch.Reasoning,
		ServerTools:               launch.ServerTools,
		ResponsesStateful:         launch.ResponsesStateful,
		RetentionPolicy:           r.opts.RetentionPolicy,
		CompactKeepTurns:          r.opts.CompactKeepTurns,
		CompactKeepTokens:         r.opts.CompactKeepTokens,
		CompactTriggerPercent:     r.opts.CompactTriggerPercent,
		CompactTargetPercent:      r.opts.CompactTargetPercent,
		DisableAutoCompaction:     r.opts.DisableAutoCompaction,
		CompactSummaryMaxTokens:   r.opts.CompactSummaryMaxTokens,
		CompactToolResultMaxBytes: r.opts.CompactToolResultMaxBytes,
		Now:                       r.opts.Now,
	})
	child.SetSystem(launch.System)
	child.SetCacheAffinityID(cacheAffinityID)
	prompt := req.Task
	if continuation != nil {
		prompt = continuationPrompt(req.ContinueChildID, req.Task)
		childTodos.Restore(continuation.state.Todos)
		child.SetTranscript(continuation.state.Messages)
	}

	// One tree per child run keeps tree.ndjson identity stable across the
	// per-closed-turn checkpoints and the final consolidated save.
	var childTree *session.Tree
	ensureChildTree := func(messages []llm.Message) (*session.Tree, error) {
		if childTree == nil {
			tree, err := session.LinearTree(created, "", messages)
			if err != nil {
				return nil, err
			}
			childTree = tree
		}
		return childTree, nil
	}

	sink := newChildSink(childDir, childTodos, hasTodoTool, progress, activity, inlineReasoningEnabled(launch.Reasoning))
	sink.messageCount = func() int { return len(child.Transcript()) }
	sink.checkpoint = func(checkpoint agent.PromptCheckpoint) error {
		updated := r.now()
		tree, err := ensureChildTree(child.Transcript())
		if err != nil {
			return err
		}
		state := r.childSessionState(runtime, launch, child, childTodos, checkpoint.Usage, created, updated, tree)
		var checkpointErr error
		switch checkpoint.Kind {
		case agent.PromptCheckpointClosedTurn:
			checkpointErr = session.SaveClosedTurnCheckpoint(childDir, state, 1, checkpoint.Turn)
			if checkpointErr == nil {
				_, checkpointErr = r.saveChildMeta(
					runtime,
					launch,
					childID,
					req,
					maxTurns,
					runtimeFingerprint,
					session.ChildStatusRunning,
					created,
					updated,
					checkpoint.Usage,
					nil,
					len(child.Transcript()),
				)
			}
		default:
			checkpointErr = session.SaveActiveTurnCheckpoint(
				childDir,
				state,
				string(checkpoint.Kind),
				1,
				checkpoint.Turn,
			)
		}
		return checkpointErr
	}
	child.SetCompactionArchiver(func(_ context.Context, archive agent.CompactionArchive) (string, error) {
		ref, err := session.SaveCompaction(childDir, session.Compaction{
			Time:          r.now(),
			Summary:       archive.Summary,
			Usage:         archive.Usage,
			Messages:      archive.Messages,
			Focus:         archive.Focus,
			ReadFiles:     archive.ReadFiles,
			ModifiedFiles: archive.ModifiedFiles,
		})
		if err == nil {
			childTodos.RequireRequestContext()
		}
		return ref, err
	})
	flushDisplay = sink.flushDisplay
	sink.User(prompt)

	failBeforePrompt := func(runErr error) (RunResult, error) {
		terminalUsage = sink.usage
		terminalMessageCount = len(child.Transcript())
		terminalUpdated = r.now()
		stateErr := r.saveChildSession(runtime, launch, childID, child, childTodos, terminalUsage, created, terminalUpdated, ensureChildTree)
		persistenceErr := errors.Join(sink.appendError(), stateErr)
		terminalErr = errors.Join(runErr, persistenceErr)
		result.Usage = terminalUsage.Usage
		result.Turns = terminalUsage.Turns
		result.TerminationReason = terminalUsage.TerminationReason
		result.ContinuationMode = req.continuationMode
		result.Agent = launch.Agent
		result.ProviderName = launch.ProviderName
		result.Model = launch.Model
		result.Progress = progress.Closure()
		result.SaveError = errors.Join(result.SaveError, persistenceErr)
		finish()
		return result, terminalErr
	}

	if continuation != nil {
		requestContext := todo.RequestContext(continuation.state.Todos)
		before := estimateContinuationContext(child, continuation.state.Messages, prompt, requestContext)
		req.continuationContextBefore = before.Total
		req.continuationContextAfter = before.Total
		req.continuationContextWindow = before.Window
		if before.Window <= 0 {
			return failBeforePrompt(continuationContextError(req.ContinueChildID, before))
		}
		if before.Total*100 > before.Window*continuationContextPercent {
			compactUsage, changed, compactErr := child.CompactForContinuation(ctx, sink)
			sink.addPreflightMaintenance("continuation_compaction", compactUsage)
			if compactErr != nil {
				return failBeforePrompt(continuationIncompatibleError(
					req.ContinueChildID,
					fmt.Sprintf("could not create a compact continuation checkpoint: %v", compactErr),
				))
			}
			if !changed {
				return failBeforePrompt(continuationIncompatibleError(
					req.ContinueChildID,
					"could not create a compact continuation checkpoint",
				))
			}
			req.continuationMode = continuationModeCheckpoint
			checkpoint := child.Transcript()
			after := estimateContinuationContext(child, checkpoint, prompt, requestContext)
			req.continuationContextAfter = after.Total
			req.continuationContextWindow = after.Window
			if after.Window <= 0 || after.Total*100 > after.Window*continuationContextPercent {
				return failBeforePrompt(continuationCheckpointContextError(req.ContinueChildID, after))
			}
		} else {
			req.continuationMode = continuationModeRetained
			child.SetProxySessionID(continuation.state.ProxySessionID)
			child.SetResponseState(continuation.state.ResponseState)
		}
		result.ContinuationMode = req.continuationMode
	}

	runErr := child.RunPrompt(ctx, prompt, sink)
	usage := sink.usage
	terminalUsage = usage
	terminalMessageCount = len(child.Transcript())
	terminalStatus = childTerminalStatus(runErr)
	terminalUpdated = r.now()
	stateErr := r.saveChildSession(runtime, launch, childID, child, childTodos, usage, created, terminalUpdated, ensureChildTree)
	persistenceErr := errors.Join(sink.appendError(), stateErr)
	result = RunResult{
		Usage:             usage.Usage,
		Turns:             usage.Turns,
		EffectiveMaxTurns: maxTurns,
		TerminationReason: usage.TerminationReason,
		Mode:              req.Mode,
		ContinuedFrom:     req.ContinueChildID,
		ContinuationMode:  req.continuationMode,
		ChildID:           childID,
		TranscriptPath:    childDir,
		Agent:             launch.Agent,
		ProviderName:      launch.ProviderName,
		Model:             launch.Model,
		SaveError:         errors.Join(result.SaveError, persistenceErr),
		Progress:          progress.Closure(),
	}
	terminalErr = errors.Join(runErr, persistenceErr)
	finish()
	if runErr != nil {
		return result, runErr
	}
	report := strings.TrimSpace(lastAssistantText(child.Transcript()))
	if report == "" {
		report = "(delegate completed without a final text response)"
	}
	report += fmt.Sprintf("\n\n[delegate: %s, turn budget %d", turnPhrase(usage.Turns), maxTurns)
	if req.Mode != "" {
		report += ", mode " + req.Mode
	}
	if req.ContinueChildID != "" {
		report += ", continued from " + req.ContinueChildID
		if req.continuationMode == continuationModeCheckpoint {
			report += " via compact checkpoint"
		}
	}
	report += fmt.Sprintf(", termination %s, %d input tokens, %d output tokens",
		usage.TerminationReason, usage.Usage.InputTokens, usage.Usage.OutputTokens)
	if childDir != "" {
		report += fmt.Sprintf(", transcript %s", childDir)
	}
	if result.SaveError != nil {
		report += fmt.Sprintf(", transcript save failed: %v", result.SaveError)
	}
	report += "]"
	result.Report = report
	return result, nil
}

type preparedRun struct {
	req          RunRequest
	runtime      Runtime
	maxTurns     int
	continuation *continuationSource
}

type continuationSource struct {
	meta  session.ChildMeta
	state session.Session
}

func (r *Runner) prepareRun(req RunRequest) (preparedRun, error) {
	if r == nil {
		return preparedRun{}, fmt.Errorf("delegate runner is not initialized")
	}
	if req.Kind == "" {
		req.Kind = "delegate"
	}
	req.Kind = strings.TrimSpace(req.Kind)
	if req.Kind == "" {
		req.Kind = "delegate"
	}
	req.Task = strings.TrimSpace(req.Task)
	if req.Task == "" {
		return preparedRun{}, fmt.Errorf("task is required")
	}
	var err error
	req.Mode, err = normalizeMode(req.Mode)
	if err != nil {
		return preparedRun{}, err
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.ContinueChildID = strings.TrimSpace(req.ContinueChildID)
	req.ChildID = strings.TrimSpace(req.ChildID)
	req.ResourceKey = strings.TrimSpace(req.ResourceKey)
	req.Access = strings.TrimSpace(req.Access)
	if !req.Background && !req.leaseAcquired && (req.ResourceKey != "" || req.Access != "") {
		return preparedRun{}, fmt.Errorf("scope and access require background:true")
	}
	if r.snapshot == nil {
		return preparedRun{}, fmt.Errorf("delegate runtime is not initialized")
	}
	runtime := r.snapshot()
	if err := r.validateDepth(runtime); err != nil {
		return preparedRun{}, err
	}
	if runtime.Provider == nil {
		return preparedRun{}, fmt.Errorf("delegate runtime is not initialized")
	}

	var continuation *continuationSource
	if req.ContinueChildID != "" {
		continuation, err = loadContinuationSource(runtime, req.ContinueChildID)
		if err != nil {
			return preparedRun{}, err
		}
		if continuation.meta.Kind != req.Kind {
			return preparedRun{}, continuationIncompatibleError(
				req.ContinueChildID,
				fmt.Sprintf("saved kind %q does not match requested kind %q", continuation.meta.Kind, req.Kind),
			)
		}
		sourceMode, err := normalizeMode(continuation.meta.Mode)
		if err != nil {
			return preparedRun{}, continuationIncompatibleError(req.ContinueChildID, "saved mode is invalid")
		}
		switch {
		case req.Mode == "":
			req.Mode = sourceMode
		case req.Mode != sourceMode:
			return preparedRun{}, continuationIncompatibleError(
				req.ContinueChildID,
				fmt.Sprintf("mode %q does not match saved mode %q", req.Mode, sourceMode),
			)
		}
		switch {
		case req.Agent == "":
			req.Agent = continuation.meta.RequestedAgent
		case req.Agent != continuation.meta.Agent:
			return preparedRun{}, continuationIncompatibleError(
				req.ContinueChildID,
				fmt.Sprintf("agent %q does not match saved agent %q", req.Agent, continuation.meta.Agent),
			)
		}
		if continuation.meta.EffectiveMaxTurns <= 0 {
			return preparedRun{}, continuationIncompatibleError(req.ContinueChildID, "saved turn budget is unavailable")
		}
		switch {
		case req.MaxTurns == nil:
			inherited := continuation.meta.EffectiveMaxTurns
			req.MaxTurns = &inherited
			req.maxTurnsInherited = true
		case *req.MaxTurns != continuation.meta.EffectiveMaxTurns:
			return preparedRun{}, continuationIncompatibleError(
				req.ContinueChildID,
				fmt.Sprintf("turn budget %d does not match saved budget %d", *req.MaxTurns, continuation.meta.EffectiveMaxTurns),
			)
		}
	}
	maxTurns, err := r.maxTurns(req.MaxTurns)
	if err != nil {
		if continuation != nil {
			return preparedRun{}, continuationIncompatibleError(req.ContinueChildID, err.Error())
		}
		return preparedRun{}, err
	}
	if req.Background {
		if req.Mode == ModeImplementation {
			req.Access = tools.BackgroundAccessExclusive
		} else if req.Access == "" {
			req.Access = r.defaultWorkspaceAccess(runtime, req, continuation)
		}
	}
	return preparedRun{
		req:          req,
		runtime:      runtime,
		maxTurns:     maxTurns,
		continuation: continuation,
	}, nil
}

func (r *Runner) defaultWorkspaceAccess(runtime Runtime, req RunRequest, continuation *continuationSource) string {
	if continuation != nil {
		switch continuation.meta.Access {
		case tools.BackgroundAccessReadOnly, tools.BackgroundAccessExclusive:
			return continuation.meta.Access
		}
	}
	agentName := req.Agent
	if agentName == "" {
		agentName = runtime.Agent
	}
	if r.opts.AgentCandidates != nil {
		for _, candidate := range r.opts.AgentCandidates(runtime) {
			if candidate.Name == agentName && candidate.WorkspaceAccess == tools.BackgroundAccessReadOnly {
				return tools.BackgroundAccessReadOnly
			}
		}
	}
	return tools.BackgroundAccessExclusive
}

func loadContinuationSource(runtime Runtime, childID string) (*continuationSource, error) {
	if runtime.SessionPath == "" {
		return nil, fmt.Errorf("delegate continuation requires an active persisted session")
	}
	if err := validateContinuationChildID(childID); err != nil {
		return nil, err
	}
	dir := session.ChildSessionDir(runtime.SessionPath, childID)
	metaPath := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("delegate continuation %q: read metadata: %w", childID, err)
	}
	var meta session.ChildMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("delegate continuation %q: decode metadata: %w", childID, err)
	}
	if meta.ID != childID {
		return nil, fmt.Errorf("delegate continuation %q: metadata id is %q", childID, meta.ID)
	}
	if meta.ParentID != runtime.ParentChildID {
		return nil, fmt.Errorf(
			"delegate continuation %q belongs to parent %q, not current parent %q",
			childID,
			meta.ParentID,
			runtime.ParentChildID,
		)
	}
	switch meta.Status {
	case session.ChildStatusCompleted, session.ChildStatusFailed, session.ChildStatusCanceled, session.ChildStatusAbandoned:
	default:
		return nil, fmt.Errorf("delegate continuation %q is not terminal (status %q)", childID, meta.Status)
	}
	if meta.RuntimeFingerprint == "" {
		return nil, continuationIncompatibleError(childID, "saved runtime fingerprint is unavailable")
	}
	state, err := session.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("delegate continuation %q: load resumable state: %w", childID, err)
	}
	if err := llm.ValidateTranscript(state.Messages); err != nil {
		return nil, fmt.Errorf("delegate continuation %q: invalid saved transcript: %w", childID, err)
	}
	if state.Provider != meta.Provider || state.Model != meta.Model || state.Agent != meta.Agent {
		return nil, continuationIncompatibleError(childID, "saved state identity does not match child metadata")
	}
	if state.ProxySessionID == "" || state.CacheAffinityID == "" {
		return nil, continuationIncompatibleError(childID, "saved runtime identifiers are unavailable")
	}
	if state.ResponseState != nil &&
		(state.ResponseState.PreviousResponseID == "" ||
			state.ResponseState.AnchorMessages < 0 ||
			state.ResponseState.AnchorMessages > len(state.Messages) ||
			!llm.MatchesMessageFingerprint(
				state.Messages[:state.ResponseState.AnchorMessages],
				state.ResponseState.AnchorDigest,
			)) {
		return nil, continuationIncompatibleError(childID, "saved provider continuation anchor is invalid")
	}
	return &continuationSource{meta: meta, state: state}, nil
}

func validateContinuationChildID(childID string) error {
	if childID == "" {
		return fmt.Errorf("continue_child_id must not be empty")
	}
	for _, r := range childID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("continue_child_id %q contains unsupported characters", childID)
	}
	return nil
}

func continuationIncompatibleError(childID, reason string) error {
	return fmt.Errorf("delegate continuation %q is incompatible: %s; start a fresh delegate", childID, reason)
}

func continuationPrompt(childID, task string) string {
	return fmt.Sprintf(
		"[delegate continuation from %s]\nContinue from the retained transcript and state. Re-check current repository state before relying on earlier observations, then address:\n\n%s",
		childID,
		strings.TrimSpace(task),
	)
}

func estimateContinuationContext(child *agent.Agent, messages []llm.Message, prompt, requestContext string) agent.ContextEstimate {
	if requestContext != "" {
		prompt += "\n\n" + requestContext
	}
	probe := append([]llm.Message(nil), messages...)
	probe = append(probe, llm.Message{
		Role:   llm.RoleUser,
		Origin: llm.MessageOriginPrompt,
		Content: []llm.ContentBlock{{
			Kind: llm.BlockText,
			Text: prompt,
		}},
	})
	child.SetTranscript(probe)
	estimate := child.EstimateContext()
	child.SetTranscript(messages)
	return estimate
}

func addDelegateUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:        a.InputTokens + b.InputTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:            a.CostUSD + b.CostUSD,
		CostKnown:          aggregateDelegateCostKnown(a, b),
	}
}

func aggregateDelegateCostKnown(a, b llm.Usage) bool {
	switch {
	case delegateUsageHasTokens(a) && !a.CostKnown:
		return false
	case delegateUsageHasTokens(b) && !b.CostKnown:
		return false
	default:
		return a.CostKnown || b.CostKnown
	}
}

func delegateUsageHasTokens(usage llm.Usage) bool {
	return usage.InputTokens != 0 ||
		usage.OutputTokens != 0 ||
		usage.CacheReadTokens != 0 ||
		usage.CacheWriteTokens != 0 ||
		usage.CacheWrite1hTokens != 0 ||
		usage.ReasoningTokens != 0
}

func continuationContextError(childID string, estimate agent.ContextEstimate) error {
	if estimate.Window <= 0 {
		return continuationIncompatibleError(childID, "current context window is unavailable")
	}
	percent := (estimate.Total*100 + estimate.Window - 1) / estimate.Window
	return continuationIncompatibleError(
		childID,
		fmt.Sprintf(
			"retained context is about %d%% of the window, above the %d%% continuation limit",
			percent,
			continuationContextPercent,
		),
	)
}

func continuationCheckpointContextError(childID string, estimate agent.ContextEstimate) error {
	if estimate.Window <= 0 {
		return continuationIncompatibleError(childID, "current context window is unavailable")
	}
	percent := (estimate.Total*100 + estimate.Window - 1) / estimate.Window
	return continuationIncompatibleError(
		childID,
		fmt.Sprintf(
			"compact checkpoint is about %d%% of the window, above the %d%% continuation limit",
			percent,
			continuationContextPercent,
		),
	)
}

func (r *Runner) runtimeFingerprint(runtime Runtime, launch Launch, req RunRequest, maxTurns int, toolNames []string) (string, error) {
	toolRegistry, err := launch.Tools.Subset(toolNames)
	if err != nil {
		return "", fmt.Errorf("delegate runtime fingerprint: %w", err)
	}
	providerImplementation := ""
	if launch.Provider != nil {
		providerImplementation = launch.Provider.Name()
	}
	contextWindow := launch.ContextWindow
	if contextWindow <= 0 {
		contextWindow = launch.Registry.ContextWindow(launch.Model)
	}
	fingerprint := struct {
		Version                int                   `json:"version"`
		Provider               string                `json:"provider"`
		ProviderImplementation string                `json:"provider_implementation"`
		Model                  string                `json:"model"`
		Agent                  string                `json:"agent"`
		RequestedAgent         string                `json:"requested_agent,omitempty"`
		Mode                   string                `json:"mode,omitempty"`
		MaxTurns               int                   `json:"max_turns"`
		Depth                  int                   `json:"depth"`
		MaxDepth               int                   `json:"max_depth"`
		ContextWindow          int                   `json:"context_window"`
		MaxOutputTokens        int                   `json:"max_output_tokens"`
		ModelOutputLimit       int                   `json:"model_output_limit"`
		MaxPromptTokens        int                   `json:"max_prompt_tokens"`
		MaxPromptCostUSD       float64               `json:"max_prompt_cost_usd"`
		Reasoning              llm.ReasoningConfig   `json:"reasoning"`
		ServerTools            []llm.ServerTool      `json:"server_tools,omitempty"`
		ResponsesStateful      bool                  `json:"responses_stateful"`
		RetentionPolicy        agent.RetentionPolicy `json:"retention_policy"`
		System                 string                `json:"system"`
		Tools                  []llm.ToolSchema      `json:"tools"`
		Compaction             struct {
			KeepTurns          int  `json:"keep_turns"`
			KeepTokens         int  `json:"keep_tokens"`
			TriggerPercent     int  `json:"trigger_percent"`
			TargetPercent      int  `json:"target_percent"`
			Disabled           bool `json:"disabled"`
			SummaryMaxTokens   int  `json:"summary_max_tokens"`
			ToolResultMaxBytes int  `json:"tool_result_max_bytes"`
		} `json:"compaction"`
	}{
		Version:                continuationFingerprintVersion,
		Provider:               launch.ProviderName,
		ProviderImplementation: providerImplementation,
		Model:                  launch.Model,
		Agent:                  launch.Agent,
		RequestedAgent:         req.Agent,
		Mode:                   req.Mode,
		MaxTurns:               maxTurns,
		Depth:                  runtime.Depth + 1,
		MaxDepth:               r.maxDepth(),
		ContextWindow:          contextWindow,
		MaxOutputTokens:        launch.MaxOutputTokens,
		ModelOutputLimit:       launch.Registry.OutputLimit(launch.Model),
		MaxPromptTokens:        runtime.MaxPromptTokens,
		MaxPromptCostUSD:       runtime.MaxPromptCostUSD,
		Reasoning:              launch.Reasoning,
		ServerTools:            slices.Clone(launch.ServerTools),
		ResponsesStateful:      launch.ResponsesStateful,
		RetentionPolicy:        r.opts.RetentionPolicy,
		System:                 launch.System,
		Tools:                  toolRegistry.Specs(),
	}
	fingerprint.Compaction.KeepTurns = r.opts.CompactKeepTurns
	fingerprint.Compaction.KeepTokens = r.opts.CompactKeepTokens
	fingerprint.Compaction.TriggerPercent = r.opts.CompactTriggerPercent
	fingerprint.Compaction.TargetPercent = r.opts.CompactTargetPercent
	fingerprint.Compaction.Disabled = r.opts.DisableAutoCompaction
	fingerprint.Compaction.SummaryMaxTokens = r.opts.CompactSummaryMaxTokens
	fingerprint.Compaction.ToolResultMaxBytes = r.opts.CompactToolResultMaxBytes
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return "", fmt.Errorf("delegate runtime fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", ModeImplementation:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be %q when provided", ModeImplementation)
	}
}

func (r *Runner) buildChildTools(parent Runtime, launch Launch, childID string, todos *todo.Store, names []string, cacheAffinityID string) (*tools.Registry, error) {
	if r.childToolBuilder != nil {
		return r.childToolBuilder(parent, launch, childID, todos, names)
	}
	return r.childToolsWithCacheAffinity(parent, launch, childID, cacheAffinityID, todos, names)
}

func (r *Runner) childTools(parent Runtime, launch Launch, childID string, todos *todo.Store, names []string) (*tools.Registry, error) {
	return r.childToolsWithCacheAffinity(parent, launch, childID, childCacheAffinityID(parent.CacheAffinityID, childID), todos, names)
}

func (r *Runner) childToolsWithCacheAffinity(parent Runtime, launch Launch, childID, cacheAffinityID string, todos *todo.Store, names []string) (*tools.Registry, error) {
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
		Provider:          launch.Provider,
		ProviderName:      launch.ProviderName,
		Model:             launch.Model,
		ContextWindow:     launch.ContextWindow,
		MaxOutputTokens:   launch.MaxOutputTokens,
		Registry:          launch.Registry,
		Reasoning:         launch.Reasoning,
		ServerTools:       launch.ServerTools,
		ResponsesStateful: launch.ResponsesStateful,
		System:            launch.System,
		Agent:             launch.Agent,
		ToolNames:         names,
		SessionPath:       parent.SessionPath,
		CacheAffinityID:   cacheAffinityID,
		ParentChildID:     childID,
		Depth:             parent.Depth + 1,
		MaxPromptTokens:   parent.MaxPromptTokens,
		MaxPromptCostUSD:  parent.MaxPromptCostUSD,
		Build:             parent.Build,
		RuntimeProfile:    parent.RuntimeProfile,
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

func (r *Runner) validateDepth(runtime Runtime) error {
	maxDepth := r.maxDepth()
	if runtime.Depth >= maxDepth {
		return fmt.Errorf("delegate maximum depth %d reached at depth %d", maxDepth, runtime.Depth)
	}
	return nil
}

func (r *Runner) maxDepth() int {
	if r == nil || r.opts.MaxDepth <= 0 {
		return DefaultMaxDepth
	}
	return r.opts.MaxDepth
}

func (r *Runner) maxTurns(requested *int) (int, error) {
	cap := r.configuredMaxTurns()
	if requested == nil {
		return cap, nil
	}
	if *requested <= 0 {
		return 0, fmt.Errorf("max_turns must be positive")
	}
	if *requested > cap {
		return 0, fmt.Errorf("max_turns %d exceeds configured maximum %d", *requested, cap)
	}
	return *requested, nil
}

func (r *Runner) configuredMaxTurns() int {
	if r == nil || r.opts.MaxTurns <= 0 {
		return DefaultMaxTurns
	}
	return r.opts.MaxTurns
}

func (r *Runner) now() time.Time {
	if r != nil && r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

func turnPhrase(n int) string {
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

func childTerminalStatus(err error) string {
	if err == nil {
		return session.ChildStatusCompleted
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return session.ChildStatusCanceled
	}
	return session.ChildStatusFailed
}

func inlineReasoningEnabled(reasoning llm.ReasoningConfig) bool {
	switch strings.ToLower(strings.TrimSpace(reasoning.Summary)) {
	case "auto", "concise", "detailed":
		return true
	default:
		return false
	}
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

func childSystemPrompt(system string) string {
	child := prompts.DelegateChild()
	if strings.HasSuffix(strings.TrimSpace(system), child) {
		return strings.TrimSpace(system)
	}
	if strings.TrimSpace(system) == "" {
		return child
	}
	return strings.TrimSpace(system) + "\n\n" + child
}

func childBudgetSystemPrompt(system string, maxTurns int) string {
	return fmt.Sprintf(
		"%s\n\n[delegate budget]\nYour tool-enabled loop budget is exactly %d turns. If you exhaust it with another tool call, Harness may issue one additional tools-disabled wind-down request. Harness records why the loop stops; finish substantive work and verification within the stated budget.",
		strings.TrimSpace(system),
		maxTurns,
	)
}

func withoutChildBudget(system string) string {
	const marker = "\n\n[delegate budget]\n"
	if index := strings.LastIndex(system, marker); index >= 0 {
		return strings.TrimSpace(system[:index])
	}
	return strings.TrimSpace(system)
}

func implementationSystemPrompt(system string) string {
	return fmt.Sprintf(
		"%s\n\n[implementation mode]\nThis is scoped mutating implementation work. Make the requested changes, verify them, and return an exact handoff with changed paths, checks run, and any remaining work. Commit only when commit ownership was explicitly delegated.",
		strings.TrimSpace(system),
	)
}

func withoutTool(names []string, excluded string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != excluded {
			out = append(out, name)
		}
	}
	return out
}

// MissingTools returns required tool names that are not available, preserving
// the required order and de-duplicating repeated names. An available git tool
// satisfies a required git_readonly: git is a strict superset of the read-only
// subcommands, so a parent with git can delegate to a read-only agent that needs
// only git_readonly. The reverse does not hold — git_readonly does not satisfy a
// required git.
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
		if name == "git_readonly" && have["git"] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	return missing
}

// DelegatableAgentCandidates returns compatible, described candidates in
// deterministic name order. Compatibility means the candidate's tools are a
// subset of the current parent's active tools.
func DelegatableAgentCandidates(available []string, candidates []AgentCandidate) []AgentCandidate {
	out := make([]AgentCandidate, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		candidate.Description = normalizeAgentDescription(candidate.Description)
		if strings.TrimSpace(candidate.Name) == "" || candidate.Description == "" || seen[candidate.Name] {
			continue
		}
		if len(MissingTools(candidate.ToolNames, available)) == 0 {
			seen[candidate.Name] = true
			out = append(out, candidate)
		}
	}
	slices.SortFunc(out, func(a, b AgentCandidate) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// DelegatableAgentNames returns the names of compatible, described agents in
// deterministic order.
func DelegatableAgentNames(available []string, candidates []AgentCandidate) []string {
	compatible := DelegatableAgentCandidates(available, candidates)
	names := make([]string, len(compatible))
	for i, candidate := range compatible {
		names[i] = candidate.Name
	}
	return names
}

func normalizeAgentDescription(description string) string {
	description = strings.Join(strings.Fields(description), " ")
	if len(description) <= maxAgentDescriptionBytes {
		return description
	}
	keep := maxAgentDescriptionBytes - len("...")
	for keep > 0 && !utf8.ValidString(description[:keep]) {
		keep--
	}
	return strings.TrimSpace(description[:keep]) + "..."
}

func schema(agents []AgentCandidate, maxTurns int) json.RawMessage {
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	agentDescription := "Agent; defaults to the current one."
	agentNames := make([]string, 0, len(agents))
	if len(agents) > 0 {
		var catalog strings.Builder
		catalog.WriteString(" Available:")
		for _, candidate := range agents {
			agentNames = append(agentNames, candidate.Name)
			catalog.WriteString("\n- ")
			catalog.WriteString(candidate.Name)
			catalog.WriteString(": ")
			catalog.WriteString(candidate.Description)
		}
		agentDescription += catalog.String()
	}
	agent := map[string]any{
		"type":        "string",
		"description": agentDescription,
	}
	if len(agentNames) > 0 {
		agent["enum"] = agentNames
	}
	properties := map[string]any{
		"task": map[string]any{
			"type":        "string",
			"description": "Self-contained child prompt: objective, scope, constraints, report, and verification.",
		},
		"agent": agent,
		"mode": map[string]any{
			"type":        "string",
			"enum":        []string{ModeImplementation},
			"description": "Optional implementation mode for scoped mutating work; instructs the child to implement, verify, and report an exact handoff. Omit for exploration and review.",
		},
		"max_turns": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     maxTurns,
			"description": fmt.Sprintf("Optional turn budget; defaults to and cannot exceed the configured maximum of %d.", maxTurns),
		},
		"continue_child_id": map[string]any{
			"type":        "string",
			"description": "Optional terminal sibling child ID to continue in a fresh child. Harness requires the same parent, agent, mode, turn budget, and runtime fingerprint plus resumable state below the context-pressure limit.",
		},
		"background": map[string]any{
			"type":        "boolean",
			"description": "Only independent, non-overlapping work while parent work remains; Harness joins automatically, so do not poll or duplicate.",
		},
		"scope": map[string]any{
			"type":        "string",
			"description": "Background-only workspace path scope. Defaults to the process cwd; give mutating sibling delegates distinct paths so they can run concurrently.",
		},
		"access": map[string]any{
			"type":        "string",
			"enum":        []string{tools.BackgroundAccessReadOnly, tools.BackgroundAccessExclusive},
			"description": "Background-only lease access. Defaults from the selected agent (explore/plan read_only; auto/independent exclusive); override only when the task contract is stricter.",
		},
	}
	body := map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"task"},
	}
	b, _ := json.Marshal(body)
	return b
}

// childCacheAffinityID derives a prompt-cache affinity key for a delegate child
// that is distinct from its parent's and from sibling delegates, yet stable for
// the child's whole multi-turn run. A child has a different system prompt and
// tool subset than its parent, so it never reads the parent's cached prefix;
// sharing the parent's routing key would only risk thrashing the same cache
// shard when a parent and concurrent delegates interleave requests. Deriving
// from the (fixed) childID keeps every turn of one child on the same key while
// routing each child to its own shard. The empty parent key still yields a
// per-child-distinct value.
func childCacheAffinityID(parentID, childID string) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + childID))
	return "harness-cache-" + hex.EncodeToString(sum[:])
}

func nextChildID(kind string) string {
	prefix := "child"
	switch strings.TrimSpace(kind) {
	case "delegate":
		prefix = "delegate"
	}
	return fmt.Sprintf("%s_%s_%06d", prefix, time.Now().UTC().Format("20060102T150405Z"), childSeq.Add(1))
}

func (r *Runner) saveChildMeta(parent Runtime, launch Launch, childID string, req RunRequest, effectiveMaxTurns int, runtimeFingerprint, status string, created, updated time.Time, usage agent.PromptUsage, runErr error, messageCount int) (string, error) {
	if parent.SessionPath == "" {
		return "", nil
	}
	meta := session.ChildMeta{
		ID:                 childID,
		ParentID:           parent.ParentChildID,
		Kind:               req.Kind,
		Mode:               req.Mode,
		ContinuedFrom:      req.ContinueChildID,
		ContinuationMode:   req.continuationMode,
		ContinuationBefore: req.continuationContextBefore,
		ContinuationAfter:  req.continuationContextAfter,
		ContinuationWindow: req.continuationContextWindow,
		RuntimeFingerprint: runtimeFingerprint,
		Agent:              launch.Agent,
		RequestedAgent:     req.Agent,
		ResourceKey:        req.ResourceKey,
		Access:             req.Access,
		Provider:           launch.ProviderName,
		Model:              launch.Model,
		Build:              parent.Build,
		Runtime:            parent.RuntimeProfile,
		Status:             status,
		TaskPreview:        preview(req.Task, 240),
		Created:            created,
		Updated:            updated,
		Usage:              usage.Usage,
		MessageCount:       messageCount,
		EffectiveMaxTurns:  effectiveMaxTurns,
		TurnsUsed:          usage.Turns,
		TerminationReason:  string(usage.TerminationReason),
	}
	if req.MaxTurns != nil && !req.maxTurnsInherited {
		requested := *req.MaxTurns
		meta.RequestedMaxTurns = &requested
	}
	if meta.TerminationReason == "" && status != session.ChildStatusRunning {
		switch {
		case status == session.ChildStatusCompleted:
			meta.TerminationReason = string(agent.TerminationModelCompleted)
		case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
			meta.TerminationReason = string(agent.TerminationCancelled)
		default:
			meta.TerminationReason = string(agent.TerminationError)
		}
	}
	if runErr != nil {
		meta.Error = runErr.Error()
	}
	return session.SaveChildMeta(parent.SessionPath, meta)
}

func (r *Runner) saveChildSession(parent Runtime, launch Launch, childID string, child *agent.Agent, todos *todo.Store, usage agent.PromptUsage, created, updated time.Time, ensureTree func([]llm.Message) (*session.Tree, error)) error {
	if parent.SessionPath == "" {
		return nil
	}
	childDir := session.ChildSessionDir(parent.SessionPath, childID)
	tree, err := ensureTree(child.Transcript())
	if err != nil {
		return err
	}
	return r.childSessionState(parent, launch, child, todos, usage, created, updated, tree).SaveConsolidated(childDir)
}

func (r *Runner) childSessionState(parent Runtime, launch Launch, child *agent.Agent, todos *todo.Store, usage agent.PromptUsage, created, updated time.Time, tree *session.Tree) session.Session {
	return session.Session{
		Version:         session.Version,
		Provider:        launch.ProviderName,
		Model:           launch.Model,
		Created:         created,
		Updated:         updated,
		Build:           parent.Build,
		Runtime:         parent.RuntimeProfile,
		System:          launch.System,
		Agent:           launch.Agent,
		ProxySessionID:  child.ProxySessionID(),
		CacheAffinityID: child.CacheAffinityID(),
		Prompt:          1,
		Messages:        child.Transcript(),
		ResponseState:   child.ResponseState(),
		Todos:           todos.Snapshot(),
		Usage:           session.UsageTotals{Usage: usage.Usage, CostUSD: usage.Usage.CostUSD},
		Tree:            tree,
	}
}

func preview(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

type childSink struct {
	usage                agent.PromptUsage
	preflightMaintenance llm.Usage
	progress             *Progress // compatibility live progress; may be nil
	activity             *ActivityRegistration
	assistant            *inlineLineAccumulator
	reasoning            bool
	sessionDir           string
	events               *session.EventAppender
	todos                *todo.Store
	todoContext          bool
	pending              map[string]pendingChildTool
	turn                 int
	attempt              int
	todoTurn             int
	appendMu             sync.Mutex
	appendErr            error
	checkpoint           func(agent.PromptCheckpoint) error
	messageCount         func() int
}

type pendingChildTool struct {
	call    llm.ToolCall
	summary string
	started time.Time
}

func newChildSink(sessionDir string, todos *todo.Store, todoContext bool, progress *Progress, activity *ActivityRegistration, reasoning ...bool) *childSink {
	sink := &childSink{
		sessionDir:  sessionDir,
		events:      session.NewEventAppender(sessionDir),
		todos:       todos,
		todoContext: todoContext,
		progress:    progress,
		activity:    activity,
		pending:     make(map[string]pendingChildTool),
	}
	if len(reasoning) > 0 {
		sink.reasoning = reasoning[0]
	}
	if activity.hasFeed() {
		sink.assistant = newInlineLineAccumulator(activityChunkMaxBytes, func(text string, continuation bool) {
			sink.activity.publishText(ActivityEventAssistant, text, sink.turn, sink.attempt, continuation)
		})
	}
	return sink
}

// Progress is a lock-protected snapshot of one delegate run's live activity,
// reported by the child sink to the parent renderer's wait ticker. It is
// best-effort diagnostic state; it is never persisted and never fed to the
// model. The ticker goroutine reads it from another goroutine while the child
// agent writes it from the run goroutine, so every read/write is guarded by
// the progress RWMutex. Snapshot returns a copy safe for the renderer to keep.
type Progress struct {
	mu       sync.RWMutex
	turn     int
	attempt  int
	agent    string
	ctx      agent.ContextEstimate
	tools    int       // count of ToolStart calls seen so far
	usage    llm.Usage // last TurnAttemptComplete usage
	finished bool
}

func NewProgress() *Progress { return &Progress{} }

// Snapshot returns a renderer-safe copy of the current live activity.
func (p *Progress) Snapshot() agent.DelegateProgressSnapshot {
	if p == nil {
		return agent.DelegateProgressSnapshot{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return agent.DelegateProgressSnapshot{
		Turn:     p.turn,
		Attempt:  p.attempt,
		Tools:    p.tools,
		Agent:    p.agent,
		Context:  p.ctx,
		Usage:    p.usage,
		Finished: p.finished,
	}
}

// SetAgent records the child agent name for the background summary label.
func (p *Progress) SetAgent(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agent = name
}

// markTurn records the current turn/attempt and context estimate.
func (p *Progress) markTurn(turn, attempt int, ctx agent.ContextEstimate) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turn = turn
	p.attempt = attempt
	p.ctx = ctx
}

// markUsage records the last per-attempt usage.
func (p *Progress) markUsage(u llm.Usage) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usage = u
}

// markContext refreshes the context estimate (e.g. from a completed turn).
func (p *Progress) markContext(ctx agent.ContextEstimate) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctx = ctx
}

// markTool increments the count of ToolStart calls seen.
func (p *Progress) markTool() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools++
}

// markFinished signals the run has returned (success or failure) so the renderer
// can render the final snapshot once before the wait clears.
func (p *Progress) markFinished() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finished = true
}

// Closure returns a func() agent.DelegateProgressSnapshot that reads this
// progress. It is the opaque `any` carried through tools/background to the
// renderer (which type-asserts it back). nil progress yields a zero snapshot.
func (p *Progress) Closure() func() agent.DelegateProgressSnapshot {
	return func() agent.DelegateProgressSnapshot { return p.Snapshot() }
}

func (s *childSink) User(text string) {
	s.append(session.Event{Type: session.EventUser, Prompt: 1, Text: text})
}

func (s *childSink) TextDelta(text string) {
	s.append(session.Event{Type: session.EventAssistantDelta, Prompt: 1, Turn: s.turn, Attempt: s.attempt, Text: text})
	s.activity.MarkActivity("replying")
	s.assistant.Write(text)
}

func (s *childSink) AssistantPhase(phase string) {
	if phase == "" || !llm.ValidAssistantPhase(phase) {
		return
	}
	s.flushDisplay()
	s.append(session.Event{Type: session.EventAssistantPhase, Prompt: 1, Turn: s.turn, Attempt: s.attempt, Phase: phase})
}

func (s *childSink) ReasoningSummary(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.flushDisplay()
	s.activity.MarkActivity("thinking")
	s.append(session.Event{Type: session.EventReasoningSummary, Prompt: 1, Turn: s.turn, Attempt: s.attempt, Text: text})
	if !s.reasoning || !s.activity.hasFeed() {
		return
	}
	accumulator := newInlineLineAccumulator(activityChunkMaxBytes, func(line string, continuation bool) {
		s.activity.publishText(ActivityEventReasoning, line, s.turn, s.attempt, continuation)
	})
	accumulator.Write(text)
	accumulator.Flush()
}

func (s *childSink) TurnAttemptStart(turn, attempt int, ctx agent.ContextEstimate) {
	if s.todos != nil && s.todoContext {
		s.todos.CommitModelRound(turn != s.todoTurn)
		s.todoTurn = turn
	}
	s.flushDisplay()
	s.turn = turn
	s.attempt = attempt
	s.progress.markTurn(turn, attempt, ctx)
	s.activity.MarkTurn(turn, attempt, ctx)
	s.append(session.Event{Type: session.EventTurnAttemptStart, Prompt: 1, Turn: turn, Attempt: attempt, Context: childContextSnapshot(ctx)})
	s.activity.publish(ActivityEvent{Kind: ActivityEventTurnStart, Turn: turn, Attempt: attempt})
}

func (s *childSink) TurnAttemptAbandoned(turn, attempt int) {
	s.flushDisplay()
	s.activity.MarkActivity(fmt.Sprintf("retrying after attempt %d", attempt))
	s.append(session.Event{
		Type:    session.EventTurnAttemptAbandoned,
		Prompt:  1,
		Turn:    turn,
		Attempt: attempt,
		Display: fmt.Sprintf("[turn: %d attempt %d discarded; retrying]", turn, attempt),
	})
	s.activity.publish(ActivityEvent{Kind: ActivityEventAttemptDiscarded, Turn: turn, Attempt: attempt})
}

func (s *childSink) TurnAttemptComplete(u agent.TurnAttemptUsage) {
	usage := u.Usage
	s.progress.markUsage(usage)
	s.activity.MarkUsage(usage)
	s.append(session.Event{Type: session.EventTurnAttemptUsage, Prompt: 1, Turn: u.Turn, Attempt: u.Attempt, Usage: &usage})
}

func (*childSink) ToolUseStart(llm.ToolCall) {}

func (*childSink) ToolUseDelta(int, string) {}

func (s *childSink) ModelRequestEvent(event llm.ModelRequestEvent) {
	kind, text, publish := safeModelRequestLine(event)
	if publish {
		s.flushDisplay()
	}
	if activity := safeModelRequestActivity(event); activity != "" {
		s.activity.MarkActivity(activity)
	}
	copyEvent := event
	s.append(session.Event{
		Type:         session.EventModelRequest,
		Prompt:       1,
		Turn:         s.turn,
		Attempt:      s.attempt,
		ModelRequest: &copyEvent,
	})
	if publish {
		s.activity.publishText(kind, text, s.turn, s.attempt, false)
	}
}

func (s *childSink) ToolStart(call llm.ToolCall) {
	s.flushDisplay()
	summary := safeToolActivity(call)
	s.pending[call.ID] = pendingChildTool{call: call, summary: summary, started: time.Now()}
	s.progress.markTool()
	s.activity.MarkActivity(summary)
	s.append(session.Event{Type: session.EventToolStart, Prompt: 1, Turn: s.turn, ToolID: call.ID, Tool: call.Name, Input: call.Input})
	s.activity.publishText(ActivityEventToolStart, summary, s.turn, s.attempt, false)
}

func (s *childSink) ToolResult(result llm.ToolResult) {
	s.flushDisplay()
	pending := s.pending[result.ForID]
	delete(s.pending, result.ForID)
	call := pending.call
	summary := pending.summary
	if summary == "" {
		summary = safeToolActivity(call)
	}
	display := fmt.Sprintf("[tool: %s completed]", call.Name)
	kind := ActivityEventToolComplete
	if result.IsError {
		display = fmt.Sprintf("[tool: %s error: %s]", call.Name, preview(firstLine(result.Text), 120))
		s.activity.MarkActivity("tool " + sanitizeRetainedText(call.Name, maxToolNameRunes) + " failed")
		kind = ActivityEventToolError
	} else {
		s.activity.MarkActivity("tool " + sanitizeRetainedText(call.Name, maxToolNameRunes) + " complete")
	}
	shownBytes := result.ShownBytes
	if shownBytes == 0 {
		shownBytes = len(result.Text)
	}
	originalBytes := result.OriginalBytes
	if originalBytes == 0 {
		originalBytes = shownBytes
	}
	var durationMS int64
	if !pending.started.IsZero() {
		durationMS = time.Since(pending.started).Milliseconds()
	}
	s.append(session.Event{
		Type:                session.EventToolResult,
		Prompt:              1,
		Turn:                s.turn,
		ToolID:              result.ForID,
		Tool:                call.Name,
		Display:             display,
		DurationMS:          durationMS,
		ResultError:         result.IsError,
		ResultTruncated:     result.Truncated,
		ResultOriginalBytes: originalBytes,
		ResultShownBytes:    shownBytes,
		ResultMetrics:       maps.Clone(result.Metrics),
	})
	s.activity.publishText(kind, summary, s.turn, s.attempt, false)
}

func (s *childSink) ArchiveToolResult(result llm.ToolResult) (agent.ToolResultArchive, error) {
	ref, err := session.SaveToolResultArtifact(s.sessionDir, 1, s.turn, result)
	if err != nil || ref == "" {
		return agent.ToolResultArchive{}, err
	}
	return agent.ToolResultArchive{
		DisplayPath: ref,
		ModelPath:   filepath.Join(s.sessionDir, ref),
	}, nil
}

func (s *childSink) Notice(msg string) {
	s.flushDisplay()
	s.append(session.Event{Type: session.EventNotice, Prompt: 1, Turn: s.turn, Display: msg})
	if text, ok := safeNoticeLine(msg); ok {
		s.activity.publishText(ActivityEventNotice, text, s.turn, s.attempt, false)
	}
}

func (s *childSink) TurnComplete(usage agent.TurnUsage) {
	u := usage.Usage
	s.progress.markContext(usage.Context)
	s.activity.MarkContext(usage.Context)
	s.append(session.Event{Type: session.EventTurnComplete, Prompt: 1, Turn: usage.Turn, Usage: &u})
}

func (s *childSink) MaintenanceComplete(usage agent.MaintenanceUsage) {
	u := usage.Usage
	s.append(session.Event{Type: session.EventMaintenanceUsage, Prompt: 1, Purpose: usage.Purpose, Usage: &u})
}

func (s *childSink) addPreflightMaintenance(purpose string, usage llm.Usage) {
	if usage == (llm.Usage{}) {
		return
	}
	s.preflightMaintenance = addDelegateUsage(s.preflightMaintenance, usage)
	s.usage.Usage = addDelegateUsage(s.usage.Usage, usage)
	s.usage.Maintenance = addDelegateUsage(s.usage.Maintenance, usage)
	s.MaintenanceComplete(agent.MaintenanceUsage{Purpose: purpose, Usage: usage})
}

func (s *childSink) PromptCheckpoint(checkpoint agent.PromptCheckpoint) {
	if s == nil || s.checkpoint == nil {
		return
	}
	checkpoint.Usage.Usage = addDelegateUsage(checkpoint.Usage.Usage, s.preflightMaintenance)
	checkpoint.Usage.Maintenance = addDelegateUsage(checkpoint.Usage.Maintenance, s.preflightMaintenance)
	started := time.Now()
	if err := s.checkpoint(checkpoint); err != nil {
		s.retainAppendError(err)
		return
	}
	messageCount := 0
	if s.messageCount != nil {
		messageCount = s.messageCount()
	}
	s.append(session.Event{
		Type:         session.EventCheckpoint,
		Prompt:       1,
		Turn:         checkpoint.Turn,
		Purpose:      string(checkpoint.Kind),
		DurationMS:   time.Since(started).Milliseconds(),
		MessageCount: messageCount,
	})
}

func (s *childSink) RetentionApplied(event agent.RetentionEvent) {
	s.append(session.Event{
		Type:   session.EventRetention,
		Prompt: 1,
		Turn:   s.turn + 1,
		Retention: &session.RetentionSnapshot{
			Policy:              event.Policy,
			Trigger:             event.Trigger,
			BlocksTrimmed:       event.BlocksTrimmed,
			BytesBefore:         event.BytesBefore,
			BytesAfter:          event.BytesAfter,
			ContextTokensBefore: event.ContextTokensBefore,
			ContextTokensAfter:  event.ContextTokensAfter,
			ResponseStateReset:  event.ResponseStateReset,
			NextRequestStateful: event.NextRequestStateful,
		},
	})
}

func (s *childSink) TranscriptRewritten() {
	if s.todos != nil && s.todoContext {
		s.todos.RequireRequestContext()
	}
}

func (s *childSink) SkillActivated(event agent.SkillActivationEvent) {
	s.append(session.Event{
		Type:    session.EventSkillActivation,
		Prompt:  1,
		Turn:    max(s.turn, 1),
		Purpose: event.Source,
		Summary: event.Status,
	})
}

func (s *childSink) RequestContext() []string {
	if s.todos == nil || !s.todoContext {
		return nil
	}
	ctx := s.todos.PendingRequestContext()
	if ctx == "" {
		return nil
	}
	return []string{ctx}
}

func (s *childSink) PeekRequestContext() []string {
	if s.todos == nil || !s.todoContext {
		return nil
	}
	ctx := s.todos.PendingRequestContext()
	if ctx == "" {
		return nil
	}
	return []string{ctx}
}

func (s *childSink) PromptComplete(usage agent.PromptUsage) {
	s.flushDisplay()
	usage.Usage = addDelegateUsage(usage.Usage, s.preflightMaintenance)
	usage.Maintenance = addDelegateUsage(usage.Maintenance, s.preflightMaintenance)
	s.usage = usage
	u := usage.Usage
	s.activity.MarkUsage(u)
	s.activity.MarkActivity("finishing")
	s.append(session.Event{
		Type:              session.EventPromptUsage,
		Prompt:            1,
		Usage:             &u,
		TerminationReason: string(usage.TerminationReason),
	})
}

func (s *childSink) flushDisplay() {
	if s == nil || s.assistant == nil {
		s.flushEvents()
		return
	}
	s.assistant.Flush()
	s.flushEvents()
}

func (s *childSink) flushEvents() {
	if s == nil || s.events == nil {
		return
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.events.Flush(); err != nil && s.appendErr == nil {
		s.appendErr = err
	}
}

func (s *childSink) append(ev session.Event) {
	if s.sessionDir == "" {
		return
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.events.Append(ev); err != nil && s.appendErr == nil {
		s.appendErr = err
	}
}

func (s *childSink) retainAppendError(err error) {
	if s == nil || err == nil {
		return
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.appendErr == nil {
		s.appendErr = err
	}
}

func (s *childSink) appendError() error {
	if s == nil {
		return nil
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.appendErr
}

func childContextSnapshot(ctx agent.ContextEstimate) *session.ContextSnapshot {
	if ctx == (agent.ContextEstimate{}) {
		return nil
	}
	return &session.ContextSnapshot{
		Total:           ctx.Total,
		Window:          ctx.Window,
		System:          ctx.System,
		Tools:           ctx.Tools,
		Messages:        ctx.Messages,
		PayloadTotal:    ctx.PayloadTotal,
		PayloadSystem:   ctx.PayloadSystem,
		PayloadTools:    ctx.PayloadTools,
		PayloadMessages: ctx.PayloadMessages,
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
