package taskcontext

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStateRequiresCompleteNonNullFields(t *testing.T) {
	data, err := json.Marshal(continuityState{Activated: true, Reset: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		for _, missing := range []bool{false, true} {
			dir := t.TempDir()
			copy := make(map[string]json.RawMessage)
			for key, value := range fields {
				copy[key] = value
			}
			if missing {
				delete(copy, key)
			} else {
				copy[key] = json.RawMessage(`null`)
			}
			data, _ := json.Marshal(copy)
			if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0600); err != nil {
				t.Fatal(err)
			}
			m := New(func() string { return dir })
			if m.ContextEnabled() || m.ContextPrepare() == nil {
				t.Fatalf("invalid state accepted, field %s missing=%v", key, missing)
			}
		}
	}
	fields["future"] = json.RawMessage(`{"compatible":true}`)
	dir := t.TempDir()
	data, _ = json.Marshal(fields)
	if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readState(dir); err != nil || !exists {
		t.Fatalf("unknown fields rejected: %v", err)
	}
}

func TestCopyNotesRejectsAliasedNoteSubdirectories(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	text := "historical evidence"
	if _, err := runNotes(context.Background(), from, noteInput{Path: "detail.md", Text: &text}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(from, "notes", "detail.md")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(from, "notes"), filepath.Join(to, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := CopyNotes(context.Background(), from, to); err == nil {
		t.Fatal("copy accepted aliased destination notes")
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("copy rewrote historical source note")
	}
}
