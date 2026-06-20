package diff

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedModify(t *testing.T) {
	got := Unified("f.txt", true, []byte("foo\nbar\n"), true, []byte("foo\nbaz\n"), Options{})
	for _, want := range []string{
		"--- a/f.txt\n",
		"+++ b/f.txt\n",
		"@@ -1,2 +1,2 @@\n",
		" foo\n",
		"-bar\n",
		"+baz\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff missing %q:\n%s", want, got)
		}
	}
}

func TestUnifiedCreateDeleteAndNoop(t *testing.T) {
	created := Unified("new.txt", false, nil, true, []byte("hello\n"), Options{})
	if !strings.Contains(created, "--- /dev/null\n") || !strings.Contains(created, "+++ b/new.txt\n") || !strings.Contains(created, "+hello\n") {
		t.Fatalf("created diff wrong:\n%s", created)
	}

	deleted := Unified("old.txt", true, []byte("bye\n"), false, nil, Options{})
	if !strings.Contains(deleted, "--- a/old.txt\n") || !strings.Contains(deleted, "+++ /dev/null\n") || !strings.Contains(deleted, "-bye\n") {
		t.Fatalf("deleted diff wrong:\n%s", deleted)
	}

	if got := Unified("same.txt", true, []byte("same\n"), true, []byte("same\n"), Options{}); got != "" {
		t.Fatalf("no-op diff = %q, want empty", got)
	}
}

func TestUnifiedMultipleHunks(t *testing.T) {
	old := []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n")
	newer := []byte("1\nTWO\n3\n4\n5\n6\n7\nEIGHT\n9\n")
	got := Unified("many.txt", true, old, true, newer, Options{Context: 1})
	if count := strings.Count(got, "@@ "); count != 2 {
		t.Fatalf("hunk count = %d, want 2:\n%s", count, got)
	}
	if !strings.Contains(got, "-2\n+TWO\n") || !strings.Contains(got, "-8\n+EIGHT\n") {
		t.Fatalf("diff missing changes:\n%s", got)
	}
}

func TestUnifiedLargeFileSmallChanges(t *testing.T) {
	var old, newer strings.Builder
	for i := 1; i <= 2300; i++ {
		oldLine := "same"
		newLine := "same"
		switch i {
		case 780:
			oldLine = "optional file diffs"
			newLine = "file diffs"
		case 1720:
			oldLine = "show-diffs"
			newLine = "show-diffs (default true)"
		}
		old.WriteString(oldLine)
		old.WriteByte('\n')
		newer.WriteString(newLine)
		newer.WriteByte('\n')
	}

	got := Unified("large.md", true, []byte(old.String()), true, []byte(newer.String()), Options{})
	if strings.Contains(got, "@@ -1,2300 +1,2300 @@") {
		t.Fatalf("large small edit fell back to whole-file diff")
	}
	if count := strings.Count(got, "@@ "); count != 2 {
		t.Fatalf("hunk count = %d, want 2:\n%s", count, got)
	}
	for _, want := range []string{
		"-optional file diffs\n+file diffs\n",
		"-show-diffs\n+show-diffs (default true)\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff missing %q:\n%s", want, got)
		}
	}
}

func TestUnifiedMissingFinalNewline(t *testing.T) {
	got := Unified("nonewline.txt", true, []byte("a"), true, []byte("a\n"), Options{})
	if !strings.Contains(got, "\\ No newline at end of file\n") {
		t.Fatalf("diff should show missing newline marker:\n%s", got)
	}
}

func TestRenderSnapshotsBinarySkip(t *testing.T) {
	before := []Snapshot{{Path: "bin.dat", Exists: true, Data: []byte{'a', 0, 'b'}}}
	after := []Snapshot{{Path: "bin.dat", Exists: true, Data: []byte{'a', 0, 'c'}}}
	got := RenderSnapshots(before, after, Options{})
	if len(got) != 1 || !got[0].BinarySkipped {
		t.Fatalf("binary diff = %+v, want one skipped result", got)
	}
}

func TestRenderSnapshotsIncrementalSameFile(t *testing.T) {
	path := filepath.Join("tmp", "f.txt")
	first := RenderSnapshots(
		[]Snapshot{{Path: path, Exists: true, Data: []byte("foo\nbar\n")}},
		[]Snapshot{{Path: path, Exists: true, Data: []byte("foo\nfoo\n")}},
		Options{},
	)
	second := RenderSnapshots(
		[]Snapshot{{Path: path, Exists: true, Data: []byte("foo\nfoo\n")}},
		[]Snapshot{{Path: path, Exists: true, Data: []byte("foo\nbar\n")}},
		Options{},
	)
	if len(first) != 1 || !strings.Contains(first[0].Text, "-bar\n+foo\n") {
		t.Fatalf("first incremental diff wrong: %+v", first)
	}
	if len(second) != 1 || !strings.Contains(second[0].Text, "-foo\n+bar\n") {
		t.Fatalf("second incremental diff wrong: %+v", second)
	}
}

func TestColorize(t *testing.T) {
	got := Colorize("--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n-old\n+new\n")
	for _, want := range []string{ansiCyan, ansiRed, ansiGreen, ansiReset} {
		if !strings.Contains(got, want) {
			t.Fatalf("colorized diff missing %q: %q", want, got)
		}
	}
}
