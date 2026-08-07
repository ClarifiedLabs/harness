// Package llm defines the provider-agnostic message model, the Provider
// streaming contract, the transcript invariant, and the model/price registry
// shared by the agent loop and the concrete provider dialects.
package llm

import (
	"encoding/json"
	"time"
)

// Role identifies the author of a message. The internal model is
// Anthropic-shaped: there is deliberately no tool role (tool results are
// content blocks on a user message) and no system role (the system prompt is a
// Request field, not a message).
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// No tool role: tool results are content blocks on a user message.
	// No system role: the system prompt is a Request field, not a message.
)

const (
	AssistantPhaseCommentary = "commentary"
	AssistantPhaseFinal      = "final_answer"
)

// ParallelToolBatch records one group of tool calls that the agent dispatched
// concurrently. ToolUseIDs stay in the model's emission order.
type ParallelToolBatch struct {
	ToolUseIDs []string `json:"tool_use_ids"`
}

// MessageOrigin records transcript-only provenance used to preserve prompt and
// turn boundaries across compaction and resume. Provider adapters intentionally
// ignore it.
type MessageOrigin string

const (
	MessageOriginPrompt               MessageOrigin = "prompt"
	MessageOriginSteer                MessageOrigin = "steer"
	MessageOriginInternal             MessageOrigin = "internal"
	MessageOriginCompactionCheckpoint MessageOrigin = "compaction_checkpoint"
)

// CompactionMetadata is transcript-only state carried by a synthetic
// compaction checkpoint. Provider adapters intentionally ignore it: it lets a
// later compaction update the exact prior summary without parsing the
// rendered checkpoint text, and preserves deterministic compacted-history file
// activity alongside the summary prose and its provenance.
type CompactionMetadata struct {
	Summary        string   `json:"summary"`
	SummarySource  string   `json:"summary_source,omitempty"`
	FallbackReason string   `json:"fallback_reason,omitempty"`
	Focus          string   `json:"focus,omitempty"`
	ReadFiles      []string `json:"read_files,omitempty"`
	ModifiedFiles  []string `json:"modified_files,omitempty"`
}

// Message is one turn-fragment in a transcript: a role plus an ordered list of
// content blocks. ParallelToolBatches is execution metadata set only on the user
// message carrying the corresponding tool results; provider adapters ignore it.
type Message struct {
	Role                Role                `json:"role"`
	Time                time.Time           `json:"time,omitempty"`
	Phase               string              `json:"phase,omitempty"`
	Origin              MessageOrigin       `json:"origin,omitempty"`
	Content             []ContentBlock      `json:"content"`
	ParallelToolBatches []ParallelToolBatch `json:"parallel_tool_batches,omitempty"`
	Compaction          *CompactionMetadata `json:"compaction,omitempty"`
}

func ValidAssistantPhase(phase string) bool {
	switch phase {
	case "", AssistantPhaseCommentary, AssistantPhaseFinal:
		return true
	default:
		return false
	}
}

// BlockKind tags a ContentBlock. Exactly the fields documented for the kind are
// set on any given block.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockImage      BlockKind = "image"
	BlockToolUse    BlockKind = "tool_use"
	BlockToolResult BlockKind = "tool_result"
	// BlockThinking carries an assistant extended-thinking block. It is replayed
	// verbatim to the same model on subsequent turns so signed reasoning
	// round-trips (Anthropic requires the signature to be echoed back unchanged).
	// Providers that don't model thinking (OpenAI/Responses) skip these blocks.
	BlockThinking BlockKind = "thinking"
	// BlockRedactedThinking carries an opaque, model-internal thinking payload
	// that must be echoed back verbatim but is never rendered.
	BlockRedactedThinking BlockKind = "redacted_thinking"
	// BlockReasoning carries an OpenAI Responses reasoning item captured in the
	// default stateless (store=false) mode via include=[reasoning.encrypted_content].
	// Its opaque encrypted_content is replayed verbatim as a reasoning input item
	// before the tool call it preceded, so a reasoning model does not re-reason
	// from scratch on every tool turn. Providers that don't model Responses
	// reasoning (Anthropic/OpenAI-chat) skip these blocks.
	BlockReasoning BlockKind = "reasoning"
	// BlockInteractionThought carries a Gemini Interactions thought summary and
	// signature. It is deliberately distinct from Anthropic thinking so signed
	// state cannot cross dialects.
	BlockInteractionThought BlockKind = "interaction_thought"
	// BlockInteractionStep carries a complete provider-managed Interactions
	// step needed for stateless replay. It is never rendered or dispatched.
	BlockInteractionStep BlockKind = "interaction_step"
)

// ContentBlock is a tagged union; exactly the fields for Kind are set.
type ContentBlock struct {
	Kind BlockKind `json:"kind"`

	// Provider-owned opaque reasoning replay. The domain records which configured
	// model targets may receive this block again; it is harness metadata and is
	// never forwarded upstream.
	ReasoningReplayDomain string `json:"reasoning_replay_domain,omitempty"`

	// BlockText
	Text string `json:"text,omitempty"`

	// BlockImage (user-provided visual input)
	ImageMediaType    string `json:"image_media_type,omitempty"`
	ImageData         string `json:"image_data,omitempty"` // base64, without data: prefix
	ImageDetail       string `json:"image_detail,omitempty"`
	ImageName         string `json:"image_name,omitempty"`
	ImageWidth        int    `json:"image_width,omitempty"`
	ImageHeight       int    `json:"image_height,omitempty"`
	ImageBytes        int    `json:"image_bytes,omitempty"`
	ImageEncodedBytes int    `json:"image_encoded_bytes,omitempty"`

	// BlockToolUse (assistant calls a tool). ToolName is also retained on the
	// matching BlockToolResult for dialects that require it in the result.
	ToolUseID string          `json:"tool_use_id,omitempty"` // provider-issued call id
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"` // complete JSON object

	// BlockToolResult (we answer a tool call). ResultContent is shallow,
	// supplementary model-visible content; it is currently restricted to images.
	ResultForID   string         `json:"result_for_id,omitempty"` // matches a ToolUseID
	ResultText    string         `json:"result_text,omitempty"`
	ResultError   bool           `json:"result_error,omitempty"`
	ResultContent []ContentBlock `json:"result_content,omitempty"`

	// BlockThinking (assistant extended-thinking; replayed verbatim on the same
	// model). ThinkingSignature is the integrity signature the API requires to be
	// echoed back unchanged.
	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinking_signature,omitempty"`

	// BlockRedactedThinking (opaque model-internal payload, echoed back verbatim).
	RedactedData string `json:"redacted_data,omitempty"`

	// BlockReasoning (Responses encrypted reasoning item, replayed verbatim in
	// stateless mode). ReasoningEncrypted is the opaque encrypted_content the API
	// returns only when store=false and include carries reasoning.encrypted_content;
	// ReasoningID is the item id (rs_…) echoed back alongside it.
	ReasoningID        string `json:"reasoning_id,omitempty"`
	ReasoningEncrypted string `json:"reasoning_encrypted,omitempty"`

	// BlockInteractionThought
	InteractionThoughtSummary   string `json:"interaction_thought_summary,omitempty"`
	InteractionThoughtSignature string `json:"interaction_thought_signature,omitempty"`

	// BlockInteractionStep
	InteractionStep json.RawMessage `json:"interaction_step,omitempty"`
}

// ToolCall is a flat view of a BlockToolUse, carried from the agent loop into
// the tool layer.
type ToolCall struct {
	ID                string
	Name              string
	Input             json.RawMessage
	InvalidInputError string
}

// ToolErrorKind is a stable, provider-neutral class for a failed tool result.
// It is diagnostics-only metadata: never sent to providers or hooks.
type ToolErrorKind string

const (
	ToolErrorUnknownTool          ToolErrorKind = "unknown_tool"
	ToolErrorInvalidArgs          ToolErrorKind = "invalid_args"
	ToolErrorTimeout              ToolErrorKind = "timeout"
	ToolErrorCancelled            ToolErrorKind = "cancelled"
	ToolErrorPanic                ToolErrorKind = "panic"
	ToolErrorPathNotFound         ToolErrorKind = "path_not_found"
	ToolErrorEditOldTextNotFound  ToolErrorKind = "edit_oldtext_not_found"
	ToolErrorEditOldTextAmbiguous ToolErrorKind = "edit_oldtext_ambiguous"
	ToolErrorHookBlocked          ToolErrorKind = "hook_blocked"
	ToolErrorBlocked              ToolErrorKind = "blocked"
	ToolErrorUnsupportedModality  ToolErrorKind = "unsupported_modality"
	ToolErrorInvalidResult        ToolErrorKind = "invalid_result"
	ToolErrorRegexInvalid         ToolErrorKind = "regex_invalid"
	ToolErrorBatchFailed          ToolErrorKind = "batch_failed"

	// Kinds assigned only by the offline analysis layer (text classification
	// of legacy logs and model_request failure mapping); producers never set
	// them at dispatch time.
	ToolErrorProviderInternalError ToolErrorKind = "provider_internal_error"
	ToolErrorProviderAuth          ToolErrorKind = "provider_auth"
	ToolErrorProviderRequest       ToolErrorKind = "provider_request"
	ToolErrorProvider5xx           ToolErrorKind = "provider_5xx"
	ToolErrorOther                 ToolErrorKind = "other"

	// Transient provider kinds: stamped at dispatch time by the delegate tool
	// (delegate.annotateRunError) and inferred by the offline analysis layer
	// for legacy logs and model_request failures.
	ToolErrorRateLimited        ToolErrorKind = "rate_limited"
	ToolErrorProviderOverloaded ToolErrorKind = "provider_overloaded"
	ToolErrorProviderError      ToolErrorKind = "provider_error"
)

// ToolResult is a flat view that becomes a BlockToolResult, carried from the
// tool layer back into the agent loop. When IsError is true, Text contains only
// the explanation: UI and provider adapters add any required error marker.
type ToolResult struct {
	ForID         string
	Text          string
	Content       []ContentBlock
	IsError       bool
	Truncated     bool
	OriginalText  string
	OriginalBytes int
	ShownBytes    int
	Usage         Usage
	// Metrics is diagnostics-only tool telemetry and is never copied into a
	// model-visible ContentBlock.
	Metrics map[string]int
	// ErrorKind is the diagnostics-only structured class of a failed result
	// (empty = unclassified; the analysis layer text-classifies). Like Metrics
	// it is never copied into a model-visible ContentBlock.
	ErrorKind ToolErrorKind
}
