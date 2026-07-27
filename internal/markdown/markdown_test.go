package markdown

import (
	"strings"
	"testing"
)

func TestRenderDisabledReturnsRawText(t *testing.T) {
	in := "**bold**\n[docs](https://example.com)"
	if got := Render(in, Options{}); got != in {
		t.Fatalf("Render disabled = %q, want raw %q", got, in)
	}
}

func TestRenderStripsEmphasisWithoutANSI(t *testing.T) {
	got := Render("Use **bold**, *italic*, and ***both***.\n---", Options{Enabled: true})
	want := "Use bold, italic, and both.\n" + HorizontalRule
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestRenderAppliesANSIEmphasisAndHeadings(t *testing.T) {
	got := Render("# Title\nUse **bold** and *italic*.", Options{Enabled: true, ANSI: true})
	for _, want := range []string{
		ansiBoldUnderline + "# Title" + ansiUnderlineOff + ansiBoldOff,
		ansiBold + "bold" + ansiBoldOff,
		ansiItalic + "italic" + ansiItalicOff,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output missing %q:\n%q", want, got)
		}
	}
}

func TestRenderLinksAndRawURLs(t *testing.T) {
	got := Render("Read [docs](https://example.com/docs) and https://example.com/path.", Options{Enabled: true})
	want := "Read docs <https://example.com/docs> and https://example.com/path."
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}

	gotANSI := Render("See https://example.com.", Options{Enabled: true, ANSI: true})
	wantANSI := ansiLink + "https://example.com" + ansiColorOff + ansiUnderlineOff + "."
	if gotANSI != "See "+wantANSI {
		t.Fatalf("ANSI URL = %q, want %q", gotANSI, "See "+wantANSI)
	}
}

func TestRenderListsNormalizeMarkersAndWrapContinuations(t *testing.T) {
	input := "* first item has several words\n  + child item\n1. ordered item"
	got := Render(input, Options{Enabled: true, Width: 24})
	want := "- first item has several\n  words\n  - child item\n1. ordered item"
	if got != want {
		t.Fatalf("Render =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderWrapsParagraphsWithPrefix(t *testing.T) {
	input := "alpha beta gamma delta epsilon zeta eta theta"
	got := Render(input, Options{Enabled: true, Width: 24, Prefix: "  "})
	want := "  alpha beta gamma delta\n  epsilon zeta eta theta"
	if got != want {
		t.Fatalf("Render =\n%q\nwant\n%q", got, want)
	}
}

// Regression: wrapping used to split the source text and render each resulting
// line on its own, so a span crossing a wrap point lost the half of its markers
// that landed on the other line. Neither half then parsed, and the raw markers
// reached the output -- in plain mode as well as colored.
func TestRenderWrapsSpansAcrossLineBreaks(t *testing.T) {
	const input = "Alpha **beta gamma delta** epsilon"

	plain := Render(input, Options{Enabled: true, Width: 20})
	if want := "Alpha beta gamma\ndelta epsilon"; plain != want {
		t.Errorf("plain wrap =\n%q\nwant\n%q", plain, want)
	}
	if strings.Contains(plain, "*") {
		t.Errorf("emphasis markers leaked into plain output: %q", plain)
	}

	got := Render(input, Options{Enabled: true, ANSI: true, Width: 20})
	want := "Alpha " + ansiBold + "beta gamma" + ansiBoldOff + "\n" +
		ansiBold + "delta" + ansiBoldOff + " epsilon"
	if got != want {
		t.Errorf("ANSI wrap =\n%q\nwant\n%q", got, want)
	}
}

// ANSI escapes separated from their visible content by span padding must not
// become zero-width "words" that introduce spaces absent from plain rendering.
func TestRenderWrapIgnoresANSIOnlyFields(t *testing.T) {
	for _, input := range []string{
		"start ` code ` end words",
		"start `` end words",
	} {
		plain := Render(input, Options{Enabled: true, Width: 12})
		colored := Render(input, Options{Enabled: true, ANSI: true, Width: 12})
		if got := stripANSI(colored); got != plain {
			t.Errorf("colored padded span stripped =\n%q\nplain =\n%q\nfor input %q", got, plain, input)
		}
	}
}

// The continuation indent belongs to the list, not to the span being carried
// over the break, so styling closes before the newline and reopens past the
// indent instead of running through it.
func TestRenderWrapKeepsContinuationIndentUnstyled(t *testing.T) {
	got := Render("- **alpha beta gamma delta**", Options{Enabled: true, ANSI: true, Width: 20})
	want := "- " + ansiBold + "alpha beta gamma" + ansiBoldOff + "\n" +
		"  " + ansiBold + "delta" + ansiBoldOff
	if got != want {
		t.Fatalf("wrapped list item =\n%q\nwant\n%q", got, want)
	}
}

// Width counts the columns the reader sees, not the source columns: the eight
// characters of "**one**" occupy three.
func TestRenderWrapsOnRenderedWidth(t *testing.T) {
	got := Render("**one** **two** **three** **four**", Options{Enabled: true, Width: 12})
	if want := "one two\nthree four"; got != want {
		t.Fatalf("wrap by rendered width =\n%q\nwant\n%q", got, want)
	}
}

// Styling decorates, it never rewrites: at any width, stripping the escapes
// from a wrapped colored render must give the wrapped plain render back, and
// no markup may survive into either.
func TestWrappedOutputStripsToPlainAtEveryWidth(t *testing.T) {
	inputs := []string{
		"Alpha **beta gamma delta** epsilon zeta eta theta",
		"Read the *manual at https://example.com/docs today* please",
		"Use `some inline code` and **bold [docs](https://example.com) here** too",
		"Use ` padded inline code ` and an `` empty span across several words",
		"- **alpha beta** gamma `delta epsilon` zeta",
		"***everything emphasized across a long enough line to wrap twice***",
	}
	for _, input := range inputs {
		for width := 12; width <= 40; width++ {
			plain := Render(input, Options{Enabled: true, Width: width})
			colored := Render(input, Options{Enabled: true, ANSI: true, Width: width})
			if got := stripANSI(colored); got != plain {
				t.Errorf("width %d: colored stripped =\n%q\nplain =\n%q\nfor input %q",
					width, got, plain, input)
			}
			for _, marker := range []string{"**", "](", "`"} {
				if strings.Contains(plain, marker) {
					t.Errorf("width %d: %q survived rendering of %q:\n%q",
						width, marker, input, plain)
				}
			}
		}
	}
}

func TestRenderFormatsTables(t *testing.T) {
	input := "| Name | Count |\n| --- | ---: |\n| a | 2 |\n| long | 10 |\n"
	got := Render(input, Options{Enabled: true})
	want := "| Name | Count |\n" +
		"| ---- | ----: |\n" +
		"| a    |     2 |\n" +
		"| long |    10 |\n"
	if got != want {
		t.Fatalf("Render =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderPrefixesTables(t *testing.T) {
	input := "| Name | Count |\n| --- | ---: |\n| a | 2 |\n"
	got := Render(input, Options{Enabled: true, Prefix: "  "})
	want := "  | Name | Count |\n" +
		"  | ---- | ----: |\n" +
		"  | a    |     2 |\n"
	if got != want {
		t.Fatalf("Render =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderPreservesCodeFences(t *testing.T) {
	input := "```\n**raw** https://example.com\n```\n"
	want := "  ```\n  **raw** https://example.com\n  ```\n"
	if got := Render(input, Options{Enabled: true, ANSI: true}); got != want {
		t.Fatalf("code fence rendered = %q, want %q", got, want)
	}
}

func TestRenderInlineCodeStripsBackticksWithoutANSI(t *testing.T) {
	got := Render("Use `foo` here.", Options{Enabled: true})
	want := "Use foo here."
	if got != want {
		t.Fatalf("inline code no-ANSI = %q, want %q", got, want)
	}
}

func TestRenderInlineCodeAppliesANSI(t *testing.T) {
	got := Render("Use `foo` here.", Options{Enabled: true, ANSI: true})
	want := "Use " + ansiCode + "foo" + ansiColorOff + " here."
	if got != want {
		t.Fatalf("inline code ANSI = %q, want %q", got, want)
	}
}

// Regression: spans used to close with a blanket "\x1b[0m", so an inline span
// nested inside a heading or emphasis cancelled the style wrapping it and the
// rest of the line rendered unstyled.
func TestRenderNestedSpansPreserveOuterStyle(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "code inside heading keeps heading bold",
			input: "# Heading with `code` and more",
			want:  ansiBoldUnderline + "# Heading with " + ansiCode + "code" + ansiColorOff + " and more" + ansiUnderlineOff + ansiBoldOff,
		},
		{
			name:  "code inside bold keeps bold",
			input: "**bold with `code` inside**",
			want:  ansiBold + "bold with " + ansiCode + "code" + ansiColorOff + " inside" + ansiBoldOff,
		},
		{
			name:  "code inside italic keeps italic",
			input: "*italic with `code` inside*",
			want:  ansiItalic + "italic with " + ansiCode + "code" + ansiColorOff + " inside" + ansiItalicOff,
		},
		{
			name:  "link inside bold keeps bold",
			input: "**see https://example.com now**",
			want:  ansiBold + "see " + ansiLink + "https://example.com" + ansiColorOff + ansiUnderlineOff + " now" + ansiBoldOff,
		},
		{
			name:  "bold inside heading does not end the heading",
			input: "## Heading with **bold** and more",
			want:  ansiBold + "## Heading with " + ansiBold + "bold" + " and more" + ansiBoldOff,
		},
		{
			name:  "link inside h1 keeps the heading underline",
			input: "# See https://example.com now",
			want:  ansiBoldUnderline + "# See " + ansiLink + "https://example.com" + ansiColorOff + " now" + ansiUnderlineOff + ansiBoldOff,
		},
		{
			name:  "bold italic closes both attributes",
			input: "***both***",
			want:  ansiBold + ansiItalic + "both" + ansiItalicOff + ansiBoldOff,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Render(tt.input, Options{Enabled: true, ANSI: true})
			if got != tt.want {
				t.Fatalf("Render(%q) =\n%q\nwant\n%q", tt.input, got, tt.want)
			}
			if strings.Contains(got, "\x1b[0m") {
				t.Errorf("Render(%q) used a blanket reset, which cancels the enclosing style: %q", tt.input, got)
			}
		})
	}
}

// Inline code is cyan, not yellow: yellow falls under 3:1 contrast against most
// light terminal backgrounds, and cyan is the terminal convention for code.
func TestInlineCodeUsesCyan(t *testing.T) {
	if ansiCode != "\x1b[36m" {
		t.Fatalf("inline code style = %q, want cyan \\x1b[36m", ansiCode)
	}
}

func TestRenderCodeFenceIsIndented(t *testing.T) {
	input := "```go\nfmt.Println()\n```\n"
	want := "  ```go\n  fmt.Println()\n  ```\n"
	if got := Render(input, Options{Enabled: true}); got != want {
		t.Fatalf("code fence indent = %q, want %q", got, want)
	}
}

func TestRenderInlineCodeUnterminated(t *testing.T) {
	// A lone backtick with no closing backtick must pass through unchanged.
	got := Render("price is 5`", Options{Enabled: true, ANSI: true})
	want := "price is 5`"
	if got != want {
		t.Fatalf("unterminated backtick = %q, want %q", got, want)
	}
}

func TestStreamHandlesSplitInlineMarkdown(t *testing.T) {
	stream := NewStream(Options{Enabled: true})
	if got := stream.Write("**bo"); got != "" {
		t.Fatalf("first split Write = %q, want empty", got)
	}
	if got := stream.Write("ld**\n"); got != "bold\n" {
		t.Fatalf("second split Write = %q, want bold line", got)
	}
	if got := stream.Flush(); got != "" {
		t.Fatalf("Flush = %q, want empty", got)
	}
}

func TestStreamBuffersTablesUntilBlockEnds(t *testing.T) {
	stream := NewStream(Options{Enabled: true})
	if got := stream.Write("| A | B |\n| --- | --- |\n| x | yy |\n"); got != "" {
		t.Fatalf("table should be buffered, got %q", got)
	}
	got := stream.Write("after\n")
	want := "| A   | B   |\n" +
		"| --- | --- |\n" +
		"| x   | yy  |\n" +
		"after\n"
	if got != want {
		t.Fatalf("table flush =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderHighlightsLabeledCodeFence(t *testing.T) {
	input := "```go\nfunc main() {\n```\n"
	got := Render(input, Options{Enabled: true, ANSI: true})
	body := strings.Split(got, "\n")[1]
	if !strings.Contains(body, "\x1b[") {
		t.Fatalf("go fence body was not highlighted: %q", body)
	}
	if !strings.Contains(body, "func") {
		t.Fatalf("go fence body lost its text: %q", body)
	}
}

// TestRenderHighlightingIsAdditive holds the renderer to the same contract the
// highlighter has: color decorates the code, it never rewrites it, so a
// stripped colored render equals the plain one.
func TestRenderHighlightingIsAdditive(t *testing.T) {
	input := "```python\n# a comment\ndef f(x):\n    return x * 2\n```\n"
	plain := Render(input, Options{Enabled: true})
	colored := Render(input, Options{Enabled: true, ANSI: true})
	if colored == plain {
		t.Fatal("colored render is identical to plain; highlighting did not run")
	}
	if got := stripANSI(colored); got != plain {
		t.Fatalf("stripped colored render = %q, want %q", got, plain)
	}
}

func TestRenderLeavesUnknownLanguageUntouched(t *testing.T) {
	for _, info := range []string{"", "brainfuck", "text"} {
		input := "```" + info + "\nfunc main() {\n```\n"
		want := Render(input, Options{Enabled: true})
		if got := Render(input, Options{Enabled: true, ANSI: true}); got != want {
			t.Errorf("fence %q was styled: %q, want %q", info, got, want)
		}
	}
}

func TestRenderLeavesFenceDelimitersUnstyled(t *testing.T) {
	got := Render("```go\nfunc main() {\n```\n", Options{Enabled: true, ANSI: true})
	lines := strings.Split(got, "\n")
	for _, i := range []int{0, 2} {
		if strings.Contains(lines[i], "\x1b[") {
			t.Errorf("fence delimiter line %d was styled: %q", i, lines[i])
		}
	}
}

func TestRenderDoesNotHighlightWithoutANSI(t *testing.T) {
	input := "```go\nfunc main() {\n```\n"
	want := "  ```go\n  func main() {\n  ```\n"
	if got := Render(input, Options{Enabled: true}); got != want {
		t.Fatalf("no-ANSI render = %q, want %q", got, want)
	}
}

// TestRenderResetsHighlighterBetweenFences catches the highlighter outliving
// its block and coloring an unlabeled one that follows.
func TestRenderResetsHighlighterBetweenFences(t *testing.T) {
	input := "```go\nfunc main() {\n```\n\n```\nfunc main() {\n```\n"
	got := Render(input, Options{Enabled: true, ANSI: true})
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[1], "\x1b[") {
		t.Fatalf("first fence body was not highlighted: %q", lines[1])
	}
	if strings.Contains(lines[5], "\x1b[") {
		t.Fatalf("unlabeled second fence was highlighted: %q", lines[5])
	}
}

// TestRenderCarriesHighlightStateAcrossFenceLines covers a construct that
// spans lines: the middle line of a block comment has no delimiter of its own
// and is only recognizable from the line before it.
func TestRenderCarriesHighlightStateAcrossFenceLines(t *testing.T) {
	input := "```go\n/* one\ntwo\nthree */\n```\n"
	got := Render(input, Options{Enabled: true, ANSI: true})
	middle := strings.Split(got, "\n")[2]
	if !strings.Contains(middle, "\x1b[") {
		t.Fatalf("continuation line of a block comment was not styled: %q", middle)
	}
}

// TestStreamHighlightsAcrossWrites checks that highlighting survives deltas
// that split a code line, which is how the stream receives model output.
func TestStreamHighlightsAcrossWrites(t *testing.T) {
	opts := Options{Enabled: true, ANSI: true}
	want := Render("```go\nfunc main() {\n```\n", opts)

	stream := NewStream(opts)
	var got strings.Builder
	for _, delta := range []string{"```go\nfu", "nc ma", "in() {\n``", "`\n"} {
		got.WriteString(stream.Write(delta))
	}
	got.WriteString(stream.Flush())
	if got.String() != want {
		t.Fatalf("streamed render =\n%q\nwant\n%q", got.String(), want)
	}
}

func TestStreamBoundaryQueryPreservesHighlightedFenceState(t *testing.T) {
	stream := NewStream(Options{Enabled: true, ANSI: true})
	if got := stream.Write("```go\n"); got != "  ```go\n" {
		t.Fatalf("opening fence = %q", got)
	}
	if !stream.AtLineBoundary() {
		t.Fatal("complete opening fence should be a safe boundary")
	}
	if got := stream.Write("/* partial"); got != "" {
		t.Fatalf("partial highlighted line = %q, want buffered", got)
	}
	if stream.AtLineBoundary() {
		t.Fatal("incomplete highlighted code line reported as a safe boundary")
	}

	first := stream.Write("\n")
	if !stream.AtLineBoundary() {
		t.Fatal("newline-complete highlighted code line should be a safe boundary")
	}
	continued := stream.Write("continued */\n```\n")
	if !strings.Contains(first, "\x1b[") || !strings.Contains(continued, "\x1b[") {
		t.Fatalf("boundary query flushed or reset multiline highlight state: first=%q continued=%q", first, continued)
	}
	if got, want := stripANSI(first+continued), "  /* partial\n  continued */\n  ```\n"; got != want {
		t.Fatalf("highlighted fence source changed:\n got %q\nwant %q", got, want)
	}
}

func TestStreamBoundaryQueryDoesNotFlushPendingTextOrTable(t *testing.T) {
	stream := NewStream(Options{Enabled: true})
	if got := stream.Write("partial"); got != "" {
		t.Fatalf("partial write = %q, want buffered", got)
	}
	if stream.AtLineBoundary() {
		t.Fatal("incomplete source line reported as a safe boundary")
	}
	if got := stream.Write("\n| A | B |\n| --- | --- |\n"); got != "partial\n" {
		t.Fatalf("complete line/table write = %q, want only partial line", got)
	}
	if !stream.AtLineBoundary() {
		t.Fatal("newline-complete buffered table should be a safe boundary")
	}
	if got := stream.Flush(); !strings.Contains(got, "| A") {
		t.Fatalf("boundary query flushed or lost table: %q", got)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
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
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
