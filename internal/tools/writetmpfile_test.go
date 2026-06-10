package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runWriteTmp(t *testing.T, wt *writeTmpFile, name, content string) (string, error) {
	t.Helper()
	b, err := json.Marshal(map[string]string{"name": name, "content": content})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return wt.Run(context.Background(), b)
}

func cleanupTmpDir(t *testing.T, wt *writeTmpFile) {
	t.Helper()
	t.Cleanup(func() {
		if wt.dir != "" {
			os.RemoveAll(wt.dir)
		}
	})
}

func TestWriteTmpFileWritesUnderTempDir(t *testing.T) {
	wt := newWriteTmpFile()
	cleanupTmpDir(t, wt)

	out, err := runWriteTmp(t, wt, "plan.md", "# plan\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if wt.dir == "" {
		t.Fatal("temp dir not created on first write")
	}
	path := filepath.Join(wt.dir, "plan.md")
	if !filepath.IsAbs(path) {
		t.Errorf("temp dir is not absolute: %q", wt.dir)
	}
	if !strings.Contains(out, path) {
		t.Errorf("result should contain the absolute path %q: %q", path, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "# plan\n" {
		t.Errorf("content = %q, want %q", data, "# plan\n")
	}
}

func TestWriteTmpFileSharesOneDirAcrossWrites(t *testing.T) {
	wt := newWriteTmpFile()
	cleanupTmpDir(t, wt)

	if _, err := runWriteTmp(t, wt, "a.txt", "a"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	dir := wt.dir
	if _, err := runWriteTmp(t, wt, "b.txt", "b"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if wt.dir != dir {
		t.Errorf("temp dir changed between writes: %q -> %q", dir, wt.dir)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

func TestWriteTmpFileCreatesSubdirectories(t *testing.T) {
	wt := newWriteTmpFile()
	cleanupTmpDir(t, wt)

	if _, err := runWriteTmp(t, wt, "notes/plan.md", "x"); err != nil {
		t.Fatalf("write with subdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.dir, "notes", "plan.md")); err != nil {
		t.Errorf("file not created under subdir: %v", err)
	}
}

func TestWriteTmpFileRejectsEscapingNames(t *testing.T) {
	wt := newWriteTmpFile()
	cleanupTmpDir(t, wt)

	for _, name := range []string{
		"",
		"/etc/passwd",
		"../escape.txt",
		"a/../../b.txt",
		"dir/",
	} {
		if out, err := runWriteTmp(t, wt, name, "x"); err == nil {
			t.Errorf("name %q should be rejected, got %q", name, out)
		}
	}
}
