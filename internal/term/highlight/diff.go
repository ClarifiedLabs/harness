package highlight

import (
	"path/filepath"
	"strings"
)

// eraseToEOL clears from the cursor to the end of the row. Emitted while a
// background color is active, terminals with BCE (background color erase) fill
// the cleared cells with that color, extending the tint to the window edge
// without printing padding spaces. Terminals without BCE degrade to tinting the
// text width.
const eraseToEOL = "\x1b[0K"

// DiffState highlights unified diff lines: header lines are line-styled, +/-
// lines get a full-row tinted background with a colored sigil, and line
// content is syntax-highlighted with independent old- and new-file language
// states.
type DiffState struct {
	oldContent  *State // nil for unknown languages
	newContent  *State // nil for unknown languages
	diffHeaders diffLineState
	palette     palette
}

// NewDiff resolves lang like New ("go", ".py", "Makefile") using the dark
// theme. Empty or unknown languages keep plain content under tinted sigils.
func NewDiff(lang string) *DiffState {
	return NewDiffWithTheme(lang, ThemeDark)
}

// NewDiffWithTheme resolves lang like NewWithTheme. A nil *DiffState is valid
// and highlights nothing.
func NewDiffWithTheme(lang string, theme Theme) *DiffState {
	p := paletteFor(theme)
	d := &DiffState{palette: p}
	l, ok := Lookup(lang)
	if !ok {
		return d
	}
	d.oldContent = &State{lang: l, palette: p}
	d.newContent = &State{lang: l, palette: p}
	return d
}

// Line styles one diff line (no trailing newline), advancing the appropriate
// old/new content states on +, -, and context lines only.
func (d *DiffState) Line(line string) string {
	if d == nil || line == "" {
		return line
	}
	if style, fileStart := d.diffHeaders.headerStyle(line, d.palette); style != "" {
		if fileStart {
			d.resetContentStates()
		}
		return styled(style, line)
	}
	content := line[1:]
	switch line[0] {
	case '+':
		d.diffHeaders.markContent('+')
		return tintLine(d.palette.addedBackground, styled(d.palette.added, "+")+d.newContent.Line(content))
	case '-':
		d.diffHeaders.markContent('-')
		return tintLine(d.palette.removedBackground, styled(d.palette.removed, "-")+d.oldContent.Line(content))
	case ' ':
		d.diffHeaders.markContent(' ')
		// Context belongs to both sides. Advance both lexers, but display the
		// mutated file's interpretation because that is the content users keep.
		d.oldContent.Line(content)
		return " " + d.newContent.Line(content)
	}
	return line
}

// ColorizeDiff syntax-highlights a complete unified diff using the dark theme.
func ColorizeDiff(path, text string) string {
	return ColorizeDiffWithTheme(path, text, ThemeDark)
}

// ColorizeDiffWithTheme syntax-highlights a complete unified diff for path.
// The extension selects the language; extensionless names resolve by basename.
func ColorizeDiffWithTheme(path, text string, theme Theme) string {
	lang := strings.TrimPrefix(filepath.Ext(path), ".")
	if lang == "" {
		lang = filepath.Base(path)
	}
	d := NewDiffWithTheme(lang, theme)
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "\n") {
			lines[i] = d.Line(strings.TrimSuffix(line, "\n")) + "\n"
		} else {
			lines[i] = d.Line(line)
		}
	}
	return strings.Join(lines, "")
}

func (d *DiffState) resetContentStates() {
	resetState := func(s *State) {
		if s == nil {
			return
		}
		lang, p := s.lang, s.palette
		*s = State{lang: lang, palette: p}
	}
	resetState(d.oldContent)
	resetState(d.newContent)
}

// tintLine wraps line in bg, re-applying bg after every inner reset so token
// colors cannot punch holes in the background. eraseToEOL then extends the
// still-active background to the window edge (BCE), and the final reset
// closes it before the caller re-appends the newline.
func tintLine(bg, line string) string {
	if line == "" {
		return line
	}
	line = strings.ReplaceAll(line, styleReset, styleReset+bg)
	return bg + line + eraseToEOL + styleReset
}
