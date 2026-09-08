package markdown

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"harness/internal/mermaid"
	"harness/internal/term/highlight"
)

const mermaidFlow = "flowchart TD\n  A[Start] --> B[End]\n"

func TestRenderMermaidDiagrams(t *testing.T) {
	for _, source := range []string{
		mermaidFlow,
		"sequenceDiagram\nAlice->>Bob: Hello\n",
		"stateDiagram-v2\n[*] --> Running\nRunning --> [*]\n",
		"classDiagram\nAnimal <|-- Duck\n",
	} {
		want, err := mermaid.Render(source, mermaid.Options{Width: DefaultWidth})
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{"```", "~~~"} {
			for _, ansi := range []bool{false, true} {
				opts := Options{Enabled: true, ANSI: ansi, ColorTheme: highlight.ThemeLight}
				input := marker + "MeRmAiD\n" + source + marker + "\n"
				if got := Render(input, opts); got != want {
					t.Errorf("Render(%q) =\n%s\nwant\n%s", input, got, want)
				}
			}
		}
	}
}

func TestStreamBuffersMermaidUntilClosingFence(t *testing.T) {
	opts := Options{Enabled: true}
	stream := NewStream(opts)
	if stream.HasBufferedBlock() {
		t.Fatal("new stream has buffered block")
	}
	for _, delta := range []string{"```mer", "maid\n", "flowchart TD\n", "  A[Start] -->", " B[End]\n", "``"} {
		if got := stream.Write(delta); got != "" {
			t.Fatalf("Write(%q) emitted incomplete diagram: %q", delta, got)
		}
	}
	if !stream.HasBufferedBlock() || stream.AtLineBoundary() {
		t.Fatal("split closing fence should be buffered, not at a line boundary")
	}
	want := Render("```mermaid\n"+mermaidFlow+"```\n", opts)
	if got := stream.Write("`\n"); got != want {
		t.Fatalf("closed diagram = %q, want %q", got, want)
	}
	if stream.HasBufferedBlock() || !stream.AtLineBoundary() || stream.LineOpen() {
		t.Fatal("closed diagram left buffered or open line state")
	}
	if got := stream.Flush(); got != "" {
		t.Fatalf("second flush duplicated diagram: %q", got)
	}
}

func TestStreamMermaidChunkAndNewlineParity(t *testing.T) {
	for _, ending := range []string{"```\n", "```", "", "  B --> C"} {
		input := "```mermaid\n" + mermaidFlow + ending
		want := Render(input, Options{Enabled: true})
		for size := 1; size <= len(input); size++ {
			stream := NewStream(Options{Enabled: true})
			var out strings.Builder
			for pos := 0; pos < len(input); pos += size {
				out.WriteString(stream.Write(input[pos:min(pos+size, len(input))]))
			}
			out.WriteString(stream.Flush())
			if out.String() != want {
				t.Fatalf("chunk size %d ending %q: got %q, want %q", size, ending, out.String(), want)
			}
			if stream.LineOpen() != !strings.HasSuffix(input, "\n") {
				t.Fatalf("incorrect LineOpen for ending %q", ending)
			}
			if stream.HasBufferedBlock() || stream.Flush() != "" {
				t.Fatal("Flush must consume the diagram exactly once")
			}
		}
	}
}

func TestStreamMermaidContinuesAsCodeAfterBoundaryFlush(t *testing.T) {
	for _, source := range []string{mermaidFlow, "unsupported diagram\n"} {
		stream := NewStream(Options{Enabled: true})
		stream.Write("```mermaid\n" + source)
		if got := stream.Flush(); got == "" {
			t.Fatal("boundary flush lost buffered diagram")
		}
		if got := stream.Flush(); got != "" || stream.HasBufferedBlock() {
			t.Fatalf("boundary flush did not consume buffer exactly once: %q", got)
		}
		got := stream.Write("B --> C\x1b[0m\n```\n**after**\n")
		want := "  B --> C[0m\n  ```\nafter\n"
		if got != want {
			t.Fatalf("resumed fence = %q, want %q", got, want)
		}
	}
}

func TestStreamMermaidBoundaryDoesNotFlush(t *testing.T) {
	stream := NewStream(Options{Enabled: true})
	stream.Write("```mermaid\nflowchart TD\n  A -->")
	if stream.AtLineBoundary() || !stream.HasBufferedBlock() {
		t.Fatal("incomplete source line must be buffered and unsafe")
	}
	if got := stream.Write(" B\n"); got != "" {
		t.Fatalf("body emitted before closing fence: %q", got)
	}
	if !stream.AtLineBoundary() || !stream.HasBufferedBlock() {
		t.Fatal("complete source line is safe for external output without flushing")
	}
	if got := stream.Write("```\n"); !strings.Contains(got, "| A |") || strings.Contains(got, "```") {
		t.Fatalf("boundary query lost diagram: %q", got)
	}
}

func TestStreamMermaidSourceBudget(t *testing.T) {
	base := "flowchart TD\nA\n%%"
	source := base + strings.Repeat("x", mermaid.MaxSourceBytes-len(base)-1) + "\n"
	stream := NewStream(Options{Enabled: true})
	if got := stream.Write("```mermaid\n" + source); got != "" || !stream.HasBufferedBlock() {
		t.Fatalf("source at limit should remain buffered: %q", got)
	}
	if got := stream.Write("```\n"); strings.Contains(got, "```") || !strings.Contains(got, "| A |") {
		t.Fatalf("source at limit should render: %q", got)
	}

	stream.Write("```mermaid\n" + source)
	got := stream.Write("%%overflow\n")
	if stream.HasBufferedBlock() || stream.diagramBytes != 0 || !strings.HasPrefix(got, "  ```mermaid\n") {
		t.Fatal("oversized source was not released as a code fence")
	}
	var want strings.Builder
	for _, line := range strings.SplitAfter("```mermaid\n"+source+"%%overflow\n", "\n") {
		if line != "" {
			want.WriteString("  " + line)
		}
	}
	if got != want.String() {
		t.Fatal("oversized fallback lost source")
	}
	if got := stream.Write("```\n**after**\n"); got != "  ```\nafter\n" {
		t.Fatalf("oversized fence lost its closer: %q", got)
	}
	if got := stream.Write("```mermaid\n" + mermaidFlow + "```\n"); strings.Contains(got, "```") || !strings.Contains(got, "| Start |") {
		t.Fatalf("budget state leaked into next diagram: %q", got)
	}
}

func TestStreamMermaidBoundsIncompleteLines(t *testing.T) {
	for _, ending := range []string{"", "```\n~~~\n```\n**after**\n"} {
		stream := NewStream(Options{Enabled: true})
		var out strings.Builder
		out.WriteString(stream.Write("```mermaid\nflowchart TD\n"))
		body := "A[" + strings.Repeat("界", mermaid.MaxSourceBytes) + "]"
		for pos := 0; pos < len(body); pos += 1024 {
			out.WriteString(stream.Write(body[pos:min(pos+1024, len(body))]))
			if len(stream.pending) > mermaid.MaxSourceBytes {
				t.Fatalf("newline-free source retained %d bytes", len(stream.pending))
			}
		}
		if out.Len() == 0 || stream.HasBufferedBlock() {
			t.Fatal("newline-free source did not fall back before completion")
		}
		out.WriteString(stream.Write(ending))
		out.WriteString(stream.Flush())
		want := "  ```mermaid\n  flowchart TD\n  " + body
		if ending != "" {
			// The first triple-backtick fragment is still part of the long
			// source line; only the later standalone delimiter closes it.
			want += "```\n  ~~~\n  ```\nafter\n"
		}
		if got := out.String(); got != want || !utf8.ValidString(got) {
			t.Fatal("fragmented fallback lost text, UTF-8, indentation, or fence state")
		}
		if stream.LineOpen() != (ending == "") {
			t.Fatal("fragmented fallback lost final newline state")
		}
	}
}

func TestRenderMermaidExpandedBudgetFallback(t *testing.T) {
	input := "```mermaid\nflowchart TD\n" + strings.Repeat("A --> B\n", 1025) + "```\n"
	want := strings.Replace(Render(strings.Replace(input, "```mermaid", "```text", 1), Options{Enabled: true}), "```text", "```mermaid", 1)
	if got := Render(input, Options{Enabled: true}); got != want {
		t.Fatalf("oversized expansion should retain source: got %d bytes, want %d", len(got), len(want))
	}
}

func TestRenderMermaidFallback(t *testing.T) {
	for _, source := range []string{"", "not valid mermaid\n", "pie\n  \"Cats\" : 5\n", "flowchart TD\n"} {
		for _, ending := range []string{"```\n", "```", ""} {
			input := "```mermaid\n" + source + ending
			var want strings.Builder
			for _, line := range strings.SplitAfter(input, "\n") {
				if line != "" {
					want.WriteString(">   " + line)
				}
			}
			if got := Render(input, Options{Enabled: true, ANSI: true, Prefix: "> "}); got != want.String() {
				t.Errorf("fallback for %q = %q, want %q", input, got, want.String())
			}
		}
	}
}

func TestRenderMermaidPreservesSurroundingsPrefixAndLayout(t *testing.T) {
	source := "---\ntitle: Flow\n---\n" + mermaidFlow
	diagram, err := mermaid.Render(source, mermaid.Options{Width: 3})
	if err != nil {
		t.Fatal(err)
	}
	var prefixed strings.Builder
	for _, row := range strings.SplitAfter(diagram, "\n") {
		if row != "" && row != "\n" {
			prefixed.WriteString("> ")
		}
		prefixed.WriteString(row)
	}
	opts := Options{Enabled: true, Prefix: "> ", Width: 5}
	before := "# Title\n| Name |\n| --- |\n| value |\n"
	after := "**after**\n```go\nfunc main() {}\n```\n"
	input := before + "```mermaid\n" + source + "```\n" + after
	want := Render(before, opts) + prefixed.String() + Render(after, opts)
	if got := Render(strings.ReplaceAll(input, "\n", "\r\n"), opts); got != want {
		t.Fatalf("surrounding Markdown, CRLF, or diagram layout changed:\n%q\nwant\n%q", got, want)
	}
}

func TestRenderMermaidOnlyTaggedFences(t *testing.T) {
	for _, info := range []string{"", "text", "mermaid extra", "{.mermaid}"} {
		input := "```" + info + "\n" + mermaidFlow + "```\n"
		got := Render(input, Options{Enabled: true})
		if !strings.Contains(got, "  flowchart TD\n") || !strings.Contains(got, "  ```"+info+"\n") {
			t.Errorf("non-Mermaid info %q changed: %q", info, got)
		}
	}
	input := "~~~text\n```mermaid\n" + mermaidFlow + "```\n~~~\n"
	if got := Render(input, Options{Enabled: true}); !strings.Contains(got, "  ```mermaid\n") {
		t.Fatalf("nested fence rendered as Mermaid: %q", got)
	}
}

func TestRenderMermaidDisabledIsRaw(t *testing.T) {
	input := "```mermaid\r\n" + mermaidFlow + "```"
	opts := Options{ANSI: true, Prefix: "> ", Width: 5}
	if got := Render(input, opts); got != input {
		t.Fatalf("disabled Render changed source: %q", got)
	}
	stream := NewStream(opts)
	if got := stream.Write(input) + stream.Flush(); got != input {
		t.Fatalf("disabled Stream changed source: %q", got)
	}
}

func TestRenderMermaidStripsTerminalControls(t *testing.T) {
	for _, source := range []string{
		"flowchart TD\nA[\x1b[31mRed\x1b[0m]\n",
		"---\ntitle: \x1b]0;title\a\n---\n" + mermaidFlow,
		"invalid \x1b[31mred\x1b[0m\n",
	} {
		for _, ansi := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/ansi=%v", source, ansi), func(t *testing.T) {
				got := Render("```mermaid\n"+source+"```\n", Options{Enabled: true, ANSI: ansi})
				if strings.ContainsAny(got, "\x1b\a") {
					t.Fatalf("terminal controls survived: %q", got)
				}
			})
		}
	}
}
