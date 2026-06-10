package patch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustParse(t *testing.T, text string) []FilePatch {
	t.Helper()
	files, err := Parse(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return files
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestApplyExact(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "alpha\nbeta\ngamma\n")
	patch := "--- a/" + p + "\n+++ b/" + p + "\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	if got := readFile(t, p); got != "alpha\nBETA\ngamma\n" {
		t.Errorf("content wrong: %q", got)
	}
}

func TestApplyShiftedOffset(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	// File has two extra lines above the hunk's stated position, so the
	// context is found at an offset (level-2 match).
	writeFile(t, p, "header1\nheader2\nalpha\nbeta\ngamma\n")
	patch := "--- a/" + p + "\n+++ b/" + p + "\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	if got := readFile(t, p); got != "header1\nheader2\nalpha\nBETA\ngamma\n" {
		t.Errorf("content wrong: %q", got)
	}
}

func TestApplyWhitespaceDrift(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	// File uses spaces for indentation; the patch context uses a tab. Level-3
	// whitespace-normalized matching should still apply, preserving the file's
	// actual (space-indented) surrounding lines.
	writeFile(t, p, "func main() {\n    x := 1\n    y := 2\n}\n")
	patch := "--- a/" + p + "\n+++ b/" + p + "\n@@ -1,4 +1,4 @@\n func main() {\n \tx := 1\n-\ty := 2\n+\ty := 3\n }\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	got := readFile(t, p)
	if !strings.Contains(got, "y := 3") {
		t.Errorf("change not applied: %q", got)
	}
	// The preserved lines keep the file's original space indentation.
	if !strings.Contains(got, "    x := 1") {
		t.Errorf("original whitespace not preserved: %q", got)
	}
}

func TestApplyFailingHunkLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	orig := "alpha\nbeta\ngamma\n"
	writeFile(t, p, orig)
	// Two hunks: the first applies, the second cannot match. The whole file
	// must be left untouched (per-file atomicity).
	patch := "--- a/" + p + "\n+++ b/" + p +
		"\n@@ -1,1 +1,1 @@\n-alpha\n+ALPHA\n" +
		"@@ -3,1 +3,1 @@\n-DOES_NOT_EXIST\n+x\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 1 {
		t.Fatalf("want 1 rejection, got %+v", res.Rejected)
	}
	if !strings.Contains(res.Rejected[0].Reason, "hunk 2 of 2") {
		t.Errorf("rejection should name hunk 2 of 2: %q", res.Rejected[0].Reason)
	}
	if got := readFile(t, p); got != orig {
		t.Errorf("file should be untouched, got %q", got)
	}
}

func TestApplyLaterHunksShiftAfterInsertion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "1\n2\n3\n4\n5\n6\n")
	// First hunk inserts two lines after line 2; second hunk edits line 5,
	// whose actual position shifts by the insertion.
	patch := "--- a/" + p + "\n+++ b/" + p +
		"\n@@ -1,3 +1,5 @@\n 1\n 2\n+inserted-a\n+inserted-b\n 3\n" +
		"@@ -4,3 +6,3 @@\n 4\n-5\n+FIVE\n 6\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	want := "1\n2\ninserted-a\ninserted-b\n3\n4\nFIVE\n6\n"
	if got := readFile(t, p); got != want {
		t.Errorf("content wrong:\n got %q\nwant %q", got, want)
	}
}

func TestApplyZeroContextInsertion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "a\nb\nc\n")
	// Pure zero-old-count insertion with OldStart >= 1: unified-diff semantics
	// insert the new line AFTER existing line OldStart (here line 1), matching
	// GNU patch, which yields "a\nINSERTED\nb\nc".
	patch := "--- a/" + p + "\n+++ b/" + p + "\n@@ -1,0 +2,1 @@\n+INSERTED\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	want := "a\nINSERTED\nb\nc\n"
	if got := readFile(t, p); got != want {
		t.Errorf("content wrong:\n got %q\nwant %q", got, want)
	}
}

func TestApplyCreate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "new.txt")
	patch := "--- /dev/null\n+++ b/" + p + "\n@@ -0,0 +1,2 @@\n+hello\n+world\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	if got := readFile(t, p); got != "hello\nworld\n" {
		t.Errorf("created content wrong: %q", got)
	}
}

func TestApplyCreateWhereExistsRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exists.txt")
	writeFile(t, p, "already here\n")
	patch := "--- /dev/null\n+++ b/" + p + "\n@@ -0,0 +1,1 @@\n+new\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 1 {
		t.Fatalf("want 1 rejection, got %+v", res.Rejected)
	}
	if got := readFile(t, p); got != "already here\n" {
		t.Errorf("existing file should be untouched: %q", got)
	}
}

func TestApplyDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.txt")
	writeFile(t, p, "hello\nworld\n")
	patch := "--- a/" + p + "\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-hello\n-world\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should be deleted, stat err = %v", err)
	}
}

func TestApplyDeleteMismatchedContentRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "actual content\n")
	// The delete hunk claims different content.
	patch := "--- a/" + p + "\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-different content\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 1 {
		t.Fatalf("want 1 rejection, got %+v", res.Rejected)
	}
	if got := readFile(t, p); got != "actual content\n" {
		t.Errorf("file should be untouched: %q", got)
	}
}

func TestApplyRename(t *testing.T) {
	dir := t.TempDir()
	oldP := filepath.Join(dir, "old.txt")
	newP := filepath.Join(dir, "new.txt")
	writeFile(t, oldP, "keep\nx\n")
	patch := "diff --git a/" + oldP + " b/" + newP + "\n" +
		"rename from " + oldP + "\nrename to " + newP + "\n" +
		"--- a/" + oldP + "\n+++ b/" + newP + "\n@@ -1,2 +1,2 @@\n keep\n-x\n+y\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	if _, err := os.Stat(oldP); !os.IsNotExist(err) {
		t.Errorf("old path should be gone, stat err = %v", err)
	}
	if got := readFile(t, newP); got != "keep\ny\n" {
		t.Errorf("renamed content wrong: %q", got)
	}
}

func TestApplyPureRename(t *testing.T) {
	dir := t.TempDir()
	oldP := filepath.Join(dir, "a.txt")
	newP := filepath.Join(dir, "b.txt")
	writeFile(t, oldP, "unchanged\n")
	patch := "diff --git a/" + oldP + " b/" + newP + "\n" +
		"rename from " + oldP + "\nrename to " + newP + "\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", res.Rejected)
	}
	if _, err := os.Stat(oldP); !os.IsNotExist(err) {
		t.Errorf("old path should be gone, stat err = %v", err)
	}
	if got := readFile(t, newP); got != "unchanged\n" {
		t.Errorf("renamed content wrong: %q", got)
	}
}

func TestApplyMissingTargetRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nope.txt")
	patch := "--- a/" + p + "\n+++ b/" + p + "\n@@ -1,1 +1,1 @@\n-a\n+b\n"
	res := Apply(mustParse(t, patch))
	if len(res.Rejected) != 1 {
		t.Fatalf("want 1 rejection, got %+v", res.Rejected)
	}
}
