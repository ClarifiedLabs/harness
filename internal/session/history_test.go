package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadHistory_Missing(t *testing.T) {
	entries, err := LoadHistory(filepath.Join(t.TempDir(), "missing"), 1000, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if entries != nil {
		t.Fatalf("LoadHistory missing file: got %v, want nil", entries)
	}
}

func TestLoadHistory_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := LoadHistory(path, 1000, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("LoadHistory: got %d entries, want 3", len(entries))
	}
	if entries[0] != "first" || entries[1] != "second" || entries[2] != "third" {
		t.Fatalf("LoadHistory: got %v, want [first second third]", entries)
	}
}

func TestLoadHistory_SkipsBlankAndMultiline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	content := "valid1\n\n   \nvalid2\nline1\rline2\nvalid3\n"
	err := os.WriteFile(path, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := LoadHistory(path, 1000, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("LoadHistory: got %d entries, want 3", len(entries))
	}
	if entries[0] != "valid1" || entries[1] != "valid2" || entries[2] != "valid3" {
		t.Fatalf("LoadHistory: got %v, want [valid1 valid2 valid3]", entries)
	}
}

func TestLoadHistory_DedupesKeepsLast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	content := "first\nsecond\nfirst\nthird\nsecond\n"
	err := os.WriteFile(path, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := LoadHistory(path, 1000, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("LoadHistory: got %d entries, want 3", len(entries))
	}
	// After dedupe keeping last: first, third, second (in chronological order)
	if entries[0] != "first" || entries[1] != "third" || entries[2] != "second" {
		t.Fatalf("LoadHistory: got %v, want [first third second]", entries)
	}

	// Verify file was rewritten with deduplicated content
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.TrimSpace(string(data)) != "first\nthird\nsecond" {
		t.Fatalf("file content: got %q, want %q", string(data), "first\nthird\nsecond\n")
	}
}

func TestLoadHistory_FileSizeCapRewrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	// Write 10 entries
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, strings.Repeat("x", i))
	}
	err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Load with fileSize=5, memSize=1000
	entries, err := LoadHistory(path, 5, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("LoadHistory: got %d entries, want 5", len(entries))
	}
	// Should get the last 5 entries
	for i, want := range lines[5:10] {
		if entries[i] != want {
			t.Fatalf("entries[%d] = %q, want %q", i, entries[i], want)
		}
	}

	// Verify file was rewritten with only 5 entries
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	fileLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(fileLines) != 5 {
		t.Fatalf("file has %d lines, want 5", len(fileLines))
	}
}

func TestLoadHistory_MemSizeCapDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	// Write 10 entries
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, strings.Repeat("x", i))
	}
	originalContent := strings.Join(lines, "\n") + "\n"
	err := os.WriteFile(path, []byte(originalContent), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Load with fileSize=20, memSize=3
	entries, err := LoadHistory(path, 20, 3)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("LoadHistory: got %d entries, want 3", len(entries))
	}
	// Should get the last 3 entries
	for i, want := range lines[7:10] {
		if entries[i] != want {
			t.Fatalf("entries[%d] = %q, want %q", i, entries[i], want)
		}
	}

	// Verify file was NOT rewritten (still has 10 entries)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != originalContent {
		t.Fatalf("file was rewritten when it shouldn't have been")
	}
}

func TestLoadHistory_ZeroFileSizeDisablesPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	entries, err := LoadHistory(path, 0, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if entries != nil {
		t.Fatalf("LoadHistory with fileSize=0: got %v, want nil", entries)
	}

	// File should not exist
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file exists when fileSize=0")
	}
}

func TestLoadHistory_ZeroMemSizeReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	err := os.WriteFile(path, []byte("test\n"), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := LoadHistory(path, 1000, 0)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if entries != nil {
		t.Fatalf("LoadHistory with memSize=0: got %v, want nil", entries)
	}
}

func TestLoadHistory_EmptyPathDisables(t *testing.T) {
	entries, err := LoadHistory("", 1000, 1000)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if entries != nil {
		t.Fatalf("LoadHistory with empty path: got %v, want nil", entries)
	}
}

func TestAppendHistory_CreatesFileAndParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "history")

	err := AppendHistory(path, "first entry")
	if err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.TrimSpace(string(data)) != "first entry" {
		t.Fatalf("file content: got %q, want %q", string(data), "first entry\n")
	}
}

func TestAppendHistory_Appends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	if err := AppendHistory(path, "first"); err != nil {
		t.Fatalf("AppendHistory first: %v", err)
	}
	if err := AppendHistory(path, "second"); err != nil {
		t.Fatalf("AppendHistory second: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.TrimSpace(string(data)) != "first\nsecond" {
		t.Fatalf("file content: got %q, want %q", string(data), "first\nsecond\n")
	}
}

func TestAppendHistory_SkipsBlankAndMultiline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	if err := AppendHistory(path, "valid1"); err != nil {
		t.Fatalf("AppendHistory valid1: %v", err)
	}
	if err := AppendHistory(path, ""); err != nil {
		t.Fatalf("AppendHistory blank: %v", err)
	}
	if err := AppendHistory(path, "   "); err != nil {
		t.Fatalf("AppendHistory whitespace: %v", err)
	}
	if err := AppendHistory(path, "line1\nline2"); err != nil {
		t.Fatalf("AppendHistory multiline: %v", err)
	}
	if err := AppendHistory(path, "valid2"); err != nil {
		t.Fatalf("AppendHistory valid2: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.TrimSpace(string(data)) != "valid1\nvalid2" {
		t.Fatalf("file content: got %q, want %q", string(data), "valid1\nvalid2\n")
	}
}

func TestAppendHistory_EmptyPathNoOp(t *testing.T) {
	if err := AppendHistory("", "test"); err != nil {
		t.Fatalf("AppendHistory empty path: %v", err)
	}
}

func TestHistoryPath(t *testing.T) {
	t.Run("empty state dir", func(t *testing.T) {
		if got := HistoryPath(""); got != "" {
			t.Fatalf("HistoryPath(\"\") = %q, want \"\"", got)
		}
	})
	t.Run("relative path", func(t *testing.T) {
		got := HistoryPath("mystate")
		if want := filepath.Join("mystate", "harness", "history"); got != want {
			t.Fatalf("HistoryPath(%q) = %q, want %q", "mystate", got, want)
		}
	})
	t.Run("absolute path", func(t *testing.T) {
		got := HistoryPath("/tmp/state")
		if want := filepath.Join("/tmp/state", "harness", "history"); got != want {
			t.Fatalf("HistoryPath(%q) = %q, want %q", "/tmp/state", got, want)
		}
	})
}
