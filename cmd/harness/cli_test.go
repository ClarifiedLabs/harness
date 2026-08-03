package main

import (
	"bytes"
	"testing"

	"harness/internal/ui"
)

func TestCommandCatalogHandlersStayInSync(t *testing.T) {
	catalog := commandCatalog(environment{getenv: func(string) string { return "" }})
	remaining := make(map[string]struct{}, len(commandHandlers))
	for id := range commandHandlers {
		remaining[id] = struct{}{}
	}
	for _, command := range catalog.Commands() {
		if !command.Runnable {
			continue
		}
		if commandHandlers[command.ID] == nil {
			t.Errorf("runnable command %q has no handler", command.ID)
		}
		delete(remaining, command.ID)
	}
	for id := range remaining {
		t.Errorf("handler %q has no runnable catalog command", id)
	}
}

func TestLegacyGroupHelpAliasesIgnoreTrailingArguments(t *testing.T) {
	for _, group := range []string{"config", "lsp"} {
		t.Run(group, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(environment{
				args:   []string{group, "help", "historically-ignored"},
				stdout: &stdout,
				stderr: &stderr,
				getenv: func(string) string { return "" },
			})
			if code != ui.ExitOK || stdout.Len() == 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}
