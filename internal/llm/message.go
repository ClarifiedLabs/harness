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

// Message is one turn-fragment in a transcript: a role plus an ordered list of
// content blocks.
type Message struct {
	Role    Role           `json:"role"`
	Time    time.Time      `json:"time,omitempty"`
	Phase   string         `json:"phase,omitempty"`
	Content []ContentBlock `json:"content"`
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
)

// ContentBlock is a tagged union; exactly the fields for Kind are set.
type ContentBlock struct {
	Kind BlockKind `json:"kind"`

	// BlockText
	Text string `json:"text,omitempty"`

	// BlockImage (user-provided visual input)
	ImageMediaType string `json:"image_media_type,omitempty"`
	ImageData      string `json:"image_data,omitempty"` // base64, without data: prefix
	ImageDetail    string `json:"image_detail,omitempty"`
	ImageName      string `json:"image_name,omitempty"`
	ImageWidth     int    `json:"image_width,omitempty"`
	ImageHeight    int    `json:"image_height,omitempty"`

	// BlockToolUse (assistant calls a tool)
	ToolUseID string          `json:"tool_use_id,omitempty"` // provider-issued call id
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"` // complete JSON object

	// BlockToolResult (we answer a tool call)
	ResultForID string `json:"result_for_id,omitempty"` // matches a ToolUseID
	ResultText  string `json:"result_text,omitempty"`
	ResultError bool   `json:"result_error,omitempty"`

	// BlockThinking (assistant extended-thinking; replayed verbatim on the same
	// model). ThinkingSignature is the integrity signature the API requires to be
	// echoed back unchanged.
	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinking_signature,omitempty"`

	// BlockRedactedThinking (opaque model-internal payload, echoed back verbatim).
	RedactedData string `json:"redacted_data,omitempty"`
}

// ToolCall is a flat view of a BlockToolUse, carried from the agent loop into
// the tool layer.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is a flat view that becomes a BlockToolResult, carried from the
// tool layer back into the agent loop.
type ToolResult struct {
	ForID         string
	Text          string
	IsError       bool
	Truncated     bool
	OriginalText  string
	OriginalBytes int
	ShownBytes    int
	Usage         Usage
}
