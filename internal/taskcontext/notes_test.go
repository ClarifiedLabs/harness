package taskcontext

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/tools"
)

func TestNotesStorageLimitIsIndependentOfReadsAndBootstrap(t *testing.T) {
	dir := t.TempDir()
	text := strings.Repeat("x", maxNotes)
	if _, err := runNotes(context.Background(), dir, noteInput{Text: &text}); err != nil {
		t.Fatal(err)
	}
	read, err := runNotes(context.Background(), dir, noteInput{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Text string
		Next int `json:"next_offset"`
		End  bool
	}
	if err := json.Unmarshal([]byte(read), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Text) != 100 || page.Next != 100 || page.End {
		t.Fatalf("unbounded page: %+v", page)
	}
	hint, err := notesHint(dir)
	if err != nil || len(hint) > noteHintBytes {
		t.Fatalf("bootstrap = %d bytes, %v", len(hint), err)
	}
	one := "y"
	if _, err := runNotes(context.Background(), dir, noteInput{Action: "append", Text: &one}); err == nil {
		t.Fatal("accepted oversized append")
	}
	data, err := os.ReadFile(filepath.Join(dir, defaultNote))
	if err != nil || string(data) != text {
		t.Fatal("failed append changed saved note")
	}
	fresh := New(func() string { return dir })
	if hint, err := fresh.ContextSummary(); err != nil || len(hint) > noteHintBytes {
		t.Fatalf("resume: %v", err)
	}
}

func TestMultipleNoteFilesAppendListSearchAndPagination(t *testing.T) {
	dir := t.TempDir()
	first, second := "Failed test: ", "invalidation race"
	for _, in := range []noteInput{{Path: "debug/cache.md", Text: &first}, {Action: "append", Path: "debug/cache.md", Text: &second}} {
		if _, err := runNotes(context.Background(), dir, in); err != nil {
			t.Fatal(err)
		}
	}
	read, err := readNote(dir, "debug/cache.md")
	if err != nil || read != first+second {
		t.Fatalf("read = %q, %v", read, err)
	}
	for _, action := range []string{"list", "search"} {
		result, err := runNotes(context.Background(), dir, noteInput{Action: action, Query: "invalidation", Prefix: "debug/"})
		if err != nil || !strings.Contains(result, "debug/cache.md") {
			t.Fatalf("%s = %s %v", action, result, err)
		}
	}
	for _, path := range []string{"../outside.md", "/absolute.md", "a/../b", "."} {
		if _, err := runNotes(context.Background(), dir, noteInput{Path: path, Text: &first}); err == nil {
			t.Fatalf("invalid note name accepted: %q", path)
		}
	}
}

func TestProviderAvailabilityAndSessionScopedRefresh(t *testing.T) {
	dir := t.TempDir()
	enabled := true
	m := New(func() string { return dir })
	m.SetEnabled(func() bool { return enabled })
	r := &tools.Registry{}
	m.Register(r)
	refresh, _ := r.Lookup("new_context")
	if _, err := refresh.Run(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if m.ContextRequested() {
		t.Fatal("refresh crossed session boundary")
	}
	enabled = false
	if len(r.Specs()) != 0 {
		t.Fatal("disabled tools were advertised")
	}
	if _, err := refresh.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("disabled tool executed")
	}
	enabled = true
	if len(r.Specs()) != len(Names) {
		t.Fatal("switching back did not restore tools")
	}
}

func TestNoteReadCannotReturnNonadvancingUTF8Page(t *testing.T) {
	dir := t.TempDir()
	text := "💾 checkpoint"
	if _, err := runNotes(context.Background(), dir, noteInput{Text: &text}); err != nil {
		t.Fatal(err)
	}
	if _, err := runNotes(context.Background(), dir, noteInput{MaxBytes: 1}); err == nil {
		t.Fatal("accepted page that cannot fit its first character")
	}
	if _, err := runNotes(context.Background(), dir, noteInput{MaxBytes: 4}); err != nil {
		t.Fatal(err)
	}
}
