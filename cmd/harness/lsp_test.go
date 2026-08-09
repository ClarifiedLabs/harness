package main

import (
	"testing"

	"harness/internal/mcptools"
)

func TestLSPSystemHint(t *testing.T) {
	if got := lspSystemHint([]string{"go", "rust"}); got != "lsp_* available for: go, rust. Prefer lsp_* over text search for definitions, references, hover, symbols, diagnostics, and rename." {
		t.Errorf("hint = %q", got)
	}
	if got := lspSystemHint(nil); got != "lsp_* tools enabled but no language server on PATH." {
		t.Errorf("empty hint = %q", got)
	}
}

func TestLSPDescriptionSuffixFitsBudget(t *testing.T) {
	if len(lspPreferSuffix) > 26 {
		t.Errorf("lspPreferSuffix = %d bytes, budget 26: %q", len(lspPreferSuffix), lspPreferSuffix)
	}
	base := "Search contents by RE2 regex; escape punctuation."
	if got := len(base) + len(lspPreferSuffix); got > 80 {
		t.Errorf("suffixed search description = %d bytes, budget 80: base %q + suffix %q", got, base, lspPreferSuffix)
	}
}

func TestLSPDescriptionSuffix(t *testing.T) {
	enabled := &lspRuntime{enabled: true, summary: mcptools.Summary{Names: []string{"lsp_definition"}}}
	disabled := &lspRuntime{enabled: false, summary: mcptools.Summary{Names: []string{"lsp_definition"}}}
	empty := &lspRuntime{enabled: true, summary: mcptools.Summary{}}

	cases := []struct {
		name    string
		runtime *lspRuntime
	}{
		{"disabled", disabled},
		{"empty summary", empty},
		{"nil", nil},
	}
	for _, tc := range cases {
		f := lspDescriptionSuffix(tc.runtime)
		for _, name := range []string{"glob", "grep", "rg", "read_file"} {
			if got := f(name, "base"); got != "base" {
				t.Errorf("%s runtime must leave %s byte-identical, got %q", tc.name, name, got)
			}
		}
	}

	f := lspDescriptionSuffix(enabled)
	for _, name := range []string{"glob", "grep", "rg"} {
		got := f(name, "base")
		if got != "base"+lspPreferSuffix {
			t.Errorf("enabled suffix for %s = %q", name, got)
		}
	}
	if got := f("read_file", "base"); got != "base" {
		t.Errorf("non-navigation tool got suffix: %q", got)
	}
}
