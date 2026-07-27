package highlight

import (
	"strings"
	"testing"
)

func TestDiffAddedLineGoContent(t *testing.T) {
	d := NewDiff("go")
	line := `+func main() { println("hi") }`
	got := d.Line(line)

	if !strings.HasPrefix(got, bgAdded) {
		t.Errorf("added line missing background prefix: %q", got)
	}
	if !strings.Contains(got, styleAdded+"+") {
		t.Errorf("added line missing green sigil: %q", got)
	}
	if !strings.Contains(got, styleKeyword+"func"+styleReset) {
		t.Errorf("added line missing keyword span: %q", got)
	}
	if !strings.Contains(got, styleString+`"hi"`+styleReset) {
		t.Errorf("added line missing string span: %q", got)
	}
	if stripped := stripANSI(got); stripped != line {
		t.Errorf("styling altered the text: in %q, out %q", line, stripped)
	}
}

func TestDiffRemovedLineTint(t *testing.T) {
	d := NewDiff("go")
	got := d.Line(`-return "x"`)
	if !strings.HasPrefix(got, bgRemoved) {
		t.Errorf("removed line missing background prefix: %q", got)
	}
	if !strings.Contains(got, styleRemoved+"-") {
		t.Errorf("removed line missing red sigil: %q", got)
	}
}

// Background integrity: on a tinted line every reset except the final one is
// immediately followed by the bg sequence, and the line ends with a reset so
// no color bleeds into the next line.
func TestDiffBackgroundIntegrity(t *testing.T) {
	d := NewDiff("go")
	for _, tc := range []struct{ line, bg string }{
		{`+func f() string { return "x" } // done`, bgAdded},
		{`-func f() string { return "x" } // done`, bgRemoved},
	} {
		got := d.Line(tc.line)
		if !strings.HasPrefix(got, tc.bg) {
			t.Errorf("%q missing background prefix: %q", tc.line, got)
		}
		if !strings.HasSuffix(got, tc.bg+styleReset) && !strings.HasSuffix(got, styleReset) {
			t.Errorf("%q does not end with a reset: %q", tc.line, got)
		}
		inner := strings.TrimPrefix(got, tc.bg)
		inner = strings.TrimSuffix(inner, styleReset)
		inner = strings.ReplaceAll(inner, styleReset+tc.bg, "")
		if strings.Contains(inner, styleReset) {
			t.Errorf("%q has an inner reset punching a hole in the background: %q", tc.line, got)
		}
	}
}

func TestDiffHeadersAndContextHaveNoBackground(t *testing.T) {
	d := NewDiff("go")
	headers := []string{
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,3 +1,4 @@",
		"diff --git a/main.go b/main.go",
		"index 83db48f..f735c2d 100644",
		"new file mode 100644",
		"deleted file mode 100644",
		"similarity index 88%",
		"rename from old.go",
	}
	for _, line := range headers {
		got := d.Line(line)
		if strings.Contains(got, bgAdded) || strings.Contains(got, bgRemoved) {
			t.Errorf("header %q gained a background: %q", line, got)
		}
		if stripped := stripANSI(got); stripped != line {
			t.Errorf("header %q altered: %q", line, stripped)
		}
	}

	got := d.Line(" package main")
	if strings.Contains(got, bgAdded) || strings.Contains(got, bgRemoved) {
		t.Errorf("context line gained a background: %q", got)
	}
	if !strings.Contains(got, styleKeyword+"package") {
		t.Errorf("context line content not highlighted: %q", got)
	}
	if stripped := stripANSI(got); stripped != " package main" {
		t.Errorf("context line altered: %q", stripped)
	}
}

func TestDiffMultiLineCommentState(t *testing.T) {
	d := NewDiff("go")
	d.Line(" /*")
	got := d.Line("+still comment */")
	if !strings.Contains(got, styleComment+"still comment */") {
		t.Errorf("block comment did not carry across lines: %q", got)
	}
	if !strings.HasPrefix(got, bgAdded) {
		t.Errorf("comment continuation line missing background: %q", got)
	}
}

// Non-content lines (hunk metadata, "\ No newline at end of file") must not
// disturb a pending multi-line construct.
func TestDiffNonContentLinesDoNotDisturbState(t *testing.T) {
	d := NewDiff("go")
	d.Line("+/* open")
	if got := d.Line(`\ No newline at end of file`); got != `\ No newline at end of file` {
		t.Errorf("non-content line should pass through plain: %q", got)
	}
	d.Line("@@ -1,2 +1,2 @@")
	got := d.Line("+close */")
	if !strings.Contains(got, styleComment+"close */") {
		t.Errorf("intervening non-content lines disturbed comment state: %q", got)
	}
}

func TestDiffUnknownLanguage(t *testing.T) {
	d := NewDiff("definitely-not-a-language")
	got := d.Line("+plain func text")

	if !strings.HasPrefix(got, bgAdded) {
		t.Errorf("unknown language missing background: %q", got)
	}
	if !strings.Contains(got, styleAdded+"+") {
		t.Errorf("unknown language missing sigil color: %q", got)
	}
	// Plain content: no token spans, so the content sits directly under the
	// re-applied background left by the sigil's reset.
	if !strings.Contains(got, bgAdded+"plain func text"+styleReset) {
		t.Errorf("content should be plain under the tint: %q", got)
	}
	if stripped := stripANSI(got); stripped != "+plain func text" {
		t.Errorf("styling altered the text: %q", stripped)
	}
}

func TestDiffEmptyLanguage(t *testing.T) {
	d := NewDiff("")
	got := d.Line("-gone")
	if !strings.HasPrefix(got, bgRemoved) || !strings.Contains(got, bgRemoved+"gone"+styleReset) {
		t.Errorf("empty language should tint with plain content: %q", got)
	}
}

func TestDiffNilState(t *testing.T) {
	var d *DiffState
	for _, line := range []string{"+added", "-removed", " context", "@@ -1 +1 @@"} {
		if got := d.Line(line); got != line {
			t.Errorf("nil DiffState should pass %q through, got %q", line, got)
		}
	}
}

func TestDiffSigilOnlyLines(t *testing.T) {
	d := NewDiff("go")
	if got := d.Line("+"); !strings.Contains(got, bgAdded) || stripANSI(got) != "+" {
		t.Errorf("lone + should be tinted and byte-preserving: %q", got)
	}
	if got := d.Line("-"); !strings.Contains(got, bgRemoved) || stripANSI(got) != "-" {
		t.Errorf("lone - should be tinted and byte-preserving: %q", got)
	}
	if got := d.Line(" "); got != " " {
		t.Errorf("lone space should pass through plain: %q", got)
	}
	if got := d.Line(""); got != "" {
		t.Errorf("empty line should pass through plain: %q", got)
	}
}
