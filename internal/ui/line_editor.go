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
	"sync"
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
// submit). A pasted range that exceeds pasteSummaryBytes renders as a one-line
// placeholder wherever it appears in surrounding prompt text to avoid scroll
// lag.
const (
	pasteEnterGap           = 10 * time.Millisecond
	pasteExitGap            = 150 * time.Millisecond
	pasteSummaryBytes       = 1000
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
	// noHistory suppresses committing submitted lines to history for the current
	// read. It is set for auxiliary prompts (e.g. /model menu selections, save
	// confirmations) so transient menu answers never pollute the recallable REPL
	// history; the main idle prompt leaves it false.
	noHistory bool
	// cycleAgent enables Shift-Tab only for the main idle REPL prompt. Auxiliary
	// reads and during-prompt capture leave it false so the key remains inert.
	cycleAgent bool

	// dimSubmittedPrompt redraws a submitted main-prompt line in dim style as it
	// is finished, so the committed prompt reads as distinct from the model output
	// that follows. Auxiliary prompts (noHistory) and during-prompt capture leave
	// it inert. Set from the renderer's color decision.
	dimSubmittedPrompt bool

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
	prevCursor      int  // cursor before the previous keystroke, used to locate a fallback paste
	pasteStart      int  // buffer-rune offset where the active fallback paste began
	pasteCRPending  bool // paste-mode CR inserted LF; suppress a following LF in CRLF input
	purePaste       bool // an unedited paste fills the buffer; submitted literally

	// viPrompt, when non-nil, returns the fully rendered main REPL prompt for a
	// given vi mode. It is invoked at the two mode-transition chokepoints
	// (enterNormal / enterInsert) and once at read start so a {vimode}
	// placeholder flips live as the user switches mode. The REPL temporarily
	// clears it for auxiliary prompts whose labels are context-specific.
	viPrompt func(viMode) string

	// cursorShapeSeq tracks the last DECSCUSR sequence this editor wrote while a
	// vi-mode raw prompt is active, avoiding duplicate writes on redraws that keep
	// the same vi mode.
	cursorShapeSeq string

	// terminalMu serializes prompt redraw/finish writes against asynchronous log
	// writes that temporarily clear and repaint the active prompt. activePrompt is
	// a display-only snapshot of the last drawn prompt state, protected by the same
	// mutex so background logs can move the prompt below themselves without racing
	// the input goroutine's terminal bytes.
	terminalMu   sync.Mutex
	activePrompt *lineEditState
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
	cursorBefore := s.cursor
	if !e.lastKeyTime.IsZero() {
		gap := now.Sub(e.lastKeyTime)
		if gap <= pasteEnterGap {
			if !e.pasteMode {
				e.pasteMode = true
				e.purePaste = e.prevBufferEmpty
				e.pasteStart = e.prevCursor
			}
		} else if e.pasteMode && gap > pasteExitGap {
			e.pasteMode = false
			e.pasteCRPending = false
		}
	}
	e.lastKeyTime = now
	e.prevBufferEmpty = emptyBefore
	e.prevCursor = cursorBefore
}

// markManualEdit clears only the pure-paste literal guarantee: collapsed paste
// ranges remain atomic placeholders while surrounding text is edited. It is a
// no-op while a paste burst is in progress, so paste bytes never clear the flag.
func (e *promptLineEditor) markManualEdit(s *lineEditState) {
	if e.pasteMode {
		return
	}
	e.purePaste = false
}

// refreshPasteSummary collapses the active non-bracketed fallback paste once it
// exceeds the byte threshold. Only the detected burst is collapsed, preserving
// any surrounding prompt or editor-prefill text. Called only while in paste mode.
func (e *promptLineEditor) refreshPasteSummary(s *lineEditState) {
	start, end := e.pasteStart, s.cursor
	if start < 0 || end < start || end > len(s.buf) {
		return
	}
	s.setPasteSummaryRange(start, end, string(s.buf[start:end]))
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

func (e *promptLineEditor) redrawPromptState(s *lineEditState, mode viMode) error {
	e.terminalMu.Lock()
	defer e.terminalMu.Unlock()
	if err := e.applyPromptCursorShape(mode); err != nil {
		return err
	}
	if err := s.redraw(e.w, e.terminalColumns()); err != nil {
		return err
	}
	e.activePrompt = s.promptSnapshot()
	return nil
}

func (e *promptLineEditor) finishPromptState(s *lineEditState) error {
	e.terminalMu.Lock()
	defer e.terminalMu.Unlock()
	if e.dimSubmittedPrompt && !e.noHistory {
		if err := s.finishStyled(e.w, ansiDim, ansiReset); err != nil {
			return err
		}
	} else if err := s.finish(e.w); err != nil {
		return err
	}
	e.activePrompt = nil
	return nil
}

func (e *promptLineEditor) clearPromptSnapshot() {
	e.terminalMu.Lock()
	e.activePrompt = nil
	e.terminalMu.Unlock()
}

func (e *promptLineEditor) resetPromptCursorShapeLocked() error {
	e.terminalMu.Lock()
	defer e.terminalMu.Unlock()
	return e.resetPromptCursorShape()
}

func (e *promptLineEditor) writeBackground(p []byte) (int, error) {
	e.terminalMu.Lock()
	defer e.terminalMu.Unlock()
	if e.activePrompt == nil || !e.activePrompt.drawn {
		return e.w.Write(p)
	}
	prompt := e.activePrompt
	if err := prompt.moveToPromptStart(e.w); err != nil {
		return 0, err
	}
	// The prompt has been erased from the screen; redraw it from the current cursor
	// position after the log line scrolls.
	prompt.drawn = false
	n, err := e.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := prompt.redraw(e.w, e.terminalColumns()); err != nil {
		return n, err
	}
	e.activePrompt = prompt
	return n, nil
}

func (e *promptLineEditor) applyPromptCursorShape(mode viMode) error {
	if e.editMode != promptEditModeVi {
		return nil
	}
	shape := term.CursorShapeSteadyBar
	if mode == viModeNormal {
		shape = term.CursorShapeSteadyBlock
	}
	seq := term.CursorShapeSequence(shape)
	if seq == e.cursorShapeSeq {
		return nil
	}
	if _, err := io.WriteString(e.w, seq); err != nil {
		return err
	}
	e.cursorShapeSeq = seq
	return nil
}

func (e *promptLineEditor) resetPromptCursorShape() error {
	if e.cursorShapeSeq == "" {
		return nil
	}
	if _, err := io.WriteString(e.w, term.CursorShapeSequence(term.CursorShapeDefault)); err != nil {
		return err
	}
	e.cursorShapeSeq = ""
	return nil
}

// readPrefilled reads a line with the editor seeded with editable prefill text
// (cursor at end). It backs both the bare prompt and the during-prompt deposit,
// where the text typed while the model ran is handed back for review/edit before
// the user submits it manually (during-prompt input).
func (e *promptLineEditor) readPrefilled(prompt, prefill string) (replInput, bool, error) {
	return e.readPrefilledClassified(prompt, prefill, false)
}

func (e *promptLineEditor) readPrefilledClassified(prompt, prefill string, purePaste bool) (replInput, bool, error) {
	state := lineEditState{prompt: prompt}
	defer e.clearPromptSnapshot()
	if prefill != "" {
		state.setText(prefill)
	}
	// Each prompt starts fresh: paste mode and the pure-paste literal flag must not
	// carry over from a previous prompt (a typed prompt after a pasted one is
	// authored, not literal). lastKeyTime is reset so the first keystroke of this
	// prompt cannot be mistaken for a paste continuation.
	e.pasteMode = false
	e.pasteCRPending = false
	e.purePaste = purePaste
	e.lastKeyTime = time.Time{}
	e.prevBufferEmpty = len(state.buf) == 0
	e.prevCursor = state.cursor
	e.pasteStart = state.cursor
	history := e.historyState()
	vi := viLineState{mode: viModeInsert}
	e.refreshViPrompt(&vi, &state)
	if e.editMode == promptEditModeVi {
		defer func() { _ = e.resetPromptCursorShapeLocked() }()
	}
	e.tracef("read start prompt=%q", prompt)
	if err := e.redrawPromptState(&state, vi.mode); err != nil {
		return replInput{}, false, err
	}

	for {
		r, size, err := e.r.ReadRune()
		if err != nil {
			if errors.Is(err, io.EOF) && len(state.buf) > 0 {
				e.tracef("read eof with buffered text len=%d purePaste=%v", len(state.buf), e.purePaste)
				if err := e.finishPromptState(&state); err != nil {
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

		result, err := e.handleKey(&vi, &state, &history, prompt, r, false)
		if err != nil {
			return replInput{}, false, err
		}
		if result.done {
			return result.input, result.ok, nil
		}
		if result.redraw {
			if err := e.redrawPromptState(&state, vi.mode); err != nil {
				return replInput{}, false, err
			}
		}
	}
}

// handleKey dispatches one decoded keystroke against the shared editor state. It
// backs both the idle prompt (readPrefilled, which redraws on result.redraw) and
// the during-prompt capture (readDuringPrompt, which mirrors buf/cursor on the status
// line). Vi normal mode delegates to handleViNormalInput; emacs and vi insert
// mode share the per-key switch below.
//
// duringPrompt selects the during-prompt capture semantics, which differ from the
// idle prompt only in terminal-finishing behavior (the multi-row redraw cannot run
// while output streams):
//
//   - Submit (raw CR / escape-submit / vi Enter) commits the current buffer via
//     submitDuringPrompt: it is returned as queued input the run loop runs as the
//     next prompt (no terminal finish). Shift-Enter/raw LF still inserts a newline
//     for multiline prompts.
//   - Edit (Ctrl-G / escape-edit) returns done with an edit request but does not
//     finish or commit history; the run loop opens $EDITOR on the buffer.
//   - Bare Esc returns done as an escape gesture (for double-Esc cancel).
//   - EOF/interrupt return done for the run loop to deposit/cancel.
//
// Every motion, insert, delete, history recall, and vi command is shared
// verbatim with the idle prompt, so the during-prompt buffer gets the same editing
// grammar (Ctrl-A/E/B/F, arrows, word motions, kill commands, full vi mode,
// vi-mode line-aware up/down history) as the idle prompt.
func (e *promptLineEditor) handleKey(v *viLineState, s *lineEditState, h *lineEditHistory, prompt string, r rune, duringPrompt bool) (viEditResult, error) {
	if duringPrompt {
		// The during-prompt buffer can carry a stale cursor (e.g. after a history
		// recall or an external reset); clamp it before any edit so insert/
		// backspace never index out of range. The idle editor keeps the cursor in
		// bounds via its own operations, so this is during-prompt-only.
		if s.cursor < 0 {
			s.cursor = 0
		}
		if s.cursor > len(s.buf) {
			s.cursor = len(s.buf)
		}
	}
	if e.editMode == promptEditModeVi && v.mode == viModeNormal {
		return e.handleViNormalInput(v, s, h, prompt, r, duringPrompt)
	}
	if e.pasteCRPending && !(e.pasteMode && r == '\n') {
		e.pasteCRPending = false
	}
	switch r {
	case '\r':
		if e.consumeShiftEnterPending() {
			e.tracef("raw CR after shift modifier inserts newline")
			e.markManualEdit(s)
			s.insert('\n')
			return viEditResult{redraw: true}, nil
		}
		if e.pasteMode {
			e.tracef("raw CR in paste inserts newline")
			s.insert('\n')
			e.pasteCRPending = true
			e.refreshPasteSummary(s)
			return viEditResult{redraw: true}, nil
		}
		if duringPrompt {
			e.tracef("raw CR during prompt submits text len=%d purePaste=%v", len(s.buf), e.purePaste)
			return e.submitDuringPrompt(s), nil
		}
		e.tracef("raw CR submits text len=%d purePaste=%v", len(s.buf), e.purePaste)
		return e.submit(s)
	case '\n':
		if e.pasteMode && e.pasteCRPending {
			e.tracef("raw LF after paste CR ignored")
			e.pasteCRPending = false
			return viEditResult{}, nil
		}
		e.consumeShiftEnterPending()
		e.markManualEdit(s)
		e.tracef("raw LF inserts newline")
		s.insert('\n')
		if e.pasteMode {
			e.refreshPasteSummary(s)
		}
		return viEditResult{redraw: true}, nil
	case ctrlA:
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		e.tracef("raw ctrl-a moves to start")
		s.home()
		return viEditResult{redraw: true}, nil
	case ctrlB:
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		e.tracef("raw ctrl-b moves left")
		s.left()
		return viEditResult{redraw: true}, nil
	case rune(lineTermEdit):
		e.clearShiftEnterPending()
		e.tracef("raw ctrl-g opens editor text len=%d", len(s.buf))
		return e.edit(s, duringPrompt)
	case ctrlE:
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		e.tracef("raw ctrl-e moves to end")
		s.end()
		return viEditResult{redraw: true}, nil
	case ctrlF:
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		e.tracef("raw ctrl-f moves right")
		s.right()
		return viEditResult{redraw: true}, nil
	case ctrlC:
		e.clearShiftEnterPending()
		e.tracef("raw ctrl-c interrupts")
		return viEditResult{input: replInput{interrupt: true}, ok: true, done: true}, nil
	case ctrlD:
		e.clearShiftEnterPending()
		e.tracef("raw ctrl-d text len=%d", len(s.buf))
		if len(s.buf) == 0 {
			return viEditResult{ok: false, done: true}, nil
		}
		return viEditResult{redraw: true}, nil
	case '\t':
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		handled, err := e.completePromptTab(s)
		if err != nil {
			return viEditResult{}, err
		}
		if !handled {
			s.insert('\t')
		}
		return viEditResult{redraw: true}, nil
	case '\b', del:
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		e.tracef("raw backspace")
		s.backspace()
		return viEditResult{redraw: true}, nil
	case rune(lineTermEscape):
		action, text, err := e.readEscape()
		if err != nil {
			if errors.Is(err, io.EOF) {
				e.tracef("escape read eof")
				return viEditResult{ok: false, done: true}, nil
			}
			e.tracef("escape read error: %v", err)
			return viEditResult{}, err
		}
		e.tracef("escape action=%s text=%q", lineEditActionName(action), text)
		if e.editMode == promptEditModeVi && action == lineEditEscape {
			e.clearShiftEnterPending()
			e.viEnterNormal(v, s)
			if duringPrompt {
				// During a prompt, the first Esc in vi insert mode still enters normal
				// mode, but it must also count as the first press of the double-Esc
				// cancel gesture; otherwise vi mode requires three Esc presses.
				return viEditResult{input: replInput{escape: true}, ok: true, done: true, redraw: true}, nil
			}
			return viEditResult{redraw: true}, nil
		}
		// During a prompt a bare Esc is the double-Esc cancel gesture (the idle
		// prompt treats bare Esc as a no-op in emacs mode). Surface it so the run
		// loop can detect two within the window.
		if duringPrompt && action == lineEditEscape {
			e.clearShiftEnterPending()
			return viEditResult{input: replInput{escape: true}, ok: true, done: true}, nil
		}
		if action == lineEditShiftModifier {
			e.markShiftEnterPending()
			return viEditResult{redraw: true}, nil
		}
		e.clearShiftEnterPending()
		if action == lineEditSubmit {
			if duringPrompt {
				e.tracef("escape submit during prompt submits text len=%d purePaste=%v", len(s.buf), e.purePaste)
				return e.submitDuringPrompt(s), nil
			}
			e.tracef("escape submit text len=%d purePaste=%v", len(s.buf), e.purePaste)
			return e.submit(s)
		}
		switch action {
		case lineEditHome:
			e.markManualEdit(s)
			s.home()
		case lineEditEnd:
			e.markManualEdit(s)
			s.end()
		case lineEditLeft:
			e.markManualEdit(s)
			s.left()
		case lineEditRight:
			e.markManualEdit(s)
			s.right()
		case lineEditBackspace:
			e.markManualEdit(s)
			s.backspace()
		case lineEditDelete:
			e.markManualEdit(s)
			s.delete()
		case lineEditEdit:
			return e.edit(s, duringPrompt)
		case lineEditEOF:
			if len(s.buf) == 0 {
				return viEditResult{ok: false, done: true}, nil
			}
			return viEditResult{redraw: true}, nil
		case lineEditInterrupt:
			return viEditResult{input: replInput{interrupt: true}, ok: true, done: true}, nil
		case lineEditCycleAgent:
			return e.cycleAgentInput(s, duringPrompt)
		case lineEditInsertNewline:
			e.markManualEdit(s)
			s.insert('\n')
			if e.pasteMode {
				e.refreshPasteSummary(s)
			}
			e.discardBufferedRawEnter()
		case lineEditInsertText:
			e.markManualEdit(s)
			if text == "\t" {
				handled, err := e.completePromptTab(s)
				if err != nil {
					return viEditResult{}, err
				}
				if handled {
					break
				}
			}
			s.insertString(text)
			if e.pasteMode {
				e.refreshPasteSummary(s)
			}
		case lineEditHistoryPrev:
			e.markManualEdit(s)
			if e.editMode == promptEditModeVi {
				e.viMoveLineUpOrHistory(s, h, 1)
			} else {
				h.prev(s)
			}
		case lineEditHistoryNext:
			e.markManualEdit(s)
			if e.editMode == promptEditModeVi {
				e.viMoveLineDownOrHistory(s, h, 1)
			} else {
				h.next(s)
			}
		case lineEditPaste:
			if !duringPrompt {
				wasEmpty := len(s.buf) == 0
				s.insertPastedText(text)
				if wasEmpty {
					e.purePaste = true
				}
				e.tracef("bracketed paste inserts len=%d summaries=%d", len(text), len(s.pasteSummaries))
				break
			}
			s.insertString(text)
		}
		return viEditResult{redraw: true}, nil
	default:
		e.clearShiftEnterPending()
		e.markManualEdit(s)
		if r == '\t' || unicode.IsPrint(r) {
			s.insert(r)
			if e.pasteMode {
				e.refreshPasteSummary(s)
			}
			return viEditResult{redraw: true}, nil
		}
		return viEditResult{redraw: true}, nil
	}
}

// submit finishes the line, commits it to history, and returns it as a submitted
// input. Centralized so the idle prompt's several submit paths (raw CR, escape
// submit, vi submit) share one history/finish sequence.
func (e *promptLineEditor) submit(s *lineEditState) (viEditResult, error) {
	if err := e.finishPromptState(s); err != nil {
		return viEditResult{}, err
	}
	e.addHistory(string(s.buf))
	return viEditResult{input: replInput{text: string(s.buf), pasted: e.purePaste}, ok: true, done: true}, nil
}

// submitDuringPrompt commits the current during-prompt buffer as queued next-prompt
// input. Unlike submit it does not finish a terminal prompt (the multi-row redraw
// cannot run while output streams); it clears the buffer so live capture can keep
// reading more input while the turn runs. An empty Enter is a no-op so a stray
// keypress does not stop capture. Submissions are marked interactive so they carry
// the same !/command/$skill semantics as an idle-prompt line.
func (e *promptLineEditor) submitDuringPrompt(s *lineEditState) viEditResult {
	text := string(s.buf)
	if text == "" {
		return viEditResult{redraw: true}
	}
	pasted := e.purePaste
	e.addHistory(text)
	s.buf = nil
	s.cursor = 0
	s.clearPasteSummaries()
	e.purePaste = false
	return viEditResult{input: replInput{text: text, pasted: pasted, interactive: true}, ok: true, done: true, redraw: true}
}

// edit returns an edit request so the caller opens $EDITOR on the text. At the
// idle prompt it finishes the line and commits history first; during a prompt it
// leaves the buffer intact (no terminal finish, no history) and hands the text
// to the run loop, which opens the editor against the deposited buffer.
func (e *promptLineEditor) edit(s *lineEditState, duringPrompt bool) (viEditResult, error) {
	if duringPrompt {
		return viEditResult{input: replInput{text: string(s.buf), edit: true}, ok: true, done: true}, nil
	}
	if err := e.finishPromptState(s); err != nil {
		return viEditResult{}, err
	}
	e.addHistory(string(s.buf))
	return viEditResult{input: replInput{text: string(s.buf), edit: true}, ok: true, done: true}, nil
}

// cycleAgentInput finishes only the main idle prompt and returns its draft for
// the REPL to prefill again after switching agents. It intentionally does not
// commit history or mark the draft as manually edited.
func (e *promptLineEditor) cycleAgentInput(s *lineEditState, duringPrompt bool) (viEditResult, error) {
	if duringPrompt || !e.cycleAgent {
		return viEditResult{redraw: true}, nil
	}
	if err := e.finishPromptState(s); err != nil {
		return viEditResult{}, err
	}
	return viEditResult{
		input: replInput{text: string(s.buf), pasted: e.purePaste, cycleAgent: true},
		ok:    true,
		done:  true,
	}, nil
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
	lineEditCycleAgent
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
		return lineEditCycleAgent, ""
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

func (e *promptLineEditor) completePromptTab(s *lineEditState) (bool, error) {
	if handled, err := e.completeAtFileReference(s); err != nil || handled {
		return handled, err
	}
	return e.completeBangLine(s)
}

type bangCompletionCandidate struct {
	value   string
	display string
	isDir   bool
}

func (e *promptLineEditor) completeAtFileReference(s *lineEditState) (bool, error) {
	start, end, token, quoted, ok := atFileCompletionContext(s.buf, s.cursor)
	if !ok {
		return false, nil
	}
	candidates := bangPathCompletions(token)
	if len(candidates) == 0 {
		return true, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].display < candidates[j].display
	})
	if len(candidates) == 1 {
		text, cursor := formatPromptRefReplacement(candidates[0].value, candidates[0].isDir, true, quoted)
		s.replaceRangeWithCursor(start, end, text, cursor)
		return true, nil
	}
	common := longestCommonCompletionPrefix(candidates)
	if len([]rune(common)) > len([]rune(token)) {
		text, cursor := formatPromptRefReplacement(common, false, false, quoted)
		s.replaceRangeWithCursor(start, end, text, cursor)
		return true, nil
	}
	if err := e.finishPromptState(s); err != nil {
		return true, err
	}
	for _, candidate := range candidates {
		if _, err := fmt.Fprintln(e.w, formatPromptRefCandidate(candidate.value, candidate.isDir)); err != nil {
			return true, err
		}
	}
	return true, nil
}

func atFileCompletionContext(buf []rune, cursor int) (start, end int, token string, quoted bool, ok bool) {
	if !promptRefCompletionAllowed(buf) {
		return 0, 0, "", false, false
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(buf) {
		cursor = len(buf)
	}
	for i := cursor - 1; i >= 0; i-- {
		if buf[i] != '@' || !isPromptRefBoundary(buf, i) || i+1 >= len(buf) || buf[i+1] != '"' || cursor < i+2 {
			continue
		}
		raw := buf[i+2 : cursor]
		if promptRefContainsUnescapedQuote(raw) {
			continue
		}
		end = cursor
		if cursor < len(buf) && buf[cursor] == '"' {
			end = cursor + 1
		}
		return i, end, unescapePromptRefPath(string(raw)), true, true
	}
	start = cursor
	for start > 0 && !unicode.IsSpace(buf[start-1]) {
		start--
	}
	if start < len(buf) && buf[start] == '@' && isPromptRefBoundary(buf, start) {
		if start+1 < len(buf) && buf[start+1] == '"' {
			return 0, 0, "", false, false
		}
		return start, cursor, string(buf[start+1 : cursor]), false, true
	}
	return 0, 0, "", false, false
}

func promptRefCompletionAllowed(buf []rune) bool {
	if len(buf) == 0 {
		return true
	}
	if buf[0] == '!' && (len(buf) < 2 || buf[1] != '!') {
		return false
	}
	if buf[0] == '/' && (len(buf) < 2 || buf[1] != '/') {
		return false
	}
	return true
}

func promptRefContainsUnescapedQuote(runes []rune) bool {
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\') {
			i++
			continue
		}
		if runes[i] == '"' {
			return true
		}
	}
	return false
}

func formatPromptRefReplacement(path string, isDir, final, forceQuote bool) (string, int) {
	if isDir && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	quote := forceQuote || needsPromptRefQuotes(path)
	if quote {
		text := "@\"" + escapePromptRefPath(path) + "\""
		if final && !isDir {
			text += " "
			return text, len([]rune(text))
		}
		return text, len([]rune(text)) - 1
	}
	text := "@" + path
	if final && !isDir {
		text += " "
	}
	return text, len([]rune(text))
}

func formatPromptRefCandidate(path string, isDir bool) string {
	if isDir && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	if needsPromptRefQuotes(path) {
		return "@\"" + escapePromptRefPath(path) + "\""
	}
	return "@" + path
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
	if err := e.finishPromptState(s); err != nil {
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
	case lineEditCycleAgent:
		return "cycle-agent"
	default:
		return fmt.Sprintf("unknown(%d)", action)
	}
}

func (e *promptLineEditor) readBracketedPaste() (string, error) {
	var b strings.Builder
	for {
		c, err := e.r.ReadByte()
		if err != nil {
			return normalizePastedNewlines(b.String()), err
		}
		b.WriteByte(c)
		text := b.String()
		if strings.HasSuffix(text, bracketedPasteEnd) {
			return normalizePastedNewlines(strings.TrimSuffix(text, bracketedPasteEnd)), nil
		}
	}
}

// normalizePastedNewlines rewrites a pasted block's line endings to '\n'.
//
// The prompt runs in raw mode with ICRNL cleared (see term.promptRawTermios),
// so the kernel no longer translates carriage return to newline on input and a
// terminal delivers a paste's line breaks as CR ("\r") or CRLF ("\r\n")
// rather than LF. Left raw, those carriage returns corrupt the prompt (a bare
// CR snaps the cursor back to column 0 and overwrites the prompt line), prevent
// consistent normalized rendering, and leak "^M" into the external editor.
// Collapsing CRLF before bare CR yields exactly one '\n' per
// pasted line for every CR/CRLF/LF terminal convention (and mixtures of them),
// and is a no-op for already-LF content. Scoped to pasted blocks only, so a
// typed Enter (raw CR) is still handled as a submit elsewhere.
func normalizePastedNewlines(text string) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func shouldSummarizePaste(text string) bool {
	return len(text) > pasteSummaryBytes
}

func (e *promptLineEditor) addHistory(text string) {
	if e.noHistory {
		return
	}
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
	// pasteSummaries replace pasted rune ranges for display while buf retains the
	// full text submitted to the model or external editor. Ranges stay sorted and
	// non-overlapping and behave as atomic placeholders in the inline editor.
	pasteSummaries []pasteSummary

	drawn     bool
	rows      int
	cursorRow int
	cursorCol int
	endRow    int
	endCol    int
}

type pasteSummary struct {
	start int
	end   int
	label []rune
}

// displayRunes returns the full prompt with summarized paste ranges replaced by
// their placeholders. buf always holds the original pasted content.
func (s *lineEditState) displayRunes() []rune {
	if len(s.pasteSummaries) == 0 {
		return s.buf
	}
	display := make([]rune, 0, len(s.buf))
	previous := 0
	for _, summary := range s.pasteSummaries {
		display = append(display, s.buf[previous:summary.start]...)
		display = append(display, summary.label...)
		previous = summary.end
	}
	return append(display, s.buf[previous:]...)
}

// displayCursor maps the full-buffer cursor into the collapsed display.
func (s *lineEditState) displayCursor() int {
	offset := 0
	for _, summary := range s.pasteSummaries {
		if s.cursor <= summary.start {
			return s.cursor + offset
		}
		if s.cursor < summary.end {
			return summary.start + offset + len(summary.label)
		}
		offset += len(summary.label) - (summary.end - summary.start)
	}
	return s.cursor + offset
}

func (s *lineEditState) promptSnapshot() *lineEditState {
	if s == nil || !s.drawn {
		return nil
	}
	return &lineEditState{
		prompt:    s.prompt,
		buf:       append([]rune(nil), s.displayRunes()...),
		cursor:    s.displayCursor(),
		drawn:     s.drawn,
		rows:      s.rows,
		cursorRow: s.cursorRow,
		cursorCol: s.cursorCol,
		endRow:    s.endRow,
		endCol:    s.endCol,
	}
}

// setPasteSummary replaces the buffer with a paste and records a display
// placeholder when it exceeds the byte threshold. It is retained for the
// empty-prompt vi path; general bracketed pastes use insertPastedText.
func (s *lineEditState) setPasteSummary(text string) {
	s.buf = []rune(text)
	s.cursor = len(s.buf)
	s.clearPasteSummaries()
	s.setPasteSummaryRange(0, len(s.buf), text)
}

func (s *lineEditState) insertPastedText(text string) {
	start := s.normalizeInsertPosition(s.cursor)
	s.cursor = start
	s.replaceRangeWithCursor(start, start, text, len([]rune(text)))
	s.setPasteSummaryRange(start, s.cursor, text)
}

// setPasteSummaryRange records a display-only placeholder when text exceeds the
// byte threshold. The full pasted range remains in buf in either case.
func (s *lineEditState) setPasteSummaryRange(start, end int, text string) {
	kept := s.pasteSummaries[:0]
	for _, summary := range s.pasteSummaries {
		if summary.end <= start || summary.start >= end {
			kept = append(kept, summary)
		}
	}
	s.pasteSummaries = kept
	if !shouldSummarizePaste(text) || start == end {
		return
	}
	s.pasteSummaries = append(s.pasteSummaries, pasteSummary{
		start: start,
		end:   end,
		label: []rune(fmt.Sprintf(pasteSummaryPlaceholder, len(text))),
	})
	sort.Slice(s.pasteSummaries, func(i, j int) bool {
		return s.pasteSummaries[i].start < s.pasteSummaries[j].start
	})
}

func (s *lineEditState) clearPasteSummaries() {
	s.pasteSummaries = nil
}

func (s *lineEditState) normalizeInsertPosition(cursor int) int {
	for _, summary := range s.pasteSummaries {
		if cursor > summary.start && cursor < summary.end {
			return summary.end
		}
	}
	return cursor
}

// expandReplaceRange makes summarized pastes atomic: a replacement that
// touches any part of a collapsed paste consumes that whole pasted range.
func (s *lineEditState) expandReplaceRange(start, end int) (int, int) {
	if start == end {
		position := s.normalizeInsertPosition(start)
		return position, position
	}
	for {
		changed := false
		for _, summary := range s.pasteSummaries {
			if start < summary.end && end > summary.start {
				if summary.start < start {
					start = summary.start
					changed = true
				}
				if summary.end > end {
					end = summary.end
					changed = true
				}
			}
		}
		if !changed {
			return start, end
		}
	}
}

func (s *lineEditState) adjustPasteSummariesForReplace(start, end, inserted int) {
	delta := inserted - (end - start)
	kept := s.pasteSummaries[:0]
	for _, summary := range s.pasteSummaries {
		switch {
		case summary.end <= start:
			kept = append(kept, summary)
		case summary.start >= end:
			summary.start += delta
			summary.end += delta
			kept = append(kept, summary)
			// Overlapping summaries are deliberately removed with their full
			// underlying range by expandReplaceRange.
		}
	}
	s.pasteSummaries = kept
}

func (s *lineEditState) insert(r rune) {
	s.replaceRangeWithCursor(s.cursor, s.cursor, string(r), 1)
}

func (s *lineEditState) insertString(text string) {
	for _, r := range text {
		s.insert(r)
	}
}

func (s *lineEditState) replaceRange(start, end int, text string) {
	s.replaceRangeWithCursor(start, end, text, len([]rune(text)))
}

func (s *lineEditState) replaceRangeWithCursor(start, end int, text string, cursor int) {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(s.buf) {
		end = len(s.buf)
	}
	start, end = s.expandReplaceRange(start, end)
	inserted := []rune(text)
	s.adjustPasteSummariesForReplace(start, end, len(inserted))
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(inserted) {
		cursor = len(inserted)
	}
	next := make([]rune, 0, len(s.buf)-(end-start)+len(inserted))
	next = append(next, s.buf[:start]...)
	next = append(next, inserted...)
	next = append(next, s.buf[end:]...)
	s.buf = next
	s.cursor = start + cursor
}

func (s *lineEditState) setText(text string) {
	s.buf = []rune(text)
	s.cursor = len(s.buf)
	s.clearPasteSummaries()
}

func (s *lineEditState) home() {
	s.cursor = 0
}

func (s *lineEditState) end() {
	s.cursor = len(s.buf)
}

func (s *lineEditState) left() {
	for _, summary := range s.pasteSummaries {
		if s.cursor > summary.start && s.cursor <= summary.end {
			s.cursor = summary.start
			return
		}
	}
	if s.cursor > 0 {
		s.cursor--
	}
}

func (s *lineEditState) right() {
	for _, summary := range s.pasteSummaries {
		if s.cursor >= summary.start && s.cursor < summary.end {
			s.cursor = summary.end
			return
		}
	}
	if s.cursor < len(s.buf) {
		s.cursor++
	}
}

func (s *lineEditState) moveLogicalLineUp() bool {
	start, _, col := s.cursorLogicalLine()
	if start == 0 {
		return false
	}
	prevEnd := start - 1
	prevStart := 0
	for i := prevEnd - 1; i >= 0; i-- {
		if s.buf[i] == '\n' {
			prevStart = i + 1
			break
		}
	}
	prevLen := prevEnd - prevStart
	if col > prevLen {
		col = prevLen
	}
	s.cursor = prevStart + col
	s.cursor = s.normalizeInsertPosition(s.cursor)
	return true
}

func (s *lineEditState) moveLogicalLineDown() bool {
	_, end, col := s.cursorLogicalLine()
	if end >= len(s.buf) {
		return false
	}
	nextStart := end + 1
	nextEnd := len(s.buf)
	for i := nextStart; i < len(s.buf); i++ {
		if s.buf[i] == '\n' {
			nextEnd = i
			break
		}
	}
	nextLen := nextEnd - nextStart
	if col > nextLen {
		col = nextLen
	}
	s.cursor = nextStart + col
	s.cursor = s.normalizeInsertPosition(s.cursor)
	return true
}

func (s *lineEditState) cursorLogicalLine() (start, end, col int) {
	cursor := s.cursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(s.buf) {
		cursor = len(s.buf)
	}
	for i := cursor - 1; i >= 0; i-- {
		if s.buf[i] == '\n' {
			start = i + 1
			break
		}
	}
	end = len(s.buf)
	for i := start; i < len(s.buf); i++ {
		if s.buf[i] == '\n' {
			end = i
			break
		}
	}
	col = cursor - start
	lineLen := end - start
	if col < 0 {
		col = 0
	}
	if col > lineLen {
		col = lineLen
	}
	return start, end, col
}

func (s *lineEditState) backspace() {
	if s.cursor == 0 {
		return
	}
	for _, summary := range s.pasteSummaries {
		if s.cursor > summary.start && s.cursor <= summary.end {
			s.replaceRangeWithCursor(summary.start, summary.end, "", 0)
			return
		}
	}
	s.replaceRangeWithCursor(s.cursor-1, s.cursor, "", 0)
}

func (s *lineEditState) delete() {
	if s.cursor >= len(s.buf) {
		return
	}
	for _, summary := range s.pasteSummaries {
		if s.cursor >= summary.start && s.cursor < summary.end {
			s.replaceRangeWithCursor(summary.start, summary.end, "", 0)
			return
		}
	}
	s.replaceRangeWithCursor(s.cursor, s.cursor+1, "", 0)
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

// finishStyled behaves like finish but, when the line is drawn, redraws the
// committed line wrapped in the given ANSI prefix/suffix before the trailing
// newline so it persists on screen in a distinct style (e.g. dim). The styling
// wraps the whole rendered line and resets before the newline, so it never
// affects the editor's cursor metrics (which use the unstyled prompt/buffer) or
// any following output. When the line is not drawn it falls back to finish.
func (s *lineEditState) finishStyled(w io.Writer, prefix, suffix string) error {
	if !s.drawn {
		return s.finish(w)
	}
	if err := moveDisplayCursor(w, s.cursorRow, s.cursorCol, s.endRow, s.endCol); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\r\x1b[2K%s%s%s%s\n", prefix, s.prompt, string(s.displayRunes()), suffix); err != nil {
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
