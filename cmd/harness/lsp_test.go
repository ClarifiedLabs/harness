package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/tools"
)

func TestLSPSystemHint(t *testing.T) {
	got := lspSystemHint([]string{"go", "rust"})
	if !strings.HasPrefix(got, "lsp_* server commands found for: go, rust. Servers initialize lazily.") {
		t.Errorf("hint = %q, want prefix listing languages", got)
	}
	if !strings.Contains(got, "Prefer lsp_* over text search") {
		t.Errorf("hint = %q, want guidance to prefer lsp_* over text search", got)
	}
	if languages := lspSystemHint(nil); languages != "lsp_* tools enabled but no language server on PATH." {
		t.Errorf("empty hint = %q", languages)
	}
}

func TestMutationDiagnosticsToolAppendsFeedbackAfterSuccessfulWrite(t *testing.T) {
	base, ok := tools.Default().Lookup("write")
	if !ok {
		t.Fatal("write tool missing")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	var gotPaths []string
	wrapped := &mutationDiagnosticsTool{
		base: base,
		diagnose: func(_ context.Context, paths []string) string {
			gotPaths = append([]string(nil), paths...)
			return "error main.go:1:1  undefined: x"
		},
	}
	input, _ := json.Marshal(map[string]any{"path": path, "content": "package main\n"})
	result, err := wrapped.RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotPaths) != 1 || gotPaths[0] != path {
		t.Fatalf("diagnostic paths = %v", gotPaths)
	}
	if !strings.Contains(result.Text, "created "+path) || !strings.Contains(result.Text, "LSP diagnostics after mutation") || !strings.Contains(result.Text, "undefined: x") {
		t.Fatalf("wrapped result = %q", result.Text)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "package main\n" {
		t.Fatalf("written file = %q, %v", content, err)
	}
	if _, ok := any(wrapped).(tools.FileMutationReporter); !ok {
		t.Fatal("wrapper lost mutation reporting")
	}
	if _, ok := any(wrapped).(tools.InputTrimmer); !ok {
		t.Fatal("wrapper lost retention trimming")
	}
}

func TestMutationDiagnosticsToolSkipsFeedbackAfterFailedWrite(t *testing.T) {
	base, _ := tools.Default().Lookup("write")
	called := false
	wrapped := &mutationDiagnosticsTool{base: base, diagnose: func(context.Context, []string) string {
		called = true
		return "should not run"
	}}
	input, _ := json.Marshal(map[string]any{"path": t.TempDir(), "content": "x"})
	if _, err := wrapped.RunResult(context.Background(), input); err == nil {
		t.Fatal("write to directory unexpectedly succeeded")
	}
	if called {
		t.Fatal("diagnostics ran after failed mutation")
	}
}
