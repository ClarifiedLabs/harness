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
	mode    viMode
	pending viOperator
}

type viEditResult struct {
	input  replInput
	ok     bool
	done   bool
	redraw bool
}

func (v *viLineState) enterNormal(s *lineEditState) {
	v.mode = viModeNormal
	v.pending = viOpNone
	if s.cursor > 0 {
		s.cursor--
	}
	s.viClampNormalCursor()
}

func (v *viLineState) enterInsert() {
	v.mode = viModeInsert
	v.pending = viOpNone
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
		v.pending = viOpNone
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
	case lineEditEscape, lineEditShiftModifier, lineEditIgnore:
		v.pending = viOpNone
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
		v.pending = viOpNone
		s.viLeft()
		return viEditResult{redraw: true}, nil
	case lineEditDelete:
		return e.handleViNormalText(v, s, h, "x"), nil
	case lineEditHistoryPrev:
		v.pending = viOpNone
		h.prev(s)
		s.viClampNormalCursor()
		return viEditResult{redraw: true}, nil
	case lineEditHistoryNext:
		v.pending = viOpNone
		h.next(s)
		s.viClampNormalCursor()
		return viEditResult{redraw: true}, nil
	case lineEditInsertNewline:
		v.enterInsert()
		if len(s.buf) > 0 && s.cursor < len(s.buf) {
			s.cursor++
		}
		s.insert('\n')
		return viEditResult{redraw: true}, nil
	case lineEditInsertText:
		return e.handleViNormalText(v, s, h, text), nil
	case lineEditPaste:
		if len(s.buf) == 0 {
			s.setText(text)
			if err := s.redraw(e.w, e.terminalColumns()); err != nil {
				return viEditResult{}, err
			}
			if err := s.finish(e.w); err != nil {
				return viEditResult{}, err
			}
			e.addHistory(text)
			return viEditResult{input: replInput{text: text, pasted: true}, ok: true, done: true}, nil
		}
		e.viPasteText(s, []rune(text), false)
		return viEditResult{redraw: true}, nil
	default:
		v.pending = viOpNone
		return viEditResult{redraw: true}, nil
	}
}

func (e *promptLineEditor) handleViNormalText(v *viLineState, s *lineEditState, h *lineEditHistory, text string) viEditResult {
	for _, r := range text {
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
	switch r {
	case 'i':
		v.enterInsert()
	case 'a':
		if len(s.buf) > 0 && s.cursor < len(s.buf) {
			s.cursor++
		}
		v.enterInsert()
	case 'I':
		s.cursor = 0
		v.enterInsert()
	case 'A':
		s.cursor = len(s.buf)
		v.enterInsert()
	case 'h':
		s.viLeft()
	case 'l', ' ':
		s.viRight()
	case '0':
		s.cursor = 0
	case '^':
		s.cursor = viFirstNonBlank(s.buf)
	case '$':
		s.viEnd()
	case 'w':
		s.cursor = viNextWordStart(s.buf, s.cursor, false)
		s.viClampNormalCursor()
	case 'W':
		s.cursor = viNextWordStart(s.buf, s.cursor, true)
		s.viClampNormalCursor()
	case 'b':
		s.cursor = viPrevWordStart(s.buf, s.cursor, false)
		s.viClampNormalCursor()
	case 'B':
		s.cursor = viPrevWordStart(s.buf, s.cursor, true)
		s.viClampNormalCursor()
	case 'e':
		s.cursor = viWordEnd(s.buf, s.cursor, false)
		s.viClampNormalCursor()
	case 'E':
		s.cursor = viWordEnd(s.buf, s.cursor, true)
		s.viClampNormalCursor()
	case 'x':
		e.viDeleteChar(s)
	case 'X':
		e.viDeleteBefore(s)
	case 'd':
		v.pending = viOpDelete
	case 'c':
		v.pending = viOpChange
	case 'y':
		v.pending = viOpYank
	case 'D':
		e.viApplyRange(s, viOpDelete, s.cursor, len(s.buf))
	case 'C':
		e.viApplyRange(s, viOpChange, s.cursor, len(s.buf))
		v.enterInsert()
	case 'Y':
		e.viSetYank(s.buf)
	case 's':
		e.viDeleteChar(s)
		v.enterInsert()
	case 'S':
		e.viApplyLine(s, viOpChange)
		v.enterInsert()
	case 'p':
		e.viPasteText(s, e.viYank, false)
	case 'P':
		e.viPasteText(s, e.viYank, true)
	case 'k':
		h.prev(s)
		s.viClampNormalCursor()
	case 'j':
		h.next(s)
		s.viClampNormalCursor()
	default:
	}
}

func (e *promptLineEditor) applyViOperator(v *viLineState, s *lineEditState, motion rune) {
	op := v.pending
	v.pending = viOpNone

	if viOperatorRune(op) == motion {
		e.viApplyLine(s, op)
		if op == viOpChange {
			v.enterInsert()
		}
		return
	}

	start, end, ok := viOperatorRange(s, motion, op)
	if !ok {
		return
	}
	e.viApplyRange(s, op, start, end)
	if op == viOpChange {
		v.enterInsert()
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
	if len(s.buf) == 0 {
		return
	}
	e.viSetYank(s.viDeleteRange(s.cursor, s.cursor+1))
	s.viClampNormalCursor()
}

func (e *promptLineEditor) viDeleteBefore(s *lineEditState) {
	if len(s.buf) == 0 || s.cursor == 0 {
		return
	}
	e.viSetYank(s.viDeleteRange(s.cursor-1, s.cursor))
	s.cursor--
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
	return viEditResult{input: replInput{text: string(s.buf)}, ok: true, done: true}, nil
}

func (e *promptLineEditor) viEdit(s *lineEditState) (viEditResult, error) {
	if err := s.finish(e.w); err != nil {
		return viEditResult{}, err
	}
	e.addHistory(string(s.buf))
	return viEditResult{input: replInput{text: string(s.buf), edit: true}, ok: true, done: true}, nil
}

func viOperatorRange(s *lineEditState, motion rune, op viOperator) (int, int, bool) {
	if len(s.buf) == 0 {
		return 0, 0, op == viOpChange
	}
	cursor := s.cursor
	switch motion {
	case 'h':
		if cursor == 0 {
			return 0, 0, false
		}
		return cursor - 1, cursor, true
	case 'l', ' ':
		return cursor, cursor + 1, true
	case '0':
		return 0, cursor, true
	case '^':
		return viFirstNonBlank(s.buf), cursor, true
	case '$':
		return cursor, len(s.buf), true
	case 'b':
		target := viPrevWordStart(s.buf, cursor, false)
		return target, cursor, true
	case 'B':
		target := viPrevWordStart(s.buf, cursor, true)
		return target, cursor, true
	case 'e':
		target := viWordEnd(s.buf, cursor, false)
		return cursor, target + 1, true
	case 'E':
		target := viWordEnd(s.buf, cursor, true)
		return cursor, target + 1, true
	case 'w':
		return cursor, viForwardOperatorEnd(s.buf, cursor, op, false), true
	case 'W':
		return cursor, viForwardOperatorEnd(s.buf, cursor, op, true), true
	default:
		return 0, 0, false
	}
}

func viForwardOperatorEnd(buf []rune, cursor int, op viOperator, big bool) int {
	if op == viOpChange && cursor < len(buf) && !unicode.IsSpace(buf[cursor]) {
		return viWordEnd(buf, cursor, big) + 1
	}
	return viNextWordStart(buf, cursor, big)
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
