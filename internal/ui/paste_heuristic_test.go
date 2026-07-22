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
	input, ok, _, err := readEditedInputTimedOutput(t, runes, gaps)
	return input, ok, err
}

func readEditedInputTimedOutput(t *testing.T, runes []rune, gaps []time.Duration) (replInput, bool, string, error) {
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
	input, ok, err := editor.read("> ")
	return input, ok, out.String(), err
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
	// Simulate a paste paced by an SSH connection: slower than the old 5ms
	// threshold, but still far faster than sustained human typing.
	gaps := burstGaps(len(runes), 8*time.Millisecond)
	gaps[len(gaps)-1] = 200 * time.Millisecond // Enter well after the burst exits paste mode

	input, ok, output, err := readEditedInputTimedOutput(t, runes, gaps)
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
	if strings.Contains(output, "bytes of pasted content") {
		t.Fatalf("short multiline fallback paste should render inline, got:\n%q", output)
	}
	if !strings.Contains(output, paste) {
		t.Fatalf("short multiline fallback paste missing from output, got:\n%q", output)
	}
}

// Single-line typing with realistic human inter-keystroke gaps (well above the
// paste-enter threshold) must not be mistaken for a paste: Enter submits
// normally and the line is treated as typed (not literal).
func TestPromptLineEditorTypedSingleLineNotDetectedAsPaste(t *testing.T) {
	runes := []rune("hello\r")
	gaps := burstGaps(len(runes), 20*time.Millisecond) // 20ms > 10ms enter threshold

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

// External-editor output is prefilled text, not a paste. Even if a rapid input
// burst immediately follows the editor handoff, the fallback paste heuristic
// must not collapse the existing text into a paste summary.
func TestPromptLineEditorExternalEditorPrefillRendersNormally(t *testing.T) {
	prefill := strings.Repeat("editor text ", pasteSummaryBytes/len("editor text ")+1)
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("xy\r"), &out)
	base := time.Unix(1_000_000, 0)
	clock := &scheduledClock{times: []time.Time{
		base,
		base.Add(time.Millisecond),
		base.Add(time.Millisecond + pasteExitGap + time.Millisecond),
	}}
	editor.configurePasteHeuristic(true, clock.now)

	input, ok, err := editor.readPrefilled("> ", prefill)
	if err != nil {
		t.Fatalf("readPrefilled = %v", err)
	}
	if !ok {
		t.Fatal("readPrefilled returned ok=false")
	}
	if input.text != prefill+"xy" {
		t.Fatalf("input text = %q, want editor prefill plus typed text", input.text)
	}
	if input.pasted {
		t.Fatal("editor prefill followed by input burst was marked as a pure paste")
	}
	if strings.Contains(out.String(), "bytes of pasted content") {
		t.Fatalf("editor prefill rendered as pasted content summary; got:\n%s", out.String())
	}
}

func TestPromptLineEditorPasteRenderingThreshold(t *testing.T) {
	tests := []struct {
		name       string
		pasted     string
		summarized bool
	}{
		{name: "short single line", pasted: "hi"},
		{name: "short multiline", pasted: "first line\nsecond line"},
		{name: "exactly 1000 ASCII bytes", pasted: strings.Repeat("x", pasteSummaryBytes)},
		{name: "1001 ASCII bytes", pasted: strings.Repeat("x", pasteSummaryBytes+1), summarized: true},
		{name: "exactly 1000 multibyte UTF-8 bytes", pasted: strings.Repeat("é", pasteSummaryBytes/len("é"))},
		{name: "1001 multibyte UTF-8 bytes", pasted: strings.Repeat("é", pasteSummaryBytes/len("é")) + "x", summarized: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			editor := newPromptLineEditor(strings.NewReader(bracketedPasteStart+tc.pasted+bracketedPasteEnd+"\r"), &out)

			input, ok, err := editor.read("> ")
			if err != nil {
				t.Fatalf("read = %v", err)
			}
			if !ok {
				t.Fatal("read returned ok=false")
			}
			if input.text != tc.pasted {
				t.Fatalf("input text length = %d, want %d (full content retained)", len(input.text), len(tc.pasted))
			}
			if !input.pasted {
				t.Fatal("input.pasted = false, want true")
			}

			placeholder := fmt.Sprintf(pasteSummaryPlaceholder, len(tc.pasted))
			if tc.summarized {
				if !strings.Contains(out.String(), placeholder) {
					t.Fatalf("output missing paste summary %q, got:\n%s", placeholder, out.String())
				}
				if strings.Contains(out.String(), tc.pasted) {
					t.Fatalf("%d-byte paste should render a summary, not the full body inline", len(tc.pasted))
				}
				return
			}
			if strings.Contains(out.String(), "bytes of pasted content") {
				t.Fatalf("%d-byte paste should render inline, not a summary; got:\n%s", len(tc.pasted), out.String())
			}
			if !strings.Contains(out.String(), tc.pasted) {
				t.Fatalf("%d-byte paste should render inline; got:\n%s", len(tc.pasted), out.String())
			}
		})
	}
}

func TestPromptLineEditorSeparateShortPastesStayInlinePastAggregateThreshold(t *testing.T) {
	first := strings.Repeat("a", 600)
	second := strings.Repeat("b", 600)
	want := "before " + first + " between " + second + " after"
	keys := "before " + bracketedPasteStart + first + bracketedPasteEnd + " between " + bracketedPasteStart + second + bracketedPasteEnd + " after\r"
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(keys), &out)

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok || input.text != want {
		t.Fatalf("input = %+v, ok=%v, want %q", input, ok, want)
	}
	if strings.Contains(out.String(), "bytes of pasted content") {
		t.Fatalf("separate sub-threshold pastes should stay inline, got:\n%q", out.String())
	}
	if !strings.Contains(out.String(), want) {
		t.Fatalf("final prompt display missing inline pasted ranges, got:\n%q", out.String())
	}
}

func TestPromptLineEditorOverThresholdPasteWithinTypedTextStaysCollapsed(t *testing.T) {
	prefix := "the daemon log says "
	pasted := "first failure\n" + strings.Repeat("x", pasteSummaryBytes)
	suffix := " and the dashboard still says Starting"
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(prefix+bracketedPasteStart+pasted+bracketedPasteEnd+suffix+"\x01X\r"), &out)

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if got, want := input.text, "X"+prefix+pasted+suffix; got != want {
		t.Fatalf("input text = %q, want %q", got, want)
	}
	if input.pasted {
		t.Fatal("prompt with authored surrounding text should not be marked as a pure paste")
	}
	placeholder := fmt.Sprintf(pasteSummaryPlaceholder, len(pasted))
	finalDisplay := "X" + prefix + placeholder + suffix
	if !strings.Contains(out.String(), finalDisplay) {
		t.Fatalf("final prompt display missing persistent collapsed paste %q, got:\n%q", finalDisplay, out.String())
	}
	if strings.Contains(out.String(), pasted) {
		t.Fatalf("pasted body should stay collapsed while surrounding text is edited, got:\n%q", out.String())
	}
}

func TestPromptLineEditorOverThresholdFallbackPasteWithinTypedTextStaysCollapsed(t *testing.T) {
	prefix := "prefix "
	pasted := "first\n" + strings.Repeat("x", pasteSummaryBytes)
	suffix := " suffix"
	runes := []rune(prefix + pasted + suffix + "\r")
	gaps := burstGaps(len(runes), 20*time.Millisecond)
	pasteStart := len([]rune(prefix))
	pasteEnd := pasteStart + len([]rune(pasted))
	gaps[pasteStart] = pasteExitGap + time.Millisecond
	for i := pasteStart + 1; i < pasteEnd; i++ {
		gaps[i] = time.Millisecond
	}
	gaps[pasteEnd] = pasteExitGap + time.Millisecond

	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(string(runes)), &out)
	clock := &scheduledClock{}
	when := time.Unix(1_000_000, 0)
	for i := range runes {
		if i > 0 {
			when = when.Add(gaps[i])
		}
		clock.times = append(clock.times, when)
	}
	editor.configurePasteHeuristic(true, clock.now)

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if got, want := input.text, prefix+pasted+suffix; got != want {
		t.Fatalf("input text = %q, want %q", got, want)
	}
	placeholder := fmt.Sprintf(pasteSummaryPlaceholder, len(pasted))
	if !strings.Contains(out.String(), prefix+placeholder+suffix) {
		t.Fatalf("fallback paste did not stay collapsed within typed text, got:\n%q", out.String())
	}
}

func TestPromptLineEditorExternalEditorPrefillExpandsCollapsedPaste(t *testing.T) {
	prefix := "before "
	pasted := "one\n" + strings.Repeat("x", pasteSummaryBytes)
	suffix := " after"
	full := prefix + pasted + suffix
	var pastedOut bytes.Buffer
	pastedEditor := newPromptLineEditor(strings.NewReader(prefix+bracketedPasteStart+pasted+bracketedPasteEnd+suffix+string(rune(lineTermEdit))), &pastedOut)

	editInput, ok, err := pastedEditor.read("> ")
	if err != nil {
		t.Fatalf("read pasted prompt = %v", err)
	}
	if !ok || !editInput.edit || editInput.text != full {
		t.Fatalf("edit input = %+v, ok=%v, want full expanded editor text %q", editInput, ok, full)
	}
	placeholder := fmt.Sprintf(pasteSummaryPlaceholder, len(pasted))
	if !strings.Contains(pastedOut.String(), prefix+placeholder+suffix) {
		t.Fatalf("paste should be collapsed before opening the editor, got:\n%q", pastedOut.String())
	}

	var prefillOut bytes.Buffer
	prefillEditor := newPromptLineEditor(strings.NewReader("\r"), &prefillOut)
	input, ok, err := prefillEditor.readPrefilled("> ", editInput.text)
	if err != nil {
		t.Fatalf("read editor prefill = %v", err)
	}
	if !ok || input.text != full {
		t.Fatalf("prefill input = %+v, ok=%v, want %q", input, ok, full)
	}
	if strings.Contains(prefillOut.String(), placeholder) || !strings.Contains(prefillOut.String(), full) {
		t.Fatalf("external-editor prefill should render expanded, got:\n%q", prefillOut.String())
	}
}

func TestLineEditStateOverThresholdCollapsedPasteIsAtomic(t *testing.T) {
	s := lineEditState{buf: []rune("before  after"), cursor: len([]rune("before "))}
	pasted := "one\n" + strings.Repeat("x", pasteSummaryBytes)
	s.insertPastedText(pasted)
	placeholder := fmt.Sprintf(pasteSummaryPlaceholder, len(pasted))
	if got, want := string(s.displayRunes()), "before "+placeholder+" after"; got != want {
		t.Fatalf("display = %q, want %q", got, want)
	}

	pasteEnd := s.cursor
	s.left()
	pasteStart := s.cursor
	if pasteStart >= pasteEnd {
		t.Fatalf("left did not jump over collapsed paste: start=%d end=%d", pasteStart, pasteEnd)
	}
	s.right()
	if s.cursor != pasteEnd {
		t.Fatalf("right cursor = %d, want paste end %d", s.cursor, pasteEnd)
	}
	s.backspace()
	if got, want := string(s.buf), "before  after"; got != want {
		t.Fatalf("backspace buffer = %q, want atomic paste deletion %q", got, want)
	}
	if len(s.pasteSummaries) != 0 {
		t.Fatalf("paste summaries remain after atomic deletion: %+v", s.pasteSummaries)
	}
}

func TestPromptLineEditorTracksMultipleOverThresholdCollapsedPastes(t *testing.T) {
	first := "one\n" + strings.Repeat("x", pasteSummaryBytes)
	second := "two\n" + strings.Repeat("y", pasteSummaryBytes)
	inputText := "a" + first + "b" + second + "c"
	keys := "a" + bracketedPasteStart + first + bracketedPasteEnd + "b" + bracketedPasteStart + second + bracketedPasteEnd + "c\r"
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(keys), &out)

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok || input.text != inputText {
		t.Fatalf("input = %+v, ok=%v, want %q", input, ok, inputText)
	}
	display := "a" + fmt.Sprintf(pasteSummaryPlaceholder, len(first)) + "b" + fmt.Sprintf(pasteSummaryPlaceholder, len(second)) + "c"
	if !strings.Contains(out.String(), display) {
		t.Fatalf("final prompt display missing multiple collapsed pastes %q, got:\n%q", display, out.String())
	}
}

func TestViOverThresholdCollapsedPasteMovementAndDeletionAreAtomic(t *testing.T) {
	s := lineEditState{buf: []rune("ab"), cursor: 1}
	pasted := "one\n" + strings.Repeat("x", pasteSummaryBytes)
	s.insertPastedText(pasted)
	span := s.pasteSummaries[0]

	v := viLineState{mode: viModeInsert}
	v.enterNormal(&s)
	if s.cursor != span.start {
		t.Fatalf("normal-mode cursor = %d, want collapsed paste start %d", s.cursor, span.start)
	}
	s.viRight()
	if s.cursor != span.end {
		t.Fatalf("vi right cursor = %d, want suffix at paste end %d", s.cursor, span.end)
	}
	s.viLeft()
	if s.cursor != span.start {
		t.Fatalf("vi left cursor = %d, want collapsed paste start %d", s.cursor, span.start)
	}

	editor := newPromptLineEditor(strings.NewReader(""), &bytes.Buffer{})
	editor.viDeleteChars(&s, 1)
	if got, want := string(s.buf), "ab"; got != want {
		t.Fatalf("vi delete buffer = %q, want atomic paste deletion %q", got, want)
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
