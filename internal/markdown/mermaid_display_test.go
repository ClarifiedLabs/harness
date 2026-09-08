package markdown

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"harness/internal/mermaid"
)

func TestMermaidUnicodePrefixWidth(t *testing.T) {
	const source = "flowchart TD\nA[abcd]\n"
	for _, tt := range []struct {
		prefix  string
		columns int
	}{
		{"界界", 4},
		{"e\u0301 ", 2},
		{"\x1b[36m界\x1b[39m", 2},
	} {
		got := Render("```mermaid\n"+source+"```\n", Options{Enabled: true, Width: 10, Prefix: tt.prefix})
		want, err := mermaid.Render(source, mermaid.Options{Width: 10 - tt.columns})
		if err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		for _, row := range strings.SplitAfter(got, "\n") {
			if row == "" || row == "\n" {
				body.WriteString(row)
				continue
			}
			if !strings.HasPrefix(row, tt.prefix) {
				t.Fatalf("prefix missing: %q", row)
			}
			content := strings.TrimPrefix(row, tt.prefix)
			// Diagram content is ASCII here; prefix columns are independently
			// specified so the test doesn't repeat the implementation's width bug.
			if tt.columns+utf8.RuneCountInString(strings.TrimSuffix(content, "\n")) > 10 {
				t.Fatalf("row exceeds ten columns: %q", row)
			}
			body.WriteString(content)
		}
		if body.String() != want {
			t.Fatalf("prefix %q used wrong column budget:\n%s\nwant:\n%s", tt.prefix, body.String(), want)
		}
	}
}

func TestMermaidBidiControlsFiltered(t *testing.T) {
	for _, control := range []rune{'\u061c', '\u200e', '\u200f', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069'} {
		for _, label := range []string{string(control), fmt.Sprintf("&#%d;", control)} {
			for _, source := range []string{
				"flowchart TD\nA[\"safe" + label + "label\"]\n",
				"flowchart TD\nA[\"safe" + label + "label\"] --> A\nA -.-> A\n",
				"unsupported\n" + label + "\n",
			} {
				input := "```mermaid\n" + source + "```\n"
				got := Render(input, Options{Enabled: true, Width: 80})
				if strings.ContainsFunc(got, func(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }) {
					t.Fatalf("bidi control survived display filtering: %q", got)
				}
				if raw := Render(input, Options{}); raw != input {
					t.Fatal("display filtering changed raw source")
				}
			}
		}
	}
}
