package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSimilarExistingPaths(t *testing.T) {
	t.Run("typo file name in existing directory", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "usage.md"), "# usage\n")
		got := similarExistingPaths(dir, "ussage.md")
		if len(got) != 1 || got[0] != filepath.Join(dir, "usage.md") {
			t.Fatalf("suggestions = %v", got)
		}
	})

	t.Run("typo directory component", func(t *testing.T) {
		dir := t.TempDir()
		docs := filepath.Join(dir, "docs")
		if err := os.Mkdir(docs, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(docs, "usage.md"), "# usage\n")
		got := similarExistingPaths(dir, filepath.Join("dock", "usage.md"))
		want := filepath.Join(docs, "usage.md")
		if len(got) == 0 || got[0] != want {
			t.Fatalf("suggestions = %v, want first %s", got, want)
		}
	})

	t.Run("nothing similar", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "readme.md"), "# readme\n")
		if got := similarExistingPaths(dir, "zzzzz-qqqqq.txt"); len(got) != 0 {
			t.Fatalf("suggestions = %v, want none", got)
		}
	})

	t.Run("capped at three", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"config.yaml", "config.yml", "config.json", "confignew.yaml", "config-old.yaml"} {
			mustWrite(t, filepath.Join(dir, name), "x\n")
		}
		if got := similarExistingPaths(dir, "config.toml"); len(got) != pathSuggestMaxResults {
			t.Fatalf("suggestions = %v, want %d", got, pathSuggestMaxResults)
		}
	})

	t.Run("missing parent directory yields nothing", func(t *testing.T) {
		dir := t.TempDir()
		if got := similarExistingPaths(dir, filepath.Join("nope", "file.txt")); len(got) != 0 {
			t.Fatalf("suggestions = %v, want none", got)
		}
	})
}

func TestSimilarExistingPathsFindsBoundedRecursiveCandidate(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "internal", "tools")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nested, "updatework.go")
	if err := os.WriteFile(want, []byte("package tools\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := similarExistingPaths(dir, "update-work.go")
	if !slices.Contains(got, want) {
		t.Fatalf("recursive suggestions = %v, want %s", got, want)
	}
}

// A mistyped read_file path (through any alias) names similar existing paths so
// the model can retarget without an exploratory list_dir.
func TestReadFileNotFoundSuggestsSimilarPaths(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "usage.md"), "# usage\n")
	missing := filepath.Join(dir, "ussage.md")

	for _, field := range []string{"path", "file_path", "filePath", "file", "filename", "filepath", "absolute_path", "target_file"} {
		t.Run(field, func(t *testing.T) {
			_, err := runReadFile(t, map[string]any{field: missing})
			if err == nil {
				t.Fatal("expected not-found error")
			}
			if !strings.Contains(err.Error(), "no such file or directory") {
				t.Errorf("error should keep the ENOENT text: %v", err)
			}
			if !strings.Contains(err.Error(), "similar existing paths: ") || !strings.Contains(err.Error(), "usage.md") {
				t.Errorf("error should suggest usage.md: %v", err)
			}
		})
	}
}

func TestReadFileNotFoundWithoutSimilarPaths(t *testing.T) {
	dir := t.TempDir()
	_, err := runReadFile(t, map[string]any{"path": filepath.Join(dir, "missing.txt")})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if strings.Contains(err.Error(), "similar existing paths") {
		t.Errorf("empty directory should not produce suggestions: %v", err)
	}
}

func TestReadManyFilesInlineErrorSuggestsSimilarPaths(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "usage.md")
	mustWrite(t, present, "# usage\n")

	out, err := runReadFile(t, map[string]any{"paths": []string{present, filepath.Join(dir, "ussage.md")}})
	if err != nil {
		t.Fatalf("batch read should not fail wholesale: %v", err)
	}
	if !strings.Contains(out, "error: ") || !strings.Contains(out, "similar existing paths: ") || !strings.Contains(out, "usage.md") {
		t.Errorf("inline per-file error should suggest usage.md:\n%s", out)
	}
}
