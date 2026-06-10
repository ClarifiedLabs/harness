package lspproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixedSnippet(m map[string]string) func(uri string, line int) string {
	return func(uri string, line int) string { return m[uri] }
}

func TestFormatLocations(t *testing.T) {
	locs := []Location{
		{URI: "file:///a", Range: Range{Start: Position{Line: 1, Character: 0}}},
		{URI: "file:///b", Range: Range{Start: Position{Line: 5, Character: 2}}},
	}
	snip := fixedSnippet(map[string]string{"file:///a": "line A", "file:///b": "line B"})
	got := formatLocations(locs, snip)
	want := "/a:2:1  line A\n/b:6:3  line B"
	if got != want {
		t.Fatalf("formatLocations =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatReferencesCap(t *testing.T) {
	locs := []Location{
		{URI: "file:///a", Range: Range{Start: Position{Line: 0}}},
		{URI: "file:///a", Range: Range{Start: Position{Line: 3}}},
		{URI: "file:///b", Range: Range{Start: Position{Line: 6}}},
	}
	got := formatReferences(locs, 2, fixedSnippet(nil))
	if strings.Count(got, "\n") != 2 { // two locations + one footer line
		t.Fatalf("expected 2 newlines, got:\n%s", got)
	}
	if !strings.Contains(got, "1 more") {
		t.Fatalf("missing omitted footer:\n%s", got)
	}
}

func TestFormatDocumentSymbols(t *testing.T) {
	syms := []Symbol{{Name: "Foo", Kind: 12, Line: 3, Children: []Symbol{{Name: "x", Kind: 13, Line: 4}}}}
	got := formatDocumentSymbols(syms, "/tmp/a.go")
	want := "function Foo  /tmp/a.go:4\n  variable x  /tmp/a.go:5"
	if got != want {
		t.Fatalf("formatDocumentSymbols =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatWorkspaceSymbols(t *testing.T) {
	syms := []Symbol{{Name: "Bar", Kind: 12, Line: 7, URI: "file:///tmp/c.go"}}
	got := formatWorkspaceSymbols(syms)
	want := "function Bar  /tmp/c.go:8"
	if got != want {
		t.Fatalf("formatWorkspaceSymbols = %q, want %q", got, want)
	}
}

func TestFormatDiagnostics(t *testing.T) {
	diags := []Diagnostic{
		{Range: Range{Start: Position{Line: 5}}, Severity: 2, Message: "unused", Source: "vet"},
		{Range: Range{Start: Position{Line: 2, Character: 4}}, Severity: 1, Message: "undefined: x", Source: "compiler", Code: json.RawMessage(`"E001"`)},
	}
	got := formatDiagnostics(diags, "/tmp/a.go")
	want := "error /tmp/a.go:3:5  undefined: x [compiler E001]\nwarning /tmp/a.go:6:1  unused [vet]"
	if got != want {
		t.Fatalf("formatDiagnostics =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatDiagnosticsEmpty(t *testing.T) {
	if got := formatDiagnostics(nil, "/tmp/a.go"); got != "no diagnostics" {
		t.Fatalf("empty diagnostics = %q", got)
	}
}

func TestFormatRenamePlan(t *testing.T) {
	edits := []FileEdits{
		{URI: "file:///tmp/a.go", Edits: []TextEdit{
			{Range: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 8}}, NewText: "Bar"},
		}},
	}
	lineFor := func(uri string, line int) (string, bool) {
		if uri == "file:///tmp/a.go" && line == 1 {
			return "func Foo() {", true
		}
		return "", false
	}
	got := formatRenamePlan(edits, lineFor)
	if !strings.Contains(got, "func Foo() {") || !strings.Contains(got, "func Bar() {") {
		t.Fatalf("rename plan missing before/after:\n%s", got)
	}
	if !strings.Contains(got, "did NOT modify") {
		t.Fatalf("rename plan missing apply instruction:\n%s", got)
	}
}

func TestSymbolKindName(t *testing.T) {
	if symbolKindName(12) != "function" || symbolKindName(5) != "class" || symbolKindName(999) != "symbol" {
		t.Fatalf("kind names: %q %q %q", symbolKindName(12), symbolKindName(5), symbolKindName(999))
	}
}
