package ui

import (
	"errors"
	"io"
	"sync"
)

type outputStream uint8

const (
	outputStdout outputStream = iota
	outputStderr
)

// OutputCoordinator is the single owner of physical stdout/stderr writes. It
// serializes transient status rows, raw prompt redraws, asynchronous logs, and
// scrolling delegate activity without changing which logical stream receives
// any content.
type OutputCoordinator struct {
	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer

	stdoutWriter *coordinatedWriter
	stderrWriter *coordinatedWriter

	prompt      *promptLineEditor
	status      []byte
	statusDrawn bool
}

type coordinatedWriter struct {
	output *OutputCoordinator
	stream outputStream
}

func NewOutputCoordinator(stdout, stderr io.Writer) *OutputCoordinator {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	output := &OutputCoordinator{stdout: stdout, stderr: stderr}
	output.stdoutWriter = &coordinatedWriter{output: output, stream: outputStdout}
	output.stderrWriter = &coordinatedWriter{output: output, stream: outputStderr}
	return output
}

func (o *OutputCoordinator) Stdout() io.Writer {
	if o == nil {
		return io.Discard
	}
	return o.stdoutWriter
}

func (o *OutputCoordinator) Stderr() io.Writer {
	if o == nil {
		return io.Discard
	}
	return o.stderrWriter
}

func (w *coordinatedWriter) Write(p []byte) (int, error) {
	if w == nil || w.output == nil {
		return len(p), nil
	}
	return w.output.write(w.stream, p)
}

func (o *OutputCoordinator) write(stream outputStream, p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if stream == outputStdout {
		clearErr := o.eraseStatusLocked()
		n, err := o.stdout.Write(p)
		repaintErr := o.repaintStatusLocked()
		return n, errors.Join(clearErr, err, repaintErr)
	}
	if o.prompt != nil && o.prompt.activePrompt != nil && o.prompt.activePrompt.drawn {
		return o.prompt.writeBackgroundRaw(o.stderr, p)
	}
	clearErr := o.eraseStatusLocked()
	n, err := o.stderr.Write(p)
	repaintErr := o.repaintStatusLocked()
	return n, errors.Join(clearErr, err, repaintErr)
}

// SetStatus replaces the transient status row. The supplied bytes are already
// fully formatted, including cursor placement when needed.
func (o *OutputCoordinator) SetStatus(status []byte) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status = append(o.status[:0], status...)
	if len(o.status) == 0 {
		_ = o.eraseStatusLocked()
		return
	}
	if o.prompt != nil && o.prompt.activePrompt != nil && o.prompt.activePrompt.drawn {
		_ = o.eraseStatusLocked()
		return
	}
	_, _ = o.stderr.Write(o.status)
	o.statusDrawn = true
}

func (o *OutputCoordinator) ClearStatus() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.eraseStatusLocked()
	o.status = nil
}

// WriteActivity atomically inserts scrolling delegate lines above any active
// status row or raw prompt and restores the transient display afterward.
func (o *OutputCoordinator) WriteActivity(p []byte) error {
	if o == nil || len(p) == 0 {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.prompt != nil && o.prompt.activePrompt != nil && o.prompt.activePrompt.drawn {
		_, err := o.prompt.writeBackgroundRaw(o.stderr, p)
		return err
	}
	clearErr := o.eraseStatusLocked()
	_, writeErr := o.stderr.Write(p)
	repaintErr := o.repaintStatusLocked()
	return errors.Join(clearErr, writeErr, repaintErr)
}

func (o *OutputCoordinator) eraseStatusLocked() error {
	if !o.statusDrawn {
		return nil
	}
	_, err := io.WriteString(o.stderr, "\r\x1b[2K")
	o.statusDrawn = false
	return err
}

func (o *OutputCoordinator) repaintStatusLocked() error {
	if len(o.status) == 0 || o.statusDrawn ||
		(o.prompt != nil && o.prompt.activePrompt != nil && o.prompt.activePrompt.drawn) {
		return nil
	}
	_, err := o.stderr.Write(o.status)
	if err == nil {
		o.statusDrawn = true
	}
	return err
}

func (o *OutputCoordinator) setPromptEditor(editor *promptLineEditor) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if editor != nil {
		_ = o.eraseStatusLocked()
	}
	o.prompt = editor
	if editor == nil {
		_ = o.repaintStatusLocked()
	}
}

func withPromptOutput(editor *promptLineEditor, fn func(io.Writer) error) error {
	if editor == nil || fn == nil {
		return nil
	}
	if writer, ok := editor.w.(*coordinatedWriter); ok && writer.output != nil {
		output := writer.output
		output.mu.Lock()
		defer output.mu.Unlock()
		raw := output.stderr
		if writer.stream == outputStdout {
			raw = output.stdout
		}
		err := fn(raw)
		if editor.activePrompt == nil {
			err = errors.Join(err, output.repaintStatusLocked())
		}
		return err
	}
	return fn(editor.w)
}

func outputCoordinatorFromWriter(w io.Writer) *OutputCoordinator {
	writer, ok := w.(*coordinatedWriter)
	if !ok {
		return nil
	}
	return writer.output
}

// unwrapOutputWriter returns the physical writer for terminal capability
// checks. Writes must still go through the coordinated wrapper.
func unwrapOutputWriter(w io.Writer) io.Writer {
	if writer, ok := w.(*coordinatedWriter); ok && writer.output != nil {
		if writer.stream == outputStdout {
			return writer.output.stdout
		}
		return writer.output.stderr
	}
	return w
}
