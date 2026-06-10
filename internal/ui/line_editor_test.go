package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func readEditedInput(t *testing.T, input string) (replInput, bool, error) {
	t.Helper()
	var out bytes.Buffer
	return newPromptLineEditor(strings.NewReader(input), &out).read("> ")
}

func readEditedInputs(t *testing.T, input string, count int) []replInput {
	t.Helper()
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(input), &out)
	inputs := make([]replInput, 0, count)
	for range count {
		input, ok, err := editor.read("> ")
		if err != nil {
			t.Fatalf("read = %v", err)
		}
		if !ok {
			t.Fatal("read returned ok=false")
		}
		inputs = append(inputs, input)
	}
	return inputs
}

func readViEditedInput(t *testing.T, input string) (replInput, bool, error) {
	t.Helper()
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader(input), &out)
	editor.setEditMode("vi")
	return editor.read("> ")
}

func TestPromptLineEditorInsertsAtCursor(t *testing.T) {
	input, ok, err := readEditedInput(t, "abc\x1b[D\x1b[DX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "aXbc" {
		t.Fatalf("input text = %q, want aXbc", input.text)
	}
}

func TestPromptLineEditorBackspaceDeletesBeforeCursor(t *testing.T) {
	input, ok, err := readEditedInput(t, "abc\x1b[D\x7f\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ac" {
		t.Fatalf("input text = %q, want ac", input.text)
	}
}

func TestPromptLineEditorDeleteDeletesAtCursor(t *testing.T) {
	input, ok, err := readEditedInput(t, "abc\x1b[D\x1b[3~\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ab" {
		t.Fatalf("input text = %q, want ab", input.text)
	}
}

func TestPromptLineEditorCursorBoundariesAreNoops(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[Da\x1b[C\x1b[CX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "aX" {
		t.Fatalf("input text = %q, want aX", input.text)
	}
}

func TestPromptLineEditorCtrlAMovesToStart(t *testing.T) {
	input, ok, err := readEditedInput(t, "abc\x01X\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "Xabc" {
		t.Fatalf("input text = %q, want Xabc", input.text)
	}
}

func TestPromptLineEditorCtrlEMovesToEnd(t *testing.T) {
	input, ok, err := readEditedInput(t, "abc\x01X\x05Y\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "XabcY" {
		t.Fatalf("input text = %q, want XabcY", input.text)
	}
}

func TestPromptLineEditorCtrlBCtrlFAreLeftRightAliases(t *testing.T) {
	input, ok, err := readEditedInput(t, "ab\x02X\x06Y\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "aXbY" {
		t.Fatalf("input text = %q, want aXbY", input.text)
	}
}

func TestPromptLineEditorHomeEndKeysMoveCursor(t *testing.T) {
	tests := []struct {
		name string
		home string
		end  string
	}{
		{name: "CSI", home: "\x1b[H", end: "\x1b[F"},
		{name: "SS3", home: "\x1bOH", end: "\x1bOF"},
		{name: "tilde 1 4", home: "\x1b[1~", end: "\x1b[4~"},
		{name: "tilde 7 8", home: "\x1b[7~", end: "\x1b[8~"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, ok, err := readEditedInput(t, "abc"+tt.home+"X"+tt.end+"Y\r")
			if err != nil {
				t.Fatalf("read = %v", err)
			}
			if !ok {
				t.Fatal("read returned ok=false")
			}
			if input.text != "XabcY" {
				t.Fatalf("input text = %q, want XabcY", input.text)
			}
		})
	}
}

func TestPromptLineEditorCSIuCtrlAMovesToStartAndCtrlEMovesToEnd(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[97;;97u\x1b[98;;98u\x1b[99;;99u\x1b[97;5u\x1b[88;;88u\x1b[101;5u\x1b[89;;89u\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "XabcY" {
		t.Fatalf("input text = %q, want XabcY", input.text)
	}
}

func TestPromptLineEditorArrowUpRecallsHistory(t *testing.T) {
	inputs := readEditedInputs(t, "first\rsecond\r\x1b[A\r", 3)

	if inputs[2].text != "second" {
		t.Fatalf("history recall = %q, want second", inputs[2].text)
	}
}

func TestPromptLineEditorArrowUpDownRestoresDraft(t *testing.T) {
	inputs := readEditedInputs(t, "first\rsecond\rdra\x1b[A\x1b[A\x1b[B\x1b[Bft\r", 3)

	if inputs[2].text != "draft" {
		t.Fatalf("draft after history navigation = %q, want draft", inputs[2].text)
	}
}

func TestPromptLineEditorRecalledHistoryCanBeEdited(t *testing.T) {
	inputs := readEditedInputs(t, "hello\r\x1b[A!\r", 2)

	if inputs[1].text != "hello!" {
		t.Fatalf("edited history recall = %q, want hello!", inputs[1].text)
	}
}

func TestPromptLineEditorSS3ArrowUpRecallsHistory(t *testing.T) {
	inputs := readEditedInputs(t, "first\r\x1bOA\r", 2)

	if inputs[1].text != "first" {
		t.Fatalf("SS3 history recall = %q, want first", inputs[1].text)
	}
}

func TestPromptLineEditorIsRuneAware(t *testing.T) {
	input, ok, err := readEditedInput(t, "aé\x1b[DX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "aXé" {
		t.Fatalf("input text = %q, want aXé", input.text)
	}
}

func TestPromptLineEditorShiftEnterCSIuInsertsNewline(t *testing.T) {
	input, ok, err := readEditedInput(t, "first\x1b[13;2usecond\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "first\nsecond" {
		t.Fatalf("input text = %q, want first\\nsecond", input.text)
	}
}

func TestPromptLineEditorKittyAllKeysTextShiftEnterAndEnter(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[102;;102u\x1b[111;;111u\x1b[111;;111u\x1b[13;2u\x1b[98;;98u\x1b[97;;97u\x1b[114;;114u\x1b[13uignored")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "foo\nbar" {
		t.Fatalf("input text = %q, want foo\\nbar", input.text)
	}
}

func TestPromptLineEditorKittyShiftEnterOnEmptyPromptInsertsNewline(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[13;2u\x1b[120;;120u\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "\nx" {
		t.Fatalf("input text = %q, want \\nx", input.text)
	}
}

func TestPromptLineEditorRawLFInsertsNewlineAndRawCRSubmits(t *testing.T) {
	input, ok, err := readEditedInput(t, "foo\nbar\rignored")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "foo\nbar" {
		t.Fatalf("input text = %q, want foo\\nbar", input.text)
	}
}

func TestPromptLineEditorITerm2ShiftEnterModifierEventInsertsNewline(t *testing.T) {
	input, ok, err := readEditedInput(t, "foo\x1b[57441;2u\nbar\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "foo\nbar" {
		t.Fatalf("input text = %q, want foo\\nbar", input.text)
	}
}

func TestPromptLineEditorITerm2ShiftEnterOnEmptyPromptInsertsNewline(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[57441;2u\nx\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "\nx" {
		t.Fatalf("input text = %q, want \\nx", input.text)
	}
}

func TestPromptLineEditorITerm2ConsecutiveShiftEntersInsertNewlines(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[57441;2u\n\x1b[57441;2u\nx\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "\n\nx" {
		t.Fatalf("input text = %q, want \\n\\nx", input.text)
	}
}

func TestPromptLineEditorShiftModifierDoesNotAffectLaterEnterAfterText(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[57441;2u\x1b[120;;120u\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "x" {
		t.Fatalf("input text = %q, want x", input.text)
	}
}

func TestPromptLineEditorRedrawNoWidthClearsMultilinePromptRows(t *testing.T) {
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("\x1b[57441;2u\n\x1b[102;;102u\x1b[13u"), &out)
	editor.columns = func() int { return 0 }

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "\nf" {
		t.Fatalf("input text = %q, want \\nf", input.text)
	}

	screen := newPromptTestScreen(4, 20, 0)
	screen.feed(out.String())
	lines := screen.visibleLines()
	if lines[0] != ">" || lines[1] != "f" {
		t.Fatalf("final screen = %#v, want prompt newline plus f", lines)
	}
}

func TestPromptLineEditorRedrawNoWidthClearsMultilinePromptRowsFromPrompt(t *testing.T) {
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("f\r"), &out)
	editor.columns = func() int { return 0 }

	input, ok, err := editor.read("ctx\n> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "f" {
		t.Fatalf("input text = %q, want f", input.text)
	}

	screen := newPromptTestScreen(4, 20, 0)
	screen.feed(out.String())
	lines := screen.visibleLines()
	if lines[0] != "ctx" || lines[1] != "> f" {
		t.Fatalf("final screen = %#v, want multiline prompt with f", lines)
	}
}

func TestPromptLineEditorShiftEnterXTermModifiedKeyInsertsNewline(t *testing.T) {
	input, ok, err := readEditedInput(t, "first\x1b[27;2;13~second\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "first\nsecond" {
		t.Fatalf("input text = %q, want first\\nsecond", input.text)
	}
}

func TestPromptLineEditorShiftEnterTildeKeyInsertsNewline(t *testing.T) {
	input, ok, err := readEditedInput(t, "first\x1b[13;2~second\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "first\nsecond" {
		t.Fatalf("input text = %q, want first\\nsecond", input.text)
	}
}

func TestPromptLineEditorCSIuEnterSubmits(t *testing.T) {
	input, ok, err := readEditedInput(t, "submit me\x1b[13uignored")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "submit me" {
		t.Fatalf("input text = %q, want submit me", input.text)
	}
}

func TestPromptLineEditorShiftEnterWithTrailingCRDoesNotSubmit(t *testing.T) {
	// Simulates a terminal that erroneously sends a raw CR after the
	// CSI u escape sequence for Shift-Enter. The stray CR must be
	// ignored; the buffer should accept more text before the final
	// plain-Enter submission.
	inputs := readEditedInputs(t, "hello\x1b[13;2u\rworld\rnext\r", 2)

	if inputs[0].text != "hello\nworld" {
		t.Fatalf("input text = %q, want hello\\nworld", inputs[0].text)
	}
	if inputs[1].text != "next" {
		t.Fatalf("second input text = %q, want next", inputs[1].text)
	}
}

func TestPromptLineEditorModifiedBackspaceDeletesAtCursor(t *testing.T) {
	input, ok, err := readEditedInput(t, "abc\x1b[D\x1b[127;2u\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ab" {
		t.Fatalf("input text = %q, want ab", input.text)
	}
}

func TestPromptLineEditorKittyPlainBackspaceDeletesBeforeCursor(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[97;;97u\x1b[98;;98u\x1b[1D\x1b[127u\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "b" {
		t.Fatalf("input text = %q, want b", input.text)
	}
}

func TestPromptLineEditorCtrlGReturnsEditInputWithDraft(t *testing.T) {
	input, ok, err := readEditedInput(t, "draft\a")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.edit || input.text != "draft" {
		t.Fatalf("input = %+v, want edit draft", input)
	}
}

func TestPromptLineEditorKittyCtrlGReturnsEditInputWithDraft(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[100;;100u\x1b[114;;114u\x1b[97;;97u\x1b[102;;102u\x1b[116;;116u\x1b[103;5u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.edit || input.text != "draft" {
		t.Fatalf("input = %+v, want edit draft", input)
	}
}

func TestPromptLineEditorCtrlDOnEmptyReturnsEOF(t *testing.T) {
	_, ok, err := readEditedInput(t, "\x04")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if ok {
		t.Fatal("Ctrl-D on empty input should return ok=false")
	}
}

func TestPromptLineEditorKittyCtrlDOnEmptyReturnsEOF(t *testing.T) {
	_, ok, err := readEditedInput(t, "\x1b[100;5u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if ok {
		t.Fatal("Ctrl-D on empty input should return ok=false")
	}
}

func TestPromptLineEditorCtrlDWithTextIsIgnored(t *testing.T) {
	input, ok, err := readEditedInput(t, "a\x04b\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ab" {
		t.Fatalf("input text = %q, want ab", input.text)
	}
}

func TestPromptLineEditorKittyCtrlDWithTextIsIgnored(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[97;;97u\x1b[100;5u\x1b[98;;98u\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ab" {
		t.Fatalf("input text = %q, want ab", input.text)
	}
}

func TestPromptLineEditorKittyCtrlCInterrupts(t *testing.T) {
	input, ok, err := readEditedInput(t, "\x1b[99;5u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.interrupt {
		t.Fatalf("input = %+v, want interrupt", input)
	}
}

func TestPromptLineEditorInputTraceWritesRawAndCSIEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.log")
	t.Setenv(replInputTraceEnv, path)

	input, ok, err := readEditedInput(t, "\x1b[120;;120u\x1b[13;2u\x1b[13u")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok || input.text != "x\n" {
		t.Fatalf("input = %+v, ok=%v, want x\\n", input, ok)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	trace := string(data)
	for _, want := range []string{
		`csi raw seq="120;;120u"`,
		`action=insert-text`,
		`csi raw seq="13;2u"`,
		`action=insert-newline`,
		`csi raw seq="13u"`,
		`action=submit`,
	} {
		if !strings.Contains(trace, want) {
			t.Fatalf("trace missing %q:\n%s", want, trace)
		}
	}
}

func TestPromptLineEditorBracketedPasteSubmitsEmptyPrompt(t *testing.T) {
	pasted := "/exit is text\nsecond line"
	input, ok, err := readEditedInput(t, bracketedPasteStart+pasted+bracketedPasteEnd)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if !input.pasted || input.text != pasted {
		t.Fatalf("input = %+v, want pasted %q", input, pasted)
	}
}

func TestPromptLineEditorMultilinePasteIsNotAddedToHistory(t *testing.T) {
	pasted := "first line\nsecond line"
	inputs := readEditedInputs(t, bracketedPasteStart+pasted+bracketedPasteEnd+"\x1b[A\r", 2)

	if !inputs[0].pasted || inputs[0].text != pasted {
		t.Fatalf("first input = %+v, want pasted %q", inputs[0], pasted)
	}
	if inputs[1].text != "" {
		t.Fatalf("history after multiline paste = %q, want empty", inputs[1].text)
	}
}

func TestPromptLineEditorBracketedPasteInsertsAtCursor(t *testing.T) {
	input, ok, err := readEditedInput(t, "ab\x1b[D"+bracketedPasteStart+"X"+bracketedPasteEnd+"\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.pasted {
		t.Fatal("paste into non-empty prompt should not force literal-paste submission")
	}
	if input.text != "aXb" {
		t.Fatalf("input text = %q, want aXb", input.text)
	}
}

func TestPromptLineEditorRedrawClearsWrappedRows(t *testing.T) {
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("abcde\r"), &out)
	editor.columns = func() int { return 6 }

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "abcde" {
		t.Fatalf("input text = %q, want abcde", input.text)
	}

	got := out.String()
	if strings.Contains(got, "\x1b8") {
		t.Fatalf("redraw should not rely on absolute saved-cursor restore: %q", got)
	}
	screen := newPromptTestScreen(4, 6, 0)
	screen.feed(got)
	lines := screen.visibleLines()
	if lines[0] != "> abcd" || lines[1] != "e" {
		t.Fatalf("final screen = %#v, want wrapped prompt without stale rows", lines)
	}
}

func TestPromptLineEditorRedrawClearsWrappedPromptRows(t *testing.T) {
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("x\r"), &out)
	editor.columns = func() int { return 3 }

	input, ok, err := editor.read("abcd")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "x" {
		t.Fatalf("input text = %q, want x", input.text)
	}

	screen := newPromptTestScreen(4, 3, 0)
	screen.feed(out.String())
	lines := screen.visibleLines()
	if lines[0] != "abc" || lines[1] != "dx" {
		t.Fatalf("final screen = %#v, want wrapped prompt rows without stale text", lines)
	}
}

func TestPromptLineEditorRedrawSurvivesBottomScroll(t *testing.T) {
	var out bytes.Buffer
	screen := newPromptTestScreen(4, 8, 3)
	state := lineEditState{prompt: "> "}

	for _, r := range "abcdefghi" {
		state.insert(r)
		before := out.Len()
		if err := state.redraw(&out, 8); err != nil {
			t.Fatalf("redraw: %v", err)
		}
		screen.feed(out.String()[before:])
	}

	lines := screen.visibleLines()
	if lines[2] != "> abcdef" || lines[3] != "ghi" {
		t.Fatalf("screen after bottom scroll = %#v, want one wrapped prompt", lines)
	}
	if got := strings.Count(strings.Join(lines, "\n"), "> "); got != 1 {
		t.Fatalf("screen contains %d prompts, want 1: %#v", got, lines)
	}
}

func TestPromptLineEditorRedrawClearsShrunkWrappedInput(t *testing.T) {
	var out bytes.Buffer
	screen := newPromptTestScreen(4, 6, 0)
	state := lineEditState{prompt: "> "}

	for _, r := range "abcde" {
		state.insert(r)
	}
	if err := state.redraw(&out, 6); err != nil {
		t.Fatalf("initial redraw: %v", err)
	}
	screen.feed(out.String())

	state.backspace()
	state.backspace()
	before := out.Len()
	if err := state.redraw(&out, 6); err != nil {
		t.Fatalf("shrunk redraw: %v", err)
	}
	screen.feed(out.String()[before:])

	lines := screen.visibleLines()
	if lines[0] != "> abc" || lines[1] != "" {
		t.Fatalf("screen after shrink = %#v, want stale wrapped row cleared", lines)
	}
}

func TestPromptLineEditorFinishMovesFromMiddleToEnd(t *testing.T) {
	var out bytes.Buffer
	screen := newPromptTestScreen(4, 6, 0)
	state := lineEditState{prompt: "> "}

	for _, r := range "abcde" {
		state.insert(r)
	}
	state.left()
	state.left()
	if err := state.redraw(&out, 6); err != nil {
		t.Fatalf("redraw: %v", err)
	}
	screen.feed(out.String())

	before := out.Len()
	if err := state.finish(&out); err != nil {
		t.Fatalf("finish: %v", err)
	}
	screen.feed(out.String()[before:])

	lines := screen.visibleLines()
	if lines[0] != "> abcd" || lines[1] != "e" || screen.row != 2 || screen.col != 0 {
		t.Fatalf("screen after finish = %#v cursor=(%d,%d), want newline after rendered end", lines, screen.row, screen.col)
	}
}

func TestPromptLineEditorPreloadedHistory(t *testing.T) {
	// Simulate a fresh REPL with preloaded history from a previous session.
	// Arrow-up should recall the most recent preloaded entry.
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("\x1b[A\r"), &out)
	editor.SetInitialHistory([]string{"old-command-1", "old-command-2"})

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	// Arrow-up should recall "old-command-2" (the most recent)
	if input.text != "old-command-2" {
		t.Fatalf("preloaded history recall = %q, want %q", input.text, "old-command-2")
	}
}

func TestPromptLineEditorOnNewHistoryCallback(t *testing.T) {
	// Verify that the onNewHistory callback is fired when a new entry is added.
	var out bytes.Buffer
	editor := newPromptLineEditor(strings.NewReader("new-command\r"), &out)

	var captured string
	editor.onNewHistory = func(entry string) {
		captured = entry
	}

	input, ok, err := editor.read("> ")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "new-command" {
		t.Fatalf("input text = %q, want %q", input.text, "new-command")
	}
	if captured != "new-command" {
		t.Fatalf("onNewHistory callback captured = %q, want %q", captured, "new-command")
	}
}

func TestPromptLineEditorViBareEscapeEntersNormalMode(t *testing.T) {
	input, ok, err := readViEditedInput(t, "abc\x1bhx\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ac" {
		t.Fatalf("input text = %q, want ac", input.text)
	}
}

func TestPromptLineEditorViEscapeBeforePrintableDoesNotSwallowCommand(t *testing.T) {
	input, ok, err := readViEditedInput(t, "abc\x1b0iX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "Xabc" {
		t.Fatalf("input text = %q, want Xabc", input.text)
	}
}

func TestPromptLineEditorViEscapeSequenceStillMovesInInsertMode(t *testing.T) {
	input, ok, err := readViEditedInput(t, "abc\x1b[DZ\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "abZc" {
		t.Fatalf("input text = %q, want abZc", input.text)
	}
}

func TestPromptLineEditorViCSIuEscapeEntersNormalMode(t *testing.T) {
	input, ok, err := readViEditedInput(t, "abc\x1b[27uhx\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "ac" {
		t.Fatalf("input text = %q, want ac", input.text)
	}
}

func TestPromptLineEditorViWordMotions(t *testing.T) {
	input, ok, err := readViEditedInput(t, "one two\x1b$biX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "one Xtwo" {
		t.Fatalf("input text = %q, want one Xtwo", input.text)
	}

	input, ok, err = readViEditedInput(t, "one two\x1b0weaX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "one twoX" {
		t.Fatalf("input text = %q, want one twoX", input.text)
	}
}

func TestPromptLineEditorViDeleteAndChangeOperators(t *testing.T) {
	input, ok, err := readViEditedInput(t, "one two three\x1b0dw\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "two three" {
		t.Fatalf("input text = %q, want two three", input.text)
	}

	input, ok, err = readViEditedInput(t, "one two\x1b0wcwX\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "one X" {
		t.Fatalf("input text = %q, want one X", input.text)
	}
}

func TestPromptLineEditorViYankAndPasteOperator(t *testing.T) {
	input, ok, err := readViEditedInput(t, "one two\x1b0yw$p\r")
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if !ok {
		t.Fatal("read returned ok=false")
	}
	if input.text != "one twoone " {
		t.Fatalf("input text = %q, want one twoone ", input.text)
	}
}

type promptTestScreen struct {
	rows, cols int
	row, col   int
	savedRow   int
	savedCol   int
	cells      [][]rune
}

func newPromptTestScreen(rows, cols, startRow int) *promptTestScreen {
	s := &promptTestScreen{
		rows: rows,
		cols: cols,
		row:  startRow,
	}
	if s.row < 0 {
		s.row = 0
	}
	if s.row >= rows {
		s.row = rows - 1
	}
	for range rows {
		s.cells = append(s.cells, blankPromptTestLine(cols))
	}
	return s
}

func blankPromptTestLine(cols int) []rune {
	line := make([]rune, cols)
	for i := range line {
		line[i] = ' '
	}
	return line
}

func (s *promptTestScreen) feed(text string) {
	for i := 0; i < len(text); {
		switch text[i] {
		case '\x1b':
			i += s.feedEscape(text[i:])
		case '\r':
			s.col = 0
			i++
		case '\n':
			s.newline()
			i++
		default:
			r, size := utf8.DecodeRuneInString(text[i:])
			if r == utf8.RuneError && size == 0 {
				return
			}
			if r >= ' ' {
				s.put(r)
			}
			i += size
		}
	}
}

func (s *promptTestScreen) feedEscape(text string) int {
	if len(text) < 2 {
		return 1
	}
	switch text[1] {
	case '7':
		s.savedRow, s.savedCol = s.row, s.col
		return 2
	case '8':
		s.row, s.col = s.savedRow, s.savedCol
		return 2
	case '[':
		end := 2
		for end < len(text) && (text[end] < '@' || text[end] > '~') {
			end++
		}
		if end >= len(text) {
			return len(text)
		}
		s.applyCSI(text[2:end], text[end])
		return end + 1
	default:
		return 2
	}
}

func (s *promptTestScreen) applyCSI(params string, final byte) {
	n := firstCSIParam(params, 1)
	switch final {
	case 'A':
		s.row -= n
		if s.row < 0 {
			s.row = 0
		}
	case 'B':
		s.row += n
		if s.row >= s.rows {
			s.row = s.rows - 1
		}
	case 'C':
		s.col += n
		if s.col >= s.cols {
			s.col = s.cols - 1
		}
	case 'D':
		s.col -= n
		if s.col < 0 {
			s.col = 0
		}
	case 'K':
		if n == 2 {
			s.cells[s.row] = blankPromptTestLine(s.cols)
		}
	}
}

func firstCSIParam(params string, fallback int) int {
	if params == "" {
		return fallback
	}
	first, _, _ := strings.Cut(params, ";")
	if first == "" {
		return fallback
	}
	n, err := strconv.Atoi(first)
	if err != nil {
		return fallback
	}
	return n
}

func (s *promptTestScreen) put(r rune) {
	s.cells[s.row][s.col] = r
	s.col++
	if s.col >= s.cols {
		s.col = 0
		s.row++
		s.scrollIfNeeded()
	}
}

func (s *promptTestScreen) newline() {
	s.col = 0
	s.row++
	s.scrollIfNeeded()
}

func (s *promptTestScreen) scrollIfNeeded() {
	for s.row >= s.rows {
		copy(s.cells, s.cells[1:])
		s.cells[s.rows-1] = blankPromptTestLine(s.cols)
		s.row--
	}
}

func (s *promptTestScreen) visibleLines() []string {
	lines := make([]string, 0, s.rows)
	for _, line := range s.cells {
		lines = append(lines, strings.TrimRight(string(line), " "))
	}
	return lines
}
