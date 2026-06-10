package ui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func TestParseEditedPromptReturnsOnlyTextAfterDelimiter(t *testing.T) {
	delimiter := "--- HARNESS EDIT test: add your request below ---"
	content := "assistant output\n\n" + delimiter + "\nnew request\nsecond line\n"

	got, err := parseEditedPrompt(content, delimiter)
	if err != nil {
		t.Fatalf("parseEditedPrompt: %v", err)
	}
	if got != "new request\nsecond line" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestParseEditedPromptRejectsMissingDelimiter(t *testing.T) {
	_, err := parseEditedPrompt("new request", "--- HARNESS EDIT x ---")
	if !errors.Is(err, errEditorDelimiterMissing) {
		t.Fatalf("error = %v, want delimiter missing", err)
	}
}

func TestREPLEditPreloadsLatestVisibleTurnAndSendsEditedPrompt(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("first answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("second answer")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 4},
		},
	)
	app := newTestApp(t, &out, &errw, fp)

	var editorInitial string
	app.OpenEditor = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		editorInitial = string(data)
		delimiter := editorDelimiterFromContent(t, editorInitial)
		updated := editorInitial[:strings.Index(editorInitial, delimiter)+len(delimiter)] +
			"\nfollow up\nsecond line\n"
		return os.WriteFile(path, []byte(updated), 0o600)
	}

	in := strings.NewReader("first prompt\n/edit\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(editorInitial, "first answer") || !strings.Contains(editorInitial, "[turn:") {
		t.Fatalf("editor preload should include visible last-turn output, got %q", editorInitial)
	}
	if strings.Contains(editorInitial, "first prompt") {
		t.Fatalf("editor preload should exclude the user prompt, got %q", editorInitial)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	msgs := app.Agent.Transcript()
	if got := msgs[2].Content[0].Text; got != "follow up\nsecond line" {
		t.Fatalf("edited prompt = %q", got)
	}
}

func TestREPLEditEmptyPromptDoesNotRunTurn(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.OpenEditor = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		delimiter := editorDelimiterFromContent(t, string(data))
		return os.WriteFile(path, []byte(delimiter+"\n\n"), 0o600)
	}

	in := strings.NewReader("/edit\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("empty edit should not invoke provider, got %d requests", len(fp.Requests))
	}
}

func TestREPLCtrlGOpensEditorWithDraft(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)

	var editorInitial string
	app.OpenEditor = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		editorInitial = string(data)
		delimiter := editorDelimiterFromContent(t, editorInitial)
		updated := editorInitial[:strings.Index(editorInitial, delimiter)+len(delimiter)] + "\nfrom ctrl-g\n"
		return os.WriteFile(path, []byte(updated), 0o600)
	}

	in := strings.NewReader("draft before editor\a/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(editorInitial, "\ndraft before editor") {
		t.Fatalf("Ctrl-G draft was not preloaded after delimiter: %q", editorInitial)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != "from ctrl-g" {
		t.Fatalf("Ctrl-G edited prompt = %q", got)
	}
}

func TestREPLCtrlGDisplaysEditedPromptBeforeModelStatus(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.OpenEditor = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		delimiter := editorDelimiterFromContent(t, string(data))
		return os.WriteFile(path, []byte(delimiter+"\nfrom editor\nsecond line\n"), 0o600)
	}

	in := strings.NewReader("\a/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}

	got := errw.String()
	if strings.Contains(got, "\a") || strings.Contains(got, "^G") {
		t.Fatalf("Ctrl-G should not be replayed to the REPL view, errw=%q", got)
	}
	want := "> from editor\nsecond line\n[model: turn 1 waiting]"
	if !strings.Contains(got, want) {
		t.Fatalf("edited prompt should be replayed before model status; missing %q in %q", want, got)
	}
}

func TestREPLEditedSlashTextIsPromptNotCommand(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("ok")},
		Stop:   llm.StopEndTurn,
	})
	app := newTestApp(t, &out, &errw, fp)
	app.OpenEditor = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		delimiter := editorDelimiterFromContent(t, string(data))
		return os.WriteFile(path, []byte(delimiter+"\n/help\n"), 0o600)
	}

	in := strings.NewReader("/edit\n/exit\n")
	if code := Run(in, app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("edited slash text should be sent as a prompt, got %d requests", len(fp.Requests))
	}
	if got := app.Agent.Transcript()[0].Content[0].Text; got != "/help" {
		t.Fatalf("edited slash prompt = %q", got)
	}
}

func editorDelimiterFromContent(t *testing.T, content string) string {
	t.Helper()
	start := strings.Index(content, editorDelimiterPrefix)
	if start < 0 {
		t.Fatalf("content missing delimiter: %q", content)
	}
	end := strings.IndexByte(content[start:], '\n')
	if end < 0 {
		return content[start:]
	}
	return content[start : start+end]
}
