// Package markdown renders a small, terminal-friendly subset of Markdown using
// only the standard library. It is intentionally not a CommonMark parser; it
// focuses on the model-output shapes that improve terminal readability.
package markdown

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ansiBold       = "\x1b[1m"
	ansiItalic     = "\x1b[3m"
	ansiLink       = "\x1b[36;4m"
	ansiCode       = "\x1b[33m"
	ansiReset      = "\x1b[0m"
	minTableRule   = 3
	codeFenceTick  = "```"
	codeFenceTilde = "~~~"
)

// Options controls Markdown rendering.
type Options struct {
	// Enabled leaves text byte-for-byte unchanged when false.
	Enabled bool
	// ANSI applies terminal styling for emphasis and links. When false, supported
	// Markdown markers are still normalized or stripped.
	ANSI bool
	// Width enables simple wrapping for list item bodies when positive.
	Width int
	// Prefix is prepended to each non-empty rendered line.
	Prefix string
}

// Render formats a complete text block.
func Render(text string, opts Options) string {
	if !opts.Enabled || text == "" {
		return text
	}
	stream := NewStream(opts)
	return stream.Write(text) + stream.Flush()
}

// Stream renders Markdown from incremental text deltas. Complete lines are
// emitted as soon as possible; tables are buffered until the table block ends so
// column widths can be calculated.
type Stream struct {
	opts        Options
	pending     string
	table       []tableLine
	inFence     bool
	fenceMarker string
	lineOpen    bool
}

// NewStream returns a new line-buffered renderer.
func NewStream(opts Options) *Stream {
	return &Stream{opts: opts}
}

// Write consumes a text delta and returns any display-ready text.
func (s *Stream) Write(text string) string {
	if text == "" {
		return ""
	}
	if !s.opts.Enabled {
		s.lineOpen = !strings.HasSuffix(text, "\n")
		return text
	}

	s.pending += text
	var out strings.Builder
	for {
		i := strings.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(s.pending[:i], "\r")
		s.pending = s.pending[i+1:]
		s.renderLine(&out, line, true)
	}
	return out.String()
}

// Flush renders any buffered incomplete line or pending table block.
func (s *Stream) Flush() string {
	if !s.opts.Enabled {
		return ""
	}
	var out strings.Builder
	if s.pending != "" {
		line := strings.TrimSuffix(s.pending, "\r")
		s.pending = ""
		s.renderLine(&out, line, false)
	}
	s.flushTable(&out)
	return out.String()
}

// LineOpen reports whether the last emitted display line lacks a trailing
// newline.
func (s *Stream) LineOpen() bool {
	return s.lineOpen
}

// CloseLine tells the stream that the caller wrote an external newline after an
// open rendered line.
func (s *Stream) CloseLine() {
	s.lineOpen = false
}

func (s *Stream) renderLine(out *strings.Builder, line string, newline bool) {
	line = strings.TrimRight(strings.ReplaceAll(line, "\t", "    "), " \r")

	if s.inFence {
		s.flushTable(out)
		s.writeLine(out, s.opts.Prefix+"  "+line, newline)
		if strings.HasPrefix(strings.TrimSpace(line), s.fenceMarker) {
			s.inFence = false
			s.fenceMarker = ""
		}
		return
	}

	if marker, ok := fenceMarker(line); ok {
		s.flushTable(out)
		s.inFence = true
		s.fenceMarker = marker
		s.writeLine(out, s.opts.Prefix+"  "+line, newline)
		return
	}

	if tableCandidate(line) {
		s.table = append(s.table, tableLine{text: line, newline: newline})
		return
	}

	s.flushTable(out)
	s.renderNonTableLine(out, line, newline)
}

func (s *Stream) renderNonTableLine(out *strings.Builder, line string, newline bool) {
	if strings.TrimSpace(line) == "" {
		s.writeLine(out, "", newline)
		return
	}
	if rendered, ok := s.renderHeading(line); ok {
		s.writeLine(out, rendered, newline)
		return
	}
	if rendered, ok := s.renderListItem(line, newline); ok {
		out.WriteString(rendered)
		s.lineOpen = !strings.HasSuffix(rendered, "\n")
		return
	}
	s.writeLine(out, s.opts.Prefix+s.renderInline(line), newline)
}

func (s *Stream) renderHeading(line string) (string, bool) {
	leading, rest := splitLeadingWhitespace(line)
	if len(leading) > 3 {
		return "", false
	}
	n := 0
	for n < len(rest) && n < 6 && rest[n] == '#' {
		n++
	}
	if n == 0 || n < len(rest) && rest[n] != ' ' && rest[n] != '\t' {
		return "", false
	}
	rendered := s.opts.Prefix + leading + s.renderInline(rest)
	if s.opts.ANSI {
		rendered = ansiBold + rendered + ansiReset
	}
	return rendered, true
}

func (s *Stream) renderListItem(line string, newline bool) (string, bool) {
	leading, rest := splitLeadingWhitespace(line)
	marker, body, ok := listMarker(rest)
	if !ok {
		return "", false
	}
	firstPrefix := s.opts.Prefix + leading + marker + " "
	nextPrefix := s.opts.Prefix + leading + strings.Repeat(" ", len(marker)+1)
	lines := wrapListBody(body, firstPrefix, nextPrefix, s.opts.Width, s.opts.ANSI)
	var out strings.Builder
	for i, rendered := range lines {
		if i == len(lines)-1 && !newline {
			out.WriteString(rendered)
			continue
		}
		out.WriteString(rendered)
		out.WriteByte('\n')
	}
	return out.String(), true
}

func (s *Stream) flushTable(out *strings.Builder) {
	if len(s.table) == 0 {
		return
	}
	lines := s.table
	s.table = nil
	if !validTable(lines) {
		for _, line := range lines {
			s.renderNonTableLine(out, line.text, line.newline)
		}
		return
	}
	for _, line := range s.formatTable(lines) {
		s.writeLine(out, line.text, line.newline)
	}
}

func (s *Stream) formatTable(lines []tableLine) []tableLine {
	aligns := parseAlignments(parseCells(lines[1].text))
	rawRows := [][]string{parseCells(lines[0].text)}
	for _, line := range lines[2:] {
		rawRows = append(rawRows, parseCells(line.text))
	}

	cols := len(aligns)
	for _, row := range rawRows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	for len(aligns) < cols {
		aligns = append(aligns, alignLeft)
	}

	rows := make([][]string, len(rawRows))
	widths := make([]int, cols)
	for i, row := range rawRows {
		rows[i] = make([]string, cols)
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(row) {
				cell = s.renderInline(strings.TrimSpace(row[c]))
			}
			rows[i][c] = cell
			if w := visibleLen(cell); w > widths[c] {
				widths[c] = w
			}
		}
	}
	for i := range widths {
		if widths[i] < minTableRule {
			widths[i] = minTableRule
		}
	}

	out := make([]tableLine, 0, len(lines))
	out = append(out, tableLine{text: formatTableRow(rows[0], widths, aligns), newline: lines[0].newline})
	out = append(out, tableLine{text: formatTableRule(widths, aligns), newline: lines[1].newline})
	for i := 1; i < len(rows); i++ {
		newline := true
		if i+1 < len(lines) {
			newline = lines[i+1].newline
		}
		out = append(out, tableLine{text: formatTableRow(rows[i], widths, aligns), newline: newline})
	}
	return out
}

func (s *Stream) writeLine(out *strings.Builder, line string, newline bool) {
	out.WriteString(line)
	if newline {
		out.WriteByte('\n')
		s.lineOpen = false
		return
	}
	s.lineOpen = line != ""
}

func (s *Stream) renderInline(text string) string {
	return renderInline(text, s.opts.ANSI)
}

func renderInline(text string, ansi bool) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '`' {
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				end += i + 1
				inner := text[i+1 : end]
				if ansi {
					out.WriteString(ansiCode + inner + ansiReset)
				} else {
					out.WriteString(inner)
				}
				i = end + 1
				continue
			}
		}
		if label, url, n, ok := markdownLink(text[i:]); ok {
			renderedURL := renderURL(url, ansi)
			if strings.TrimSpace(label) == "" || strings.TrimSpace(label) == url {
				out.WriteString(renderedURL)
			} else {
				out.WriteString(renderInline(label, ansi))
				out.WriteString(" <")
				out.WriteString(renderedURL)
				out.WriteByte('>')
			}
			i += n
			continue
		}
		if url, trailing, n, ok := rawURL(text[i:]); ok {
			out.WriteString(renderURL(url, ansi))
			out.WriteString(trailing)
			i += n
			continue
		}
		if rendered, n, ok := emphasis(text, i, ansi); ok {
			out.WriteString(rendered)
			i += n
			continue
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

func renderURL(url string, ansi bool) string {
	if !ansi {
		return url
	}
	return ansiLink + url + ansiReset
}

func markdownLink(s string) (label, url string, n int, ok bool) {
	if !strings.HasPrefix(s, "[") {
		return "", "", 0, false
	}
	closeLabel := strings.Index(s, "](")
	if closeLabel <= 1 {
		return "", "", 0, false
	}
	closeURL := strings.IndexByte(s[closeLabel+2:], ')')
	if closeURL < 0 {
		return "", "", 0, false
	}
	closeURL += closeLabel + 2
	url = strings.TrimSpace(s[closeLabel+2 : closeURL])
	if !isURLStart(url) {
		return "", "", 0, false
	}
	return s[1:closeLabel], url, closeURL + 1, true
}

func rawURL(s string) (url, trailing string, n int, ok bool) {
	if !isURLStart(s) {
		return "", "", 0, false
	}
	end := 0
	for end < len(s) {
		r := rune(s[end])
		if unicode.IsSpace(r) || strings.ContainsRune("<>()", r) {
			break
		}
		end++
	}
	url = s[:end]
	trimmed := strings.TrimRight(url, ".,;:!?")
	trailing = url[len(trimmed):]
	return trimmed, trailing, end, trimmed != ""
}

func isURLStart(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "mailto:")
}

func emphasis(s string, i int, ansi bool) (string, int, bool) {
	for _, marker := range []string{"***", "___", "**", "__", "*", "_"} {
		if !strings.HasPrefix(s[i:], marker) || !emphasisBoundaryStart(s, i, marker) {
			continue
		}
		start := i + len(marker)
		endRel := strings.Index(s[start:], marker)
		if endRel < 0 {
			continue
		}
		end := start + endRel
		if end == start || !emphasisBoundaryEnd(s, end, marker) {
			continue
		}
		content := renderInline(s[start:end], ansi)
		if ansi {
			switch marker {
			case "***", "___":
				content = ansiBold + ansiItalic + content + ansiReset
			case "**", "__":
				content = ansiBold + content + ansiReset
			default:
				content = ansiItalic + content + ansiReset
			}
		}
		return content, end + len(marker) - i, true
	}
	return "", 0, false
}

func emphasisBoundaryStart(s string, i int, marker string) bool {
	if marker[0] == '*' {
		return i+len(marker) < len(s) && !unicode.IsSpace(rune(s[i+len(marker)]))
	}
	if i > 0 && isAlphaNum(rune(s[i-1])) {
		return false
	}
	return i+len(marker) < len(s) && !unicode.IsSpace(rune(s[i+len(marker)]))
}

func emphasisBoundaryEnd(s string, end int, marker string) bool {
	if end == 0 || unicode.IsSpace(rune(s[end-1])) {
		return false
	}
	after := end + len(marker)
	if marker[0] == '_' && after < len(s) && isAlphaNum(rune(s[after])) {
		return false
	}
	return true
}

func isAlphaNum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func fenceMarker(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, codeFenceTick) {
		return codeFenceTick, true
	}
	if strings.HasPrefix(trimmed, codeFenceTilde) {
		return codeFenceTilde, true
	}
	return "", false
}

func splitLeadingWhitespace(s string) (string, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[:i], s[i:]
}

func listMarker(s string) (marker, body string, ok bool) {
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && unicode.IsSpace(rune(s[1])) {
		return "-", strings.TrimLeft(s[2:], " \t"), true
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(s)-1 || (s[i] != '.' && s[i] != ')') || !unicode.IsSpace(rune(s[i+1])) {
		return "", "", false
	}
	return s[:i+1], strings.TrimLeft(s[i+2:], " \t"), true
}

func wrapListBody(body, firstPrefix, nextPrefix string, width int, ansi bool) []string {
	if body == "" {
		return []string{strings.TrimRight(firstPrefix, " ")}
	}
	if width <= len(firstPrefix)+8 {
		return []string{firstPrefix + renderInline(body, ansi)}
	}
	words := strings.Fields(body)
	if len(words) == 0 {
		return []string{strings.TrimRight(firstPrefix, " ")}
	}
	var out []string
	prefix := firstPrefix
	line := words[0]
	limit := width - visibleLen(prefix)
	for _, word := range words[1:] {
		if visibleLen(line)+1+visibleLen(word) > limit {
			out = append(out, prefix+renderInline(line, ansi))
			prefix = nextPrefix
			limit = width - visibleLen(prefix)
			line = word
			continue
		}
		line += " " + word
	}
	out = append(out, prefix+renderInline(line, ansi))
	return out
}

type tableLine struct {
	text    string
	newline bool
}

type alignment int

const (
	alignLeft alignment = iota
	alignRight
	alignCenter
)

func tableCandidate(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && strings.Contains(trimmed, "|")
}

func validTable(lines []tableLine) bool {
	if len(lines) < 2 {
		return false
	}
	header := parseCells(lines[0].text)
	separator := parseCells(lines[1].text)
	return len(header) > 0 && len(separator) > 0 && isSeparatorRow(separator)
}

func parseCells(line string) []string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "|") {
		line = line[1:]
	}
	if strings.HasSuffix(line, "|") {
		line = line[:len(line)-1]
	}
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func isSeparatorRow(cells []string) bool {
	for _, cell := range cells {
		if !isSeparatorCell(cell) {
			return false
		}
	}
	return true
}

func isSeparatorCell(cell string) bool {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return false
	}
	dashes := 0
	for i, r := range cell {
		switch r {
		case '-':
			dashes++
		case ':':
			if i != 0 && i != len(cell)-1 {
				return false
			}
		default:
			return false
		}
	}
	return dashes >= minTableRule
}

func parseAlignments(cells []string) []alignment {
	out := make([]alignment, len(cells))
	for i, cell := range cells {
		cell = strings.TrimSpace(cell)
		left := strings.HasPrefix(cell, ":")
		right := strings.HasSuffix(cell, ":")
		switch {
		case left && right:
			out[i] = alignCenter
		case right:
			out[i] = alignRight
		default:
			out[i] = alignLeft
		}
	}
	return out
}

func formatTableRow(row []string, widths []int, aligns []alignment) string {
	var b strings.Builder
	b.WriteByte('|')
	for i, width := range widths {
		cell := ""
		if i < len(row) {
			cell = row[i]
		}
		fmt.Fprintf(&b, " %s |", padCell(cell, width, aligns[i]))
	}
	return b.String()
}

func formatTableRule(widths []int, aligns []alignment) string {
	var b strings.Builder
	b.WriteByte('|')
	for i, width := range widths {
		ruleWidth := width
		if ruleWidth < minTableRule {
			ruleWidth = minTableRule
		}
		rule := strings.Repeat("-", ruleWidth)
		switch aligns[i] {
		case alignRight:
			rule = strings.Repeat("-", ruleWidth-1) + ":"
		case alignCenter:
			if ruleWidth <= 3 {
				rule = ":-:"
			} else {
				rule = ":" + strings.Repeat("-", ruleWidth-2) + ":"
			}
		}
		fmt.Fprintf(&b, " %s |", rule)
	}
	return b.String()
}

func padCell(cell string, width int, align alignment) string {
	pad := width - visibleLen(cell)
	if pad <= 0 {
		return cell
	}
	switch align {
	case alignRight:
		return strings.Repeat(" ", pad) + cell
	case alignCenter:
		left := pad / 2
		right := pad - left
		return strings.Repeat(" ", left) + cell + strings.Repeat(" ", right)
	default:
		return cell + strings.Repeat(" ", pad)
	}
}

func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < '@' || s[i] > '~') {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			size = 1
		}
		i += size
		n++
	}
	return n
}
