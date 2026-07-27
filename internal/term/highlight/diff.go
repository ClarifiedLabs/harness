package highlight

import "strings"

// Background tints for added and removed diff lines, GitHub-style. These stay
// inside the 16-color palette so they follow the terminal's theme like the
// foreground styles do.
const (
	bgAdded   = "\x1b[102m"
	bgRemoved = "\x1b[101m"
)

// DiffState highlights unified diff lines: header lines are line-styled, +/-
// lines get a tinted background with a colored sigil, and line content is
// syntax-highlighted with an inner language state.
type DiffState struct {
	content *State // inner language highlighter; nil for unknown languages
}

// NewDiff resolves lang like New ("go", ".py", "Makefile"); empty or unknown
// yields plain content under the same tinted sigils. A nil *DiffState is valid
// and highlights nothing.
func NewDiff(lang string) *DiffState {
	l, ok := Lookup(lang)
	if !ok {
		return &DiffState{}
	}
	return &DiffState{content: &State{lang: l}}
}

// Line styles one diff line (no trailing newline), advancing the inner
// content state on +, -, and context lines only.
func (d *DiffState) Line(line string) string {
	if d == nil || line == "" {
		return line
	}
	if style := diffHeaderStyle(line); style != "" {
		return styled(style, line)
	}
	switch line[0] {
	case '+':
		return tintLine(bgAdded, styled(styleAdded, "+")+d.content.Line(line[1:]))
	case '-':
		return tintLine(bgRemoved, styled(styleRemoved, "-")+d.content.Line(line[1:]))
	case ' ':
		return " " + d.content.Line(line[1:])
	}
	return line
}

// tintLine wraps line in bg, re-applying bg after every inner reset so token
// colors cannot punch holes in the background. The final reset closes the
// background before the caller re-appends the newline.
func tintLine(bg, line string) string {
	if line == "" {
		return line
	}
	line = strings.ReplaceAll(line, styleReset, styleReset+bg)
	return bg + line + styleReset
}
