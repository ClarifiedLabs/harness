package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"harness/internal/term"
)

// replInputTraceEnv is the env-var checked by both this package and
// internal/term to append timestamped terminal-input events to a log file
// (or stderr when set to "-"). The two packages write to the same destination
// but cannot share the constant without an import cycle; keep the value in
// sync with term_unix.go:replInputTraceEnv.
const replInputTraceEnv = "HARNESS_REPL_INPUT_TRACE"

const (
	ctrlA = 0x01
	ctrlB = 0x02
	ctrlC = 0x03
	ctrlD = 0x04
	ctrlE = 0x05
	ctrlF = 0x06
	del   = 0x7f
)

const bareEscapeSequenceTimeout = 50 * time.Millisecond

// Paste-burst detection thresholds (non-bracketed paste fallback). A keystroke
// arriving within pasteEnterGap of the previous one enters "paste mode"; paste
// mode exits after a gap longer than pasteExitGap. Staying in paste mode too long
// is the safe failure direction (an extra inserted newline, never a premature
// submit). A paste filling an empty prompt that exceeds pasteSummaryBytes or has
// at least pasteSummaryLines renders a one-line placeholder instead of the full
// content inline (avoids scroll lag on large pastes).
const (
	pasteEnterGap           = 5 * time.Millisecond
	pasteExitGap            = 150 * time.Millisecond
	pasteSummaryBytes       = 1000
	pasteSummaryLines       = 50
	pasteSummaryPlaceholder = "[%d bytes of pasted content]"
)

// replPasteHeuristicEnv disables the non-bracketed paste-burst heuristic when
// set to "off" (default on, interactive TTY only). It mirrors the
// HARNESS_REPL_INPUT_TRACE pattern.
const replPasteHeuristicEnv = "HARNESS_REPL_PASTE_HEURISTIC"

const (
	lineKeyDelete    = 3
	lineKeyEscape    = 27
	lineKeyBackspace = 127
	lineKeyEnter     = 13
	lineKeyTab       = 9

	lineKeyLeftShift  = 57441
	lineKeyRightShift = 57447

	lineModShift = 2
)

type promptEditMode string

const (
	promptEditModeEmacs promptEditMode = "emacs"
	promptEditModeVi    promptEditMode = "vi"
)

type promptLineEditor struct {
	r                   *bufio.Reader
	w                   io.Writer
	columns             func() int
	editMode            promptEditMode
	escapeSequenceReady func(time.Duration) bool
	escapeSequenceWait  time.Duration
	history             []string
	viYank              []rune
	onNewHistory        func(string) // optional callback fired when a new entry is added
	shiftEnterPending   bool

	// Non-bracketed paste-burst heuristic (interactive TTY only). When now is
	// non-nil and pasteHeuristic is true, updatePasteTiming tracks inter-keystroke
	// gaps and enters pasteMode when bytes arrive faster than a human can type
	// (<=pasteEnterGap), so newlines in a paste insert instead of submitting and
	// the whole paste fills the buffer for review. Tests drive now with a fake
	// clock; production wires time.Now on the TTY fd. pasteHeuristic defaults off
	// (non-interactive readers and tests) and is switched on in newREPLReader.
	now             func() time.Time
	pasteHeuristic  bool
	pasteMode       bool
	lastKeyTime     time.Time
	prevBufferEmpty bool // whether the buffer was empty before the current keystroke
	purePaste       bool // an unedited paste fills the buffer; submitted literally

	// viPrompt, when non-nil, returns the fully rendered prompt for a given vi
	// mode. It is invoked at the two mode-transition chokepoints (enterNormal /
	// enterInsert) and once at read start so a {vimode} placeholder in the prompt
	// flips live as the user switches mode. nil in emacs mode, templates without
	// a vimode variant, and tests — behavior is then identical to today.
	viPrompt func(viMode) string
}

func newPromptLineEditor(in io.Reader, w io.Writer) *promptLineEditor {
	return newPromptLineEditorWithReader(bufio.NewReader(in), w)
}

func newPromptLineEditorWithReader(r *bufio.Reader, w io.Writer) *promptLineEditor {
	return &promptLineEditor{
		r:                  r,
		w:                  w,
		columns:            promptEditorColumns,
		editMode:           promptEditModeEmacs,
		escapeSequenceWait: bareEscapeSequenceTimeout,
	}
}

// configurePasteHeuristic enables the non-bracketed paste-burst detection on
// the interactive TTY path and supplies the clock (time.Now in production; a
// fake clock in tests). It is a no-op when the heuristic is disabled.
func (e *promptLineEditor) configurePasteHeuristic(enabled bool, now func() time.Time) {
	e.pasteHeuristic = enabled && now != nil
	e.now = now
}

// pasteHeuristicEnabled reports whether the non-bracketed paste-burst heuristic
// is active. It defaults on and is disabled by HARNESS_REPL_PASTE_HEURISTIC=off.
func pasteHeuristicEnabled() bool {
	return !strings.EqualFold(os.Getenv(replPasteHeuristicEnv), "off")
}

// updatePasteTiming runs after each keystroke is read. It tracks inter-keystroke
// gaps and enters paste mode when bytes arrive faster than a human can type
// (<=pasteEnterGap), so newlines in a paste insert instead of submitting. Paste
// mode exits after a gap longer than pasteExitGap. The heuristic is a fallback
// for terminals that do not honor bracketed paste; it is a no-op unless
// configurePasteHeuristic enabled it.
//
// purePaste is set when a burst is recognized that started from an empty buffer
// (prevBufferEmpty recorded before the burst's first byte): such a paste fills
// the prompt and is submitted literally. Staying in paste mode too long is the
// safe failure direction (an extra inserted newline, never a premature submit).
func (e *promptLineEditor) updatePasteTiming(s *lineEditState) {
	if !e.pasteHeuristic || e.now == nil {
		return
	}
	now := e.now()
	emptyBefore := len(s.buf) == 0
	if !e.lastKeyTime.IsZero() {
		gap := now.Sub(e.lastKeyTime)
		if gap <= pasteEnterGap {
			if !e.pasteMode {
				e.pasteMode = true
				e.purePaste = e.prevBufferEmpty
			}
		} else if e.pasteMode && gap > pasteExitGap {
			e.pasteMode = false
		}
	}
	e.lastKeyTime = now
	e.prevBufferEmpty = emptyBefore
}

// markManualEdit clears the pure-paste literal guarantee and any paste summary:
// any manual keystroke (insert, delete, or cursor motion) after a paste makes
// the whole submitted line typed (honoring !/command/$skill). It is a no-op
// while a paste burst is in progress, so paste bytes never clear the flag.
func (e *promptLineEditor) markManualEdit(s *lineEditState) {
	if e.pasteMode {
		return
	}
	e.purePaste = false
	s.summary = ""
}

// refreshPasteSummary collapses the display to the one-line paste placeholder
// once the accumulated buffer crosses the size/line threshold, so a large
// non-bracketed paste stops rendering inline (avoiding scroll lag). Below the
// threshold the real content renders inline. Called only while in paste mode.
func (e *promptLineEditor) refreshPasteSummary(s *lineEditState) {
	text := string(s.buf)
	if len(text) > pasteSummaryBytes || strings.Count(text, "\n") >= pasteSummaryLines {
		s.summary = fmt.Sprintf(pasteSummaryPlaceholder, len(text))
	} else {
		s.summary = ""
	}
}

func (e *promptLineEditor) setEditMode(mode string) {
	switch mode {
	case string(promptEditModeVi):
		e.editMode = promptEditModeVi
	default:
		e.editMode = promptEditModeEmacs
	}
}

func (e *promptLineEditor) read(prompt string) (replInput, bool, error) {
	return e.readPrefilled(prompt, "")
}

// readPrefilled reads a line with the editor seeded with editable prefill text
// (cursor at end). It backs both the bare prompt and the during-turn deposit,
// where the text typed while the model ran is handed back for review/edit before
// the user submits it manually (during-turn input).
func (e *promptLineEditor) readPrefilled(prompt, prefill string) (replInput, bool, error) {
	state := lineEditState{prompt: prompt}
	if prefill != "" {
		state.setText(prefill)
	}
	// Each prompt starts fresh: paste mode and the pure-paste literal flag must not
	// carry over from a previous prompt (a typed prompt after a pasted one is
	// authored, not literal). lastKeyTime is reset so the first keystroke of this
	// prompt cannot be mistaken for a paste continuation.
	e.pasteMode = false
	e.purePaste = false
	e.lastKeyTime = time.Time{}
	e.prevBufferEmpty = len(state.buf) == 0
	history := e.historyState()
	vi := viLineState{mode: viModeInsert}
	e.refreshViPrompt(&vi, &state)
	e.tracef("read start prompt=%q", prompt)
	if err := state.redraw(e.w, e.terminalColumns()); err != nil {
		return replInput{}, false, err
	}

	for {
		r, size, err := e.r.ReadRune()
		if err != nil {
			if errors.Is(err, io.EOF) && len(state.buf) > 0 {
				e.tracef("read eof with buffered text len=%d purePaste=%v", len(state.buf), e.purePaste)
				if err := state.finish(e.w); err != nil {
					return replInput{}, false, err
				}
				return replInput{text: string(state.buf), pasted: e.purePaste}, true, nil
			}
			if errors.Is(err, io.EOF) {
				e.tracef("read eof empty")
				return replInput{}, false, nil
			}
			e.tracef("read error: %v", err)
			return replInput{}, false, err
		}
		e.tracef("read rune=%s size=%d buffered=%d", traceRune(r), size, e.r.Buffered())
		e.updatePasteTiming(&state)

		if e.editMode == promptEditModeVi && vi.mode == viModeNormal {
			result, err := e.handleViNormalInput(&vi, &state, &history, prompt, r)
			if err != nil {
				return replInput{}, false, err
			}
			if result.done {
				return result.input, result.ok, nil
			}
			if result.redraw {
				if err := state.redraw(e.w, e.terminalColumns()); err != nil {
					return replInput{}, false, err
				}
			}
			continue
		}

		switch r {
		case '\r':
			if e.consumeShiftEnterPending() {
				e.tracef("raw CR after shift modifier inserts newline")
				e.markManualEdit(&state)
				state.insert('\n')
				if err := state.redraw(e.w, e.terminalColumns()); err != nil {
					return replInput{}, false, err
				}
				continue
			}
			if e.pasteMode {
				e.tracef("raw CR in paste inserts newline")
				state.insert('\n')
				e.refreshPasteSummary(&state)
				if err := state.redraw(e.w, e.terminalColumns()); err != nil {
					return replInput{}, false, err
				}
				continue
			}
			e.tracef("raw CR submits text len=%d purePaste=%v", len(state.buf), e.purePaste)
			if err := state.finish(e.w); err != nil {
				return replInput{}, false, err
			}
			e.addHistory(string(state.buf))
			return replInput{text: string(state.buf), pasted: e.purePaste}, true, nil
		case '\n':
			e.consumeShiftEnterPending()
			e.markManualEdit(&state)
			e.tracef("raw LF inserts newline")
			state.insert('\n')
			if e.pasteMode {
				e.refreshPasteSummary(&state)
			}
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case ctrlA:
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			e.tracef("raw ctrl-a moves to start")
			state.home()
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case ctrlB:
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			e.tracef("raw ctrl-b moves left")
			state.left()
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case rune(lineTermEdit):
			e.clearShiftEnterPending()
			e.tracef("raw ctrl-g opens editor text len=%d", len(state.buf))
			if err := state.finish(e.w); err != nil {
				return replInput{}, false, err
			}
			e.addHistory(string(state.buf))
			return replInput{text: string(state.buf), edit: true}, true, nil
		case ctrlE:
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			e.tracef("raw ctrl-e moves to end")
			state.end()
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case ctrlF:
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			e.tracef("raw ctrl-f moves right")
			state.right()
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case ctrlC:
			e.clearShiftEnterPending()
			e.tracef("raw ctrl-c interrupts")
			return replInput{interrupt: true}, true, nil
		case ctrlD:
			e.clearShiftEnterPending()
			e.tracef("raw ctrl-d text len=%d", len(state.buf))
			if len(state.buf) == 0 {
				return replInput{}, false, nil
			}
		case '\t':
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			handled, err := e.completeBangLine(&state)
			if err != nil {
				return replInput{}, false, err
			}
			if !handled {
				state.insert('\t')
			}
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case '\b', del:
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			e.tracef("raw backspace")
			state.backspace()
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		case rune(lineTermEscape):
			action, text, err := e.readEscape()
			if err != nil {
				if errors.Is(err, io.EOF) {
					e.tracef("escape read eof")
					return replInput{}, false, nil
				}
				e.tracef("escape read error: %v", err)
				return replInput{}, false, err
			}
			e.tracef("escape action=%s text=%q", lineEditActionName(action), text)
			if e.editMode == promptEditModeVi && action == lineEditEscape {
				e.clearShiftEnterPending()
				e.viEnterNormal(&vi, &state)
				if err := state.redraw(e.w, e.terminalColumns()); err != nil {
					return replInput{}, false, err
				}
				continue
			}
			if action == lineEditShiftModifier {
				e.markShiftEnterPending()
				if err := state.redraw(e.w, e.terminalColumns()); err != nil {
					return replInput{}, false, err
				}
				continue
			}
			e.clearShiftEnterPending()
			if action == lineEditSubmit {
				e.tracef("escape submit text len=%d purePaste=%v", len(state.buf), e.purePaste)
				if err := state.finish(e.w); err != nil {
					return replInput{}, false, err
				}
				e.addHistory(string(state.buf))
				return replInput{text: string(state.buf), pasted: e.purePaste}, true, nil
			}
			switch action {
			case lineEditHome:
				e.markManualEdit(&state)
				state.home()
			case lineEditEnd:
				e.markManualEdit(&state)
				state.end()
			case lineEditLeft:
				e.markManualEdit(&state)
				state.left()
			case lineEditRight:
				e.markManualEdit(&state)
				state.right()
			case lineEditBackspace:
				e.markManualEdit(&state)
				state.backspace()
			case lineEditDelete:
				e.markManualEdit(&state)
				state.delete()
			case lineEditEdit:
				if err := state.finish(e.w); err != nil {
					return replInput{}, false, err
				}
				e.addHistory(string(state.buf))
				return replInput{text: string(state.buf), edit: true}, true, nil
			case lineEditEOF:
				if len(state.buf) == 0 {
					return replInput{}, false, nil
				}
			case lineEditInterrupt:
				return replInput{interrupt: true}, true, nil
			case lineEditInsertNewline:
				e.markManualEdit(&state)
				state.insert('\n')
				if e.pasteMode {
					e.refreshPasteSummary(&state)
				}
				e.discardBufferedRawEnter()
			case lineEditInsertText:
				e.markManualEdit(&state)
				if text == "\t" {
					handled, err := e.completeBangLine(&state)
					if err != nil {
						return replInput{}, false, err
					}
					if handled {
						break
					}
				}
				state.insertString(text)
				if e.pasteMode {
					e.refreshPasteSummary(&state)
				}
			case lineEditHistoryPrev:
				e.markManualEdit(&state)
				history.prev(&state)
			case lineEditHistoryNext:
				e.markManualEdit(&state)
				history.next(&state)
			case lineEditPaste:
				if len(state.buf) == 0 {
					state.setPasteSummary(text)
					e.purePaste = true
					e.tracef("bracketed paste fills empty prompt len=%d summary=%q", len(text), state.summary)
					break
				}
				state.insertString(text)
			}
			if err := state.redraw(e.w, e.terminalColumns()); err != nil {
				return replInput{}, false, err
			}
		default:
			e.clearShiftEnterPending()
			e.markManualEdit(&state)
			if r == '\t' || unicode.IsPrint(r) {
				state.insert(r)
				if e.pasteMode {
					e.refreshPasteSummary(&state)
				}
				if err := state.redraw(e.w, e.terminalColumns()); err != nil {
					return replInput{}, false, err
				}
			}
		}
	}
}

func (e *promptLineEditor) terminalColumns() int {
	if e.columns == nil {
		return 0
	}
	return e.columns()
}

func promptEditorColumns() int {
	_, cols, ok := term.Size()
	if !ok {
		return 0
	}
	return cols
}

type lineEditAction int

const (
	lineEditIgnore lineEditAction = iota
	lineEditHome
	lineEditEnd
	lineEditLeft
	lineEditRight
	lineEditBackspace
	lineEditDelete
	lineEditPaste
	lineEditHistoryPrev
	lineEditHistoryNext
	lineEditInsertNewline
	lineEditInsertText
	lineEditSubmit
	lineEditEdit
	lineEditEOF
	lineEditInterrupt
	lineEditEscape
	lineEditShiftModifier
)

func (e *promptLineEditor) readEscape() (lineEditAction, string, error) {
	if !e.escapeSequenceAvailable() {
		e.tracef("escape interpreted as bare")
		return lineEditEscape, "", nil
	}
	b, err := e.r.Peek(1)
	if err != nil {
		if errors.Is(err, io.EOF) {
			e.tracef("escape peek eof; interpreted as bare")
			return lineEditEscape, "", nil
		}
		return lineEditIgnore, "", err
	}
	if len(b) == 0 {
		e.tracef("escape peek empty; interpreted as bare")
		return lineEditEscape, "", nil
	}
	if b[0] != '[' && b[0] != 'O' {
		e.tracef("escape next=%s is not a sequence starter; interpreted as bare", traceByte(b[0]))
		return lineEditEscape, "", nil
	}
	c, err := e.r.ReadByte()
	if err != nil {
		return lineEditIgnore, "", err
	}
	e.tracef("escape introducer next=%s buffered=%d", traceByte(c), e.r.Buffered())
	switch c {
	case '[':
		seq, err := e.readCSI()
		if err != nil {
			return lineEditIgnore, "", err
		}
		e.tracef("csi raw seq=%q", seq)
		switch seq {
		case "A":
			return lineEditHistoryPrev, "", nil
		case "B":
			return lineEditHistoryNext, "", nil
		case "C":
			return lineEditRight, "", nil
		case "D":
			return lineEditLeft, "", nil
		case "3~":
			return lineEditDelete, "", nil
		case "200~":
			text, err := e.readBracketedPaste()
			if err != nil {
				return lineEditIgnore, "", err
			}
			return lineEditPaste, text, nil
		default:
			action, text := e.actionForKeySequence(seq)
			return action, text, nil
		}
	case 'O':
		c, err := e.r.ReadByte()
		if err != nil {
			return lineEditIgnore, "", err
		}
		switch c {
		case 'A':
			return lineEditHistoryPrev, "", nil
		case 'B':
			return lineEditHistoryNext, "", nil
		case 'C':
			return lineEditRight, "", nil
		case 'D':
			return lineEditLeft, "", nil
		case 'H':
			return lineEditHome, "", nil
		case 'F':
			return lineEditEnd, "", nil
		default:
			return lineEditIgnore, "", nil
		}
	default:
		return lineEditIgnore, "", nil
	}
}

func (e *promptLineEditor) escapeSequenceAvailable() bool {
	if e.r.Buffered() > 0 {
		return true
	}
	if e.escapeSequenceReady == nil {
		return false
	}
	wait := e.escapeSequenceWait
	if wait <= 0 {
		wait = bareEscapeSequenceTimeout
	}
	return e.escapeSequenceReady(wait)
}

func (e *promptLineEditor) readCSI() (string, error) {
	var b strings.Builder
	for {
		c, err := e.r.ReadByte()
		if err != nil {
			return b.String(), err
		}
		b.WriteByte(c)
		if c >= '@' && c <= '~' {
			return b.String(), nil
		}
	}
}

func (e *promptLineEditor) actionForKeySequence(seq string) (lineEditAction, string) {
	if seq == "Z" {
		action, text := actionForKeyEvent(lineKeyEvent{key: lineKeyTab, modifier: lineModShift})
		e.tracef("csi seq=%q key=%d modifier=%d event=%d final=%q text=%q action=%s", seq, lineKeyTab, lineModShift, 1, byte(0), text, lineEditActionName(action))
		return action, text
	}
	ev, ok := parseKeySequence(seq)
	if !ok {
		e.tracef("csi seq=%q parse=failed", seq)
		return lineEditIgnore, ""
	}
	action, text := actionForKeyEvent(ev)
	e.tracef("csi seq=%q key=%d modifier=%d event=%d final=%q text=%q action=%s", seq, ev.key, ev.modifier, ev.eventType, ev.final, text, lineEditActionName(action))
	return action, text
}

type lineKeyEvent struct {
	key       int
	modifier  int
	eventType int
	final     byte
	text      string
}

func actionForKeyEvent(ev lineKeyEvent) (lineEditAction, string) {
	if ev.eventType == 3 {
		return lineEditIgnore, ""
	}
	if ev.text != "" {
		return lineEditInsertText, ev.text
	}
	switch {
	case ev.key == lineKeyEnter && ev.modifier == lineModShift:
		return lineEditInsertNewline, ""
	case ev.key == lineKeyEnter:
		return lineEditSubmit, ""
	case ev.key == lineKeyEscape:
		return lineEditEscape, ""
	case ev.key == lineKeyDelete && ev.final == '~':
		return lineEditDelete, ""
	case ev.key == lineKeyBackspace && ev.modifier == 1:
		return lineEditBackspace, ""
	case ev.key == lineKeyBackspace:
		return lineEditDelete, ""
	case ev.key == lineKeyTab && ev.modifier == lineModShift:
		return lineEditIgnore, ""
	case ev.key == lineKeyTab:
		return lineEditInsertText, "\t"
	case (ev.key == lineKeyLeftShift || ev.key == lineKeyRightShift) && keyModifierHasShift(ev.modifier):
		return lineEditShiftModifier, ""
	case ev.key == 'a' && keyModifierHasCtrl(ev.modifier):
		return lineEditHome, ""
	case ev.key == 'b' && keyModifierHasCtrl(ev.modifier):
		return lineEditLeft, ""
	case ev.key == 'c' && keyModifierHasCtrl(ev.modifier):
		return lineEditInterrupt, ""
	case ev.key == 'd' && keyModifierHasCtrl(ev.modifier):
		return lineEditEOF, ""
	case ev.key == 'e' && keyModifierHasCtrl(ev.modifier):
		return lineEditEnd, ""
	case ev.key == 'f' && keyModifierHasCtrl(ev.modifier):
		return lineEditRight, ""
	case ev.key == 'g' && keyModifierHasCtrl(ev.modifier):
		return lineEditEdit, ""
	case ev.key == 1 && ev.final == 'A':
		return lineEditHistoryPrev, ""
	case ev.key == 1 && ev.final == 'B':
		return lineEditHistoryNext, ""
	case ev.key == 1 && ev.final == 'C':
		return lineEditRight, ""
	case ev.key == 1 && ev.final == 'D':
		return lineEditLeft, ""
	case ev.final == 'H':
		return lineEditHome, ""
	case ev.final == 'F':
		return lineEditEnd, ""
	case ev.final == '~' && (ev.key == 1 || ev.key == 7):
		return lineEditHome, ""
	case ev.final == '~' && (ev.key == 4 || ev.key == 8):
		return lineEditEnd, ""
	default:
		return lineEditIgnore, ""
	}
}

func keyModifierHasCtrl(modifier int) bool {
	if modifier <= 1 {
		return false
	}
	return (modifier-1)&4 != 0
}

func keyModifierHasShift(modifier int) bool {
	if modifier <= 1 {
		return false
	}
	return (modifier-1)&1 != 0
}

func parseKeySequence(seq string) (lineKeyEvent, bool) {
	if strings.HasSuffix(seq, "u") {
		return parseCSIuKeySequence(strings.TrimSuffix(seq, "u"))
	}
	if strings.HasSuffix(seq, "~") {
		return parseTildeKeySequence(strings.TrimSuffix(seq, "~"))
	}
	return parseFinalKeySequence(seq)
}

func parseCSIuKeySequence(params string) (lineKeyEvent, bool) {
	fields := strings.Split(params, ";")
	if len(fields) == 0 || fields[0] == "" {
		return lineKeyEvent{}, false
	}
	key, ok := parseKeyIntField(fields[0])
	if !ok {
		return lineKeyEvent{}, false
	}
	modifier, eventType := 1, 1
	if len(fields) >= 2 && fields[1] != "" {
		var ok bool
		modifier, eventType, ok = parseModifierField(fields[1])
		if !ok {
			return lineKeyEvent{}, false
		}
	}
	text := ""
	if len(fields) >= 3 && fields[2] != "" {
		var ok bool
		text, ok = parseAssociatedText(fields[2])
		if !ok {
			return lineKeyEvent{}, false
		}
	}
	return lineKeyEvent{key: key, modifier: modifier, eventType: eventType, final: 'u', text: text}, true
}

func parseTildeKeySequence(params string) (lineKeyEvent, bool) {
	if params == "" {
		return lineKeyEvent{}, false
	}
	fields := strings.Split(params, ";")
	if len(fields) == 3 && fields[0] == "27" {
		modifier, ok := parseIntField(fields[1])
		if !ok {
			return lineKeyEvent{}, false
		}
		key, ok := parseIntField(fields[2])
		if !ok {
			return lineKeyEvent{}, false
		}
		return lineKeyEvent{key: key, modifier: modifier, eventType: 1, final: '~'}, true
	}
	key, ok := parseIntField(fields[0])
	if !ok {
		return lineKeyEvent{}, false
	}
	modifier := 1
	if len(fields) >= 2 && fields[1] != "" {
		modifier, ok = parseIntField(fields[1])
		if !ok {
			return lineKeyEvent{}, false
		}
	}
	return lineKeyEvent{key: key, modifier: modifier, eventType: 1, final: '~'}, true
}

func parseFinalKeySequence(seq string) (lineKeyEvent, bool) {
	if seq == "" {
		return lineKeyEvent{}, false
	}
	final := seq[len(seq)-1]
	if !strings.ContainsRune("ABCDHF", rune(final)) {
		return lineKeyEvent{}, false
	}
	params := seq[:len(seq)-1]
	key, modifier := 1, 1
	if params != "" {
		fields := strings.Split(params, ";")
		if fields[0] != "" {
			var ok bool
			key, ok = parseIntField(fields[0])
			if !ok {
				return lineKeyEvent{}, false
			}
		}
		if len(fields) >= 2 && fields[1] != "" {
			var ok bool
			modifier, ok = parseIntField(fields[1])
			if !ok {
				return lineKeyEvent{}, false
			}
		}
	}
	return lineKeyEvent{key: key, modifier: modifier, eventType: 1, final: final}, true
}

func parseKeyIntField(field string) (int, bool) {
	if before, _, found := strings.Cut(field, ":"); found {
		field = before
	}
	return parseIntField(field)
}

func parseModifierField(field string) (modifier, eventType int, ok bool) {
	modifier, eventType = 1, 1
	parts := strings.Split(field, ":")
	if len(parts) > 0 && parts[0] != "" {
		var parsed bool
		modifier, parsed = parseIntField(parts[0])
		if !parsed {
			return 0, 0, false
		}
	}
	if len(parts) >= 2 && parts[1] != "" {
		var parsed bool
		eventType, parsed = parseIntField(parts[1])
		if !parsed {
			return 0, 0, false
		}
	}
	return modifier, eventType, true
}

func parseAssociatedText(field string) (string, bool) {
	var b strings.Builder
	for _, part := range strings.Split(field, ":") {
		codepoint, ok := parseIntField(part)
		if !ok || codepoint < 0 {
			return "", false
		}
		b.WriteRune(rune(codepoint))
	}
	return b.String(), true
}

func parseIntField(field string) (int, bool) {
	if field == "" {
		return 0, false
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (e *promptLineEditor) markShiftEnterPending() {
	e.shiftEnterPending = true
	e.tracef("shift modifier pending")
}

func (e *promptLineEditor) consumeShiftEnterPending() bool {
	if !e.shiftEnterPending {
		return false
	}
	e.shiftEnterPending = false
	return true
}

func (e *promptLineEditor) clearShiftEnterPending() {
	e.shiftEnterPending = false
}

func (e *promptLineEditor) discardBufferedRawEnter() {
	if e.r.Buffered() == 0 {
		return
	}
	b, err := e.r.Peek(1)
	if err != nil || len(b) == 0 {
		return
	}
	if b[0] != '\r' && b[0] != '\n' {
		return
	}
	_, _ = e.r.ReadByte()
	e.tracef("discarded buffered raw enter after shift-enter byte=%#x", b[0])
}

func (e *promptLineEditor) tracef(format string, args ...any) {
	traceREPLInputf("ui: "+format, args...)
}

type bangCompletionCandidate struct {
	value   string
	display string
	isDir   bool
}

func (e *promptLineEditor) completeBangLine(s *lineEditState) (bool, error) {
	start, token, first, ok := bangCompletionContext(s.buf, s.cursor)
	if !ok {
		return false, nil
	}
	var candidates []bangCompletionCandidate
	if first && !bangCompletionUsesPath(token) {
		candidates = bangCommandCompletions(token)
	} else {
		candidates = bangPathCompletions(token)
	}
	if len(candidates) == 0 {
		return true, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].display < candidates[j].display
	})
	if len(candidates) == 1 {
		value := candidates[0].value
		if candidates[0].isDir {
			if !strings.HasSuffix(value, "/") {
				value += "/"
			}
		} else {
			value += " "
		}
		s.replaceRange(start, s.cursor, value)
		return true, nil
	}
	common := longestCommonCompletionPrefix(candidates)
	if len([]rune(common)) > len([]rune(token)) {
		s.replaceRange(start, s.cursor, common)
		return true, nil
	}
	if err := s.finish(e.w); err != nil {
		return true, err
	}
	for _, candidate := range candidates {
		display := candidate.display
		if candidate.isDir && !strings.HasSuffix(display, "/") {
			display += "/"
		}
		if _, err := fmt.Fprintln(e.w, display); err != nil {
			return true, err
		}
	}
	return true, nil
}

func bangCompletionContext(buf []rune, cursor int) (start int, token string, first bool, ok bool) {
	if len(buf) == 0 || buf[0] != '!' {
		return 0, "", false, false
	}
	if cursor < 1 {
		return 0, "", false, false
	}
	if cursor > len(buf) {
		cursor = len(buf)
	}
	start = 1
	for i := cursor - 1; i >= 1; i-- {
		if unicode.IsSpace(buf[i]) {
			start = i + 1
			break
		}
	}
	first = true
	for i := 1; i < start; i++ {
		if !unicode.IsSpace(buf[i]) {
			first = false
			break
		}
	}
	return start, string(buf[start:cursor]), first, true
}

func bangCompletionUsesPath(token string) bool {
	return strings.HasPrefix(token, "/") ||
		strings.HasPrefix(token, "~/") ||
		strings.HasPrefix(token, "./") ||
		strings.HasPrefix(token, "../") ||
		strings.Contains(token, "/")
}

func bangCommandCompletions(prefix string) []bangCompletionCandidate {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []bangCompletionCandidate
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if seen[name] || !strings.HasPrefix(name, prefix) {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				continue
			}
			seen[name] = true
			out = append(out, bangCompletionCandidate{value: name, display: name})
		}
	}
	return out
}

func bangPathCompletions(token string) []bangCompletionCandidate {
	dirPart, base := splitCompletionPath(token)
	searchDir, ok := expandCompletionDir(dirPart)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	var out []bangCompletionCandidate
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		value := dirPart + name
		display := value
		out = append(out, bangCompletionCandidate{value: value, display: display, isDir: entry.IsDir()})
	}
	return out
}

func splitCompletionPath(token string) (dirPart, base string) {
	idx := strings.LastIndex(token, "/")
	if idx < 0 {
		return "", token
	}
	return token[:idx+1], token[idx+1:]
}

func expandCompletionDir(dirPart string) (string, bool) {
	if dirPart == "" {
		return ".", true
	}
	if strings.HasPrefix(dirPart, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		rest := strings.TrimPrefix(dirPart, "~/")
		if rest == "" {
			return home, true
		}
		return filepath.Join(home, rest), true
	}
	return dirPart, true
}

func longestCommonCompletionPrefix(candidates []bangCompletionCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	common := []rune(candidates[0].value)
	for _, candidate := range candidates[1:] {
		next := []rune(candidate.value)
		n := 0
		for n < len(common) && n < len(next) && common[n] == next[n] {
			n++
		}
		common = common[:n]
		if len(common) == 0 {
			return ""
		}
	}
	return string(common)
}

func traceREPLInputf(format string, args ...any) {
	path := os.Getenv(replInputTraceEnv)
	if path == "" {
		return
	}
	var w io.Writer
	var close func()
	if path == "-" {
		w = os.Stderr
	} else {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		w = f
		close = func() { _ = f.Close() }
	}
	if close != nil {
		defer close()
	}
	fmt.Fprintf(w, "%s ", time.Now().Format(time.RFC3339Nano))
	fmt.Fprintf(w, format, args...)
	fmt.Fprintln(w)
}

func traceRune(r rune) string {
	switch r {
	case '\r':
		return "CR(0x0d)"
	case '\n':
		return "LF(0x0a)"
	case '\x1b':
		return "ESC(0x1b)"
	case '\t':
		return "TAB(0x09)"
	case ctrlA:
		return "CTRL-A(0x01)"
	case ctrlB:
		return "CTRL-B(0x02)"
	case ctrlC:
		return "CTRL-C(0x03)"
	case ctrlD:
		return "CTRL-D(0x04)"
	case ctrlE:
		return "CTRL-E(0x05)"
	case ctrlF:
		return "CTRL-F(0x06)"
	case rune(lineTermEdit):
		return "CTRL-G(0x07)"
	case del:
		return "DEL(0x7f)"
	default:
		return fmt.Sprintf("%q(U+%04X)", r, r)
	}
}

func traceByte(b byte) string {
	switch b {
	case '\r':
		return "CR(0x0d)"
	case '\n':
		return "LF(0x0a)"
	case '\x1b':
		return "ESC(0x1b)"
	case '[':
		return "'['(0x5b)"
	default:
		if b >= 0x20 && b <= 0x7e {
			return fmt.Sprintf("%q(0x%02x)", b, b)
		}
		return fmt.Sprintf("0x%02x", b)
	}
}

func lineEditActionName(action lineEditAction) string {
	switch action {
	case lineEditIgnore:
		return "ignore"
	case lineEditHome:
		return "home"
	case lineEditEnd:
		return "end"
	case lineEditLeft:
		return "left"
	case lineEditRight:
		return "right"
	case lineEditBackspace:
		return "backspace"
	case lineEditDelete:
		return "delete"
	case lineEditPaste:
		return "paste"
	case lineEditHistoryPrev:
		return "history-prev"
	case lineEditHistoryNext:
		return "history-next"
	case lineEditInsertNewline:
		return "insert-newline"
	case lineEditInsertText:
		return "insert-text"
	case lineEditSubmit:
		return "submit"
	case lineEditEdit:
		return "edit"
	case lineEditEOF:
		return "eof"
	case lineEditInterrupt:
		return "interrupt"
	case lineEditEscape:
		return "escape"
	case lineEditShiftModifier:
		return "shift-modifier"
	default:
		return fmt.Sprintf("unknown(%d)", action)
	}
}

func (e *promptLineEditor) readBracketedPaste() (string, error) {
	var b strings.Builder
	for {
		c, err := e.r.ReadByte()
		if err != nil {
			return b.String(), err
		}
		b.WriteByte(c)
		text := b.String()
		if strings.HasSuffix(text, bracketedPasteEnd) {
			return strings.TrimSuffix(text, bracketedPasteEnd), nil
		}
	}
}

func (e *promptLineEditor) addHistory(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if strings.ContainsAny(text, "\r\n") {
		return
	}
	if len(e.history) > 0 && e.history[len(e.history)-1] == text {
		return
	}
	e.history = append(e.history, text)
	if e.onNewHistory != nil {
		e.onNewHistory(text)
	}
}

// SetInitialHistory replaces the in-memory history with entries loaded from
// the session file. This is typically called during REPL initialization.
func (e *promptLineEditor) SetInitialHistory(entries []string) {
	e.history = entries
}

func (e *promptLineEditor) historyState() lineEditHistory {
	return lineEditHistory{index: len(e.history), items: e.history}
}

type lineEditHistory struct {
	index int
	draft string
	seen  bool
	items []string
}

func (h *lineEditHistory) prev(s *lineEditState) {
	if len(h.items) == 0 {
		return
	}
	if !h.seen {
		h.draft = string(s.buf)
		h.index = len(h.items)
		h.seen = true
	}
	if h.index == 0 {
		return
	}
	h.index--
	s.setText(h.items[h.index])
}

func (h *lineEditHistory) next(s *lineEditState) {
	if !h.seen {
		return
	}
	if h.index < len(h.items)-1 {
		h.index++
		s.setText(h.items[h.index])
		return
	}
	h.index = len(h.items)
	h.seen = false
	s.setText(h.draft)
}

type lineEditState struct {
	prompt string
	buf    []rune
	cursor int
	// summary, when non-empty, is a one-line placeholder shown in place of buf
	// (e.g. "[N bytes of pasted content]" for a large paste into an empty prompt).
	// The real content stays in buf and is submitted on Enter; the placeholder
	// avoids scroll lag from rendering a huge paste inline. The first manual
	// keystroke clears it so typing reveals the full content.
	summary string

	drawn     bool
	rows      int
	cursorRow int
	cursorCol int
	endRow    int
	endCol    int
}

// displayRunes returns the runes the editor should render: the summary placeholder
// when set, otherwise the real buffer. buf always holds the real content.
func (s *lineEditState) displayRunes() []rune {
	if s.summary != "" {
		return []rune(s.summary)
	}
	return s.buf
}

// displayCursor returns the cursor position for rendering: at the end of the
// summary placeholder when set, otherwise the real cursor.
func (s *lineEditState) displayCursor() int {
	if s.summary != "" {
		return len([]rune(s.summary))
	}
	return s.cursor
}

// setPasteSummary replaces the buffer with a paste and, when the paste is large
// or multi-line enough, records a one-line placeholder to render instead of the
// full content. cursor lands at the end of the real content.
func (s *lineEditState) setPasteSummary(text string) {
	s.buf = []rune(text)
	s.cursor = len(s.buf)
	if len(text) > pasteSummaryBytes || strings.Count(text, "\n") >= pasteSummaryLines {
		s.summary = fmt.Sprintf(pasteSummaryPlaceholder, len(text))
	} else {
		s.summary = ""
	}
}

func (s *lineEditState) insert(r rune) {
	s.buf = append(s.buf, 0)
	copy(s.buf[s.cursor+1:], s.buf[s.cursor:])
	s.buf[s.cursor] = r
	s.cursor++
}

func (s *lineEditState) insertString(text string) {
	for _, r := range text {
		s.insert(r)
	}
}

func (s *lineEditState) replaceRange(start, end int, text string) {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(s.buf) {
		end = len(s.buf)
	}
	next := make([]rune, 0, len(s.buf)-(end-start)+len([]rune(text)))
	next = append(next, s.buf[:start]...)
	next = append(next, []rune(text)...)
	next = append(next, s.buf[end:]...)
	s.buf = next
	s.cursor = start + len([]rune(text))
	s.summary = "" // a content replacement reveals the real buffer
}

func (s *lineEditState) setText(text string) {
	s.buf = []rune(text)
	s.cursor = len(s.buf)
	s.summary = "" // a content replacement reveals the real buffer
}

func (s *lineEditState) home() {
	s.cursor = 0
}

func (s *lineEditState) end() {
	s.cursor = len(s.buf)
}

func (s *lineEditState) left() {
	if s.cursor > 0 {
		s.cursor--
	}
}

func (s *lineEditState) right() {
	if s.cursor < len(s.buf) {
		s.cursor++
	}
}

func (s *lineEditState) backspace() {
	if s.cursor == 0 {
		return
	}
	copy(s.buf[s.cursor-1:], s.buf[s.cursor:])
	s.buf = s.buf[:len(s.buf)-1]
	s.cursor--
}

func (s *lineEditState) delete() {
	if s.cursor >= len(s.buf) {
		return
	}
	copy(s.buf[s.cursor:], s.buf[s.cursor+1:])
	s.buf = s.buf[:len(s.buf)-1]
}

func (s *lineEditState) redraw(w io.Writer, cols int) error {
	if err := s.moveToPromptStart(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s%s", s.prompt, string(s.displayRunes())); err != nil {
		return err
	}

	cursorRow, cursorCol, rows, endRow, endCol := s.metrics(cols)
	if err := moveDisplayCursor(w, endRow, endCol, cursorRow, cursorCol); err != nil {
		return err
	}
	s.drawn = true
	s.rows = rows
	s.cursorRow = cursorRow
	s.cursorCol = cursorCol
	s.endRow = endRow
	s.endCol = endCol
	return nil
}

func (s *lineEditState) finish(w io.Writer) error {
	if s.drawn {
		if err := moveDisplayCursor(w, s.cursorRow, s.cursorCol, s.endRow, s.endCol); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	s.drawn = false
	s.rows = 0
	s.cursorRow = 0
	s.cursorCol = 0
	s.endRow = 0
	s.endCol = 0
	return nil
}

func (s *lineEditState) moveToPromptStart(w io.Writer) error {
	if !s.drawn {
		_, err := fmt.Fprint(w, "\r\x1b[2K")
		return err
	}
	if s.cursorCol > 0 {
		if _, err := fmt.Fprint(w, "\r"); err != nil {
			return err
		}
	}
	if s.cursorRow > 0 {
		if _, err := fmt.Fprintf(w, "\x1b[%dA", s.cursorRow); err != nil {
			return err
		}
	}
	if err := clearPromptRows(w, s.rows); err != nil {
		return err
	}
	if s.rows > 1 {
		if _, err := fmt.Fprintf(w, "\x1b[%dA", s.rows-1); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\r"); err != nil {
		return err
	}
	return nil
}

func (s *lineEditState) metrics(cols int) (cursorRow, cursorCol, rows, endRow, endCol int) {
	buf := s.displayRunes()
	cursor := s.displayCursor()
	if cols <= 0 {
		cursorRow, cursorCol, rows = s.metricsNoWrap()
		endRow, endCol = textPositionNoWrap(s.prompt + string(buf))
		return cursorRow, cursorCol, rows, endRow, endCol
	}
	row, col := textPositionWrapped(s.prompt, cols)
	cursorRow, cursorCol = row, col
	seenCursor := cursor == 0
	for i, r := range buf {
		if seenCursor {
			rows = max(rows, row+occupiedRows(col+1, cols))
		}
		if r == '\n' {
			rows = max(rows, row+occupiedRows(col+1, cols))
			row += occupiedRows(col+1, cols)
			col = 0
		} else {
			col++
			if col >= cols {
				row += col / cols
				col %= cols
			}
		}
		if i+1 == cursor {
			cursorRow, cursorCol = row, col
			seenCursor = true
		}
	}
	rows = max(rows, row+occupiedRows(col+1, cols))
	return cursorRow, cursorCol, rows, row, col
}

func (s *lineEditState) metricsNoWrap() (cursorRow, cursorCol, rows int) {
	buf := s.displayRunes()
	cursor := s.displayCursor()
	row, col := textPositionNoWrap(s.prompt)
	cursorRow, cursorCol = row, col
	seenCursor := cursor == 0
	for i, r := range buf {
		if r == '\n' {
			row++
			col = 0
		} else {
			col++
		}
		if i+1 == cursor {
			cursorRow, cursorCol = row, col
			seenCursor = true
		}
	}
	if !seenCursor {
		cursorRow, cursorCol = row, col
	}
	return cursorRow, cursorCol, row + 1
}

func textPositionNoWrap(text string) (row, col int) {
	for _, r := range text {
		if r == '\n' {
			row++
			col = 0
			continue
		}
		col++
	}
	return row, col
}

func textPositionWrapped(text string, cols int) (row, col int) {
	if cols <= 0 {
		return textPositionNoWrap(text)
	}
	for _, r := range text {
		if r == '\n' {
			row += occupiedRows(col+1, cols)
			col = 0
			continue
		}
		col++
		if col >= cols {
			row += col / cols
			col %= cols
		}
	}
	return row, col
}

func clearPromptRows(w io.Writer, rows int) error {
	if rows < 1 {
		rows = 1
	}
	for i := 0; i < rows; i++ {
		if _, err := fmt.Fprint(w, "\r\x1b[2K"); err != nil {
			return err
		}
		if i < rows-1 {
			if _, err := fmt.Fprint(w, "\x1b[B"); err != nil {
				return err
			}
		}
	}
	return nil
}

func moveDisplayCursor(w io.Writer, fromRow, fromCol, toRow, toCol int) error {
	if fromRow != toRow {
		if fromCol > 0 {
			if _, err := fmt.Fprint(w, "\r"); err != nil {
				return err
			}
		}
		if toRow < fromRow {
			if _, err := fmt.Fprintf(w, "\x1b[%dA", fromRow-toRow); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w, "\x1b[%dB", toRow-fromRow); err != nil {
				return err
			}
		}
		if toCol > 0 {
			if _, err := fmt.Fprintf(w, "\x1b[%dC", toCol); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case toCol < fromCol:
		if _, err := fmt.Fprint(w, "\r"); err != nil {
			return err
		}
		if toCol > 0 {
			if _, err := fmt.Fprintf(w, "\x1b[%dC", toCol); err != nil {
				return err
			}
		}
	case toCol > fromCol:
		if _, err := fmt.Fprintf(w, "\x1b[%dC", toCol-fromCol); err != nil {
			return err
		}
	}
	return nil
}

func occupiedRows(cells, cols int) int {
	if cells <= 0 || cols <= 0 {
		return 1
	}
	return ((cells - 1) / cols) + 1
}
