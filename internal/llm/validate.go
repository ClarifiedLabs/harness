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
			}
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
	return block.Text != "" ||
		block.ToolUseID != "" ||
		block.ToolName != "" ||
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
		len(block.InteractionStep) > 0
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
