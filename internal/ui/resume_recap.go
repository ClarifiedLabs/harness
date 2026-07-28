package ui

import (
	"fmt"
	"strings"

	"harness/internal/llm"
	"harness/internal/session"
)

// resumeRecapKind identifies how the resumed session ended.
type resumeRecapKind int

const (
	recapClean resumeRecapKind = iota
	recapInterruptedStream
	recapInterruptedTools
	recapUnansweredPrompt
	recapRecovered
)

// resumeRecap is the classified tail of a resumed session: the last human
// prompt, the last assistant text, and an optional trailer explaining how the
// session ended when it did not end cleanly at the prompt.
type resumeRecap struct {
	kind      resumeRecapKind
	prompt    string
	assistant string
	trailer   string
}

// buildResumeRecap derives the recap from the persisted transcript alone: the
// recovery marker, the final message's role/phase/origin, and the synthetic
// "interrupted" tool results left by save-time repair (design §4). A nil recap
// means there is nothing useful to show.
func buildResumeRecap(s *session.Session) *resumeRecap {
	if s == nil || len(s.Messages) == 0 {
		return nil
	}
	msgs := s.Messages
	recap := &resumeRecap{}

	// Walk backwards once to collect the most recent assistant text and the
	// most recent human prompt. Tool_use-only assistant messages carry nothing
	// user-visible; thinking blocks are never rendered.
	var haveAssistant, havePrompt bool
	for i := len(msgs) - 1; i >= 0 && !(haveAssistant && havePrompt); i-- {
		m := msgs[i]
		switch {
		case m.Role == llm.RoleAssistant && !haveAssistant:
			var text strings.Builder
			hasText := false
			for _, b := range m.Content {
				if b.Kind == llm.BlockText {
					text.WriteString(b.Text)
					hasText = true
				}
			}
			if hasText {
				recap.assistant = text.String()
				haveAssistant = true
			}
		case m.Role == llm.RoleUser && !havePrompt:
			if m.Origin != llm.MessageOriginPrompt && m.Origin != llm.MessageOriginSteer {
				continue
			}
			var text strings.Builder
			for _, b := range m.Content {
				if b.Kind == llm.BlockText {
					text.WriteString(b.Text)
				}
			}
			recap.prompt = text.String()
			havePrompt = true
		}
	}

	tail := msgs[len(msgs)-1]
	switch {
	case s.Recovery != nil:
		recap.kind = recapRecovered
		recap.trailer = "[session ended mid-turn; recovered from checkpoint — showing the last durable exchange]"
	case tail.Role == llm.RoleAssistant:
		switch tail.Phase {
		case llm.AssistantPhaseFinal, "":
			// Phase "" marks pre-phase sessions; never cry "interrupted" on
			// old data.
			recap.kind = recapClean
		case llm.AssistantPhaseCommentary:
			if !assistantTextOnly(tail) {
				return nil
			}
			recap.kind = recapInterruptedStream
			recap.trailer = "[turn interrupted mid-reply — the answer above is partial]"
		default:
			return nil
		}
	case tail.Role == llm.RoleUser:
		switch {
		case tail.Origin == llm.MessageOriginPrompt || tail.Origin == llm.MessageOriginSteer:
			recap.kind = recapUnansweredPrompt
			recap.trailer = "[turn interrupted before the model replied]"
		case tail.Origin == llm.MessageOriginCompactionCheckpoint:
			recap.kind = recapClean
			recap.trailer = "[history was compacted after this exchange]"
		case hasToolResults(tail):
			recap.kind = recapInterruptedTools
			if names := interruptedToolNames(msgs); len(names) > 0 {
				recap.trailer = fmt.Sprintf("[turn interrupted during tool execution: %s did not complete]", strings.Join(names, ", "))
			} else {
				recap.trailer = "[turn ended after tool execution, before the model replied]"
			}
		default:
			return nil
		}
	default:
		return nil
	}
	return recap
}

// PrintResumeRecap prints the classified recap to app.Errw. Stdout stays
// untouched so the one-shot contract (assistant text only on stdout) holds.
func PrintResumeRecap(app *App, s *session.Session) {
	recap := buildResumeRecap(s)
	if recap == nil {
		return
	}
	if recap.kind == recapClean && recap.prompt == "" && recap.assistant == "" {
		return
	}
	fmt.Fprintln(app.Errw, "--- resuming session: last exchange ---")
	if prompt := strings.Join(strings.Fields(recap.prompt), " "); prompt != "" {
		fmt.Fprintf(app.Errw, "> %s\n", prompt)
	}
	if recap.assistant != "" {
		fmt.Fprintln(app.Errw, renderRecapMarkdown(app, recap.assistant))
	}
	if recap.trailer != "" {
		fmt.Fprintln(app.Errw, recap.trailer)
	}
	fmt.Fprintln(app.Errw, "---")
}

// renderRecapMarkdown renders the assistant excerpt through the renderer when
// one is wired, so the recap follows the same markdown/ANSI/width policy as
// streamed text.
func renderRecapMarkdown(app *App, text string) string {
	if app == nil || app.Renderer == nil {
		return text
	}
	return app.Renderer.FormatMarkdown(text)
}

// assistantTextOnly reports whether the message is a text-only assistant
// fragment — the shape a mid-stream interrupt persists (design §4).
func assistantTextOnly(m llm.Message) bool {
	hasText := false
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockText:
			hasText = true
		case llm.BlockToolUse:
			return false
		}
	}
	return hasText
}

func hasToolResults(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Kind == llm.BlockToolResult {
			return true
		}
	}
	return false
}

// interruptedToolNames names the tool calls in the tail user message whose
// results are the synthetic "interrupted" marker, resolving each ResultForID
// through the preceding assistant tool_use blocks.
func interruptedToolNames(msgs []llm.Message) []string {
	tail := msgs[len(msgs)-1]
	toolNames := map[string]string{}
	for _, m := range msgs[:len(msgs)-1] {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolUse {
				toolNames[b.ToolUseID] = b.ToolName
			}
		}
	}
	var names []string
	for _, b := range tail.Content {
		if b.Kind != llm.BlockToolResult || !b.ResultError || b.ResultText != "interrupted" {
			continue
		}
		name := toolNames[b.ResultForID]
		if name == "" {
			name = b.ToolName
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
