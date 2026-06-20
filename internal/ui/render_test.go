package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
)

// fixedClock returns successive instants spaced by step, so duration math in the
// usage line is deterministic without sleeping (design §13).
func fixedClock(start time.Time, step time.Duration) func() time.Time {
	t := start
	first := true
	return func() time.Time {
		if first {
			first = false
			return t
		}
		t = t.Add(step)
		return t
	}
}

func TestToolSummaryLine(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.ToolStart(llm.ToolCall{
		ID:    "c1",
		Name:  "grep",
		Input: json.RawMessage(`{"args":["-R","-n","func main","."]}`),
	})
	r.ToolResult(llm.ToolResult{
		ForID: "c1",
		Text:  "a.go:1:func main\nb.go:2:func main\n",
	})

	got := errw.String()
	if out.Len() != 0 {
		t.Errorf("tool lines must go to errw, not out; out=%q", out.String())
	}
	if !strings.Contains(got, "[tool: grep started") {
		t.Errorf("tool start should be reported, got %q", got)
	}
	if !strings.Contains(got, "[grep]") {
		t.Errorf("summary should include [grep], got %q", got)
	}
	if !strings.Contains(got, `args=["-R","-n","func main","."]`) {
		t.Errorf("summary should show argv-style args, got %q", got)
	}
	if !strings.Contains(got, "→") {
		t.Errorf("summary should show the arrow separator, got %q", got)
	}
	if !strings.Contains(got, "2 lines") {
		t.Errorf("summary should report 2 lines, got %q", got)
	}
}

func TestToolSummaryErrorMarked(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})
	r.ToolStart(llm.ToolCall{ID: "e1", Name: "edit", Input: json.RawMessage(`{"path":"x"}`)})
	r.ToolResult(llm.ToolResult{ForID: "e1", Text: "error: files is required", IsError: true})

	got := errw.String()
	if !strings.Contains(got, "error") {
		t.Errorf("error result should surface the error text, got %q", got)
	}
}

func TestToolSummaryFinishesAssistantLine(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TextDelta("calling a tool")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "list_dir", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})

	if got := out.String(); got != "calling a tool\n" {
		t.Errorf("tool summary should force a newline after assistant text, got %q", got)
	}
	if got := errw.String(); !strings.Contains(got, "[list_dir]") {
		t.Errorf("tool summary should still go to errw, got %q", got)
	}
}

func TestToolSummaryDoesNotDoubleSpaceAfterAssistantNewline(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TextDelta("calling a tool\n")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "list_dir", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})

	if got := out.String(); got != "calling a tool\n" {
		t.Errorf("tool summary should not add a second newline, got %q", got)
	}
}

func TestVerboseAddsSnippet(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Verbose: true})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)})
	body := "line1\nline2\nline3\nline4\nline5\nline6\nline7\n"
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: body})

	got := errw.String()
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line5") {
		t.Errorf("verbose should include the first ~5 lines, got %q", got)
	}
	if strings.Contains(got, "line6") {
		t.Errorf("verbose should cap the snippet at ~5 lines, got %q", got)
	}
}

func TestQuietSuppressesStatusButKeepsUsage(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Quiet: true, ToolStream: true})

	// Progress noise (ModelTurnStart, tool lines, notices) is suppressed, but
	// -quiet alone still prints the single per-turn usage line (r25).
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	r.ToolUseStart(llm.ToolCall{ID: "c1", Name: "read_file"})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "package main\n"})
	r.Notice("[something happened]")
	r.StartTurn()
	r.TurnComplete(agent.TurnUsage{})

	got := errw.String()
	if strings.Contains(got, "waiting") || strings.Contains(got, "read_file") || strings.Contains(got, "something happened") {
		t.Errorf("quiet mode should suppress progress lines, got %q", got)
	}
	if !strings.Contains(got, "[turn:") {
		t.Errorf("quiet mode should still print the per-turn usage line (r25), got %q", got)
	}

	// Assistant text must still flow to out.
	r.TextDelta("hello")
	if out.String() != "hello" {
		t.Errorf("quiet mode: assistant text = %q, want %q", out.String(), "hello")
	}
}

func TestSuppressUsageSilencesEverything(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Quiet: true, SuppressUsage: true})

	// A fully silent piped run: even the usage line is dropped (r25).
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	r.StartTurn()
	r.TurnComplete(agent.TurnUsage{})

	if errw.Len() != 0 {
		t.Errorf("SuppressUsage should silence errw entirely, got %q", errw.String())
	}
}

func TestToolDiffWritesToErr(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TextDelta("partial")
	r.ToolDiff(llm.ToolCall{ID: "c1", Name: "edit"}, "--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n-old\n+new\n")

	if got := out.String(); got != "partial\n" {
		t.Fatalf("ToolDiff should finish assistant line, got out=%q", got)
	}
	got := errw.String()
	if !strings.Contains(got, "--- a/f.txt") || !strings.Contains(got, "-old\n+new\n") {
		t.Fatalf("ToolDiff missing diff text:\n%s", got)
	}
}

func TestToolDiffColor(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Color: true})

	r.ToolDiff(llm.ToolCall{ID: "c1", Name: "edit"}, "--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n-old\n+new\n")

	got := errw.String()
	if !strings.Contains(got, "\x1b[31m-old") || !strings.Contains(got, "\x1b[32m+new") || !strings.Contains(got, "\x1b[36m@@") {
		t.Fatalf("colored diff missing ANSI styling:\n%q", got)
	}
}

func TestUsageLineKnownModelShowsCost(t *testing.T) {
	var out, errw bytes.Buffer
	start := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	r := NewRenderer(&out, &errw, RenderOptions{
		Model: "claude-opus-4-8",
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{
			"claude-opus-4-8": {
				ContextWindow: 1_000_000,
				Price:         llm.Price{Input: 5.0, Output: 25.0},
			},
		}),
		Now: fixedClock(start, 4300*time.Millisecond),
	})
	r.StartTurn()
	r.TurnComplete(agent.TurnUsage{
		ModelTurns: 3,
		Usage:      llm.Usage{InputTokens: 12400, OutputTokens: 1800},
	})

	got := errw.String()
	if out.Len() != 0 {
		t.Errorf("TurnComplete should not write a newline before usage with no assistant text, got out=%q", out.String())
	}
	if !strings.Contains(got, "[turn:") {
		t.Errorf("usage line should be bracketed, got %q", got)
	}
	if !strings.Contains(got, "3 model turns") {
		t.Errorf("usage line should show model-turn count, got %q", got)
	}
	if !strings.Contains(got, "12.4k (12.4k) in") || !strings.Contains(got, "1.8k (1.8k) out") {
		t.Errorf("usage line should show per-turn (cumulative) token counts, got %q", got)
	}
	if !strings.Contains(got, "$") {
		t.Errorf("known model should show a cost, got %q", got)
	}
	// Both per-turn and cumulative cost should appear (parenthesised cumulative).
	if !strings.Contains(got, "($") {
		t.Errorf("usage line should show cumulative cost in parens, got %q", got)
	}
	if !strings.Contains(got, "4.3s") {
		t.Errorf("usage line should show elapsed duration, got %q", got)
	}
}

func TestUsageLineUnknownModelOmitsCost(t *testing.T) {
	var out, errw bytes.Buffer
	start := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	r := NewRenderer(&out, &errw, RenderOptions{
		Model: "some-local-llama",
		Now:   fixedClock(start, time.Second),
	})
	r.StartTurn()
	r.TurnComplete(agent.TurnUsage{ModelTurns: 1, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}})

	got := errw.String()
	if strings.Contains(got, "$") {
		t.Errorf("unknown model must omit cost, got %q", got)
	}
}

func TestColorSuppressedWhenNotTTY(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Color: false})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "list_dir", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})
	if strings.Contains(errw.String(), "\x1b[") {
		t.Errorf("no ANSI escapes when color disabled, got %q", errw.String())
	}
}

func TestColorEmittedWhenEnabled(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Color: true})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "list_dir", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})
	if !strings.Contains(errw.String(), "\x1b[") {
		t.Errorf("expected ANSI dim escapes when color enabled, got %q", errw.String())
	}
}

func TestTurnCompleteWritesTrailingNewline(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})
	r.StartTurn()
	r.TextDelta("hello world")
	r.TurnComplete(agent.TurnUsage{ModelTurns: 1})

	got := out.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("TurnComplete should write a trailing newline to out, got %q", got)
	}
	// The trailing newline should appear after the text.
	if !strings.Contains(got, "hello world\n") {
		t.Errorf("trailing newline must come after assistant text, got %q", got)
	}
}

func TestTextDeltaGoesToStdout(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})
	r.TextDelta("hello ")
	r.TextDelta("world")
	if out.String() != "hello world" {
		t.Errorf("assistant text should stream raw to out, got %q", out.String())
	}
	if errw.Len() != 0 {
		t.Errorf("assistant text must not touch errw, got %q", errw.String())
	}
}

func TestFinalAnswerSeparatorBetweenCommentaryAndFinal(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.AssistantPhase(llm.AssistantPhaseCommentary)
	r.TextDelta("I have enough to answer.")
	r.AssistantPhase(llm.AssistantPhaseFinal)
	r.TextDelta("Yes, with limits.")

	want := "I have enough to answer.\n\n---\n\nYes, with limits."
	if out.String() != want {
		t.Fatalf("assistant text = %q, want %q", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Fatalf("phase separator should not touch stderr, got %q", errw.String())
	}
}

func TestFinalAnswerSeparatorNotInsertedForFinalOnly(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.AssistantPhase(llm.AssistantPhaseFinal)
	r.TextDelta("Yes, with limits.")

	if out.String() != "Yes, with limits." {
		t.Fatalf("assistant text = %q", out.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("final-only text should not touch stderr, got %q", errw.String())
	}
}

func TestFinalAnswerSeparatorOnlyOnce(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.AssistantPhase(llm.AssistantPhaseCommentary)
	r.TextDelta("I have enough to answer.")
	r.AssistantPhase(llm.AssistantPhaseFinal)
	r.TextDelta("Yes")
	r.TextDelta(", with limits.")

	if got := strings.Count(out.String(), "\n---\n"); got != 1 {
		t.Fatalf("separator count = %d, output = %q", got, out.String())
	}
	if !strings.HasSuffix(out.String(), "Yes, with limits.") {
		t.Fatalf("final text was not preserved, got %q", out.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("phase separator should not touch stderr, got %q", errw.String())
	}
}

func TestTextDeltaRendersMarkdownWhenEnabled(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true})

	r.TextDelta("Use **bold** and [docs](https://example.com)")
	r.finishAssistantLine()

	want := "Use bold and docs <https://example.com>\n"
	if out.String() != want {
		t.Fatalf("markdown assistant text = %q, want %q", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("assistant text must not touch errw, got %q", errw.String())
	}
}

func TestMarkdownAssistantFlushesBeforeStatusLine(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true})

	r.TextDelta("calling **tool**")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "list_dir", Input: json.RawMessage(`{"path":"."}`)})

	if got := out.String(); got != "calling tool\n" {
		t.Fatalf("assistant markdown should flush before status, got %q", got)
	}
	if got := errw.String(); !strings.Contains(got, "[tool: list_dir started path=.]") {
		t.Fatalf("status line missing after markdown flush, got %q", got)
	}
}

func TestModelTurnStartGoesToStderr(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.ModelTurnStart(2, 1, agent.ContextEstimate{})
	r.ModelTurnStart(2, 3, agent.ContextEstimate{})

	if out.Len() != 0 {
		t.Errorf("model progress must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	if !strings.Contains(got, "[model: turn 2 waiting]") {
		t.Errorf("missing model-turn wait line, got %q", got)
	}
	if !strings.Contains(got, "[model: turn 2 retry 2 waiting]") {
		t.Errorf("missing retry wait line, got %q", got)
	}
}

func TestModelTurnCompleteShowsCostCheckpoints(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Model: "priced-model",
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{
			"priced-model": {
				Price: llm.Price{Input: 10, Output: 20},
			},
		}),
	})
	r.SetCumulativeUsage(0, 0, 1.25)
	r.StartTurn()

	r.ModelTurnComplete(agent.ModelTurnUsage{
		ModelTurn: 1,
		Attempt:   1,
		Usage:     llm.Usage{InputTokens: 100_000, OutputTokens: 50_000},
	})
	r.ModelTurnComplete(agent.ModelTurnUsage{
		ModelTurn: 2,
		Attempt:   1,
		Usage:     llm.Usage{InputTokens: 50_000},
	})

	if out.Len() != 0 {
		t.Errorf("model cost progress must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	for _, want := range []string{
		"[model: turn 1 cost: $2.0000 · totals: $2.0000 prompt · $3.2500 session]",
		"[model: turn 2 cost: $0.5000 · totals: $2.5000 prompt · $3.7500 session]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing cost checkpoint %q:\n%s", want, got)
		}
	}
}

func TestModelTurnCompletePrintsCostBeforeToolUseProgress(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Model:      "priced-model",
		ToolStream: true,
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{
			"priced-model": {
				Price: llm.Price{Input: 10},
			},
		}),
	})
	r.StartTurn()

	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read_file"})
	if strings.Contains(errw.String(), "[tool-call:") {
		t.Fatalf("tool-call progress should wait for model cost, got:\n%s", errw.String())
	}
	r.ModelTurnComplete(agent.ModelTurnUsage{
		ModelTurn: 1,
		Attempt:   1,
		Usage:     llm.Usage{InputTokens: 100_000},
	})

	if out.Len() != 0 {
		t.Errorf("model and tool-call progress must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	waiting := strings.Index(got, "[model: turn 1 waiting]")
	cost := strings.Index(got, "[model: turn 1 cost: $1.0000")
	toolCall := strings.Index(got, "[tool-call: read_file id=call_1]")
	if waiting < 0 || cost < 0 || toolCall < 0 {
		t.Fatalf("missing expected progress lines:\n%s", got)
	}
	if !(waiting < cost && cost < toolCall) {
		t.Fatalf("progress order =\n%s\nwant waiting, cost, then tool-call", got)
	}
}

func TestModelTurnCompleteUnknownModelOmitsCostButWarnsOnce(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Model:    "unknown-model",
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{}),
	})
	r.StartTurn()
	r.ModelTurnComplete(agent.ModelTurnUsage{
		ModelTurn: 1,
		Attempt:   1,
		Usage:     llm.Usage{InputTokens: 100, OutputTokens: 10},
	})
	r.ModelTurnComplete(agent.ModelTurnUsage{
		ModelTurn: 2,
		Attempt:   1,
		Usage:     llm.Usage{InputTokens: 50, OutputTokens: 5},
	})

	if out.Len() != 0 {
		t.Errorf("unknown model cost should not write to out, out=%q", out.String())
	}
	got := errw.String()
	// No dollar figure is ever shown for an unpriced model...
	if strings.Contains(got, "$") {
		t.Errorf("unknown model must not print a cost, got %q", got)
	}
	// ...but the one-time no-price notice is emitted exactly once (r16).
	if n := strings.Count(got, "no price configured"); n != 1 {
		t.Errorf("no-price notice should appear exactly once, got %d in %q", n, got)
	}
}

func TestTimestampsOnlyBracketedStatusLines(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Now:             func() time.Time { return time.Date(2026, 6, 13, 16, 15, 34, 0, time.Local) },
		TimestampLayout: TimestampShortLayout,
		ToolStream:      true,
	})

	r.TextDelta("plain assistant text\n")
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read_file"})
	r.Notice("unbracketed notice")
	r.Notice("[bracketed notice]")

	if out.String() != "plain assistant text\n" {
		t.Fatalf("assistant text should stay raw, got %q", out.String())
	}
	got := errw.String()
	for _, want := range []string{
		"[16:15:34 model: turn 1 waiting]",
		"[16:15:34 tool-call: read_file id=call_1]",
		"unbracketed notice\n",
		"[16:15:34 bracketed notice]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "16:15:34 unbracketed notice") {
		t.Errorf("unbracketed dim lines should not be timestamped:\n%s", got)
	}
}

func TestReasoningSummaryRendersTimestampedIndentedToStdout(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Color:           true,
		Now:             func() time.Time { return time.Date(2026, 6, 13, 16, 15, 34, 0, time.Local) },
		TimestampLayout: TimestampShortLayout,
	})

	r.ReasoningSummary("**Exploring context usage reduction**\n\nInspecting [schemas](https://foo.example.com/docs)")

	got := out.String()
	want := "[16:15:34 reasoning]\n" +
		"  \x1b[1mExploring context usage reduction" + ansiReset + "\n" +
		"\n" +
		"  Inspecting schemas <\x1b[36;4mhttps://foo.example.com/docs" + ansiReset + ">\n" +
		"[end reasoning]\n"
	if got != want {
		t.Fatalf("reasoning summary output mismatch:\nwant %q\n got %q", want, got)
	}
	if strings.Contains(got, "[reasoning] Inspecting") {
		t.Fatalf("reasoning summary should not be continuation-prefixed:\n%s", got)
	}
	if errw.Len() != 0 {
		t.Fatalf("interactive reasoning summary should not write stderr, got %q", errw.String())
	}
}

func TestReasoningSummaryStatusRendersTimestampedToStderr(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Now:             func() time.Time { return time.Date(2026, 6, 13, 16, 15, 34, 0, time.Local) },
		TimestampLayout: TimestampShortLayout,
		Width:           func() int { return 38 },
	})

	r.ReasoningSummaryStatus("Checking defaults\n- Pick a narrow implementation path for readability")

	if out.Len() != 0 {
		t.Fatalf("status reasoning summary should not write stdout, got %q", out.String())
	}
	got := errw.String()
	want := "[16:15:34 reasoning]\n" +
		"  Checking defaults\n" +
		"  - Pick a narrow implementation path\n" +
		"    for readability\n" +
		"[end reasoning]\n"
	if got != want {
		t.Fatalf("status reasoning summary output mismatch:\nwant %q\n got %q", want, got)
	}
}

func TestReasoningSummaryStatusWrapsPlainParagraph(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Now:             func() time.Time { return time.Date(2026, 6, 13, 16, 15, 34, 0, time.Local) },
		TimestampLayout: TimestampShortLayout,
		Width:           func() int { return 24 },
	})

	r.ReasoningSummaryStatus("alpha beta gamma delta epsilon zeta eta theta")

	if out.Len() != 0 {
		t.Fatalf("status reasoning summary should not write stdout, got %q", out.String())
	}
	got := errw.String()
	want := "[16:15:34 reasoning]\n" +
		"  alpha beta gamma delta\n" +
		"  epsilon zeta eta theta\n" +
		"[end reasoning]\n"
	if got != want {
		t.Fatalf("status reasoning summary output mismatch:\nwant %q\n got %q", want, got)
	}
}

func TestToolUseStreamEnabledWritesProgressOnlyToStderr(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ToolStream: true})

	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read_file"})
	r.ToolUseDelta(0, `{"path":`)
	r.ToolUseDelta(0, `"a.go"}`)
	r.Notice("[done]")

	if out.Len() != 0 {
		t.Errorf("tool-call stream must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	if !strings.Contains(got, "[tool-call: read_file id=call_1]") {
		t.Errorf("missing tool-call start line, got %q", got)
	}
	if strings.Contains(got, "[tool-call args]") || strings.Contains(got, `{"path"`) {
		t.Errorf("tool-call args should not dump raw JSON, got %q", got)
	}
	if !strings.Contains(got, "[done]") {
		t.Errorf("notice should still render after ignored argument deltas, got %q", got)
	}
}

func TestEditToolCallDoesNotDumpLargeJSONArgs(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ToolStream: true})

	input := json.RawMessage(`{"files":[{"path":"internal/ui/repl.go","edits":[{"oldText":"line1\nline2\nline3","newText":"line1\nline two changed\nline3"}]}]}`)
	r.ToolUseStart(llm.ToolCall{ID: "call_edit", Name: "edit"})
	r.ToolUseDelta(0, `{"files":[{"path":"internal/ui/repl.go","edits":[{"oldText":"line1\nline2\nline3",`)
	r.ToolUseDelta(0, `"newText":"line1\nline two changed\nline3"}]}]}`)
	r.ToolStart(llm.ToolCall{ID: "call_edit", Name: "edit", Input: input})
	r.ToolResult(llm.ToolResult{
		ForID:   "call_edit",
		Text:    "error: could not find oldText in internal/ui/repl.go",
		IsError: true,
	})

	got := errw.String()
	if out.Len() != 0 {
		t.Errorf("tool-call stream must not touch stdout, got %q", out.String())
	}
	if strings.Contains(got, "[tool-call args]") || strings.Contains(got, `{"path":"internal/ui/repl.go"`) {
		t.Errorf("large edit args should not be dumped as raw JSON, got %q", got)
	}
	for _, want := range []string{
		"[tool-call: edit id=call_edit]",
		"[tool: edit started",
		"path=internal/ui/repl.go",
		"edits=1",
		"internal/ui/repl.go",
		"[edit]",
		"error: error: could not find oldText in internal/ui/repl.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q:\n%s", want, got)
		}
	}
}

func TestToolUseStreamDisabledSuppressesRawArgs(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ToolStream: false})

	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read_file"})
	r.ToolUseDelta(0, `{"path":"a.go"}`)

	if out.Len() != 0 {
		t.Errorf("disabled tool stream must not touch stdout, got %q", out.String())
	}
	if errw.Len() != 0 {
		t.Errorf("disabled tool stream must not touch stderr, got %q", errw.String())
	}
}

func TestUsageLineCumulativeAcrossTurns(t *testing.T) {
	var out, errw bytes.Buffer
	start := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	r := NewRenderer(&out, &errw, RenderOptions{
		Model: "claude-opus-4-8",
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{
			"claude-opus-4-8": {
				ContextWindow: 1_000_000,
				Price:         llm.Price{Input: 5.0, Output: 25.0},
			},
		}),
		Now: fixedClock(start, time.Second),
	})

	// Turn 1: 1000 in, 200 out.
	r.StartTurn()
	r.TurnComplete(agent.TurnUsage{
		ModelTurns: 1,
		Usage:      llm.Usage{InputTokens: 1000, OutputTokens: 200},
	})
	line1 := errw.String()
	errw.Reset()
	if !strings.Contains(line1, "1.0k (1.0k) in") {
		t.Errorf("turn 1 should show per-turn = cumulative, got %q", line1)
	}
	if !strings.Contains(line1, "200 (200) out") {
		t.Errorf("turn 1 output should match cumulative, got %q", line1)
	}

	// Turn 2: 500 in, 300 out. Cumulative: 1500 in, 500 out.
	r.StartTurn()
	r.TurnComplete(agent.TurnUsage{
		ModelTurns: 2,
		Usage:      llm.Usage{InputTokens: 500, OutputTokens: 300},
	})
	line2 := errw.String()
	if !strings.Contains(line2, "500 (1.5k) in") {
		t.Errorf("turn 2 should show 500 per-turn and 1.5k cumulative, got %q", line2)
	}
	if !strings.Contains(line2, "300 (500) out") {
		t.Errorf("turn 2 should show 300 per-turn and 500 cumulative, got %q", line2)
	}
}

// --- r12 live wait-time counter + during-turn input line ---

func liveRenderer(out, errw *bytes.Buffer, now func() time.Time) *Renderer {
	return NewRenderer(out, errw, RenderOptions{
		LiveStatus: true,
		Now:        now,
		Width:      func() int { return 80 },
	})
}

func TestLiveCounterPaintsInPlaceAndCarriesContextPercent(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartTurn()
	r.ModelTurnStart(1, 1, agent.ContextEstimate{Total: 30, Window: 100})
	defer r.StopProgress()

	got := errw.String()
	if out.Len() != 0 {
		t.Fatalf("counter must not touch stdout, got %q", out.String())
	}
	if !strings.Contains(got, "\r\x1b[2K") {
		t.Fatalf("counter should repaint in place with \\r\\x1b[2K, got %q", got)
	}
	if !strings.Contains(got, "[model: turn 1 · 0s · ctx 30%]") {
		t.Fatalf("counter should show elapsed + context %%, got %q", got)
	}
	if strings.Contains(got, "waiting") {
		t.Fatalf("live mode should not print the static waiting line, got %q", got)
	}
}

func TestLiveCounterTickAdvancesElapsed(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartTurn()
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	now = now.Add(12 * time.Second)
	r.tick() // simulate a ticker fire without waiting on the real timer
	defer r.StopProgress()

	if got := errw.String(); !strings.Contains(got, "[model: turn 1 · 12s]") {
		t.Fatalf("tick should repaint with the elapsed seconds, got %q", got)
	}
}

func TestLiveCounterErasedWhenOutputAppears(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartTurn()
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	errw.Reset() // focus on what happens when streamed output arrives
	r.TextDelta("hello")

	if got := out.String(); got != "hello" {
		t.Fatalf("assistant text should stream to stdout, got %q", got)
	}
	if got := errw.String(); got != "\r\x1b[2K" {
		t.Fatalf("first output should erase the counter, got %q", got)
	}
}

func TestLiveCounterTicksDuringToolGap(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartTurn()
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "grep", Input: json.RawMessage(`{"args":["x"]}`)})
	defer r.StopProgress()

	got := errw.String()
	if !strings.Contains(got, "[tool: grep started") {
		t.Fatalf("tool start line should scroll, got %q", got)
	}
	if !strings.Contains(got, "[tool: grep · 0s]") {
		t.Fatalf("a counter should tick during the tool gap, got %q", got)
	}
}

func TestLiveInputLineRendersTypedBuffer(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartTurn()
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	errw.Reset()
	r.SetInputLine("fix the bug", len("fix the bug"))
	defer r.StopProgress()

	if got := errw.String(); !strings.Contains(got, "[model: turn 1 · 0s] > fix the bug") {
		t.Fatalf("input line should render the typed buffer after the counter, got %q", got)
	}
}

func TestLiveInputLineSanitizesNewlines(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartTurn()
	r.ModelTurnStart(1, 1, agent.ContextEstimate{})
	errw.Reset()
	r.SetInputLine("line1\nline2", len("line1\nline2"))
	defer r.StopProgress()

	got := errw.String()
	if strings.Contains(got, "\n") {
		t.Fatalf("the typed buffer's newline must not break the single counter line, got %q", got)
	}
	if !strings.Contains(got, "line1 line2") {
		t.Fatalf("embedded newline should render as a space, got %q", got)
	}
}

func TestUsageLineShowsCacheAndReasoning(t *testing.T) {
	line := usageLine(agent.TurnUsage{
		ModelTurns: 1,
		Usage:      llm.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 3000, ReasoningTokens: 450},
	}, time.Second, 0, false, 1000, 200, 0)
	if !strings.Contains(line, "cache 3.0k read") {
		t.Errorf("usage line should report cache reads, got %q", line)
	}
	if !strings.Contains(line, "(75%)") {
		t.Errorf("usage line should report the cache-hit ratio, got %q", line)
	}
	if !strings.Contains(line, "450 reasoning") {
		t.Errorf("usage line should report reasoning tokens, got %q", line)
	}
}

func TestClipDisplayTailKeepsTailWithinWidth(t *testing.T) {
	out := clipDisplayTail("abcdefghij", 5)
	if displayWidth(out) > 5 {
		t.Fatalf("clip should fit within 5 cols, got %q (width %d)", out, displayWidth(out))
	}
	if !strings.HasPrefix(out, "…") || !strings.HasSuffix(out, "j") {
		t.Fatalf("clip should keep the tail and mark truncation, got %q", out)
	}
}

func TestClipDisplayTailCountsWideRunes(t *testing.T) {
	// Five double-width runes = 10 cols; must clip to fit 6 cols without wrap.
	out := clipDisplayTail("漢字漢字漢", 6)
	if displayWidth(out) > 6 {
		t.Fatalf("wide-char clip exceeded width: %q has width %d", out, displayWidth(out))
	}
}

func TestClipStatusLineCursorColumn(t *testing.T) {
	const prefix = "[model: turn 1 · 0s] > "
	prefixW := displayWidth(prefix)

	t.Run("fits shows whole line, cursor at true column", func(t *testing.T) {
		text, col := clipStatusLine(prefix, "hello", 2, 80)
		if text != prefix+"hello" {
			t.Fatalf("text = %q, want %q", text, prefix+"hello")
		}
		if want := prefixW + displayWidth("he"); col != want {
			t.Fatalf("cursorCol = %d, want %d", col, want)
		}
	})

	t.Run("wide runes advance the cursor by display width", func(t *testing.T) {
		// Cursor between the two double-width runes.
		text, col := clipStatusLine("> ", "漢字", 1, 80)
		if text != "> 漢字" {
			t.Fatalf("text = %q, want %q", text, "> 漢字")
		}
		if want := displayWidth("> ") + displayWidth("漢"); col != want {
			t.Fatalf("cursorCol = %d, want %d (must count the wide rune as 2 cols)", col, want)
		}
	})

	t.Run("overflow tail-anchored keeps cursor at end visible", func(t *testing.T) {
		input := strings.Repeat("a", 100)
		maxW := 10
		text, col := clipStatusLine("> ", input, len(input), maxW)
		if displayWidth(text) > maxW {
			t.Fatalf("clipped text %q width %d exceeds maxW %d", text, displayWidth(text), maxW)
		}
		if !strings.HasPrefix(text, "…") {
			t.Fatalf("overflow should mark the hidden head with a leading …, got %q", text)
		}
		if col < 0 || col > maxW {
			t.Fatalf("cursorCol = %d, must be within [0,%d] so it stays on the single row", col, maxW)
		}
		if col != displayWidth(text) {
			t.Fatalf("cursor at end should sit just past the visible tail: col=%d, visible width=%d", col, displayWidth(text))
		}
	})

	t.Run("overflow scrolls left to reveal the cursor", func(t *testing.T) {
		input := strings.Repeat("a", 100)
		maxW := 10
		text, col := clipStatusLine("> ", input, 0, maxW)
		if displayWidth(text) > maxW {
			t.Fatalf("clipped text %q width %d exceeds maxW %d", text, displayWidth(text), maxW)
		}
		if !strings.HasPrefix(text, "…") || !strings.HasSuffix(text, "…") {
			t.Fatalf("cursor at home in a long line should show both head and tail markers, got %q", text)
		}
		// Cursor sits just after the leading … (column 1), proving the window
		// scrolled left to keep it visible instead of staying tail-anchored.
		if col != 1 {
			t.Fatalf("cursorCol = %d, want 1 (just after the leading …)", col)
		}
	})
}

func TestApproachingCompactionNoticeOnce(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})
	r.StartTurn()
	r.ModelTurnStart(1, 1, agent.ContextEstimate{Total: 85, Window: 100})
	r.ModelTurnStart(2, 1, agent.ContextEstimate{Total: 90, Window: 100})
	if n := strings.Count(errw.String(), "approaching compaction"); n != 1 {
		t.Fatalf("compaction notice should fire once, got %d in %q", n, errw.String())
	}
}
