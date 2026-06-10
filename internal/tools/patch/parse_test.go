package patch

import (
	"strings"
	"testing"
)

func TestParseSingleHunk(t *testing.T) {
	in := `--- a/foo.txt
+++ b/foo.txt
@@ -1,3 +1,3 @@
 alpha
-beta
+BETA
 gamma
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Old != "foo.txt" || f.New != "foo.txt" {
		t.Errorf("prefix stripping wrong: Old=%q New=%q", f.Old, f.New)
	}
	if f.IsCreate || f.IsDelete || f.IsRename {
		t.Errorf("flags should all be false: %+v", f)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 1 || h.OldCount != 3 || h.NewStart != 1 || h.NewCount != 3 {
		t.Errorf("hunk header parsed wrong: %+v", h)
	}
	if len(h.Lines) != 4 {
		t.Fatalf("want 4 hunk lines, got %d", len(h.Lines))
	}
	wantKinds := []LineKind{Context, Del, Add, Context}
	for i, k := range wantKinds {
		if h.Lines[i].Kind != k {
			t.Errorf("line %d kind = %v, want %v", i, h.Lines[i].Kind, k)
		}
	}
	if h.Lines[1].Text != "beta" || h.Lines[2].Text != "BETA" {
		t.Errorf("line text wrong: %q %q", h.Lines[1].Text, h.Lines[2].Text)
	}
}

func TestParseMultiHunk(t *testing.T) {
	in := `--- a/x.go
+++ b/x.go
@@ -1,2 +1,2 @@
 one
-two
+TWO
@@ -10,2 +10,2 @@
 ten
-eleven
+ELEVEN
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(files[0].Hunks))
	}
	if files[0].Hunks[1].OldStart != 10 || files[0].Hunks[1].NewStart != 10 {
		t.Errorf("second hunk header wrong: %+v", files[0].Hunks[1])
	}
}

func TestParseMultiFile(t *testing.T) {
	in := `--- a/one.txt
+++ b/one.txt
@@ -1 +1 @@
-a
+A
--- a/two.txt
+++ b/two.txt
@@ -1 +1 @@
-b
+B
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].New != "one.txt" || files[1].New != "two.txt" {
		t.Errorf("file names wrong: %q %q", files[0].New, files[1].New)
	}
}

func TestParseCreate(t *testing.T) {
	in := `--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := files[0]
	if !f.IsCreate {
		t.Errorf("should be a create: %+v", f)
	}
	if f.New != "new.txt" {
		t.Errorf("new path wrong: %q", f.New)
	}
}

func TestParseDelete(t *testing.T) {
	in := `--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-hello
-world
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := files[0]
	if !f.IsDelete {
		t.Errorf("should be a delete: %+v", f)
	}
	if f.Old != "gone.txt" {
		t.Errorf("old path wrong: %q", f.Old)
	}
}

func TestParseRename(t *testing.T) {
	in := `diff --git a/old/name.go b/new/name.go
similarity index 95%
rename from old/name.go
rename to new/name.go
--- a/old/name.go
+++ b/new/name.go
@@ -1,2 +1,2 @@
 keep
-x
+y
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := files[0]
	if !f.IsRename {
		t.Errorf("should be a rename: %+v", f)
	}
	if f.Old != "old/name.go" || f.New != "new/name.go" {
		t.Errorf("rename paths wrong: Old=%q New=%q", f.Old, f.New)
	}
}

func TestParseRenameNoHunks(t *testing.T) {
	in := `diff --git a/a.txt b/b.txt
similarity index 100%
rename from a.txt
rename to b.txt
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if !f.IsRename {
		t.Errorf("should be a rename: %+v", f)
	}
	if f.Old != "a.txt" || f.New != "b.txt" {
		t.Errorf("rename paths wrong: Old=%q New=%q", f.Old, f.New)
	}
	if len(f.Hunks) != 0 {
		t.Errorf("pure rename should have no hunks: %+v", f.Hunks)
	}
}

func TestParsePrefixStripping(t *testing.T) {
	in := `--- a/dir/sub/file.go
+++ b/dir/sub/file.go
@@ -1 +1 @@
-x
+y
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files[0].Old != "dir/sub/file.go" || files[0].New != "dir/sub/file.go" {
		t.Errorf("a/ b/ prefixes not stripped: %q %q", files[0].Old, files[0].New)
	}
}

func TestParseMalformedHunkHeader(t *testing.T) {
	in := `--- a/x.txt
+++ b/x.txt
@@ this is not a hunk header @@
 context
`
	_, err := Parse(in)
	if err == nil {
		t.Fatal("expected error for malformed hunk header")
	}
}

func TestParseNoNewlineAtEOF(t *testing.T) {
	in := "--- a/f.txt\n" +
		"+++ b/f.txt\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"\\ No newline at end of file\n" +
		"+new\n" +
		"\\ No newline at end of file\n"
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h := files[0].Hunks[0]
	if len(h.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(h.Lines))
	}
	if !h.Lines[0].NoNewline {
		t.Errorf("deleted line should be marked NoNewline")
	}
	if !h.Lines[1].NoNewline {
		t.Errorf("added line should be marked NoNewline")
	}
	if h.Lines[0].Text != "old" || h.Lines[1].Text != "new" {
		t.Errorf("line text wrong: %q %q", h.Lines[0].Text, h.Lines[1].Text)
	}
}

func TestParseHunkHeaderSingleLineCounts(t *testing.T) {
	// "@@ -L +L @@" with omitted counts means a count of 1.
	in := `--- a/f.txt
+++ b/f.txt
@@ -5 +5 @@
-x
+y
`
	files, err := Parse(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h := files[0].Hunks[0]
	if h.OldStart != 5 || h.OldCount != 1 || h.NewStart != 5 || h.NewCount != 1 {
		t.Errorf("omitted counts should default to 1: %+v", h)
	}
}

func TestParseEmptyPatch(t *testing.T) {
	_, err := Parse("   \n")
	if err == nil {
		t.Fatal("expected error for a patch with no file headers")
	}
	if !strings.Contains(err.Error(), "no file") && !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should explain there is nothing to apply: %v", err)
	}
}
