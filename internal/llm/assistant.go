package llm

import "encoding/json"

// PersistedReasoningBlock converts a reasoning stream event into the neutral
// block that must be replayed on a subsequent request. Plain display-only
// summaries are intentionally not persisted.
func PersistedReasoningBlock(ev StreamEvent) (ContentBlock, bool) {
	if ev.ReasoningEncrypted != "" {
		return ContentBlock{Kind: BlockReasoning, ReasoningID: ev.ReasoningID, ReasoningEncrypted: ev.ReasoningEncrypted}, true
	}
	if ev.RedactedData != "" {
		return ContentBlock{Kind: BlockRedactedThinking, RedactedData: ev.RedactedData}, true
	}
	if ev.Signature == "" {
		return ContentBlock{}, false
	}
	if ev.ReasoningFormat == ReasoningFormatGeminiInteractions {
		return ContentBlock{
			Kind:                        BlockInteractionThought,
			InteractionThoughtSummary:   ev.Text,
			InteractionThoughtSignature: ev.Signature,
		}, true
	}
	return ContentBlock{Kind: BlockThinking, Thinking: ev.Text, ThinkingSignature: ev.Signature}, true
}

// PersistedInteractionStep converts a hidden Interactions step event into the
// neutral opaque block that sessions and stateless requests round-trip.
func PersistedInteractionStep(ev StreamEvent) (ContentBlock, bool) {
	if ev.Kind != EventInteractionStep || !validInteractionStep(ev.InteractionStep) {
		return ContentBlock{}, false
	}
	return ContentBlock{
		Kind:            BlockInteractionStep,
		InteractionStep: append(json.RawMessage(nil), ev.InteractionStep...),
	}, true
}

// ResolvedAssistantPhase applies an explicit valid streamed phase when present,
// otherwise deriving the transcript phase from the normalized stop reason.
func ResolvedAssistantPhase(explicit string, stop StopReason) string {
	if explicit != "" && ValidAssistantPhase(explicit) {
		return explicit
	}
	if stop == StopToolUse {
		return AssistantPhaseCommentary
	}
	return AssistantPhaseFinal
}

// BuildAssistantMessage creates the exact zero-time neutral assistant message
// represented by one completed provider response.
func BuildAssistantMessage(reasoning []ContentBlock, text string, calls []ToolCall, explicitPhase string, stop StopReason) Message {
	content := make([]ContentBlock, 0, len(reasoning)+1+len(calls))
	content = append(content, reasoning...)
	if text != "" {
		content = append(content, ContentBlock{Kind: BlockText, Text: text})
	}
	for _, call := range calls {
		content = append(content, ContentBlock{
			Kind:      BlockToolUse,
			ToolUseID: call.ID,
			ToolName:  call.Name,
			ToolInput: call.Input,
		})
	}
	return Message{
		Role:    RoleAssistant,
		Phase:   ResolvedAssistantPhase(explicitPhase, stop),
		Content: content,
	}
}
