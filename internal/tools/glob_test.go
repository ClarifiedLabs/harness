package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "a/b.go", false},
		{"**/*.go", "a.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"a/**/*.go", "a/b/c.go", true},
		{"a/**/*.go", "a/c.go", true}, // ** matches zero segments
		{"a/**/*.go", "x/c.go", false},
		{"**", "anything/at/all", true},
		{"**/test*", "pkg/test_x", true},
		{"foo/*", "foo/bar", true},
		{"foo/*", "foo/bar/baz", false},
		{"*config*.go", "myconfig.go", true},
	}
	for _, c := range cases {
		got, err := globMatch(c.pat, c.name)
		if err != nil {
			t.Errorf("globMatch(%q,%q) error: %v", c.pat, c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestGlobToolFindsFiles(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "main.go"))
	mkfile(t, filepath.Join(dir, "internal", "a", "config.go"))
	mkfile(t, filepath.Join(dir, "internal", "b", "config_test.go"))
	mkfile(t, filepath.Join(dir, "readme.md"))

	out, err := runTool(t, glob{}, map[string]any{"pattern": "**/*config*.go", "root": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, filepath.Join(dir, "internal", "a", "config.go")) {
		t.Errorf("config.go not matched: %q", out)
	}
	if !strings.Contains(out, filepath.Join(dir, "internal", "b", "config_test.go")) {
		t.Errorf("config_test.go not matched: %q", out)
	}
	if strings.Contains(out, "main.go") || strings.Contains(out, "readme.md") {
		t.Errorf("non-matching files were included: %q", out)
	}
}

func TestGlobToolRootScopingAndDirs(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "top.go"))
	mkfile(t, filepath.Join(dir, "sub", "inner.go"))

	// Top-level-only pattern (no **) must not match nested files.
	out, err := runTool(t, glob{}, map[string]any{"pattern": "*.go", "root": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, filepath.Join(dir, "top.go")) {
		t.Errorf("top.go not matched: %q", out)
	}
	if strings.Contains(out, "inner.go") {
		t.Errorf("*.go must not match nested files: %q", out)
	}

	// A directory match shows a trailing slash.
	out, err = runTool(t, glob{}, map[string]any{"pattern": "sub", "root": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "sub/") {
		t.Errorf("directory match should show a trailing slash: %q", out)
	}
}

func TestGlobToolNoMatches(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "a.txt"))
	out, err := runTool(t, glob{}, map[string]any{"pattern": "**/*.go", "root": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "(no matches)" {
		t.Errorf("no-match output = %q, want (no matches)", out)
	}
}

func TestGlobToolValidatesArgs(t *testing.T) {
	if _, err := runTool(t, glob{}, map[string]any{}); err == nil {
		t.Error("expected error for missing pattern")
	}
	if _, err := runTool(t, glob{}, map[string]any{"pattern": "[bad"}); err == nil {
		t.Error("expected error for malformed glob pattern")
	}
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "f"))
	if _, err := runTool(t, glob{}, map[string]any{"pattern": "*", "root": filepath.Join(dir, "f")}); err == nil {
		t.Error("expected error when root is not a directory")
	}
}

func TestGlobIsReadOnly(t *testing.T) {
	if !(glob{}).ReadOnly(nil) {
		t.Error("glob must be read-only so it can dispatch concurrently")
	}
}

// mkfile creates path (including parent directories) with placeholder content.
func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
}
