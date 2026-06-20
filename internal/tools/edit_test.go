package tools

import (
	"fmt"
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

func TestEditReplaceAllReplacesEveryOccurrence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	mustWrite(t, p, "old old\nfoo := old\nreturn old\n")

	out, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p, map[string]any{"oldText": "old", "newText": "new", "replaceAll": true}),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFileContent(t, p, "new new\nfoo := new\nreturn new\n")
	if !strings.Contains(out, "4 replacement(s)") {
		t.Errorf("success message should report 4 replacements: %q", out)
	}
}

func TestEditReplaceAllBypassesUniquenessError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "x x x\n")

	// Without replaceAll this is a duplicate-occurrence error (see
	// TestEditMultipleOccurrencesRejected); replaceAll must accept it.
	if _, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "x", "newText": "y", "replaceAll": true})},
	}); err != nil {
		t.Fatalf("replaceAll should bypass the uniqueness error: %v", err)
	}
	assertFileContent(t, p, "y y y\n")
}

func TestEditReplaceAllNotFoundStillErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "alpha\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "absent", "newText": "x", "replaceAll": true})},
	})
	if err == nil {
		t.Fatal("expected not-found error even with replaceAll")
	}
	if !strings.Contains(err.Error(), "could not find oldText") {
		t.Errorf("error text wrong: %v", err)
	}
}

func TestEditReplaceAllCoexistsWithUniqueEdit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "old old\nUNIQUE\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p,
				map[string]any{"oldText": "old", "newText": "new", "replaceAll": true},
				map[string]any{"oldText": "UNIQUE", "newText": "DONE"},
			),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFileContent(t, p, "new new\nDONE\n")
}

// A replaceAll span that overlaps a DIFFERENT edit must raise the overlap error
// rather than silently corrupting the file. Regression for the guard that only
// exempted spans from the same replaceAll block.
func TestEditReplaceAllOverlappingDifferentEditRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "foobar foo\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{
			editFileArg(p,
				map[string]any{"oldText": "foo", "newText": "X", "replaceAll": true},
				map[string]any{"oldText": "foobar", "newText": "Y"},
			),
		},
	})
	if err == nil {
		t.Fatal("expected overlap error when a replaceAll span overlaps a different edit")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error should mention overlap: %v", err)
	}
	// The file must be untouched, not silently corrupted to "X\n".
	assertFileContent(t, p, "foobar foo\n")
}

func TestEditReplaceAllDefaultsOff(t *testing.T) {
	// Omitting replaceAll keeps the strict uniqueness behavior.
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "x x\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "x", "newText": "y"})},
	})
	if err == nil {
		t.Fatal("expected duplicate error when replaceAll is omitted")
	}
}

// r35: a near-miss oldText (not caught by fuzzy normalization) gets a
// nearest-similar-line hint so the model can recover without a re-read.
func TestEditNotFoundIncludesNearestLineHint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	mustWrite(t, p, "package main\n\nfunc calculateTotal(items []int) int {\n\treturn 0\n}\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{
			"oldText": "func calcTotal(items []int) int {",
			"newText": "x",
		})},
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "nearest similar line is L3:") {
		t.Errorf("expected nearest-line hint pointing to L3: %v", err)
	}
	if !strings.Contains(err.Error(), "calculateTotal") {
		t.Errorf("hint should echo the similar line: %v", err)
	}
}

// A dissimilar oldText must not get a spurious hint.
func TestEditNotFoundNoHintWhenDissimilar(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "alpha\nbeta\ngamma\n")

	_, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "zzzzz qqqqq", "newText": "x"})},
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if strings.Contains(err.Error(), "nearest similar line") {
		t.Errorf("dissimilar oldText should not produce a hint: %v", err)
	}
}

func TestNearestSimilarLine(t *testing.T) {
	content := "import os\nimport sys\n\ndef compute_average(values):\n    return sum(values) / len(values)\n"
	n, text, ok := nearestSimilarLine(content, "def compute_avg(values):")
	if !ok {
		t.Fatal("expected a nearest line")
	}
	if n != 4 {
		t.Errorf("nearest line number = %d, want 4", n)
	}
	if !strings.Contains(text, "compute_average") {
		t.Errorf("nearest line text = %q", text)
	}

	if _, _, ok := nearestSimilarLine(content, "wholly unrelated xyzzy"); ok {
		t.Error("unrelated needle should not match any line")
	}
}

// r40: a successful edit returns a small numbered snippet of the changed region
// so the model can confirm the change without a follow-up read_file.
func TestEditSuccessIncludesVerificationSnippet(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	mustWrite(t, p, "package main\n\nfunc calculateTotal() int {\n\treturn 0\n}\n")

	out, err := runEdit(t, map[string]any{
		"files": []any{editFileArg(p, map[string]any{"oldText": "return 0", "newText": "return 42"})},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Changed line is L4; the snippet shows it (numbered) plus one line of context.
	if !strings.Contains(out, "4\t\treturn 42") {
		t.Errorf("snippet should show changed line 4 with new text:\n%q", out)
	}
	if !strings.Contains(out, "3\tfunc calculateTotal() int {") {
		t.Errorf("snippet should include a line of context above:\n%q", out)
	}
}

func TestEditSnippet(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	body := b.String()

	t.Run("two distant regions with context and ellipsis", func(t *testing.T) {
		snip := editSnippet(body, []editRegion{{startLine: 3, endLine: 3}, {startLine: 15, endLine: 15}})
		for _, want := range []string{"2\tline2", "3\tline3", "4\tline4", "14\tline14", "15\tline15", "16\tline16", "…"} {
			if !strings.Contains(snip, want) {
				t.Errorf("snippet missing %q:\n%s", want, snip)
			}
		}
	})

	t.Run("adjacent regions merge into one block", func(t *testing.T) {
		snip := editSnippet(body, []editRegion{{startLine: 5, endLine: 5}, {startLine: 6, endLine: 6}})
		if strings.Contains(snip, "…") {
			t.Errorf("adjacent regions should merge without an ellipsis:\n%s", snip)
		}
	})

	t.Run("region count is capped", func(t *testing.T) {
		regions := []editRegion{
			{1, 1}, {5, 5}, {9, 9}, {13, 13}, {17, 17},
		}
		snip := editSnippet(body, regions)
		if got := strings.Count(snip, "…"); got > editSnippetMaxRegions-1 {
			t.Errorf("expected at most %d separators, got %d:\n%s", editSnippetMaxRegions-1, got, snip)
		}
	})

	t.Run("byte budget trims at a line boundary", func(t *testing.T) {
		var wide strings.Builder
		for i := 1; i <= 60; i++ {
			fmt.Fprintf(&wide, "%s\n", strings.Repeat("x", 40))
		}
		snip := editSnippet(wide.String(), []editRegion{{startLine: 1, endLine: 50}})
		if len(snip) > editSnippetMaxBytes+len("\n…") {
			t.Errorf("snippet exceeded byte budget: %d bytes", len(snip))
		}
		if !strings.HasSuffix(snip, "…") {
			t.Errorf("trimmed snippet should end with an ellipsis:\n%s", snip)
		}
	})
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
