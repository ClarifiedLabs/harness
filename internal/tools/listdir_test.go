package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runListDir(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, listDir{}, args)
}

func TestListDirOrdering(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "zebra.txt"), "z")
	mustWrite(t, filepath.Join(dir, "apple.txt"), "a")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0755); err != nil {
		t.Fatal(err)
	}

	out, err := runListDir(t, map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 entries, got %d:\n%s", len(lines), out)
	}
	// Dirs first (alphabetical), each with a trailing slash; then files.
	if !strings.HasSuffix(lines[0], "alpha/") {
		t.Errorf("line 0 should be alpha/: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "sub/") {
		t.Errorf("line 1 should be sub/: %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "apple.txt") {
		t.Errorf("line 2 should be apple.txt: %q", lines[2])
	}
	if !strings.HasSuffix(lines[3], "zebra.txt") {
		t.Errorf("line 3 should be zebra.txt: %q", lines[3])
	}
	// Each line carries a type char and a size column before the name.
	if !strings.HasPrefix(lines[0], "d") {
		t.Errorf("dir line should start with type char 'd': %q", lines[0])
	}
}

func TestListDirGlob(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "x")
	mustWrite(t, filepath.Join(dir, "b.go"), "x")
	mustWrite(t, filepath.Join(dir, "c.txt"), "x")

	out, err := runListDir(t, map[string]any{"path": dir, "glob": "*.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "c.txt") {
		t.Errorf("glob should exclude c.txt: %q", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("glob should include go files: %q", out)
	}
}

func TestListDirCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 1100; i++ {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), "x")
	}
	out, err := runListDir(t, map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected truncation marker at 1000-entry cap")
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1001 { // 1000 entries + marker
		t.Errorf("want 1001 lines (1000 + marker), got %d", len(lines))
	}
}

func TestListDirNotADir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "x")
	_, err := runListDir(t, map[string]any{"path": p})
	if err == nil {
		t.Fatal("expected error listing a file")
	}
}

func TestListDirDefaultPath(t *testing.T) {
	// Default path "." must not error in the test's working directory.
	_, err := runListDir(t, map[string]any{})
	if err != nil {
		t.Fatalf("default path should list cwd: %v", err)
	}
}

func TestListDirUnreadableShowsQuestionMark(t *testing.T) {
	// A broken symlink cannot be Stat'd via Lstat->Stat; its size renders "?"
	// and the listing continues.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "real.txt"), "x")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "broken")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	out, err := runListDir(t, map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("listing should continue past unreadable entry: %v", err)
	}
	if !strings.Contains(out, "real.txt") {
		t.Errorf("readable entry missing: %q", out)
	}
	if !strings.Contains(out, "broken") {
		t.Errorf("unreadable entry should still be listed: %q", out)
	}
	if !strings.Contains(out, "?") {
		t.Errorf("unreadable entry should show '?' size: %q", out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
