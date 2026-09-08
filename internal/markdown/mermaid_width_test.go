package markdown

import (
	"fmt"
	"strings"
	"testing"

	"harness/internal/mermaid"
)

func TestMermaidWidthAndPrefix(t *testing.T) {
	for _, source := range []string{
		"flowchart LR\nA[Alpha] --> B[Beta] --> C[Gamma] --> D[Delta]\n",
		"flowchart TD\nA[A long label that needs wrapping to fit a narrow terminal]\n",
		"flowchart TD\nA -->|this edge label must not be clipped| B\n",
	} {
		for _, width := range []int{0, -1, 1, 8, 40, 80, 120} {
			for _, prefix := range []string{"", "> ", "\x1b[36m>\x1b[39m "} {
				t.Run(fmt.Sprintf("%s/%d/%q", source, width, prefix), func(t *testing.T) {
					available := width
					if available <= 0 {
						available = DefaultWidth
					}
					contentWidth := max(1, available-visibleLen(prefix))
					diagram, err := mermaid.Render(source, mermaid.Options{Width: contentWidth})
					if err != nil {
						t.Fatal(err)
					}
					var want strings.Builder
					for _, row := range strings.SplitAfter(diagram, "\n") {
						if row != "" && row != "\n" {
							want.WriteString(prefix)
						}
						want.WriteString(row)
					}
					got := Render("```mermaid\n"+source+"```\n", Options{Enabled: true, Width: width, Prefix: prefix})
					if got != want.String() {
						t.Fatalf("width/prefix not passed through:\n%s\nwant:\n%s", got, want.String())
					}
					for _, row := range strings.Split(got, "\n") {
						if visibleLen(row) > max(available, visibleLen(prefix)+1) {
							t.Fatalf("row exceeds width: %q", row)
						}
					}
				})
			}
		}
	}
}

func TestResponsiveMermaidStreamParity(t *testing.T) {
	// Multiple self-loops require a list even on wide displays. Labels must not
	// be reinterpreted as Markdown, and decoded terminal controls stay filtered.
	source := "flowchart LR\nA[\"**literal** &#27;[31m\"] -->|first| A\nA -.->|second| A\n"
	for _, width := range []int{8, 40, 120} {
		for _, ending := range []string{"```\n", "```", ""} {
			input := "```mermaid\n" + source + ending
			opts := Options{Enabled: true, ANSI: true, Width: width, Prefix: "  "}
			want := Render(input, opts)
			if strings.ContainsAny(want, "\x1b\a") || strings.Contains(want, "```mermaid") {
				t.Fatalf("list contained controls or fell back to source: %q", want)
			}
			if width >= 40 && (!strings.Contains(want, "Nodes:") || !strings.Contains(want, "**literal**") || !strings.Contains(want, "A -.-> A: second")) {
				t.Fatalf("list lost literal label or edge semantics: %q", want)
			}
			for _, size := range []int{1, 7, 32} {
				stream := NewStream(opts)
				var got strings.Builder
				for pos := 0; pos < len(input); pos += size {
					got.WriteString(stream.Write(input[pos:min(pos+size, len(input))]))
				}
				got.WriteString(stream.Flush())
				if got.String() != want || stream.LineOpen() != !strings.HasSuffix(input, "\n") || stream.HasBufferedBlock() || stream.Flush() != "" {
					t.Fatalf("width %d chunk %d ending %q lost stream state or output", width, size, ending)
				}
			}
		}
	}
}

func TestResponsiveMermaidLimitsStillShowSource(t *testing.T) {
	var source strings.Builder
	source.WriteString("flowchart TD\n")
	for i := 0; i < 46; i++ {
		fmt.Fprintf(&source, "N%d --> N%d\n", i, i+1)
	}
	for i := 2; i < 47; i++ {
		fmt.Fprintf(&source, "N0 --> N%d\n", i)
	}
	// This small source needs 1,035 virtual layout nodes, over Harness's cap.
	for _, width := range []int{8, 80, 120} {
		opts := Options{Enabled: true, Width: width}
		input := "```mermaid\n" + source.String() + "```\n"
		want := strings.Replace(Render(strings.Replace(input, "```mermaid", "```text", 1), opts), "```text", "```mermaid", 1)
		if got := Render(input, opts); got != want {
			t.Fatalf("width %d hid the resource limit with a list", width)
		}
	}
}
