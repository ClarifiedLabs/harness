package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/mdcli/markdown"
)

func TestMarkdownDependencyPreservesLiveLayoutBudget(t *testing.T) {
	// This source needs 1,035 virtual nodes: legal for mdcli's default 8,192,
	// but too expensive for Harness's synchronous display budget of 1,024.
	var source strings.Builder
	source.WriteString("```mermaid\nflowchart TD\n")
	for i := 0; i < 46; i++ {
		fmt.Fprintf(&source, "N%d --> N%d\n", i, i+1)
	}
	for i := 2; i < 47; i++ {
		fmt.Fprintf(&source, "N0 --> N%d\n", i)
	}
	source.WriteString("```\n")
	input := source.String()
	if got := markdown.Render(input, markdown.Options{Enabled: true, Width: 80}); strings.Contains(got, "```mermaid") {
		t.Fatal("fixture must render under mdcli's default budget")
	}
	want := markdown.Render(input, markdown.Options{Enabled: true, Width: 80, MermaidMaxVirtualNodes: 1024})
	if !strings.Contains(want, "```mermaid") {
		t.Fatal("fixture must fall back under Harness's budget")
	}

	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true})
	if got := r.FormatMarkdown(input); got != want {
		t.Fatalf("complete Markdown lost layout budget: %q", got)
	}
	for _, chunk := range strings.SplitAfter(input, "\n") {
		r.TextDelta(chunk)
	}
	r.StopProgress()
	if got := out.String(); got != want {
		t.Fatalf("streamed Markdown lost layout budget: %q", got)
	}
	out.Reset()
	r.ReasoningSummary(input)
	r.ReasoningSummaryStatus(input)
	for name, got := range map[string]string{"reasoning stdout": out.String(), "reasoning stderr": errw.String()} {
		if !strings.Contains(got, "    ```mermaid\n") {
			t.Errorf("%s lost layout budget or indentation: %q", name, got)
		}
	}
}
