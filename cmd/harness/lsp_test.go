package main

import "testing"

func TestLSPSystemHint(t *testing.T) {
	if got := lspSystemHint([]string{"go", "rust"}); got != "Native lsp_* code-intelligence tools, when present in this request, have configured language-server binaries available for: go, rust. Prefer them for semantic navigation, diagnostics, hierarchies, code actions, formatting, and rename when the target language supports the operation." {
		t.Errorf("hint = %q", got)
	}
	if got := lspSystemHint(nil); got != "Native lsp_* code-intelligence tools are configured, but no configured language-server binary is on PATH." {
		t.Errorf("empty hint = %q", got)
	}
}
