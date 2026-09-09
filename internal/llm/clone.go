package llm

import (
	"encoding/json"
	"slices"
)

// CloneMessages returns a deep copy of messages, preserving nil and empty slices.
func CloneMessages(messages []Message) []Message {
	out := slices.Clone(messages)
	for i := range out {
		out[i] = CloneMessage(messages[i])
	}
	return out
}

// CloneMessage returns a copy with independent message-owned content and metadata.
func CloneMessage(message Message) Message {
	message.Content = CloneContentBlocks(message.Content)
	message.ParallelToolBatches = slices.Clone(message.ParallelToolBatches)
	for i := range message.ParallelToolBatches {
		message.ParallelToolBatches[i].ToolUseIDs = slices.Clone(message.ParallelToolBatches[i].ToolUseIDs)
	}
	message.Compaction = CloneCompactionMetadata(message.Compaction)
	if message.ReasoningState != nil {
		state := *message.ReasoningState
		state.Baseline = cloneReasoningConfig(state.Baseline)
		state.Active = cloneReasoningConfig(state.Active)
		message.ReasoningState = &state
	}
	return message
}

func cloneReasoningConfig(config ReasoningConfig) ReasoningConfig {
	if config.Enabled != nil {
		enabled := *config.Enabled
		config.Enabled = &enabled
	}
	if config.BudgetTokens != nil {
		budget := *config.BudgetTokens
		config.BudgetTokens = &budget
	}
	return config
}

// CloneContentBlocks deeply copies content, including nested result content and
// opaque provider payloads. Nil and empty slices remain distinct.
func CloneContentBlocks(blocks []ContentBlock) []ContentBlock {
	out := slices.Clone(blocks)
	for i := range out {
		out[i].ResultContent = CloneContentBlocks(blocks[i].ResultContent)
		out[i].ToolInput = slices.Clone(blocks[i].ToolInput)
		out[i].InteractionStep = slices.Clone(blocks[i].InteractionStep)
		out[i].ResponsesToolSearch = slices.Clone(blocks[i].ResponsesToolSearch)
		out[i].AnthropicToolSearch = slices.Clone(blocks[i].AnthropicToolSearch)
		out[i].ProviderCompaction = CloneRawMessages(blocks[i].ProviderCompaction)
	}
	return out
}

// CloneRawMessages copies both the item slice and each opaque JSON payload,
// preserving nil and empty slices.
func CloneRawMessages(items []json.RawMessage) []json.RawMessage {
	out := slices.Clone(items)
	for i := range out {
		out[i] = slices.Clone(items[i])
	}
	return out
}

// CloneCompactionMetadata deeply copies checkpoint metadata, including preserved
// user instructions. A nil metadata pointer remains nil.
func CloneCompactionMetadata(meta *CompactionMetadata) *CompactionMetadata {
	if meta == nil {
		return nil
	}
	out := *meta
	out.UserInstructions = CloneContentBlocks(meta.UserInstructions)
	out.ReadFiles = slices.Clone(meta.ReadFiles)
	out.ModifiedFiles = slices.Clone(meta.ModifiedFiles)
	return &out
}
