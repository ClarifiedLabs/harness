package markdown

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"harness/internal/mermaid"
)

// Keep source line endings for the plain-code fallback and LineOpen contract.
type fenceLine struct {
	text    string
	newline bool
}

func (s *Stream) bufferMermaidLine(out *strings.Builder, line string, newline bool) {
	closed := strings.HasPrefix(strings.TrimSpace(line), s.fenceMarker)
	s.diagram = append(s.diagram, fenceLine{text: line, newline: newline})
	if !closed {
		s.diagramBytes += len(line) + 1
	}
	if closed || s.diagramBytes > mermaid.MaxSourceBytes {
		s.flushMermaid(out, closed)
	}
}

// Include incomplete source lines in the cap. Once in fallback, long lines can
// stream in fragments rather than retaining and repeatedly copying all of them.
func (s *Stream) flushMermaidPending(out *strings.Builder) {
	if (s.diagram == nil && !s.diagramFallback) || len(s.pending) <= mermaid.MaxSourceBytes-s.diagramBytes {
		return
	}
	if s.diagram != nil {
		s.diagramBytes = mermaid.MaxSourceBytes + 1
		s.flushMermaid(out, false)
	}
	end := len(s.pending)
	if end > 0 && s.pending[end-1] == '\r' {
		end-- // retain a possible CRLF ending
	}
	if end > 0 {
		start := end - 1
		for start > 0 && !utf8.RuneStart(s.pending[start]) {
			start--
		}
		if !utf8.FullRuneInString(s.pending[start:end]) {
			end = start
		}
	}
	if end == 0 {
		return
	}
	line := strings.ReplaceAll(s.pending[:end], "\t", "    ")
	s.pending = s.pending[end:]
	s.writeMermaidPartial(out, line, false)
}

func (s *Stream) writeMermaidPartial(out *strings.Builder, line string, newline bool) {
	line = diagramText(line)
	if line != "" || newline {
		if !s.lineOpen && line != "" {
			line = s.opts.Prefix + "  " + line
		}
		s.writeLine(out, line, newline)
	}
	s.diagramPartial = !newline
}

func (s *Stream) writeFenceLines(out *strings.Builder, lines []fenceLine) {
	for _, line := range lines {
		s.writeLine(out, s.opts.Prefix+"  "+diagramText(line.text), line.newline)
	}
}

func (s *Stream) flushMermaid(out *strings.Builder, closed bool) {
	if s.diagram == nil {
		return
	}
	lines, sourceBytes := s.diagram, s.diagramBytes
	s.diagram = nil
	s.diagramBytes = 0
	if closed {
		s.inFence = false
		s.fenceMarker = ""
	} else {
		// A notice can force a flush before the source fence closes. Keep
		// recognizing its closer; any later body lines are plain code, not
		// a second partial diagram or prose.
		s.diagramFallback = true
		s.diagramPartial = !lines[len(lines)-1].newline
	}
	s.code = nil

	if sourceBytes > mermaid.MaxSourceBytes {
		s.writeFenceLines(out, lines)
		return
	}
	body := lines[1:]
	if closed {
		body = body[:len(body)-1]
	}
	var source strings.Builder
	for _, line := range body {
		source.WriteString(diagramText(line.text))
		source.WriteByte('\n')
	}
	width := s.opts.Width
	if width <= 0 {
		width = DefaultWidth
	}
	width = max(1, width-diagramDisplayWidth(s.opts.Prefix))
	rendered, err := mermaid.Render(source.String(), mermaid.Options{Width: width})
	if err != nil {
		// Write directly rather than re-entering Mermaid fence detection.
		s.writeFenceLines(out, lines)
		return
	}
	// Emit adaptive art and list fallbacks directly: Markdown/prose wrapping
	// would corrupt the layout or interpret literal labels as markup.
	rows := strings.Split(strings.TrimSuffix(diagramText(rendered), "\n"), "\n")
	for i, row := range rows {
		if row != "" {
			row = s.opts.Prefix + row
		}
		s.writeLine(out, row, i+1 < len(rows) || lines[len(lines)-1].newline)
	}
}

// Use the diagram's column model (wide runes and combining marks), not the
// Markdown prose rune count. Prefix styling does not consume columns.
func diagramDisplayWidth(text string) int {
	width := 0
	for _, unit := range renderedUnits(text) {
		if unit.visible {
			width += mermaid.DisplayWidth(unit.text)
		}
	}
	return width
}

// Mermaid labels and front-matter titles are data, never terminal commands.
// Filter both source and rendered output so no-color diagrams and fallbacks
// cannot acquire terminal or bidi controls, including controls decoded from
// labels. Bidi overrides could reorder node IDs and arrows in list fallbacks.
func diagramText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Bidi_Control, r) || r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}
