package markdown

import (
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestRenderCitationSources(t *testing.T) {
	sources := llm.FormatCitationSources([]llm.URLCitation{{URL: "https://example.com/a(b)?q=a b", Title: "**Example**\n[title]\x1b"}})
	if strings.Contains(sources, "\x1b") {
		t.Fatal("citation text contains ANSI")
	}
	if got := Render(sources, Options{}); got != sources {
		t.Fatalf("raw text changed: %q", got)
	}
	for _, ansi := range []bool{false, true} {
		opts := Options{Enabled: true, ANSI: ansi}
		got := Render(sources, opts)
		for _, want := range []string{"Sources:", "https://example.com/a%28b%29?q=a%20b", "Example title"} {
			if !strings.Contains(got, want) {
				t.Fatalf("rendered sources missing %q: %q", want, got)
			}
		}
		if !ansi && strings.Contains(got, "\x1b") {
			t.Fatalf("ANSI in plain render: %q", got)
		}
		for split := 0; split <= len(sources); split++ {
			stream := NewStream(opts)
			streamed := stream.Write(sources[:split]) + stream.Write(sources[split:]) + stream.Flush()
			if streamed != got {
				t.Fatalf("split %d render = %q, want %q", split, streamed, got)
			}
		}
	}
}
