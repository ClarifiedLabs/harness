package taskcontext

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadNoteValidationAndBound(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadNote("", defaultNote); err == nil {
		t.Fatal("blank session directory accepted")
	}
	if _, err := ReadNote(" \t", defaultNote); err == nil {
		t.Fatal("whitespace session directory accepted")
	}
	for _, name := range []string{"../escape", "/absolute", "a/../b", "a\\b", "a\x1b.md"} {
		if _, err := ReadNote(dir, name); err == nil {
			t.Fatalf("invalid note path accepted: %q", name)
		}
	}
	if text, err := ReadNote(dir, "missing.md"); err != nil || text != "" {
		t.Fatalf("missing note = %q, %v", text, err)
	}
	for _, text := range []string{strings.Repeat("x", maxNotes+1), "bad\xffUTF8", "bad\x1b[31m"} {
		if err := os.WriteFile(filepath.Join(dir, defaultNote), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadNote(dir, ""); err == nil {
			t.Fatal("invalid or oversized note accepted")
		}
	}
	text := strings.Repeat("a", maxNotes)
	if err := os.WriteFile(filepath.Join(dir, defaultNote), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadNote(dir, ""); err != nil || got != text {
		t.Fatalf("maximum-sized note rejected: %v", err)
	}
}

func TestCopyNotesPreservesStateAndHistoricalSource(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	policy := Policy{Reset: true}
	m, registry := policyManager(&from, &policy)
	checkpoint(t, registry, "source checkpoint")
	text := "draft evidence"
	if _, err := runNotes(context.Background(), from, noteInput{Path: "draft/answer.md", Text: &text}); err != nil {
		t.Fatal(err)
	}
	policy.Reset = false
	m.ContextEnabled()
	before, err := os.ReadFile(filepath.Join(from, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyNotes(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(to, stateFile))
	if err != nil || string(after) != string(before) {
		t.Fatalf("copy changed context state: %v", err)
	}
	child, childRegistry := policyManager(&to, &policy)
	if !child.ContextMemoryEnabled() || child.ContextEnabled() {
		t.Fatal("copy lost sticky state or reconciliation guard")
	}
	checkpoint(t, childRegistry, "child checkpoint")
	if got, err := ReadNote(from, ""); err != nil || got != "source checkpoint" {
		t.Fatal("child changed source notes")
	}
	if got, err := ReadNote(to, "draft/answer.md"); err != nil || got != text {
		t.Fatalf("named note missing from copy: %q, %v", got, err)
	}
	after, err = os.ReadFile(filepath.Join(from, stateFile))
	if err != nil || string(after) != string(before) {
		t.Fatal("copy or child mutated historical source state")
	}
}

func TestCopyNotesLegacyCancellationAndInvalidState(t *testing.T) {
	from := t.TempDir()
	to := filepath.Join(t.TempDir(), "uncreated")
	text := "legacy note"
	if _, err := runNotes(context.Background(), from, noteInput{Text: &text}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CopyNotes(ctx, from, to); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy = %v", err)
	}
	if _, err := os.Stat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled copy wrote destination")
	}
	if err := CopyNotes(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{from, to} {
		if _, err := os.Stat(filepath.Join(dir, stateFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy copy migrated historical state")
		}
	}
	for _, bad := range []string{"{", strings.Repeat("x", maxState+1)} {
		if err := os.WriteFile(filepath.Join(from, stateFile), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := CopyNotes(context.Background(), from, t.TempDir()); err == nil {
			t.Fatal("invalid context state copied")
		}
	}
}
