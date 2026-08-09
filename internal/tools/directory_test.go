package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryRenderingOrderingAndTypes(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "zebra.txt"), "z")
	mustWrite(t, filepath.Join(dir, "apple.txt"), "a")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := renderDirectory(dir, readDirectoryCap)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 entries, got %d:\n%s", len(lines), out)
	}
	for i, suffix := range []string{"alpha/", "sub/", "apple.txt", "zebra.txt"} {
		if !strings.HasSuffix(lines[i], suffix) {
			t.Errorf("line %d = %q, want suffix %q", i, lines[i], suffix)
		}
	}
	if !strings.HasPrefix(lines[0], "d") || !strings.HasPrefix(lines[2], "-") {
		t.Errorf("entry type columns missing: %q", out)
	}
	if !strings.Contains(lines[2], "1B") {
		t.Errorf("file size missing: %q", lines[2])
	}
}

func TestDirectoryRenderingEmpty(t *testing.T) {
	out, err := renderDirectory(t.TempDir(), readDirectoryCap)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(empty directory)" {
		t.Fatalf("empty directory = %q", out)
	}
}

func TestDirectoryRenderingCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < readDirectoryCap+5; i++ {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), "x")
	}
	out, err := renderDirectory(dir, readDirectoryCap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated: showing first 200 of 205 entries]") {
		t.Fatalf("missing neutral truncation marker: %q", out[max(0, len(out)-100):])
	}
	if got := len(strings.Split(out, "\n")); got != readDirectoryCap+1 {
		t.Fatalf("listing lines = %d, want %d", got, readDirectoryCap+1)
	}
}

func TestDirectoryRenderingBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "real.txt"), "x")
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "broken")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	out, err := renderDirectory(dir, readDirectoryCap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "l        ?  broken") || !strings.Contains(out, "real.txt") {
		t.Fatalf("symlink listing = %q", out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
