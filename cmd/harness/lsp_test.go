package main

import "testing"

func TestLSPSystemHint(t *testing.T) {
	if got := lspSystemHint([]string{"go", "rust"}); got != "lsp_* available for: go, rust. Prefer lsp_* over search for definitions, references, hover, symbols, diagnostics, and rename." {
		t.Errorf("hint = %q", got)
	}
	if got := lspSystemHint(nil); got != "lsp_* tools enabled but no language server on PATH." {
		t.Errorf("empty hint = %q", got)
	}
}
