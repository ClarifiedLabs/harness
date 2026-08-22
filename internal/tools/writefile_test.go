package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
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

func TestWriteFileExpectedSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "old\n")

	if _, err := runWriteFile(t, map[string]any{
		"path": p, "content": "new\n", "expected_sha256": sha256Hex([]byte("old\n")),
	}); err != nil {
		t.Fatalf("guarded overwrite: %v", err)
	}
	assertFileContent(t, p, "new\n")

	_, err := runWriteFile(t, map[string]any{
		"path": p, "content": "clobbered\n", "expected_sha256": sha256Hex([]byte("old\n")),
	})
	if err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("stale write error = %v", err)
	}
	assertFileContent(t, p, "new\n")
}

func TestWriteFileExpectedSHA256RejectsMissingPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.txt")
	_, err := runWriteFile(t, map[string]any{
		"path": p, "content": "new\n", "expected_sha256": sha256Hex([]byte("old\n")),
	})
	if err == nil || !strings.Contains(err.Error(), "disappeared since it was read") {
		t.Fatalf("missing guarded write error = %v", err)
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Fatalf("guarded write created missing path: %v", statErr)
	}
}

func TestFileSHA256ArgumentsRejectMalformedDigest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "old\n")
	if _, err := runWriteFile(t, map[string]any{"path": p, "content": "new", "expected_sha256": "not-a-digest"}); err == nil || !strings.Contains(err.Error(), "64-character") {
		t.Fatalf("malformed digest error = %v", err)
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

func TestWriteRetentionInputReceipt(t *testing.T) {
	tool := writeFile{}
	input := []byte(`{"path":"main.go","content":"` + strings.Repeat(`package main\n`, 60) + `"}`)
	receipt, ok := tool.RetentionInputReceipt(input)
	if !ok {
		t.Fatal("RetentionInputReceipt = ok false for a full write input")
	}
	var decoded struct {
		Path       string `json:"path"`
		Superseded string `json:"_superseded"`
	}
	if err := json.Unmarshal(receipt, &decoded); err != nil {
		t.Fatalf("receipt is not a JSON object: %v (%s)", err, receipt)
	}
	if decoded.Path != "main.go" {
		t.Fatalf("receipt path = %q, want main.go", decoded.Path)
	}
	if decoded.Superseded == "" {
		t.Fatal("receipt missing the superseded explanation")
	}
	if paths, err := tool.MutatedPaths(receipt); err != nil || len(paths) != 1 || paths[0] != "main.go" {
		t.Fatalf("receipt lost MutatedPaths decodability: %v %v", paths, err)
	}
	if len(receipt) >= len(input) {
		t.Fatalf("receipt did not shrink: %d -> %d", len(input), len(receipt))
	}
	if llm.ValidateToolInputObject(receipt) != nil {
		t.Fatalf("receipt is not a complete JSON object: %s", receipt)
	}
}

func TestWriteRetentionInputReceiptRejectsInvalid(t *testing.T) {
	tool := writeFile{}
	for _, tc := range []struct{ name, input string }{
		{"missing path", `{"content":"x"}`},
		{"not json", `path`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tool.RetentionInputReceipt([]byte(tc.input)); ok {
				t.Fatal("RetentionInputReceipt = ok true, want false")
			}
		})
	}
}
