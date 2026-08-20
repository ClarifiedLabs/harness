package main

import (
	"strings"
	"testing"
)

func TestLSPSystemHint(t *testing.T) {
	got := lspSystemHint([]string{"go", "rust"})
	if !strings.HasPrefix(got, "lsp_* available for: go, rust.") {
		t.Errorf("hint = %q, want prefix listing languages", got)
	}
	if !strings.Contains(got, "Prefer lsp_* over text search") {
		t.Errorf("hint = %q, want guidance to prefer lsp_* over text search", got)
	}
	if languages := lspSystemHint(nil); languages != "lsp_* tools enabled but no language server on PATH." {
		t.Errorf("empty hint = %q", languages)
	}
}
