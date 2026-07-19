// Package agent runs one user prompt as a loop of turns until the model stops
// asking for tools, executing each turn's tool calls in emission order
// (concurrently when they are all read-only) and upholding the transcript
// invariant after every mutation (design §8, §4).
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"harness/internal/diff"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/llm/tokencount"
	"harness/internal/retry"
	"harness/internal/toolresult"
	"harness/internal/tools"
)

// streamRetries is the per-turn mid-stream retry budget: a turn whose stream
// fails after the first byte may be re-requested this many times (spec §2).
// Retries do not consume the maxTurns budget.
const streamRetries = 2

// maxParallelTools bounds concurrent read-only dispatch (spec §8).
const maxParallelTools = 8

// EventSink receives the prompt's observable events for rendering. The agent loop
// owns the transcript and the control flow; the sink only reports. Phase 10's
// renderer implements it (design §8.1, §10).
type EventSink interface {
	TextDelta(text string) // incremental assistant text
	ReasoningSummary(text string)
	TurnAttemptStart(turn, attempt int, ctx ContextEstimate)
	TurnAttemptComplete(usage TurnAttemptUsage)
	ToolUseStart(call llm.ToolCall)
	ToolUseDelta(index int, delta string)
	ToolStart(call llm.ToolCall)      // a tool call is about to run
	ToolResult(result llm.ToolResult) // a tool call finished
	Notice(msg string)                // out-of-band notices (max-turns, cancelled)
	TurnComplete(usage TurnUsage)     // end of one conversational turn
	PromptComplete(usage PromptUsage) // end of the prompt
}

// AssistantPhaseSink is implemented by sinks that want provider phase metadata
// before the corresponding assistant text is rendered.
type AssistantPhaseSink interface {
	AssistantPhase(phase string)
}

// CompactionProgressSink is implemented by sinks that want transient progress
// while compaction inspects or summarizes old context. The callbacks are
// balanced around every invoked compaction, including local degradation.
type CompactionProgressSink interface {
	CompactionStart()
	CompactionComplete()
}

// ToolResultArchive and ToolResultArchiver remain agent-level aliases because
// event sinks implement the archival boundary. Formatting and model guidance
// live in toolresult so foreground and background results share them.
type ToolResultArchive = toolresult.Archive
type ToolResultArchiver = toolresult.Archiver

// ToolDiffSink is implemented by renderers that want user-facing file diffs
// after a mutating tool result. Diffs are not transcript/tool-result content.
type ToolDiffSink interface {
	ToolDiff(call llm.ToolCall, text string)
}

// HookContextReceiver is implemented by sinks that can keep hook-generated
// context available for later turns without adding it to the saved transcript.
type HookContextReceiver interface {
	AddHookContext([]string)
}

// RequestContextProvider is implemented by sinks that can add fresh
// request-only context before each model request.
type RequestContextProvider interface {
	RequestContext() []string
}

// PromptWorkCoordinator is implemented by sinks that own background work whose
// results must be incorporated before the current parent prompt may finish.
// Usage is drained exactly once into the parent prompt; completion context is
// delivered separately through RequestContextProvider.
type PromptWorkCoordinator interface {
	PendingPromptWork() bool
	WaitForPromptWork(context.Context) (llm.Usage, error)
	DrainPromptWorkUsage() llm.Usage
}

// PromptWorkProgressSink is implemented by sinks that want transient progress
// while the parent prompt waits for join-required background work. The callbacks
// are balanced around every wait, including cancellation and errors.
type PromptWorkProgressSink interface {
	PromptWorkWaitStart()
	PromptWorkWaitComplete()
}

// SteerInput is a prepared in-prompt steering message. Text and images are
// appended as a RoleUser transcript message when a tool round gives the loop a
// chance to inject it; RequestContext is visible to subsequent model requests in
// the current prompt without being persisted into the transcript.
type SteerInput struct {
	Text           string
	Images         []llm.ContentBlock
	RequestContext []string
}

// TurnUsage is the accounting for one conversational turn. A turn contains one
// successful provider response (plus any retry attempts) and the complete tool
// result batch requested by that response.
type TurnUsage struct {
	Turn     int
	Attempts int
	Usage    llm.Usage
	Wasted   llm.Usage
	Context  ContextEstimate
}

// PromptUsage is the per-prompt summary handed to the sink (design §10 usage line).
type PromptUsage struct {
	Turns int
	Usage llm.Usage
	// Maintenance is the subset of Usage spent on model calls that are not
	// conversational turns, such as automatic compaction.
	Maintenance llm.Usage
	// Wasted is the subset of Usage spent on turn attempts that were
	// discarded and re-requested after a mid-stream failure (r51+r52). It is
	// already included in Usage; surfacing it lets the UI show the retry cost.
	Wasted  llm.Usage
	Context ContextEstimate
}

// TurnAttemptUsage is the token accounting for one provider request attempt.
// Turn is the logical turn in the current prompt; Attempt is 1
// for the first stream request and higher for retry attempts.
type TurnAttemptUsage struct {
	Turn    int
	Attempt int
	Usage   llm.Usage
}

// MaintenanceUsage reports a model call that supports a prompt without
// creating a conversational turn.
type MaintenanceUsage struct {
	Purpose string
	Usage   llm.Usage
}

// MaintenanceSink is implemented by sinks that persist maintenance accounting
// separately from conversational turns.
type MaintenanceSink interface {
	MaintenanceComplete(MaintenanceUsage)
}

// ContextEstimate is a coarse request-footprint estimate for UI diagnostics.
type ContextEstimate struct {
	Total    int
	Window   int
	System   int
	Tools    int
	Messages int

	PayloadTotal    int
	PayloadSystem   int
	PayloadTools    int
	PayloadMessages int
}

// Options configures an Agent. The zero value is valid; MaxTurns <= 0 means
// unlimited.
type Options struct {
	MaxTurns int
	// MaxPromptTokens stops a prompt once its accumulated tokens reach
	// this ceiling; zero means unlimited. Enforcement lives in the turn loop.
	MaxPromptTokens int
	// MaxOutputTokens caps one normal turn's output; zero uses the shared
	// automatic provider policy. Prewarm and compaction summaries set their own
	// request caps and do not use this value.
	MaxOutputTokens int
	// MaxPromptCostUSD stops a prompt once its accumulated model cost (USD)
	// reaches this ceiling; zero means unlimited. Enforced only when provider
	// usage includes known cost, otherwise the budget cannot fire.
	MaxPromptCostUSD float64
	// Model is the resolved model id stamped onto every request. The agent loop
	// owns Request.Model because the provider config carries no model (one
	// provider can serve many models); main injects the resolved value here.
	Model string
	// ContextWindow is the resolved -context-window override (tokens). When
	// positive it drives the compaction trigger and degradation budget instead of
	// the model registry's window; zero means "use the registry default" (design
	// §6, §12). Plumbing it here is what makes the override actually move the
	// trigger for unknown/local models whose real window differs from the default
	// default.
	ContextWindow int
	// Registry supplies model context windows and pricing loaded from provider
	// config files.
	Registry *llm.Registry
	// Reasoning is forwarded to every model request. Empty means provider
	// default.
	Reasoning llm.ReasoningConfig
	// ServerTools are provider-hosted tools such as web_search that are declared
	// alongside local function tools but handled entirely by the provider.
	ServerTools []llm.ServerTool
	// Now stamps transcript messages. Nil defaults to time.Now.
	Now func() time.Time
	// CompactKeepTurns controls how many whole recent turns remain verbatim after
	// compaction. Zero uses the default.
	CompactKeepTurns int
	// CompactSummaryMaxTokens caps summarization output. Zero uses the default.
	CompactSummaryMaxTokens int
	// CompactToolResultMaxBytes caps old tool-result bodies before they are sent
	// to the summarizer. Zero uses the default; negative disables this pre-pass.
	CompactToolResultMaxBytes int
	// Hooks runs configured lifecycle hooks. Nil disables hooks.
	Hooks *hooks.Runner
	// ShowDiffs emits per-tool-call file diffs for built-in file mutation tools.
	ShowDiffs bool
	// ResponsesStateful enables Responses API previous_response_id chaining.
	// Main only sets it when the selected provider is Responses-capable.
	ResponsesStateful bool
	// Interactive marks a session whose multi-minute pauses justify the 1h
	// Anthropic prompt-cache breakpoint on the stable prefix (set for the REPL).
	// One-shot, delegate, and non-interactive runs leave it false to take the
	// cheaper 5-minute breakpoint. Forwarded to llm.Request.LongCacheTTL.
	Interactive bool
	// Steer enables in-prompt steering: input submitted while a prompt runs is
	// injected as a RoleUser message before the next turn rather than waiting
	// for the prompt to end. The REPL supplies text via Steer. Disabled (false)
	// leaves the loop untouched.
	Steer bool
}

// Agent drives the turn loop against one provider and tool registry, owning the
// running transcript.
type Agent struct {
	provider                  llm.Provider
	tools                     *tools.Registry
	toolSpecs                 []llm.ToolSchema
	registry                  *llm.Registry
	transcript                []llm.Message
	validatedPrefix           int // count of leading transcript messages already known valid (r62)
	system                    string
	model                     string
	maxTurns                  int
	maxPromptTokens           int     // accumulated-token ceiling per prompt; 0 = unlimited
	maxOutputTokens           int     // per-turn output cap; 0 = automatic
	maxPromptCostUSD          float64 // accumulated USD ceiling per prompt; 0 = unlimited
	contextWindow             int     // -context-window override; 0 = use the registry default
	observedContextWindow     int     // smaller provider-reported limit learned from an overflow error
	reasoning                 llm.ReasoningConfig
	serverTools               []llm.ServerTool
	now                       func() time.Time
	sleep                     func(context.Context, time.Duration) error // mid-stream retry backoff; nil-free, set in New
	compactKeepTurns          int
	compactSummaryMaxTokens   int
	compactToolResultMaxBytes int
	compactFallbackNotice     compactFallbackNoticeState
	archiveCompaction         CompactionArchiver
	hooks                     *hooks.Runner
	showDiffs                 bool
	responsesStateful         bool
	interactive               bool            // 1h Anthropic cache breakpoint; see Options.Interactive
	steer                     chan SteerInput // buffered in-prompt steer input; nil when Options.Steer is false
	responseState             llm.ResponseState
	proxySessionID            string
}

type compactFallbackNoticeState struct {
	noShrink    bool
	smallShrink bool
}

// New constructs an Agent. A non-positive Options.MaxTurns means unlimited.
func New(provider llm.Provider, registry *tools.Registry, opts Options) *Agent {
	modelRegistry := opts.Registry
	if modelRegistry == nil {
		modelRegistry = llm.NewRegistry(nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	a := &Agent{
		provider:                  provider,
		tools:                     registry,
		toolSpecs:                 registry.Specs(),
		registry:                  modelRegistry,
		model:                     opts.Model,
		maxTurns:                  opts.MaxTurns,
		maxPromptTokens:           opts.MaxPromptTokens,
		maxOutputTokens:           opts.MaxOutputTokens,
		maxPromptCostUSD:          opts.MaxPromptCostUSD,
		contextWindow:             opts.ContextWindow,
		reasoning:                 opts.Reasoning,
		serverTools:               cloneServerTools(opts.ServerTools),
		now:                       now,
		sleep:                     sleepContext,
		compactKeepTurns:          opts.CompactKeepTurns,
		compactSummaryMaxTokens:   opts.CompactSummaryMaxTokens,
		compactToolResultMaxBytes: opts.CompactToolResultMaxBytes,
		hooks:                     opts.Hooks,
		showDiffs:                 opts.ShowDiffs,
		responsesStateful:         opts.ResponsesStateful,
		interactive:               opts.Interactive,
		proxySessionID:            newProxySessionID(),
	}
	if opts.Steer {
		a.steer = make(chan SteerInput, 16)
	}
	return a
}

// window returns the context window the compaction trigger and degradation
// budget should use: the resolved -context-window override when positive,
// otherwise the model registry's window (256k by default when metadata lacks a
// window). This is what honors the §6 "overridable with -context-window" promise
// in the §12 trigger.
func (a *Agent) window() int {
	configured := a.contextWindow
	if configured <= 0 {
		configured = a.registry.ContextWindow(a.model)
	}
	if a.contextWindow > 0 {
		configured = a.contextWindow
	}
	if a.observedContextWindow > 0 && a.observedContextWindow < configured {
		return a.observedContextWindow
	}
	return configured
}

// SetSystem sets the system prompt sent on every request.
func (a *Agent) SetSystem(system string) {
	a.system = system
	a.resetResponseState()
}

// ToolNames returns the names of tools in the agent's active registry in
// registration order.
func (a *Agent) ToolNames() []string { return a.tools.Names() }

// ToolSpecs returns the model-facing tool specs in registration order.
func (a *Agent) ToolSpecs() []llm.ToolSchema { return cloneToolSpecs(a.toolSpecs) }

// SetTools replaces the tool registry used for subsequent requests. Because the
// agent advertises (Specs) and dispatches from the same registry, swapping it
// changes both what the model sees and what it can call — the hook an agent
// switch uses. A nil registry is ignored.
func (a *Agent) SetTools(registry *tools.Registry) {
	if registry != nil {
		a.tools = registry
		a.toolSpecs = registry.Specs()
		a.resetResponseState()
	}
}

// SetProvider replaces the provider used for subsequent model calls.
func (a *Agent) SetProvider(provider llm.Provider) {
	if provider != nil {
		a.provider = provider
		a.observedContextWindow = 0
		a.resetResponseState()
	}
}

// SetModel replaces the model id stamped onto subsequent requests. contextWindow
// is the same override as Options.ContextWindow: zero means use the registry.
func (a *Agent) SetModel(model string, contextWindow int) {
	a.model = model
	a.contextWindow = contextWindow
	a.observedContextWindow = 0
	a.resetResponseState()
}

// SetReasoning replaces the reasoning controls sent on subsequent requests.
func (a *Agent) SetReasoning(reasoning llm.ReasoningConfig) {
	a.reasoning = reasoning
	a.resetResponseState()
}

// SetServerTools replaces provider-hosted tool declarations for subsequent
// requests.
func (a *Agent) SetServerTools(serverTools []llm.ServerTool) {
	a.serverTools = cloneServerTools(serverTools)
	a.resetResponseState()
}

// SetHooks replaces the lifecycle hook runner used by subsequent turns.
func (a *Agent) SetHooks(runner *hooks.Runner) { a.hooks = runner }

// Steer injects text as an in-prompt steering message. It is the simple text-only
// helper for callers that do not need images or request-only context.
func (a *Agent) Steer(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.SteerContent(SteerInput{Text: text})
}

// SteerContent injects prepared content as an in-prompt steering message: the loop
// drains it before the next model request (between tool rounds) and appends it as
// a RoleUser message. It is a no-op when steering was not enabled
// (Options.Steer false), the input is empty, or the steer buffer is full, so a
// flooding caller cannot block the REPL. SteerContent is safe to call
// concurrently with a running prompt.
func (a *Agent) SteerContent(input SteerInput) {
	if a.steer == nil || steerInputEmpty(input) {
		return
	}
	input.Images = cloneImageBlocks(input.Images)
	input.RequestContext = cleanContext(input.RequestContext)
	select {
	case a.steer <- input:
	default:
	}
}

// DrainSteer pops all queued in-prompt steer text that the loop has not yet
// consumed (e.g. a turn that ended with StopEndTurn and no tool round to inject
// into, a budget/cancel break). The REPL calls this at prompt completion to recover
// undelivered steers and run them as the next prompt, so input submitted during
// a turn is never silently lost. It returns "" when steering is disabled or
// nothing is queued. Non-blocking.
func (a *Agent) DrainSteer() string {
	return a.DrainSteerContent().Text
}

// DrainSteerContent pops all queued prepared in-prompt steer input that the loop
// has not yet consumed. Non-blocking.
func (a *Agent) DrainSteerContent() SteerInput {
	return a.drainSteer()
}

// SetTranscript replaces the running transcript (used when resuming a session).
func (a *Agent) SetTranscript(msgs []llm.Message) {
	a.transcript = msgs
	a.validatedPrefix = 0 // resumed/replaced content must be fully re-validated (r62)
	a.resetResponseState()
}

// SetResponsesStateful toggles Responses API continuation for subsequent
// requests. Disabling or changing the mode clears any previous remote anchor.
func (a *Agent) SetResponsesStateful(enabled bool) {
	if a.responsesStateful == enabled {
		return
	}
	a.responsesStateful = enabled
	a.resetResponseState()
}

// ResponseState returns a copy of the current Responses continuation state.
func (a *Agent) ResponseState() *llm.ResponseState {
	if a.responseState.PreviousResponseID == "" {
		return nil
	}
	state := a.responseState
	return &state
}

// SetResponseState restores Responses continuation state after session resume.
func (a *Agent) SetResponseState(state *llm.ResponseState) {
	a.resetResponseState()
	if state == nil || state.PreviousResponseID == "" {
		return
	}
	a.responseState = *state
}

func (a *Agent) resetResponseState() {
	a.responseState = llm.ResponseState{}
}

// SetSleep replaces the mid-stream retry backoff function. Tests inject a no-op
// to keep the loop free of real time; a nil argument is ignored.
func (a *Agent) SetSleep(sleep func(time.Duration)) {
	if sleep != nil {
		a.sleep = func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			sleep(d)
			return ctx.Err()
		}
	}
}

// SetCompactionArchiver installs the callback used to preserve raw messages
// removed from the active transcript. A nil callback disables archiving.
func (a *Agent) SetCompactionArchiver(archive CompactionArchiver) {
	a.archiveCompaction = archive
}

// Transcript returns the current transcript. The slice is owned by the Agent;
// callers must not mutate it.
func (a *Agent) Transcript() []llm.Message { return a.transcript }

// ContextRequest returns the provider-neutral request shape for the current
// active context: system prompt, advertised tools, transcript, model, and
// reasoning controls. The returned slices are copies so callers can serialize or
// inspect them without mutating the agent.
func (a *Agent) ContextRequest() llm.Request {
	return a.ContextRequestWithContext(nil)
}

// ContextRequestWithContext is ContextRequest plus request-only context, matching
// the message shape used by RunPromptContentWithContext.
func (a *Agent) ContextRequestWithContext(extraContext []string) llm.Request {
	return llm.Request{
		Model:          a.model,
		Purpose:        llm.RequestPurposeTurn,
		System:         a.system,
		Messages:       append([]llm.Message(nil), a.transcript...),
		Tools:          cloneToolSpecs(a.toolSpecs),
		ServerTools:    cloneServerTools(a.serverTools),
		Reasoning:      a.reasoning,
		RequestContext: append([]string(nil), extraContext...),
		ProxySessionID: a.proxySessionID,
		LongCacheTTL:   a.interactive,
	}
}

// ProxySessionID returns the opaque local key used to isolate proxy-managed
// model state for this agent session.
func (a *Agent) ProxySessionID() string {
	return a.proxySessionID
}

// SetProxySessionID restores a persisted proxy session key on resume.
func (a *Agent) SetProxySessionID(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		a.proxySessionID = newProxySessionID()
		return
	}
	a.proxySessionID = id
}

// ResetProxySessionID rotates proxy-managed model state for a fresh session.
func (a *Agent) ResetProxySessionID() {
	a.proxySessionID = newProxySessionID()
	a.resetResponseState()
}

func newProxySessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "harness-session-" + hex.EncodeToString(b[:])
	}
	return "harness-session-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// PrewarmRequest builds a minimal request that writes the prompt cache — the
// stable tools+system prefix plus any existing (e.g. resumed) transcript —
// without producing a real turn, so the first real request reads a warm cache
// instead of paying the cold cache-write latency. ok is false when there is
// nothing cacheable yet. The returned request is a self-contained snapshot.
func (a *Agent) PrewarmRequest() (llm.Request, bool) {
	if a.system == "" && len(a.toolSpecs) == 0 && len(a.serverTools) == 0 {
		return llm.Request{}, false
	}
	req := a.ContextRequest()
	if len(req.Messages) == 0 {
		// The Messages API requires at least one message; this placeholder rides
		// after the cached prefix and is never persisted.
		req.Messages = []llm.Message{a.userMessage("warm cache", nil)}
	}
	req.MaxTokens = 1 // smallest legal cap: only the prefill matters
	req.Purpose = llm.RequestPurposePrewarm
	req.Reasoning = llm.ReasoningConfig{} // no thinking/effort — a pure prefix write
	req.RequestContext = nil
	return req, true
}

// PrewarmFunc captures the current provider and a PrewarmRequest snapshot and
// returns a closure that streams the warm-up request, discards its output, and
// returns any provider-reported usage for maintenance accounting.
// Call it on the goroutine that owns the agent (before the input loop); the
// returned closure shares no mutable agent state, so it is safe to run in a
// background goroutine. ok is false when there is nothing to warm.
func (a *Agent) PrewarmFunc() (func(context.Context) llm.Usage, bool) {
	req, ok := a.PrewarmRequest()
	if !ok {
		return nil, false
	}
	provider := a.provider
	return func(ctx context.Context) llm.Usage {
		var usage llm.Usage
		for event, err := range provider.Stream(ctx, req) {
			if err != nil {
				// Best-effort: a failed warm-up just means the first real request
				// pays the cold-cache cost. Preserve usage already reported before
				// the failure because those tokens may still be billed.
				return usage
			}
			if (event.Kind == llm.EventUsage || event.Kind == llm.EventDone) && event.Usage != nil {
				usage = mergeUsage(usage, *event.Usage)
			}
		}
		return usage
	}, true
}

// EstimateContext estimates the next request footprint using the current
// transcript, system prompt, and advertised tools.
func (a *Agent) EstimateContext() ContextEstimate {
	return a.estimateContext(nil)
}

// triggerTokens estimates the next request's input footprint for the compaction
// trigger: the real input-token count the last measured response reported plus a
// byte estimate of only the messages appended since (boundary..end). Using the
// real measurement for the bulk and estimating just the delta is far more
// accurate than a whole-request byte/4 estimate, especially with images (r44).
// With no prior measurement (boundary 0, lastInput 0) it degrades to a full
// transcript estimate.
func (a *Agent) triggerTokens(lastInput, boundary int) int {
	if boundary < 0 {
		boundary = 0
	}
	if boundary > len(a.transcript) {
		boundary = len(a.transcript)
	}
	return lastInput + estimateTokens(a.transcript[boundary:])
}

func (a *Agent) estimateContext(extraContext []string) ContextEstimate {
	return a.estimateContextForTranscript(extraContext, a.transcript)
}

func (a *Agent) estimateContextForTranscript(extraContext []string, transcript []llm.Message) ContextEstimate {
	est := estimateRequest(llm.Request{
		System:         a.system,
		Messages:       transcript,
		Tools:          a.toolSpecs,
		ServerTools:    a.serverTools,
		RequestContext: extraContext,
	}, a.window())
	est.PayloadSystem = est.System
	est.PayloadTools = est.Tools
	est.PayloadMessages = est.Messages
	est.PayloadTotal = est.Total
	return est
}

// turnResult holds what one conversational turn produced after assembly.
type turnResult struct {
	text       string
	reasoning  []llm.ContentBlock // thinking / redacted_thinking / reasoning blocks, in arrival order
	toolCalls  []llm.ToolCall
	phase      string
	usage      llm.Usage
	stopReason llm.StopReason
	responseID string
	attempts   int
}

func (r turnResult) hasPartialOutput() bool {
	return r.text != "" || len(r.toolCalls) > 0
}

type modelRequest struct {
	request      llm.Request
	estimate     ContextEstimate
	usedPrevious bool
}

// RequestSnapshot is a read-only view of the provider-neutral request the agent
// would send for its current context. It is used by diagnostics that must not
// call the model.
type RequestSnapshot struct {
	Request      llm.Request
	Estimate     ContextEstimate
	UsedPrevious bool
}

// TurnAttemptAbandonSink is an optional event sink extension for renderers that
// persist replay metadata. It marks a streamed attempt whose visible deltas were
// discarded from the transcript because the turn will be retried.
type TurnAttemptAbandonSink interface {
	TurnAttemptAbandoned(turn, attempt int)
}

func (a *Agent) modelRequest(requestContext []string) modelRequest {
	return a.modelRequestForTranscript(requestContext, a.transcript)
}

func (a *Agent) modelRequestForTranscript(requestContext []string, transcript []llm.Message) modelRequest {
	payloadMessages, usedPrevious := a.payloadMessagesIn(transcript)
	estimate := a.estimatePayloadContextForTranscript(requestContext, transcript, payloadMessages)
	req := llm.Request{
		Model:                a.model,
		Purpose:              llm.RequestPurposeTurn,
		System:               a.system,
		Messages:             payloadMessages,
		Tools:                cloneToolSpecs(a.toolSpecs),
		ServerTools:          cloneServerTools(a.serverTools),
		Reasoning:            a.reasoning,
		MaxTokens:            a.maxOutputTokens,
		StoreResponse:        a.responsesStateful,
		RequestContext:       append([]string(nil), requestContext...),
		ProxySessionID:       a.proxySessionID,
		LongCacheTTL:         a.interactive,
		EstimatedInputTokens: estimate.Total,
		ContextWindowHint:    estimate.Window,
	}
	if usedPrevious {
		req.PreviousResponseID = a.responseState.PreviousResponseID
	}
	return modelRequest{
		request:      req,
		estimate:     estimate,
		usedPrevious: usedPrevious,
	}
}

// DebugRequest snapshots the next provider-neutral model request without
// appending to the live transcript or contacting the provider. When includeUser
// is true, it includes the supplied user content as the pending first turn.
func (a *Agent) DebugRequest(includeUser bool, userText string, images []llm.ContentBlock, extraContext []string) RequestSnapshot {
	transcript := a.transcript
	if includeUser {
		transcript = cloneMessages(a.transcript)
		transcript = append(transcript, a.userMessage(userText, images))
	}
	mr := a.modelRequestForTranscript(extraContext, transcript)
	return RequestSnapshot{
		Request:      mr.request,
		Estimate:     mr.estimate,
		UsedPrevious: mr.usedPrevious,
	}
}

func (a *Agent) countModelRequestInput(ctx context.Context, mr modelRequest) modelRequest {
	count, ok := a.countInputTokens(ctx, mr.request)
	if !ok || count <= 0 {
		return mr
	}
	old := mr.request.EstimatedInputTokens
	mr.request.EstimatedInputTokens = count
	delta := count - old
	mr.estimate.Total += delta
	mr.estimate.PayloadTotal += delta
	mr.estimate.Messages += delta
	mr.estimate.PayloadMessages += delta
	if mr.estimate.Messages < 0 {
		mr.estimate.Messages = 0
	}
	if mr.estimate.PayloadMessages < 0 {
		mr.estimate.PayloadMessages = 0
	}
	return mr
}

func (a *Agent) countInputTokens(ctx context.Context, req llm.Request) (int, bool) {
	if counter, ok := a.provider.(llm.InputTokenCounter); ok {
		count, err := counter.CountInputTokens(ctx, req)
		if err == nil && count.InputTokens > 0 {
			return count.InputTokens, true
		}
	}
	if tokencount.ShouldEstimateOpenAIChat(a.provider.Name()) {
		if count := tokencount.EstimateOpenAIChat(req); count > 0 {
			return count, true
		}
	}
	return 0, false
}

func (a *Agent) payloadMessages() ([]llm.Message, bool) {
	return a.payloadMessagesIn(a.transcript)
}

func (a *Agent) payloadMessagesIn(transcript []llm.Message) ([]llm.Message, bool) {
	if !a.validResponseStateFor(len(transcript)) {
		return transcript, false
	}
	return transcript[a.responseState.AnchorMessages:], true
}

func (a *Agent) validResponseState() bool {
	return a.validResponseStateFor(len(a.transcript))
}

func (a *Agent) validResponseStateFor(messageCount int) bool {
	return a.responsesStateful &&
		a.responseState.PreviousResponseID != "" &&
		a.responseState.AnchorMessages >= 0 &&
		a.responseState.AnchorMessages <= messageCount
}

func (a *Agent) estimatePayloadContext(requestContext []string, payloadMessages []llm.Message) ContextEstimate {
	return a.estimatePayloadContextForTranscript(requestContext, a.transcript, payloadMessages)
}

func (a *Agent) estimatePayloadContextForTranscript(requestContext []string, transcript, payloadMessages []llm.Message) ContextEstimate {
	est := a.estimateContextForTranscript(requestContext, transcript)
	payload := estimateRequest(llm.Request{
		System:         a.system,
		Messages:       payloadMessages,
		Tools:          a.toolSpecs,
		ServerTools:    a.serverTools,
		RequestContext: requestContext,
	}, a.window())
	est.PayloadSystem = payload.System
	est.PayloadTools = payload.Tools
	est.PayloadMessages = payload.Messages
	est.PayloadTotal = payload.Total
	return est
}

func (a *Agent) updateResponseState(res turnResult) {
	if !a.responsesStateful {
		return
	}
	if res.responseID == "" {
		a.resetResponseState()
		return
	}
	a.responseState = llm.ResponseState{
		PreviousResponseID: res.responseID,
		AnchorMessages:     len(a.transcript),
	}
}

// validateTranscript asserts the §4 invariant after a mutation. It validates
// incrementally: the loop only ever appends whole turns that leave the message
// list at a clean boundary (no open tool_use), so a prior successful validation
// already proved everything up to validatedPrefix valid, and only the suffix
// appended since needs re-walking (r62). A full walk runs only after the prefix
// is reset — on SetTranscript/resume, after compaction replaces the transcript,
// or after any failure. This turns the per-prompt validation cost from O(n²) over
// a long session into O(n).
func (a *Agent) validateTranscript(phase string) error {
	if a.validatedPrefix < 0 || a.validatedPrefix > len(a.transcript) {
		a.validatedPrefix = 0 // transcript was replaced/shrank out from under us
	}
	if err := llm.ValidateTranscript(a.transcript[a.validatedPrefix:]); err != nil {
		a.resetResponseState()
		a.validatedPrefix = 0 // force a full re-walk next time
		return fmt.Errorf("agent transcript invalid %s: %w", phase, err)
	}
	a.validatedPrefix = len(a.transcript)
	return nil
}

// RunPrompt appends the user message, then loops over turns until the model
// stops requesting tools or the prompt's turn budget is hit (design §8.1). Cancellation
// mid-stream applies the §4 cancel repair and returns ctx.Err(); the transcript
// is left valid (re-sendable) in every exit path.
func (a *Agent) RunPrompt(ctx context.Context, userText string, sink EventSink) error {
	return a.RunPromptContent(ctx, userText, nil, sink)
}

// RunPromptContent is RunPrompt with optional user-provided image blocks. Images
// are placed before text so vision providers see the visual context first.
func (a *Agent) RunPromptContent(ctx context.Context, userText string, images []llm.ContentBlock, sink EventSink) error {
	return a.RunPromptContentWithContext(ctx, userText, images, nil, 0, sink)
}

// RunPromptContentWithContext is RunPromptContent plus request-only hook context.
// extraContext is visible to model requests for this prompt but is not persisted
// into the transcript.
func (a *Agent) RunPromptContentWithContext(ctx context.Context, userText string, images []llm.ContentBlock, extraContext []string, promptID int, sink EventSink) error {
	a.compactFallbackNotice = compactFallbackNoticeState{}
	promptMessage := a.userMessage(userText, images)
	promptMessage.Origin = llm.MessageOriginPrompt
	a.transcript = append(a.transcript, promptMessage)

	var total llm.Usage
	var maintenanceTotal llm.Usage
	var lastInput int // input tokens the final turn reported (drives the trigger)
	var lastContext ContextEstimate
	turns := 0
	unlimited := a.maxTurns <= 0
	stopHookActive := false
	var guard turnGuard
	var wastedTotal llm.Usage // tokens spent on retried-and-discarded turn attempts (r51+r52)
	appendBoundary := 0       // transcript length measured by lastInput (drives the r44 trigger)
	var steerContext []string
	forcePromptWorkSynthesis := false

	for unlimited || turns < a.maxTurns || forcePromptWorkSynthesis {
		// Live-transcript retention (design §12, r9+r20): shrink stale large
		// tool outputs and aged images before building the request, so they are
		// not re-sent verbatim every turn. Pure local edit, invariant-preserving.
		a.applyRetention(sink)
		requestContext := a.requestContext(appendPromptContext(extraContext, steerContext), sink)
		modelReq := a.modelRequest(requestContext)
		lastContext = modelReq.estimate
		// Proactive trigger (spec §4): a turn whose tool results balloon the
		// context compacts before the next request, not after the turn. The
		// trigger leans on the last real input count plus an estimate of only the
		// messages appended since it was measured (r44), not a whole-request byte
		// estimate.
		if a.overThreshold(a.triggerTokens(lastInput, appendBoundary)) {
			// Only reset the trigger state when compaction actually rewrote the
			// transcript. A no-op compaction that reset lastInput/appendBoundary
			// would force a full-transcript re-estimate every turn with zero
			// progress (no-op churn).
			compUsage, changed, err := a.compactTriggered(ctx, sink, "auto")
			if compUsage != (llm.Usage{}) {
				total = add(total, compUsage)
				maintenanceTotal = add(maintenanceTotal, compUsage)
				reportMaintenance(sink, "compaction", compUsage)
			}
			if err == nil && changed {
				// The old reported count no longer describes the compacted
				// transcript and would re-trigger every turn.
				lastInput = 0
				appendBoundary = 0
				requestContext = a.requestContext(appendPromptContext(extraContext, steerContext), sink)
				modelReq = a.modelRequest(requestContext)
				lastContext = modelReq.estimate
			}
		}
		if err := a.validateTranscript("before model request"); err != nil {
			sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
			return err
		}
		modelReq = a.countModelRequestInput(ctx, modelReq)
		lastContext = modelReq.estimate
		if a.overThreshold(modelReq.estimate.Total) {
			compUsage, changed, err := a.compactTriggered(ctx, sink, "input-count")
			if compUsage != (llm.Usage{}) {
				total = add(total, compUsage)
				maintenanceTotal = add(maintenanceTotal, compUsage)
				reportMaintenance(sink, "compaction", compUsage)
			}
			if err == nil && changed {
				lastInput = 0
				appendBoundary = 0
				requestContext = a.requestContext(appendPromptContext(extraContext, steerContext), sink)
				modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
				lastContext = modelReq.estimate
			}
		}
		if err := a.validateTranscript("before model request"); err != nil {
			sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
			return err
		}

		// The request we are about to send reflects the current transcript;
		// remember its length so the next trigger only re-estimates what gets
		// appended after the response we are about to measure.
		appendBoundary = len(a.transcript)
		// RequestContext above may have drained a just-completed delegate report.
		// Fold its usage before the provider call and remember whether older
		// join-required work is still running during this parent model round.
		total = add(total, drainPromptWorkUsage(sink))
		pendingBeforeRequest := pendingPromptWork(sink)
		forcePromptWorkSynthesis = false
		res, wasted, err := a.streamWithRetry(ctx, modelReq.request, sink, turns+1, lastContext)
		if err != nil && !res.hasPartialOutput() {
			if learned, ok := contextOverflowWindow(err); ok {
				if learned > 0 && a.observeContextWindow(learned) {
					sink.Notice(fmt.Sprintf("[context window adjusted: provider reported %d tokens; retrying request]", learned))
				} else {
					sink.Notice("[context overflow: compacting and retrying request]")
				}
				compUsage, changed, cerr := a.compactTriggered(ctx, sink, "context-overflow")
				if compUsage != (llm.Usage{}) {
					total = add(total, compUsage)
					maintenanceTotal = add(maintenanceTotal, compUsage)
					reportMaintenance(sink, "compaction", compUsage)
				}
				if cerr == nil && changed {
					lastInput = 0
					appendBoundary = 0
				}
				requestContext = a.requestContext(appendPromptContext(extraContext, steerContext), sink)
				modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
				lastContext = modelReq.estimate
				res, wasted, err = a.streamWithRetry(ctx, modelReq.request, sink, turns+1, lastContext)
			}
		}
		if err != nil && modelReq.request.StoreResponse && !res.hasPartialOutput() && storeResponseRejected(err) {
			a.SetResponsesStateful(false)
			sink.Notice("[responses state disabled: provider rejected stored responses; retrying stateless]")
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = modelReq.estimate
			var retryWasted llm.Usage
			res, retryWasted, err = a.streamWithRetry(ctx, modelReq.request, sink, turns+1, lastContext)
			wasted = add(wasted, retryWasted)
		}
		if err != nil && modelReq.usedPrevious && !res.hasPartialOutput() && previousResponseRejected(err) {
			a.resetResponseState()
			sink.Notice("[responses state reset: previous response unavailable; retrying with full context]")
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = modelReq.estimate
			var retryWasted llm.Usage
			res, retryWasted, err = a.streamWithRetry(ctx, modelReq.request, sink, turns+1, lastContext)
			wasted = add(wasted, retryWasted)
		}
		wastedTotal = add(wastedTotal, wasted)
		total = add(total, add(res.usage, wasted))
		// Context-size signal, not billing: cached tokens occupy the window too.
		lastInput = res.usage.InputTokens + res.usage.CacheReadTokens + res.usage.CacheWriteTokens

		if err != nil {
			a.resetResponseState()
			// Cancellation repair: keep streamed partial text as a text-only
			// assistant message; drop the message entirely if nothing streamed.
			// Un-executed tool calls are never appended.
			cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			if cancelled && res.text != "" {
				a.transcript = append(a.transcript, a.partialAssistantMessage(res))
				turns++
			}
			if cancelled {
				sink.Notice("[cancelled]")
			}
			if verr := a.validateTranscript("after failed turn"); verr != nil {
				err = errors.Join(err, verr)
			}
			if turns > 0 && cancelled && res.text != "" {
				sink.TurnComplete(TurnUsage{Turn: turns, Attempts: res.attempts, Usage: add(res.usage, wasted), Wasted: wasted, Context: lastContext})
			}
			sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
			return err
		}

		turns++
		a.transcript = append(a.transcript, a.assistantMessage(res))
		a.updateResponseState(res)

		if res.stopReason != llm.StopToolUse {
			sink.TurnComplete(TurnUsage{Turn: turns, Attempts: res.attempts, Usage: add(res.usage, wasted), Wasted: wasted, Context: lastContext})
			// A model may try to finalize while background delegates are still
			// running. Join them, then issue another model request with their reports
			// injected as request context so the parent actually synthesizes them.
			if pendingPromptWork(sink) {
				usage, waitErr := waitForPromptWork(ctx, sink)
				total = add(total, usage)
				if waitErr != nil {
					sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
					return waitErr
				}
				message := a.textMessage(llm.RoleUser, "[background delegates completed; synthesize their reports from request context before finishing]")
				message.Origin = llm.MessageOriginInternal
				a.transcript = append(a.transcript, message)
				if err := a.validateTranscript("after background delegate join"); err != nil {
					sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
					return err
				}
				forcePromptWorkSynthesis = true
				continue
			}
			if notice := stopReasonNotice(res.stopReason); notice != "" {
				sink.Notice(notice)
			}
			if a.hooks != nil && !stopHookActive && a.hooks.HasEvent(hooks.Stop) {
				hookRes := a.hooks.Run(ctx, hooks.Stop, "", hooks.Payload{
					"prompt_id":              promptID,
					"turn_id":                turns,
					"stop_hook_active":       stopHookActive,
					"last_assistant_message": res.text,
				})
				for _, notice := range hookRes.Notices {
					sink.Notice(notice)
				}
				if len(hookRes.AdditionalContext) > 0 {
					extraContext = append(extraContext, hookRes.AdditionalContext...)
				}
				if hookRes.Block {
					reason := hookRes.Reason()
					if reason == "" {
						reason = "Stop hook requested continuation"
					}
					message := a.textMessage(llm.RoleUser, "[hook Stop requested continuation]\n"+reason)
					message.Origin = llm.MessageOriginInternal
					a.transcript = append(a.transcript, message)
					stopHookActive = true
					if err := a.validateTranscript("after stop hook continuation"); err != nil {
						sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
						return err
					}
					continue
				}
			}
			if err := a.validateTranscript("after assistant turn"); err != nil {
				sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
				return err
			}
			break
		}

		results, parallelBatches, toolUsage := a.dispatchCalls(ctx, res.toolCalls, promptID, turns, sink)
		total = add(total, toolUsage)
		a.transcript = append(a.transcript, llm.Message{
			Role:                llm.RoleUser,
			Time:                a.now(),
			Content:             results,
			ParallelToolBatches: parallelBatches,
		})
		if err := a.validateTranscript("after tool results"); err != nil {
			sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
			return err
		}
		sink.TurnComplete(TurnUsage{Turn: turns, Attempts: res.attempts, Usage: add(add(res.usage, wasted), toolUsage), Wasted: wasted, Context: lastContext})

		// Give a newly launched background delegate one subsequent parent model
		// round for useful independent work. If work was already pending before
		// this request, join it now and synthesize its injected report next.
		if (pendingBeforeRequest || (!unlimited && turns >= a.maxTurns)) && pendingPromptWork(sink) {
			usage, waitErr := waitForPromptWork(ctx, sink)
			total = add(total, usage)
			if waitErr != nil {
				sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
				return waitErr
			}
			forcePromptWorkSynthesis = true
			continue
		}

		// Runaway guardrails (design §8.1). The transcript now ends on a closed
		// tool_use/tool_result pair, so injecting a steering RoleUser message or
		// breaking here keeps the §4 invariant intact.
		guard.recordTools(res.toolCalls, results)

		// Mid-prompt steering (design §8.1): drain input the user submitted while
		// this turn was running and inject it as a single RoleUser message the
		// next model request sees. A steer is a deliberate change of approach, so
		// it resets the loop-guard streaks — the model is not penalized for the
		// repeat/error run that preceded the redirect, and no redundant guard
		// nudge fires immediately after. Steer does not consume a maxTurns slot:
		// the message rides on the next model request the loop was already going
		// to make. It falls through to the usual budget checks so a configured
		// ceiling still bounds the turn.
		if steered := a.drainSteer(); !steerInputEmpty(steered) {
			steerMessage := a.userMessage(steered.Text, steered.Images)
			steerMessage.Origin = llm.MessageOriginSteer
			a.transcript = append(a.transcript, steerMessage)
			steerContext = append(steerContext, steered.RequestContext...)
			requestContext = a.requestContext(appendPromptContext(extraContext, steerContext), sink)
			guard.repeatRuns = 0
			guard.repeatSteered = false
			guard.errorRuns = 0
			guard.errorSteered = false
			if err := a.validateTranscript("after in-prompt steer"); err != nil {
				sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
				return err
			}
		}

		// Hard stop: an unrelenting error storm. Finalize with a tools-disabled
		// summary so the turn ends on an assistant message, not a dangling result.
		if guard.shouldBreakErrors() {
			sink.Notice(errorStormNotice(guard.errorRuns))
			fu, fctx, completed := a.finalizeWithSummary(ctx, sink, requestContext, turns+1)
			total = add(total, fu)
			lastContext = fctx
			if completed {
				turns++
			}
			break
		}

		// Hard stop: a byte-identical successful repeat loop that ignored the one
		// steering nudge. Finalize the same way so the turn ends on an assistant
		// message (the success-loop analogue of the error-storm break).
		if guard.shouldBreakRepeat() {
			sink.Notice(repeatLoopNotice(guard.repeatRuns))
			fu, fctx, completed := a.finalizeWithSummary(ctx, sink, requestContext, turns+1)
			total = add(total, fu)
			lastContext = fctx
			if completed {
				turns++
			}
			break
		}

		// Token budget: stop before the next (paid) request. No final summary —
		// the whole point is to stop spending.
		if a.maxPromptTokens > 0 && totalTokens(total) >= a.maxPromptTokens {
			sink.Notice(promptTokenBudgetNotice(a.maxPromptTokens))
			break
		}

		// Cost budget: the dollar analogue of the token budget, same hard stop.
		// Only fires when provider usage includes known cost, so an unpriced model
		// silently has no cost ceiling.
		if a.maxPromptCostUSD > 0 {
			if total.CostKnown && total.CostUSD >= a.maxPromptCostUSD {
				sink.Notice(promptCostBudgetNotice(a.maxPromptCostUSD, total.CostUSD))
				break
			}
		}

		// One steering nudge per condition (repetition / error storm share a slot).
		if msg := guard.steerMessage(); msg != "" {
			message := a.textMessage(llm.RoleUser, msg)
			message.Origin = llm.MessageOriginInternal
			a.transcript = append(a.transcript, message)
			if err := a.validateTranscript("after loop-guard steer"); err != nil {
				sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
				return err
			}
		}

		// Wrap-up nudge once, before the final allowed turn, so the model
		// can stop calling tools and summarize within budget (r3).
		if !unlimited && turns == a.maxTurns-1 && !guard.wrapUpSteered {
			guard.wrapUpSteered = true
			message := a.textMessage(llm.RoleUser, wrapUpSteer)
			message.Origin = llm.MessageOriginInternal
			a.transcript = append(a.transcript, message)
			if err := a.validateTranscript("after wrap-up steer"); err != nil {
				sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
				return err
			}
		}

		if !unlimited && turns >= a.maxTurns {
			sink.Notice(maxTurnsNotice(a.maxTurns))
			fu, fctx, completed := a.finalizeWithSummary(ctx, sink, requestContext, turns+1)
			total = add(total, fu)
			lastContext = fctx
			if completed {
				turns++
			}
			break
		}
	}

	// Budget/guard exits can bypass the normal synthesis continuation. They still
	// must not abandon join-required delegates or lose their spend.
	if pendingPromptWork(sink) {
		usage, waitErr := waitForPromptWork(ctx, sink)
		total = add(total, usage)
		if waitErr != nil {
			sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
			return waitErr
		}
	}
	total = add(total, drainPromptWorkUsage(sink))

	// Post-prompt compaction trigger (design §12, §8.1): fires after the final
	// turn, before returning to the REPL. The summary call's usage folds into the
	// prompt total and is also reported separately as maintenance. A compaction
	// error never fails the prompt — the warning was already reported and
	// the transcript was kept intact.
	lastContext = a.estimateContext(a.requestContext(appendPromptContext(extraContext, steerContext), sink))
	compUsage, changed, err := a.MaybeCompact(ctx, a.triggerTokens(lastInput, appendBoundary), sink)
	if compUsage != (llm.Usage{}) {
		total = add(total, compUsage)
		maintenanceTotal = add(maintenanceTotal, compUsage)
		reportMaintenance(sink, "compaction", compUsage)
	}
	if err == nil && changed {
		lastContext = a.estimateContext(a.requestContext(appendPromptContext(extraContext, steerContext), sink))
	}
	if err := a.validateTranscript("after prompt"); err != nil {
		sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
		return err
	}

	sink.PromptComplete(PromptUsage{Turns: turns, Usage: total, Maintenance: maintenanceTotal, Wasted: wastedTotal, Context: lastContext})
	return nil
}

func reportMaintenance(sink EventSink, purpose string, usage llm.Usage) {
	// A pressured transcript can be rewritten entirely through the local
	// degradation path when there is no older completed turn to summarize.
	// That is maintenance work, but it is not a model call and must not appear
	// in model-call accounting.
	if usage == (llm.Usage{}) {
		return
	}
	if maintenance, ok := sink.(MaintenanceSink); ok {
		maintenance.MaintenanceComplete(MaintenanceUsage{Purpose: purpose, Usage: usage})
	}
}

// finalizeWithSummary issues one final model request with tools disabled so a
// turn that hit a hard stop right after tool dispatch ends on an assistant
// summary instead of a dangling tool_result (r3+r49). It is best-effort: a
// failed or empty summary leaves the already-valid transcript untouched. Any
// tool calls the model emits despite tools being disabled are ignored — only
// the summary text is appended, so no unanswered tool_use can be created. It
// returns the request's usage (counted toward the turn total) and estimate.
func (a *Agent) finalizeWithSummary(ctx context.Context, sink EventSink, requestContext []string, turn int) (llm.Usage, ContextEstimate, bool) {
	modelReq := a.modelRequest(requestContext)
	modelReq.request.Tools = nil // no tools: force a text-only wind-down
	modelReq.request.ServerTools = nil
	res, wasted, err := a.streamWithRetry(ctx, modelReq.request, sink, turn, modelReq.estimate)
	usage := add(res.usage, wasted)
	if err != nil {
		a.resetResponseState()
		return usage, modelReq.estimate, false
	}
	if strings.TrimSpace(res.text) != "" {
		msg := a.textMessage(llm.RoleAssistant, res.text)
		msg.Phase = llm.AssistantPhaseFinal
		a.transcript = append(a.transcript, msg)
		a.updateResponseState(res)
	}
	sink.TurnComplete(TurnUsage{Turn: turn, Attempts: res.attempts, Usage: usage, Wasted: wasted, Context: modelReq.estimate})
	return usage, modelReq.estimate, true
}

// dispatchCalls runs one turn's tool calls. Consecutive read-only calls
// dispatch concurrently when tool hooks are inactive; mutating calls remain ordering
// barriers. Sink events and the returned blocks are in emission order either way,
// and the sink is only ever called from this goroutine (spec §8). The returned
// parallel batches record the complete ordered membership of each concurrent island.
func (a *Agent) dispatchCalls(ctx context.Context, calls []llm.ToolCall, promptID, turnID int, sink EventSink) ([]llm.ContentBlock, []llm.ParallelToolBatch, llm.Usage) {
	blocks := make([]llm.ContentBlock, len(calls))
	var parallelBatches []llm.ParallelToolBatch
	var total llm.Usage

	toolHooksActive := a.hooks != nil && (a.hooks.HasEvent(hooks.PreToolUse) || a.hooks.HasEvent(hooks.PostToolUse))
	for i := 0; i < len(calls); {
		if toolHooksActive || !a.tools.CallReadOnly(calls[i]) {
			block, usage := a.dispatchSequentialCall(ctx, calls[i], promptID, turnID, sink)
			blocks[i] = block
			total = add(total, usage)
			i++
			continue
		}

		start := i
		for i < len(calls) && a.tools.CallReadOnly(calls[i]) {
			i++
		}
		if i-start == 1 {
			block, usage := a.dispatchSequentialCall(ctx, calls[start], promptID, turnID, sink)
			blocks[start] = block
			total = add(total, usage)
			continue
		}

		batchCalls := calls[start:i]
		batch := llm.ParallelToolBatch{ToolUseIDs: make([]string, len(batchCalls))}
		for j, call := range batchCalls {
			batch.ToolUseIDs[j] = call.ID
		}
		parallelBatches = append(parallelBatches, batch)

		usage := a.dispatchReadOnlyBatch(ctx, batchCalls, blocks[start:i], sink)
		total = add(total, usage)
	}
	return blocks, parallelBatches, total
}

func (a *Agent) dispatchSequentialCall(ctx context.Context, call llm.ToolCall, promptID, turnID int, sink EventSink) (llm.ContentBlock, llm.Usage) {
	sink.ToolStart(call)
	diffState := a.snapshotToolDiff(call)
	r := a.dispatchOne(ctx, call, promptID, turnID, sink)
	block, usage := a.finishToolResult(r, sink)
	a.emitToolDiff(call, diffState, sink)
	return block, usage
}

func (a *Agent) dispatchReadOnlyBatch(ctx context.Context, calls []llm.ToolCall, blocks []llm.ContentBlock, sink EventSink) llm.Usage {
	for _, call := range calls {
		sink.ToolStart(call)
	}

	results := make([]llm.ToolResult, len(calls))
	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = a.tools.Dispatch(ctx, call)
		}()
	}
	wg.Wait()

	var total llm.Usage
	for i, r := range results {
		block, usage := a.finishToolResult(r, sink)
		blocks[i] = block
		total = add(total, usage)
	}
	return total
}

func (a *Agent) finishToolResult(r llm.ToolResult, sink EventSink) (llm.ContentBlock, llm.Usage) {
	var notice string
	r, notice = a.prepareToolResult(r, sink)
	sink.ToolResult(r)
	if notice != "" {
		sink.Notice(notice)
	}
	return resultBlock(r), r.Usage
}

type toolDiffState struct {
	enabled bool
	paths   []string
	before  []diff.Snapshot
}

func (a *Agent) snapshotToolDiff(call llm.ToolCall) toolDiffState {
	if !a.showDiffs {
		return toolDiffState{}
	}
	paths, ok := a.tools.MutatedPaths(call)
	if !ok {
		return toolDiffState{}
	}
	return toolDiffState{
		enabled: true,
		paths:   paths,
		before:  diff.SnapshotPaths(paths),
	}
}

func (a *Agent) emitToolDiff(call llm.ToolCall, state toolDiffState, sink EventSink) {
	if !state.enabled {
		return
	}
	after := diff.SnapshotPaths(state.paths)
	for _, fd := range diff.RenderSnapshots(state.before, after, diff.Options{}) {
		switch {
		case fd.Err != nil:
			sink.Notice(fmt.Sprintf("[diff: skipped %s: %v]", fd.Path, fd.Err))
		case fd.BinarySkipped:
			sink.Notice(fmt.Sprintf("[diff: skipped binary file %s]", fd.Path))
		case strings.TrimSpace(fd.Text) != "":
			if ds, ok := sink.(ToolDiffSink); ok {
				ds.ToolDiff(call, fd.Text)
			}
		}
	}
}

func (a *Agent) dispatchOne(ctx context.Context, call llm.ToolCall, promptID, turnID int, sink EventSink) llm.ToolResult {
	if call.InvalidInputError != "" {
		return llm.ToolResult{ForID: call.ID, Text: invalidToolInputResult(call), IsError: true}
	}
	if a.isKimiWebSearchCall(call) {
		text := strings.TrimSpace(string(call.Input))
		if text == "" {
			text = "{}"
		}
		return llm.ToolResult{ForID: call.ID, Text: text}
	}

	var preContext []string
	if a.hooks != nil && a.hooks.HasEvent(hooks.PreToolUse) {
		res := a.hooks.Run(ctx, hooks.PreToolUse, call.Name, hooks.Payload{
			"prompt_id":   promptID,
			"turn_id":     turnID,
			"tool_name":   call.Name,
			"tool_use_id": call.ID,
			"tool_input":  rawJSONValue(call.Input),
		})
		for _, notice := range res.Notices {
			sink.Notice(notice)
		}
		preContext = append(preContext, res.AdditionalContext...)
		if res.Block {
			reason := res.Reason()
			if reason == "" {
				reason = "blocked by PreToolUse hook"
			}
			return llm.ToolResult{ForID: call.ID, Text: reason, IsError: true}
		}
	}

	r := a.tools.Dispatch(ctx, call)
	if len(preContext) > 0 {
		appendHookContext(&r, preContext)
	}
	if a.hooks != nil && a.hooks.HasEvent(hooks.PostToolUse) {
		res := a.hooks.Run(ctx, hooks.PostToolUse, call.Name, hooks.Payload{
			"prompt_id":     promptID,
			"turn_id":       turnID,
			"tool_name":     call.Name,
			"tool_use_id":   call.ID,
			"tool_input":    rawJSONValue(call.Input),
			"tool_response": toolResponsePayload(r),
		})
		for _, notice := range res.Notices {
			sink.Notice(notice)
		}
		if len(res.AdditionalContext) > 0 {
			appendHookContext(&r, res.AdditionalContext)
		}
		if res.Block {
			reason := res.Reason()
			if reason == "" {
				reason = "blocked by PostToolUse hook"
			}
			r.Text = reason
			r.IsError = true
		}
	}
	return r
}

// kimiWebSearchToolName is Moonshot/Kimi's hosted builtin_function. Unlike other
// providers' server-side web search, Kimi surfaces it as a tool call the client
// must echo back to trigger the search, so the agent intercepts it (design §9).
const kimiWebSearchToolName = "$web_search"

// isKimiWebSearchCall reports whether call is the Kimi web-search builtin that
// the client must echo. It requires the active web-search server tool to be the
// Kimi kind so a stray $web_search call from another provider isn't echoed; when
// the agent's neutral copy carries no resolved Kind (the model proxy fills it in
// per provider and harness can't always tag it), it falls back to the web_search
// feature name so Kimi targets still work.
func (a *Agent) isKimiWebSearchCall(call llm.ToolCall) bool {
	if call.Name != kimiWebSearchToolName {
		return false
	}
	for _, tool := range a.serverTools {
		if tool.Kind == llm.ServerToolKindKimiWebSearch {
			return true
		}
		if tool.Kind == "" && tool.Name == llm.ServerToolWebSearch {
			return true
		}
	}
	return false
}

func invalidToolInputResult(call llm.ToolCall) string {
	msg := "invalid tool call arguments"
	if call.Name != "" {
		msg += " for " + call.Name
	}
	msg += ": " + call.InvalidInputError + ". Provide arguments as a valid JSON object matching the tool schema."
	switch call.Name {
	case "rg", "grep":
		msg += ` For rg/grep, use {"args":["-n","PATTERN","."]}; do not use shell syntax or bare tokens inside JSON.`
	}
	return msg
}

func (a *Agent) prepareToolResult(r llm.ToolResult, sink EventSink) (llm.ToolResult, string) {
	archiver, _ := sink.(ToolResultArchiver)
	return toolresult.PrepareTruncated(r, archiver)
}

func resultBlock(r llm.ToolResult) llm.ContentBlock {
	return llm.ContentBlock{
		Kind:        llm.BlockToolResult,
		ResultForID: r.ForID,
		ResultText:  r.Text,
		ResultError: r.IsError,
	}
}

func pendingPromptWork(sink EventSink) bool {
	coordinator, ok := sink.(PromptWorkCoordinator)
	return ok && coordinator.PendingPromptWork()
}

func waitForPromptWork(ctx context.Context, sink EventSink) (llm.Usage, error) {
	coordinator, ok := sink.(PromptWorkCoordinator)
	if !ok {
		return llm.Usage{}, nil
	}
	if progress, ok := sink.(PromptWorkProgressSink); ok {
		progress.PromptWorkWaitStart()
		defer progress.PromptWorkWaitComplete()
	}
	return coordinator.WaitForPromptWork(ctx)
}

func drainPromptWorkUsage(sink EventSink) llm.Usage {
	coordinator, ok := sink.(PromptWorkCoordinator)
	if !ok {
		return llm.Usage{}
	}
	return coordinator.DrainPromptWorkUsage()
}

func (a *Agent) requestContext(extraContext []string, sink EventSink) []string {
	out := append([]string(nil), extraContext...)
	provider, ok := sink.(RequestContextProvider)
	if !ok {
		return out
	}
	for _, item := range provider.RequestContext() {
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out
}

func rawJSONValue(raw []byte) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func appendHookContext(r *llm.ToolResult, ctx []string) {
	text := llm.RequestContextText(ctx)
	if text == "" {
		return
	}
	if r.Text != "" {
		r.Text += "\n\n"
	}
	r.Text += text
}

func toolResponsePayload(r llm.ToolResult) map[string]any {
	return map[string]any{
		"tool_use_id": r.ForID,
		"text":        r.Text,
		"is_error":    r.IsError,
		"truncated":   r.Truncated,
	}
}

// streamWithRetry runs stream, re-requesting the turn from scratch when it
// fails mid-flight with a retryable error. Partial output from a failed
// attempt is never committed to the transcript; wasted carries the usage
// failed attempts reported (paid for, so counted) — it never drives the
// compaction trigger.
func (a *Agent) streamWithRetry(ctx context.Context, req llm.Request, sink EventSink, turn int, estimate ContextEstimate) (res turnResult, wasted llm.Usage, err error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return turnResult{}, wasted, err
		}
		sink.TurnAttemptStart(turn, attempt+1, estimate)
		res, err = a.stream(ctx, req, sink)
		res.attempts = attempt + 1
		sink.TurnAttemptComplete(TurnAttemptUsage{Turn: turn, Attempt: attempt + 1, Usage: res.usage})
		if err == nil || attempt >= streamRetries || !retryableStreamError(err) {
			return res, wasted, err
		}
		wasted = add(wasted, res.usage)
		delay := retry.Next(attempt, streamRetryAfter(err))
		if abandon, ok := sink.(TurnAttemptAbandonSink); ok {
			abandon.TurnAttemptAbandoned(turn, attempt+1)
		}
		discarded := ""
		if n := totalTokens(res.usage); n > 0 {
			discarded = fmt.Sprintf("; discarded ~%d tokens", n)
		}
		sink.Notice(fmt.Sprintf("[stream interrupted: %v; retrying turn in %s%s]", err, delay, discarded))
		if serr := a.sleep(ctx, delay); serr != nil {
			return turnResult{}, wasted, serr
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryableStreamError reports whether a mid-stream failure may be retried by
// re-requesting the turn. Cancellation is the user's call to stop; a
// non-retryable APIError (invalid_request, auth) will not get better by
// asking again. Everything else — truncated streams, transport resets,
// retryable API errors — is transient (spec §2).
//
// The rate-limit class (HTTP 429/529) is the exception: the provider's connect
// loop already spent its full attempt budget backing off on it (a connect-origin
// rate limit carries the status code), and these recover over minutes, so
// re-running the whole turn would only multiply attempts (up to 3×5=15) and
// hammer a busy API. Transient 500/502/503 keep retrying as before. A mid-stream
// rate-limit frame (no status code) is not connect-exhausted, so it stays
// retryable and still honors its Retry-After hint.
func retryableStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		if retry.RateLimitedStatus(apiErr.StatusCode) {
			return false
		}
		return apiErr.Retryable
	}
	return true
}

func streamRetryAfter(err error) time.Duration {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

var contextWindowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length is\s+([0-9][0-9,]*)`),
	regexp.MustCompile(`(?i)context window(?: is| of)?\s+([0-9][0-9,]*)`),
}

func contextOverflowWindow(err error) (int, bool) {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	code := strings.ToLower(apiErr.Code)
	msg := strings.ToLower(apiErr.Message)
	isOverflow := strings.Contains(code, "context_length") ||
		strings.Contains(code, "context_window") ||
		(strings.Contains(msg, "context") &&
			(strings.Contains(msg, "exceed") ||
				strings.Contains(msg, "maximum") ||
				strings.Contains(msg, "length") ||
				strings.Contains(msg, "requested") ||
				strings.Contains(msg, "too long")))
	if !isOverflow {
		return 0, false
	}
	for _, re := range contextWindowPatterns {
		m := re.FindStringSubmatch(apiErr.Message)
		if len(m) != 2 {
			continue
		}
		n, convErr := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		if convErr == nil && n > 0 {
			return n, true
		}
	}
	return 0, true
}

func (a *Agent) observeContextWindow(window int) bool {
	if window <= 0 {
		return false
	}
	current := a.window()
	if current > 0 && window >= current {
		return false
	}
	if a.observedContextWindow > 0 && window >= a.observedContextWindow {
		return false
	}
	a.observedContextWindow = window
	a.resetResponseState()
	return true
}

func previousResponseRejected(err error) bool {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.Code)
	if strings.Contains(code, "previous_response") {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "previous_response_id") || strings.Contains(msg, "previous response")
}

func storeResponseRejected(err error) bool {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.Code)
	if strings.Contains(code, "store") {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "store") &&
		(strings.Contains(msg, "false") || strings.Contains(msg, "unsupported") || strings.Contains(msg, "not supported"))
}

func stopReasonNotice(reason llm.StopReason) string {
	switch reason {
	case llm.StopMaxTokens:
		return "[stopped: model reached max tokens]"
	case llm.StopStop:
		return "[stopped: stop sequence matched]"
	default:
		return ""
	}
}

// stream consumes one provider stream: it forwards text deltas to the sink,
// assembles completed tool calls in emission order, and captures the final
// usage and stop reason. A terminal stream error is returned with whatever
// partial text streamed so far (for cancel repair).
func (a *Agent) stream(ctx context.Context, req llm.Request, sink EventSink) (turnResult, error) {
	var res turnResult
	var text []byte

	for ev, err := range a.provider.Stream(ctx, req) {
		if err != nil {
			res.text = string(text)
			return res, err
		}
		switch ev.Kind {
		case llm.EventTextDelta:
			text = append(text, ev.Text...)
			sink.TextDelta(ev.Text)
		case llm.EventReasoningSummary:
			if summary := reasoningSummaryText(ev.Text); summary != "" {
				sink.ReasoningSummary(summary)
			}
			if block, ok := reasoningBlock(ev); ok {
				res.reasoning = append(res.reasoning, block)
			}
		case llm.EventAssistantPhase:
			if llm.ValidAssistantPhase(ev.Phase) && ev.Phase != "" {
				res.phase = ev.Phase
				if phaseSink, ok := sink.(AssistantPhaseSink); ok {
					phaseSink.AssistantPhase(ev.Phase)
				}
			}
		case llm.EventToolCallStart:
			sink.ToolUseStart(llm.ToolCall{
				ID:    ev.ToolID,
				Name:  ev.ToolName,
				Input: ev.ToolInput,
			})
		case llm.EventToolCallDelta:
			sink.ToolUseDelta(ev.Index, ev.ArgsDelta)
		case llm.EventToolCallDone:
			res.toolCalls = append(res.toolCalls, llm.ToolCall{
				ID:                ev.ToolID,
				Name:              ev.ToolName,
				Input:             ev.ToolInput,
				InvalidInputError: ev.InvalidInputError,
			})
		case llm.EventUsage:
			if ev.Usage != nil {
				res.usage = mergeUsage(res.usage, *ev.Usage)
			}
		case llm.EventDone:
			if ev.Usage != nil {
				res.usage = mergeUsage(res.usage, *ev.Usage)
			}
			res.stopReason = ev.StopReason
			res.responseID = ev.ResponseID
		}
	}

	res.text = string(text)
	return res, nil
}

func reasoningSummaryText(text string) string {
	return strings.TrimSpace(text)
}

// reasoningBlock converts an EventReasoningSummary into a persistable reasoning
// block. Three payloads must be replayed verbatim on the next turn for the API
// to accept the transcript: signed thinking and redacted thinking (Anthropic),
// and encrypted Responses reasoning items (stateless store=false mode). A plain
// unsigned summary (the display digest) carries none of these and is not stored
// — it only goes to the dedicated sink. Text is kept verbatim (not trimmed) so a
// signed block replays exactly. ok is false when there is nothing to persist.
func reasoningBlock(ev llm.StreamEvent) (llm.ContentBlock, bool) {
	if ev.ReasoningEncrypted != "" {
		return llm.ContentBlock{Kind: llm.BlockReasoning, ReasoningID: ev.ReasoningID, ReasoningEncrypted: ev.ReasoningEncrypted}, true
	}
	if ev.RedactedData != "" {
		return llm.ContentBlock{Kind: llm.BlockRedactedThinking, RedactedData: ev.RedactedData}, true
	}
	if ev.Signature == "" {
		return llm.ContentBlock{}, false
	}
	return llm.ContentBlock{Kind: llm.BlockThinking, Thinking: ev.Text, ThinkingSignature: ev.Signature}, true
}

// textMessage builds the single-text-block message shape shared by user prompts
// and cancel repair.
func (a *Agent) textMessage(role llm.Role, text string) llm.Message {
	return textMessageAt(a.now(), role, text)
}

// drainSteer pops all queued in-prompt steer inputs, joining their text with a
// blank line and concatenating images/context into one steering message. It
// returns an empty input when steering is disabled or nothing is queued. Draining
// is non-blocking so a concurrent Steer caller can never stall the loop; inputs
// queued after this drain are deferred to the next tool round.
func (a *Agent) drainSteer() SteerInput {
	if a.steer == nil {
		return SteerInput{}
	}
	var out SteerInput
	for {
		select {
		case input := <-a.steer:
			if strings.TrimSpace(input.Text) != "" {
				if out.Text != "" {
					out.Text += "\n\n"
				}
				out.Text += input.Text
			}
			out.Images = append(out.Images, cloneImageBlocks(input.Images)...)
			out.RequestContext = append(out.RequestContext, cleanContext(input.RequestContext)...)
		default:
			return out
		}
	}
}

func (a *Agent) partialAssistantMessage(res turnResult) llm.Message {
	msg := a.textMessage(llm.RoleAssistant, res.text)
	msg.Phase = res.phase
	if msg.Phase == "" {
		msg.Phase = llm.AssistantPhaseCommentary
	}
	return msg
}

func (a *Agent) userMessage(text string, images []llm.ContentBlock) llm.Message {
	content := make([]llm.ContentBlock, 0, len(images)+1)
	for _, image := range images {
		if image.Kind == llm.BlockImage {
			content = append(content, image)
		}
	}
	if text != "" || len(content) == 0 {
		content = append(content, llm.ContentBlock{Kind: llm.BlockText, Text: text})
	}
	return llm.Message{Role: llm.RoleUser, Time: a.now(), Content: content}
}

func textMessageAt(at time.Time, role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Time: at, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}}}
}

// assistantMessage builds the assistant message for a completed turn:
// thinking block(s) first (so signed reasoning is replayed before the tool_use
// it justified), then the text block (if any), then tool_use blocks in emission
// order (design §8.1).
func (a *Agent) assistantMessage(res turnResult) llm.Message {
	content := make([]llm.ContentBlock, 0, len(res.reasoning)+1+len(res.toolCalls))
	content = append(content, res.reasoning...)
	if res.text != "" {
		content = append(content, llm.ContentBlock{Kind: llm.BlockText, Text: res.text})
	}
	for _, call := range res.toolCalls {
		content = append(content, llm.ContentBlock{
			Kind:      llm.BlockToolUse,
			ToolUseID: call.ID,
			ToolName:  call.Name,
			ToolInput: call.Input,
		})
	}
	return llm.Message{Role: llm.RoleAssistant, Time: a.now(), Phase: assistantPhase(res), Content: content}
}

func assistantPhase(res turnResult) string {
	if res.phase != "" && llm.ValidAssistantPhase(res.phase) {
		return res.phase
	}
	if res.stopReason == llm.StopToolUse {
		return llm.AssistantPhaseCommentary
	}
	return llm.AssistantPhaseFinal
}

// maxTurnsNotice is the exact guard message printed when the turn budget is
// exhausted (design §8.1).
func maxTurnsNotice(maxTurns int) string {
	return fmt.Sprintf("[stopped: reached max turns (%d)]", maxTurns)
}

func add(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:          a.CostUSD + b.CostUSD,
		CostKnown:        aggregateCostKnown(a, b),
	}
}

func cloneToolSpecs(specs []llm.ToolSchema) []llm.ToolSchema {
	out := append([]llm.ToolSchema(nil), specs...)
	for i := range out {
		out[i].Parameters = append(json.RawMessage(nil), out[i].Parameters...)
	}
	return out
}

func cloneServerTools(serverTools []llm.ServerTool) []llm.ServerTool {
	out := append([]llm.ServerTool(nil), serverTools...)
	for i := range out {
		out[i].Parameters = append(json.RawMessage(nil), out[i].Parameters...)
	}
	return out
}

func steerInputEmpty(input SteerInput) bool {
	if strings.TrimSpace(input.Text) != "" {
		return false
	}
	if len(input.Images) > 0 {
		return false
	}
	for _, item := range input.RequestContext {
		if strings.TrimSpace(item) != "" {
			return false
		}
	}
	return true
}

func cloneImageBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Kind != llm.BlockImage {
			continue
		}
		out = append(out, block)
	}
	return out
}

func cleanContext(context []string) []string {
	out := make([]string, 0, len(context))
	for _, item := range context {
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out
}

func appendPromptContext(extraContext, steerContext []string) []string {
	out := append([]string(nil), extraContext...)
	out = append(out, steerContext...)
	return out
}

// mergeUsage merges a cumulative usage snapshot into acc element-wise. The
// provider contract says snapshots are cumulative; max keeps a zeroed or
// partial late frame from erasing earlier numbers (spec §3).
func mergeUsage(acc, in llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:      max(acc.InputTokens, in.InputTokens),
		OutputTokens:     max(acc.OutputTokens, in.OutputTokens),
		CacheReadTokens:  max(acc.CacheReadTokens, in.CacheReadTokens),
		CacheWriteTokens: max(acc.CacheWriteTokens, in.CacheWriteTokens),
		ReasoningTokens:  max(acc.ReasoningTokens, in.ReasoningTokens),
		CostUSD:          mergeCost(acc, in),
		CostKnown:        mergeCostKnown(acc, in),
	}
}

func mergeCost(acc, in llm.Usage) float64 {
	if in.CostKnown {
		return in.CostUSD
	}
	return acc.CostUSD
}

func mergeCostKnown(acc, in llm.Usage) bool {
	if in.CostKnown {
		return true
	}
	if usageHasTokens(in) {
		return false
	}
	return acc.CostKnown
}

func aggregateCostKnown(a, b llm.Usage) bool {
	aHasUsage := usageHasTokens(a)
	bHasUsage := usageHasTokens(b)
	switch {
	case aHasUsage && !a.CostKnown:
		return false
	case bHasUsage && !b.CostKnown:
		return false
	default:
		return a.CostKnown || b.CostKnown
	}
}

func usageHasTokens(u llm.Usage) bool {
	return u.InputTokens != 0 ||
		u.OutputTokens != 0 ||
		u.CacheReadTokens != 0 ||
		u.CacheWriteTokens != 0 ||
		u.ReasoningTokens != 0
}
