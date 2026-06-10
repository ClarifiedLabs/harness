package llm

import "fmt"

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
	// open maps a tool_use ID awaiting a result to true. It is populated by an
	// assistant message and drained by the following user message.
	open := map[string]bool{}

	for i, m := range msgs {
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
