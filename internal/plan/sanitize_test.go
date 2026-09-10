package plan

import (
	"os"
	"strings"
	"testing"
)

func TestNoteTextSanitizerRunsBeforeValidationAndArtifactWrite(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	draft := "unsafe body"
	reads := 0
	tool := NewToolWithTextSanitizer(store, func() string { return dir }, func(text string) string {
		return strings.ReplaceAll(text, "unsafe", "")
	}).WithNoteReader(func(gotDir, path string) (string, error) {
		reads++
		if gotDir != dir || path != "unsafe.md" {
			t.Fatalf("reader arguments = %q, %q; path must not be sanitized", gotDir, path)
		}
		return draft, nil
	})
	args := map[string]any{"title": "unsafe title", "path": "unsafe.md"}
	if _, err := runRecord(t, tool, args); err != nil {
		t.Fatal(err)
	}
	latest, ok := store.Latest()
	if !ok || latest.Title != "title" || latest.Body != "body" {
		t.Fatalf("latest = %+v, present=%v", latest, ok)
	}
	if body, err := os.ReadFile(latest.Path); err != nil || string(body) != "# title\n\nbody\n" {
		t.Fatalf("artifact = %q, %v", body, err)
	}
	if draft != "unsafe body" {
		t.Fatal("sanitization changed the draft")
	}
	draft = "unsafe"
	if _, err := runRecord(t, tool, args); err == nil {
		t.Fatal("note body empty after sanitization was accepted")
	}
	args["title"] = "unsafe"
	if _, err := runRecord(t, tool, args); err == nil {
		t.Fatal("title empty after sanitization was accepted")
	}
	if reads != 2 {
		t.Fatalf("reader calls = %d, want two; invalid title must not read", reads)
	}
	if got, _ := store.Latest(); got != latest {
		t.Fatal("failed sanitization replaced latest")
	}
}

func TestTextSanitizerRunsBeforeValidationAndArtifactWrite(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	tool := NewToolWithTextSanitizer(store, func() string { return dir }, func(text string) string { return strings.ReplaceAll(text, "unsafe", "") })
	if _, err := runRecord(t, tool, map[string]any{"title": "unsafe title", "plan": "unsafe body"}); err != nil {
		t.Fatal(err)
	}
	latest, ok := store.Latest()
	if !ok || latest.Title != "title" || latest.Body != "body" {
		t.Fatalf("latest: %+v", latest)
	}
	body, err := os.ReadFile(latest.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "# title\n\nbody\n" {
		t.Fatalf("artifact = %q", body)
	}
	for _, input := range []map[string]any{{"title": "unsafe", "plan": "body"}, {"title": "title", "plan": "unsafe"}} {
		if _, err := runRecord(t, tool, input); err == nil {
			t.Fatal("text empty after sanitization was accepted")
		}
		if got, _ := store.Latest(); got != latest {
			t.Fatal("invalid plan changed store")
		}
	}
	if _, err := runRecord(t, NewTool(store, func() string { return dir }), map[string]any{"title": "unsafe", "plan": "unsafe"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Latest(); got.Title != "unsafe" || got.Body != "unsafe" {
		t.Fatal("ordinary tool unexpectedly sanitizes text")
	}
}
