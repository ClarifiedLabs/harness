package llm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ValidateMessageContent validates provider-neutral content without requiring
// messages to form a self-contained conversation. It is suitable for Responses
// continuation deltas, which may begin with a tool_result whose tool_use lives
// in the remote prefix.
func ValidateMessageContent(msgs []Message) error {
	for i, message := range msgs {
		openToolSearch := make(map[string]bool)
		seenToolSearch := make(map[string]bool)
		for j, block := range message.Content {
			switch block.Kind {
			case BlockImage:
				if message.Role != RoleUser {
					return fmt.Errorf("message %d block %d: image block is only valid in a user message", i, j)
				}
				if err := validateImageBlock(block); err != nil {
					return fmt.Errorf("message %d block %d: %w", i, j, err)
				}
			case BlockToolResult:
				if err := ValidateToolResultContent(block.ResultContent, block.ResultError); err != nil {
					return fmt.Errorf("message %d block %d: %w", i, j, err)
				}
			case BlockInteractionThought:
				if message.Role != RoleAssistant {
					return fmt.Errorf("message %d block %d: interaction thought is only valid in an assistant message", i, j)
				}
				if block.InteractionThoughtSignature == "" {
					return fmt.Errorf("message %d block %d: interaction thought signature is empty", i, j)
				}
			case BlockInteractionStep:
				if message.Role != RoleAssistant {
					return fmt.Errorf("message %d block %d: interaction step is only valid in an assistant message", i, j)
				}
				if !validInteractionStep(block.InteractionStep) {
					return fmt.Errorf("message %d block %d: invalid interaction step", i, j)
				}
			case BlockResponsesToolSearch:
				if message.Role != RoleAssistant {
					return fmt.Errorf("message %d block %d: Responses tool search is only valid in an assistant message", i, j)
				}
				if !validResponsesToolSearchItem(block.ResponsesToolSearch) {
					return fmt.Errorf("message %d block %d: invalid Responses tool search item", i, j)
				}
			case BlockAnthropicToolSearch:
				if message.Role != RoleAssistant {
					return fmt.Errorf("message %d block %d: Anthropic tool search is only valid in an assistant message", i, j)
				}
				info, ok := parseAnthropicToolSearchBlock(block.AnthropicToolSearch)
				if !ok {
					return fmt.Errorf("message %d block %d: invalid Anthropic tool search block", i, j)
				}
				if info.server {
					if seenToolSearch[info.id] {
						return fmt.Errorf("message %d block %d: duplicate Anthropic tool-search server id %q", i, j, info.id)
					}
					seenToolSearch[info.id] = true
					openToolSearch[info.id] = true
				} else {
					if !openToolSearch[info.id] {
						return fmt.Errorf("message %d block %d: Anthropic tool-search result %q does not match an open server call", i, j, info.id)
					}
					delete(openToolSearch, info.id)
				}
			case BlockProviderCompaction:
				if message.Role != RoleUser || message.Origin != MessageOriginProviderCompaction {
					return fmt.Errorf("message %d block %d: provider compaction requires a provider-compaction user message", i, j)
				}
				if len(message.Content) != 1 {
					return fmt.Errorf("message %d block %d: provider compaction must be the message's only block", i, j)
				}
				if block.ReasoningReplayDomain == "" {
					return fmt.Errorf("message %d block %d: provider compaction replay domain is empty", i, j)
				}
				if err := validateProviderCompactionItems(block.ProviderCompaction); err != nil {
					return fmt.Errorf("message %d block %d: %w", i, j, err)
				}
			}
		}
		if len(openToolSearch) > 0 {
			return fmt.Errorf("message %d: %d Anthropic tool-search server call(s) have no result", i, len(openToolSearch))
		}
	}
	return nil
}

// ValidateToolResultContent validates the deliberately narrow rich-result
// contract. Supplementary result content is shallow and image-only, and error
// results must remain text-only.
func ValidateToolResultContent(content []ContentBlock, isError bool) error {
	if len(content) > 0 && isError {
		return fmt.Errorf("error tool_result cannot carry supplementary content")
	}
	for i, child := range content {
		if child.Kind != BlockImage {
			return fmt.Errorf("tool_result child %d has kind %q; only image children are supported", i, child.Kind)
		}
		if err := validateImageBlock(child); err != nil {
			return fmt.Errorf("tool_result child %d: %w", i, err)
		}
	}
	return nil
}

func validateImageBlock(block ContentBlock) error {
	switch block.ImageMediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return fmt.Errorf("unsupported image media type")
	}
	switch block.ImageDetail {
	case "", "auto", "low", "high", "original":
	default:
		return fmt.Errorf("invalid image detail")
	}
	if block.ImageWidth < 0 || block.ImageHeight < 0 || block.ImageBytes < 0 || block.ImageEncodedBytes < 0 {
		return fmt.Errorf("image metadata cannot be negative")
	}
	if imageBlockHasForeignFields(block) {
		return fmt.Errorf("image block contains fields from another content kind")
	}
	if block.ImageData == "" {
		return fmt.Errorf("image data is empty")
	}

	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(block.ImageData))
	var prefix [12]byte
	prefixLen := 0
	buf := make([]byte, 32*1024)
	for {
		n, err := decoder.Read(buf)
		if n > 0 && prefixLen < len(prefix) {
			prefixLen += copy(prefix[prefixLen:], buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid image base64")
		}
		if n == 0 {
			return fmt.Errorf("invalid image base64")
		}
	}
	if !imageMagicMatches(block.ImageMediaType, prefix[:prefixLen]) {
		return fmt.Errorf("image media type does not match decoded data")
	}
	return nil
}

func imageBlockHasForeignFields(block ContentBlock) bool {
	return block.ReasoningReplayDomain != "" ||
		block.Text != "" ||
		block.ToolUseID != "" ||
		block.ToolName != "" ||
		block.ToolNamespace != "" ||
		len(block.ToolInput) > 0 ||
		block.ResultForID != "" ||
		block.ResultText != "" ||
		block.ResultError ||
		len(block.ResultContent) > 0 ||
		block.Thinking != "" ||
		block.ThinkingSignature != "" ||
		block.RedactedData != "" ||
		block.ReasoningID != "" ||
		block.ReasoningEncrypted != "" ||
		block.InteractionThoughtSummary != "" ||
		block.InteractionThoughtSignature != "" ||
		len(block.InteractionStep) > 0 ||
		len(block.ResponsesToolSearch) > 0 ||
		len(block.AnthropicToolSearch) > 0 ||
		len(block.ProviderCompaction) > 0
}

func validateProviderCompactionItems(items []json.RawMessage) error {
	if len(items) == 0 {
		return fmt.Errorf("provider compaction has no items")
	}
	foundCompaction := false
	for i, raw := range items {
		if len(raw) == 0 || !json.Valid(raw) {
			return fmt.Errorf("provider compaction item %d is invalid JSON", i)
		}
		var header struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.Type == "" {
			return fmt.Errorf("provider compaction item %d has no type", i)
		}
		if header.Type == "compaction" {
			if header.EncryptedContent == "" {
				return fmt.Errorf("provider compaction item %d is missing encrypted_content", i)
			}
			foundCompaction = true
		}
	}
	if !foundCompaction {
		return fmt.Errorf("provider compaction window has no compaction item")
	}
	return nil
}

func validInteractionStep(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return false
	}
	switch header.Type {
	case "google_search_call", "google_search_result":
		return true
	default:
		return false
	}
}

func validResponsesToolSearchItem(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var header struct {
		Type      string `json:"type"`
		Execution string `json:"execution"`
		Status    string `json:"status"`
	}
	if json.Unmarshal(raw, &header) != nil || header.Execution != "server" || header.Status != "completed" {
		return false
	}
	return header.Type == "tool_search_call" || header.Type == "tool_search_output"
}

type anthropicToolSearchBlockInfo struct {
	id     string
	server bool
}

func validAnthropicToolSearchBlock(raw json.RawMessage) bool {
	_, ok := parseAnthropicToolSearchBlock(raw)
	return ok
}

func parseAnthropicToolSearchBlock(raw json.RawMessage) (anthropicToolSearchBlockInfo, bool) {
	if len(raw) == 0 || !json.Valid(raw) {
		return anthropicToolSearchBlockInfo{}, false
	}
	var block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return anthropicToolSearchBlockInfo{}, false
	}
	switch block.Type {
	case "server_tool_use":
		if block.ID == "" || (block.Name != "tool_search_tool_bm25" && block.Name != "tool_search_tool_regex") || ValidateToolInputObject(block.Input) != nil {
			return anthropicToolSearchBlockInfo{}, false
		}
		return anthropicToolSearchBlockInfo{id: block.ID, server: true}, true
	case "tool_search_tool_result":
		if block.ToolUseID == "" || len(block.Content) == 0 {
			return anthropicToolSearchBlockInfo{}, false
		}
		var content struct {
			Type           string          `json:"type"`
			ToolReferences json.RawMessage `json:"tool_references"`
			ErrorCode      string          `json:"error_code"`
			ErrorMessage   string          `json:"error_message"`
		}
		if json.Unmarshal(block.Content, &content) != nil {
			return anthropicToolSearchBlockInfo{}, false
		}
		switch content.Type {
		case "tool_search_tool_search_result":
			if len(content.ToolReferences) == 0 || !strings.HasPrefix(strings.TrimSpace(string(content.ToolReferences)), "[") {
				return anthropicToolSearchBlockInfo{}, false
			}
			var references []struct {
				Type     string `json:"type"`
				ToolName string `json:"tool_name"`
			}
			if json.Unmarshal(content.ToolReferences, &references) != nil {
				return anthropicToolSearchBlockInfo{}, false
			}
			for _, reference := range references {
				if reference.Type != "tool_reference" || reference.ToolName == "" {
					return anthropicToolSearchBlockInfo{}, false
				}
			}
		case "tool_search_tool_result_error":
			switch content.ErrorCode {
			case "invalid_tool_input", "unavailable", "too_many_requests", "execution_time_exceeded":
			default:
				return anthropicToolSearchBlockInfo{}, false
			}
			if content.ErrorMessage == "" {
				return anthropicToolSearchBlockInfo{}, false
			}
		default:
			return anthropicToolSearchBlockInfo{}, false
		}
		return anthropicToolSearchBlockInfo{id: block.ToolUseID}, true
	default:
		return anthropicToolSearchBlockInfo{}, false
	}
}

func imageMagicMatches(mediaType string, data []byte) bool {
	switch mediaType {
	case "image/png":
		return len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/jpeg":
		return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
	case "image/gif":
		return len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a")
	case "image/webp":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	default:
		return false
	}
}

// ValidateTranscript enforces the transcript invariant: every assistant
// tool_use block has exactly one matching tool_result block in the immediately
// following user message, and no tool_result is orphaned. Both provider APIs
// hard-reject conversations that violate this, so the agent loop asserts the
// invariant after every operation that mutates a transcript.
//
// The walk tracks the set of open tool_use IDs (calls awaiting results). An
// assistant message may only be reached when that set is empty; it then opens a
// new set from its tool_use blocks. The next user message must close exactly
// that set with one matching tool_result each.
func ValidateTranscript(msgs []Message) error {
	if err := ValidateMessageContent(msgs); err != nil {
		return err
	}
	// open maps a tool_use ID awaiting a result to true. It is populated by an
	// assistant message and drained by the following user message.
	open := map[string]bool{}

	for i, m := range msgs {
		if m.Compaction != nil && (m.Role != RoleUser || m.Origin != MessageOriginCompactionCheckpoint) {
			return fmt.Errorf("message %d: compaction metadata requires a compaction-checkpoint user message", i)
		}
		switch m.Role {
		case RoleAssistant:
			if !ValidAssistantPhase(m.Phase) {
				return fmt.Errorf("message %d: invalid assistant phase %q", i, m.Phase)
			}
			// An assistant message may not appear while prior tool calls are
			// still open: those calls would never be answered.
			if len(open) > 0 {
				return fmt.Errorf("message %d: assistant message reached with %d unanswered tool_use call(s)", i, len(open))
			}
			for _, b := range m.Content {
				switch b.Kind {
				case BlockToolResult:
					return fmt.Errorf("message %d: tool_result block in an assistant message", i)
				case BlockToolUse:
					if b.ToolUseID == "" {
						return fmt.Errorf("message %d: tool_use block with empty id", i)
					}
					if err := ValidateToolInputObject(b.ToolInput); err != nil {
						return fmt.Errorf("message %d: tool_use %q has invalid input: %w", i, b.ToolUseID, err)
					}
					if open[b.ToolUseID] {
						return fmt.Errorf("message %d: duplicate tool_use id %q", i, b.ToolUseID)
					}
					open[b.ToolUseID] = true
				}
			}

		case RoleUser:
			if m.Phase != "" {
				return fmt.Errorf("message %d: phase is only valid on assistant messages", i)
			}
			// Collect the results in this user message and validate each
			// against the open set.
			for _, b := range m.Content {
				if b.Kind != BlockToolResult {
					continue
				}
				if b.ResultForID == "" {
					return fmt.Errorf("message %d: tool_result with empty result_for_id", i)
				}
				if err := ValidateToolResultContent(b.ResultContent, b.ResultError); err != nil {
					return fmt.Errorf("message %d: tool_result %q: %w", i, b.ResultForID, err)
				}
				if !open[b.ResultForID] {
					// Either no matching tool_use was issued (orphan), or this
					// id was already answered (two results for one call).
					return fmt.Errorf("message %d: tool_result %q does not match an open tool_use", i, b.ResultForID)
				}
				delete(open, b.ResultForID)
			}
			// After a user message, every previously open call must have been
			// answered. A user message that does not fully close the open set
			// (or that carries no results at all while calls are open) is
			// invalid.
			if len(open) > 0 {
				return fmt.Errorf("message %d: %d tool_use call(s) left unanswered by this user message", i, len(open))
			}

		default:
			return fmt.Errorf("message %d: unknown role %q", i, m.Role)
		}
	}

	// A trailing assistant message that issued tool calls leaves them dangling.
	if len(open) > 0 {
		return fmt.Errorf("transcript ends with %d unanswered tool_use call(s)", len(open))
	}
	return nil
}
