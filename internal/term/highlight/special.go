package highlight

import "strings"

// Languages whose structure is carried by line shape rather than by tokens get
// their own small lexer instead of the generic scanner. Each may still delegate
// to the scanner for the parts that are ordinary code.

const (
	styleAdded   = "\x1b[32m"
	styleRemoved = "\x1b[31m"
)

func init() {
	languages["diff"] = &Language{lex: lexDiff}

	languages["yaml"] = &Language{
		LineComments: []string{"#"},
		Quotes:       `"'`,
		Escape:       '\\',
		StartExtra:   "&*",
		VarPrefix:    "&*",
		Builtins:     words(`true false null yes no on off True False Null None ~`),
		lex:          lexYAML,
	}

	languages["toml"] = &Language{
		LineComments: []string{"#", ";"},
		Quotes:       `"'`,
		RawStrings:   []string{`"""`, `'''`},
		Escape:       '\\',
		PartExtra:    "-",
		Builtins:     words(`true false inf nan`),
		lex:          lexTOML,
	}

	languages["html"] = &Language{lex: lexHTML}

	languages["css"] = func() *Language {
		l := cFamily(
			`@charset @font-face @import @keyframes @layer @media @supports and from
			 important not only to`,
			`align-items animation background background-color border border-radius bottom
			 box-shadow box-sizing color content cursor display flex flex-direction
			 flex-wrap float font font-family font-size font-style font-weight gap grid
			 grid-template-columns grid-template-rows height justify-content left
			 letter-spacing line-height list-style margin margin-bottom margin-left
			 margin-right margin-top max-height max-width min-height min-width opacity
			 outline overflow padding padding-bottom padding-left padding-right
			 padding-top position right text-align text-decoration text-transform top
			 transform transition user-select vertical-align visibility white-space width
			 word-break z-index`,
		)
		l.PartExtra = "-"
		l.StartExtra = "@"
		return l
	}()
}

// lexDiff colors unified diffs by line prefix. Header lines are checked before
// the bare +/- cases so "+++" and "---" are not mistaken for content.
func lexDiff(_ *State, line string) string {
	if style := diffHeaderStyle(line); style != "" {
		return styled(style, line)
	}
	switch {
	case strings.HasPrefix(line, "+"):
		return styled(styleAdded, line)
	case strings.HasPrefix(line, "-"):
		return styled(styleRemoved, line)
	}
	return line
}

// diffHeaderStyle returns the whole-line style for a unified-diff header or
// git metadata line, or "" when the line is diff content. DiffState shares
// this classification so the two cannot drift.
func diffHeaderStyle(line string) string {
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return styleBuiltin
	case strings.HasPrefix(line, "@@"):
		return styleKeyword
	case strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
		strings.HasPrefix(line, "old mode"), strings.HasPrefix(line, "new mode"),
		strings.HasPrefix(line, "similarity "), strings.HasPrefix(line, "rename "):
		return styleComment
	}
	return ""
}

// lexYAML colors mapping keys distinctly and hands the value to the scanner.
func lexYAML(s *State, line string) string {
	var b strings.Builder
	b.Grow(len(line) + 16)

	i := skipSpace(line, 0)
	// Sequence markers, which may nest on one line: "- - item".
	for i < len(line) && line[i] == '-' && (i+1 == len(line) || isSpace(line[i+1])) {
		i = skipSpace(line, i+1)
	}
	b.WriteString(line[:i])

	rest := line[i:]
	if k := yamlKeyEnd(rest); k > 0 {
		span(&b, styleBuiltin, rest[:k])
		b.WriteByte(':')
		b.WriteString(s.scan(rest[k+1:]))
		return b.String()
	}
	b.WriteString(s.scan(rest))
	return b.String()
}

// yamlKeyEnd returns the offset of the colon ending a mapping key, or 0 when
// the line does not open one.
func yamlKeyEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ':':
			if i+1 == len(s) || isSpace(s[i+1]) {
				return i
			}
		case '#':
			return 0
		}
	}
	return 0
}

// lexTOML colors [section] headers and hands everything else to the scanner.
func lexTOML(s *State, line string) string {
	trimmed := strings.TrimSpace(line)
	if s.mode == modeNormal && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return styled(styleKeyword, line)
	}
	return s.scan(line)
}

// lexHTML colors tags, attribute names and attribute values, leaving the text
// between tags plain.
func lexHTML(s *State, line string) string {
	var b strings.Builder
	b.Grow(len(line) + 16)

	i, done := s.resume(&b, line)
	if done {
		return b.String()
	}

	for i < len(line) {
		if strings.HasPrefix(line[i:], "<!--") {
			end, ok := closes(line, i+len("<!--"), "-->")
			if !ok {
				span(&b, styleComment, line[i:])
				s.suspend(modeComment, "-->")
				return b.String()
			}
			span(&b, styleComment, line[i:end])
			i = end
			continue
		}
		if line[i] == '<' && i+1 < len(line) && isTagStart(line[i+1]) {
			i = htmlTag(&b, line, i)
			continue
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

func isTagStart(c byte) bool {
	return isLetter(c) || c == '/' || c == '!' || c == '?'
}

// htmlTag styles the tag opening at line[i] and returns the offset past it.
func htmlTag(b *strings.Builder, line string, i int) int {
	b.WriteByte('<')
	j := i + 1
	if line[j] == '/' || line[j] == '!' || line[j] == '?' {
		b.WriteByte(line[j])
		j++
	}
	name := j
	for j < len(line) && isNameByte(line[j]) {
		j++
	}
	span(b, styleKeyword, line[name:j])

	for j < len(line) && line[j] != '>' {
		switch c := line[j]; {
		case c == '"' || c == '\'':
			end, ok := scanQuoted(line, j, 0)
			if !ok {
				end = len(line)
			}
			span(b, styleString, line[j:end])
			j = end
		case isLetter(c) || c == '_':
			k := j
			for k < len(line) && isNameByte(line[k]) {
				k++
			}
			span(b, styleBuiltin, line[j:k])
			j = k
		default:
			b.WriteByte(c)
			j++
		}
	}
	if j < len(line) {
		b.WriteByte('>')
		j++
	}
	return j
}

// isNameByte reports whether c may appear in a tag or attribute name.
func isNameByte(c byte) bool {
	return isLetter(c) || isDigit(c) || strings.IndexByte("-_:.@", c) >= 0
}

func skipSpace(s string, i int) int {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	return i
}
