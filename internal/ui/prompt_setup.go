package ui

import (
	"fmt"
	"io"

	"harness/internal/replprompt"
	"harness/internal/session"
	"harness/internal/term"
)

// setupPromptReader owns terminal modes, editor hooks, and history setup.
// Scheduling stays in runWithInitialPrompt; cleanup reverses setup order.
func (app *App) setupPromptReader(in io.Reader, usePromptEditor bool, promptTemplate *replprompt.Template) (*replReader, func(), func(), func()) {
	// Restore a usable terminal before the first prompt (termios sane plus an
	// emulator soft reset), in case a prior process left it in raw, no-echo,
	// or mouse-reporting state. Targets /dev/tty directly; no-op without one.
	var restorePromptTerm func() error
	disableIdlePromptTerm := func() {
		_ = term.SetBracketedPaste(false)
		if restorePromptTerm != nil {
			_ = restorePromptTerm()
			restorePromptTerm = nil
		}
		if usePromptEditor && promptEditMode(app.PromptEditMode) == promptEditModeVi {
			_ = term.SetCursorShape(term.CursorShapeDefault)
		}
	}
	enableIdlePromptTerm := func() {
		if err := term.Reset(); err != nil {
			fmt.Fprintf(app.Errw, "[term reset: %v]\n", err)
		}
		if usePromptEditor {
			if cleanup, err := term.EnablePromptRawMode(); err == nil {
				restorePromptTerm = cleanup
			}
		} else if cleanup, err := term.EnableCtrlGLineEnd(); err == nil {
			restorePromptTerm = cleanup
		}
		_ = term.SetBracketedPaste(true)
	}
	enableIdlePromptTerm()

	prevBeforeEditor, prevAfterEditor := app.BeforeEditor, app.AfterEditor
	app.BeforeEditor = func() {
		disableIdlePromptTerm()
		if prevBeforeEditor != nil {
			prevBeforeEditor()
		}
	}
	app.AfterEditor = func() {
		if prevAfterEditor != nil {
			prevAfterEditor()
		}
		enableIdlePromptTerm()
	}

	reader := newREPLReader(in, app.Errw, usePromptEditor, app.PromptEditMode)
	if reader.editor != nil {
		reader.editor.skillNames = sortedSkillNames(app.Skills)
	}
	output := outputCoordinatorFromWriter(app.Errw)
	if output != nil && reader.editor != nil {
		output.setPromptEditor(reader.editor)
	}
	app.SetPromptEditMode = func(mode string) {
		if reader.editor != nil {
			reader.editor.setEditMode(mode)
		}
	}
	// When the prompt template uses a {vimode} placeholder, re-render the prompt
	// for the current vi mode at each mode transition (Esc/i/a/...) so the label
	// flips live during a read. The closure mirrors renderPrompt but with the
	// editor's current mode; it is nil in emacs mode and for templates without a
	// vimode variant, so behavior is unchanged there and in tests.
	if usePromptEditor && reader.editor != nil && promptTemplate.UsesViMode() {
		reader.editor.viPrompt = func(m viMode) string {
			return promptTemplate.Render(app.promptValues(promptTemplate, viModeName(m)))
		}
	}
	// Render the during-prompt typed buffer live on the status line (during-prompt
	// input). The reader calls this from its read goroutine; SetInputLine is
	// mutex-guarded so it never interleaves with the agent's renderer writes.
	if usePromptEditor && app.Renderer != nil {
		reader.onPromptInput = func(buf string, cursor int) { app.Renderer.SetInputLine(buf, cursor) }
	}
	// Load and configure REPL history persistence (bash-style HISTFILE/HISTFILESIZE/HISTSIZE).
	// The in-memory editor receives a pre-loaded slice and a callback that appends each new
	// entry to the on-disk history file. Errors are warned but never fatal.
	if usePromptEditor && reader.editor != nil && app.HistFile != "" {
		if entries, err := session.LoadHistory(app.HistFile, app.HistFileSize, app.HistSize); err != nil {
			fmt.Fprintf(app.Errw, "[history load error: %v]\n", err)
		} else {
			reader.editor.SetInitialHistory(entries)
		}
		reader.editor.onNewHistory = func(entry string) {
			if err := session.AppendHistory(app.HistFile, entry); err != nil {
				fmt.Fprintf(app.Errw, "[history save error: %v]\n", err)
			}
		}
	}
	cleanup := func() {
		if output != nil && reader.editor != nil {
			output.setPromptEditor(nil)
		}
		app.BeforeEditor = prevBeforeEditor
		app.AfterEditor = prevAfterEditor
		disableIdlePromptTerm()
	}
	return reader, enableIdlePromptTerm, disableIdlePromptTerm, cleanup
}
