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

func TestModelTurnCompleteUnknownModelOmitsCost(t *testing.T) {
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

	if out.Len() != 0 || errw.Len() != 0 {
		t.Errorf("unknown model cost should be silent, out=%q errw=%q", out.String(), errw.String())
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
