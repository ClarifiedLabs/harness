package ui

import (
	"io"
	"unicode"
)

type viMode int

const (
	viModeInsert viMode = iota
	viModeNormal
)

type viOperator int

const (
	viOpNone viOperator = iota
	viOpDelete
	viOpChange
	viOpYank
)

type viLineState struct {
	mode          viMode
	pending       viOperator
	count         int
	operatorCount int
}

type viEditResult struct {
	input  replInput
	ok     bool
	done   bool
	redraw bool
}

const viMaxCount = 10000

func (v *viLineState) enterNormal(s *lineEditState) {
	v.mode = viModeNormal
	v.resetCommand()
	if s.cursor > 0 {
		s.cursor--
	}
	s.viClampNormalCursor()
}

func (v *viLineState) enterInsert() {
	v.mode = viModeInsert
	v.resetCommand()
}

func (v *viLineState) resetCommand() {
	v.pending = viOpNone
	v.count = 0
	v.operatorCount = 0
}

func (v *viLineState) appendCount(r rune) bool {
	if r < '0' || r > '9' {
		return false
	}
	if r == '0' && v.count == 0 {
		return false
	}
	digit := int(r - '0')
	if v.count > (viMaxCount-digit)/10 {
		v.count = viMaxCount
		return true
	}
	v.count = v.count*10 + digit
	return true
}

func (v *viLineState) takeCount() int {
	if v.count == 0 {
		return 1
	}
	count := v.count
	v.count = 0
	return count
}

func (v *viLineState) startOperator(op viOperator, count int) {
	if count <= 0 {
		count = 1
	}
	v.pending = op
	v.operatorCount = count
	v.count = 0
}

func (v *viLineState) takeOperatorMotionCount() int {
	motionCount := v.takeCount()
	operatorCount := v.operatorCount
	if operatorCount == 0 {
		operatorCount = 1
	}
	v.operatorCount = 0
	if motionCount > viMaxCount/operatorCount {
		return viMaxCount
	}
	return motionCount * operatorCount
}

// refreshViPrompt re-renders the prompt for the current vi mode when a
// viPrompt callback is wired (a {vimode} placeholder is in use) and the editor
// is in vi mode, so the label flips live at the mode-transition chokepoints.
// It is a no-op otherwise: the callback is nil in emacs mode / templates without
// a vimode variant / tests, and emacs mode has no vi mode to reflect (the
// prompt's empty {vimode} label is already rendered by renderPrompt at idle).
func (e *promptLineEditor) refreshViPrompt(v *viLineState, s *lineEditState) {
	if e.viPrompt == nil || e.editMode != promptEditModeVi {
		return
	}
	s.prompt = e.viPrompt(v.mode)
}

// viEnterNormal / viEnterInsert are the routed mode transitions: they perform
// the underlying state change then refresh the prompt, so every mode switch
// funnels through refreshViPrompt regardless of which command triggered it.
func (e *promptLineEditor) viEnterNormal(v *viLineState, s *lineEditState) {
	v.enterNormal(s)
	e.refreshViPrompt(v, s)
}

func (e *promptLineEditor) viEnterInsert(v *viLineState, s *lineEditState) {
	v.enterInsert()
	e.refreshViPrompt(v, s)
}

func (e *promptLineEditor) handleViNormalInput(v *viLineState, s *lineEditState, h *lineEditHistory, prompt string, r rune) (viEditResult, error) {
	e.clearShiftEnterPending()
	switch r {
	case '\r', '\n':
		return e.viSubmit(s)
	case ctrlC:
		return viEditResult{input: replInput{interrupt: true}, ok: true, done: true}, nil
	case ctrlD:
		if len(s.buf) == 0 {
			return viEditResult{ok: false, done: true}, nil
		}
		return viEditResult{redraw: true}, nil
	case rune(lineTermEdit):
		return e.viEdit(s)
	case '\b', del:
		e.markManualEdit(s)
		v.resetCommand()
		s.viLeft()
		return viEditResult{redraw: true}, nil
	case rune(lineTermEscape):
		action, text, err := e.readEscape()
		if err != nil {
			if err == io.EOF {
				return viEditResult{ok: false, done: true}, nil
			}
			return viEditResult{}, err
		}
		return e.handleViNormalAction(v, s, h, prompt, action, text)
	default:
		if r == '\t' || unicode.IsPrint(r) {
			return e.handleViNormalText(v, s, h, string(r)), nil
		}
		return viEditResult{redraw: true}, nil
	}
}

func (e *promptLineEditor) handleViNormalAction(v *viLineState, s *lineEditState, h *lineEditHistory, prompt string, action lineEditAction, text string) (viEditResult, error) {
	switch action {
	case lineEditEscape, lineEditIgnore:
		v.resetCommand()
		return viEditResult{redraw: true}, nil
	case lineEditShiftModifier:
		return viEditResult{redraw: true}, nil
	case lineEditSubmit:
		return e.viSubmit(s)
	case lineEditEdit:
		return e.viEdit(s)
	case lineEditEOF:
		if len(s.buf) == 0 {
			return viEditResult{ok: false, done: true}, nil
		}
		return viEditResult{redraw: true}, nil
	case lineEditInterrupt:
		return viEditResult{input: replInput{interrupt: true}, ok: true, done: true}, nil
	case lineEditHome:
		return e.handleViNormalText(v, s, h, "0"), nil
	case lineEditEnd:
		return e.handleViNormalText(v, s, h, "$"), nil
	case lineEditLeft:
		return e.handleViNormalText(v, s, h, "h"), nil
	case lineEditRight:
		return e.handleViNormalText(v, s, h, "l"), nil
	case lineEditBackspace:
		e.markManualEdit(s)
		v.resetCommand()
		s.viLeft()
		return viEditResult{redraw: true}, nil
	case lineEditDelete:
		return e.handleViNormalText(v, s, h, "x"), nil
	case lineEditHistoryPrev:
		e.markManualEdit(s)
		count := v.takeCount()
		v.resetCommand()
		for range count {
			h.prev(s)
		}
		s.viClampNormalCursor()
		return viEditResult{redraw: true}, nil
	case lineEditHistoryNext:
		e.markManualEdit(s)
		count := v.takeCount()
		v.resetCommand()
		for range count {
			h.next(s)
		}
		s.viClampNormalCursor()
		return viEditResult{redraw: true}, nil
	case lineEditInsertNewline:
		e.markManualEdit(s)
		v.resetCommand()
		e.viEnterInsert(v, s)
		if len(s.buf) > 0 && s.cursor < len(s.buf) {
			s.cursor++
		}
		s.insert('\n')
		return viEditResult{redraw: true}, nil
	case lineEditInsertText:
		return e.handleViNormalText(v, s, h, text), nil
	case lineEditPaste:
		v.resetCommand()
		if len(s.buf) == 0 {
			s.setPasteSummary(text)
			e.purePaste = true
			e.viEnterInsert(v, s)
			return viEditResult{redraw: true}, nil
		}
		e.viPasteText(s, []rune(text), false)
		return viEditResult{redraw: true}, nil
	default:
		v.resetCommand()
		return viEditResult{redraw: true}, nil
	}
}

func (e *promptLineEditor) handleViNormalText(v *viLineState, s *lineEditState, h *lineEditHistory, text string) viEditResult {
	// A manual vi command (motion, delete, insert, paste-from-yank, or history
	// recall) after a pure paste clears the literal flag, honoring the "any manual
	// keystroke clears the mark" rule. Submit, edit, Esc, and the empty-buffer
	// paste-fill branch in handleViNormalAction do not route here, so the pure-paste
	// flag survives Esc into normal mode and is carried by viSubmit.
	e.markManualEdit(s)
	for _, r := range text {
		if v.appendCount(r) {
			continue
		}
		if v.pending != viOpNone {
			e.applyViOperator(v, s, r)
			continue
		}
		e.applyViCommand(v, s, h, r)
		if v.mode == viModeInsert {
			break
		}
	}
	return viEditResult{redraw: true}
}

func (e *promptLineEditor) applyViCommand(v *viLineState, s *lineEditState, h *lineEditHistory, r rune) {
	count := v.takeCount()
	switch r {
	case 'i':
		e.viEnterInsert(v, s)
	case 'a':
		if len(s.buf) > 0 && s.cursor < len(s.buf) {
			s.cursor++
		}
		e.viEnterInsert(v, s)
	case 'I':
		s.cursor = 0
		e.viEnterInsert(v, s)
	case 'A':
		s.cursor = len(s.buf)
		e.viEnterInsert(v, s)
	case 'h':
		for range count {
			s.viLeft()
		}
	case 'l', ' ':
		for range count {
			s.viRight()
		}
	case '0':
		s.cursor = 0
	case '^':
		s.cursor = viFirstNonBlank(s.buf)
	case '$':
		s.viEnd()
	case 'w':
		s.cursor = viRepeatNextWordStart(s.buf, s.cursor, count, false)
		s.viClampNormalCursor()
	case 'W':
		s.cursor = viRepeatNextWordStart(s.buf, s.cursor, count, true)
		s.viClampNormalCursor()
	case 'b':
		s.cursor = viRepeatPrevWordStart(s.buf, s.cursor, count, false)
		s.viClampNormalCursor()
	case 'B':
		s.cursor = viRepeatPrevWordStart(s.buf, s.cursor, count, true)
		s.viClampNormalCursor()
	case 'e':
		s.cursor = viRepeatWordEnd(s.buf, s.cursor, count, false)
		s.viClampNormalCursor()
	case 'E':
		s.cursor = viRepeatWordEnd(s.buf, s.cursor, count, true)
		s.viClampNormalCursor()
	case 'x':
		e.viDeleteChars(s, count)
	case 'X':
		e.viDeleteBeforeCount(s, count)
	case 'd':
		v.startOperator(viOpDelete, count)
	case 'c':
		v.startOperator(viOpChange, count)
	case 'y':
		v.startOperator(viOpYank, count)
	case 'D':
		e.viApplyRange(s, viOpDelete, s.cursor, len(s.buf))
	case 'C':
		e.viApplyRange(s, viOpChange, s.cursor, len(s.buf))
		e.viEnterInsert(v, s)
	case 'Y':
		e.viSetYank(s.buf)
	case 's':
		e.viApplyRange(s, viOpChange, s.cursor, s.cursor+count)
		e.viEnterInsert(v, s)
	case 'S':
		e.viApplyLine(s, viOpChange)
		e.viEnterInsert(v, s)
	case 'p':
		for range count {
			e.viPasteText(s, e.viYank, false)
		}
	case 'P':
		for range count {
			e.viPasteText(s, e.viYank, true)
		}
	case 'k':
		for range count {
			h.prev(s)
		}
		s.viClampNormalCursor()
	case 'j':
		for range count {
			h.next(s)
		}
		s.viClampNormalCursor()
	default:
		v.resetCommand()
	}
}

func (e *promptLineEditor) applyViOperator(v *viLineState, s *lineEditState, motion rune) {
	op := v.pending
	count := v.takeOperatorMotionCount()
	v.pending = viOpNone

	if viOperatorRune(op) == motion {
		v.resetCommand()
		e.viApplyLine(s, op)
		if op == viOpChange {
			e.viEnterInsert(v, s)
		}
		return
	}

	start, end, ok := viOperatorRange(s, motion, op, count)
	if !ok {
		v.resetCommand()
		return
	}
	e.viApplyRange(s, op, start, end)
	if op == viOpChange {
		e.viEnterInsert(v, s)
	}
}

func viOperatorRune(op viOperator) rune {
	switch op {
	case viOpDelete:
		return 'd'
	case viOpChange:
		return 'c'
	case viOpYank:
		return 'y'
	default:
		return 0
	}
}

func (e *promptLineEditor) viApplyLine(s *lineEditState, op viOperator) {
	e.viApplyRange(s, op, 0, len(s.buf))
}

func (e *promptLineEditor) viApplyRange(s *lineEditState, op viOperator, start, end int) {
	if start > end {
		start, end = end, start
	}
	start, end = viClampRange(start, end, len(s.buf))
	if start == end && op != viOpChange {
		return
	}
	switch op {
	case viOpDelete:
		e.viSetYank(s.viDeleteRange(start, end))
		s.viClampNormalCursor()
	case viOpChange:
		e.viSetYank(s.viDeleteRange(start, end))
		s.cursor = start
	case viOpYank:
		e.viSetYank(s.buf[start:end])
		s.viClampNormalCursor()
	}
}

func (e *promptLineEditor) viDeleteChar(s *lineEditState) {
	e.viDeleteChars(s, 1)
}

func (e *promptLineEditor) viDeleteChars(s *lineEditState, count int) {
	if len(s.buf) == 0 || count <= 0 {
		return
	}
	e.viApplyRange(s, viOpDelete, s.cursor, s.cursor+count)
}

func (e *promptLineEditor) viDeleteBefore(s *lineEditState) {
	e.viDeleteBeforeCount(s, 1)
}

func (e *promptLineEditor) viDeleteBeforeCount(s *lineEditState, count int) {
	if len(s.buf) == 0 || s.cursor == 0 || count <= 0 {
		return
	}
	start := s.cursor - count
	if start < 0 {
		start = 0
	}
	e.viSetYank(s.viDeleteRange(start, s.cursor))
	s.cursor = start
	s.viClampNormalCursor()
}

func (e *promptLineEditor) viSetYank(text []rune) {
	if len(text) == 0 {
		return
	}
	e.viYank = append(e.viYank[:0], text...)
}

func (e *promptLineEditor) viPasteText(s *lineEditState, text []rune, before bool) {
	if len(text) == 0 {
		return
	}
	idx := s.cursor
	if !before && len(s.buf) > 0 {
		idx++
	}
	s.viInsertRunesAt(idx, text)
	if before {
		s.cursor = idx
	} else {
		s.cursor = idx + len(text) - 1
	}
	s.viClampNormalCursor()
}

func (e *promptLineEditor) viSubmit(s *lineEditState) (viEditResult, error) {
	if err := s.finish(e.w); err != nil {
		return viEditResult{}, err
	}
	e.addHistory(string(s.buf))
	// A pure paste that filled the buffer submits literally even when entered
	// from vi normal mode (Esc then Enter), matching every emacs-mode submit
	// path (raw CR, escape-submit, EOF-with-buffer). The pure-paste flag survives
	// Esc into normal mode; it is cleared only by a manual edit/motion keystroke
	// (handleViNormalText/handleViNormalAction call markManualEdit), never by the
	// mode switch itself. See the "any manual keystroke clears the mark" rule.
	return viEditResult{input: replInput{text: string(s.buf), pasted: e.purePaste}, ok: true, done: true}, nil
}

func (e *promptLineEditor) viEdit(s *lineEditState) (viEditResult, error) {
	if err := s.finish(e.w); err != nil {
		return viEditResult{}, err
	}
	e.addHistory(string(s.buf))
	return viEditResult{input: replInput{text: string(s.buf), edit: true}, ok: true, done: true}, nil
}

func viOperatorRange(s *lineEditState, motion rune, op viOperator, count int) (int, int, bool) {
	if len(s.buf) == 0 {
		return 0, 0, op == viOpChange
	}
	if count <= 0 {
		count = 1
	}
	cursor := s.cursor
	switch motion {
	case 'h':
		if cursor == 0 {
			return 0, 0, false
		}
		return cursor - count, cursor, true
	case 'l', ' ':
		return cursor, cursor + count, true
	case '0':
		return 0, cursor, true
	case '^':
		return viFirstNonBlank(s.buf), cursor, true
	case '$':
		return cursor, len(s.buf), true
	case 'b':
		target := viRepeatPrevWordStart(s.buf, cursor, count, false)
		return target, cursor, true
	case 'B':
		target := viRepeatPrevWordStart(s.buf, cursor, count, true)
		return target, cursor, true
	case 'e':
		target := viRepeatWordEnd(s.buf, cursor, count, false)
		return cursor, target + 1, true
	case 'E':
		target := viRepeatWordEnd(s.buf, cursor, count, true)
		return cursor, target + 1, true
	case 'w':
		return cursor, viForwardOperatorEnd(s.buf, cursor, op, count, false), true
	case 'W':
		return cursor, viForwardOperatorEnd(s.buf, cursor, op, count, true), true
	default:
		return 0, 0, false
	}
}

func viForwardOperatorEnd(buf []rune, cursor int, op viOperator, count int, big bool) int {
	if count <= 0 {
		count = 1
	}
	if op == viOpChange && count == 1 && cursor < len(buf) && !unicode.IsSpace(buf[cursor]) {
		return viWordEnd(buf, cursor, big) + 1
	}
	return viRepeatNextWordStart(buf, cursor, count, big)
}

func (s *lineEditState) viLeft() {
	if s.cursor > 0 {
		s.cursor--
	}
}

func (s *lineEditState) viRight() {
	if len(s.buf) == 0 {
		s.cursor = 0
		return
	}
	if s.cursor < len(s.buf)-1 {
		s.cursor++
	}
}

func (s *lineEditState) viEnd() {
	if len(s.buf) == 0 {
		s.cursor = 0
		return
	}
	s.cursor = len(s.buf) - 1
}

func (s *lineEditState) viClampNormalCursor() {
	switch {
	case len(s.buf) == 0:
		s.cursor = 0
	case s.cursor < 0:
		s.cursor = 0
	case s.cursor >= len(s.buf):
		s.cursor = len(s.buf) - 1
	}
}

func (s *lineEditState) viDeleteRange(start, end int) []rune {
	start, end = viClampRange(start, end, len(s.buf))
	if start >= end {
		return nil
	}
	deleted := append([]rune(nil), s.buf[start:end]...)
	copy(s.buf[start:], s.buf[end:])
	s.buf = s.buf[:len(s.buf)-(end-start)]
	if s.cursor > len(s.buf) {
		s.cursor = len(s.buf)
	}
	return deleted
}

func (s *lineEditState) viInsertRunesAt(index int, text []rune) {
	if len(text) == 0 {
		return
	}
	if index < 0 {
		index = 0
	}
	if index > len(s.buf) {
		index = len(s.buf)
	}
	s.buf = append(s.buf, make([]rune, len(text))...)
	copy(s.buf[index+len(text):], s.buf[index:])
	copy(s.buf[index:], text)
}

func viClampRange(start, end, length int) (int, int) {
	if start < 0 {
		start = 0
	}
	if start > length {
		start = length
	}
	if end < 0 {
		end = 0
	}
	if end > length {
		end = length
	}
	return start, end
}

func viFirstNonBlank(buf []rune) int {
	for i, r := range buf {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return 0
}

type viCharKind int

const (
	viCharSpace viCharKind = iota
	viCharWord
	viCharPunct
)

func viKind(r rune) viCharKind {
	if unicode.IsSpace(r) {
		return viCharSpace
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return viCharWord
	}
	return viCharPunct
}

func viNextWordStart(buf []rune, pos int, big bool) int {
	if len(buf) == 0 {
		return 0
	}
	if pos < 0 {
		pos = 0
	}
	if pos >= len(buf) {
		return len(buf)
	}
	i := pos
	if big {
		if !unicode.IsSpace(buf[i]) {
			for i < len(buf) && !unicode.IsSpace(buf[i]) {
				i++
			}
		}
		for i < len(buf) && unicode.IsSpace(buf[i]) {
			i++
		}
		return i
	}

	kind := viKind(buf[i])
	if kind != viCharSpace {
		for i < len(buf) && viKind(buf[i]) == kind {
			i++
		}
	}
	for i < len(buf) && viKind(buf[i]) == viCharSpace {
		i++
	}
	return i
}

func viRepeatNextWordStart(buf []rune, pos, count int, big bool) int {
	if count <= 0 {
		count = 1
	}
	for range count {
		next := viNextWordStart(buf, pos, big)
		if next == pos {
			return next
		}
		pos = next
	}
	return pos
}

func viPrevWordStart(buf []rune, pos int, big bool) int {
	if len(buf) == 0 || pos <= 0 {
		return 0
	}
	i := pos - 1
	if i >= len(buf) {
		i = len(buf) - 1
	}
	if big {
		for i > 0 && unicode.IsSpace(buf[i]) {
			i--
		}
		for i > 0 && !unicode.IsSpace(buf[i-1]) {
			i--
		}
		return i
	}

	for i > 0 && viKind(buf[i]) == viCharSpace {
		i--
	}
	kind := viKind(buf[i])
	for i > 0 && viKind(buf[i-1]) == kind {
		i--
	}
	return i
}

func viRepeatPrevWordStart(buf []rune, pos, count int, big bool) int {
	if count <= 0 {
		count = 1
	}
	for range count {
		next := viPrevWordStart(buf, pos, big)
		if next == pos {
			return next
		}
		pos = next
	}
	return pos
}

func viWordEnd(buf []rune, pos int, big bool) int {
	if len(buf) == 0 {
		return 0
	}
	if pos < 0 {
		pos = 0
	}
	if pos >= len(buf) {
		return len(buf) - 1
	}
	i := pos
	if big {
		if !unicode.IsSpace(buf[i]) {
			for i+1 < len(buf) && !unicode.IsSpace(buf[i+1]) {
				i++
			}
			if i > pos {
				return i
			}
			i++
		}
		for i < len(buf) && unicode.IsSpace(buf[i]) {
			i++
		}
		if i >= len(buf) {
			return len(buf) - 1
		}
		for i+1 < len(buf) && !unicode.IsSpace(buf[i+1]) {
			i++
		}
		return i
	}

	kind := viKind(buf[i])
	if kind != viCharSpace {
		for i+1 < len(buf) && viKind(buf[i+1]) == kind {
			i++
		}
		if i > pos {
			return i
		}
		i++
	}
	for i < len(buf) && viKind(buf[i]) == viCharSpace {
		i++
	}
	if i >= len(buf) {
		return len(buf) - 1
	}
	kind = viKind(buf[i])
	for i+1 < len(buf) && viKind(buf[i+1]) == kind {
		i++
	}
	return i
}

func viRepeatWordEnd(buf []rune, pos, count int, big bool) int {
	if count <= 0 {
		count = 1
	}
	for range count {
		next := viWordEnd(buf, pos, big)
		if next == pos {
			return next
		}
		pos = next
	}
	return pos
}
