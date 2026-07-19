package main

import "testing"

func TestLSPSystemHint(t *testing.T) {
	if got := lspSystemHint([]string{"go", "rust"}); got != "LSP tools are available for: go, rust." {
		t.Errorf("hint = %q", got)
	}
	if got := lspSystemHint(nil); got != "LSP tools are registered, but no configured language-server binary is on PATH." {
		t.Errorf("empty hint = %q", got)
	}
}
