package ui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"harness/internal/session"
)

const editorDelimiterPrefix = "--- HARNESS EDIT "

var errEditorDelimiterMissing = errors.New("editor delimiter missing")

func (app *App) editPrompt(draft string) (string, bool) {
	lastOutput, err := session.LatestTurnOutput(app.SessionPath)
	if err != nil {
		fmt.Fprintf(app.Errw, "[edit failed: %v]\n", err)
		return "", false
	}

	delimiter := newEditorDelimiter()
	path, err := writeEditorTemp(lastOutput, delimiter, draft)
	if err != nil {
		fmt.Fprintf(app.Errw, "[edit failed: %v]\n", err)
		return "", false
	}

	open := app.OpenEditor
	if open == nil {
		open = defaultOpenEditor
	}
	if app.BeforeEditor != nil {
		app.BeforeEditor()
	}
	err = open(path)
	if app.AfterEditor != nil {
		app.AfterEditor()
	}
	if err != nil {
		fmt.Fprintf(app.Errw, "[edit failed: %v; file kept at %s]\n", err, path)
		return "", false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(app.Errw, "[edit failed: %v; file kept at %s]\n", err, path)
		return "", false
	}
	prompt, err := parseEditedPrompt(string(data), delimiter)
	if err != nil {
		fmt.Fprintf(app.Errw, "[edit failed: %v; file kept at %s]\n", err, path)
		return "", false
	}
	_ = os.Remove(path)
	if strings.TrimSpace(prompt) == "" {
		return "", false
	}
	return prompt, true
}

func writeEditorTemp(lastOutput, delimiter, draft string) (string, error) {
	f, err := os.CreateTemp("", "harness-prompt-*.md")
	if err != nil {
		return "", err
	}
	path := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(path)
		}
	}()
	defer f.Close()

	if lastOutput != "" {
		if _, err := f.WriteString(strings.TrimRight(lastOutput, "\n") + "\n\n"); err != nil {
			return "", err
		}
	}
	if _, err := f.WriteString(delimiter + "\n" + draft); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func newEditorDelimiter() string {
	return editorDelimiterPrefix + newEditorNonce() + ": add your request below ---"
}

func newEditorNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func parseEditedPrompt(content, delimiter string) (string, error) {
	idx := strings.Index(content, delimiter)
	if idx < 0 {
		return "", errEditorDelimiterMissing
	}
	prompt := content[idx+len(delimiter):]
	switch {
	case strings.HasPrefix(prompt, "\r\n"):
		prompt = prompt[2:]
	case strings.HasPrefix(prompt, "\n") || strings.HasPrefix(prompt, "\r"):
		prompt = prompt[1:]
	}
	return strings.TrimRight(prompt, "\r\n"), nil
}

func defaultOpenEditor(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/tty: %w", err)
	}
	defer tty.Close()

	cmd := exec.Command("sh", "-c", "exec "+editor+" \"$1\"", "harness-editor", path) // nosemgrep: dangerous-exec-command
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	return cmd.Run()
}
