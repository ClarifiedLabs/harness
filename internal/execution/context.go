package execution

import "harness/internal/llm"

// ContextComposition is a numeric snapshot of caller-visible request content,
// before dialect serialization. It excludes implicit server-held context and
// retains no content, tool names, schemas, identifiers, or image data. The system
// prompt is measured separately and is never treated as a history message.
type ContextComposition struct {
	Messages, Blocks                                             int
	SystemTextBytes, UserTextBytes, AssistantTextBytes           int
	ToolInputBytes, ToolResultBytes, ToolSchemaBytes             int
	ReasoningTextBytes, ReasoningOpaqueBytes, ProviderStateBytes int
	ImageEncodedBytes                                            int
}

// ComposeRequest counts content payload bytes without serializing, decoding, or
// retaining any payload. It does not estimate tokens or dialect-added framing.
// SystemTextBytes includes request-only instruction strings, without treating
// them as history messages. ToolSchemaBytes includes supplied local, deferred,
// and server-tool declarations before a dialect chooses their representation.
// Routing/correlation IDs and transcript-only metadata are deliberately omitted.
func ComposeRequest(req llm.Request) ContextComposition {
	c := ContextComposition{Messages: len(req.Messages), SystemTextBytes: len(req.System)}
	for _, text := range req.RequestContext {
		c.SystemTextBytes += len(text)
	}
	for _, tool := range req.Tools {
		c.ToolSchemaBytes += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	for _, group := range req.DeferredToolGroups {
		c.ToolSchemaBytes += len(group.Name) + len(group.Description)
		for _, tool := range group.Tools {
			c.ToolSchemaBytes += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
		}
	}
	for _, tool := range req.ServerTools {
		c.ToolSchemaBytes += len(tool.Name) + len(tool.Kind) + len(tool.Parameters)
	}
	for _, message := range req.Messages {
		c.addBlocks(message.Content, message.Role, false)
	}
	return c
}

func (c *ContextComposition) addBlocks(blocks []llm.ContentBlock, role llm.Role, result bool) {
	c.Blocks += len(blocks)
	for i := range blocks {
		block := &blocks[i]
		switch block.Kind {
		case llm.BlockText:
			if result {
				c.ToolResultBytes += len(block.Text)
			} else if role == llm.RoleAssistant {
				c.AssistantTextBytes += len(block.Text)
			} else if role == llm.RoleUser {
				c.UserTextBytes += len(block.Text)
			}
		case llm.BlockImage:
			if block.ImageData != "" {
				c.ImageEncodedBytes += len(block.ImageData)
			} else {
				// Metadata-only images can occur in caller snapshots. Never infer
				// encoded length from decoded bytes or image dimensions.
				c.ImageEncodedBytes += max(0, block.ImageEncodedBytes)
			}
		case llm.BlockToolUse:
			c.ToolInputBytes += len(block.ToolInput)
		case llm.BlockToolResult:
			c.ToolResultBytes += len(block.ResultText)
			c.addBlocks(block.ResultContent, role, true)
		case llm.BlockThinking:
			c.ReasoningTextBytes += len(block.Thinking)
			c.ReasoningOpaqueBytes += len(block.ThinkingSignature)
		case llm.BlockRedactedThinking:
			c.ReasoningOpaqueBytes += len(block.RedactedData)
		case llm.BlockReasoning:
			c.ReasoningOpaqueBytes += len(block.ReasoningEncrypted)
		case llm.BlockInteractionThought:
			c.ReasoningTextBytes += len(block.InteractionThoughtSummary)
			c.ReasoningOpaqueBytes += len(block.InteractionThoughtSignature)
		case llm.BlockInteractionStep:
			c.ProviderStateBytes += len(block.InteractionStep)
		case llm.BlockResponsesToolSearch:
			c.ProviderStateBytes += len(block.ResponsesToolSearch)
		case llm.BlockAnthropicToolSearch:
			c.ProviderStateBytes += len(block.AnthropicToolSearch)
		case llm.BlockProviderCompaction:
			for _, item := range block.ProviderCompaction {
				c.ProviderStateBytes += len(item)
			}
		}
	}
}
