// Package agent runs one user prompt as a loop of turns until the model stops
// asking for tools, executing each turn's tool calls with default parallelism,
// explicit sequential barriers, and emission-ordered overlapping file mutations
// (best-effort on shared cwd/files; no sandbox), while upholding the transcript
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"harness/internal/diff"
	"harness/internal/hooks"
	"harness/internal/inputimage"
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

// maxStreamRetryAfter keeps a provider-supplied delay from parking an
// interactive prompt for minutes or hours. This covers rate-limit errors
// delivered inside an otherwise successful HTTP stream, which bypass the
// connect-level Retry-After guard.
const maxStreamRetryAfter = time.Minute

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

// SteerDeliveredSink is implemented by sinks that want to know when queued
// in-prompt steer input is injected as the Origin=MessageOriginSteer RoleUser
// message ahead of the next model request. It fires only after the message is
// appended and the retained transcript validates, never on the rollback path,
// and never when steering is disabled.
type SteerDeliveredSink interface {
	SteerDelivered(input SteerInput)
}

// ModelRequestEventSink receives diagnostics-only model request lifecycle
// telemetry. Implementations may render or persist it, but it must never be
// added to the agent transcript or subsequent model requests.
type ModelRequestEventSink interface {
	ModelRequestEvent(llm.ModelRequestEvent)
}

// ModelErrorDiagnostic is the safe structured record emitted for a final model
// failure carrying a proxy compatibility classification. Message and Code have
// already been sanitized by the proxy; Diagnostic contains only correlation and
// bounded request-shape metadata.
type ModelErrorDiagnostic struct {
	Prompt     int
	Turn       int
	Attempt    int
	StatusCode int
	Code       string
	Message    string
	Diagnostic *llm.APIErrorDiagnostic
}

// ModelErrorDiagnosticSink is optionally implemented by event sinks that want a
// durable structured record in addition to the ordinary user-facing notice. It
// deliberately does not expand EventSink, so non-UI sinks remain compatible.
type ModelErrorDiagnosticSink interface {
	ModelErrorDiagnostic(ModelErrorDiagnostic)
}

// HookDiagnosticSink optionally receives bounded hook execution diagnostics.
type HookDiagnosticSink interface {
	HookDiagnostic(hooks.Diagnostic)
}

// EvaluatorResultSink optionally receives bounded semantic outcomes from Stop
// hooks. These are diagnostics-only events; repair context is added separately
// by the agent when a rejecting result is allowed to continue the prompt.
type EvaluatorResultSink interface {
	EvaluatorResult(hooks.EvaluatorResult)
}

// StagnationNudgeSink owns the replayable trajectory decision. It returns true
// only after a lane-scoped trigger has been durably recorded.
type StagnationNudgeSink interface {
	TryStagnationNudge(threshold int) bool
}

// StagnationNudgeThreshold is the validated trigger: real
// successful evaluator trajectories peaked at one, while the synthetic
// plateau/cycle oracle reaches two.
const StagnationNudgeThreshold = 2

const stagnationNudgeContext = `[host strategy reset]
The evaluator has rejected or failed to improve two consecutive candidates in the same evaluation lane. Before changing anything else, stop repeating the current approach. Re-read the latest evaluator evidence and relevant current state, form a materially different hypothesis, and test that strategy. Preserve verified progress, stay within the task, and do not bypass the evaluator.`

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
	ToolDiff(call llm.ToolCall, path, text string)
}

// ToolMutationSink receives host-derived paths after a mutation-reporting tool
// completes successfully. It is diagnostics-only and never changes the tool
// result or model transcript.
type ToolMutationSink interface {
	ToolMutation(call llm.ToolCall, paths []string)
}

// DelegateProgressSnapshot is a best-effort, lock-protected snapshot of one
// delegate run's live activity, reported by the child sink to the parent
// renderer's wait ticker. It is diagnostic only: never persisted and never fed
// to the model. Zero values render as "no stats yet".
//
// The live object is a func() DelegateProgressSnapshot closure created in
// internal/delegate (which imports this package) and carried as an opaque
// `any` through internal/tools and internal/background so neither of those
// packages needs to import agent. The renderer type-asserts the `any` back to
// the concrete closure type to read the snapshot.
type DelegateProgressSnapshot struct {
	Turn     int
	Attempt  int
	Tools    int    // count of ToolStart calls seen so far
	Agent    string // child agent name, for the background summary label
	Context  ContextEstimate
	Usage    llm.Usage // last TurnAttemptComplete usage
	Finished bool      // set once the run returns (success or failure)
}

// ToolProgressSink is optionally implemented by sinks that want live child-run
// progress attached to an outstanding tool call's wait ticker. Set is called
// with a progress closure (opaque `any`) before the call's ToolResult, and
// cleared with nil after. Tools that do not support live progress are simply
// skipped.
type ToolProgressSink interface {
	ToolProgress(call llm.ToolCall, progress any)
}

// RetentionRequestMode describes the request shape before or after a retention
// observation. Empty means the mode was not recorded by an older event.
type RetentionRequestMode string

const (
	RetentionRequestModeFull           RetentionRequestMode = "full"
	RetentionRequestModeStatefulSuffix RetentionRequestMode = "stateful_suffix"
	RetentionRequestModeStateless      RetentionRequestMode = "stateless"
)

// RetentionEvent describes one transcript-retention epoch. It is diagnostics
// only and is never added to the transcript or provider request.
type RetentionEvent struct {
	Policy              string
	Trigger             string
	BlocksTrimmed       int
	BytesBefore         int
	BytesAfter          int
	ContextTokensBefore int
	ContextTokensAfter  int
	ResponseStateReset  bool
	NextRequestStateful bool

	DecisionContextTokens     int
	DecisionContextSource     string
	LocalEstimateTokensBefore int
	LocalEstimateTokensAfter  int
	EstimatedTokensRemoved    int
	BytesRemoved              int
	MeasurementAnchorReset    bool
	ContinuationStatePresent  bool
	ContinuationStateReset    bool
	PreviousRequestMode       RetentionRequestMode
	NextRequestMode           RetentionRequestMode
}

// RetentionEventSink is implemented by sinks that persist retention decisions
// and their effect on Responses continuation.
type RetentionEventSink interface {
	RetentionApplied(RetentionEvent)
}

// RetentionPolicy selects when the live transcript retention pass runs.
type RetentionPolicy string

const (
	// RetentionPolicyAuto uses pressure epochs for every provider.
	RetentionPolicyAuto RetentionPolicy = "auto"
	// RetentionPolicyAge runs the legacy age-based pass before every request.
	RetentionPolicyAge RetentionPolicy = "age"
	// RetentionPolicyPressure runs hysteretic context-pressure epochs.
	RetentionPolicyPressure RetentionPolicy = "pressure"
	// RetentionPolicyDisabled turns off live retention while leaving compaction
	// and provider-overflow recovery unchanged.
	RetentionPolicyDisabled RetentionPolicy = "disabled"
)

// HookContextReceiver is implemented by sinks that can keep hook-generated
// context available for later turns without adding it to the saved transcript.
type HookContextReceiver interface {
	AddHookContext([]string)
}

// RequestContextProvider is implemented by sinks that can add fresh
// request-only context before conversational model rounds. RequestContext may
// consume one-shot signals as it attaches them; sizing-only paths use
// RequestContextPeeker when available. Transport and compatibility retries
// reuse attached context, while a retry after a transcript rewrite may refresh
// and merge it.
type RequestContextProvider interface {
	RequestContext() []string
}

// RequestContextPeeker is implemented by sinks whose RequestContext consumes
// one-shot signals. PeekRequestContext returns what the next request would
// carry without consuming anything, for local sizing estimates.
type RequestContextPeeker interface {
	PeekRequestContext() []string
}

// TranscriptRewriteSink is implemented by sinks that need to recover
// request-only state after compaction or another transcript rewrite.
type TranscriptRewriteSink interface {
	TranscriptRewritten()
}

// ContextEpochSink may replace a closed-turn transcript at a semantic work
// boundary. It is consulted only after complete tool results have been
// appended, so implementations never observe or create dangling tool calls.
type ContextEpochSink interface {
	TakeContextEpoch(before []llm.Message) (after []llm.Message, applied bool, err error)
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
	// CorrelationID is opaque caller metadata. The agent never sends it to the
	// model; interactive protocol drivers use it only when recovering an input
	// that was not consumed before the prompt ended.
	CorrelationID string
	// DeliveryMetadata is opaque caller metadata retained while inputs are queued,
	// combined, delivered, or recovered. The agent never interprets or sends it to
	// the model.
	DeliveryMetadata []any
}

// TurnUsage is the accounting for one conversational turn. A turn contains one
// accepted provider response (plus any transport retries or discarded
// request-shape attempts) and the complete tool result batch requested by that
// response.
type TurnUsage struct {
	Turn     int
	Attempts int
	Usage    llm.Usage
	Wasted   llm.Usage
	Context  ContextEstimate
}

// ClosureTrigger records why Harness first entered a prompt wind-down phase.
// It is orthogonal to TerminationReason, which continues to describe loop control.
type ClosureTrigger string

const (
	ClosureTriggerTurnBudget  ClosureTrigger = "turn_budget"
	ClosureTriggerStagnation  ClosureTrigger = "stagnation"
	ClosureTriggerRepeatGuard ClosureTrigger = "repeat_guard"
	ClosureTriggerErrorGuard  ClosureTrigger = "error_guard"
)

// WorkflowOutcome is optional orchestrator-supplied task state. Harness never
// infers semantic completion from a final model message.
type WorkflowOutcome string

const (
	WorkflowOutcomeComplete   WorkflowOutcome = "complete"
	WorkflowOutcomeWaiting    WorkflowOutcome = "waiting"
	WorkflowOutcomeBlocked    WorkflowOutcome = "blocked"
	WorkflowOutcomeEscalated  WorkflowOutcome = "escalated"
	WorkflowOutcomeInProgress WorkflowOutcome = "in_progress"
	WorkflowOutcomeUnknown    WorkflowOutcome = "unknown"
)

// WorkflowStatus is a bounded optional status seam for orchestrators. Available
// distinguishes an absent provider from a provider that explicitly reports an
// unknown outcome. RemainingRequirements is nil when not supplied.
type WorkflowStatus struct {
	Available             bool
	Outcome               WorkflowOutcome
	RemainingRequirements *int
	ExpectedWait          bool
}

// WorkflowStatusProvider is optionally implemented by an EventSink. Detailed
// model-visible requirements continue to use RequestContextProvider.
type WorkflowStatusProvider interface {
	WorkflowStatus() WorkflowStatus
}

// ClosureEvent is diagnostics-only and records the first closure trigger at the
// point it is selected, including optional workflow status sampled then.
type ClosureEvent struct {
	Trigger        ClosureTrigger
	Turn           int
	WorkflowStatus WorkflowStatus
}

type ClosureEventSink interface {
	ClosureStarted(ClosureEvent)
}

// TurnProgressSink optionally receives diagnostics-only semantic activity after
// completed tool turns. These events never enter model history.
type TurnProgressSink interface {
	TurnProgress(TurnProgress)
}

// PromptUsage is the per-prompt summary handed to the sink (design §10 usage line).
type PromptUsage struct {
	Turns       int
	Usage       llm.Usage
	Compactions int // successful transcript rewrites during this prompt
	// Maintenance is the subset of Usage spent on model calls that are not
	// conversational turns, such as automatic compaction.
	Maintenance llm.Usage
	// Wasted is the subset of Usage spent on turn attempts discarded after a
	// mid-stream failure or a compatibility/request-shape rebuild. It is already
	// included in Usage; surfacing it lets the UI show the retry cost.
	Wasted              llm.Usage
	Context             ContextEstimate
	TerminationReason   TerminationReason
	ClosureTrigger      ClosureTrigger
	ClosureTurn         int
	TurnBudgetExhausted bool
	WorkflowStatus      WorkflowStatus
}

// PromptCheckpointKind identifies a resumable model boundary. Request
// boundaries contain a valid transcript ready for the provider. Tool-dispatch
// boundaries intentionally contain an assistant tool-use message whose results
// are not complete yet. Closed-turn boundaries contain a fully validated turn.
type PromptCheckpointKind string

const (
	PromptCheckpointRequestBoundary PromptCheckpointKind = "request_boundary"
	PromptCheckpointToolDispatch    PromptCheckpointKind = "tool_dispatch"
	PromptCheckpointClosedTurn      PromptCheckpointKind = "closed_turn"
)

// PromptCheckpoint is the exact prompt accounting at a resumable boundary.
// The sink can snapshot the Agent transcript and provider continuation state
// synchronously while the loop is paused at that boundary.
type PromptCheckpoint struct {
	Kind  PromptCheckpointKind
	Turn  int
	Usage PromptUsage
}

// PromptCheckpointSink is implemented by durable root and child session sinks.
// Checkpoint failures are reported by the sink and do not corrupt agent-loop
// control flow or turn an otherwise successful model/tool turn into a failure.
type PromptCheckpointSink interface {
	PromptCheckpoint(PromptCheckpoint)
}

// TerminationReason records why Harness stopped a prompt loop. It describes
// loop control, not semantic task completion.
type TerminationReason string

const (
	TerminationModelCompleted TerminationReason = "model_completed"
	TerminationTurnLimit      TerminationReason = "turn_limit"
	TerminationTokenLimit     TerminationReason = "token_limit"
	TerminationCostLimit      TerminationReason = "cost_limit"
	TerminationRepeatGuard    TerminationReason = "repeat_guard"
	TerminationErrorGuard     TerminationReason = "error_guard"
	TerminationCancelled      TerminationReason = "cancelled"
	TerminationError          TerminationReason = "error"
)

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
const (
	ContextEstimateSourceBytes              = "bytes"
	ContextEstimateSourceProviderCount      = "provider_count"
	ContextEstimateSourceResponseUsageDelta = "response_usage_delta"
)

type ContextEstimate struct {
	Total    int
	Window   int
	System   int
	Tools    int
	Messages int
	Source   string

	PayloadTotal    int
	PayloadSystem   int
	PayloadTools    int
	PayloadMessages int
	PayloadSource   string

	// ProviderInput* records the provider observation independently from the
	// logical and request-payload estimates. Unknown-scope continuation counts
	// remain observable without being applied to either accounting view.
	ProviderInputTokens int
	ProviderInputSource string
	ProviderInputScope  llm.InputTokenCountScope
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
	// ReasoningReplayDomain identifies model targets that can safely replay the
	// same provider-owned opaque reasoning. Empty preserves the standalone
	// Agent zero-value behavior.
	ReasoningReplayDomain string
	// ServerTools are provider-hosted tools such as web_search that are declared
	// alongside local function tools but handled entirely by the provider.
	ServerTools []llm.ServerTool
	// Now stamps transcript messages. Nil defaults to time.Now.
	Now func() time.Time
	// CompactKeepTurns controls how many whole recent turns remain verbatim after
	// compaction. Zero uses the default.
	CompactKeepTurns int
	// CompactKeepTokens is the desired approximate size of the raw recent suffix.
	// Whole rounds are retained until this target is reached, subject to
	// CompactKeepTurns. Zero uses the default.
	CompactKeepTokens int
	// CompactTriggerPercent and CompactTargetPercent control automatic triggering
	// and the post-compaction low-water mark. Zero uses the defaults.
	CompactTriggerPercent int
	CompactTargetPercent  int
	// DisableAutoCompaction suppresses threshold-based compaction while preserving
	// manual compaction and provider-overflow recovery. The inverted setting keeps
	// the Options zero value enabled.
	DisableAutoCompaction bool
	// CompactSummaryMaxTokens caps summarization output. Zero uses the default.
	CompactSummaryMaxTokens int
	// CompactTimeout bounds the complete model summarization phase, including
	// chunking and retries. Non-positive uses the default.
	CompactTimeout time.Duration
	// CompactToolResultMaxBytes caps old tool-result bodies before they are sent
	// to the summarizer. Zero uses the default; negative disables this pre-pass.
	CompactToolResultMaxBytes int
	// Hooks runs configured lifecycle hooks. Nil disables hooks.
	Hooks *hooks.Runner
	// StagnationNudge enables one visible strategy-reset instruction per
	// evaluator lane after StagnationNudgeThreshold consecutive non-improvements.
	StagnationNudge bool
	// ShowDiffs emits per-tool-call file diffs for built-in file mutation tools.
	ShowDiffs bool
	// ResponsesStateful enables Responses API previous_response_id chaining.
	// Main only sets it when the selected provider is Responses-capable.
	ResponsesStateful bool
	// NativeCompaction enables an explicitly advertised provider-native
	// compaction capability. The optional provider interface is still checked at
	// runtime; textual compaction remains the fallback.
	NativeCompaction bool
	// RetentionPolicy selects live-transcript retention behavior. The zero value
	// uses the provider-aware automatic policy.
	RetentionPolicy RetentionPolicy
	// RetentionFloorTokens is an absolute-context fallback for the pressure
	// retention epoch: when the estimated context exceeds it, the same
	// hysteretic trim pass runs even below the window-percentage high-water
	// mark (60%), which on very large windows is otherwise never reached. Zero
	// disables the floor. Opt-in: trimming rewrites history and invalidates the
	// cache prefix from the first trimmed block.
	RetentionFloorTokens int
	// RetentionKeepTurns is how many recent turns keep their tool results and
	// inputs verbatim during live retention. Zero uses the 4-turn default;
	// positive decouples the retention age from the compaction suffix.
	RetentionKeepTurns int
	// RetentionResultHeadBytes is how many bytes of a trimmed tool result stay
	// live (clamped to the 4096-byte retention threshold). Zero uses the
	// 800-byte default.
	RetentionResultHeadBytes int
	// Interactive marks a session whose multi-minute pauses justify the 1h
	// Anthropic prompt-cache breakpoint on the stable prefix (set for the REPL).
	// One-shot, delegate, and non-interactive runs leave it false to take the
	// cheaper 5-minute breakpoint.
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
	deferredToolGroups        []llm.ToolGroup
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
	reasoningReplayDomain     string
	disabledReasoningReplay   map[string]bool
	serverTools               []llm.ServerTool
	now                       func() time.Time
	sleep                     func(context.Context, time.Duration) error // mid-stream retry backoff; nil-free, set in New
	compactKeepTurns          int
	compactKeepTokens         int
	compactTriggerPercent     int
	compactTargetPercent      int
	disableAutoCompaction     bool
	compactSummaryMaxTokens   int
	compactTimeout            time.Duration
	compactToolResultMaxBytes int
	compactFallbackNotice     compactFallbackNoticeState
	compactionRuntimeVersion  uint64
	compactions               int
	archiveCompaction         CompactionArchiver
	hooks                     *hooks.Runner
	stagnationNudge           bool
	showDiffs                 bool
	// failGuard is the per-prompt repeated-identical-failure guard (design
	// §8.1). It is non-nil only while RunAdmittedPromptWithContext executes.
	failGuard *failureGuard
	// toolRunSem bounds concurrent tool executions within this agent (design
	// §8.1). Parallel dispatch starts one worker per call; the semaphore keeps
	// a degenerate or simply large batch from forking hundreds of processes at
	// once. It is per-agent so delegate children can never deadlock against
	// permits held by their parent's dispatch.
	toolRunSem        chan struct{}
	responsesStateful bool
	nativeCompaction  bool
	// disabledNativeCompaction is keyed by provider replay domain. It suppresses
	// a checkpoint rejected by its originating provider while retaining the full
	// semantic transcript for an immediate stateless retry.
	disabledNativeCompaction map[string]bool
	// continuationFailures counts consecutive unavailable continuation attempts,
	// whether found by a local probe or rejected by the provider. Three in a row
	// disables stateful mode for the rest of the run.
	continuationFailures int
	interactive          bool            // 1h Anthropic cache breakpoint; see Options.Interactive
	steer                chan SteerInput // buffered in-prompt steer input; nil when Options.Steer is false
	responseState        llm.ResponseState
	responseStateEpoch   uint64
	// measuredInput/measuredBoundary persist the last turn's actual billed
	// input (uncached + cache read/write) and the transcript length it
	// measured, so the next reported context estimate can anchor to actuals
	// instead of a count-API/byte estimate that may systematically miss opaque
	// replay state. Zeroed whenever compaction or retention rewrites the
	// transcript outside a run's own tracking.
	measuredInput             int
	measuredBoundary          int
	retentionPolicy           RetentionPolicy
	retentionFloorTokens      int
	retentionKeepTurnsSetting int
	retentionResultHeadBytes  int
	retentionEpochArmed       bool
	proxySessionID            string
	cacheAffinityID           string
}

type compactFallbackNoticeState struct {
	noShrink    bool
	smallShrink bool
}

// MaxTurns returns the per-prompt turn limit. A non-positive value means
// unlimited.
func (a *Agent) MaxTurns() int { return a.maxTurns }

// SetMaxTurns changes the turn limit for subsequent prompts. A non-positive
// value means unlimited.
func (a *Agent) SetMaxTurns(maxTurns int) { a.maxTurns = maxTurns }

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
		deferredToolGroups:        registry.DeferredToolGroups(),
		registry:                  modelRegistry,
		model:                     opts.Model,
		maxTurns:                  opts.MaxTurns,
		maxPromptTokens:           opts.MaxPromptTokens,
		maxOutputTokens:           opts.MaxOutputTokens,
		maxPromptCostUSD:          opts.MaxPromptCostUSD,
		contextWindow:             opts.ContextWindow,
		reasoning:                 opts.Reasoning,
		reasoningReplayDomain:     opts.ReasoningReplayDomain,
		serverTools:               cloneServerTools(opts.ServerTools),
		now:                       now,
		sleep:                     sleepContext,
		compactKeepTurns:          opts.CompactKeepTurns,
		compactKeepTokens:         opts.CompactKeepTokens,
		compactTriggerPercent:     opts.CompactTriggerPercent,
		compactTargetPercent:      opts.CompactTargetPercent,
		disableAutoCompaction:     opts.DisableAutoCompaction,
		compactSummaryMaxTokens:   opts.CompactSummaryMaxTokens,
		compactTimeout:            opts.CompactTimeout,
		compactToolResultMaxBytes: opts.CompactToolResultMaxBytes,
		hooks:                     opts.Hooks,
		stagnationNudge:           opts.StagnationNudge,
		showDiffs:                 opts.ShowDiffs,
		responsesStateful:         opts.ResponsesStateful,
		nativeCompaction:          opts.NativeCompaction,
		retentionPolicy:           normalizeRetentionPolicy(opts.RetentionPolicy),
		retentionFloorTokens:      opts.RetentionFloorTokens,
		retentionKeepTurnsSetting: opts.RetentionKeepTurns,
		retentionResultHeadBytes:  opts.RetentionResultHeadBytes,
		interactive:               opts.Interactive,
		retentionEpochArmed:       true,
		toolRunSem:                make(chan struct{}, maxConcurrentToolRuns),
		proxySessionID:            newProxySessionID(),
		cacheAffinityID:           newCacheAffinityID(),
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
	if a.observedContextWindow > 0 && a.observedContextWindow < configured {
		return a.observedContextWindow
	}
	return configured
}

// SetSystem sets the system prompt sent on every request.
func (a *Agent) SetSystem(system string) {
	a.system = system
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// ToolNames returns the names of tools in the agent's active registry in
// registration order.
func (a *Agent) ToolNames() []string { return a.tools.Names() }

// ToolActivity returns the diagnostics-only classification used by the loop
// guard for the agent's current registry.
func (a *Agent) ToolActivity(call llm.ToolCall) tools.Activity {
	return a.tools.CallActivity(call)
}

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
		a.deferredToolGroups = registry.DeferredToolGroups()
		a.compactionRuntimeVersion++
		a.retentionEpochArmed = true
		a.resetResponseState()
	}
}

// SetProvider replaces the provider used for subsequent model calls.
func (a *Agent) SetProvider(provider llm.Provider) {
	if provider != nil {
		a.provider = provider
		a.compactionRuntimeVersion++
		a.observedContextWindow = 0
		a.retentionEpochArmed = true
		a.resetResponseState()
	}
}

// SetModel replaces the model id stamped onto subsequent requests. contextWindow
// is the same override as Options.ContextWindow: zero means use the registry.
func (a *Agent) SetModel(model string, contextWindow int) {
	a.model = model
	a.contextWindow = contextWindow
	a.compactionRuntimeVersion++
	a.observedContextWindow = 0
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// SetReasoning replaces the reasoning controls sent on subsequent requests.
func (a *Agent) SetReasoning(reasoning llm.ReasoningConfig) {
	a.reasoning = reasoning
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// SetReasoningReplayDomain changes which persisted provider-owned reasoning
// blocks may be sent on subsequent requests.
func (a *Agent) SetReasoningReplayDomain(domain string) {
	a.reasoningReplayDomain = domain
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// SetServerTools replaces provider-hosted tool declarations for subsequent
// requests.
func (a *Agent) SetServerTools(serverTools []llm.ServerTool) {
	a.serverTools = cloneServerTools(serverTools)
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// SetHooks replaces the lifecycle hook runner used by subsequent turns.
func (a *Agent) SetHooks(runner *hooks.Runner) {
	a.hooks = runner
	a.compactionRuntimeVersion++
}

// Steer injects text as an in-prompt steering message. It is the simple text-only
// helper for callers that do not need images or request-only context.
func (a *Agent) Steer(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	return a.SteerContent(SteerInput{Text: text})
}

// SteerContent tries to inject prepared content as an in-prompt steering
// message: the loop drains it before the next model request (between tool
// rounds) and appends it as a RoleUser message. It returns false when steering
// was not enabled (Options.Steer false), the input is empty, or the steer buffer
// is full, so non-blocking callers can queue the input elsewhere instead of
// losing it. SteerContent is safe to call concurrently with a running prompt.
func (a *Agent) SteerContent(input SteerInput) bool {
	if a.steer == nil || steerInputEmpty(input) {
		return false
	}
	input.Images = cloneImageBlocks(input.Images)
	input.RequestContext = cleanContext(input.RequestContext)
	select {
	case a.steer <- input:
		return true
	default:
		return false
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
// has not yet consumed and combines it into one model-bound input. Non-blocking.
func (a *Agent) DrainSteerContent() SteerInput {
	return a.drainSteer()
}

// DrainSteerContents pops queued steer inputs without combining them. Protocol
// drivers use this form to preserve each input's correlation metadata when a
// prompt ends before the agent consumes it. Non-blocking.
func (a *Agent) DrainSteerContents() []SteerInput {
	return a.drainSteerInputs()
}

// SetTranscript replaces the running transcript (used when resuming a session).
func (a *Agent) SetTranscript(msgs []llm.Message) {
	a.transcript = msgs
	a.validatedPrefix = 0 // resumed/replaced content must be fully re-validated (r62)
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// SetResponsesStateful toggles Responses API continuation for subsequent
// requests. Disabling or changing the mode clears any previous remote anchor.
func (a *Agent) SetResponsesStateful(enabled bool) {
	if a.responsesStateful == enabled {
		return
	}
	a.responsesStateful = enabled
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// SetNativeCompaction changes whether subsequent foreground compactions may use
// a provider-native canonical context window. Provider switches call this with
// the selected target capability; existing checkpoints remain durable but are
// ignored outside their replay domain.
func (a *Agent) SetNativeCompaction(enabled bool) {
	if a.nativeCompaction == enabled {
		return
	}
	a.nativeCompaction = enabled
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
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
	a.responseStateEpoch++
}

// probeResponseContinuation reports whether the provider can still continue
// the anchored previous response. Providers without transport-local
// continuation constraints do not implement the probe and always continue.
func (a *Agent) probeResponseContinuation() bool {
	probe, ok := a.provider.(llm.ResponseContinuationProbe)
	if !ok {
		return true
	}
	return probe.CanContinueResponse(a.responseState.PreviousResponseID)
}

// ensureResponseContinuation checks the provider can continue the anchored
// response before the request round-trips. When it cannot, the anchor is reset
// locally so the request carries full context instead of burning a provider
// round-trip discovering the failure. It reports whether state was reset.
func (a *Agent) ensureResponseContinuation(sink EventSink) bool {
	if a.responseState.PreviousResponseID == "" || a.probeResponseContinuation() {
		return false
	}
	a.resetResponseState()
	if sink != nil {
		sink.Notice("[responses state reset: continuation unavailable; sending full context]")
	}
	a.noteContinuationFailure(sink)
	return true
}

func (a *Agent) noteContinuationFailure(sink EventSink) {
	a.continuationFailures++
	if a.continuationFailures < 3 {
		return
	}
	a.continuationFailures = 0
	a.SetResponsesStateful(false)
	if sink != nil {
		sink.Notice("[responses state disabled: continuation repeatedly unavailable; running stateless]")
	}
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

// CompactionCount returns the number of successful compaction rewrites applied
// by this agent instance. Failed, blocked, no-op, and discarded idle attempts do
// not increment it.
func (a *Agent) CompactionCount() int { return a.compactions }

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
// the message shape used by RunPromptContentWithContext. EstimatedInputTokens
// is the current estimate anchored to the last measured turn when one exists.
func (a *Agent) ContextRequestWithContext(extraContext []string) llm.Request {
	messages := a.providerVisibleMessages(a.transcript)
	est := a.anchorContextEstimate(estimateRequest(llm.Request{
		System:         a.system,
		Messages:       messages,
		Tools:          a.toolSpecs,
		ServerTools:    a.serverTools,
		RequestContext: extraContext,
	}, a.window()), a.measuredInput, a.measuredBoundary)
	return llm.Request{
		Model:                a.model,
		Purpose:              llm.RequestPurposeTurn,
		System:               a.system,
		Messages:             append([]llm.Message(nil), messages...),
		Tools:                cloneToolSpecs(a.toolSpecs),
		DeferredToolGroups:   cloneToolGroups(a.deferredToolGroups),
		ToolSearchFallback:   tools.ToolCatalogName,
		ServerTools:          cloneServerTools(a.serverTools),
		Reasoning:            a.reasoning,
		RequestContext:       append([]string(nil), extraContext...),
		ProxySessionID:       a.proxySessionID,
		CacheAffinityID:      a.cacheAffinityID,
		CachePolicy:          a.cachePolicyForTranscript(messages, 0, false),
		EstimatedInputTokens: est.Total,
	}
}

// ProxySessionID returns the opaque local key used for proxy sticky routing and
// connection-affine transport isolation for this agent session.
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

// ResetProxySessionID rotates transport affinity and the CLI-owned continuation
// anchor without changing prompt-cache affinity.
func (a *Agent) ResetProxySessionID() {
	a.proxySessionID = newProxySessionID()
	a.resetResponseState()
}

// CacheAffinityID returns the stable local key used to route this conversation's
// requests to the same provider prompt-cache shard.
func (a *Agent) CacheAffinityID() string {
	return a.cacheAffinityID
}

// SetCacheAffinityID restores a persisted prompt-cache affinity key on resume.
func (a *Agent) SetCacheAffinityID(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		a.cacheAffinityID = newCacheAffinityID()
		return
	}
	a.cacheAffinityID = id
}

// ResetSessionIDs rotates both continuation state and prompt-cache affinity for
// a genuinely new logical session.
func (a *Agent) ResetSessionIDs() {
	a.proxySessionID = newProxySessionID()
	a.cacheAffinityID = newCacheAffinityID()
	a.retentionEpochArmed = true
	a.resetResponseState()
}

func newProxySessionID() string {
	return newOpaqueID("harness-session-")
}

func newCacheAffinityID() string {
	return newOpaqueID("harness-cache-")
}

func newOpaqueID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return prefix + hex.EncodeToString(b[:])
	}
	return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
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
		req.CachePolicy.StableMessagePrefix = 0
	}
	req.MaxTokens = 1 // smallest legal cap: only the prefill matters
	req.Purpose = llm.RequestPurposePrewarm
	req.StoreResponse = a.responsesStateful
	req.CachePolicy.StaticTTL = llm.CacheTTLDefault
	req.Reasoning = llm.ReasoningConfig{} // no thinking/effort — a pure prefix write
	req.RequestContext = nil
	return req, true
}

// PrewarmFunc captures the current provider and a PrewarmRequest snapshot and
// returns a closure that streams the warm-up request, discards its output, and
// returns provider-reported usage plus any explicitly safe response anchor.
// Call it on the goroutine that owns the agent (before the input loop); the
// returned closure shares no mutable agent state, so it is safe to run in a
// background goroutine. ok is false when there is nothing to warm.
type PrewarmResult struct {
	Usage              llm.Usage
	ResponseState      *llm.ResponseState
	ResponseStateEpoch uint64
	ProxySessionID     string
	TranscriptMessages int
}

func (a *Agent) PrewarmFunc() (func(context.Context) PrewarmResult, bool) {
	req, ok := a.PrewarmRequest()
	if !ok {
		return nil, false
	}
	provider := a.provider
	epoch := a.responseStateEpoch
	proxySessionID := a.proxySessionID
	transcript := cloneMessages(a.transcript)
	return func(ctx context.Context) PrewarmResult {
		result := PrewarmResult{
			ResponseStateEpoch: epoch,
			ProxySessionID:     proxySessionID,
			TranscriptMessages: len(transcript),
		}
		if err := validateRequestImageContent(req.Messages); err != nil {
			return result
		}
		for event, err := range provider.Stream(ctx, req) {
			if err != nil {
				// Best-effort: a failed warm-up just means the first real request
				// pays the cold-cache cost. Preserve usage already reported before
				// the failure because those tokens may still be billed.
				return result
			}
			if (event.Kind == llm.EventUsage || event.Kind == llm.EventDone) && event.Usage != nil {
				result.Usage = mergeUsage(result.Usage, *event.Usage)
			}
			if event.Kind == llm.EventDone &&
				event.ResponseID != "" &&
				event.ResponseIDAnchor != nil &&
				*event.ResponseIDAnchor >= 0 &&
				*event.ResponseIDAnchor <= len(transcript) {
				digest, digestErr := llm.FingerprintMessages(transcript[:*event.ResponseIDAnchor])
				if digestErr == nil {
					result.ResponseState = &llm.ResponseState{
						PreviousResponseID: event.ResponseID,
						AnchorMessages:     *event.ResponseIDAnchor,
						AnchorDigest:       digest,
					}
				}
			}
		}
		return result
	}, true
}

// ApplyPrewarmResult installs a background prewarm anchor only when the
// initiating agent state is still current. It must be called on the goroutine
// that owns the Agent.
func (a *Agent) ApplyPrewarmResult(result PrewarmResult) bool {
	state := result.ResponseState
	if !a.responsesStateful ||
		state == nil ||
		state.PreviousResponseID == "" ||
		result.ResponseStateEpoch != a.responseStateEpoch ||
		result.ProxySessionID != a.proxySessionID ||
		result.TranscriptMessages != len(a.transcript) ||
		a.responseState.PreviousResponseID != "" ||
		state.AnchorMessages < 0 ||
		state.AnchorMessages > len(a.transcript) ||
		!llm.MatchesMessageFingerprint(
			a.transcript[:state.AnchorMessages],
			state.AnchorDigest,
		) {
		return false
	}
	a.responseState = *state
	return true
}

// ResponseStateEpoch identifies the current continuation-reset generation.
func (a *Agent) ResponseStateEpoch() uint64 {
	return a.responseStateEpoch
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
	if lastInput <= 0 {
		return estimateTokens(a.providerVisibleMessages(a.transcript))
	}
	if boundary < 0 {
		boundary = 0
	}
	if boundary > len(a.transcript) {
		boundary = len(a.transcript)
	}
	return lastInput + estimateTokens(a.providerVisibleMessages(a.transcript[boundary:]))
}

// anchorContextEstimate anchors the reported context total to the last
// measured input (actual billed usage: uncached + cache read/write) plus a
// local estimate of the messages appended since that measurement — the same
// r44 pattern as the compaction trigger. Provider count-token endpoints can
// systematically under-report opaque replay state (some Anthropic-compatible
// endpoints bill thinking signatures the count route ignores), so once a turn
// has been measured, actuals are the honest baseline. With no measurement
// (fresh run, or after a compaction/retention reset) the estimate passes
// through unchanged. Per-section (system/tools/messages) estimates and the
// payload breakdown stay estimator-based; only the reported total anchors.
func (a *Agent) anchorContextEstimate(est ContextEstimate, lastInput, boundary int) ContextEstimate {
	if lastInput <= 0 {
		return est
	}
	est.Total = a.triggerTokens(lastInput, boundary)
	est.Source = ContextEstimateSourceResponseUsageDelta
	return est
}

func (a *Agent) estimateContext(extraContext []string) ContextEstimate {
	return a.estimateContextForTranscript(extraContext, a.transcript)
}

func (a *Agent) estimateContextForTranscript(extraContext []string, transcript []llm.Message) ContextEstimate {
	transcript = a.providerVisibleMessages(transcript)
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
	reasoning  []llm.ContentBlock // provider-owned replay state, in arrival order
	content    []llm.ContentBlock // exact provider block order when hosted search interleaves content
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

// turnAttemptCoordinator keeps physical attempt IDs and discarded billed usage
// continuous across request-shape rebuilds within one logical turn. Each rebuilt
// request receives a fresh local transport-retry budget.
type turnAttemptCoordinator struct {
	agent     *Agent
	sink      EventSink
	turn      int
	next      int
	wasted    llm.Usage
	abandoned map[int]bool
}

func newTurnAttemptCoordinator(a *Agent, sink EventSink, turn int) *turnAttemptCoordinator {
	return &turnAttemptCoordinator{
		agent:     a,
		sink:      sink,
		turn:      turn,
		next:      1,
		abandoned: make(map[int]bool),
	}
}

func (c *turnAttemptCoordinator) request(ctx context.Context, req llm.Request, estimate ContextEstimate) (turnResult, error) {
	res, wasted, err := c.agent.streamWithRetry(ctx, req, c.sink, c.turn, c.next, estimate)
	c.wasted = add(c.wasted, wasted)
	if res.attempts >= c.next {
		c.next = res.attempts + 1
	}
	return res, err
}

func (c *turnAttemptCoordinator) rerun(ctx context.Context, previous turnResult, req llm.Request, estimate ContextEstimate) (turnResult, error) {
	if err := ctx.Err(); err != nil {
		return previous, err
	}
	c.abandon(previous)
	return c.request(ctx, req, estimate)
}

func (c *turnAttemptCoordinator) abandon(res turnResult) {
	if res.attempts <= 0 || c.abandoned[res.attempts] {
		return
	}
	c.abandoned[res.attempts] = true
	c.wasted = add(c.wasted, res.usage)
	if abandon, ok := c.sink.(TurnAttemptAbandonSink); ok {
		abandon.TurnAttemptAbandoned(c.turn, res.attempts)
	}
}

func (a *Agent) modelRequest(requestContext []string) modelRequest {
	return a.modelRequestForTranscript(requestContext, a.transcript)
}

func (a *Agent) modelRequestForTranscript(requestContext []string, transcript []llm.Message) modelRequest {
	payloadMessages, usedPrevious := a.payloadMessagesIn(transcript)
	visibleTranscript := a.providerVisibleMessages(transcript)
	payloadMessages = a.providerVisibleMessages(payloadMessages)
	visiblePayloadStart := len(visibleTranscript) - len(payloadMessages)
	if visiblePayloadStart < 0 {
		visiblePayloadStart = 0
	}
	estimate := a.estimatePayloadContextForTranscript(requestContext, visibleTranscript, payloadMessages)
	req := llm.Request{
		Model:                a.model,
		Purpose:              llm.RequestPurposeTurn,
		System:               a.system,
		Messages:             payloadMessages,
		Tools:                cloneToolSpecs(a.toolSpecs),
		DeferredToolGroups:   cloneToolGroups(a.deferredToolGroups),
		ToolSearchFallback:   tools.ToolCatalogName,
		ServerTools:          cloneServerTools(a.serverTools),
		Reasoning:            a.reasoning,
		MaxTokens:            a.maxOutputTokens,
		StoreResponse:        a.responsesStateful,
		RequestContext:       append([]string(nil), requestContext...),
		ProxySessionID:       a.proxySessionID,
		CacheAffinityID:      a.cacheAffinityID,
		CachePolicy:          a.cachePolicyForTranscript(visibleTranscript, visiblePayloadStart, false),
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

func providerOwnedReasoningBlock(kind llm.BlockKind) bool {
	switch kind {
	case llm.BlockThinking,
		llm.BlockRedactedThinking,
		llm.BlockReasoning,
		llm.BlockInteractionThought,
		llm.BlockInteractionStep:
		return true
	default:
		return false
	}
}

func providerOwnedReplayBlock(kind llm.BlockKind) bool {
	return providerOwnedReasoningBlock(kind) ||
		kind == llm.BlockResponsesToolSearch ||
		kind == llm.BlockAnthropicToolSearch
}

// providerVisibleMessages returns request-only copies when messages contain
// opaque reasoning that is incompatible with the selected target. Every
// provider boundary uses this helper; the durable transcript is never mutated.
func (a *Agent) providerVisibleMessages(messages []llm.Message) []llm.Message {
	domain := a.reasoningReplayDomain
	disabledReasoning := a.disabledReasoningReplay[domain]
	nativeIndex := -1
	if a.nativeCompaction && !a.disabledNativeCompaction[domain] {
		for i := len(messages) - 1; i >= 0 && nativeIndex < 0; i-- {
			for _, block := range messages[i].Content {
				if block.Kind == llm.BlockProviderCompaction && block.ReasoningReplayDomain == domain {
					nativeIndex = i
					break
				}
			}
		}
	}
	start := 0
	if nativeIndex >= 0 {
		start = nativeIndex
	}
	out := make([]llm.Message, 0, len(messages)-start)
	for i := start; i < len(messages); i++ {
		message := messages[i]
		content := make([]llm.ContentBlock, 0, len(message.Content))
		for _, block := range message.Content {
			switch {
			case block.Kind == llm.BlockProviderCompaction:
				if i == nativeIndex && block.ReasoningReplayDomain == domain {
					content = append(content, block)
				}
			case !providerOwnedReplayBlock(block.Kind):
				content = append(content, block)
			case block.ReasoningReplayDomain != domain:
				// Provider-owned replay state never crosses compatibility domains.
			case !disabledReasoning || !providerOwnedReasoningBlock(block.Kind):
				// invalid_encrypted_content disables only signed/encrypted reasoning;
				// same-domain hosted-search state remains required for exact replay.
				content = append(content, block)
			}
		}
		if len(content) == 0 {
			continue
		}
		message.Content = content
		out = append(out, message)
	}
	return out
}

func hasProviderCompaction(messages []llm.Message) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockProviderCompaction {
				return true
			}
		}
	}
	return false
}

func hasProviderOwnedReasoning(messages []llm.Message) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if providerOwnedReasoningBlock(block.Kind) {
				return true
			}
		}
	}
	return false
}

func invalidEncryptedContent(err error) bool {
	var apiErr *llm.APIError
	return errors.As(err, &apiErr) &&
		strings.EqualFold(strings.TrimSpace(apiErr.Code), "invalid_encrypted_content")
}

func (a *Agent) disableCurrentReasoningReplay() {
	if a.disabledReasoningReplay == nil {
		a.disabledReasoningReplay = make(map[string]bool)
	}
	domain := a.reasoningReplayDomain
	if a.disabledReasoningReplay[domain] {
		return
	}
	a.disabledReasoningReplay[domain] = true
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

func (a *Agent) disableCurrentNativeCompaction() {
	if a.disabledNativeCompaction == nil {
		a.disabledNativeCompaction = make(map[string]bool)
	}
	domain := a.reasoningReplayDomain
	if domain == "" || a.disabledNativeCompaction[domain] {
		return
	}
	a.disabledNativeCompaction[domain] = true
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

// discardCurrentNativeCompaction removes rejected opaque checkpoints for the
// active compatibility domain. The provider-neutral semantic transcript was
// retained alongside them, so this loses no conversational state and prevents
// a resumed session from retrying the same invalid encrypted content.
func (a *Agent) discardCurrentNativeCompaction() bool {
	domain := a.reasoningReplayDomain
	kept := make([]llm.Message, 0, len(a.transcript))
	removed := false
	for _, message := range a.transcript {
		matches := false
		for _, block := range message.Content {
			if block.Kind == llm.BlockProviderCompaction && block.ReasoningReplayDomain == domain {
				matches = true
				break
			}
		}
		if matches {
			removed = true
			continue
		}
		kept = append(kept, message)
	}
	if !removed {
		return false
	}
	a.transcript = kept
	a.validatedPrefix = 0
	a.clearMeasuredContext()
	a.retentionEpochArmed = true
	return true
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
	count, providerObservation, ok := a.countInputTokens(ctx, mr.request)
	if !ok || count.InputTokens <= 0 {
		return mr
	}
	scope := llm.NormalizeInputTokenCountScope(count.Scope)
	if providerObservation {
		mr.estimate.ProviderInputTokens = count.InputTokens
		mr.estimate.ProviderInputSource = count.Source
		mr.estimate.ProviderInputScope = scope
	}

	// Without continuation state, the request payload and effective context are
	// the same input regardless of whether an older provider declared scope.
	if !mr.usedPrevious {
		applyCountToContextEstimate(&mr.estimate.Total, &mr.estimate.Messages, &mr.estimate.Source,
			mr.estimate.System, mr.estimate.Tools, count.InputTokens, countEstimateSource(providerObservation))
		applyCountToContextEstimate(&mr.estimate.PayloadTotal, &mr.estimate.PayloadMessages, &mr.estimate.PayloadSource,
			mr.estimate.PayloadSystem, mr.estimate.PayloadTools, count.InputTokens, countEstimateSource(providerObservation))
		mr.request.EstimatedInputTokens = count.InputTokens
		return mr
	}

	switch scope {
	case llm.InputTokenCountScopeEffectiveContext:
		applyCountToContextEstimate(&mr.estimate.Total, &mr.estimate.Messages, &mr.estimate.Source,
			mr.estimate.System, mr.estimate.Tools, count.InputTokens, countEstimateSource(providerObservation))
		mr.request.EstimatedInputTokens = count.InputTokens
	case llm.InputTokenCountScopeRequestPayload:
		applyCountToContextEstimate(&mr.estimate.PayloadTotal, &mr.estimate.PayloadMessages, &mr.estimate.PayloadSource,
			mr.estimate.PayloadSystem, mr.estimate.PayloadTools, count.InputTokens, countEstimateSource(providerObservation))
	}
	return mr
}

func applyCountToContextEstimate(total, messages *int, source *string, system, tools, count int, countSource string) {
	*total = count
	if static := system + tools; count >= static {
		*messages = count - static
	}
	*source = countSource
}

func countEstimateSource(providerObservation bool) string {
	if providerObservation {
		return ContextEstimateSourceProviderCount
	}
	return ContextEstimateSourceBytes
}

func (a *Agent) countInputTokens(ctx context.Context, req llm.Request) (llm.InputTokenCount, bool, bool) {
	if err := validateRequestImageContent(req.Messages); err != nil {
		return llm.InputTokenCount{}, false, false
	}
	if counter, ok := a.provider.(llm.InputTokenCounter); ok {
		count, err := counter.CountInputTokens(ctx, req)
		if err == nil && count.InputTokens > 0 {
			count.Scope = llm.NormalizeInputTokenCountScope(count.Scope)
			return count, true, true
		}
	}
	if tokencount.ShouldEstimateOpenAIChat(a.provider.Name()) {
		if count := tokencount.EstimateOpenAIChat(req); count > 0 {
			scope := llm.InputTokenCountScopeEffectiveContext
			if req.PreviousResponseID != "" {
				scope = llm.InputTokenCountScopeRequestPayload
			}
			return llm.InputTokenCount{InputTokens: count, Source: ContextEstimateSourceBytes, Scope: scope}, false, true
		}
	}
	return llm.InputTokenCount{}, false, false
}

func (a *Agent) payloadMessagesIn(transcript []llm.Message) ([]llm.Message, bool) {
	if !a.validResponseStateFor(transcript) {
		return transcript, false
	}
	return transcript[a.responseState.AnchorMessages:], true
}

func (a *Agent) retentionRequestMode(usedPrevious bool) RetentionRequestMode {
	if usedPrevious {
		return RetentionRequestModeStatefulSuffix
	}
	if a.responsesStateful {
		return RetentionRequestModeFull
	}
	return RetentionRequestModeStateless
}

func (a *Agent) validResponseStateFor(transcript []llm.Message) bool {
	valid := a.responsesStateful &&
		a.responseState.PreviousResponseID != "" &&
		a.responseState.AnchorMessages >= 0 &&
		a.responseState.AnchorMessages <= len(transcript) &&
		llm.MatchesMessageFingerprint(
			transcript[:a.responseState.AnchorMessages],
			a.responseState.AnchorDigest,
		)
	if !valid && a.responseState.PreviousResponseID != "" {
		a.resetResponseState()
	}
	return valid
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
	digest, err := llm.FingerprintMessages(a.transcript)
	if err != nil {
		a.resetResponseState()
		return
	}
	a.responseState = llm.ResponseState{
		PreviousResponseID: res.responseID,
		AnchorMessages:     len(a.transcript),
		AnchorDigest:       digest,
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

func validateRequestImageContent(messages []llm.Message) error {
	if _, err := inputimage.ValidateMessages(messages); err != nil {
		return err
	}
	if err := llm.ValidateMessageContent(messages); err != nil {
		return err
	}
	return nil
}

func (a *Agent) validateRetainedTranscript(phase string) error {
	if err := validateRequestImageContent(a.transcript); err != nil {
		return fmt.Errorf("agent transcript invalid %s: %w", phase, err)
	}
	return a.validateTranscript(phase)
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

// PromptAdmission identifies a user message already appended to an Agent's
// transcript but not yet submitted to the provider. Its fields are private so
// only Agent can construct a valid admission.
type PromptAdmission struct {
	agent       *Agent
	promptIndex int
}

// AdmitPromptContent appends one prompt-origin user message synchronously and
// returns the admission needed to execute it. Splitting admission from execution
// lets callers make transcript insertion part of a larger atomic state transition.
func (a *Agent) AdmitPromptContent(userText string, images []llm.ContentBlock) PromptAdmission {
	promptIndex := len(a.transcript)
	promptMessage := a.userMessage(userText, images)
	promptMessage.Origin = llm.MessageOriginPrompt
	a.transcript = append(a.transcript, promptMessage)
	return PromptAdmission{agent: a, promptIndex: promptIndex}
}

// RunPromptContentWithContext is RunPromptContent plus request-only hook context.
// extraContext is visible to model requests for this prompt but is not persisted
// into the transcript.
func (a *Agent) RunPromptContentWithContext(ctx context.Context, userText string, images []llm.ContentBlock, extraContext []string, promptID int, sink EventSink) error {
	admission := a.AdmitPromptContent(userText, images)
	return a.RunAdmittedPromptWithContext(ctx, admission, extraContext, promptID, sink)
}

// RunAdmittedPromptWithContext executes a prompt previously inserted by
// AdmitPromptContent. Callers must execute admissions in creation order and at
// most once; Agent prompt execution is serialized by its owner.
func (a *Agent) RunAdmittedPromptWithContext(ctx context.Context, admission PromptAdmission, extraContext []string, promptID int, sink EventSink) error {
	if admission.agent != a || admission.promptIndex < 0 || admission.promptIndex >= len(a.transcript) || a.transcript[admission.promptIndex].Origin != llm.MessageOriginPrompt {
		return fmt.Errorf("invalid or stale prompt admission")
	}
	return a.runPromptLoopWithContext(ctx, admission.promptIndex, true, extraContext, promptID, sink)
}

// ContinuePromptWithContext resumes model work from the current valid transcript
// boundary without appending a message. It is intended for a host that assigns a
// fresh prompt/accounting ID after a terminal provider API failure.
func (a *Agent) ContinuePromptWithContext(ctx context.Context, extraContext []string, promptID int, sink EventSink) error {
	if len(a.transcript) == 0 {
		return fmt.Errorf("cannot continue an empty transcript")
	}
	return a.runPromptLoopWithContext(ctx, len(a.transcript), false, extraContext, promptID, sink)
}

func (a *Agent) runPromptLoopWithContext(ctx context.Context, promptIndex int, initialPromptPending bool, extraContext []string, promptID int, sink EventSink) (retErr error) {
	a.compactFallbackNotice = compactFallbackNoticeState{}

	var total llm.Usage
	var maintenanceTotal llm.Usage
	// Seed the measurement anchor from the previous prompt's final measured
	// turn so the reported context and the r44 trigger stay anchored to
	// actuals across prompts; compaction/retention resets below clear it.
	lastInput := a.measuredInput // input tokens the final measured turn reported (drives the trigger)
	var lastContext ContextEstimate
	turns := 0
	compactionsAtStart := a.compactions
	unlimited := a.maxTurns <= 0
	stopHookActive := false
	var guard turnGuard
	a.failGuard = newFailureGuard()
	defer func() { a.failGuard = nil }()
	var wastedTotal llm.Usage            // tokens spent on retried-and-discarded turn attempts (r51+r52)
	appendBoundary := a.measuredBoundary // transcript length measured by lastInput (drives the r44 trigger)
	var steerContext []string
	forcePromptWorkSynthesis := false
	outputContinuations := 0
	var terminationReason TerminationReason
	var closureTrigger ClosureTrigger
	var closureTurn int
	var turnBudgetExhausted bool
	var workflowStatus WorkflowStatus
	var evaluatorResults []hooks.EvaluatorResult
	var evaluatorUnrepresentedBlock bool
	trackEvaluatorResults := func(result hooks.Result) {
		evaluatorResults = append(evaluatorResults, result.EvaluatorResults...)
		if !result.Block {
			return
		}
		for _, evaluation := range result.EvaluatorResults {
			if !evaluation.Accepted {
				return
			}
		}
		evaluatorUnrepresentedBlock = true
	}
	currentWorkflowStatus := func() WorkflowStatus {
		return sampleWorkflowStatusWithEvaluations(sink, evaluatorResults, evaluatorUnrepresentedBlock)
	}
	startClosure := func(trigger ClosureTrigger, turn int) {
		if closureTrigger != "" {
			return
		}
		closureTrigger = trigger
		closureTurn = turn
		workflowStatus = currentWorkflowStatus()
		reportClosure(sink, ClosureEvent{Trigger: trigger, Turn: turn, WorkflowStatus: workflowStatus})
	}
	completeTurn := func() {
		turns++
		if !unlimited && turns >= a.maxTurns {
			turnBudgetExhausted = true
		}
	}
	promptUsage := func() PromptUsage {
		return PromptUsage{
			Turns:               turns,
			Usage:               total,
			Maintenance:         maintenanceTotal,
			Wasted:              wastedTotal,
			Compactions:         a.compactions - compactionsAtStart,
			Context:             lastContext,
			TerminationReason:   terminationReason,
			ClosureTrigger:      closureTrigger,
			ClosureTurn:         closureTurn,
			TurnBudgetExhausted: turnBudgetExhausted,
			WorkflowStatus:      workflowStatus,
		}
	}
	checkpoint := func(kind PromptCheckpointKind) {
		reportPromptCheckpoint(sink, PromptCheckpoint{Kind: kind, Turn: turns, Usage: promptUsage()})
	}
	defer func() {
		// Persist the measurement anchor for the next prompt and for /context;
		// it stays zero after a compaction/retention reset until the next
		// measured turn.
		a.measuredInput, a.measuredBoundary = lastInput, appendBoundary
		reason := terminationReason
		if retErr != nil {
			reason = TerminationError
			if errors.Is(retErr, context.Canceled) || errors.Is(retErr, context.DeadlineExceeded) {
				reason = TerminationCancelled
			}
		} else if reason == "" {
			reason = TerminationModelCompleted
		}
		terminationReason = reason
		workflowStatus = currentWorkflowStatus()
		sink.PromptComplete(promptUsage())
	}()
	// Stop notifies session coordinators on every prompt exit. The normal
	// model-stop path below can request another turn; every other exit still needs
	// one non-blockable notification before the caller regains control.
	stopHookRan := false
	defer func() {
		if a.hooks == nil || stopHookRan || !a.hooks.HasEvent(hooks.Stop) {
			return
		}
		// The prompt's context (or a provider's derived request context) may be
		// canceled on this path. Always detach and bound the non-blockable terminal
		// notification so cancellation cannot suppress it or make it unbounded.
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		hookResult := a.runStopHook(hookCtx, sink, hooks.Payload{
			"prompt_id":        promptID,
			"turn_id":          turns,
			"stop_hook_active": false,
			"can_block":        false,
		})
		trackEvaluatorResults(hookResult)
	}()

	for unlimited || turns < a.maxTurns || forcePromptWorkSynthesis {
		if !unlimited && !guard.turnBudgetClosureSteered && shouldEnterTurnBudgetClosure(a.maxTurns, turns) {
			guard.turnBudgetClosureSteered = true
			startClosure(ClosureTriggerTurnBudget, turns+1)
			if turns == 0 {
				// A budget that enters closure immediately has no prior closed tool
				// turn where a RoleUser steer can be inserted, so carry closure as
				// request-only context.
				steerContext = append(steerContext, wrapUpSteer)
			} else {
				message := a.textMessage(llm.RoleUser, wrapUpSteer)
				message.Origin = llm.MessageOriginInternal
				a.transcript = append(a.transcript, message)
				if err := a.validateTranscript("after turn-budget closure steer"); err != nil {
					return err
				}
			}
		}

		// Live-transcript retention (design §12, r9+r20): shrink stale large
		// tool outputs and aged images before building the request, so they are
		// not re-sent verbatim every turn. Pure local edit, invariant-preserving.
		localRetention := a.estimateContext(nil)
		retentionDecision := localRetention
		if anchored := a.triggerTokens(lastInput, appendBoundary); anchored > retentionDecision.Total {
			retentionDecision.Total = anchored
			if lastInput > 0 {
				retentionDecision.Source = ContextEstimateSourceResponseUsageDelta
			}
		}
		continuationPresent := a.validResponseStateFor(a.transcript)
		previousRequestMode := a.retentionRequestMode(continuationPresent)
		retention := retentionPass{}
		// A native compactable target retains the semantic transcript so provider
		// switches can fall back to it. If native compaction fails, its textual
		// fallback still applies the existing bounded degradation ladder.
		if !a.nativeCompactionEligible("auto", false, "") {
			retention = a.applyRetentionPolicyWithDecision(sink, retentionDecision)
		}
		retention.event.ContinuationStatePresent = continuationPresent
		retention.event.PreviousRequestMode = previousRequestMode
		if retention.changed {
			retention.event.ResponseStateReset = a.responseState.PreviousResponseID != ""
			retention.event.ContinuationStateReset = continuationPresent
			retention.event.MeasurementAnchorReset = lastInput > 0
			a.resetResponseState()
			// The last provider measurement describes the pre-retention
			// transcript. Do not combine it with a delta from the rewritten
			// transcript when evaluating compaction; restart from a full local
			// estimate just as a successful compaction does.
			lastInput = 0
			appendBoundary = 0
		}
		if err := a.validateRetainedTranscript("before model request"); err != nil {
			if initialPromptPending && promptIndex < len(a.transcript) {
				a.transcript = a.transcript[:promptIndex]
				if a.validatedPrefix > len(a.transcript) {
					a.validatedPrefix = len(a.transcript)
				}
			}
			return err
		}
		initialPromptPending = false
		baseRequestContext := appendPromptContext(extraContext, steerContext)
		requestContext := a.requestContext(baseRequestContext, sink)
		modelReq := a.modelRequest(requestContext)
		lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)
		// Proactive trigger (spec §4): a turn whose tool results balloon the
		// context compacts before the next request, not after the turn. The
		// trigger leans on the last real input count plus an estimate of only the
		// messages appended since it was measured (r44), not a whole-request byte
		// estimate.
		if a.autoCompactionEnabled() && a.overThreshold(a.triggerTokens(lastInput, appendBoundary)) {
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
				requestContext = a.requestContext(baseRequestContext, sink)
				modelReq = a.modelRequest(requestContext)
				lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)
			}
		}
		if err := a.validateRetainedTranscript("before model request"); err != nil {
			return err
		}
		// Probe transport-local continuation before the request round-trips: a
		// dead anchor would otherwise be discovered by a failed request that
		// re-sent the truncated payload and then retried with full context.
		if a.ensureResponseContinuation(sink) {
			requestContext = a.requestContext(baseRequestContext, sink)
			modelReq = a.modelRequest(requestContext)
		}
		modelReq = a.countModelRequestInput(ctx, modelReq)
		lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)
		if a.autoCompactionEnabled() && a.overThreshold(modelReq.estimate.Total) {
			compUsage, changed, err := a.compactTriggered(ctx, sink, "input-count")
			if compUsage != (llm.Usage{}) {
				total = add(total, compUsage)
				maintenanceTotal = add(maintenanceTotal, compUsage)
				reportMaintenance(sink, "compaction", compUsage)
			}
			if err == nil && changed {
				lastInput = 0
				appendBoundary = 0
				requestContext = a.requestContext(baseRequestContext, sink)
				if err := a.validateRetainedTranscript("after input-count compaction"); err != nil {
					return err
				}
				modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
				lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)
			}
		}
		if err := a.validateRetainedTranscript("before model request"); err != nil {
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
		if retention.observed {
			retention.event.NextRequestStateful = modelReq.usedPrevious
			retention.event.NextRequestMode = a.retentionRequestMode(modelReq.usedPrevious)
			reportRetention(sink, retention.event)
		}
		checkpoint(PromptCheckpointRequestBoundary)
		attempts := newTurnAttemptCoordinator(a, sink, turns+1)
		res, err := attempts.request(ctx, modelReq.request, lastContext)
		if err != nil && !res.hasPartialOutput() {
			if learned, ok := contextOverflowWindow(err); ok {
				if learned > 0 && a.observeContextWindow(learned) {
					sink.Notice(fmt.Sprintf("[context window adjusted: provider reported %d tokens; retrying request]", learned))
				} else {
					sink.Notice(NoticeContextOverflowCompacting)
				}
				compUsage, changed, cerr := a.compactTriggered(ctx, sink, "context-overflow")
				if compUsage != (llm.Usage{}) {
					total = add(total, compUsage)
					maintenanceTotal = add(maintenanceTotal, compUsage)
					reportMaintenance(sink, "compaction", compUsage)
				}
				if cerr != nil {
					err = cerr
				} else {
					if changed {
						lastInput = 0
						appendBoundary = 0
					}
					if verr := a.validateRetainedTranscript("after context-overflow compaction"); verr != nil {
						err = verr
					} else {
						if changed {
							refreshed := a.requestContext(baseRequestContext, sink)
							requestContext = appendMissingRequestContext(requestContext, refreshed)
						}
						modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
						appendBoundary = len(a.transcript)
						lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)
						res, err = attempts.rerun(ctx, res, modelReq.request, lastContext)
					}
				}
			}
		}
		if err != nil && modelReq.request.StoreResponse && !res.hasPartialOutput() && storeResponseRejected(err) {
			a.SetResponsesStateful(false)
			sink.Notice(NoticeResponsesStateDisabledRejected)
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)

			res, err = attempts.rerun(ctx, res, modelReq.request, lastContext)

		}
		if err != nil && modelReq.usedPrevious && !res.hasPartialOutput() && previousResponseRejected(err) {
			a.resetResponseState()
			sink.Notice(NoticeResponsesStateResetUnavailable)
			a.noteContinuationFailure(sink)
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)

			res, err = attempts.rerun(ctx, res, modelReq.request, lastContext)

		}
		if err != nil &&
			!res.hasPartialOutput() &&
			hasProviderCompaction(modelReq.request.Messages) &&
			invalidEncryptedContent(err) {
			a.disableCurrentNativeCompaction()
			if a.discardCurrentNativeCompaction() {
				notifyTranscriptRewritten(sink)
			}
			lastInput = 0
			appendBoundary = 0
			sink.Notice(NoticeNativeCompactionReplayDisabled)
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)

			res, err = attempts.rerun(ctx, res, modelReq.request, lastContext)

		}
		if err != nil &&
			!res.hasPartialOutput() &&
			hasProviderOwnedReasoning(modelReq.request.Messages) &&
			invalidEncryptedContent(err) {
			a.disableCurrentReasoningReplay()
			sink.Notice(NoticeReasoningReplayDisabled)
			modelReq = a.countModelRequestInput(ctx, a.modelRequest(requestContext))
			lastContext = a.anchorContextEstimate(modelReq.estimate, lastInput, appendBoundary)

			res, err = attempts.rerun(ctx, res, modelReq.request, lastContext)

		}
		wasted := attempts.wasted
		wastedTotal = add(wastedTotal, wasted)
		total = add(total, add(res.usage, wasted))
		// Context-size signal, not billing: cached tokens occupy the window too.
		lastInput = res.usage.InputTokens + res.usage.CacheReadTokens +
			res.usage.CacheWriteTokens + res.usage.CacheWrite1hTokens

		if err != nil {
			emitModelErrorDiagnostic(sink, err, promptID, turns+1, res.attempts)
			a.resetResponseState()
			// Cancellation repair: keep streamed partial text as a text-only
			// assistant message; drop the message entirely if nothing streamed.
			// Un-executed tool calls are never appended.
			cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			if cancelled && res.text != "" {
				a.transcript = append(a.transcript, a.partialAssistantMessage(res))
				completeTurn()
			}
			if cancelled {
				sink.Notice(NoticeCancelled)
			}
			if verr := a.validateTranscript("after failed turn"); verr != nil {
				err = errors.Join(err, verr)
			}
			if turns > 0 && cancelled && res.text != "" {
				sink.TurnComplete(TurnUsage{Turn: turns, Attempts: res.attempts, Usage: add(res.usage, wasted), Wasted: wasted, Context: lastContext})
				checkpoint(PromptCheckpointClosedTurn)
			}
			return err
		}

		completeTurn()
		a.transcript = append(a.transcript, a.assistantMessage(res))
		a.updateResponseState(res)
		if modelReq.usedPrevious {
			// Only a turn that actually continued the anchor clears the
			// consecutive-failure count; a stateless recovery turn must not.
			a.continuationFailures = 0
		}
		if res.stopReason != llm.StopToolUse {
			if err := a.validateTranscript("after assistant turn"); err != nil {
				return err
			}
			sink.TurnComplete(TurnUsage{Turn: turns, Attempts: res.attempts, Usage: add(res.usage, wasted), Wasted: wasted, Context: lastContext})
			checkpoint(PromptCheckpointClosedTurn)
			// A model may try to finalize while background delegates are still
			// running. Join them, then issue another model request with their reports
			// injected as request context so the parent actually synthesizes them.
			if pendingPromptWork(sink) {
				usage, waitErr := waitForPromptWork(ctx, sink)
				total = add(total, usage)
				if waitErr != nil {
					return waitErr
				}
				message := a.textMessage(llm.RoleUser, "[background delegates completed; synthesize their reports from request context before finishing]")
				message.Origin = llm.MessageOriginInternal
				a.transcript = append(a.transcript, message)
				if err := a.validateTranscript("after background delegate join"); err != nil {
					return err
				}
				forcePromptWorkSynthesis = true
				continue
			}
			// A normal response may be cut off by the provider's output ceiling. Keep
			// the partial assistant message already appended above and request exactly
			// one continuation, provided the prompt's explicit turn/token/cost budgets
			// still allow another paid request. The loop's ordinary proactive trigger
			// compacts first when this extra request would approach the context window.
			if res.stopReason == llm.StopMaxTokens && outputContinuations == 0 &&
				(unlimited || turns < a.maxTurns) &&
				(a.maxPromptTokens <= 0 || totalTokens(total) < a.maxPromptTokens) &&
				(a.maxPromptCostUSD <= 0 || !total.CostKnown || total.CostUSD < a.maxPromptCostUSD) {
				outputContinuations++
				sink.Notice(NoticeContinuingMaxTokens)
				message := a.textMessage(llm.RoleUser, "[The previous response was truncated by the output-token limit. Continue from the exact point it stopped without repeating completed content.]")
				message.Origin = llm.MessageOriginInternal
				a.transcript = append(a.transcript, message)
				if err := a.validateTranscript("after max-token continuation"); err != nil {
					return err
				}
				continue
			}
			if notice := stopReasonNotice(res.stopReason); notice != "" {
				sink.Notice(notice)
			}
			if a.hooks != nil && !stopHookActive && a.hooks.HasEvent(hooks.Stop) {
				canBlock := (unlimited || turns < a.maxTurns) &&
					(a.maxPromptTokens <= 0 || totalTokens(total) < a.maxPromptTokens) &&
					(a.maxPromptCostUSD <= 0 || !total.CostKnown || total.CostUSD < a.maxPromptCostUSD)
				hookRes := a.runStopHook(ctx, sink, hooks.Payload{
					"prompt_id":              promptID,
					"turn_id":                turns,
					"stop_hook_active":       stopHookActive,
					"last_assistant_message": res.text,
					"can_block":              canBlock,
				})
				trackEvaluatorResults(hookRes)
				// A cancellation before Runner dispatched any handler gets one
				// detached terminal attempt from the defer. Once a handler started,
				// its diagnostic proves the event was dispatched and prevents a
				// duplicate side effect.
				stopHookRan = ctx.Err() == nil || len(hookRes.Diagnostics) > 0
				if len(hookRes.AdditionalContext) > 0 {
					extraContext = append(extraContext, hookRes.AdditionalContext...)
				}
				if hookRes.Block && canBlock {
					reason := hookRes.Reason()
					if reason == "" {
						reason = "Stop hook requested continuation"
					}
					if a.stagnationNudge && hasRejectedEvaluatorResult(hookRes.EvaluatorResults) {
						if nudgeSink, ok := sink.(StagnationNudgeSink); ok && nudgeSink.TryStagnationNudge(StagnationNudgeThreshold) {
							reason += "\n\n" + stagnationNudgeContext
						}
					}
					message := a.textMessage(llm.RoleUser, "[hook Stop requested continuation]\n"+reason)
					message.Origin = llm.MessageOriginInternal
					a.transcript = append(a.transcript, message)
					stopHookActive = true
					if err := a.validateTranscript("after stop hook continuation"); err != nil {
						return err
					}
					continue
				}
			}
			terminationReason = TerminationModelCompleted
			break
		}

		checkpoint(PromptCheckpointToolDispatch)
		executionCalls := executionToolCalls(res.toolCalls)
		liveInputTokens := a.toolDispatchLiveInputTokens(lastInput, appendBoundary, lastContext)
		dispatchCtx := withReadResultBatchBudget(ctx, a.readResultBatchByteBudget(liveInputTokens))
		results, parallelBatches, toolUsage := a.dispatchCalls(dispatchCtx, res.toolCalls, promptID, turns, sink)
		total = add(total, toolUsage)
		a.transcript = append(a.transcript, llm.Message{
			Role:                llm.RoleUser,
			Time:                a.now(),
			Content:             results,
			ParallelToolBatches: parallelBatches,
		})
		a.refreshToolSpecs()
		if err := a.validateTranscript("after tool results"); err != nil {
			return err
		}
		sink.TurnComplete(TurnUsage{Turn: turns, Attempts: res.attempts, Usage: add(add(res.usage, wasted), toolUsage), Wasted: wasted, Context: lastContext})
		checkpoint(PromptCheckpointClosedTurn)
		progress := guard.aggregateTurnProgress(a.tools, turns, executionCalls, results)

		// Give a newly launched background delegate one subsequent parent model
		// round for useful independent work. If work was already pending before
		// this request, join it now and synthesize its injected report next.
		if (pendingBeforeRequest || (!unlimited && turns >= a.maxTurns)) && pendingPromptWork(sink) {
			usage, waitErr := waitForPromptWork(ctx, sink)
			total = add(total, usage)
			if waitErr != nil {
				return waitErr
			}
			forcePromptWorkSynthesis = true
			reportTurnProgress(sink, progress)
			continue
		}

		// Runaway guardrails (design §8.1). The transcript now ends on a closed
		// tool_use/tool_result pair, so injecting a steering RoleUser message or
		// breaking here keeps the §4 invariant intact.
		guard.recordTurn(executionCalls, results, &progress)

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
			steerIndex := len(a.transcript)
			steerMessage := a.userMessage(steered.Text, steered.Images)
			steerMessage.Origin = llm.MessageOriginSteer
			a.transcript = append(a.transcript, steerMessage)
			steerContext = append(steerContext, steered.RequestContext...)
			guard.resetForUserSteer(&progress)
			if err := a.validateRetainedTranscript("after in-prompt steer"); err != nil {
				a.transcript = a.transcript[:steerIndex]
				if a.validatedPrefix > len(a.transcript) {
					a.validatedPrefix = len(a.transcript)
				}
				return err
			}
			if deliverySink, ok := sink.(SteerDeliveredSink); ok {
				deliverySink.SteerDelivered(steered)
			}
		}

		// Hard stop: an unrelenting error storm. Finalize with a tools-disabled
		// summary so the turn ends on an assistant message, not a dangling result.
		if guard.shouldBreakErrors() {
			reportTurnProgress(sink, progress)
			startClosure(ClosureTriggerErrorGuard, turns)
			terminationReason = TerminationErrorGuard
			sink.Notice(errorStormNotice(guard.errorRuns))
			fu, fw, fctx, completed := a.finalizeWithSummary(ctx, sink, appendPromptContext(extraContext, steerContext), turns+1)
			total = add(total, fu)
			wastedTotal = add(wastedTotal, fw)
			lastContext = fctx
			if completed {
				completeTurn()
				if err := a.validateTranscript("after error-guard summary"); err != nil {
					return err
				}
				checkpoint(PromptCheckpointClosedTurn)
			}
			break
		}

		// Hard stop: an exact repeat loop or a command-family loop that ignored
		// steering. Finalize the same way so the turn ends on an assistant message
		// (the success-loop analogue of the error-storm break).
		if guard.shouldBreakRepeat() || guard.shouldBreakCommandRepeat() {
			reportTurnProgress(sink, progress)
			startClosure(ClosureTriggerRepeatGuard, turns)
			terminationReason = TerminationRepeatGuard
			if guard.shouldBreakRepeat() {
				sink.Notice(repeatLoopNotice(guard.repeatRuns))
			} else {
				sink.Notice(commandRepeatLoopNotice(guard.commandRuns))
			}
			fu, fw, fctx, completed := a.finalizeWithSummary(ctx, sink, appendPromptContext(extraContext, steerContext), turns+1)
			total = add(total, fu)
			wastedTotal = add(wastedTotal, fw)
			lastContext = fctx
			if completed {
				completeTurn()
				if err := a.validateTranscript("after repeat-guard summary"); err != nil {
					return err
				}
				checkpoint(PromptCheckpointClosedTurn)
			}
			break
		}

		// Token budget: stop before the next (paid) request. No final summary —
		// the whole point is to stop spending.
		if a.maxPromptTokens > 0 && totalTokens(total) >= a.maxPromptTokens {
			reportTurnProgress(sink, progress)
			terminationReason = TerminationTokenLimit
			sink.Notice(promptTokenBudgetNotice(a.maxPromptTokens))
			break
		}

		// Cost budget: the dollar analogue of the token budget, same hard stop.
		// Only fires when provider usage includes known cost, so an unpriced model
		// silently has no cost ceiling.
		if a.maxPromptCostUSD > 0 {
			if total.CostKnown && total.CostUSD >= a.maxPromptCostUSD {
				reportTurnProgress(sink, progress)
				terminationReason = TerminationCostLimit
				sink.Notice(promptCostBudgetNotice(a.maxPromptCostUSD, total.CostUSD))
				break
			}
		}

		// One steering nudge per condition. Semantic stagnation is advisory only.
		if reason, msg := guard.nextSteer(); msg != "" {
			progress.SteerReason = reason
			message := a.textMessage(llm.RoleUser, msg)
			message.Origin = llm.MessageOriginInternal
			a.transcript = append(a.transcript, message)
			if err := a.validateTranscript("after loop-guard steer"); err != nil {
				return err
			}
		}
		reportTurnProgress(sink, progress)
		if unlimited || turns < a.maxTurns {
			if err := a.applyContextEpoch(sink); err != nil {
				sink.Notice("[work context checkpoint failed; continuing current context: " + err.Error() + "]")
			}
		}

		if !unlimited && turns >= a.maxTurns {
			turnBudgetExhausted = true
			startClosure(ClosureTriggerTurnBudget, turns)
			terminationReason = TerminationTurnLimit
			sink.Notice(maxTurnsNotice(a.maxTurns))
			fu, fw, fctx, completed := a.finalizeWithSummary(ctx, sink, appendPromptContext(extraContext, steerContext), turns+1)
			total = add(total, fu)
			wastedTotal = add(wastedTotal, fw)
			lastContext = fctx
			if completed {
				completeTurn()
				if err := a.validateTranscript("after turn-limit summary"); err != nil {
					return err
				}
				checkpoint(PromptCheckpointClosedTurn)
			}
			break
		}
	}
	if !unlimited && turns >= a.maxTurns {
		turnBudgetExhausted = true
		startClosure(ClosureTriggerTurnBudget, turns)
		if terminationReason == "" {
			terminationReason = TerminationTurnLimit
		}
	}

	// Budget/guard exits can bypass the normal synthesis continuation. They still
	// must not abandon join-required delegates or lose their spend.
	if pendingPromptWork(sink) {
		usage, waitErr := waitForPromptWork(ctx, sink)
		total = add(total, usage)
		if waitErr != nil {
			return waitErr
		}
	}
	total = add(total, drainPromptWorkUsage(sink))

	// Post-prompt compaction trigger (design §12, §8.1): fires after the final
	// turn, before returning to the REPL. The summary call's usage folds into the
	// prompt total and is also reported separately as maintenance. A compaction
	// error never fails the prompt — the warning was already reported and
	// the transcript was kept intact.
	lastContext = a.estimateContext(a.estimateRequestContext(appendPromptContext(extraContext, steerContext), sink))
	compUsage, changed, err := a.MaybeCompact(ctx, a.triggerTokens(lastInput, appendBoundary), sink)
	if compUsage != (llm.Usage{}) {
		total = add(total, compUsage)
		maintenanceTotal = add(maintenanceTotal, compUsage)
		reportMaintenance(sink, "compaction", compUsage)
	}
	if err == nil && changed {
		lastInput = 0
		appendBoundary = 0
		lastContext = a.estimateContext(a.estimateRequestContext(appendPromptContext(extraContext, steerContext), sink))
	}
	if err := a.validateTranscript("after prompt"); err != nil {
		return err
	}

	return nil
}

func (a *Agent) refreshToolSpecs() {
	next := a.tools.Specs()
	nextDeferred := a.tools.DeferredToolGroups()
	if equalToolSpecs(a.toolSpecs, next) && equalToolGroups(a.deferredToolGroups, nextDeferred) {
		return
	}
	a.toolSpecs = next
	a.deferredToolGroups = nextDeferred
	a.compactionRuntimeVersion++
	a.retentionEpochArmed = true
	a.resetResponseState()
}

func (a *Agent) applyContextEpoch(sink EventSink) error {
	provider, ok := sink.(ContextEpochSink)
	if !ok {
		return nil
	}
	next, applied, err := provider.TakeContextEpoch(cloneMessages(a.transcript))
	if err != nil || !applied {
		return err
	}
	if err := llm.ValidateTranscript(next); err != nil {
		return fmt.Errorf("invalid work context epoch: %w", err)
	}
	a.transcript = cloneMessages(next)
	a.validatedPrefix = 0
	a.clearMeasuredContext()
	a.retentionEpochArmed = true
	a.ResetProxySessionID()
	return nil
}

func reportPromptCheckpoint(sink EventSink, checkpoint PromptCheckpoint) {
	if checkpointSink, ok := sink.(PromptCheckpointSink); ok {
		checkpointSink.PromptCheckpoint(checkpoint)
	}
}

func reportClosure(sink EventSink, event ClosureEvent) {
	if closureSink, ok := sink.(ClosureEventSink); ok {
		closureSink.ClosureStarted(event)
	}
}

func reportTurnProgress(sink EventSink, progress TurnProgress) {
	if progressSink, ok := sink.(TurnProgressSink); ok {
		progressSink.TurnProgress(progress)
	}
}

func (a *Agent) runStopHook(ctx context.Context, sink EventSink, payload hooks.Payload) hooks.Result {
	result := a.hooks.Run(ctx, hooks.Stop, "", payload)
	reportHookDiagnostics(sink, result.Diagnostics)
	reportEvaluatorResults(sink, result.EvaluatorResults)
	for _, notice := range result.Notices {
		sink.Notice(notice)
	}
	return result
}

func hasRejectedEvaluatorResult(results []hooks.EvaluatorResult) bool {
	for _, result := range results {
		if !result.Accepted {
			return true
		}
	}
	return false
}

func reportHookDiagnostics(sink EventSink, diagnostics []hooks.Diagnostic) {
	diagnosticSink, ok := sink.(HookDiagnosticSink)
	if !ok {
		return
	}
	for _, diagnostic := range diagnostics {
		diagnosticSink.HookDiagnostic(diagnostic)
	}
}

func reportEvaluatorResults(sink EventSink, results []hooks.EvaluatorResult) {
	evaluatorSink, ok := sink.(EvaluatorResultSink)
	if !ok {
		return
	}
	for _, result := range results {
		evaluatorSink.EvaluatorResult(result)
	}
}

func normalizeWorkflowOutcome(outcome WorkflowOutcome) WorkflowOutcome {
	switch outcome {
	case WorkflowOutcomeComplete, WorkflowOutcomeWaiting, WorkflowOutcomeBlocked,
		WorkflowOutcomeEscalated, WorkflowOutcomeInProgress, WorkflowOutcomeUnknown:
		return outcome
	default:
		return WorkflowOutcomeUnknown
	}
}

func sampleWorkflowStatus(sink EventSink) WorkflowStatus {
	provider, ok := sink.(WorkflowStatusProvider)
	if !ok {
		return WorkflowStatus{}
	}
	status := provider.WorkflowStatus()
	if !status.Available {
		return WorkflowStatus{}
	}
	status.Outcome = normalizeWorkflowOutcome(status.Outcome)
	if status.RemainingRequirements != nil {
		if *status.RemainingRequirements < 0 {
			status.RemainingRequirements = nil
		} else {
			remaining := *status.RemainingRequirements
			status.RemainingRequirements = &remaining
		}
	}
	return status
}

func sampleWorkflowStatusWithEvaluations(sink EventSink, results []hooks.EvaluatorResult, unrepresentedBlock bool) WorkflowStatus {
	if status := sampleWorkflowStatus(sink); status.Available {
		return status
	}
	if len(results) == 0 || unrepresentedBlock {
		return WorkflowStatus{}
	}
	status := WorkflowStatus{Available: true, Outcome: WorkflowOutcomeComplete}
	for _, result := range results {
		if !result.Accepted {
			status.Outcome = WorkflowOutcomeInProgress
			break
		}
	}
	// A remaining-requirements count has no generic aggregation rule across
	// independent evaluator handlers. Project it only when one result makes the
	// meaning unambiguous; every individual value remains available in the
	// evaluator_result session events.
	if len(results) == 1 && results[0].RemainingRequirements != nil {
		remaining := *results[0].RemainingRequirements
		status.RemainingRequirements = &remaining
	}
	return status
}

func reportRetention(sink EventSink, event RetentionEvent) {
	if retentionSink, ok := sink.(RetentionEventSink); ok {
		retentionSink.RetentionApplied(event)
	}
}

func emitModelErrorDiagnostic(sink EventSink, err error, prompt, turn, attempt int) {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Diagnostic == nil || apiErr.Diagnostic.Compatibility == nil {
		return
	}
	diagnostic := ModelErrorDiagnostic{
		Prompt:     prompt,
		Turn:       turn,
		Attempt:    attempt,
		StatusCode: apiErr.StatusCode,
		Code:       apiErr.Code,
		Message:    apiErr.Message,
		Diagnostic: cloneAPIErrorDiagnostic(apiErr.Diagnostic),
	}
	sink.Notice(modelCompatibilityNotice(diagnostic.Diagnostic))
	if diagnosticSink, ok := sink.(ModelErrorDiagnosticSink); ok {
		diagnosticSink.ModelErrorDiagnostic(diagnostic)
	}
}

func modelCompatibilityNotice(diagnostic *llm.APIErrorDiagnostic) string {
	compatibility := diagnostic.Compatibility
	target := strings.TrimSpace(diagnostic.TargetID)
	if target == "" {
		target = "selected target"
	}
	message := fmt.Sprintf("[model compatibility: target %s rejected %s", target, compatibility.Category)
	if remediation := strings.TrimSpace(compatibility.Remediation); remediation != "" {
		message += "; " + remediation
	}
	if diagnostic.ProxyRequestID != 0 {
		message += fmt.Sprintf("; proxy request %d", diagnostic.ProxyRequestID)
	}
	if diagnostic.TraceID != "" {
		message += "; trace " + diagnostic.TraceID
	}
	return message + "]"
}

func cloneAPIErrorDiagnostic(in *llm.APIErrorDiagnostic) *llm.APIErrorDiagnostic {
	if in == nil {
		return nil
	}
	out := *in
	if in.Compatibility != nil {
		compatibility := *in.Compatibility
		out.Compatibility = &compatibility
	}
	if in.MultimodalShape != nil {
		shape := *in.MultimodalShape
		shape.MIMETypes = append([]string(nil), shape.MIMETypes...)
		shape.Details = append([]string(nil), shape.Details...)
		shape.Dimensions = append([]llm.ImageDimension(nil), shape.Dimensions...)
		out.MultimodalShape = &shape
	}
	return &out
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
// gathers fresh request-only context for this distinct model round and returns
// the request's usage (counted toward the turn total) and estimate.
func (a *Agent) finalizeWithSummary(ctx context.Context, sink EventSink, extraContext []string, turn int) (llm.Usage, llm.Usage, ContextEstimate, bool) {
	requestContext := a.requestContext(extraContext, sink)
	modelReq := a.modelRequest(requestContext)
	modelReq.request.Tools = nil // no tools: force a text-only wind-down
	modelReq.request.ServerTools = nil
	attempts := newTurnAttemptCoordinator(a, sink, turn)
	res, err := attempts.request(ctx, modelReq.request, modelReq.estimate)
	if err != nil && !res.hasPartialOutput() && hasProviderOwnedReasoning(modelReq.request.Messages) && invalidEncryptedContent(err) {
		a.disableCurrentReasoningReplay()
		sink.Notice(NoticeReasoningReplayDisabled)
		modelReq = a.modelRequest(requestContext)
		modelReq.request.Tools = nil
		modelReq.request.ServerTools = nil
		res, err = attempts.rerun(ctx, res, modelReq.request, modelReq.estimate)
	}
	wasted := attempts.wasted
	usage := add(res.usage, wasted)
	if err != nil {
		a.resetResponseState()
		return usage, wasted, modelReq.estimate, false
	}
	if strings.TrimSpace(res.text) != "" {
		msg := a.textMessage(llm.RoleAssistant, res.text)
		msg.Phase = llm.AssistantPhaseFinal
		a.transcript = append(a.transcript, msg)
		a.updateResponseState(res)
	}
	sink.TurnComplete(TurnUsage{Turn: turn, Attempts: res.attempts, Usage: usage, Wasted: wasted, Context: modelReq.estimate})
	return usage, wasted, modelReq.estimate, true
}

type callStage struct {
	start int
	end   int
}

// planToolStages validates a complete emitted batch before dispatch and builds
// stripped execution copies. Calls without _stage inherit the current stage;
// malformed provider input keeps its assembler diagnostic and current stage.
func planToolStages(calls []llm.ToolCall) ([]llm.ToolCall, []callStage, error) {
	executionCalls := append([]llm.ToolCall(nil), calls...)
	resolvedStages := make([]int, len(calls))
	currentStage := 1
	var planErr error
	for i, call := range calls {
		if call.InvalidInputError != "" || call.Name == kimiWebSearchToolName {
			resolvedStages[i] = currentStage
			continue
		}
		clean, metadata, err := tools.ExtractExecutionMetadata(call.Input)
		executionCalls[i].Input = clean
		if err != nil {
			if planErr == nil {
				planErr = fmt.Errorf("call %d (%q): %w", i+1, call.Name, err)
			}
			resolvedStages[i] = currentStage
			continue
		}
		if metadata.HasStage {
			if metadata.Stage < currentStage {
				if planErr == nil {
					planErr = fmt.Errorf("call %d (%q) decreases _stage from %d to %d", i+1, call.Name, currentStage, metadata.Stage)
				}
			} else {
				currentStage = metadata.Stage
			}
		}
		resolvedStages[i] = currentStage
	}

	stages := make([]callStage, 0, len(calls))
	for start := 0; start < len(calls); {
		end := start + 1
		for end < len(calls) && resolvedStages[end] == resolvedStages[start] {
			end++
		}
		stages = append(stages, callStage{start: start, end: end})
		start = end
	}
	return executionCalls, stages, planErr
}

// executionToolCalls returns the same stripped copies used for dispatch to
// post-dispatch classifiers and guards. dispatchCalls remains the authoritative
// preflight gate and performs the complete validation before any side effect.
func executionToolCalls(calls []llm.ToolCall) []llm.ToolCall {
	executionCalls, _, _ := planToolStages(calls)
	return executionCalls
}

func invalidStagePlanText(err error) string {
	return "invalid tool stage plan: " + err.Error() + ". _stage must be an integer greater than or equal to 1, and explicit stages must be non-decreasing"
}

const (
	readResultContextPercent          = 20
	readResultContextTokenCap         = 48_000
	readResultMinimumReceiptBytes     = 64
	readResultHookReserveBytes        = 8 * 1024
	readResultArchiveHintReserveBytes = 8 * 1024
)

type readResultBatchBudget struct{ maxBytes int }
type readResultBatchBudgetContextKey struct{}
type toolResultByteLimit struct {
	maxBytes      int
	dispatchBytes int
}
type toolResultByteLimitsContextKey struct{}

func withReadResultBatchBudget(ctx context.Context, maxBytes int) context.Context {
	return context.WithValue(ctx, readResultBatchBudgetContextKey{}, readResultBatchBudget{maxBytes: maxBytes})
}

// toolDispatchLiveInputTokens combines the current request's full estimate with
// the assistant message appended after it when provider usage is unavailable.
// With measured usage, triggerTokens keeps the actual-input-plus-delta path.
func (a *Agent) toolDispatchLiveInputTokens(lastInput, boundary int, requestEstimate ContextEstimate) int {
	if lastInput > 0 {
		return a.triggerTokens(lastInput, boundary)
	}
	if boundary < 0 {
		boundary = 0
	}
	if boundary > len(a.transcript) {
		boundary = len(a.transcript)
	}
	return requestEstimate.Total + estimateTokens(a.providerVisibleMessages(a.transcript[boundary:]))
}

// readResultBatchByteBudget reserves the next response's resolved output
// allowance, then gives all reads in this tool turn one shared fraction of the
// remaining context. bytesPerToken matches the agent's coarse context estimate.
func (a *Agent) readResultBatchByteBudget(liveInputTokens int) int {
	window := a.window()
	outputTokens := llm.ResolveMaxTokens(llm.Request{
		MaxTokens:            a.maxOutputTokens,
		EstimatedInputTokens: liveInputTokens,
	}, window, a.registry.OutputLimit(a.model))
	remaining := window - liveInputTokens - outputTokens
	if remaining <= 0 {
		return 0
	}
	budgetTokens := min(readResultContextTokenCap, remaining*readResultContextPercent/100)
	return budgetTokens * bytesPerToken
}

// dispatchCalls runs one turn's tool calls. It preflights Harness-owned stage
// metadata, executes stages serially, and retains the existing default-parallel,
// hook-barrier, and mutation-dependency scheduler within each stage. Sink events
// and returned blocks remain in global emission order (design §8).
func (a *Agent) dispatchCalls(ctx context.Context, calls []llm.ToolCall, promptID, turnID int, sink EventSink) ([]llm.ContentBlock, []llm.ParallelToolBatch, llm.Usage) {
	executionCalls, stages, planErr := planToolStages(calls)
	blocks := make([]llm.ContentBlock, len(calls))
	if planErr != nil {
		text := invalidStagePlanText(planErr)
		for i, call := range executionCalls {
			sink.ToolStart(call)
			result := llm.ToolResult{ForID: call.ID, Text: text, IsError: true, ErrorKind: llm.ToolErrorInvalidArgs}
			blocks[i], _ = a.finishToolResult(result, call.Name, sink)
		}
		return blocks, nil, llm.Usage{}
	}

	suppressed, duplicates, overLimit := planCallSuppression(executionCalls, stages)
	if len(suppressed) > 0 {
		sink.Notice(suppressionNotice(duplicates, overLimit))
	}
	ctx = withPerReadResultLimits(ctx, executionCalls, suppressed)

	var parallelBatches []llm.ParallelToolBatch
	crossStageDependencies := a.crossStageMutationDependencies(executionCalls, stages, suppressed)
	actualCompletions := make([]<-chan struct{}, len(executionCalls))
	richEncodedBytes, err := inputimage.ValidateMessages(a.transcript)
	if err != nil {
		// The full retained gate normally makes this unreachable. Treat an
		// invalid baseline as exhausted so no additional rich content is accepted.
		richEncodedBytes = inputimage.MaxTotalEncodedBytes
	}
	var total llm.Usage
	for _, stage := range stages {
		batches, usage := a.dispatchCallStage(ctx, executionCalls, stage, promptID, turnID, sink, blocks, &richEncodedBytes, crossStageDependencies, actualCompletions, suppressed)
		parallelBatches = append(parallelBatches, batches...)
		total = add(total, usage)
	}
	return blocks, parallelBatches, total
}

func withPerReadResultLimits(ctx context.Context, calls []llm.ToolCall, suppressed map[int]string) context.Context {
	budget, ok := ctx.Value(readResultBatchBudgetContextKey{}).(readResultBatchBudget)
	if !ok {
		return ctx
	}
	readCalls := 0
	for i, call := range calls {
		if call.Name == "read" {
			if _, blocked := suppressed[i]; !blocked {
				readCalls++
			}
		}
	}
	if readCalls == 0 {
		return ctx
	}
	share := budget.maxBytes / readCalls
	if share < readResultMinimumReceiptBytes {
		// An exact continuation receipt is fixed protocol overhead. Preserve it even
		// when the estimated content allowance is exhausted.
		share = readResultMinimumReceiptBytes
	}
	dispatchShare := share - readResultHookReserveBytes - readResultArchiveHintReserveBytes
	if dispatchShare < readResultMinimumReceiptBytes {
		dispatchShare = min(share, readResultMinimumReceiptBytes)
	}
	limits := make(map[string]toolResultByteLimit, readCalls)
	for i, call := range calls {
		if call.Name == "read" {
			if _, blocked := suppressed[i]; !blocked {
				limits[call.ID] = toolResultByteLimit{maxBytes: share, dispatchBytes: dispatchShare}
			}
		}
	}
	return context.WithValue(ctx, toolResultByteLimitsContextKey{}, limits)
}

func (a *Agent) dispatchCallStage(ctx context.Context, calls []llm.ToolCall, stage callStage, promptID, turnID int, sink EventSink, blocks []llm.ContentBlock, richEncodedBytes *int, crossStageDependencies [][]int, actualCompletions []<-chan struct{}, suppressed map[int]string) ([]llm.ParallelToolBatch, llm.Usage) {
	var parallelBatches []llm.ParallelToolBatch
	var total llm.Usage
	for i := stage.start; i < stage.end; {
		if reason, blocked := suppressed[i]; blocked {
			blocks[i] = a.emitGuardedCall(calls[i], reason, sink)
			i++
			continue
		}
		if !a.tools.SupportsParallel(calls[i]) || a.hooksHasMatchingHooks(calls[i].Name) {
			waitForActualCompletions(crossStageDependencies[i], actualCompletions)
			block, usage, completion := a.dispatchSequentialCall(ctx, calls[i], promptID, turnID, sink, a.hasMutationConflictBefore(calls, i, stage.end), richEncodedBytes)
			blocks[i] = block
			actualCompletions[i] = completion
			total = add(total, usage)
			i++
			continue
		}

		start := i
		for i < stage.end && a.tools.SupportsParallel(calls[i]) && !a.hooksHasMatchingHooks(calls[i].Name) {
			if _, blocked := suppressed[i]; blocked {
				break
			}
			i++
		}
		batchCalls := calls[start:i]
		dependencies, concurrent := a.mutationDependencies(batchCalls)
		if !concurrent {
			for j, call := range batchCalls {
				globalIndex := start + j
				waitForActualCompletions(crossStageDependencies[globalIndex], actualCompletions)
				block, usage, completion := a.dispatchSequentialCall(ctx, call, promptID, turnID, sink, a.hasMutationConflictBefore(calls, globalIndex, stage.end), richEncodedBytes)
				blocks[globalIndex] = block
				actualCompletions[globalIndex] = completion
				total = add(total, usage)
			}
			continue
		}

		batch := llm.ParallelToolBatch{ToolUseIDs: make([]string, len(batchCalls))}
		for j, call := range batchCalls {
			batch.ToolUseIDs[j] = call.ID
		}
		parallelBatches = append(parallelBatches, batch)

		awaitActual := make([]bool, len(batchCalls))
		for j := range batchCalls {
			awaitActual[j] = a.hasMutationConflictBefore(calls, start+j, stage.end)
		}
		usage := a.dispatchParallelBatch(ctx, batchCalls, dependencies, awaitActual, blocks[start:i], sink, richEncodedBytes, start, crossStageDependencies[start:i], actualCompletions)
		total = add(total, usage)
	}
	return parallelBatches, total
}

// emitGuardedCall reports a suppressed call (design §8.1) without running it:
// no hooks, progress, or diff capture, mirroring the invalid-stage preflight
// path. The guard error keeps the transcript closed and steers the model.
func (a *Agent) emitGuardedCall(call llm.ToolCall, text string, sink EventSink) llm.ContentBlock {
	sink.ToolStart(call)
	result := llm.ToolResult{ForID: call.ID, Text: text, IsError: true, ErrorKind: llm.ToolErrorBlocked}
	block, _ := a.finishToolResult(result, call.Name, sink)
	return block
}

// mutationDependencies builds latest-writer edges for normalized mutation keys.
// The boolean reports whether at least two calls can ever run concurrently.
func (a *Agent) mutationDependencies(calls []llm.ToolCall) ([][]int, bool) {
	dependencies := make([][]int, len(calls))
	latest := make(map[string]int)
	for i, call := range calls {
		seenPredecessors := make(map[int]struct{})
		keys := a.tools.MutationKeys(call)
		for _, key := range keys {
			if predecessor, ok := latest[key]; ok {
				if _, seen := seenPredecessors[predecessor]; !seen {
					dependencies[i] = append(dependencies[i], predecessor)
					seenPredecessors[predecessor] = struct{}{}
				}
			}
		}
		for _, key := range keys {
			latest[key] = i
		}
	}
	if len(calls) < 2 {
		return dependencies, false
	}
	// With edges only to earlier calls, model order is one complete dependency
	// chain exactly when every call directly depends on its immediate predecessor.
	for i := 1; i < len(calls); i++ {
		if !slices.Contains(dependencies[i], i-1) {
			return dependencies, true
		}
	}
	return dependencies, false
}

func (a *Agent) crossStageMutationDependencies(calls []llm.ToolCall, stages []callStage, suppressed map[int]string) [][]int {
	dependencies := make([][]int, len(calls))
	stageIndexes := make([]int, len(calls))
	for stageIndex, stage := range stages {
		for i := stage.start; i < stage.end; i++ {
			stageIndexes[i] = stageIndex
		}
	}
	latest := make(map[string]int)
	for i, call := range calls {
		if _, blocked := suppressed[i]; blocked {
			// A suppressed call never runs, so it has no side effects: it must
			// neither become a dependency target nor replace the real latest
			// writer for its mutation keys.
			continue
		}
		seen := make(map[int]struct{})
		keys := a.tools.MutationKeys(call)
		for _, key := range keys {
			if predecessor, ok := latest[key]; ok && stageIndexes[predecessor] != stageIndexes[i] {
				if _, duplicate := seen[predecessor]; !duplicate {
					dependencies[i] = append(dependencies[i], predecessor)
					seen[predecessor] = struct{}{}
				}
			}
		}
		for _, key := range keys {
			latest[key] = i
		}
	}
	return dependencies
}

func (a *Agent) hasMutationConflictBefore(calls []llm.ToolCall, index, end int) bool {
	keys := a.tools.MutationKeys(calls[index])
	if len(keys) == 0 {
		return false
	}
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	for _, call := range calls[index+1 : end] {
		for _, key := range a.tools.MutationKeys(call) {
			if _, ok := wanted[key]; ok {
				return true
			}
		}
	}
	return false
}

func waitForActualCompletions(dependencies []int, completions []<-chan struct{}) {
	for _, predecessor := range dependencies {
		if completion := completions[predecessor]; completion != nil {
			<-completion
		}
	}
}

func (a *Agent) hooksHasMatchingHooks(target string) bool {
	return a.hooks != nil && a.hooks.HasMatchingHooks(target)
}

func (a *Agent) dispatchSequentialCall(ctx context.Context, call llm.ToolCall, promptID, turnID int, sink EventSink, awaitActualCompletion bool, richEncodedBytes *int) (llm.ContentBlock, llm.Usage, <-chan struct{}) {
	startToolProgress(a.tools, call, sink)
	sink.ToolStart(call)
	diffState := a.snapshotToolDiff(call)
	r, completion := a.dispatchOne(ctx, call, promptID, turnID, sink)
	if awaitActualCompletion && completion != nil && (r.ErrorKind == llm.ToolErrorTimeout || r.ErrorKind == llm.ToolErrorCancelled) {
		<-completion
	}
	diffEvents := a.captureToolDiff(diffState)
	r = acceptRichResult(r, richEncodedBytes, a.registry.SupportsInputModality(a.model, "image"))
	block, usage := a.finishToolResultWithLimit(r, call.Name, sink, toolResultTextLimit(ctx, call.ID))
	clearToolProgress(call, sink)
	reportToolMutation(a.tools, call, r, sink)
	emitToolDiff(call, diffEvents, sink)
	return block, usage, completion
}

type scheduledToolResult struct {
	result           llm.ToolResult
	diffEvents       []toolDiffEvent
	actualCompletion <-chan struct{}
}

func (a *Agent) dispatchParallelBatch(ctx context.Context, calls []llm.ToolCall, dependencies [][]int, awaitActual []bool, blocks []llm.ContentBlock, sink EventSink, richEncodedBytes *int, globalStart int, crossStageDependencies [][]int, actualCompletions []<-chan struct{}) llm.Usage {
	for _, call := range calls {
		startToolProgress(a.tools, call, sink)
		sink.ToolStart(call)
	}

	results := make([]scheduledToolResult, len(calls))
	hasDependents := make([]bool, len(calls))
	for _, predecessors := range dependencies {
		for _, predecessor := range predecessors {
			hasDependents[predecessor] = true
		}
	}
	completed := make([]chan struct{}, len(calls))
	guardFolded := make([]chan struct{}, len(calls))
	for i := range completed {
		completed[i] = make(chan struct{})
		guardFolded[i] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c llm.ToolCall) {
			defer wg.Done()
			defer close(completed[idx])
			for _, predecessor := range dependencies[idx] {
				<-completed[predecessor]
			}
			waitForActualCompletions(crossStageDependencies[idx], actualCompletions)
			diffState := a.snapshotToolDiff(c)
			a.toolRunSem <- struct{}{}
			result, actualCompletion, foldFailureGuard := a.dispatchTool(ctx, c)
			<-a.toolRunSem
			results[idx].actualCompletion = actualCompletion
			if (hasDependents[idx] || awaitActual[idx]) && actualCompletion != nil && (result.ErrorKind == llm.ToolErrorTimeout || result.ErrorKind == llm.ToolErrorCancelled) {
				<-actualCompletion
			}
			// Capture and render after actual completion and before releasing
			// same-path successors so each mutation retains its own incremental view.
			results[idx].diffEvents = a.captureToolDiff(diffState)
			// Failure-guard state and warning text follow emission order rather than
			// worker completion order. Folding before completed closes also lets a
			// same-path successor observe a successful mutation's reset.
			if idx > 0 {
				<-guardFolded[idx-1]
			}
			if foldFailureGuard && a.failGuard != nil {
				result = a.failGuard.afterCall(a.tools, c, result)
			}
			results[idx].result = result
			close(guardFolded[idx])
		}(i, call)
	}
	wg.Wait()

	var total llm.Usage
	for i, scheduled := range results {
		actualCompletions[globalStart+i] = scheduled.actualCompletion
		r := acceptRichResult(scheduled.result, richEncodedBytes, a.registry.SupportsInputModality(a.model, "image"))
		block, usage := a.finishToolResultWithLimit(r, calls[i].Name, sink, toolResultTextLimit(ctx, calls[i].ID))
		blocks[i] = block
		clearToolProgress(calls[i], sink)
		reportToolMutation(a.tools, calls[i], r, sink)
		emitToolDiff(calls[i], scheduled.diffEvents, sink)
		total = add(total, usage)
	}
	return total
}

func safeToolResultForSink(r llm.ToolResult) llm.ToolResult {
	if len(r.Content) == 0 {
		return r
	}
	r.Content = append([]llm.ContentBlock(nil), r.Content...)
	for i := range r.Content {
		if r.Content[i].ImageEncodedBytes == 0 {
			r.Content[i].ImageEncodedBytes = len(r.Content[i].ImageData)
		}
		if r.Content[i].ImageBytes == 0 {
			r.Content[i].ImageBytes = decodedImageSize(r.Content[i].ImageData)
		}
		r.Content[i].ImageData = ""
	}
	return r
}

func decodedImageSize(data string) int {
	n := len(data) * 3 / 4
	if strings.HasSuffix(data, "==") {
		n -= 2
	} else if strings.HasSuffix(data, "=") {
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}

// dispatchTool executes a hook-free call. The boolean asks its caller to fold
// the result into the failure guard; parallel workers order that fold by emission
// index so completion timing cannot change guard semantics or warning text.
func (a *Agent) dispatchTool(ctx context.Context, call llm.ToolCall) (llm.ToolResult, <-chan struct{}, bool) {
	// Parallel workers bypass dispatchOne because hooks are island barriers, but
	// malformed streamed calls and Kimi's hosted echo still need the same handling.
	if call.InvalidInputError != "" {
		return llm.ToolResult{ForID: call.ID, Text: invalidToolInputResult(call), IsError: true, ErrorKind: llm.ToolErrorInvalidArgs}, nil, false
	}
	if a.isKimiWebSearchCall(call) {
		text := strings.TrimSpace(string(call.Input))
		if text == "" {
			text = "{}"
		}
		return llm.ToolResult{ForID: call.ID, Text: text}, nil, false
	}
	if modality, ok := a.tools.RequiredModality(call); ok && !a.registry.SupportsInputModality(a.model, modality) {
		return llm.ToolResult{
			ForID:     call.ID,
			Text:      fmt.Sprintf("tool %q requires %s input, but the current model does not advertise %s support", call.Name, modality, modality),
			IsError:   true,
			ErrorKind: llm.ToolErrorUnsupportedModality,
		}, nil, false
	}
	if g := a.failGuard; g != nil {
		if res, blocked := g.beforeCall(call); blocked {
			return res, nil, false
		}
	}
	dispatchLimits := tools.DispatchLimits{}
	if limits, ok := ctx.Value(toolResultByteLimitsContextKey{}).(map[string]toolResultByteLimit); ok {
		dispatchLimits.MaxResultBytes = limits[call.ID].dispatchBytes
	}
	res, completion := a.tools.DispatchWithCompletionLimits(ctx, call, dispatchLimits)
	return res, completion, true
}

func acceptRichResult(r llm.ToolResult, encodedTotal *int, imageSupported bool) llm.ToolResult {
	if len(r.Content) == 0 {
		return r
	}
	if r.IsError {
		r.Content = nil
		return r
	}
	if !imageSupported {
		r.Text = "tool result includes images, but the current model does not advertise image support"
		r.Content = nil
		r.IsError = true
		r.ErrorKind = llm.ToolErrorUnsupportedModality
		r.Truncated = false
		r.OriginalText = ""
		r.OriginalBytes = 0
		r.ShownBytes = 0
		return r
	}
	nextTotal, err := inputimage.ValidateBlocks(r.Content, *encodedTotal)
	if err != nil {
		r.Text = "tool result images rejected: " + err.Error()
		r.Content = nil
		r.IsError = true
		r.ErrorKind = llm.ToolErrorUnsupportedModality
		r.Truncated = false
		r.OriginalText = ""
		r.OriginalBytes = 0
		r.ShownBytes = 0
		return r
	}
	*encodedTotal = nextTotal
	return r
}

func (a *Agent) finishToolResult(r llm.ToolResult, toolName string, sink EventSink) (llm.ContentBlock, llm.Usage) {
	return a.finishToolResultWithLimit(r, toolName, sink, 0)
}

func (a *Agent) finishToolResultWithLimit(r llm.ToolResult, toolName string, sink EventSink, maxBytes int) (llm.ContentBlock, llm.Usage) {
	var notice string
	r, notice = a.prepareToolResult(r, sink)
	if maxBytes > 0 && len(r.Text) > maxBytes {
		r.Text = utf8Prefix(r.Text, maxBytes)
	}
	sink.ToolResult(safeToolResultForSink(r))
	if notice != "" {
		sink.Notice(notice)
	}
	return resultBlock(r, toolName), r.Usage
}

// startToolProgress hands the renderer the tool's live-progress closure before
// the call begins, so the wait ticker can show child-run activity while the
// (possibly long, blocking) tool runs. Sinks that do not implement
// ToolProgressSink, or tools without ProgressStarter, are silently skipped.
func startToolProgress(registry *tools.Registry, call llm.ToolCall, sink EventSink) {
	ps, ok := sink.(ToolProgressSink)
	if !ok || registry == nil {
		return
	}
	progress, ok := registry.ProgressFor(call)
	if !ok {
		return
	}
	ps.ToolProgress(call, progress)
}

// clearToolProgress drops the tool's progress closure after the call returns so
// a stale snapshot cannot leak into the next tool's wait ticker.
func clearToolProgress(call llm.ToolCall, sink EventSink) {
	if ps, ok := sink.(ToolProgressSink); ok {
		ps.ToolProgress(call, nil)
	}
}

type toolDiffState struct {
	enabled bool
	paths   []string
	before  []diff.Snapshot
}

type toolDiffEvent struct {
	path   string
	text   string
	notice string
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

// captureToolDiff performs all filesystem observation and rendering while the
// call's dependency slot is still held. EventSink emission remains on the parent.
func (a *Agent) captureToolDiff(state toolDiffState) []toolDiffEvent {
	if !state.enabled {
		return nil
	}
	after := diff.SnapshotPaths(state.paths)
	var events []toolDiffEvent
	for _, fd := range diff.RenderSnapshots(state.before, after, diff.Options{}) {
		switch {
		case fd.Err != nil:
			events = append(events, toolDiffEvent{notice: fmt.Sprintf("[diff: skipped %s: %v]", fd.Path, fd.Err)})
		case fd.BinarySkipped:
			events = append(events, toolDiffEvent{notice: fmt.Sprintf("[diff: skipped binary file %s]", fd.Path)})
		case strings.TrimSpace(fd.Text) != "":
			events = append(events, toolDiffEvent{path: fd.Path, text: fd.Text})
		}
	}
	return events
}

func emitToolDiff(call llm.ToolCall, events []toolDiffEvent, sink EventSink) {
	for _, event := range events {
		if event.notice != "" {
			sink.Notice(event.notice)
			continue
		}
		if ds, ok := sink.(ToolDiffSink); ok {
			ds.ToolDiff(call, event.path, event.text)
		}
	}
}

func reportToolMutation(registry *tools.Registry, call llm.ToolCall, result llm.ToolResult, sink EventSink) {
	if result.IsError || registry == nil {
		return
	}
	paths, ok := registry.MutatedPaths(call)
	if !ok {
		return
	}
	mutationSink, ok := sink.(ToolMutationSink)
	if !ok {
		return
	}
	mutationSink.ToolMutation(call, append([]string(nil), paths...))
}

func (a *Agent) dispatchOne(ctx context.Context, call llm.ToolCall, promptID, turnID int, sink EventSink) (llm.ToolResult, <-chan struct{}) {
	if call.InvalidInputError != "" {
		return llm.ToolResult{ForID: call.ID, Text: invalidToolInputResult(call), IsError: true, ErrorKind: llm.ToolErrorInvalidArgs}, nil
	}
	if a.isKimiWebSearchCall(call) {
		text := strings.TrimSpace(string(call.Input))
		if text == "" {
			text = "{}"
		}
		return llm.ToolResult{ForID: call.ID, Text: text}, nil
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
		reportHookDiagnostics(sink, res.Diagnostics)
		for _, notice := range res.Notices {
			sink.Notice(notice)
		}
		preContext = append(preContext, res.AdditionalContext...)
		if res.Block {
			reason := res.Reason()
			if reason == "" {
				reason = "blocked by PreToolUse hook"
			}
			return llm.ToolResult{ForID: call.ID, Text: reason, IsError: true, ErrorKind: llm.ToolErrorHookBlocked}, nil
		}
	}

	r, completion, foldFailureGuard := a.dispatchTool(ctx, call)
	if foldFailureGuard && a.failGuard != nil {
		r = a.failGuard.afterCall(a.tools, call, r)
	}
	if len(preContext) > 0 {
		appendHookContext(&r, preContext, toolResultHookTextLimit(ctx, call.ID))
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
		reportHookDiagnostics(sink, res.Diagnostics)
		for _, notice := range res.Notices {
			sink.Notice(notice)
		}
		if len(res.AdditionalContext) > 0 {
			appendHookContext(&r, res.AdditionalContext, toolResultHookTextLimit(ctx, call.ID))
		}
		if res.Block {
			reason := res.Reason()
			if reason == "" {
				reason = "blocked by PostToolUse hook"
			}
			r.Text = reason
			r.Content = nil
			r.IsError = true
			r.ErrorKind = llm.ToolErrorHookBlocked
		}
	}
	return r, completion
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
	case "write", "edit":
		// A huge file body is the common cause of truncated streamed args; the
		// fix is smaller writes, not another monolithic retry.
		if strings.Contains(call.InvalidInputError, "unexpected end of JSON input") ||
			strings.Contains(call.InvalidInputError, "unexpected EOF") {
			msg += " The arguments were truncated mid-JSON; write the file in smaller chunks with write, then use edit to append additional content."
		}
	}
	return msg
}

func (a *Agent) prepareToolResult(r llm.ToolResult, sink EventSink) (llm.ToolResult, string) {
	archiver, _ := sink.(ToolResultArchiver)
	return toolresult.PrepareTruncated(r, archiver)
}

func resultBlock(r llm.ToolResult, toolName string) llm.ContentBlock {
	return llm.ContentBlock{
		Kind:          llm.BlockToolResult,
		ToolName:      toolName,
		ResultForID:   r.ForID,
		ResultText:    r.Text,
		ResultError:   r.IsError,
		ResultUseless: r.Useless,
		ResultContent: append([]llm.ContentBlock(nil), r.Content...),
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
	provider, ok := sink.(RequestContextProvider)
	if !ok {
		return append([]string(nil), extraContext...)
	}
	return mergeRequestContext(extraContext, provider.RequestContext())
}

// estimateRequestContext gathers request context for local sizing only. No
// request is sent, so use the sink's non-consuming peek when available and
// otherwise omit dynamic context rather than consuming a one-shot signal.
func (a *Agent) estimateRequestContext(extraContext []string, sink EventSink) []string {
	peeker, ok := sink.(RequestContextPeeker)
	if !ok {
		return append([]string(nil), extraContext...)
	}
	return mergeRequestContext(extraContext, peeker.PeekRequestContext())
}

func mergeRequestContext(extraContext, items []string) []string {
	out := append([]string(nil), extraContext...)
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out
}

func appendMissingRequestContext(base, fresh []string) []string {
	out := append([]string(nil), base...)
	seen := make(map[string]struct{}, len(out))
	for _, item := range out {
		seen[item] = struct{}{}
	}
	for _, item := range fresh {
		if strings.TrimSpace(item) == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		out = append(out, item)
		seen[item] = struct{}{}
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

func toolResultTextLimit(ctx context.Context, callID string) int {
	limits, ok := ctx.Value(toolResultByteLimitsContextKey{}).(map[string]toolResultByteLimit)
	if !ok {
		return 0
	}
	return limits[callID].maxBytes
}

func toolResultHookTextLimit(ctx context.Context, callID string) int {
	limits, ok := ctx.Value(toolResultByteLimitsContextKey{}).(map[string]toolResultByteLimit)
	if !ok {
		return 0
	}
	limit, ok := limits[callID]
	if !ok {
		return 0
	}
	return max(limit.maxBytes-readResultArchiveHintReserveBytes, readResultMinimumReceiptBytes)
}

func appendHookContext(r *llm.ToolResult, ctx []string, maxBytes int) {
	text := llm.RequestContextText(ctx)
	if text == "" {
		return
	}
	separator := ""
	if r.Text != "" {
		separator = "\n\n"
	}
	addition := separator + text
	if maxBytes > 0 && len(r.Text)+len(addition) > maxBytes {
		available := maxBytes - len(r.Text)
		if available <= 0 {
			return
		}
		const marker = "\n[hook context truncated]"
		if available <= len(marker) {
			addition = marker[:available]
		} else {
			keep := available - len(marker)
			if keep > len(addition) {
				keep = len(addition)
			}
			addition = utf8Prefix(addition, keep) + marker
		}
	}
	r.Text += addition
}

func utf8Prefix(s string, maxBytes int) string {
	if maxBytes >= len(s) {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	for maxBytes > 0 && maxBytes < len(s) && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func toolResponsePayload(r llm.ToolResult) map[string]any {
	return map[string]any{
		"tool_use_id": r.ForID,
		"text":        r.Text,
		"is_error":    r.IsError,
		"truncated":   r.Truncated,
	}
}

// streamWithRetry runs one request shape, re-requesting it from scratch when it
// fails mid-flight with a retryable error. startAttempt keeps physical attempt
// IDs absolute across compatibility/request-shape reruns. Partial output from a
// failed attempt is never committed; wasted carries billed usage from local
// transport retries and never drives the compaction trigger.
func (a *Agent) streamWithRetry(ctx context.Context, req llm.Request, sink EventSink, turn, startAttempt int, estimate ContextEstimate) (res turnResult, wasted llm.Usage, err error) {
	if err := validateRequestImageContent(req.Messages); err != nil {
		return turnResult{}, llm.Usage{}, fmt.Errorf("validate model request images: %w", err)
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return res, wasted, err
		}
		physicalAttempt := startAttempt + attempt
		sink.TurnAttemptStart(turn, physicalAttempt, estimate)
		res, err = a.stream(ctx, req, sink)
		res.attempts = physicalAttempt
		sink.TurnAttemptComplete(TurnAttemptUsage{Turn: turn, Attempt: physicalAttempt, Usage: res.usage})
		if err == nil || attempt >= streamRetries || !retryableStreamError(err) {
			return res, wasted, err
		}
		retryAfter := streamRetryAfter(err)
		if retryAfter > maxStreamRetryAfter {
			return res, wasted, err
		}
		wasted = add(wasted, res.usage)
		delay := retry.Next(attempt, retryAfter)
		retryEvent := modelRequestEventFromError(err, llm.ModelRequestRetryScheduled)
		retryEvent.Outcome = ""
		retryEvent.Attempt = physicalAttempt + 1
		retryEvent.MaxAttempts = startAttempt + streamRetries
		retryEvent.RetryDelayMS = delay.Milliseconds()
		emitModelRequestEvent(sink, retryEvent)
		if abandon, ok := sink.(TurnAttemptAbandonSink); ok {
			abandon.TurnAttemptAbandoned(turn, physicalAttempt)
		}
		discarded := ""
		if n := totalTokens(res.usage); n > 0 {
			discarded = fmt.Sprintf("; discarded ~%d tokens", n)
		}
		sink.Notice(fmt.Sprintf("[stream interrupted: %v; retrying turn in %s%s]", err, delay, discarded))
		if serr := a.sleep(ctx, delay); serr != nil {
			// This physical attempt was already abandoned and moved into wasted.
			// Preserve its terminal attempt/response metadata for diagnostics, but
			// clear discarded output and usage so final accounting cannot count it
			// again as an accepted terminal attempt.
			res.text = ""
			res.reasoning = nil
			res.content = nil
			res.toolCalls = nil
			res.usage = llm.Usage{}
			return res, wasted, serr
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
	if strings.Contains(code, "previous_interaction") {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "previous_response_id") ||
		strings.Contains(msg, "previous response") ||
		strings.Contains(msg, "previous_interaction_id") ||
		strings.Contains(msg, "previous interaction")
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
		return NoticeStoppedMaxTokens
	case llm.StopStop:
		return NoticeStoppedStopSequence
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
	textBlocks := make(map[int][]byte)
	orderedBlocks := make(map[int]llm.ContentBlock)
	anthropicToolSearch := false
	terminalModelEventSeen := false

	for ev, err := range a.provider.Stream(ctx, req) {
		if err != nil {
			if !terminalModelEventSeen {
				state := llm.ModelRequestFailed
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					state = llm.ModelRequestCancelled
				}
				emitModelRequestEvent(sink, modelRequestEventFromError(err, state))
			}
			res.text = string(text)
			return res, err
		}
		switch ev.Kind {
		case llm.EventTextDelta:
			text = append(text, ev.Text...)
			textBlocks[ev.Index] = append(textBlocks[ev.Index], ev.Text...)
			sink.TextDelta(ev.Text)
		case llm.EventReasoningSummary:
			if summary := reasoningSummaryText(ev.Text); summary != "" {
				sink.ReasoningSummary(summary)
			}
			if block, ok := llm.PersistedReasoningBlock(ev); ok {
				block.ReasoningReplayDomain = a.reasoningReplayDomain
				res.reasoning = append(res.reasoning, block)
				orderedBlocks[ev.Index] = block
			}
		case llm.EventInteractionStep:
			if block, ok := llm.PersistedInteractionStep(ev); ok {
				block.ReasoningReplayDomain = a.reasoningReplayDomain
				res.reasoning = append(res.reasoning, block)
			}
		case llm.EventResponsesToolSearch:
			if block, ok := llm.PersistedResponsesToolSearch(ev); ok {
				block.ReasoningReplayDomain = a.reasoningReplayDomain
				res.reasoning = append(res.reasoning, block)
			}
		case llm.EventAnthropicToolSearch:
			if block, ok := llm.PersistedAnthropicToolSearch(ev); ok {
				block.ReasoningReplayDomain = a.reasoningReplayDomain
				res.reasoning = append(res.reasoning, block)
				orderedBlocks[ev.Index] = block
				anthropicToolSearch = true
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
				ID:        ev.ToolID,
				Name:      ev.ToolName,
				Namespace: ev.ToolNamespace,
				Input:     ev.ToolInput,
			})
		case llm.EventToolCallDelta:
			sink.ToolUseDelta(ev.Index, ev.ArgsDelta)
		case llm.EventToolCallDone:
			call := llm.ToolCall{
				ID:                ev.ToolID,
				Name:              ev.ToolName,
				Namespace:         ev.ToolNamespace,
				Input:             ev.ToolInput,
				InvalidInputError: ev.InvalidInputError,
			}
			res.toolCalls = append(res.toolCalls, call)
			orderedBlocks[ev.Index] = llm.ContentBlock{
				Kind:          llm.BlockToolUse,
				ToolUseID:     call.ID,
				ToolName:      call.Name,
				ToolNamespace: call.Namespace,
				ToolInput:     call.Input,
			}
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
		case llm.EventModelRequest:
			if ev.ModelRequest == nil {
				continue
			}
			event := *ev.ModelRequest
			switch event.State {
			case llm.ModelRequestFailed, llm.ModelRequestCancelled:
				terminalModelEventSeen = true
			case llm.ModelRequestUpstreamAttemptFailed:
				terminalModelEventSeen = event.Outcome == llm.ModelRequestOutcomeTerminal
			}
			emitModelRequestEvent(sink, event)
		}
	}

	res.text = string(text)
	if anthropicToolSearch {
		for index, blockText := range textBlocks {
			if len(blockText) > 0 {
				orderedBlocks[index] = llm.ContentBlock{Kind: llm.BlockText, Text: string(blockText)}
			}
		}
		indexes := make([]int, 0, len(orderedBlocks))
		for index := range orderedBlocks {
			indexes = append(indexes, index)
		}
		slices.Sort(indexes)
		for _, index := range indexes {
			res.content = append(res.content, orderedBlocks[index])
		}
	}
	if len(res.toolCalls) > 0 {
		// Tool calls are authoritative. Some compatible providers incorrectly
		// report a normal stop after streaming complete calls.
		res.stopReason = llm.StopToolUse
	} else if res.stopReason == llm.StopToolUse {
		return res, &llm.APIError{
			Code:    "invalid_tool_use_stream",
			Message: "provider ended with tool_use but emitted no usable tool call",
		}
	}
	return res, nil
}

func emitModelRequestEvent(sink EventSink, event llm.ModelRequestEvent) {
	if statusSink, ok := sink.(ModelRequestEventSink); ok {
		statusSink.ModelRequestEvent(event)
	}
}

func modelRequestEventFromError(err error, state llm.ModelRequestState) llm.ModelRequestEvent {
	event := llm.ModelRequestEvent{
		State:   state,
		Outcome: llm.ModelRequestOutcomeTerminal,
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		event.StatusCode = apiErr.StatusCode
		event.Code = apiErr.Code
		event.Message = apiErr.Message
		event.ResponsePayload = apiErr.ResponsePayload
		event.Retryable = apiErr.Retryable
		event.RetryAfterMS = apiErr.RetryAfter.Milliseconds()
		if apiErr.Diagnostic != nil {
			event.Stage = apiErr.Diagnostic.Stage
			event.ProxyInstanceID = apiErr.Diagnostic.ProxyInstanceID
			event.ProxyRequestID = apiErr.Diagnostic.ProxyRequestID
			event.UpstreamRequestID = apiErr.Diagnostic.UpstreamRequestID
			event.TraceID = apiErr.Diagnostic.TraceID
			event.SpanID = apiErr.Diagnostic.SpanID
			event.TargetID = apiErr.Diagnostic.TargetID
			event.Provider = apiErr.Diagnostic.Provider
			event.APIType = apiErr.Diagnostic.APIType
			event.Model = apiErr.Diagnostic.Model
		}
		return event
	}
	if err != nil {
		event.Message = err.Error()
	}
	return event
}

func reasoningSummaryText(text string) string {
	return strings.TrimSpace(text)
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
	var out SteerInput
	for _, input := range a.drainSteerInputs() {
		if strings.TrimSpace(input.Text) != "" {
			if out.Text != "" {
				out.Text += "\n\n"
			}
			out.Text += input.Text
		}
		out.Images = append(out.Images, cloneImageBlocks(input.Images)...)
		out.RequestContext = append(out.RequestContext, cleanContext(input.RequestContext)...)
		out.DeliveryMetadata = append(out.DeliveryMetadata, input.DeliveryMetadata...)
	}
	return out
}

func (a *Agent) drainSteerInputs() []SteerInput {
	if a.steer == nil {
		return nil
	}
	var out []SteerInput
	for {
		select {
		case input := <-a.steer:
			out = append(out, input)
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

// assistantMessage builds the assistant message for a completed turn. Most
// dialects use normalized reasoning/text/tool ordering; Anthropic hosted search
// supplies exact indexed content because its search blocks can interleave text
// before the eventual client tool_use.
func (a *Agent) assistantMessage(res turnResult) llm.Message {
	if len(res.content) > 0 {
		return llm.Message{
			Role:    llm.RoleAssistant,
			Time:    a.now(),
			Phase:   llm.ResolvedAssistantPhase(res.phase, res.stopReason),
			Content: res.content,
		}
	}
	message := llm.BuildAssistantMessage(res.reasoning, res.text, res.toolCalls, res.phase, res.stopReason)
	message.Time = a.now()
	return message
}

// maxTurnsNotice is the exact guard message printed when the turn budget is
// exhausted (design §8.1).
func maxTurnsNotice(maxTurns int) string {
	return fmt.Sprintf("[stopped: reached max turns (%d)]", maxTurns)
}

func add(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:        a.InputTokens + b.InputTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:            a.CostUSD + b.CostUSD,
		CostKnown:          aggregateCostKnown(a, b),
	}
}

func cloneToolSpecs(specs []llm.ToolSchema) []llm.ToolSchema {
	out := append([]llm.ToolSchema(nil), specs...)
	for i := range out {
		out[i].Parameters = append(json.RawMessage(nil), out[i].Parameters...)
	}
	return out
}

func cloneToolGroups(groups []llm.ToolGroup) []llm.ToolGroup {
	out := append([]llm.ToolGroup(nil), groups...)
	for i := range out {
		out[i].Tools = cloneToolSpecs(groups[i].Tools)
	}
	return out
}

func equalToolSpecs(left, right []llm.ToolSchema) bool {
	return slices.EqualFunc(left, right, func(left, right llm.ToolSchema) bool {
		return left.Name == right.Name && left.Description == right.Description && string(left.Parameters) == string(right.Parameters)
	})
}

func equalToolGroups(left, right []llm.ToolGroup) bool {
	return slices.EqualFunc(left, right, func(left, right llm.ToolGroup) bool {
		return left.Name == right.Name && left.Description == right.Description && equalToolSpecs(left.Tools, right.Tools)
	})
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

func appendPromptContext(contexts ...[]string) []string {
	var out []string
	for _, context := range contexts {
		out = append(out, context...)
	}
	return out
}

// mergeUsage merges a cumulative usage snapshot into acc element-wise. The
// provider contract says snapshots are cumulative; max keeps a zeroed or
// partial late frame from erasing earlier numbers (spec §3).
func mergeUsage(acc, in llm.Usage) llm.Usage {
	out := llm.Usage{
		InputTokens:        max(acc.InputTokens, in.InputTokens),
		OutputTokens:       max(acc.OutputTokens, in.OutputTokens),
		CacheReadTokens:    max(acc.CacheReadTokens, in.CacheReadTokens),
		CacheWriteTokens:   max(acc.CacheWriteTokens, in.CacheWriteTokens),
		CacheWrite1hTokens: max(acc.CacheWrite1hTokens, in.CacheWrite1hTokens),
		ReasoningTokens:    max(acc.ReasoningTokens, in.ReasoningTokens),
		CostUSD:            mergeCost(acc, in),
		CostKnown:          mergeCostKnown(acc, in),
	}
	if in.ServiceTier != "" {
		out.ServiceTier = in.ServiceTier
	} else {
		out.ServiceTier = acc.ServiceTier
	}
	if in.Speed != "" {
		out.Speed = in.Speed
	} else {
		out.Speed = acc.Speed
	}
	return out
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
		u.CacheWrite1hTokens != 0 ||
		u.ReasoningTokens != 0
}
