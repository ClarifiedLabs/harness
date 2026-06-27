// Package agent runs one user turn as a loop of model turns until the model
// stops asking for tools, executing each model turn's tool calls in emission order
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
	"hash/fnv"
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
	"harness/internal/tools"
)

// streamRetries is the per-model-turn mid-stream retry budget: a model turn whose stream
// fails after the first byte may be re-requested this many times (spec §2).
// Retries do not consume the maxTurns budget.
const streamRetries = 2

// maxParallelTools bounds concurrent read-only dispatch (spec §8).
const maxParallelTools = 8

// EventSink receives the turn's observable events for rendering. The agent loop
// owns the transcript and the control flow; the sink only reports. Phase 10's
// renderer implements it (design §8.1, §10).
type EventSink interface {
	TextDelta(text string) // incremental assistant text
	ReasoningSummary(text string)
	ModelTurnStart(modelTurn, attempt int, ctx ContextEstimate)
	ModelTurnComplete(usage ModelTurnUsage)
	ToolUseStart(call llm.ToolCall)
	ToolUseDelta(index int, delta string)
	ToolStart(call llm.ToolCall)      // a tool call is about to run
	ToolResult(result llm.ToolResult) // a tool call finished
	Notice(msg string)                // out-of-band notices (max-turns, cancelled)
	TurnComplete(usage TurnUsage)     // end of the turn
}

// AssistantPhaseSink is implemented by sinks that want provider phase metadata
// before the corresponding assistant text is rendered.
type AssistantPhaseSink interface {
	AssistantPhase(phase string)
}

// ToolResultArchive is an optional sink-provided reference to the full raw tool
// output behind a truncated result.
type ToolResultArchive struct {
	DisplayPath string
	ModelPath   string
}

// ToolResultArchiver is implemented by sinks that can persist full raw output
// and return a path the next model turn can read or search.
type ToolResultArchiver interface {
	ArchiveToolResult(llm.ToolResult) (ToolResultArchive, error)
}

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

// TurnUsage is the per-user-turn summary handed to the sink (design §10 usage line).
type TurnUsage struct {
	ModelTurns int
	Usage      llm.Usage
	// Wasted is the subset of Usage spent on model-turn attempts that were
	// discarded and re-requested after a mid-stream failure (r51+r52). It is
	// already included in Usage; surfacing it lets the UI show the retry cost.
	Wasted  llm.Usage
	Context ContextEstimate
}

// ModelTurnUsage is the token accounting for one provider request attempt.
// ModelTurn is the logical model turn in the current user turn; Attempt is 1
// for the first stream request and higher for retry attempts.
type ModelTurnUsage struct {
	ModelTurn int
	Attempt   int
	Usage     llm.Usage
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
	// MaxTurnTokens stops a user turn once accumulated tokens for the turn reach
	// this ceiling; zero means unlimited. Enforcement lives in the turn loop.
	MaxTurnTokens int
	// MaxOutputTokens caps one normal model turn's output; zero uses the shared
	// automatic provider policy. Prewarm and compaction summaries set their own
	// request caps and do not use this value.
	MaxOutputTokens int
	// MaxPromptCostUSD stops a user turn once its accumulated model cost (USD)
	// reaches this ceiling; zero means unlimited. Enforced only for models with
	// catalog pricing (otherwise cost is unknown and the budget cannot fire).
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
	maxTurnTokens             int     // accumulated-token ceiling per user turn; 0 = unlimited
	maxOutputTokens           int     // per-normal-model-turn output cap; 0 = automatic
	maxPromptCostUSD          float64 // accumulated USD ceiling per user turn; 0 = unlimited
	contextWindow             int     // -context-window override; 0 = use the registry default
	observedContextWindow     int     // smaller provider-reported limit learned from an overflow error
	reasoning                 llm.ReasoningConfig
	now                       func() time.Time
	sleep                     func(context.Context, time.Duration) error // mid-stream retry backoff; nil-free, set in New
	compactKeepTurns          int
	compactSummaryMaxTokens   int
	compactToolResultMaxBytes int
	archiveCompaction         CompactionArchiver
	hooks                     *hooks.Runner
	showDiffs                 bool
	responsesStateful         bool
	interactive               bool // 1h Anthropic cache breakpoint; see Options.Interactive
	responseState             llm.ResponseState
	promptCacheSessionID      string
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
	return &Agent{
		provider:                  provider,
		tools:                     registry,
		toolSpecs:                 registry.Specs(),
		registry:                  modelRegistry,
		model:                     opts.Model,
		maxTurns:                  opts.MaxTurns,
		maxTurnTokens:             opts.MaxTurnTokens,
		maxOutputTokens:           opts.MaxOutputTokens,
		maxPromptCostUSD:          opts.MaxPromptCostUSD,
		contextWindow:             opts.ContextWindow,
		reasoning:                 opts.Reasoning,
		now:                       now,
		sleep:                     sleepContext,
		compactKeepTurns:          opts.CompactKeepTurns,
		compactSummaryMaxTokens:   opts.CompactSummaryMaxTokens,
		compactToolResultMaxBytes: opts.CompactToolResultMaxBytes,
		hooks:                     opts.Hooks,
		showDiffs:                 opts.ShowDiffs,
		responsesStateful:         opts.ResponsesStateful,
		interactive:               opts.Interactive,
		promptCacheSessionID:      newPromptCacheSessionID(),
	}
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

// SetHooks replaces the lifecycle hook runner used by subsequent turns.
func (a *Agent) SetHooks(runner *hooks.Runner) { a.hooks = runner }

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
// the message shape used by RunTurnContentWithContext.
func (a *Agent) ContextRequestWithContext(extraContext []string) llm.Request {
	return llm.Request{
		Model:          a.model,
		System:         a.system,
		Messages:       append([]llm.Message(nil), a.transcript...),
		Tools:          cloneToolSpecs(a.toolSpecs),
		Reasoning:      a.reasoning,
		RequestContext: append([]string(nil), extraContext...),
		PromptCacheKey: a.promptCacheKey(),
		LongCacheTTL:   a.interactive,
	}
}

// promptCacheKey is a stable per-session prompt-cache routing hint. Its prefix
// is derived from the system prompt and advertised tool names; the per-agent
// suffix keeps proxy-managed Responses continuation state from leaking across
// independent harness sessions that share the same prompt prefix.
func (a *Agent) promptCacheKey() string {
	prefix := a.promptCachePrefix()
	if a.promptCacheSessionID == "" {
		return prefix
	}
	return prefix + "-" + a.promptCacheSessionID
}

func (a *Agent) promptCachePrefix() string {
	h := fnv.New64a()
	h.Write([]byte(a.system))
	for _, t := range a.toolSpecs {
		h.Write([]byte{0})
		h.Write([]byte(t.Name))
	}
	return "harness-" + strconv.FormatUint(h.Sum64(), 16)
}

func newPromptCacheSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// PrewarmRequest builds a minimal request that writes the prompt cache — the
// stable tools+system prefix plus any existing (e.g. resumed) transcript —
// without producing a real turn, so the first real request reads a warm cache
// instead of paying the cold cache-write latency. ok is false when there is
// nothing cacheable yet. The returned request is a self-contained snapshot.
func (a *Agent) PrewarmRequest() (llm.Request, bool) {
	if a.system == "" && len(a.toolSpecs) == 0 {
		return llm.Request{}, false
	}
	req := a.ContextRequest()
	if len(req.Messages) == 0 {
		// The Messages API requires at least one message; this placeholder rides
		// after the cached prefix and is never persisted.
		req.Messages = []llm.Message{a.userMessage("warm cache", nil)}
	}
	req.MaxTokens = 1                     // smallest legal cap: only the prefill matters
	req.Reasoning = llm.ReasoningConfig{} // no thinking/effort — a pure prefix write
	req.RequestContext = nil
	return req, true
}

// PrewarmFunc captures the current provider and a PrewarmRequest snapshot and
// returns a closure that streams the warm-up request and discards its output.
// Call it on the goroutine that owns the agent (before the input loop); the
// returned closure shares no mutable agent state, so it is safe to run in a
// background goroutine. ok is false when there is nothing to warm.
func (a *Agent) PrewarmFunc() (func(context.Context), bool) {
	req, ok := a.PrewarmRequest()
	if !ok {
		return nil, false
	}
	provider := a.provider
	return func(ctx context.Context) {
		for _, err := range provider.Stream(ctx, req) {
			if err != nil {
				// Best-effort: a failed warm-up just means the first real request
				// pays the cold-cache cost.
				return
			}
		}
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
		RequestContext: extraContext,
	}, a.window())
	est.PayloadSystem = est.System
	est.PayloadTools = est.Tools
	est.PayloadMessages = est.Messages
	est.PayloadTotal = est.Total
	return est
}

// modelTurnResult holds what one model turn produced after assembly.
type modelTurnResult struct {
	text       string
	reasoning  []llm.ContentBlock // thinking / redacted_thinking / reasoning blocks, in arrival order
	toolCalls  []llm.ToolCall
	phase      string
	usage      llm.Usage
	stopReason llm.StopReason
	responseID string
}

func (r modelTurnResult) hasPartialOutput() bool {
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

// ModelTurnAbandonSink is an optional event sink extension for renderers that
// persist replay metadata. It marks a streamed attempt whose visible deltas were
// discarded from the transcript because the model turn will be retried.
type ModelTurnAbandonSink interface {
	ModelTurnAbandoned(modelTurn, attempt int)
}

func (a *Agent) modelRequest(requestContext []string) modelRequest {
	return a.modelRequestForTranscript(requestContext, a.transcript)
}

func (a *Agent) modelRequestForTranscript(requestContext []string, transcript []llm.Message) modelRequest {
	payloadMessages, usedPrevious := a.payloadMessagesIn(transcript)
	estimate := a.estimatePayloadContextForTranscript(requestContext, transcript, payloadMessages)
	req := llm.Request{
		Model:                a.model,
		System:               a.system,
		Messages:             payloadMessages,
		Tools:                cloneToolSpecs(a.toolSpecs),
		Reasoning:            a.reasoning,
		MaxTokens:            a.maxOutputTokens,
		StoreResponse:        a.responsesStateful,
		RequestContext:       append([]string(nil), requestContext...),
		PromptCacheKey:       a.promptCacheKey(),
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
		RequestContext: requestContext,
	}, a.window())
	est.PayloadSystem = payload.System
	est.PayloadTools = payload.Tools
	est.PayloadMessages = payload.Messages
	est.PayloadTotal = payload.Total
	return est
}

func (a *Agent) updateResponseState(res modelTurnResult) {
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
// or after any failure. This turns the per-turn validation cost from O(n²) over
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

// RunTurn appends the user message, then loops model turns until the model
// stops requesting tools or the model-turn budget is hit (design §8.1). Cancellation
// mid-stream applies the §4 cancel repair and returns ctx.Err(); the transcript
// is left valid (re-sendable) in every exit path.
func (a *Agent) RunTurn(ctx context.Context, userText string, sink EventSink) error {
	return a.RunTurnContent(ctx, userText, nil, sink)
}

// RunTurnContent is RunTurn with optional user-provided image blocks. Images
// are placed before text so vision providers see the visual context first.
func (a *Agent) RunTurnContent(ctx context.Context, userText string, images []llm.ContentBlock, sink EventSink) error {
	return a.RunTurnContentWithContext(ctx, userText, images, nil, 0, sink)
}

// RunTurnContentWithContext is RunTurnContent plus request-only hook context.
// extraContext is visible to model requests for this turn but is not persisted
// into the transcript.
func (a *Agent) RunTurnContentWithContext(ctx context.Context, userText string, images []llm.ContentBlock, extraContext []string, turnID int, sink EventSink) error {
	a.transcript = append(a.transcript, a.userMessage(userText, images))

	var total llm.Usage
	var lastInput int // input tokens the final model turn reported (drives the trigger)
	var lastContext ContextEstimate
	modelTurns := 0
	unlimited := a.maxTurns <= 0
	stopHookActive := false
	var guard turnGuard
	var wastedTotal llm.Usage // tokens spent on retried-and-discarded model-turn attempts (r51+r52)
	appendBoundary := 0       // transcript length measured by lastInput (drives the r44 trigger)

	for unlimited || modelTurns < a.maxTurns {
		// Live-transcript retention (design §12, r9+r20): shrink stale large
		// tool outputs and aged images before building the request, so they are
		// not re-sent verbatim every turn. Pure local edit, invariant-preserving.
		a.applyRetention(sink)
		requestContext := a.requestContext(extraContext, sink)
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
			// would force a full-transcript re-estimate every model turn with zero
			// progress (no-op churn).
			if compUsage, changed, err := a.compactTriggered(ctx, sink, "auto"); err == nil && changed {
				total = add(total, compUsage)
				// The old reported count no longer describes the compacted
				// transcript and would re-trigger every model turn.
				lastInput = 0
				appendBoundary = 0
				requestContext = a.requestContext(extraContext, sink)
				modelReq = a.modelRequest(requestContext)
				lastContext = modelReq.estimate
			}
		}
		if err := a.validateTranscript("before model request"); err != nil {
			sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
			return err
		}
		modelReq = a.countModelRequestInput(ctx, modelReq)
		lastContext = modelReq.estimate
		if a.overThreshold(modelReq.estimate.Total) {
			if compUsage, changed, err := a.compactTriggered(ctx, sink, "input-count"); err == nil && changed {
				total = add(total, compUsage)
				lastInput = 0
				appendBoundary = 0
				requestContext = a.requestContext(extraContext, sink)
				modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
				lastContext = modelReq.estimate
			}
		}
		if err := a.validateTranscript("before model request"); err != nil {
			sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
			return err
		}

		// The request we are about to send reflects the current transcript;
		// remember its length so the next trigger only re-estimates what gets
		// appended after the response we are about to measure.
		appendBoundary = len(a.transcript)
		res, wasted, err := a.streamWithRetry(ctx, modelReq.request, sink, modelTurns+1, lastContext)
		if err != nil && !res.hasPartialOutput() {
			if learned, ok := contextOverflowWindow(err); ok {
				if learned > 0 && a.observeContextWindow(learned) {
					sink.Notice(fmt.Sprintf("[context window adjusted: provider reported %d tokens; retrying request]", learned))
				} else {
					sink.Notice("[context overflow: compacting and retrying request]")
				}
				if compUsage, changed, cerr := a.compactTriggered(ctx, sink, "context-overflow"); cerr == nil && changed {
					total = add(total, compUsage)
					lastInput = 0
					appendBoundary = 0
				}
				requestContext = a.requestContext(extraContext, sink)
				modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
				lastContext = modelReq.estimate
				res, wasted, err = a.streamWithRetry(ctx, modelReq.request, sink, modelTurns+1, lastContext)
			}
		}
		if err != nil && modelReq.request.StoreResponse && !res.hasPartialOutput() && storeResponseRejected(err) {
			a.SetResponsesStateful(false)
			sink.Notice("[responses state disabled: provider rejected stored responses; retrying stateless]")
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = modelReq.estimate
			var retryWasted llm.Usage
			res, retryWasted, err = a.streamWithRetry(ctx, modelReq.request, sink, modelTurns+1, lastContext)
			wasted = add(wasted, retryWasted)
		}
		if err != nil && modelReq.usedPrevious && !res.hasPartialOutput() && previousResponseRejected(err) {
			a.resetResponseState()
			sink.Notice("[responses state reset: previous response unavailable; retrying with full context]")
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = modelReq.estimate
			var retryWasted llm.Usage
			res, retryWasted, err = a.streamWithRetry(ctx, modelReq.request, sink, modelTurns+1, lastContext)
			wasted = add(wasted, retryWasted)
		}
		modelTurns++
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
			}
			if cancelled {
				sink.Notice("[cancelled]")
			}
			if verr := a.validateTranscript("after failed model turn"); verr != nil {
				err = errors.Join(err, verr)
			}
			sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
			return err
		}

		a.transcript = append(a.transcript, a.assistantMessage(res))
		a.updateResponseState(res)

		if res.stopReason != llm.StopToolUse {
			if notice := stopReasonNotice(res.stopReason); notice != "" {
				sink.Notice(notice)
			}
			if a.hooks != nil && !stopHookActive && a.hooks.HasEvent(hooks.Stop) {
				hookRes := a.hooks.Run(ctx, hooks.Stop, "", hooks.Payload{
					"turn_id":                turnID,
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
					a.transcript = append(a.transcript, a.textMessage(llm.RoleUser, "[hook Stop requested continuation]\n"+reason))
					stopHookActive = true
					if err := a.validateTranscript("after stop hook continuation"); err != nil {
						sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
						return err
					}
					continue
				}
			}
			if err := a.validateTranscript("after assistant turn"); err != nil {
				sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
				return err
			}
			break
		}

		results, toolUsage := a.dispatchCalls(ctx, res.toolCalls, turnID, sink)
		total = add(total, toolUsage)
		a.transcript = append(a.transcript, llm.Message{
			Role:    llm.RoleUser,
			Time:    a.now(),
			Content: results,
		})
		if err := a.validateTranscript("after tool results"); err != nil {
			sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
			return err
		}

		// Runaway guardrails (design §8.1). The transcript now ends on a closed
		// tool_use/tool_result pair, so injecting a steering RoleUser message or
		// breaking here keeps the §4 invariant intact.
		guard.recordTools(res.toolCalls, results)

		// Hard stop: an unrelenting error storm. Finalize with a tools-disabled
		// summary so the turn ends on an assistant message, not a dangling result.
		if guard.shouldBreakErrors() {
			sink.Notice(errorStormNotice(guard.errorRuns))
			fu, fctx := a.finalizeWithSummary(ctx, sink, requestContext, modelTurns+1)
			total = add(total, fu)
			lastContext = fctx
			break
		}

		// Hard stop: a byte-identical successful repeat loop that ignored the one
		// steering nudge. Finalize the same way so the turn ends on an assistant
		// message (the success-loop analogue of the error-storm break).
		if guard.shouldBreakRepeat() {
			sink.Notice(repeatLoopNotice(guard.repeatRuns))
			fu, fctx := a.finalizeWithSummary(ctx, sink, requestContext, modelTurns+1)
			total = add(total, fu)
			lastContext = fctx
			break
		}

		// Token budget: stop before the next (paid) request. No final summary —
		// the whole point is to stop spending.
		if a.maxTurnTokens > 0 && totalTokens(total) >= a.maxTurnTokens {
			sink.Notice(turnTokenBudgetNotice(a.maxTurnTokens))
			break
		}

		// Cost budget: the dollar analogue of the token budget, same hard stop.
		// Only fires for models with catalog pricing — Cost reports known=false
		// otherwise, so an unpriced model silently has no cost ceiling.
		if a.maxPromptCostUSD > 0 {
			if total.CostKnown && total.CostUSD >= a.maxPromptCostUSD {
				sink.Notice(promptCostBudgetNotice(a.maxPromptCostUSD, total.CostUSD))
				break
			}
		}

		// One steering nudge per condition (repetition / error storm share a slot).
		if msg := guard.steerMessage(); msg != "" {
			a.transcript = append(a.transcript, a.textMessage(llm.RoleUser, msg))
			if err := a.validateTranscript("after loop-guard steer"); err != nil {
				sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
				return err
			}
		}

		// Wrap-up nudge once, before the final allowed model turn, so the model
		// can stop calling tools and summarize within budget (r3).
		if !unlimited && modelTurns == a.maxTurns-1 && !guard.wrapUpSteered {
			guard.wrapUpSteered = true
			a.transcript = append(a.transcript, a.textMessage(llm.RoleUser, wrapUpSteer))
			if err := a.validateTranscript("after wrap-up steer"); err != nil {
				sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
				return err
			}
		}

		if !unlimited && modelTurns >= a.maxTurns {
			sink.Notice(maxTurnsNotice(a.maxTurns))
			fu, fctx := a.finalizeWithSummary(ctx, sink, requestContext, modelTurns+1)
			total = add(total, fu)
			lastContext = fctx
			break
		}
	}

	// Post-turn compaction trigger (design §12, §8.1): fires after the turn
	// completes, before returning to the prompt. The summary call's usage folds
	// into the turn total so session totals (via the sink) include compaction. A
	// compaction error never fails the turn — the warning was already reported and
	// the transcript was kept intact.
	lastContext = a.estimateContext(a.requestContext(extraContext, sink))
	if compUsage, changed, err := a.MaybeCompact(ctx, a.triggerTokens(lastInput, appendBoundary), sink); err == nil && changed {
		total = add(total, compUsage)
		lastContext = a.estimateContext(a.requestContext(extraContext, sink))
	}
	if err := a.validateTranscript("after turn"); err != nil {
		sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
		return err
	}

	sink.TurnComplete(TurnUsage{ModelTurns: modelTurns, Usage: total, Wasted: wastedTotal, Context: lastContext})
	return nil
}

// finalizeWithSummary issues one final model request with tools disabled so a
// turn that hit a hard stop right after tool dispatch ends on an assistant
// summary instead of a dangling tool_result (r3+r49). It is best-effort: a
// failed or empty summary leaves the already-valid transcript untouched. Any
// tool calls the model emits despite tools being disabled are ignored — only
// the summary text is appended, so no unanswered tool_use can be created. It
// returns the request's usage (counted toward the turn total) and estimate.
func (a *Agent) finalizeWithSummary(ctx context.Context, sink EventSink, requestContext []string, modelTurn int) (llm.Usage, ContextEstimate) {
	modelReq := a.modelRequest(requestContext)
	modelReq.request.Tools = nil // no tools: force a text-only wind-down
	res, wasted, err := a.streamWithRetry(ctx, modelReq.request, sink, modelTurn, modelReq.estimate)
	usage := add(res.usage, wasted)
	if err != nil {
		a.resetResponseState()
		return usage, modelReq.estimate
	}
	if strings.TrimSpace(res.text) == "" {
		return usage, modelReq.estimate
	}
	msg := a.textMessage(llm.RoleAssistant, res.text)
	msg.Phase = llm.AssistantPhaseFinal
	a.transcript = append(a.transcript, msg)
	a.updateResponseState(res)
	return usage, modelReq.estimate
}

// dispatchCalls runs one model turn's tool calls. Consecutive read-only calls
// dispatch concurrently when tool hooks are inactive; mutating calls remain ordering
// barriers. Sink events and the returned blocks are in emission order either way,
// and the sink is only ever called from this goroutine (spec §8).
func (a *Agent) dispatchCalls(ctx context.Context, calls []llm.ToolCall, turnID int, sink EventSink) ([]llm.ContentBlock, llm.Usage) {
	blocks := make([]llm.ContentBlock, len(calls))
	var total llm.Usage

	toolHooksActive := a.hooks != nil && (a.hooks.HasEvent(hooks.PreToolUse) || a.hooks.HasEvent(hooks.PostToolUse))
	for i := 0; i < len(calls); {
		if toolHooksActive || !a.tools.CallReadOnly(calls[i]) {
			block, usage := a.dispatchSequentialCall(ctx, calls[i], turnID, sink)
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
			block, usage := a.dispatchSequentialCall(ctx, calls[start], turnID, sink)
			blocks[start] = block
			total = add(total, usage)
			continue
		}

		usage := a.dispatchReadOnlyBatch(ctx, calls[start:i], blocks[start:i], sink)
		total = add(total, usage)
	}
	return blocks, total
}

func (a *Agent) dispatchSequentialCall(ctx context.Context, call llm.ToolCall, turnID int, sink EventSink) (llm.ContentBlock, llm.Usage) {
	sink.ToolStart(call)
	diffState := a.snapshotToolDiff(call)
	r := a.dispatchOne(ctx, call, turnID, sink)
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

func (a *Agent) dispatchOne(ctx context.Context, call llm.ToolCall, turnID int, sink EventSink) llm.ToolResult {
	if call.InvalidInputError != "" {
		return llm.ToolResult{ForID: call.ID, Text: invalidToolInputResult(call), IsError: true}
	}

	var preContext []string
	if a.hooks != nil && a.hooks.HasEvent(hooks.PreToolUse) {
		res := a.hooks.Run(ctx, hooks.PreToolUse, call.Name, hooks.Payload{
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
			return llm.ToolResult{ForID: call.ID, Text: "error: " + reason, IsError: true}
		}
	}

	r := a.tools.Dispatch(ctx, call)
	if len(preContext) > 0 {
		appendHookContext(&r, preContext)
	}
	if a.hooks != nil && a.hooks.HasEvent(hooks.PostToolUse) {
		res := a.hooks.Run(ctx, hooks.PostToolUse, call.Name, hooks.Payload{
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
			r.Text = "error: " + reason
			r.IsError = true
		}
	}
	return r
}

func invalidToolInputResult(call llm.ToolCall) string {
	msg := "error: invalid tool call arguments"
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
	if !r.Truncated {
		return r, ""
	}
	msg := fmt.Sprintf("[tool result truncated: showing %s of %s", tools.HumanBytes(r.ShownBytes), tools.HumanBytes(r.OriginalBytes))
	archiver, ok := sink.(ToolResultArchiver)
	if !ok {
		return r, msg + "]"
	}
	archive, err := archiver.ArchiveToolResult(r)
	if err != nil {
		return r, fmt.Sprintf("[tool result truncated; full output archive failed: %v]", err)
	}
	if archive.DisplayPath != "" {
		msg += "; full output: " + archive.DisplayPath
	}
	if archive.ModelPath != "" {
		r.Text += "\n" + archivedToolResultHint(archive.ModelPath)
	}
	return r, msg + "]"
}

func archivedToolResultHint(path string) string {
	quoted := strconv.Quote(path)
	return fmt.Sprintf(`[full output archived at %s; use read_file {"path":%s,"offset":1,"limit":200} or rg {"args":["-n","<pattern>",%s]} to inspect it]`, quoted, quoted, quoted)
}

func resultBlock(r llm.ToolResult) llm.ContentBlock {
	return llm.ContentBlock{
		Kind:        llm.BlockToolResult,
		ResultForID: r.ForID,
		ResultText:  r.Text,
		ResultError: r.IsError,
	}
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

// streamWithRetry runs stream, re-requesting the model turn from scratch when it
// fails mid-flight with a retryable error. Partial output from a failed
// attempt is never committed to the transcript; wasted carries the usage
// failed attempts reported (paid for, so counted) — it never drives the
// compaction trigger.
func (a *Agent) streamWithRetry(ctx context.Context, req llm.Request, sink EventSink, modelTurn int, estimate ContextEstimate) (res modelTurnResult, wasted llm.Usage, err error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return modelTurnResult{}, wasted, err
		}
		sink.ModelTurnStart(modelTurn, attempt+1, estimate)
		res, err = a.stream(ctx, req, sink)
		sink.ModelTurnComplete(ModelTurnUsage{ModelTurn: modelTurn, Attempt: attempt + 1, Usage: res.usage})
		if err == nil || attempt >= streamRetries || !retryableStreamError(err) {
			return res, wasted, err
		}
		wasted = add(wasted, res.usage)
		delay := retry.Next(attempt, streamRetryAfter(err))
		if abandon, ok := sink.(ModelTurnAbandonSink); ok {
			abandon.ModelTurnAbandoned(modelTurn, attempt+1)
		}
		discarded := ""
		if n := totalTokens(res.usage); n > 0 {
			discarded = fmt.Sprintf("; discarded ~%d tokens", n)
		}
		sink.Notice(fmt.Sprintf("[stream interrupted: %v; retrying model turn in %s%s]", err, delay, discarded))
		if serr := a.sleep(ctx, delay); serr != nil {
			return modelTurnResult{}, wasted, serr
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
// re-requesting the model turn. Cancellation is the user's call to stop; a
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
func (a *Agent) stream(ctx context.Context, req llm.Request, sink EventSink) (modelTurnResult, error) {
	var res modelTurnResult
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

func (a *Agent) partialAssistantMessage(res modelTurnResult) llm.Message {
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

// assistantMessage builds the assistant message for a completed model turn:
// thinking block(s) first (so signed reasoning is replayed before the tool_use
// it justified), then the text block (if any), then tool_use blocks in emission
// order (design §8.1).
func (a *Agent) assistantMessage(res modelTurnResult) llm.Message {
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

func assistantPhase(res modelTurnResult) string {
	if res.phase != "" && llm.ValidAssistantPhase(res.phase) {
		return res.phase
	}
	if res.stopReason == llm.StopToolUse {
		return llm.AssistantPhaseCommentary
	}
	return llm.AssistantPhaseFinal
}

// maxTurnsNotice is the exact guard message printed when the model-turn budget is
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
