package highlight

import (
	"path/filepath"
	"strings"
)

// Background tints for added and removed diff lines, taken from the Codex
// CLI's dark-terminal palette: #213A2B green, #4A221D red. These truecolor
// tints are more desaturated than the darkest ANSI-256 cube entries (22 and
// 52, used here before), so syntax foreground colors stay distinct over a
// subdued tint. Terminals without 24-bit color quantize them to their
// nearest palette entries.
const (
	bgAdded   = "\x1b[48;2;33;58;43m"
	bgRemoved = "\x1b[48;2;74;34;29m"
	// eraseToEOL clears from the cursor to the end of the row. Emitted while
	// a background color is active, terminals with BCE (background color
	// erase) fill the cleared cells with that color, extending the tint to
	// the window edge without printing padding spaces: rows keep their
	// original bytes, so window-shrink reflow has nothing to wrap and
	// copy-paste stays clean. Terminals without BCE erase with the default
	// background, degrading the tint to the text width.
	eraseToEOL = "\x1b[0K"
)

// DiffState highlights unified diff lines: header lines are line-styled, +/-
// lines get a full-row tinted background with a colored sigil, and line
// content is syntax-highlighted with independent old- and new-file language
// states.
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

// ColorizeDiff syntax-highlights a complete unified diff for the file at
// path: added and removed lines get a tinted background with a colored sigil,
// and line content is highlighted in the mutated file's language (plain when
// the language is unknown). The file extension selects the language;
// extensionless names (Makefile, Dockerfile) resolve by basename.
func ColorizeDiff(path, text string) string {
	lang := strings.TrimPrefix(filepath.Ext(path), ".")
	if lang == "" {
		lang = filepath.Base(path)
	}
	d := NewDiff(lang)
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
		lang := s.lang
		*s = State{lang: lang}
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
