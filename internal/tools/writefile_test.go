package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runWriteFile(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, writeFile{}, args)
}

func TestWriteFileCreateWithParents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "c.txt")
	out, err := runWriteFile(t, map[string]any{"path": p, "content": "hello\nworld\n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("should report created: %q", out)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatalf("file not created: %v", rerr)
	}
	if string(got) != "hello\nworld\n" {
		t.Errorf("content wrong: %q", got)
	}
}

func TestWriteFileReportsBytesAndLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	out, err := runWriteFile(t, map[string]any{"path": p, "content": "one\ntwo\nthree\n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "3 lines") {
		t.Errorf("should report 3 lines: %q", out)
	}
	if !strings.Contains(out, "14 bytes") {
		t.Errorf("should report 14 bytes: %q", out)
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "old\n")
	out, err := runWriteFile(t, map[string]any{"path": p, "content": "new\n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "overwrote") {
		t.Errorf("should report overwrote: %q", out)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "new\n" {
		t.Errorf("content wrong: %q", got)
	}
}

func TestWriteFileEmptyContentAllowed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	_, err := runWriteFile(t, map[string]any{"path": p, "content": ""})
	if err != nil {
		t.Fatalf("empty content should be allowed: %v", err)
	}
	if _, serr := os.Stat(p); serr != nil {
		t.Errorf("file should exist: %v", serr)
	}
}

func TestWriteFilePathIsDir(t *testing.T) {
	dir := t.TempDir()
	_, err := runWriteFile(t, map[string]any{"path": dir, "content": "x"})
	if err == nil {
		t.Fatal("expected error writing to a directory path")
	}
}

func TestWriteFileTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	_, err := runWriteFile(t, map[string]any{"path": filepath.Join(dir, "x") + "/", "content": "x"})
	if err == nil {
		t.Fatal("expected error for trailing-slash path")
	}
}

func TestWriteFileMissingPathArg(t *testing.T) {
	_, err := runWriteFile(t, map[string]any{"content": "x"})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestWriteFileMutatedPaths(t *testing.T) {
	paths, err := (writeFile{}).MutatedPaths([]byte(`{"path":"a.txt","content":"x"}`))
	if err != nil {
		t.Fatalf("MutatedPaths: %v", err)
	}
	if len(paths) != 1 || paths[0] != "a.txt" {
		t.Fatalf("MutatedPaths = %v, want [a.txt]", paths)
	}
}
