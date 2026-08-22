package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/term/highlight"
)

func stripRenderTestANSI(s string) string {
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
		Name:  "shell",
		Input: json.RawMessage(`{"argv":["rg","-n","func main","."]}`),
	})
	r.ToolResult(llm.ToolResult{
		ForID: "c1",
		Text:  "a.go:1:func main\nb.go:2:func main\n",
	})

	got := errw.String()
	if out.Len() != 0 {
		t.Errorf("tool lines must go to errw, not out; out=%q", out.String())
	}
	if strings.Contains(got, "[tool: shell started") {
		t.Errorf("tool start should be hidden by default, got %q", got)
	}
	if !strings.Contains(got, "[shell]") {
		t.Errorf("summary should include [shell], got %q", got)
	}
	if !strings.Contains(got, `argv=["rg","-n","func main","."]`) {
		t.Errorf("summary should show argv-style args, got %q", got)
	}
	if !strings.Contains(got, "→") {
		t.Errorf("summary should show the arrow separator, got %q", got)
	}
	if !strings.Contains(got, "2 lines") {
		t.Errorf("summary should report 2 lines, got %q", got)
	}
}

func TestToolSummaryRichImagesUsesSafeMetadata(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Verbose: true})
	payload := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"
	r.ToolStart(llm.ToolCall{ID: "image_1", Name: "view_image", Input: json.RawMessage(`{"path":"/private/screen.png"}`)})
	r.ToolResult(llm.ToolResult{
		ForID: "image_1",
		Text:  "image attached",
		Content: []llm.ContentBlock{{
			Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: payload,
			ImageDetail: "high", ImageWidth: 1, ImageHeight: 1,
		}},
	})

	got := errw.String()
	if !strings.Contains(got, "1 image (image/png") || !strings.Contains(got, "1x1") || !strings.Contains(got, "detail=high") {
		t.Fatalf("rich summary = %q", got)
	}
	if strings.Contains(got, payload) {
		t.Fatalf("base64 leaked into rich summary: %q", got)
	}
}

func TestToolSummaryErrorMarked(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})
	r.ToolStart(llm.ToolCall{ID: "e1", Name: "edit", Input: json.RawMessage(`{"path":"x"}`)})
	r.ToolResult(llm.ToolResult{ForID: "e1", Text: "files is required", IsError: true})

	got := errw.String()
	if !strings.Contains(got, "error") {
		t.Errorf("error result should surface the error text, got %q", got)
	}
}

func TestToolSummaryFinishesAssistantLine(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TextDelta("calling a tool")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})

	if got := out.String(); got != "calling a tool\n" {
		t.Errorf("tool summary should force a newline after assistant text, got %q", got)
	}
	if got := errw.String(); !strings.Contains(got, "[read]") {
		t.Errorf("tool summary should still go to errw, got %q", got)
	}
}

func TestToolSummaryDoesNotDoubleSpaceAfterAssistantNewline(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TextDelta("calling a tool\n")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})

	if got := out.String(); got != "calling a tool\n" {
		t.Errorf("tool summary should not add a second newline, got %q", got)
	}
}

func TestToolLinesRelativizePathsUnderCWD(t *testing.T) {
	dir := t.TempDir()
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{CWD: dir, ToolStream: true})

	under := filepath.Join(dir, "sub", "file.go")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(fmt.Sprintf(`{"path": %q}`, under))})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})

	got := errw.String()
	if !strings.Contains(got, "[tool: read started path=sub/file.go]") {
		t.Errorf("tool progress should show the relative path, got %q", got)
	}
	if !strings.Contains(got, "[read] path=sub/file.go → ") {
		t.Errorf("tool summary should show the relative path, got %q", got)
	}
	if strings.Contains(got, under) {
		t.Errorf("tool lines must not contain the absolute path %q, got %q", under, got)
	}
}

func TestToolLinesKeepPathsOutsideCWD(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(string(filepath.Separator), "outside", "file.go")
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{CWD: dir, ToolStream: true})

	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(fmt.Sprintf(`{"path": %q}`, outside))})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})

	got := errw.String()
	if !strings.Contains(got, "[tool: read started path="+outside+"]") {
		t.Errorf("tool progress should keep the absolute path, got %q", got)
	}
	if !strings.Contains(got, "[read] path="+outside+" → ") {
		t.Errorf("tool summary should keep the absolute path, got %q", got)
	}
}

func TestVerboseAddsSnippet(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Verbose: true})
	r.ToolUseStart(llm.ToolCall{ID: "c1", Name: "read"})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)})
	body := "line1\nline2\nline3\nline4\nline5\nline6\nline7\n"
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: body})

	got := errw.String()
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line5") {
		t.Errorf("verbose should include the first ~5 lines, got %q", got)
	}
	if !strings.Contains(got, "[tool: read started path=a.go]") {
		t.Errorf("verbose should include tool progress details, got %q", got)
	}
	if !strings.Contains(got, "[tool-call: read id=c1]") {
		t.Errorf("verbose should include streamed tool-call progress, got %q", got)
	}
	if strings.Contains(got, "line6") {
		t.Errorf("verbose should cap the snippet at ~5 lines, got %q", got)
	}
}

func TestQuietSuppressesStatusButKeepsUsage(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Quiet: true, ToolStream: true})

	// Progress noise (TurnAttemptStart, tool lines, notices) is suppressed, but
	// -quiet alone still prints the single per-prompt usage line (r25).
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	r.ToolUseStart(llm.ToolCall{ID: "c1", Name: "read"})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "package main\n"})
	r.Notice("[something happened]")
	r.StartPromptRun()
	r.PromptComplete(agent.PromptUsage{})

	got := errw.String()
	if strings.Contains(got, "waiting") || strings.Contains(got, "read") || strings.Contains(got, "something happened") {
		t.Errorf("quiet mode should suppress progress lines, got %q", got)
	}
	if !strings.Contains(got, "[prompt:") {
		t.Errorf("quiet mode should still print the per-prompt usage line (r25), got %q", got)
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
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	r.StartPromptRun()
	r.PromptComplete(agent.PromptUsage{})

	if errw.Len() != 0 {
		t.Errorf("SuppressUsage should silence errw entirely, got %q", errw.String())
	}
}

func TestSubmittedPromptSeparator(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		var out, errw bytes.Buffer
		r := NewRenderer(&out, &errw, RenderOptions{})
		r.SubmittedPromptSeparator()
		if got, want := errw.String(), submittedPromptRule+"\n"; got != want {
			t.Fatalf("plain separator = %q, want %q", got, want)
		}
	})
	t.Run("color", func(t *testing.T) {
		var out, errw bytes.Buffer
		r := NewRenderer(&out, &errw, RenderOptions{Color: true})
		r.SubmittedPromptSeparator()
		if got, want := errw.String(), ansiDim+submittedPromptRule+ansiReset+"\n"; got != want {
			t.Fatalf("color separator = %q, want %q", got, want)
		}
	})
	t.Run("quiet still prints", func(t *testing.T) {
		var out, errw bytes.Buffer
		r := NewRenderer(&out, &errw, RenderOptions{Quiet: true})
		r.SubmittedPromptSeparator()
		if got, want := errw.String(), submittedPromptRule+"\n"; got != want {
			t.Fatalf("quiet separator = %q, want %q (separator is structural, not a status line)", got, want)
		}
	})
}

func TestToolDiffWritesToErr(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TextDelta("partial")
	r.ToolDiff(llm.ToolCall{ID: "c1", Name: "edit"}, "f.txt", "--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n-old\n+new\n")

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

	text := "--- a/main.go\n+++ b/main.go\n@@ -1,1 +1,1 @@\n-func old() {}\n+func main() {}\n"
	r.ToolDiff(llm.ToolCall{ID: "c1", Name: "edit"}, "main.go", text)

	got := errw.String()
	for _, want := range []string{
		"\x1b[38;2;78;201;176m---",                   // file header builtin/type color
		"\x1b[38;2;101;169;224m@@",                   // hunk keyword color
		"\x1b[48;2;33;58;43m\x1b[38;2;137;209;133m+", // added row and sigil
		"\x1b[48;2;74;34;29m\x1b[38;2;244;135;113m-", // removed row and sigil
		"\x1b[38;2;101;169;224mfunc",                 // Go keyword
		"\x1b[0K",                                    // erase-to-EOL extends the tint to the window edge
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("colored diff missing %q:\n%q", want, got)
		}
	}
	for _, bright := range []string{"\x1b[102m", "\x1b[101m"} {
		if strings.Contains(got, bright) {
			t.Fatalf("colored diff retained bright background %q:\n%q", bright, got)
		}
	}
	if stripped := stripRenderTestANSI(got); stripped != text {
		t.Fatalf("strip-ANSI roundtrip altered the diff:\n in: %q\nout: %q", text, stripped)
	}
}

func TestToolDiffColorUnknownLanguage(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Color: true})

	text := "--- a/notes.txt\n+++ b/notes.txt\n@@ -1,1 +1,1 @@\n-old\n+new func\n"
	r.ToolDiff(llm.ToolCall{ID: "c1", Name: "edit"}, "notes.txt", text)

	got := errw.String()
	for _, want := range []string{"\x1b[48;2;33;58;43m", "\x1b[38;2;137;209;133m+", "\x1b[48;2;74;34;29m", "\x1b[38;2;244;135;113m-"} {
		if !strings.Contains(got, want) {
			t.Fatalf("colored diff missing %q:\n%q", want, got)
		}
	}
	// Unknown language: content is plain, so no keyword span on "func".
	if strings.Contains(got, "\x1b[38;2;101;169;224mfunc") {
		t.Fatalf("unknown language should not gain token spans:\n%q", got)
	}
	if stripped := stripRenderTestANSI(got); stripped != text {
		t.Fatalf("strip-ANSI roundtrip altered the diff:\n in: %q\nout: %q", text, stripped)
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
	r.StartPromptRun()
	r.PromptComplete(agent.PromptUsage{
		Turns: 3,
		Usage: llm.Usage{InputTokens: 12400, OutputTokens: 1800, CostUSD: 0.107, CostKnown: true},
	})

	got := errw.String()
	if out.Len() != 0 {
		t.Errorf("PromptComplete should not write a newline before usage with no assistant text, got out=%q", out.String())
	}
	if !strings.Contains(got, "[prompt:") {
		t.Errorf("usage line should be bracketed, got %q", got)
	}
	if !strings.Contains(got, "3 turns") {
		t.Errorf("usage line should show turn count, got %q", got)
	}
	if !strings.Contains(got, "12.4k (12.4k) in") || !strings.Contains(got, "1.8k (1.8k) out") {
		t.Errorf("usage line should show per-prompt (cumulative) token counts, got %q", got)
	}
	if !strings.Contains(got, "$") {
		t.Errorf("known model should show a cost, got %q", got)
	}
	// Both per-prompt and cumulative cost should appear (parenthesised cumulative).
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
	r.StartPromptRun()
	r.PromptComplete(agent.PromptUsage{Turns: 1, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}})

	got := errw.String()
	if strings.Contains(got, "$") {
		t.Errorf("unknown model must omit cost, got %q", got)
	}
}

func TestColorSuppressedWhenNotTTY(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Color: false})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})
	if strings.Contains(errw.String(), "\x1b[") {
		t.Errorf("no ANSI escapes when color disabled, got %q", errw.String())
	}
}

func TestColorEmittedWhenEnabled(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Color: true})
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"."}`)})
	r.ToolResult(llm.ToolResult{ForID: "c1", Text: "a\nb\n"})
	if !strings.Contains(errw.String(), "\x1b[") {
		t.Errorf("expected ANSI dim escapes when color enabled, got %q", errw.String())
	}
}

func TestTurnCompleteWritesTrailingNewline(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})
	r.StartPromptRun()
	r.TextDelta("hello world")
	r.PromptComplete(agent.PromptUsage{Turns: 1})

	got := out.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("PromptComplete should write a trailing newline to out, got %q", got)
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
	r := NewRenderer(&out, &errw, RenderOptions{Color: true, Markdown: true})

	r.AssistantPhase(llm.AssistantPhaseCommentary)
	r.TextDelta("I have enough to answer.")
	r.AssistantPhase(llm.AssistantPhaseFinal)
	r.TextDelta("Yes, with limits.")
	r.finishAssistantLine()

	want := "I have enough to answer.\n" +
		ansiDim + submittedPromptRule + ansiReset +
		"\nYes, with limits.\n"
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

	if got := strings.Count(out.String(), "\n"+submittedPromptRule+"\n"); got != 1 {
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

func TestTextDeltaHighlightsTaggedFenceAcrossDeltas(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, Color: true})

	for _, delta := range []string{"```go\nfu", "nc main() {\n", "``", "`\n"} {
		r.TextDelta(delta)
	}

	got := out.String()
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("highlighted fence lines = %q", lines)
	}
	if !strings.Contains(lines[1], "\x1b[") {
		t.Fatalf("tagged fence body was not colored: %q", lines[1])
	}
	if strings.Contains(lines[0], "\x1b[") || strings.Contains(lines[2], "\x1b[") {
		t.Fatalf("fence delimiters were colored: opening=%q closing=%q", lines[0], lines[2])
	}
	if stripped, want := stripRenderTestANSI(got), "  ```go\n  func main() {\n  ```\n"; stripped != want {
		t.Fatalf("stripped streamed fence = %q, want %q", stripped, want)
	}
	if errw.Len() != 0 {
		t.Fatalf("assistant fence must not touch stderr, got %q", errw.String())
	}
}

func TestRendererPropagatesLightThemeToEveryCodePath(t *testing.T) {
	const fence = "```go\nfunc main() {}\n```\n"
	const lightKeyword = "\x1b[38;2;0;0;255mfunc"
	const darkKeyword = "\x1b[38;2;101;169;224mfunc"

	newLight := func(out, errw *bytes.Buffer) *Renderer {
		return NewRenderer(out, errw, RenderOptions{Markdown: true, Color: true, ColorTheme: highlight.ThemeLight})
	}
	assertLight := func(name, got string) {
		t.Helper()
		if !strings.Contains(got, lightKeyword) || strings.Contains(got, darkKeyword) {
			t.Errorf("%s did not use only the light keyword palette: %q", name, got)
		}
	}

	var out, errw bytes.Buffer
	r := newLight(&out, &errw)
	assertLight("complete markdown", r.FormatMarkdown(fence))

	out.Reset()
	r = newLight(&out, &errw)
	for _, delta := range []string{"```go\nfu", "nc main() {}\n```\n"} {
		r.TextDelta(delta)
	}
	assertLight("streamed markdown", out.String())

	out.Reset()
	r = newLight(&out, &errw)
	r.ReasoningSummary(fence)
	assertLight("reasoning summary", out.String())

	errw.Reset()
	r = newLight(&out, &errw)
	r.ToolDiff(llm.ToolCall{ID: "c1", Name: "edit"}, "main.go", "@@ -1 +1 @@\n-func old() {}\n+func main() {}\n")
	assertLight("tool diff", errw.String())
	for _, want := range []string{"\x1b[48;2;218;251;225m", "\x1b[48;2;255;235;233m"} {
		if !strings.Contains(errw.String(), want) {
			t.Errorf("tool diff missing light row background %q: %q", want, errw.String())
		}
	}

	out.Reset()
	errw.Reset()
	plain := NewRenderer(&out, &errw, RenderOptions{Markdown: true, ColorTheme: highlight.ThemeLight})
	if got := plain.FormatMarkdown(fence); strings.Contains(got, "\x1b[") {
		t.Errorf("theme enabled ANSI in complete markdown: %q", got)
	}
	plain.TextDelta(fence)
	plain.ToolDiff(llm.ToolCall{}, "main.go", "+func main() {}\n")
	if strings.Contains(out.String()+errw.String(), "\x1b[") {
		t.Errorf("theme enabled ANSI in live output: stdout=%q stderr=%q", out.String(), errw.String())
	}
}

func TestFormatMarkdownUsesRendererPolicy(t *testing.T) {
	const input = "* first item has several words\n  + child item\n1. ordered item"
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Markdown: true,
		Width:    func() int { return 24 },
	})

	want := "- first item has several\n  words\n  - child item\n1. ordered item"
	if got := r.FormatMarkdown(input); got != want {
		t.Fatalf("FormatMarkdown = %q, want %q", got, want)
	}

	r = NewRenderer(&out, &errw, RenderOptions{Width: func() int { return 1 }})
	if got := r.FormatMarkdown(input); got != input {
		t.Fatalf("FormatMarkdown disabled = %q, want raw %q", got, input)
	}
}

func TestTextDeltaFitsMarkdownTableToWidth(t *testing.T) {
	const width = 24
	const source = "| Description | Count |\n" +
		"| --- | ---: |\n" +
		"| very long item | 12345 |\n"

	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Markdown: true,
		Width:    func() int { return width },
	})
	r.TextDelta(source)
	if out.Len() != 0 {
		t.Fatalf("table was emitted before assistant flush: %q", out.String())
	}
	r.finishAssistantLine()

	got := out.String()
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if displayWidth(line) > width {
			t.Fatalf("table line width = %d, want <= %d: %q", displayWidth(line), width, line)
		}
	}
	if !strings.Contains(got, "Description") || !strings.Contains(got, "12345") {
		t.Fatalf("responsive table lost content: %q", got)
	}
	if errw.Len() != 0 {
		t.Fatalf("assistant markdown wrote stderr: %q", errw.String())
	}
}

func TestMarkdownAssistantFlushesBeforeStatusLine(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, ToolStream: true})

	r.TextDelta("calling **tool**")
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"."}`)})

	if got := out.String(); got != "calling tool\n" {
		t.Fatalf("assistant markdown should flush before status, got %q", got)
	}
	if got := errw.String(); !strings.Contains(got, "[tool: read started path=.]") {
		t.Fatalf("status line missing after markdown flush, got %q", got)
	}
}

func TestLiveTableNotSplitByWaitCounter(t *testing.T) {
	// Reproduces the live-vs-replay table misalignment seen in
	// harness session 20260807T180550Z.  The session's first table is
	// 4 columns wide; the live TextDelta stream is split mid-table across
	// several deltas.  Before the fix, resumeLiveModelWaitAfterAssistantText
	// and beginWait would Flush the markdown stream after a newline-terminated
	// delta, splitting the buffered table into two separately-formatted
	// halves.  The short early rows then lacked padding for the wide later
	// rows (e.g. "explore (no edit/write)"), producing
	// "| independent | same |" live but "| independent                  | same |" on replay.
	full := "| agent | baseline → candidate (-no-env, request_bytes)  | Δ total      | vs system    |\n| --- | --- | --- | --- |\n| auto | 8282+13980=22275 → 7580+14313=~21906 see below | -369 (-1.7%) | -5.0% system |\n| independent | same | -369 | -5.0% |\n| explore (no edit/write) | 8524+10236=18773 → 7822+10355=18190 | -583 (-3.1%) | -8.3% system |\n| plan | 8949+13062=22024 → 8247+13112=~21372 | -652 | -7.8% |\n\n"
	// Split the way the real session did: header+separator+first row end in
	// one delta, the next delta continues with the second row.
	deltas := []string{
		"| agent | baseline → candidate (-no-env, request_bytes)  | Δ total      | vs system    |\n| --- | --- | --- | --- |\n| auto | 8282+13980=22275 → 7580+14313=~21906 see below | -369 (-1.7%) | -5.0% system |\n",
		"| independent | same | -369 | -5.0% |\n| explore (no edit/write) | 8524+10236=18773 → 7822+10355=18190 | -583 (-3.1%) | -8.3% system |\n| plan | 8949+13062=22024 → 8247+13112=~21372 | -652 | -7.8% |\n\n",
	}
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, LiveStatus: true, Width: func() int { return 240 }})
	r.StartPrompt()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	for _, d := range deltas {
		r.TextDelta(d)
	}
	r.TurnComplete(agent.TurnUsage{})
	r.PromptComplete(agent.PromptUsage{})
	live := out.String()
	replay := ""
	// replay is the single-Render equivalent (what session.Replay does)
	replay = r2String(full)
	if live != replay {
		t.Fatalf("live vs replay mismatch:\nlive=%q\nreplay=%q", live, replay)
	}
	if !strings.Contains(live, "| independent             | same") {
		t.Fatalf("live table not padded correctly, got %q", live)
	}
}

func r2String(s string) string {
	// helper so the test does not import markdown directly
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, Width: func() int { return 240 }})
	r.TextDelta(s)
	r.TurnComplete(agent.TurnUsage{})
	r.PromptComplete(agent.PromptUsage{})
	return out.String()
}

func TestTurnAttemptStartGoesToStderr(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{})

	r.TurnAttemptStart(2, 1, agent.ContextEstimate{})
	r.TurnAttemptStart(2, 3, agent.ContextEstimate{})

	if out.Len() != 0 {
		t.Errorf("model progress must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	if !strings.Contains(got, "[turn: 2 waiting]") {
		t.Errorf("missing turn wait line, got %q", got)
	}
	if !strings.Contains(got, "[turn: 2 attempt 3 waiting]") {
		t.Errorf("missing attempt wait line, got %q", got)
	}
}

func TestTurnAttemptCompleteDoesNotPrintCostCheckpoints(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Model: "priced-model",
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{
			"priced-model": {
				Price: llm.Price{Input: 10, Output: 20},
			},
		}),
	})
	r.SetCumulativeUsage(0, 0, 1.25, 0)
	r.StartPromptRun()

	r.TurnAttemptComplete(agent.TurnAttemptUsage{
		Turn:    1,
		Attempt: 1,
		Usage:   llm.Usage{InputTokens: 100_000, OutputTokens: 50_000, CostUSD: 2, CostKnown: true},
	})
	r.TurnAttemptComplete(agent.TurnAttemptUsage{
		Turn:    2,
		Attempt: 1,
		Usage:   llm.Usage{InputTokens: 50_000, CostUSD: 0.5, CostKnown: true},
	})

	if out.Len() != 0 {
		t.Errorf("model cost progress must not touch stdout, got %q", out.String())
	}
	if got := errw.String(); got != "" {
		t.Errorf("attempt completion should be accounted without a durable status line, got %q", got)
	}
}

func TestTurnAttemptCompleteFlushesToolUseProgress(t *testing.T) {
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
	r.StartPromptRun()

	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read"})
	if strings.Contains(errw.String(), "[tool-call:") {
		t.Fatalf("tool-call progress should wait for model cost, got:\n%s", errw.String())
	}
	r.TurnAttemptComplete(agent.TurnAttemptUsage{
		Turn:    1,
		Attempt: 1,
		Usage:   llm.Usage{InputTokens: 100_000, CostUSD: 1, CostKnown: true},
	})

	if out.Len() != 0 {
		t.Errorf("model and tool-call progress must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	waiting := strings.Index(got, "[turn: 1 waiting]")
	toolCall := strings.Index(got, "[tool-call: read id=call_1]")
	if waiting < 0 || toolCall < 0 {
		t.Fatalf("missing expected progress lines:\n%s", got)
	}
	if waiting >= toolCall {
		t.Fatalf("progress order =\n%s\nwant waiting, then tool-call", got)
	}
}

func TestTurnAttemptCompleteUnknownModelOmitsCostButWarnsOnce(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{
		Model:    "unknown-model",
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{}),
	})
	r.StartPromptRun()
	r.TurnAttemptComplete(agent.TurnAttemptUsage{
		Turn:    1,
		Attempt: 1,
		Usage:   llm.Usage{InputTokens: 100, OutputTokens: 10},
	})
	r.TurnAttemptComplete(agent.TurnAttemptUsage{
		Turn:    2,
		Attempt: 1,
		Usage:   llm.Usage{InputTokens: 50, OutputTokens: 5},
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
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read"})
	r.Notice("unbracketed notice")
	r.Notice("[bracketed notice]")

	if out.String() != "plain assistant text\n" {
		t.Fatalf("assistant text should stay raw, got %q", out.String())
	}
	got := errw.String()
	for _, want := range []string{
		"[16:15:34 turn: 1 waiting]",
		"[16:15:34 tool-call: read id=call_1]",
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
		"  \x1b[1mExploring context usage reduction\x1b[22m\n" +
		"\n" +
		"  Inspecting schemas <\x1b[36;4mhttps://foo.example.com/docs\x1b[39m\x1b[24m>\n" +
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

	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read"})
	r.ToolUseDelta(0, `{"path":`)
	r.ToolUseDelta(0, `"a.go"}`)
	r.Notice("[done]")

	if out.Len() != 0 {
		t.Errorf("tool-call stream must not touch stdout, got %q", out.String())
	}
	got := errw.String()
	if !strings.Contains(got, "[tool-call: read id=call_1]") {
		t.Errorf("missing tool-call start line, got %q", got)
	}
	if strings.Contains(got, "[tool-call args]") || strings.Contains(got, `{"path"`) {
		t.Errorf("tool-call args should not dump raw JSON, got %q", got)
	}
	if !strings.Contains(got, "[done]") {
		t.Errorf("notice should still render after ignored argument deltas, got %q", got)
	}
}

// Submission notices arrive from the REPL goroutine while tool-use events are
// rendered by the active prompt goroutine. Pending tool-call lines must remain
// race-free and must be flushed exactly once under that overlap.
func TestRendererNoticeConcurrentWithToolUseStart(t *testing.T) {
	var out, errw lockedBuffer
	r := NewRenderer(&out, &errw, RenderOptions{ToolStream: true})

	const calls = 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < calls; i++ {
			r.ToolUseStart(llm.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "probe"})
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < calls; i++ {
			r.Notice("[submission queued]")
		}
	}()
	close(start)
	wg.Wait()
	r.Notice("[flush]")

	got := errw.String()
	for i := 0; i < calls; i++ {
		line := fmt.Sprintf("[tool-call: probe id=call_%d]", i)
		if count := strings.Count(got, line); count != 1 {
			t.Fatalf("tool-call line %q count = %d, want 1", line, count)
		}
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
		Text:    "could not find oldText in internal/ui/repl.go",
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
		"error: could not find oldText in internal/ui/repl.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q:\n%s", want, got)
		}
	}
}

func TestToolUseStreamDisabledSuppressesRawArgs(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{ToolStream: false})

	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read"})
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

	// Prompt 1: 1000 in, 200 out.
	r.StartPromptRun()
	r.PromptComplete(agent.PromptUsage{
		Turns: 1,
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 200},
	})
	line1 := errw.String()
	errw.Reset()
	if !strings.Contains(line1, "1.0k (1.0k) in") {
		t.Errorf("prompt 1 should show per-prompt = cumulative, got %q", line1)
	}
	if !strings.Contains(line1, "200 (200) out") {
		t.Errorf("prompt 1 output should match cumulative, got %q", line1)
	}

	// Prompt 2: 500 in, 300 out. Cumulative: 1500 in, 500 out.
	r.StartPromptRun()
	r.PromptComplete(agent.PromptUsage{
		Turns: 2,
		Usage: llm.Usage{InputTokens: 500, OutputTokens: 300},
	})
	line2 := errw.String()
	if !strings.Contains(line2, "500 (1.5k) in") {
		t.Errorf("prompt 2 should show 500 per-prompt and 1.5k cumulative, got %q", line2)
	}
	if !strings.Contains(line2, "300 (500) out") {
		t.Errorf("prompt 2 should show 300 per-prompt and 500 cumulative, got %q", line2)
	}
}

// --- r12 live wait-time counter + during-prompt input line ---

func liveRenderer(out, errw *bytes.Buffer, now func() time.Time) *Renderer {
	return NewRenderer(out, errw, RenderOptions{
		LiveStatus: true,
		Now:        now,
		Width:      func() int { return 80 },
	})
}

func TestLiveDelegateStatusShowsForegroundActivity(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "durable-child", Agent: "explore", Depth: 1})
	registration.MarkTurn(2, 1, agent.ContextEstimate{Total: 20, Window: 100})
	registration.MarkActivity("thinking")
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return 100 },
	})
	t.Cleanup(func() {
		registration.Finish("completed", 0)
		r.StopProgress()
	})

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	got := errw.String()
	if !strings.Contains(got, "delegate d1 explore: turn 2 · thinking · ctx 20%") {
		t.Fatalf("foreground delegate status = %q", got)
	}
	if out.Len() != 0 {
		t.Fatalf("delegate status must not touch stdout, got %q", out.String())
	}
}

func TestLiveDelegateStatusKeepsElapsedCounterWithLongBackgroundWaitArgs(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 7, 29, 10, 13, 28, 0, time.Local)
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "background-child", Agent: "auto"})
	registration.MarkTurn(2, 1, agent.ContextEstimate{})
	registration.MarkActivity("tool shell")
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Now:              func() time.Time { return now },
		Width:            func() int { return 80 },
	})
	t.Cleanup(func() {
		registration.Finish("completed", 0)
		r.StopProgress()
	})

	r.StartPrompt()
	r.ToolStart(llm.ToolCall{
		ID:    "wait",
		Name:  "background_jobs",
		Input: json.RawMessage(`{"action":"wait","ids":["bg_20260729T171254Z_000006"],"timeout_seconds":5400,"until":"all"}`),
	})
	now = now.Add(12 * time.Second)
	errw.Reset()
	r.tick()

	got := errw.String()
	want := "[tool: background_jobs · 12s · delegate d1 auto: turn 2 · tool shell]"
	if !strings.Contains(got, want) {
		t.Fatalf("long background wait status = %q, want compact live counter %q", got, want)
	}
	if strings.Contains(got, "[d1 auto]") {
		t.Fatalf("long background wait collapsed to static delegate identity: %q", got)
	}
}

func TestLiveDelegateStatusUsesWaitLifecycleAcrossBackgroundJoin(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "background-child", Agent: "explore"})
	registration.MarkActivity("thinking")
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return 80 },
	})
	r.SetBackgroundProgress([]any{func() agent.DelegateProgressSnapshot {
		return agent.DelegateProgressSnapshot{Agent: "legacy", Turn: 9}
	}})
	t.Cleanup(func() {
		registration.Finish("completed", 0)
		r.StopProgress()
	})

	r.StartPromptRun()
	r.PromptWorkWaitStart()
	if got := errw.String(); !strings.Contains(got, "delegate d1 explore:") || strings.Contains(got, "legacy") {
		t.Fatalf("authoritative background join status = %q", got)
	}

	registration.Finish("completed", 0)
	errw.Reset()
	r.tick()
	if got := errw.String(); strings.Contains(got, "delegate d1") {
		t.Fatalf("completed background delegate remained visible: %q", got)
	}
	r.PromptWorkWaitComplete()
	r.StopProgress()
	r.renderMu.Lock()
	tickerRunning := r.ticker != nil
	r.renderMu.Unlock()
	if tickerRunning {
		t.Fatal("status ticker still running after the prompt boundary")
	}
}

func TestLiveDelegateTickDoesNotEraseStreamingParentOutput(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "background-child", Agent: "explore"})
	defer registration.Finish("completed", 0)
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return 80 },
	})
	defer r.StopProgress()

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	r.TextDelta("partial parent response")
	errw.Reset()
	r.tick()
	if got := errw.String(); got != "" {
		t.Fatalf("delegate tick touched terminal during parent output: %q", got)
	}
	if got := out.String(); got != "partial parent response" {
		t.Fatalf("parent output = %q", got)
	}
}

func TestLiveDelegateStatusSelectsLatestConcurrentNestedChild(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	first := registry.Register(delegate.ActivityStart{ID: "first", Agent: "explore", Depth: 1})
	latest := registry.Register(delegate.ActivityStart{ID: "nested", ParentID: "first", Agent: "plan", Depth: 2})
	third := registry.Register(delegate.ActivityStart{ID: "third", Agent: "auto", Depth: 1})
	latest.MarkTurn(3, 2, agent.ContextEstimate{})
	latest.MarkActivity("tool read path=\"internal/ui/render.go\"")
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return 120 },
	})
	r.SetToolProgress("delegate", func() agent.DelegateProgressSnapshot {
		return agent.DelegateProgressSnapshot{Agent: "legacy", Turn: 9}
	})
	t.Cleanup(func() {
		first.Finish("completed", 0)
		latest.Finish("completed", 0)
		third.Finish("completed", 0)
		r.StopProgress()
	})

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	got := errw.String()
	if !strings.Contains(got, "3 delegates · latest d2 plan: turn 3 attempt 2 · tool read") {
		t.Fatalf("concurrent delegate status = %q", got)
	}
	if strings.Contains(got, "latest d3") || strings.Contains(got, "legacy") {
		t.Fatalf("status selected registration order or legacy progress instead of authoritative activity: %q", got)
	}
}

func TestLiveDelegateStatusRespectsQuietAndNonTTYGates(t *testing.T) {
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "child", Agent: "explore"})
	defer registration.Finish("completed", 0)
	for _, tc := range []struct {
		name       string
		liveStatus bool
		quiet      bool
	}{
		{name: "quiet", liveStatus: true, quiet: true},
		{name: "non-TTY", liveStatus: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			r := NewRenderer(&out, &errw, RenderOptions{
				LiveStatus:       tc.liveStatus,
				Quiet:            tc.quiet,
				DelegateActivity: registry,
			})
			r.StartPromptRun()
			r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
			r.StopProgress()
			if strings.Contains(out.String(), "delegate d1") || strings.Contains(errw.String(), "delegate d1") {
				t.Fatalf("gated output exposed delegate status: stdout=%q stderr=%q", out.String(), errw.String())
			}
			if tc.quiet && (out.Len() != 0 || errw.Len() != 0) {
				t.Fatalf("quiet mode touched output: stdout=%q stderr=%q", out.String(), errw.String())
			}
		})
	}
}

func TestLiveDelegateStatusPreservesIdentityAtNarrowUnicodeWidth(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: strings.Repeat("durable", 20), Agent: "探索漢字レビュー担当"})
	registration.MarkActivity("tool read path=\"非常に長いパス.go\"")
	const terminalWidth = 18
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return terminalWidth },
	})
	defer registration.Finish("completed", 0)

	activity := registry.Snapshot()
	r.renderMu.Lock()
	text, _, _ := r.statusTextLocked(activity)
	r.renderMu.Unlock()
	if !strings.Contains(text, "d1") {
		t.Fatalf("narrow status dropped delegate identity: %q", text)
	}
	if got := displayWidth(text); got > terminalWidth-1 {
		t.Fatalf("narrow status width = %d, max %d: %q", got, terminalWidth-1, text)
	}
	if strings.ContainsAny(text, "\r\n") {
		t.Fatalf("narrow status wrapped: %q", text)
	}
}

func TestLiveDelegateStatusPreservesIDWithNarrowPromptInput(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "child", Agent: "探索漢字レビュー担当"})
	defer registration.Finish("completed", 0)
	const terminalWidth = 11
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return terminalWidth },
	})

	r.renderMu.Lock()
	r.statusInput = "abc"
	r.statusInputCursor = 3
	text, cursorCol, showCursor := r.statusTextLocked(registry.Snapshot())
	r.renderMu.Unlock()
	if !showCursor || !strings.Contains(text, "d1") || !strings.Contains(text, "> abc") {
		t.Fatalf("narrow delegate input status = %q cursor=%d show=%v", text, cursorCol, showCursor)
	}
	if got := displayWidth(text); got > terminalWidth-1 {
		t.Fatalf("narrow input status width = %d, max %d: %q", got, terminalWidth-1, text)
	}
	if cursorCol != displayWidth(text) {
		t.Fatalf("narrow input cursor column = %d, want %d", cursorCol, displayWidth(text))
	}
}

func TestLiveDelegateStatusUpdatesDuringPromptInput(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "child", Agent: "explore"})
	registration.MarkTurn(1, 1, agent.ContextEstimate{})
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:       true,
		DelegateActivity: registry,
		Width:            func() int { return 120 },
	})
	t.Cleanup(func() {
		registration.Finish("completed", 0)
		r.StopProgress()
	})

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	r.SetInputLine("hello", 5)
	registration.MarkActivity("tool read path=\"README.md\"")
	errw.Reset()
	r.tick()
	got := errw.String()
	if !strings.Contains(got, "d1 explore") || !strings.Contains(got, "tool read") || !strings.Contains(got, "> hello") {
		t.Fatalf("during-prompt delegate status = %q", got)
	}
	if !strings.Contains(got, "\r\x1b[") {
		t.Fatalf("during-prompt status did not restore the edit cursor: %q", got)
	}
}

func TestDelegateStatusOffSuppressesRegistryAndLegacyOnly(t *testing.T) {
	var out, errw bytes.Buffer
	registry := delegate.NewActivityRegistry(nil)
	registration := registry.Register(delegate.ActivityStart{ID: "child", Agent: "explore"})
	registration.MarkActivity("thinking")
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:            true,
		DelegateActivity:      registry,
		DisableDelegateStatus: true,
		Width:                 func() int { return 80 },
	})
	t.Cleanup(func() {
		registration.Finish("completed", 0)
		r.StopProgress()
	})
	r.SetToolProgress("delegate", func() agent.DelegateProgressSnapshot {
		return agent.DelegateProgressSnapshot{Agent: "legacy", Turn: 9}
	})
	r.StartPromptRun()
	r.TurnAttemptStart(2, 1, agent.ContextEstimate{})
	got := errw.String()
	if strings.Contains(got, "delegate") || strings.Contains(got, "turn 9") {
		t.Fatalf("delegate_output=off leaked delegate detail: %q", got)
	}
	if !strings.Contains(got, "[turn: 2 ·") {
		t.Fatalf("delegate_output=off suppressed unrelated live status: %q", got)
	}
}

func TestLiveDelegateStatusRetainsLegacyFallbackWithoutRegistry(t *testing.T) {
	var out, errw bytes.Buffer
	r := liveRenderer(&out, &errw, time.Now)
	defer r.StopProgress()
	r.SetToolProgress("delegate", func() agent.DelegateProgressSnapshot {
		return agent.DelegateProgressSnapshot{Agent: "legacy", Turn: 4, Tools: 2}
	})
	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	if got := errw.String(); !strings.Contains(got, "turn 4 · 2 tools") {
		t.Fatalf("legacy delegate progress fallback = %q", got)
	}
}

func TestLiveCounterPaintsInPlaceAndCarriesContextUsage(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{Total: 30_000, Window: 100_000})
	defer r.StopProgress()

	got := errw.String()
	if out.Len() != 0 {
		t.Fatalf("counter must not touch stdout, got %q", out.String())
	}
	if !strings.Contains(got, "\r\x1b[2K") {
		t.Fatalf("counter should repaint in place with \\r\\x1b[2K, got %q", got)
	}
	if !strings.Contains(got, "[turn: 1 · 0s · ctx 30% 30.0k/100.0k │ prompt 0s]") {
		t.Fatalf("counter should show elapsed + context usage + visually separated total, got %q", got)
	}
	if strings.Contains(got, "waiting") {
		t.Fatalf("live mode should not print the static waiting line, got %q", got)
	}
}

func TestCompletedTurnCounterIncludesContextUsage(t *testing.T) {
	got := turnUsageLine(agent.TurnUsage{
		Turn:    19,
		Context: agent.ContextEstimate{Total: 100_000, Window: 200_000},
	}, 10_500*time.Millisecond, 224_100*time.Millisecond)
	want := "[turn: 19 · 10.5s · ctx 50% 100.0k/200.0k │ prompt 224.1s]"
	if got != want {
		t.Fatalf("completed turn counter = %q, want %q", got, want)
	}
}

func TestCompletedTurnCounterShowsCostWhenKnown(t *testing.T) {
	got := turnUsageLine(agent.TurnUsage{
		Turn:    2,
		Usage:   llm.Usage{InputTokens: 12_000, OutputTokens: 800, CostUSD: 0.032, CostKnown: true},
		Context: agent.ContextEstimate{Total: 100_000, Window: 200_000},
	}, 6_000*time.Millisecond, 18_000*time.Millisecond)
	want := "[turn: 2 · 6.0s · $0.032 · ctx 50% 100.0k/200.0k │ prompt 18.0s]"
	if got != want {
		t.Fatalf("completed turn counter = %q, want %q", got, want)
	}
}

func TestCompletedTurnCounterOmitsCostWhenUnknown(t *testing.T) {
	got := turnUsageLine(agent.TurnUsage{
		Turn:  2,
		Usage: llm.Usage{InputTokens: 12_000, OutputTokens: 800},
	}, 6_000*time.Millisecond, 18_000*time.Millisecond)
	if strings.Contains(got, "$") {
		t.Fatalf("unknown cost must not print a dollar figure, got %q", got)
	}
	want := "[turn: 2 · 6.0s │ prompt 18.0s]"
	if got != want {
		t.Fatalf("completed turn counter = %q, want %q", got, want)
	}
}

func TestLiveCounterTickAdvancesElapsed(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	now = now.Add(12 * time.Second)
	r.tick() // simulate a ticker fire without waiting on the real timer
	defer r.StopProgress()

	if got := errw.String(); !strings.Contains(got, "[turn: 1 · 12s │ prompt 12s]") {
		t.Fatalf("tick should repaint with the elapsed seconds, got %q", got)
	}
}

func TestLiveCounterTracksAndCompletesCompaction(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 7, 18, 2, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.CompactionStart()
	now = now.Add(3 * time.Second)
	r.tick()
	defer r.StopProgress()

	if out.Len() != 0 {
		t.Fatalf("compaction progress must not touch stdout, got %q", out.String())
	}
	if got := errw.String(); !strings.Contains(got, "\r\x1b[2K[context: compacting · 3s]") {
		t.Fatalf("compaction should repaint an elapsed counter in place, got %q", got)
	}

	errw.Reset()
	r.CompactionComplete()
	if got := errw.String(); got != "\r\x1b[2K" {
		t.Fatalf("completing compaction should erase its transient row, got %q", got)
	}
	r.renderMu.Lock()
	active, drawn := r.statusActive, r.statusDrawn
	label, statusCtx := r.statusLabel, r.statusCtx
	tickerRunning := r.ticker != nil
	r.renderMu.Unlock()
	if active || drawn || label != "" || statusCtx != (agent.ContextEstimate{}) {
		t.Fatalf("completed compaction left stale status state: active=%t drawn=%t label=%q ctx=%+v",
			active, drawn, label, statusCtx)
	}
	if !tickerRunning {
		t.Fatal("compaction completion should preserve the turn-wide ticker")
	}

	errw.Reset()
	r.tick()
	r.TextDelta("done\n")
	if got := errw.String(); got != "" {
		t.Fatalf("a tick or complete assistant line must not revive completed compaction, got %q", got)
	}
}

func TestLiveCounterTracksAndCompletesPromptWorkWait(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 7, 19, 10, 47, 35, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPrompt()
	now = now.Add(5 * time.Second)
	r.PromptWorkWaitStart()
	now = now.Add(12 * time.Second)
	r.tick()
	defer r.StopProgress()

	if got := errw.String(); !strings.Contains(got, "[background: waiting for delegates · 12s │ prompt 17s]") {
		t.Fatalf("background wait should repaint elapsed and prompt totals, got %q", got)
	}

	errw.Reset()
	r.PromptWorkWaitComplete()
	if got := errw.String(); got != "\r\x1b[2K" {
		t.Fatalf("completing background wait should erase its transient row, got %q", got)
	}
	r.renderMu.Lock()
	active, drawn, label := r.statusActive, r.statusDrawn, r.statusLabel
	r.renderMu.Unlock()
	if active || drawn || label != "" {
		t.Fatalf("completed background wait left stale status state: active=%t drawn=%t label=%q", active, drawn, label)
	}
}

func TestLiveCounterShowsTotalElapsedSincePromptSubmission(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPrompt()
	now = now.Add(5 * time.Second)
	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	now = now.Add(2 * time.Second)
	r.tick()
	defer r.StopProgress()

	if got := errw.String(); !strings.Contains(got, "[turn: 1 · 2s │ prompt 7s]") {
		t.Fatalf("counter should include total elapsed since prompt submission, got %q", got)
	}
}

func TestLiveCounterErasedWhenPartialLineOutputAppears(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	errw.Reset() // focus on what happens when streamed output arrives
	r.TextDelta("hello")

	if got := out.String(); got != "hello" {
		t.Fatalf("assistant text should stream to stdout, got %q", got)
	}
	if got := errw.String(); got != "\r\x1b[2K" {
		t.Fatalf("partial-line output should erase the counter without restarting it, got %q", got)
	}
}

func TestLiveCounterResumesAfterCompleteAssistantLine(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPrompt()
	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	errw.Reset()
	r.TextDelta("Working.\n")
	now = now.Add(4 * time.Second)
	r.tick()
	defer r.StopProgress()

	if got := out.String(); got != "Working.\n" {
		t.Fatalf("assistant text should stream unchanged, got %q", got)
	}
	got := errw.String()
	if !strings.Contains(got, "[turn: 1 · 4s │ prompt 4s]") {
		t.Fatalf("counter should resume while the model continues after a full line, got %q", got)
	}
}

func TestLiveCounterDoesNotFlushMarkdownTokenDeltasAsLines(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 7, 16, 14, 0, 0, 0, time.Local)
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus: true,
		Markdown:   true,
		Now:        func() time.Time { return now },
	})

	r.StartPrompt()
	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	for _, delta := range []string{"I", "’ll", " trace", " the", " error", ".\n"} {
		r.TextDelta(delta)
	}
	defer r.StopProgress()

	if got, want := out.String(), "I’ll trace the error.\n"; got != want {
		t.Fatalf("streamed Markdown output = %q, want %q", got, want)
	}
}

func TestLiveCounterTicksDuringToolGap(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPromptRun()
	r.ToolStart(llm.ToolCall{ID: "c1", Name: "shell", Input: json.RawMessage(`{"argv":["rg","x"]}`)})
	defer r.StopProgress()

	got := errw.String()
	if strings.Contains(got, "[tool: shell started") {
		t.Fatalf("tool start line should not scroll by default, got %q", got)
	}
	if !strings.Contains(got, `[tool: shell argv=["rg","x"] · 0s │ prompt 0s]`) {
		t.Fatalf("a counter should show the tool arguments while ticking during the tool gap, got %q", got)
	}
}

func TestLiveCounterResumesForStreamedToolCallAfterAssistantText(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPrompt()
	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	errw.Reset()
	r.TextDelta("I’ll inspect")
	if got := errw.String(); got != "\r\x1b[2K" {
		t.Fatalf("assistant text should erase the original counter, got %q", got)
	}

	errw.Reset()
	now = now.Add(3 * time.Second)
	r.ToolUseStart(llm.ToolCall{ID: "call_1", Name: "read"})
	now = now.Add(2 * time.Second)
	r.tick()
	defer r.StopProgress()

	if got := out.String(); got != "I’ll inspect\n" {
		t.Fatalf("streamed tool-call status should move to its own row without changing text, got %q", got)
	}
	got := errw.String()
	if strings.Contains(got, "[tool-call:") {
		t.Fatalf("tool-call status should not force durable tool-stream output, got %q", got)
	}
	if !strings.Contains(got, "[turn: tool call read · 2s │ prompt 5s]") {
		t.Fatalf("counter should resume while streamed tool arguments finish, got %q", got)
	}
}

func TestLiveInputLineRendersTypedBuffer(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	errw.Reset()
	r.SetInputLine("fix the bug", len("fix the bug"))
	defer r.StopProgress()

	if got := errw.String(); !strings.Contains(got, "[turn: 1 · 0s │ prompt 0s] > fix the bug") {
		t.Fatalf("input line should render the typed buffer after the counter, got %q", got)
	}
}

func TestLiveInputLineSanitizesNewlines(t *testing.T) {
	var out, errw bytes.Buffer
	now := time.Date(2026, 6, 13, 16, 0, 0, 0, time.Local)
	r := liveRenderer(&out, &errw, func() time.Time { return now })

	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
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

func TestUsageLineConditionallyShowsCompactionsAfterContext(t *testing.T) {
	tests := []struct {
		name   string
		prompt int
		total  int
		want   string
	}{
		{name: "none", total: 0},
		{name: "single previous prompt omitted", total: 1},
		{name: "single current prompt", prompt: 1, total: 1, want: " · compactions 1"},
		{name: "session total only", total: 2, want: " · compactions 2 total"},
		{name: "current and total", prompt: 1, total: 3, want: " · compactions 1 (3 total)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := usageLine(agent.PromptUsage{
				Turns:       2,
				Compactions: tt.prompt,
				Context:     agent.ContextEstimate{Total: 100, Window: 1000},
			}, time.Second, 0.5, true, 0, 0, 0.5, tt.total)
			if tt.want == "" {
				if strings.Contains(line, "compaction") {
					t.Fatalf("usage line should omit compactions, got %q", line)
				}
				return
			}
			wantSuffix := " · ctx 100/1.0k" + tt.want + " · 1.0s]"
			if !strings.HasSuffix(line, wantSuffix) {
				t.Fatalf("usage line = %q, want suffix %q", line, wantSuffix)
			}
		})
	}
}

func TestUsageLineShowsCacheAndReasoning(t *testing.T) {
	line := usageLine(agent.PromptUsage{
		Turns: 1,
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 3000, CacheWrite1hTokens: 125, ReasoningTokens: 450},
	}, time.Second, 0, false, 1000, 200, 0, 0)
	if !strings.Contains(line, "cache 3.0k read") {
		t.Errorf("usage line should report cache reads, got %q", line)
	}
	if !strings.Contains(line, "(75%)") {
		t.Errorf("usage line should report the cache-hit ratio, got %q", line)
	}
	if !strings.Contains(line, "450 reasoning") {
		t.Errorf("usage line should report reasoning tokens, got %q", line)
	}
	if !strings.Contains(line, "125 cache write (1h)") {
		t.Errorf("usage line should report 1h cache writes, got %q", line)
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
	const prefix = "[turn: 1 · 0s │ prompt 0s] > "
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
	r.StartPromptRun()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{Total: 85, Window: 100})
	r.TurnAttemptStart(2, 1, agent.ContextEstimate{Total: 90, Window: 100})
	if n := strings.Count(errw.String(), "approaching compaction"); n != 1 {
		t.Fatalf("compaction notice should fire once, got %d in %q", n, errw.String())
	}
}

func TestApproachingCompactionNoticeUsesConfiguredTriggerAndDisable(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{CompactionTriggerPercent: 60})
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{Total: 54, Window: 100})
	if strings.Contains(errw.String(), "approaching compaction") {
		t.Fatalf("warning fired before configured lead point: %q", errw.String())
	}
	r.TurnAttemptStart(2, 1, agent.ContextEstimate{Total: 55, Window: 100})
	if !strings.Contains(errw.String(), "approaching compaction") {
		t.Fatalf("warning did not follow configured trigger: %q", errw.String())
	}

	out.Reset()
	errw.Reset()
	r = NewRenderer(&out, &errw, RenderOptions{CompactionTriggerPercent: 60, DisableAutoCompaction: true})
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{Total: 100, Window: 100})
	if strings.Contains(errw.String(), "approaching compaction") {
		t.Fatalf("disabled automatic compaction still warned: %q", errw.String())
	}
}

func TestLargeContextWarningNamesPayloadEstimateAndHistory(t *testing.T) {
	line := largeRequestWarning(agent.ContextEstimate{
		Total:           136_000,
		Window:          272_000,
		System:          3_000,
		Tools:           1_000,
		Messages:        132_000,
		PayloadTotal:    136_000,
		PayloadSystem:   3_000,
		PayloadTools:    1_000,
		PayloadMessages: 132_000,
	})
	for _, want := range []string{"large model context", "payload estimate", "message/tool-result history dominates", "cached or continued tokens"} {
		if !strings.Contains(line, want) {
			t.Fatalf("warning %q missing %q", line, want)
		}
	}
}

func TestLargeToolSchemaWarning(t *testing.T) {
	line := largeRequestWarning(agent.ContextEstimate{Total: 8_000, Window: 272_000, Tools: 12_000})
	if !strings.Contains(line, "large tool schema payload") || strings.Contains(line, "large model context") {
		t.Fatalf("tool schema warning = %q", line)
	}
}

func TestFormatActivityBatchPrefixBodiesAndGapGrammar(t *testing.T) {
	batch := delegate.FeedBatch{Items: []delegate.FeedItem{
		{Kind: delegate.FeedItemEvent, Event: delegate.ActivityEvent{
			Kind:           delegate.ActivityEventStart,
			DisplayID:      "d1",
			Agent:          "explore",
			Depth:          1,
			TranscriptPath: "/tmp/child",
		}},
		{Kind: delegate.FeedItemEvent, Event: delegate.ActivityEvent{
			Kind:         delegate.ActivityEventAssistant,
			DisplayID:    "d2",
			Agent:        "plan",
			Depth:        2,
			Text:         "continued",
			Continuation: true,
		}},
		{Kind: delegate.FeedItemEvent, Event: delegate.ActivityEvent{
			Kind:      delegate.ActivityEventAttemptDiscarded,
			DisplayID: "d2",
			Agent:     "plan",
			Depth:     2,
			Turn:      3,
			Attempt:   2,
		}},
		{Kind: delegate.FeedItemEvent, Event: delegate.ActivityEvent{
			Kind:      delegate.ActivityEventTerminal,
			DisplayID: "d1",
			Agent:     "explore",
			Status:    session.ChildStatusCompleted,
			Turn:      1,
		}},
		{Kind: delegate.FeedItemGap, Gap: delegate.SequenceGap{First: 9, Last: 9}},
		{Kind: delegate.FeedItemGap, Gap: delegate.SequenceGap{First: 10, Last: 12}},
	}}
	got := formatActivityBatch(batch)
	for _, want := range []string{
		"[delegate d1 explore] started · transcript /tmp/child\n",
		"[delegate d2 plan depth=2] assistant+: continued\n",
		"[delegate d2 plan depth=2] turn 3 attempt 2 discarded; retrying\n",
		"[delegate d1 explore] completed · 1 turn\n",
		"[delegate output] omitted 1 event\n",
		"[delegate output] omitted 3 events\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted batch missing %q:\n%s", want, got)
		}
	}
}

func TestDelegateLinesWaitForPlainAssistantBoundaryAndDoNotChangeStdout(t *testing.T) {
	var out, errw bytes.Buffer
	feed := delegate.NewActivityFeed()
	registry := delegate.NewActivityRegistry(feed)
	r := NewRenderer(&out, &errw, RenderOptions{DelegateFeed: feed})
	r.StartPrompt()
	r.TextDelta("parent")

	registration := registry.Register(delegate.ActivityStart{ID: "child", Agent: "explore"})
	registration.Finish(session.ChildStatusCompleted, 2)
	r.drainActivity()
	if errw.Len() != 0 {
		t.Fatalf("delegate lines split an incomplete parent line: %q", errw.String())
	}
	if got := out.String(); got != "parent" {
		t.Fatalf("stdout before boundary = %q, want parent", got)
	}

	r.TextDelta("\n")
	r.StopProgress()
	if got := out.String(); got != "parent\n" {
		t.Fatalf("delegate lines changed parent stdout = %q", got)
	}
	for _, want := range []string{
		"[delegate d1 explore] started\n",
		"[delegate d1 explore] completed · 2 turns\n",
	} {
		if !strings.Contains(errw.String(), want) {
			t.Fatalf("stderr missing %q: %q", want, errw.String())
		}
	}
}

func TestDelegateLinesWaitForMarkdownSourceBoundaryWithoutFlushing(t *testing.T) {
	var out, errw bytes.Buffer
	feed := delegate.NewActivityFeed()
	registry := delegate.NewActivityRegistry(feed)
	r := NewRenderer(&out, &errw, RenderOptions{Markdown: true, DelegateFeed: feed})
	r.StartPrompt()
	r.TextDelta("**parent")
	registration := registry.Register(delegate.ActivityStart{ID: "child"})
	registration.Finish(session.ChildStatusCompleted, 1)
	r.drainActivity()
	if out.Len() != 0 || errw.Len() != 0 {
		t.Fatalf("incomplete Markdown was flushed: stdout=%q stderr=%q", out.String(), errw.String())
	}

	r.TextDelta("**\n")
	r.StopProgress()
	if got := out.String(); got != "parent\n" {
		t.Fatalf("Markdown stdout = %q, want parent newline", got)
	}
	if !strings.Contains(errw.String(), "[delegate d1 auto] completed · 1 turn\n") {
		t.Fatalf("terminal line missing after Markdown boundary: %q", errw.String())
	}
}

func TestDelegateLinesArePromptScopedAndWorkWithoutLiveStatus(t *testing.T) {
	var out, errw bytes.Buffer
	feed := delegate.NewActivityFeed()
	registry := delegate.NewActivityRegistry(feed)
	r := NewRenderer(&out, &errw, RenderOptions{DelegateFeed: feed, LiveStatus: false})

	r.StartPrompt()
	first := registry.Register(delegate.ActivityStart{ID: "first"})
	first.Finish(session.ChildStatusCompleted, 1)
	r.StopProgress()
	if !strings.Contains(errw.String(), "[delegate d1 auto] completed") {
		t.Fatalf("first prompt did not render lines: %q", errw.String())
	}

	errw.Reset()
	between := registry.Register(delegate.ActivityStart{ID: "between"})
	between.Finish(session.ChildStatusCompleted, 1)
	r.StartPrompt()
	r.StopProgress()
	if errw.Len() != 0 {
		t.Fatalf("pre-prompt retained events replayed: %q", errw.String())
	}

	r.StartPrompt()
	fast := registry.Register(delegate.ActivityStart{ID: "fast"})
	fast.Finish(session.ChildStatusFailed, 0)
	r.StopProgress()
	if got := errw.String(); !strings.Contains(got, "[delegate d3 auto] started\n") ||
		!strings.Contains(got, "[delegate d3 auto] failed\n") {
		t.Fatalf("fast lifecycle output = %q", got)
	}
	if out.Len() != 0 {
		t.Fatalf("delegate lifecycle wrote stdout: %q", out.String())
	}
}

func TestDelegateLinesEraseAndRepaintLiveStatus(t *testing.T) {
	var out, errw bytes.Buffer
	feed := delegate.NewActivityFeed()
	registry := delegate.NewActivityRegistry(feed)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := NewRenderer(&out, &errw, RenderOptions{
		LiveStatus:   true,
		DelegateFeed: feed,
		Now:          func() time.Time { return now },
		Width:        func() int { return 120 },
	})
	r.StartPrompt()
	r.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	registration := registry.Register(delegate.ActivityStart{ID: "child"})
	registration.Finish(session.ChildStatusCompleted, 1)
	r.drainActivity()

	got := errw.String()
	lineAt := strings.Index(got, "[delegate d1 auto] started")
	if lineAt < 0 {
		t.Fatalf("delegate line missing from status output: %q", got)
	}
	if !strings.Contains(got[:lineAt], "\r\x1b[2K") ||
		!strings.Contains(got[lineAt:], "[turn: 1") {
		t.Fatalf("status was not erased and repainted around delegate line: %q", got)
	}
	r.StopProgress()
}

func TestQuietRendererDoesNotConsumeDelegateFeed(t *testing.T) {
	var out, errw bytes.Buffer
	feed := delegate.NewActivityFeed()
	registry := delegate.NewActivityRegistry(feed)
	r := NewRenderer(&out, &errw, RenderOptions{Quiet: true, DelegateFeed: feed, LiveStatus: true})
	r.StartPrompt()
	registration := registry.Register(delegate.ActivityStart{ID: "child"})
	registration.Finish(session.ChildStatusCompleted, 1)
	r.StopProgress()
	if out.Len() != 0 || errw.Len() != 0 {
		t.Fatalf("quiet delegate UI wrote output: stdout=%q stderr=%q", out.String(), errw.String())
	}
}
