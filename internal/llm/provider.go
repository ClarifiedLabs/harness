package llm

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
)

// Provider runs one model call as a stream of events. Concrete implementations
// live in internal/llm/anthropic and internal/llm/openai.
type Provider interface {
	Name() string // "openai" | "responses" | "anthropic"

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

// InputTokenCount is a provider-specific input-token count for one request.
type InputTokenCount struct {
	InputTokens int    `json:"input_tokens"`
	Source      string `json:"source,omitempty"`
}

// Request is one model call's worth of input, provider-neutral.
type Request struct {
	Model       string          `json:"model"`
	System      string          `json:"system,omitempty"`
	Messages    []Message       `json:"messages,omitempty"`
	Tools       []ToolSchema    `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`  // 0 = provider policy (see design §5.4)
	Temperature *float64        `json:"temperature,omitempty"` // nil = omit
	Reasoning   ReasoningConfig `json:"reasoning,omitempty"`
	StopSeqs    []string        `json:"stop_seqs,omitempty"`

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

	// PromptCacheKey is a stable per-session routing hint emitted as
	// prompt_cache_key on OpenAI/Responses so a session's requests, which share a
	// large system+tools prefix, keep landing on the same cache backend without
	// sharing provider-side continuation state with independent sessions. Empty =
	// omitted; ignored by providers that don't support it.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`

	// LongCacheTTL requests the 1-hour Anthropic prompt-cache breakpoint on the
	// stable system+tools anchors (worth its 2x write cost only across the
	// multi-minute pauses of an interactive REPL). One-shot, delegate, and other
	// non-interactive runs leave it false to take the cheaper 5-minute breakpoint.
	// Ignored by non-Anthropic dialects.
	LongCacheTTL bool `json:"long_cache_ttl,omitempty"`
}

// ResponseState is the resumable continuation state for Responses API
// previous_response_id chaining.
type ResponseState struct {
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	AnchorMessages     int    `json:"anchor_messages,omitempty"`
}

// ToolSchema is the model-facing declaration of one tool. Parameters is the raw
// JSON Schema object owned by the tool layer; it is passed through unchanged.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema object, owned by the tool layer
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
	Signature    string `json:"signature,omitempty"`
	RedactedData string `json:"redacted_data,omitempty"`

	// ReasoningID / ReasoningEncrypted carry a Responses reasoning item's id and
	// opaque encrypted_content on an EventReasoningSummary. They are set (with an
	// empty Text, so nothing displays) when stateless reasoning replay is enabled,
	// and persisted as a BlockReasoning to round-trip on the next turn.
	ReasoningID        string `json:"reasoning_id,omitempty"`
	ReasoningEncrypted string `json:"reasoning_encrypted,omitempty"`

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
	ResponseID string     `json:"response_id,omitempty"` // EventDone, Responses API
}

// StopReason is the normalized reason a model turn ended.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopStop      StopReason = "stop" // stop sequence matched
)

// Usage is the normalized token accounting for a model call. After
// normalization InputTokens means the same thing on both dialects: uncached
// input billed at full rate (see design §6).
type Usage struct {
	InputTokens      int     `json:"input_tokens"` // uncached input, billed at full rate
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	CostKnown        bool    `json:"cost_known,omitempty"`
}
