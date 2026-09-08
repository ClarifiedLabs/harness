package ui

import (
	"bytes"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/delegate"
	"harness/internal/markdown"
	"harness/internal/session"
)

const liveMermaidSource = "```mermaid\nflowchart TD\n  A[Start] --> B[End]\n"

func TestLiveMermaidNotFlushedByWaitCounter(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, LiveStatus: true})
	t.Cleanup(r.StopProgress)
	r.StartPrompt()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	for _, delta := range strings.SplitAfter(liveMermaidSource, "\n") {
		r.TextDelta(delta)
		r.renderMu.Lock()
		got, statusActive := out.String(), r.statusActive
		r.renderMu.Unlock()
		if got != "" || !statusActive {
			t.Fatalf("wait counter flushed diagram or stopped: stdout=%q active=%v", got, statusActive)
		}
	}
	r.TextDelta("```\n")
	r.StopProgress()
	want := markdown.Render(liveMermaidSource+"```\n", markdown.Options{Enabled: true})
	if got := out.String(); got != want || strings.Contains(got, "```") {
		t.Fatalf("live diagram = %q, want %q", got, want)
	}
	if !strings.Contains(errw.String(), "[turn: 1") {
		t.Fatalf("wait counter never painted: %q", errw.String())
	}
}

func TestStopProgressFlushesUnclosedMermaidOnce(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true})
	r.TextDelta(strings.TrimSuffix(liveMermaidSource, "\n"))
	r.StopProgress()
	r.StopProgress()
	want := markdown.Render(liveMermaidSource, markdown.Options{Enabled: true})
	if got := out.String(); got != want {
		t.Fatalf("unclosed diagram = %q, want %q", got, want)
	}
}

func TestDelegateLinesUseMermaidSourceBoundariesWithoutFlushing(t *testing.T) {
	var out, errw bytes.Buffer
	feed := delegate.NewActivityFeed()
	registry := delegate.NewActivityRegistry(feed)
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, DelegateFeed: feed})
	t.Cleanup(r.StopProgress)
	r.StartPrompt()
	r.TextDelta("```mermaid\nflowchart TD\n  A -->")
	registration := registry.Register(delegate.ActivityStart{ID: "child"})
	registration.Finish(session.ChildStatusCompleted, 1)
	r.drainActivity()
	if out.Len() != 0 || errw.Len() != 0 {
		t.Fatalf("incomplete source flushed: stdout=%q stderr=%q", out.String(), errw.String())
	}
	r.TextDelta(" B\n")
	r.drainActivity()
	if out.Len() != 0 || !strings.Contains(errw.String(), "[delegate d1 auto] completed") {
		t.Fatalf("delegate output should not flush complete Mermaid lines: stdout=%q stderr=%q", out.String(), errw.String())
	}
	r.TextDelta("```\n")
	r.StopProgress()
	if got := out.String(); strings.Contains(got, "```") || !strings.Contains(got, "| A |") {
		t.Fatalf("diagram lost after delegate output: %q", got)
	}
}

func TestMermaidNoticeDoesNotReopenClosingFence(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true})
	r.TextDelta(liveMermaidSource)
	r.Notice("[native steer applied]")
	r.TextDelta("B --> C\n```\n**after**\n")
	r.StopProgress()
	if got := out.String(); !strings.HasSuffix(got, "  B --> C\n  ```\nafter\n") {
		t.Fatalf("notice corrupted resumed fence: %q", got)
	}
}

func TestResponsiveMermaidDisplayPaths(t *testing.T) {
	input := "```mermaid\nflowchart LR\nA[Alpha] --> B[Beta] --> C[Gamma] --> D[Delta]\n```\n"
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, Width: func() int { return 40 }})
	want := markdown.Render(input, markdown.Options{Enabled: true, Width: 40})
	if got := r.FormatMarkdown(input); got != want {
		t.Fatalf("plan width not applied: %q, want %q", got, want)
	}
	for _, chunk := range strings.SplitAfter(input, "\n") {
		r.TextDelta(chunk)
	}
	r.StopProgress()
	if got := out.String(); got != want {
		t.Fatalf("live width not applied: %q, want %q", got, want)
	}
	out.Reset()
	want = "[reasoning]\n" + markdown.Render(input, markdown.Options{Enabled: true, Width: 40, Prefix: "  "}) + "[end reasoning]\n"
	r.ReasoningSummary(input)
	r.ReasoningSummaryStatus(input)
	if out.String() != want || errw.String() != want {
		t.Fatalf("reasoning width/prefix not applied: stdout=%q stderr=%q want=%q", out.String(), errw.String(), want)
	}
}

func TestMermaidMarkdownDisplayPaths(t *testing.T) {
	input := liveMermaidSource + "```\n"
	var out, errw bytes.Buffer
	raw := NewRenderer(&out, &errw, RenderOptions{})
	raw.TextDelta(input)
	raw.StopProgress()
	if out.String() != input {
		t.Fatalf("non-Markdown stdout changed: %q", out.String())
	}
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true})
	if got := r.FormatMarkdown(input); strings.Contains(got, "```") || !strings.Contains(got, "| Start |") {
		t.Fatalf("formatted plan lost diagram: %q", got)
	}
	out.Reset()
	r.ReasoningSummary(input)
	if got := out.String(); strings.Contains(got, "```") || !strings.Contains(got, "  | Start |") {
		t.Fatalf("reasoning summary lost diagram or prefix: %q", got)
	}
	r.ReasoningSummaryStatus(input)
	if got := errw.String(); strings.Contains(got, "```") || !strings.Contains(got, "  | Start |") {
		t.Fatalf("reasoning status lost diagram or prefix: %q", got)
	}
}
