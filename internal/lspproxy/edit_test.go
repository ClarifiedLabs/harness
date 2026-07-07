package lspproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyWorkspaceTextEditsCrossFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	if err := os.WriteFile(a, []byte("package main\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package main\nvar _ = Foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := applyWorkspaceTextEdits([]FileEdits{
		{URI: uriForPath(a), Edits: []TextEdit{{
			Range:   Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 8}},
			NewText: "Bar",
		}}},
		{URI: uriForPath(b), Edits: []TextEdit{{
			Range:   Range{Start: Position{Line: 1, Character: 8}, End: Position{Line: 1, Character: 11}},
			NewText: "Bar",
		}}},
	})
	if err != nil {
		t.Fatalf("applyWorkspaceTextEdits: %v", err)
	}
	if result.Total != 2 || len(result.Files) != 2 {
		t.Fatalf("result = %+v, want 2 edits across 2 files", result)
	}
	if got := mustReadFile(t, a); got != "package main\nfunc Bar() {}\n" {
		t.Fatalf("a.go = %q", got)
	}
	if got := mustReadFile(t, b); got != "package main\nvar _ = Bar\n" {
		t.Fatalf("b.go = %q", got)
	}
}

func TestApplyWorkspaceTextEditsUTF16(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("😀Foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := applyWorkspaceTextEdits([]FileEdits{{URI: uriForPath(path), Edits: []TextEdit{{
		Range:   Range{Start: Position{Line: 0, Character: 2}, End: Position{Line: 0, Character: 5}},
		NewText: "Bar",
	}}}})
	if err != nil {
		t.Fatalf("applyWorkspaceTextEdits: %v", err)
	}
	if got := mustReadFile(t, path); got != "😀Bar\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestApplyWorkspaceTextEditsRejectsOverlapBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("good\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := applyWorkspaceTextEdits([]FileEdits{
		{URI: uriForPath(a), Edits: []TextEdit{
			{Range: Range{Start: Position{Line: 0, Character: 1}, End: Position{Line: 0, Character: 4}}, NewText: "X"},
			{Range: Range{Start: Position{Line: 0, Character: 3}, End: Position{Line: 0, Character: 5}}, NewText: "Y"},
		}},
		{URI: uriForPath(b), Edits: []TextEdit{{
			Range:   Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 4}},
			NewText: "bad",
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "overlapping edits") {
		t.Fatalf("err = %v, want overlap error", err)
	}
	if got := mustReadFile(t, a); got != "abcdef\n" {
		t.Fatalf("a.txt changed after validation failure: %q", got)
	}
	if got := mustReadFile(t, b); got != "good\n" {
		t.Fatalf("b.txt changed after validation failure: %q", got)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
