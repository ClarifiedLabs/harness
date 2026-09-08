package mermaid

import (
	"sort"
	"strings"
	"unicode"
)

// wideCont fills the second column of a double-width rune. Keeping it in the
// grid means x coordinates are always terminal columns rather than rune
// counts; it is dropped again when the canvas is stringified.
const wideCont = '\x00'

// canvas is a growable 2-D grid of terminal cells used to compose ASCII
// drawings. One cell is one display column.
type canvas struct {
	rows [][]rune
	w, h int
	err  error
}

// reserve checks the bounding rectangle before any row allocation or draw loop.
// Errors stick: subsequent drawing becomes a no-op, and result discards the canvas.
func (c *canvas) reserve(x, y int) bool {
	if c.err != nil {
		return false
	}
	w, h := maxInt(c.w, x+1), maxInt(c.h, y+1)
	if err := checkCanvasSize(w, h); err != nil {
		c.err = err
		return false
	}
	c.w, c.h = w, h
	return true
}

func (c *canvas) ensure(x, y int) bool {
	if !c.reserve(x, y) {
		return false
	}
	for len(c.rows) <= y {
		c.rows = append(c.rows, nil)
	}
	row := c.rows[y]
	for len(row) <= x {
		row = append(row, ' ')
	}
	c.rows[y] = row
	return true
}

func (c *canvas) get(x, y int) rune {
	if y < 0 || y >= len(c.rows) || x < 0 || x >= len(c.rows[y]) {
		return ' '
	}
	return c.rows[y][x]
}

// put writes ch unconditionally.
func (c *canvas) put(x, y int, ch rune) {
	if x < 0 || y < 0 {
		return
	}
	if !c.ensure(x, y) {
		return
	}
	c.detach(x, y)
	c.rows[y][x] = ch
}

// detach blanks the other half of a double-width rune overlapping (x, y), so
// that overwriting one column never leaves an orphaned lead or continuation
// behind to throw the row's column count off.
func (c *canvas) detach(x, y int) {
	row := c.rows[y]
	if row[x] == wideCont {
		if x > 0 {
			row[x-1] = ' '
		}
		return
	}
	if runeWidth(row[x]) == 2 && x+1 < len(row) && row[x+1] == wideCont {
		row[x+1] = ' '
	}
}

// putLine writes a line character, merging with a perpendicular line
// already present at the same cell into a '+' junction.
func (c *canvas) putLine(x, y int, ch rune) {
	c.put(x, y, mergeLine(c.get(x, y), ch))
}

func isVertChar(r rune) bool  { return r == '|' || r == ':' || r == '#' }
func isHorizChar(r rune) bool { return r == '-' || r == '.' || r == '=' }

func mergeLine(old, ch rune) rune {
	switch {
	case old == ' ' || old == 0:
		return ch
	case old == ch:
		return ch
	case old == '+':
		return '+'
	case isVertChar(old) && isHorizChar(ch), isHorizChar(old) && isVertChar(ch):
		return '+'
	}
	return ch
}

// text writes a string starting at (x, y), overwriting existing cells. Each
// rune advances x by its display width.
func (c *canvas) text(x, y int, s string) {
	if c.err != nil || !c.reserve(x+dispWidth(s)-1, y) {
		return
	}
	for _, r := range s {
		w := runeWidth(r)
		if w == 0 {
			continue // combining marks ride along with the previous cell
		}
		c.put(x, y, r)
		if w == 2 {
			c.put(x+1, y, wideCont)
		}
		x += w
	}
}

func (c *canvas) hline(x1, x2, y int, ch rune) {
	if x2 < x1 {
		x1, x2 = x2, x1
	}
	if !c.reserve(x2, y) {
		return
	}
	for x := x1; x <= x2; x++ {
		c.putLine(x, y, ch)
	}
}

func (c *canvas) vline(y1, y2, x int, ch rune) {
	if y2 < y1 {
		y1, y2 = y2, y1
	}
	if !c.reserve(x, y2) {
		return
	}
	for y := y1; y <= y2; y++ {
		c.putLine(x, y, ch)
	}
}

// dashedHline draws an alternating "- - -" line (used for dashed arrows and
// frame dividers in sequence diagrams).
func (c *canvas) dashedHline(x1, x2, y int) {
	if x2 < x1 {
		x1, x2 = x2, x1
	}
	if !c.reserve(x2, y) {
		return
	}
	for x := x1; x <= x2; x++ {
		if (x-x1)%2 == 0 {
			c.putLine(x, y, '-')
		}
	}
}

// rect draws a plain box border with '+' corners.
func (c *canvas) rect(x, y, w, h int) {
	c.hline(x+1, x+w-2, y, '-')
	c.hline(x+1, x+w-2, y+h-1, '-')
	c.vline(y+1, y+h-2, x, '|')
	c.vline(y+1, y+h-2, x+w-1, '|')
	c.put(x, y, '+')
	c.put(x+w-1, y, '+')
	c.put(x, y+h-1, '+')
	c.put(x+w-1, y+h-1, '+')
}

// clear blanks a rectangular region.
func (c *canvas) clear(x, y, w, h int) {
	if !c.reserve(x+w-1, y+h-1) {
		return
	}
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			c.put(xx, yy, ' ')
		}
	}
}

func (c *canvas) result() (string, error) {
	if c.err != nil {
		return "", c.err
	}
	return c.String(), nil
}

func (c *canvas) String() string {
	var b strings.Builder
	line := make([]rune, 0, 128)
	for _, row := range c.rows {
		line = line[:0]
		for _, r := range row {
			if r == wideCont {
				continue // its lead rune already covers this column
			}
			line = append(line, r)
		}
		b.WriteString(strings.TrimRight(string(line), " "))
		b.WriteByte('\n')
	}
	s := strings.TrimRight(b.String(), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// dispWidth returns how many terminal columns s occupies.
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth returns the number of terminal columns r occupies: 0 for
// combining marks and other zero-width characters, 2 for East Asian wide and
// fullwidth characters, 1 otherwise.
func runeWidth(r rune) int {
	switch {
	case r == wideCont:
		return 0
	case r == 0 || r == 0x200B || r == 0xFEFF: // NUL, zero-width space, BOM
		return 0
	case r < 0x0300: // fast path: ASCII and Latin, all below the combining marks
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r < 0x1100: // nothing wide below the Hangul Jamo block
		return 1
	}
	i := sort.Search(len(wideRanges), func(i int) bool { return wideRanges[i].hi >= r })
	if i < len(wideRanges) && wideRanges[i].lo <= r {
		return 2
	}
	return 1
}

// wideRanges lists the East Asian Wide (W) and Fullwidth (F) blocks, sorted
// and non-overlapping. It covers the CJK, Hangul, Kana and emoji ranges that
// turn up in diagram labels rather than every code point in the standard.
var wideRanges = []struct{ lo, hi rune }{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A},
	{0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653},
	{0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5},
	{0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA},
	{0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E},
	{0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x303E}, {0x3041, 0x33FF},
	{0x3400, 0x4DBF}, {0x4E00, 0x9FFF},
	{0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3},
	{0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F},
	{0xFF00, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x16FE0, 0x16FE4}, {0x17000, 0x18CD5}, {0x1B000, 0x1B2FB},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E},
	{0x1F191, 0x1F19A}, {0x1F200, 0x1F320}, {0x1F32D, 0x1F335},
	{0x1F337, 0x1F37C}, {0x1F37E, 0x1F393}, {0x1F3A0, 0x1F3CA},
	{0x1F3CF, 0x1F3D3}, {0x1F3E0, 0x1F3F0}, {0x1F3F4, 0x1F3F4},
	{0x1F3F8, 0x1F43E}, {0x1F440, 0x1F440}, {0x1F442, 0x1F4FC},
	{0x1F4FF, 0x1F53D}, {0x1F54B, 0x1F54E}, {0x1F550, 0x1F567},
	{0x1F57A, 0x1F57A}, {0x1F595, 0x1F596}, {0x1F5A4, 0x1F5A4},
	{0x1F5FB, 0x1F64F}, {0x1F680, 0x1F6C5}, {0x1F6CC, 0x1F6CC},
	{0x1F6D0, 0x1F6D2}, {0x1F6EB, 0x1F6EC}, {0x1F6F4, 0x1F6FC},
	{0x1F7E0, 0x1F7EB}, {0x1F90C, 0x1F93A}, {0x1F93C, 0x1F945},
	{0x1F947, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

func maxLineLen(lines []string) int {
	m := 0
	for _, l := range lines {
		if w := dispWidth(l); w > m {
			m = w
		}
	}
	return m
}
