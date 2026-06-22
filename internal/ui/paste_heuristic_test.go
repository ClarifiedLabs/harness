package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// scheduledClock returns pre-seeded times, one per now() call, so the
// paste-burst heuristic can be driven deterministically without real sleeps.
type scheduledClock struct {
	times []time.Time
	next  int
}

func (c *scheduledClock) now() time.Time {
	if len(c.times) == 0 {
		return time.Time{}
	}
	if c.next >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	t := c.times[c.next]
	c.next++
	return t
}

// readEditedInputTimed drives the editor with a sequence of runes, advancing a
// fake clock by gaps[i] before rune i (gaps[0] is unused). Each rune is one
// now() call (the editor calls now once per ReadRune on the plain-rune path).
// The non-bracketed paste-burst heuristic is enabled so a fast burst is
// recognized as a paste and a slow one as human typing.
func readEditedInputTimed(t *testing.T, runes []rune, gaps []time.Duration) (replInput, bool, error) {
	t.Helper()
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(string(runes)), &out)
	clock := &scheduledClock{}
	// Start from a non-zero base so the editor's lastKeyTime.IsZero() sentinel
	// ("no previous keystroke") is not confused with a keystroke at time zero.
	acc := time.Unix(1_000_000, 0)
	for i := range runes {
		if i > 0 && i < len(gaps) {
			acc = acc.Add(gaps[i])
		}
		clock.times = append(clock.times, acc)
	}
	editor.configurePasteHeuristic(true, clock.now)
	return editor.read("> ")
}

// burstGaps returns a gaps slice of length n where every entry is gap, suitable
// for the bytes of a paste burst that arrive with a uniform small inter-byte
// delay. The first entry (index 0) is always 0 (no predecessor).
func burstGaps(n int, gap time.Duration) []time.Duration {
	gaps := make([]time.Duration, n)
	for i := range gaps {
		if i == 0 {
			continue
		}
		gaps[i] = gap
	}
	return gaps
}

// A non-bracketed multi-line paste arrives as a fast burst of bytes with no
// bracketed-paste markers. The heuristic detects the burst, so embedded
// newlines insert instead of submitting; a later Enter (after the burst gap)
// submits the whole thing as one literal prompt with newlines preserved.
func TestPromptLineEditorNonBracketedPasteBurstSubmitsAsOnePrompt(t *testing.T) {
	paste := "line1\nline2\nline3"
	runes := []rune(paste + "\r")
	gaps := burstGaps(len(runes), time.Millisecond)
	gaps[len(gaps)-1] = 200 * time.Millisecond // Enter well after the burst exits paste mode

	input, ok, err := readEditedInputTimed(t, runes, gaps)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != paste {
		t.Fatalf("input text = %q, want %q (newlines preserved, one prompt)", input.text, paste)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true (pure paste submits literally)")
	}
}

// Single-line typing with realistic human inter-keystroke gaps (well above the
// paste-enter threshold) must not be mistaken for a paste: Enter submits
// normally and the line is treated as typed (not literal).
func TestPromptLineEditorTypedSingleLineNotDetectedAsPaste(t *testing.T) {
	runes := []rune("hello\r")
	gaps := burstGaps(len(runes), 20*time.Millisecond) // 20ms > 5ms enter threshold

	input, ok, err := readEditedInputTimed(t, runes, gaps)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "hello" {
		t.Fatalf("input text = %q, want hello", input.text)
	}
	if input.pasted {
		t.Fatalf("input.pasted = true, want false (typed line is not a paste)")
	}
}

// A pure paste of a bang line is submitted literally (no shell escape). The
// pure-paste flag is set when the burst fills an empty buffer.
func TestPromptLineEditorPurePasteBangIsLiteral(t *testing.T) {
	paste := "!echo foo"
	runes := []rune(paste + "\r")
	gaps := burstGaps(len(runes), time.Millisecond)
	gaps[len(gaps)-1] = 200 * time.Millisecond

	input, ok, err := readEditedInputTimed(t, runes, gaps)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != paste {
		t.Fatalf("input text = %q, want %q", input.text, paste)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true (pure paste is literal)")
	}
}

// Any manual keystroke after a paste clears the pure-paste literal guarantee,
// so the whole submitted line is treated as typed. Here a paste of "!echo foo"
// followed by typing "X" submits as typed text, not a literal paste.
func TestPromptLineEditorTypingAfterPasteClearsLiteralFlag(t *testing.T) {
	runes := []rune("!echo fooX\r")
	gaps := burstGaps(len(runes), time.Millisecond)
	// Bytes 1..9 are the paste burst; byte 10 ('X') is a manual keystroke after
	// a long gap (exits paste mode and clears purePaste); Enter follows.
	gaps[9] = 200 * time.Millisecond
	gaps[10] = 20 * time.Millisecond

	input, ok, err := readEditedInputTimed(t, runes, gaps)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "!echo fooX" {
		t.Fatalf("input text = %q, want !echo fooX", input.text)
	}
	if input.pasted {
		t.Fatalf("input.pasted = true, want false (typing after paste makes the line typed)")
	}
}

// A pure paste of a /command is submitted literally (no meta-command dispatch).
func TestPromptLineEditorPurePasteSlashCommandIsLiteral(t *testing.T) {
	paste := "/exit is text"
	runes := []rune(paste + "\r")
	gaps := burstGaps(len(runes), time.Millisecond)
	gaps[len(gaps)-1] = 200 * time.Millisecond

	input, ok, err := readEditedInputTimed(t, runes, gaps)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != paste {
		t.Fatalf("input text = %q, want %q", input.text, paste)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true (pure paste is literal, no /command dispatch)")
	}
}

// A large paste into an empty prompt renders a one-line placeholder instead of
// the full content inline (avoiding scroll lag), while the real content is
// retained in the buffer and submitted on Enter.
func TestPromptLineEditorLargePasteRendersSummary(t *testing.T) {
	large := strings.Repeat("x", pasteSummaryBytes+1)
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(bracketedPasteStart+large+bracketedPasteEnd+"\r"), &out)

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != large {
		t.Fatalf("input text length = %d, want %d (full content retained)", len(input.text), len(large))
	}
	placeholder := fmt.Sprintf(pasteSummaryPlaceholder, len(large))
	if !strings.Contains(out.String(), placeholder) {
		t.Fatalf("output missing large-paste summary %q, got:\n%s", placeholder, out.String())
	}
	// The placeholder replaces the inline content, so the huge body must not be
	// rendered verbatim.
	if strings.Contains(out.String(), large) {
		t.Fatalf("large paste should render a summary, not the full %d-byte body inline", len(large))
	}
}

// A small single-line paste renders inline (no summary placeholder).
func TestPromptLineEditorSmallPasteRendersInline(t *testing.T) {
	small := "hi"
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(bracketedPasteStart+small+bracketedPasteEnd+"\r"), &out)

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != small {
		t.Fatalf("input text = %q, want %q", input.text, small)
	}
	if strings.Contains(out.String(), "bytes of pasted content") {
		t.Fatalf("small paste should render inline, not a summary; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), small) {
		t.Fatalf("small paste should render inline; got:\n%s", out.String())
	}
}

// Ctrl-G on a pasted buffer opens the external editor with the FULL pasted
// content; on submit from the editor it is treated as edited/typed.
func TestPromptLineEditorCtrlGOnPastedBufferOpensEditorWithFullContent(t *testing.T) {
	pasted := "multi\nline\npaste"
	input, ok, err := readEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd+string(rune(lineTermEdit)))
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.edit {
		t.Fatal("input.edit = false, want true (Ctrl-G opens the editor)")
	}
	if input.text != pasted {
		t.Fatalf("input text = %q, want full pasted content %q", input.text, pasted)
	}
}

// Bracketed paste and the non-bracketed heuristic both route through the same
// fill-buffer + literal flow: a bracketed paste into an empty prompt fills the
// buffer (not auto-submit) and is submitted literally on Enter.
func TestPromptLineEditorBracketedPasteFillsBufferAndSubmitsLiteralOnEnter(t *testing.T) {
	pasted := "/exit is text\nsecond line"
	input, ok, err := readEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd+"\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.pasted || input.text != pasted {
		t.Fatalf("input = %+v, want pasted %q (bracketed paste fills buffer, Enter submits literally)", input, pasted)
	}
}

// vi normal-mode submit must honor the pure-paste literal guarantee: a pure
// paste that fills an empty prompt then Esc (into normal mode) then Enter submits
// literally, the same as the insert-mode Enter path. Regression test for the
// viSubmit path that previously dropped pasted (defaulted to false), so a
// pasted bang line was dispatched as a shell escape. Covers bang content.
func TestPromptLineEditorViPasteEscEnterSubmitsBangLiteral(t *testing.T) {
	pasted := "!echo foo"
	// bracketed paste fills the empty buffer in insert mode (sets purePaste),
	// Esc enters normal mode (carrying the flag), Enter submits via viSubmit.
	input, ok, err := readViEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd+"\x1b\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != pasted {
		t.Fatalf("input text = %q, want %q", input.text, pasted)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true (vi normal-mode Enter after a pure paste submits literally; a pasted ! line must not dispatch as a shell escape)")
	}
}

// Same guarantee for /command content: a pure paste of a /command submitted
// from vi normal mode (Esc then Enter) is literal, with no meta-command
// dispatch. Regression test for the viSubmit path.
func TestPromptLineEditorViPasteEscEnterSubmitsSlashCommandLiteral(t *testing.T) {
	pasted := "/exit is text"
	input, ok, err := readViEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd+"\x1b\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != pasted {
		t.Fatalf("input text = %q, want %q", input.text, pasted)
	}
	if !input.pasted {
		t.Fatalf("input.pasted = false, want true (vi normal-mode Enter after a pure /command paste submits literally; no /command dispatch)")
	}
}

// The pure-paste flag survives Esc into normal mode (so paste+Esc+Enter is
// literal) but is cleared by any manual vi normal-mode keystroke, honoring the
// "any manual keystroke clears the mark" rule. A motion ('l' right) after the
// paste makes the whole line typed, so Enter dispatches !/command as authored.
// The flag is NOT cleared by the mode switch itself, only by the edit/motion.
func TestPromptLineEditorViPasteEscNormalThenMotionClearsLiteralFlag(t *testing.T) {
	pasted := "!echo foo"
	// paste (purePaste=true), Esc (normal, flag carried), 'l' motion (manual
	// keystroke -> markManualEdit clears purePaste), Enter submits as typed.
	input, ok, err := readViEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd+"\x1bl\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != pasted {
		t.Fatalf("input text = %q, want %q (motion does not edit content)", input.text, pasted)
	}
	if input.pasted {
		t.Fatalf("input.pasted = true, want false (a manual vi motion after a pure paste clears the literal flag; the line is typed and a ! line dispatches as authored)")
	}
}

// A vi normal-mode delete after a pure paste also clears the literal flag (a
// mutating manual keystroke), and the deletion is reflected in the submitted
// text. Confirms markManualEdit is wired into the delete path, not just motions.
func TestPromptLineEditorViPasteEscNormalThenDeleteClearsLiteralFlag(t *testing.T) {
	pasted := "!echo foo"
	// paste, Esc (normal), 'x' deletes the last char and clears purePaste, Enter.
	input, ok, err := readViEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd+"\x1bx\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "!echo fo" {
		t.Fatalf("input text = %q, want !echo fo (vi 'x' deleted the last char)", input.text)
	}
	if input.pasted {
		t.Fatalf("input.pasted = true, want false (a manual vi delete after a pure paste clears the literal flag)")
	}
}
