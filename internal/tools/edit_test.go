package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runEdit(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, edit{}, args)
}

func editFileArg(path string, edits ...map[string]any) map[string]any {
	return map[string]any{"path": path, "edits": edits}
}

func TestEditMultipleFilesAndEdits(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	mustWrite(t, a, "alpha beta gamma\n")
	mustWrite(t, b, "one two three\n")

	out, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(a,
				map[string]any{"oldText": "alpha", "newText": "ALPHA"},
				map[string]any{"oldText": "gamma", "newText": "GAMMA"},
			),
			editFileArg(b,
				map[string]any{"oldText": "two", "newText": "TWO"},
			),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "edited 2 file(s), 3 replacement(s)") {
		t.Errorf("success message should report file and replacement counts: %q", out)
	}
	assertFileContent(t, a, "ALPHA beta GAMMA\n")
	assertFileContent(t, b, "one TWO three\n")
}

func TestEditMatchesAllEditsAgainstOriginalContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "a b c d\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p,
				map[string]any{"oldText": "a", "newText": "b"},
				map[string]any{"oldText": "b", "newText": "c"},
			),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFileContent(t, p, "b c c d\n")
}

func TestEditZeroMatches(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "alpha\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "absent", "newText": "x"})},
	})
	if err == nil {
		t.Fatal("expected error for zero matches")
	}
	if !strings.Contains(err.Error(), "could not find oldText") {
		t.Errorf("error text wrong: %v", err)
	}
}

func TestEditMultipleOccurrencesRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "x x x\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "x", "newText": "y"})},
	})
	if err == nil {
		t.Fatal("expected error for multiple matches")
	}
	if !strings.Contains(err.Error(), "found 3 occurrences") {
		t.Errorf("error should report count: %v", err)
	}
	assertFileContent(t, p, "x x x\n")
}

func TestEditOverlappingEditsRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "abcdef\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p,
				map[string]any{"oldText": "abc", "newText": "ABC"},
				map[string]any{"oldText": "bcd", "newText": "BCD"},
			),
		},
	})
	if err == nil {
		t.Fatal("expected error for overlapping edits")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error should mention overlap: %v", err)
	}
	assertFileContent(t, p, "abcdef\n")
}

func TestEditNoPartialWriteWhenLaterFileFails(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	mustWrite(t, a, "alpha\n")
	mustWrite(t, b, "beta\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(a, map[string]any{"oldText": "alpha", "newText": "ALPHA"}),
			editFileArg(b, map[string]any{"oldText": "missing", "newText": "MISSING"}),
		},
	})
	if err == nil {
		t.Fatal("expected error for second file")
	}
	assertFileContent(t, a, "alpha\n")
	assertFileContent(t, b, "beta\n")
}

func TestEditFuzzyMatchNormalizesSmartQuotesDashesAndTrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "const s = “hello”   \nconst dash = a—b\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p,
				map[string]any{"oldText": "const s = \"hello\"", "newText": "const s = \"hi\""},
				map[string]any{"oldText": "const dash = a-b", "newText": "const dash = a + b"},
			),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFileContent(t, p, "const s = \"hi\"\nconst dash = a + b\n")
}

func TestEditPreservesCRLFAndBOM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("\xEF\xBB\xBFone\r\ntwo\r\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "one\ntwo", "newText": "uno\ndos"})},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if string(got) != "\xEF\xBB\xBFuno\r\ndos\r\n" {
		t.Errorf("file content wrong: %q", got)
	}
}

func TestEditNoChangesRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "alpha\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "alpha", "newText": "alpha"})},
	})
	if err == nil {
		t.Fatal("expected error when replacement is identical")
	}
}

func TestEditMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(filepath.Join(dir, "nope.txt"), map[string]any{"oldText": "a", "newText": "b"})},
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "write_file") {
		t.Errorf("missing file should direct to write_file: %v", err)
	}
}

func TestEditMissingRequiredArgs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "alpha\n")

	tests := []map[string]any{
		{},
		{"files": []any{}},
		{"files": []any{map[string]any{"edits": []any{map[string]any{"oldText": "a", "newText": "b"}}}}},
		{"files": []any{map[string]any{"path": p}}},
		{"files": []any{editFileArg(p, map[string]any{"newText": "b"})}},
		{"files": []any{editFileArg(p, map[string]any{"oldText": "a"})}},
		{"files": []any{editFileArg(p, map[string]any{"oldText": "", "newText": "b"})}},
	}
	for i, tc := range tests {
		if _, err := runEdit(t, tc); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestEditDuplicateFileEntryRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "alpha\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p, map[string]any{"oldText": "alpha", "newText": "ALPHA"}),
			editFileArg(filepath.Join(dir, ".", "f.txt"), map[string]any{"oldText": "alpha", "newText": "ALPHA"}),
		},
	})
	if err == nil {
		t.Fatal("expected duplicate path error")
	}
	assertFileContent(t, p, "alpha\n")
}

func TestEditMutatedPaths(t *testing.T) {
	paths, err := (edit{}).MutatedPaths([]byte(`{"files":[{"path":"a.txt","edits":[{"oldText":"a","newText":"b"}]},{"path":"b.txt","edits":[{"oldText":"c","newText":"d"}]}]}`))
	if err != nil {
		t.Fatalf("MutatedPaths: %v", err)
	}
	if len(paths) != 2 || paths[0] != "a.txt" || paths[1] != "b.txt" {
		t.Fatalf("MutatedPaths = %v, want [a.txt b.txt]", paths)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s content = %q, want %q", path, got, want)
	}
}
