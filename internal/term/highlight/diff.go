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
// syntax-highlighted with independent old- and new-file language states.
type DiffState struct {
	oldContent  *State // nil for unknown languages
	newContent  *State // nil for unknown languages
	diffHeaders diffLineState
}

// NewDiff resolves lang like New ("go", ".py", "Makefile"); empty or unknown
// yields plain content under the same tinted sigils. A nil *DiffState is valid
// and highlights nothing.
func NewDiff(lang string) *DiffState {
	l, ok := Lookup(lang)
	if !ok {
		return &DiffState{}
	}
	return &DiffState{
		oldContent: &State{lang: l},
		newContent: &State{lang: l},
	}
}

// Line styles one diff line (no trailing newline), advancing the appropriate
// old/new content states on +, -, and context lines only.
func (d *DiffState) Line(line string) string {
	if d == nil || line == "" {
		return line
	}
	if style, fileStart := d.diffHeaders.headerStyle(line); style != "" {
		if fileStart {
			d.resetContentStates()
		}
		return styled(style, line)
	}
	content := line[1:]
	switch line[0] {
	case '+':
		d.diffHeaders.markContent('+')
		return tintLine(bgAdded, styled(styleAdded, "+")+d.newContent.Line(content))
	case '-':
		d.diffHeaders.markContent('-')
		return tintLine(bgRemoved, styled(styleRemoved, "-")+d.oldContent.Line(content))
	case ' ':
		d.diffHeaders.markContent(' ')
		// Context belongs to both sides. Advance both lexers, but display the
		// mutated file's interpretation because that is the content users keep.
		d.oldContent.Line(content)
		return " " + d.newContent.Line(content)
	}
	return line
}

func (d *DiffState) resetContentStates() {
	resetState := func(s *State) {
		if s == nil {
			return
		}
		lang := s.lang
		*s = State{lang: lang}
	}
	resetState(d.oldContent)
	resetState(d.newContent)
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
