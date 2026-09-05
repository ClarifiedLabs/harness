package acp

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const truncationMarker = "…"

// SanitizeModelFacingText removes terminal escape sequences and controls from
// untrusted ACP text and limits the result to MaxModelFacingTextBytes. Newlines
// and tabs are retained; CR and CRLF are normalized to newline. Truncation never
// splits UTF-8 and is marked with an ellipsis.
func SanitizeModelFacingText(text string) string {
	return sanitizeModelText(text, MaxModelFacingTextBytes)
}

// SanitizeModelFacingContent returns a copy with human-readable text fields
// cleaned and bounded. URIs are cleaned as well: they are displayable, and a
// well-formed URI contains no control characters, so cleaning never rewrites a
// valid one. Binary data, annotations, metadata, and Raw are not suitable for
// model input and are left untouched.
func SanitizeModelFacingContent(content ContentBlock) ContentBlock {
	content.Text = SanitizeModelFacingText(content.Text)
	content.Name = SanitizeModelFacingText(content.Name)
	content.Title = SanitizeModelFacingText(content.Title)
	content.Description = SanitizeModelFacingText(content.Description)
	content.URI = SanitizeModelFacingText(content.URI)
	if content.Resource != nil {
		resource := *content.Resource
		resource.URI = SanitizeModelFacingText(resource.URI)
		if resource.Text != nil {
			text := SanitizeModelFacingText(*resource.Text)
			resource.Text = &text
		}
		content.Resource = &resource
	}
	return content
}

// SanitizeModelFacingUpdate returns a detached copy of a known update with its
// human-readable content sanitized. Call Validate before forwarding it. Unknown
// update Raw bytes are deliberately preserved but must not be sent to a model.
func SanitizeModelFacingUpdate(update SessionUpdate) SessionUpdate {
	out := update
	if update.ContentChunk != nil {
		v := *update.ContentChunk
		v.Content = SanitizeModelFacingContent(v.Content)
		out.ContentChunk = &v
	}
	if update.ToolCall != nil {
		v := *update.ToolCall
		v.Title = SanitizeModelFacingText(v.Title)
		v.Content = sanitizeToolContent(v.Content)
		out.ToolCall = &v
	}
	if update.ToolCallUpdate != nil {
		v := *update.ToolCallUpdate
		if v.Title != nil {
			text := SanitizeModelFacingText(*v.Title)
			v.Title = &text
		}
		if v.Content != nil {
			content := sanitizeToolContent(*v.Content)
			v.Content = &content
		}
		out.ToolCallUpdate = &v
	}
	if update.Plan != nil {
		v := *update.Plan
		v.Entries = append([]PlanEntry(nil), v.Entries...)
		for i := range v.Entries {
			v.Entries[i].Content = SanitizeModelFacingText(v.Entries[i].Content)
		}
		out.Plan = &v
	}
	if update.AvailableCommands != nil {
		v := *update.AvailableCommands
		v.AvailableCommands = append([]AvailableCommand(nil), v.AvailableCommands...)
		for i := range v.AvailableCommands {
			v.AvailableCommands[i].Name = SanitizeModelFacingText(v.AvailableCommands[i].Name)
			v.AvailableCommands[i].Description = SanitizeModelFacingText(v.AvailableCommands[i].Description)
		}
		out.AvailableCommands = &v
	}
	if update.SessionInfo != nil {
		v := *update.SessionInfo
		if v.Title != nil {
			text := SanitizeModelFacingText(*v.Title)
			v.Title = &text
		}
		out.SessionInfo = &v
	}
	return out
}

func sanitizeToolContent(in []ToolCallContent) []ToolCallContent {
	out := append([]ToolCallContent(nil), in...)
	for i := range out {
		if out[i].Content != nil {
			content := SanitizeModelFacingContent(*out[i].Content)
			out[i].Content = &content
		}
		out[i].Path = SanitizeModelFacingText(out[i].Path)
		if out[i].OldText != nil {
			text := SanitizeModelFacingText(*out[i].OldText)
			out[i].OldText = &text
		}
		out[i].NewText = SanitizeModelFacingText(out[i].NewText)
	}
	return out
}

func sanitizeModelText(text string, limit int) string {
	if text == "" || limit <= 0 {
		return ""
	}
	inputTruncated := false
	if len(text) > MaxTextBytes {
		cut := MaxTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
		inputTruncated = true
	}
	var b strings.Builder
	if len(text) < limit {
		b.Grow(len(text))
	} else {
		b.Grow(limit)
	}
	truncated := inputTruncated
	for i := 0; i < len(text); {
		if text[i] == 0x1b {
			i = skipEscape(text, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			r = utf8.RuneError
		}
		if r == '\r' {
			if i+size < len(text) && text[i+size] == '\n' {
				i += size
			}
			r = '\n'
		}
		if r == '\u009b' {
			i = skipCSI(text, i+size)
			continue
		}
		if r == '\u009d' || r == '\u0090' || r == '\u0098' || r == '\u009e' || r == '\u009f' {
			i = skipControlString(text, i+size)
			continue
		}
		i += size
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.In(r, unicode.Cf) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > limit {
			truncated = true
			break
		}
		b.WriteRune(r)
	}
	if truncated {
		value := b.String()
		for len(value)+len(truncationMarker) > limit && len(value) > 0 {
			_, size := utf8.DecodeLastRuneInString(value)
			value = value[:len(value)-size]
		}
		return value + truncationMarker
	}
	return b.String()
}

func skipEscape(s string, esc int) int {
	if esc+1 >= len(s) {
		return len(s)
	}
	switch s[esc+1] {
	case '[':
		return skipCSI(s, esc+2)
	case ']', 'P', 'X', '^', '_':
		return skipControlString(s, esc+2)
	default:
		i := esc + 1
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) {
			return i + 1
		}
		return len(s)
	}
}

func skipCSI(s string, i int) int {
	for i < len(s) {
		b := s[i]
		switch {
		case b >= 0x40 && b <= 0x7e:
			return i + 1
		case b >= 0x20 && b <= 0x3f:
			i++
		default:
			// This is not a valid CSI sequence. Preserve subsequent text rather
			// than consuming it while still dropping the escape introducer.
			return i
		}
	}
	return len(s)
}

func skipControlString(s string, i int) int {
	for i < len(s) {
		if s[i] == 0x07 {
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\u009c' {
			return i + size
		}
		i += size
	}
	return len(s)
}

func sessionUpdateTextBytes(update SessionUpdate) int {
	total := 0
	addContent := func(c ContentBlock) {
		total += len(c.Text) + len(c.Name) + len(c.Title) + len(c.Description)
		if c.Resource != nil && c.Resource.Text != nil {
			total += len(*c.Resource.Text)
		}
	}
	addToolContent := func(content []ToolCallContent) {
		for _, c := range content {
			total += len(c.Path) + len(c.NewText)
			if c.OldText != nil {
				total += len(*c.OldText)
			}
			if c.Content != nil {
				addContent(*c.Content)
			}
		}
	}
	if update.ContentChunk != nil {
		addContent(update.ContentChunk.Content)
	}
	if update.ToolCall != nil {
		total += len(update.ToolCall.Title)
		addToolContent(update.ToolCall.Content)
	}
	if update.ToolCallUpdate != nil {
		if update.ToolCallUpdate.Title != nil {
			total += len(*update.ToolCallUpdate.Title)
		}
		if update.ToolCallUpdate.Content != nil {
			addToolContent(*update.ToolCallUpdate.Content)
		}
	}
	if update.Plan != nil {
		for _, e := range update.Plan.Entries {
			total += len(e.Content)
		}
	}
	if update.AvailableCommands != nil {
		for _, c := range update.AvailableCommands.AvailableCommands {
			total += len(c.Name) + len(c.Description)
		}
	}
	if update.SessionInfo != nil && update.SessionInfo.Title != nil {
		total += len(*update.SessionInfo.Title)
	}
	return total
}
