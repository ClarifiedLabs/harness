package main

import (
	"bytes"
	"reflect"
	"testing"

	"harness/internal/config"
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

func TestSessionResumeFlagsTrackRootFlagsExceptResume(t *testing.T) {
	want := make([]any, 0)
	for _, flag := range config.CLIFlags() {
		if flag.ID != "resume" {
			want = append(want, flag)
		}
	}
	command, ok := commandCatalog(environment{getenv: func(string) string { return "" }}).Lookup("session.resume")
	if !ok {
		t.Fatal("session.resume command is missing")
	}
	got := make([]any, len(command.Flags))
	for i, flag := range command.Flags {
		got[i] = flag
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session.resume flags drifted from root flags except resume\n got: %#v\nwant: %#v", got, want)
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
