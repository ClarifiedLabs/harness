package llm

import (
	"context"
	"encoding/json"
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

	StoreResponse      bool     `json:"store_response,omitempty"`
	PreviousResponseID string   `json:"previous_response_id,omitempty"`
	RequestContext     []string `json:"request_context,omitempty"`
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

	// EventToolCall*; Index disambiguates parallel calls within one turn.
	Index     int             `json:"index,omitempty"`
	ToolID    string          `json:"tool_id,omitempty"`    // Start/Done
	ToolName  string          `json:"tool_name,omitempty"`  // Start/Done
	ArgsDelta string          `json:"args_delta,omitempty"` // Delta
	ToolInput json.RawMessage `json:"tool_input,omitempty"` // Done only: complete, valid JSON

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
	InputTokens      int `json:"input_tokens"` // uncached input, billed at full rate
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}
