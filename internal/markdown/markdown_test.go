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
	got := Render("Use **bold**, *italic*, and ***both***.", Options{Enabled: true})
	want := "Use bold, italic, and both."
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestRenderAppliesANSIEmphasisAndHeadings(t *testing.T) {
	got := Render("# Title\nUse **bold** and *italic*.", Options{Enabled: true, ANSI: true})
	for _, want := range []string{
		ansiBold + "# Title" + ansiReset,
		ansiBold + "bold" + ansiReset,
		ansiItalic + "italic" + ansiReset,
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
	wantANSI := ansiLink + "https://example.com" + ansiReset + "."
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
	want := "Use " + ansiCode + "foo" + ansiReset + " here."
	if got != want {
		t.Fatalf("inline code ANSI = %q, want %q", got, want)
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
