package mermaid

import (
	"strings"
	"testing"
)

func TestMergeLine(t *testing.T) {
	tests := []struct {
		old, ch, want rune
	}{
		{' ', '-', '-'},
		{'-', '-', '-'},
		{'|', '-', '+'},
		{'-', '|', '+'},
		{':', '-', '+'},
		{'.', '|', '+'},
		{'+', '-', '+'},
		{'a', '-', '-'},
	}
	for _, tt := range tests {
		if got := mergeLine(tt.old, tt.ch); got != tt.want {
			t.Errorf("mergeLine(%q, %q) = %q, want %q", tt.old, tt.ch, got, tt.want)
		}
	}
}

func TestCanvasBasics(t *testing.T) {
	c := &canvas{}
	c.text(2, 1, "hi")
	got := c.String()
	want := "\n  hi\n"
	if got != want {
		t.Errorf("canvas.String() = %q, want %q", got, want)
	}
	if c.get(2, 1) != 'h' || c.get(99, 99) != ' ' {
		t.Errorf("get returned unexpected values")
	}
}

func TestCanvasLinesCross(t *testing.T) {
	c := &canvas{}
	c.hline(0, 4, 1, '-')
	c.vline(0, 2, 2, '|')
	if got := c.get(2, 1); got != '+' {
		t.Errorf("crossing = %q, want '+'", got)
	}
}

func TestDispWidth(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"日本語", 6},      // CJK: two columns each
		{"Ünïcodé", 7},  // latin-1 accents: one column each
		{"✓", 1},        // narrow symbol
		{"ｱｲｳ", 3},      // halfwidth katakana stays narrow
		{"ＡＢ", 4},       // fullwidth latin
		{"한글", 4},       // hangul syllables
		{"e\u0301", 1},  // combining acute adds no width
		{"a\u200Bb", 2}, // zero-width space
		{"a日b", 4},
	}
	for _, tt := range tests {
		if got := dispWidth(tt.s); got != tt.want {
			t.Errorf("dispWidth(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

// A wide rune must occupy two canvas columns, so that anything drawn to its
// right lands where the layout put it.
func TestCanvasWideRuneColumns(t *testing.T) {
	c := &canvas{}
	c.text(0, 0, "日本")
	c.put(4, 0, '|')
	if got := c.String(); got != "日本|\n" {
		t.Errorf("String() = %q, want %q", got, "日本|\n")
	}
	// the pipe sits at column 4, not at rune index 4
	if got := dispWidth(strings.TrimSuffix(c.String(), "\n")); got != 5 {
		t.Errorf("row width = %d, want 5", got)
	}
}

// Overwriting half of a wide rune must not leave the row a column short.
func TestCanvasOverwriteWideRune(t *testing.T) {
	for _, x := range []int{0, 1} {
		c := &canvas{}
		c.text(0, 0, "日x")
		c.put(x, 0, '-')
		got := strings.TrimSuffix(c.String(), "\n")
		if dispWidth(got) != 3 {
			t.Errorf("put at %d: %q is %d columns, want 3", x, got, dispWidth(got))
		}
	}
}

// A box drawn around a CJK label must line up with it.
func TestRenderWideLabelBoxAligns(t *testing.T) {
	out := mustRender(t, "flowchart TD\n  A[日本語 text] --> B[ok]")
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.Contains(l, "日") {
			continue
		}
		border := strings.Split(strings.TrimSuffix(out, "\n"), "\n")[0]
		if dispWidth(l) != dispWidth(border) {
			t.Errorf("label row is %d columns but border is %d:\n%s",
				dispWidth(l), dispWidth(border), out)
		}
	}
}

func TestCanvasRect(t *testing.T) {
	c := &canvas{}
	c.rect(0, 0, 4, 3)
	want := "+--+\n|  |\n+--+\n"
	if got := c.String(); got != want {
		t.Errorf("rect = %q, want %q", got, want)
	}
}
