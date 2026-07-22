package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

// The prompt runs in raw mode (ICRNL cleared), so a terminal delivers a paste's
// line breaks as bare CR ("\r"). Those must be normalized to "\n"; leaving them
// raw corrupts the prompt line, defeats newline-based paste line counting, and
// leaks "^M" into the external editor. Regression test for the reported bug.
func TestNormalizePastedNewlines(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"bare CR", "a\rb\rc", "a\nb\nc"},
		{"CRLF", "a\r\nb\r\nc", "a\nb\nc"},
		{"LF unchanged", "a\nb\nc", "a\nb\nc"},
		{"mixed CR and CRLF", "a\rb\r\nc\nd", "a\nb\nc\nd"},
		{"trailing CR", "a\r", "a\n"},
		{"no line breaks", "abc", "abc"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePastedNewlines(tc.in); got != tc.want {
				t.Fatalf("normalizePastedNewlines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The literal example from the bug report: a six-line paste whose lines are
// separated by bare CR must render inline with normalized newlines and keep that
// newline-separated content in the real buffer.
func TestPromptLineEditorReportCRPasteRendersInlineAndSubmitsNewlines(t *testing.T) {
	pastedCR := "pasted line 1\rpasted line 2\rpasted line 3\rpasted line 4\rpasted line 5\rpasted line 6"
	want := "pasted line 1\npasted line 2\npasted line 3\npasted line 4\npasted line 5\npasted line 6"

	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(bracketedPasteStart+pastedCR+bracketedPasteEnd+"\r"), &out)
	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != want {
		t.Fatalf("input text = %q, want %q", input.text, want)
	}
	if strings.ContainsRune(input.text, '\r') {
		t.Fatalf("input text still contains a carriage return: %q", input.text)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true")
	}
	if strings.Contains(out.String(), "bytes of pasted content") {
		t.Fatalf("short normalized multiline paste rendered as a summary:\n%q", out.String())
	}
	if !strings.Contains(out.String(), want) {
		t.Fatalf("normalized multiline paste was not rendered inline:\n%q", out.String())
	}
	if strings.Contains(out.String(), pastedCR) || strings.Contains(out.String(), "pasted line 6\r") {
		t.Fatalf("raw CR-separated paste body was rendered to the terminal:\n%q", out.String())
	}
}

// A CRLF-separated paste (some terminals/emulators) must collapse each CRLF to a
// single "\n", not two line breaks.
func TestPromptLineEditorCRLFPasteNormalizedToNewlines(t *testing.T) {
	pastedCRLF := "line 1\r\nline 2\r\nline 3"
	want := "line 1\nline 2\nline 3"

	input, ok, err := readEditedInput(t, bracketedPasteStart+pastedCRLF+bracketedPasteEnd+"\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != want {
		t.Fatalf("input text = %q, want %q", input.text, want)
	}
	if strings.ContainsRune(input.text, '\r') {
		t.Fatalf("input text still contains a carriage return: %q", input.text)
	}
}

// A CR-separated paste into a non-empty prompt inserts newline-normalized text at
// the cursor (exercises the insertString branch of lineEditPaste rather than the
// empty-prompt fill).
func TestPromptLineEditorCRPasteInsertsAtCursorNormalized(t *testing.T) {
	// "ab", cursor left one (between a and b), paste "X\rY", submit.
	input, ok, err := readEditedInput(t, "ab\x1b[D"+bracketedPasteStart+"X\rY"+bracketedPasteEnd+"\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "aX\nYb" {
		t.Fatalf("input text = %q, want %q", input.text, "aX\nYb")
	}
	if strings.ContainsRune(input.text, '\r') {
		t.Fatalf("input text still contains a carriage return: %q", input.text)
	}
}

// Ctrl-G / the external-editor path must receive the full normalized content,
// not raw CRs that vim would display as ^M.
func TestPromptLineEditorCtrlGOnCRPasteReturnsNormalizedDraft(t *testing.T) {
	pastedCR := "multi\rline\rpaste"
	want := "multi\nline\npaste"

	input, ok, err := readEditedInput(t, bracketedPasteStart+pastedCR+bracketedPasteEnd+string(rune(lineTermEdit)))
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.edit {
		t.Fatal("input.edit = false, want true (Ctrl-G opens the editor)")
	}
	if input.text != want {
		t.Fatalf("editor draft = %q, want %q", input.text, want)
	}
	if strings.ContainsRune(input.text, '\r') {
		t.Fatalf("editor draft still contains a carriage return: %q", input.text)
	}
}

// The non-bracketed paste fallback sees raw runes one by one. CRLF line endings
// must become one newline per pasted line, not two.
func TestPromptLineEditorFallbackCRLFPasteCollapsesLineEndings(t *testing.T) {
	paste := "line1\r\nline2\r\nline3"
	want := "line1\nline2\nline3"
	runes := []rune(paste + "\r")
	gaps := burstGaps(len(runes), pasteEnterGap/2)
	gaps[len(gaps)-1] = pasteExitGap + time.Millisecond

	input, ok, err := readEditedInputTimed(t, runes, gaps)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != want {
		t.Fatalf("input text = %q, want %q", input.text, want)
	}
	if strings.ContainsRune(input.text, '\r') {
		t.Fatalf("input text still contains a carriage return: %q", input.text)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true")
	}
}

// The non-TTY reader path (no prompt editor) also assembles bracketed pastes; a
// CR-separated paste submitted through the full REPL must reach the model as
// newline-separated text.
func TestREPLNonTTYCRPasteNormalizedToNewlines(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	pastedCR := "one\rtwo\rthree"
	want := "one\ntwo\nthree"
	in := strings.NewReader(bracketedPasteStart + pastedCR + bracketedPasteEnd + "\n/exit\n")
	if code := run(in, app, nil, false); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("bracketed paste should send one prompt, got %d requests", fp.RequestCount())
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != want {
		t.Fatalf("pasted prompt = %q, want %q", got, want)
	}
}
