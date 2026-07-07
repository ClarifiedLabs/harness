package ui

import (
	"io"
	"sync"
)

// PromptRedrawWriter writes log output to the underlying stream. While the raw
// REPL line editor is active, it asks the editor to temporarily clear and then
// redraw the current prompt around each write so asynchronous logs appear above
// the prompt instead of after it on the same terminal row.
type PromptRedrawWriter struct {
	w io.Writer

	mu     sync.Mutex
	editor *promptLineEditor
}

func NewPromptRedrawWriter(w io.Writer) *PromptRedrawWriter {
	if w == nil {
		w = io.Discard
	}
	return &PromptRedrawWriter{w: w}
}

func (w *PromptRedrawWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	editor := w.editor
	base := w.w
	w.mu.Unlock()
	if editor != nil {
		return editor.writeBackground(p)
	}
	return base.Write(p)
}

func (w *PromptRedrawWriter) setPromptEditor(editor *promptLineEditor) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.editor = editor
	w.mu.Unlock()
}
