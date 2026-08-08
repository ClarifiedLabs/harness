package llm

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
)

// Provider runs one model call as a stream of events. Concrete implementations
// live in the provider dialect packages under internal/llm.
type Provider interface {
	Name() string // "openai" | "responses" | "anthropic" | "interactions"

	// Stream runs one model call. The iterator yields events until a Done
	// event or a terminal error (yielded at most once, last). Consumer break
	// or ctx cancellation aborts the underlying HTTP request.
	//
	// Usage events carry cumulative snapshots of the whole call, never
	// deltas; consumers may merge them with element-wise max.
	Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error]
}

// InputTokenCounter is an optional Provider side interface for exact or
// provider-specific preflight input-token counts.
type InputTokenCounter interface {
	CountInputTokens(ctx context.Context, req Request) (InputTokenCount, error)
}

// ErrInputTokenCountUnsupported marks providers without preflight counting.
var ErrInputTokenCountUnsupported = errors.New("input token count unsupported")

// InputTokenCountScope describes which model-visible input a provider counted.
// Stateful continuation APIs may count either the effective logical context or
// only the payload sent for the current request.
type InputTokenCountScope string

const (
	InputTokenCountScopeUnknown          InputTokenCountScope = "unknown"
	InputTokenCountScopeEffectiveContext InputTokenCountScope = "effective_context"
	InputTokenCountScopeRequestPayload   InputTokenCountScope = "request_payload"
)

// NormalizeInputTokenCountScope maps missing and unrecognized values to
// unknown so older providers and model proxies remain compatible.
func NormalizeInputTokenCountScope(scope InputTokenCountScope) InputTokenCountScope {
	switch scope {
	case InputTokenCountScopeEffectiveContext, InputTokenCountScopeRequestPayload:
		return scope
	default:
		return InputTokenCountScopeUnknown
	}
}

// InputTokenCount is a provider-specific input-token count for one request.
type InputTokenCount struct {
	InputTokens int                  `json:"input_tokens"`
	Source      string               `json:"source,omitempty"`
	Scope       InputTokenCountScope `json:"scope,omitempty"`
}

// RequestPurpose classifies why a model request exists. Values are deliberately
// bounded so they are safe to use as a Prometheus label.
type RequestPurpose string

const (
	RequestPurposeUnknown       RequestPurpose = "unknown"
	RequestPurposeTurn          RequestPurpose = "turn"
	RequestPurposeCompaction    RequestPurpose = "compaction"
	RequestPurposePrewarm       RequestPurpose = "prewarm"
	RequestPurposeBranchSummary RequestPurpose = "branch_summary"
)

// NormalizeRequestPurpose maps missing or unrecognized values to unknown,
// preventing external proxy clients from creating unbounded metric labels.
func NormalizeRequestPurpose(purpose RequestPurpose) RequestPurpose {
	switch purpose {
	case RequestPurposeTurn, RequestPurposeCompaction, RequestPurposePrewarm, RequestPurposeBranchSummary:
		return purpose
	default:
		return RequestPurposeUnknown
	}
}

// Request is one model call's worth of input, provider-neutral.
type Request struct {
	Model       string          `json:"model"`
	Purpose     RequestPurpose  `json:"purpose,omitempty"` // harness/model-proxy metadata; never forwarded upstream
	System      string          `json:"system,omitempty"`
	Messages    []Message       `json:"messages,omitempty"`
	Tools       []ToolSchema    `json:"tools,omitempty"`
	ServerTools []ServerTool    `json:"server_tools,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`  // 0 = provider policy (see design §5.4)
	Temperature *float64        `json:"temperature,omitempty"` // nil = omit
	Reasoning   ReasoningConfig `json:"reasoning,omitempty"`
	StopSeqs    []string        `json:"stop_seqs,omitempty"`
	// ServiceTier selects a provider-advertised scheduling/cost tier such as
	// priority or flex. Before provider dispatch the model proxy resolves the
	// selected catalog target into this wire value and any Speed/Betas below.
	ServiceTier string `json:"service_tier,omitempty"`
	// Speed selects a provider's named inference speed when that provider does
	// not use service_tier (for example Anthropic fast mode).
	Speed string `json:"speed,omitempty"`
	// Betas contains bounded provider beta feature identifiers required by the
	// selected catalog tier. Dialects ignore identifiers they do not own.
	Betas []string `json:"betas,omitempty"`

	// EstimatedInputTokens is the caller's estimate of all model-visible input
	// tokens for this request. Dialects use it to keep max output tokens within
	// the context window. Zero means "estimate from the neutral request".
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
	// ContextWindowHint is the caller's effective context window for this
	// request, including overrides or provider errors learned at runtime. Zero
	// means "use provider configuration".
	ContextWindowHint int `json:"context_window_hint,omitempty"`

	StoreResponse      bool     `json:"store_response,omitempty"`
	PreviousResponseID string   `json:"previous_response_id,omitempty"`
	RequestContext     []string `json:"request_context,omitempty"`

	// ProxySessionID is a harness-local session key used as a sticky-routing hint
	// and to isolate connection-affine WebSocket transports. The CLI, not the
	// proxy, owns continuation anchors. Concrete provider dialects must not
	// forward this value upstream.
	ProxySessionID string `json:"proxy_session_id,omitempty"`

	// CacheAffinityID is a stable harness-local conversation key used by
	// harness-model-proxy to derive the provider-facing prompt cache key. Unlike
	// ProxySessionID, it survives continuation resets such as compaction, model
	// switches, and branch navigation. Concrete provider dialects must not
	// forward this value upstream.
	CacheAffinityID string `json:"cache_affinity_id,omitempty"`

	// PromptCacheKey is a provider-facing cache-affinity hint emitted as
	// prompt_cache_key or session_id by providers that support it. When requests
	// flow through harness-model-proxy, the proxy derives this from
	// CacheAffinityID instead of forwarding the local conversation id.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`

	// CachePolicy declares caller-owned semantic cache boundaries. Dialects
	// choose provider-specific breakpoint placement.
	CachePolicy CachePolicy `json:"cache_policy,omitempty"`
}

type CacheTTL string

const (
	CacheTTLDefault  CacheTTL = "5m"
	CacheTTLExtended CacheTTL = "1h"
)

type CachePolicy struct {
	StaticTTL           CacheTTL `json:"static_ttl,omitempty"`
	StableMessagePrefix int      `json:"stable_message_prefix,omitempty"`
}

// ResponseState is resumable provider continuation state. PreviousResponseID
// carries either an OpenAI previous_response_id or a Gemini
// previous_interaction_id; the neutral name keeps continuation bookkeeping out
// of the agent's provider-specific code.
type ResponseState struct {
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	AnchorMessages     int    `json:"anchor_messages,omitempty"`
	AnchorDigest       string `json:"anchor_digest,omitempty"`
}

// ToolSchema is the model-facing declaration of one tool. Parameters is the raw
// JSON Schema object owned by the tool layer; it is passed through unchanged.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema object, owned by the tool layer
}

const (
	ServerToolWebSearch = "web_search"

	ServerToolKindOpenAIWebSearch     = "openai_web_search"
	ServerToolKindAnthropicWebSearch  = "anthropic_web_search"
	ServerToolKindOpenRouterWebSearch = "openrouter_web_search"
	ServerToolKindMimoWebSearch       = "mimo_web_search"
	ServerToolKindKimiWebSearch       = "kimi_web_search"
	ServerToolKindZAIWebSearch        = "zai_web_search"
	ServerToolKindGoogleSearch        = "google_search"
)

// ServerTool is a provider-hosted tool declaration. Name is the neutral feature
// requested by harness, while Kind and Parameters are filled by the model proxy
// after resolving the concrete target/provider wire format.
type ServerTool struct {
	Name       string          `json:"name"`
	Kind       string          `json:"kind,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// EventKind discriminates the StreamEvent union.
type EventKind int

const (
	EventTextDelta        EventKind = iota // incremental assistant text
	EventToolCallStart                     // tool_use began: ID + Name known
	EventToolCallDelta                     // partial JSON args (rendering only)
	EventToolCallDone                      // one call fully assembled
	EventUsage                             // usage snapshot (may arrive >1x)
	EventDone                              // turn end: StopReason + final Usage
	EventReasoningSummary                  // display-ready provider-visible reasoning summary text
	EventAssistantPhase                    // assistant message phase metadata
	EventInteractionStep                   // hidden complete Gemini server-managed step for stateless replay
	EventModelRequest                      // diagnostics-only request lifecycle metadata; never model content
)

// ModelRequestState identifies one out-of-band model request lifecycle event.
// These events are for rendering, logging, and session analysis only. They must
// never be converted into transcript messages or sent back to a model.
type ModelRequestState string

const (
	ModelRequestAccepted              ModelRequestState = "accepted"
	ModelRequestUpstreamAttemptFailed ModelRequestState = "upstream_attempt_failed"
	ModelRequestRetryScheduled        ModelRequestState = "retry_scheduled"
	ModelRequestCompleted             ModelRequestState = "completed"
	ModelRequestFailed                ModelRequestState = "failed"
	ModelRequestCancelled             ModelRequestState = "cancelled"
)

// ModelRequestOutcome describes what follows a failed upstream attempt.
type ModelRequestOutcome string

const (
	ModelRequestOutcomeRetrying ModelRequestOutcome = "retrying"
	ModelRequestOutcomeTerminal ModelRequestOutcome = "terminal"
)

// ModelRequestEvent is structured, provider-neutral telemetry for one model
// request. Message is the already parsed/redacted API error message.
// ResponsePayload is a bounded, redacted upstream response fragment; no request
// body, credentials, or unredacted model output belongs in this structure.
type ModelRequestEvent struct {
	State             ModelRequestState   `json:"state"`
	Outcome           ModelRequestOutcome `json:"outcome,omitempty"`
	Sequence          int                 `json:"sequence,omitempty"`
	ProxyInstanceID   string              `json:"proxy_instance_id,omitempty"`
	ProxyRequestID    uint64              `json:"proxy_request_id,omitempty"`
	UpstreamRequestID string              `json:"upstream_request_id,omitempty"`
	TraceID           string              `json:"trace_id,omitempty"`
	SpanID            string              `json:"span_id,omitempty"`
	TargetID          string              `json:"target_id,omitempty"`
	Provider          string              `json:"provider,omitempty"`
	APIType           string              `json:"api_type,omitempty"`
	Model             string              `json:"model,omitempty"`
	Purpose           RequestPurpose      `json:"purpose,omitempty"`
	Stage             APIErrorStage       `json:"stage,omitempty"`
	Attempt           int                 `json:"attempt,omitempty"`
	MaxAttempts       int                 `json:"max_attempts,omitempty"`
	StatusCode        int                 `json:"status_code,omitempty"`
	Code              string              `json:"code,omitempty"`
	Message           string              `json:"message,omitempty"`
	ResponsePayload   DiagnosticPayload   `json:"response_payload,omitempty"`
	Retryable         bool                `json:"retryable,omitempty"`
	RetryAfterMS      int64               `json:"retry_after_ms,omitempty"`
	RetryDelayMS      int64               `json:"retry_delay_ms,omitempty"`
	AttemptDurationMS int64               `json:"attempt_duration_ms,omitempty"`
	ElapsedMS         int64               `json:"elapsed_ms,omitempty"`
}

// ReasoningFormat identifies provider-owned reasoning state carried by a
// summary event. It prevents one dialect's signed state from being replayed
// into an incompatible provider after a model switch.
type ReasoningFormat string

const (
	ReasoningFormatAnthropic          ReasoningFormat = "anthropic_thinking"
	ReasoningFormatOpenAIResponses    ReasoningFormat = "openai_responses"
	ReasoningFormatGeminiInteractions ReasoningFormat = "gemini_interactions"
	// ReasoningFormatOpenAIChat marks chat-completions reasoning_content that
	// carries no provider signature. It is emitted only when the provider opted
	// into reasoning replay, and is persisted as an unsigned thinking block so
	// the next request can replay it as reasoning_content.
	ReasoningFormatOpenAIChat ReasoningFormat = "openai_chat"
)

// StreamEvent is one event in a provider stream. Which fields are populated
// depends on Kind.
type StreamEvent struct {
	Kind EventKind `json:"kind"`

	Text  string `json:"text,omitempty"` // EventTextDelta / EventReasoningSummary
	Phase string `json:"phase,omitempty"`

	// Signature carries an EventReasoningSummary's thinking-block signature, used
	// to persist and replay signed reasoning verbatim on the next turn. Empty for
	// providers/models that don't return a signature. For an EventReasoningSummary,
	// Text is the verbatim thinking text (the display layer trims it); RedactedData
	// carries an opaque redacted-thinking payload instead of Text.
	Signature       string          `json:"signature,omitempty"`
	RedactedData    string          `json:"redacted_data,omitempty"`
	ReasoningFormat ReasoningFormat `json:"reasoning_format,omitempty"`

	// ReasoningID / ReasoningEncrypted carry a Responses reasoning item's id and
	// opaque encrypted_content on an EventReasoningSummary. They are set (with an
	// empty Text, so nothing displays) when stateless reasoning replay is enabled,
	// and persisted as a BlockReasoning to round-trip on the next turn.
	ReasoningID        string `json:"reasoning_id,omitempty"`
	ReasoningEncrypted string `json:"reasoning_encrypted,omitempty"`

	// InteractionStep carries a complete provider-managed Gemini Interactions
	// step (currently Google Search call/result) for invisible persistence and
	// exact stateless replay.
	InteractionStep json.RawMessage `json:"interaction_step,omitempty"`

	// EventToolCall*; Index disambiguates parallel calls within one turn.
	Index     int             `json:"index,omitempty"`
	ToolID    string          `json:"tool_id,omitempty"`    // Start/Done
	ToolName  string          `json:"tool_name,omitempty"`  // Start/Done
	ArgsDelta string          `json:"args_delta,omitempty"` // Delta
	ToolInput json.RawMessage `json:"tool_input,omitempty"` // Done only: complete JSON object
	// InvalidInputError is set on EventToolCallDone when the provider streamed
	// malformed tool-call JSON. ToolInput still contains a valid diagnostic
	// object so the transcript can feed an error result back to the model.
	InvalidInputError string `json:"invalid_input_error,omitempty"`

	Usage      *Usage     `json:"usage,omitempty"`       // EventUsage / EventDone
	StopReason StopReason `json:"stop_reason,omitempty"` // EventDone
	ResponseID string     `json:"response_id,omitempty"` // EventDone, provider continuation id
	// ResponseIDAnchor is meaningful only on EventDone. A nil value means the
	// response ID must not be installed as an out-of-band prewarm anchor.
	ResponseIDAnchor *int `json:"response_id_anchor,omitempty"`

	// ModelRequest carries EventModelRequest telemetry. It is intentionally
	// separate from every content-bearing field above.
	ModelRequest *ModelRequestEvent `json:"model_request,omitempty"`
}

// StopReason is the normalized reason a turn ended.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopStop      StopReason = "stop" // stop sequence matched
)

// Usage is the normalized token accounting for a model call. After
// normalization InputTokens means the same thing across dialects: uncached
// input billed at full rate (see design §6).
type Usage struct {
	// InputTokens is uncached input, billed at full rate. Dialects must
	// normalize provider usage into this contract: endpoints that report
	// input INCLUDING cached tokens subtract the cache buckets (see the
	// usage_input_includes_cache provider quirk) so session accounting and
	// pricing never double-count cache reads.
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	// CacheWriteTokens is the default-rate cache-write bucket. Anthropic's
	// default is the 5-minute TTL; longer 1-hour writes are separate so they can
	// be priced at their documented rate.
	CacheWriteTokens   int `json:"cache_write_tokens"`
	CacheWrite1hTokens int `json:"cache_write_1h_tokens"`
	// CacheWriteTTLKnown is internal pricing metadata. It is intentionally not
	// serialized across the harness/proxy protocol.
	CacheWriteTTLKnown bool    `json:"-"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	CostUSD            float64 `json:"cost_usd,omitempty"`
	CostKnown          bool    `json:"cost_known,omitempty"`
	// ServiceTier and Speed report the tier actually served when the provider
	// exposes it. They let the proxy choose standard versus mode-specific rates
	// after graceful downgrades.
	ServiceTier string `json:"service_tier,omitempty"`
	Speed       string `json:"speed,omitempty"`
}
