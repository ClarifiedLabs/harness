package lspproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLanguageForExt(t *testing.T) {
	cases := map[string]string{
		".go":  "go",
		".rs":  "rust",
		".py":  "python",
		".ts":  "typescript",
		".tsx": "typescriptreact",
		".js":  "javascript",
		".jsx": "javascriptreact",
		".c":   "c",
		".h":   "c",
		".cpp": "cpp",
		".hpp": "cpp",
		".GO":  "go", // case-insensitive
	}
	for ext, want := range cases {
		got, ok := languageForExt(ext)
		if !ok || got != want {
			t.Errorf("languageForExt(%q) = (%q, %v), want (%q, true)", ext, got, ok, want)
		}
	}
	if got, ok := languageForExt(".unknownext"); ok {
		t.Errorf("languageForExt(.unknownext) = (%q, true), want not found", got)
	}
}

func TestDetectRootPriorityAndNearest(t *testing.T) {
	root := t.TempDir()
	svc := filepath.Join(root, "svc")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "go.work"))
	mustWrite(t, filepath.Join(svc, "go.mod"))
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// go.work has highest priority: even though go.mod is nearer, the workspace
	// root (where go.work lives) wins.
	if got, ok := detectRoot(svc, []string{"go.work", "go.mod", ".git"}); !ok || got != root {
		t.Fatalf("detectRoot = (%q, %v), want (%q, true)", got, ok, root)
	}
	// With go.mod first, the nearest go.mod (the service dir) wins.
	if got, ok := detectRoot(svc, []string{"go.mod", "go.work", ".git"}); !ok || got != svc {
		t.Fatalf("detectRoot = (%q, %v), want (%q, true)", got, ok, svc)
	}
	// No marker found: single-file fallback to the starting dir.
	if got, ok := detectRoot(svc, []string{"Cargo.toml"}); ok || got != svc {
		t.Fatalf("detectRoot fallback = (%q, %v), want (%q, false)", got, ok, svc)
	}
}

func TestUTF16Len(t *testing.T) {
	cases := map[string]int{
		"":     0,
		"abc":  3,
		"café": 4, // é is one UTF-16 code unit
		"😀":    2, // astral plane: two UTF-16 code units (surrogate pair)
		"a😀b":  4,
	}
	for s, want := range cases {
		if got := utf16Len(s); got != want {
			t.Errorf("utf16Len(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestSymbolColumnUTF16(t *testing.T) {
	// Plain ASCII.
	if got, ok := symbolColumnUTF16("func Foo() {", "Foo"); !ok || got != 5 {
		t.Errorf("ascii: got (%d, %v), want (5, true)", got, ok)
	}
	// Multibyte prefix: é is 2 bytes but 1 UTF-16 unit, so the column is rune/unit
	// based, not byte based.
	if got, ok := symbolColumnUTF16("café x", "x"); !ok || got != 5 {
		t.Errorf("multibyte: got (%d, %v), want (5, true)", got, ok)
	}
	// Astral prefix: emoji is 2 UTF-16 units.
	if got, ok := symbolColumnUTF16("😀x", "x"); !ok || got != 2 {
		t.Errorf("astral: got (%d, %v), want (2, true)", got, ok)
	}
	// Missing symbol.
	if _, ok := symbolColumnUTF16("func Foo()", "Bar"); ok {
		t.Errorf("missing symbol reported found")
	}
}

func TestRuneColToUTF16(t *testing.T) {
	// 1-based rune column → UTF-16 column.
	if got := runeColToUTF16("abc", 1); got != 0 {
		t.Errorf("col 1 = %d, want 0", got)
	}
	if got := runeColToUTF16("abc", 3); got != 2 {
		t.Errorf("col 3 = %d, want 2", got)
	}
	// é is 1 rune / 1 UTF-16 unit; the 'x' after "café" is rune col 5 → UTF-16 4.
	if got := runeColToUTF16("caféx", 5); got != 4 {
		t.Errorf("multibyte col 5 = %d, want 4", got)
	}
	// past the end clamps to the line's UTF-16 length.
	if got := runeColToUTF16("ab", 99); got != 2 {
		t.Errorf("clamp = %d, want 2", got)
	}
}

func TestUTF16ColToByteOffset(t *testing.T) {
	if got := utf16ColToByteOffset("café x", 5); got != 6 {
		t.Errorf("multibyte: got %d, want 6", got)
	}
	if got := utf16ColToByteOffset("😀x", 2); got != 4 {
		t.Errorf("astral: got %d, want 4", got)
	}
	if got := utf16ColToByteOffset("abc", 1); got != 1 {
		t.Errorf("ascii: got %d, want 1", got)
	}
	// Past the end clamps to len.
	if got := utf16ColToByteOffset("ab", 99); got != 2 {
		t.Errorf("clamp: got %d, want 2", got)
	}
}

func TestURIRoundTrip(t *testing.T) {
	cases := map[string]string{
		"/tmp/proj":      "file:///tmp/proj",
		"/tmp/a b/x.go":  "file:///tmp/a%20b/x.go",
		"/home/u/c#/m.c": "file:///home/u/c%23/m.c",
	}
	for path, wantURI := range cases {
		if got := uriForPath(path); got != wantURI {
			t.Errorf("uriForPath(%q) = %q, want %q", path, got, wantURI)
		}
		if got := uriToPath(wantURI); got != path {
			t.Errorf("uriToPath(%q) = %q, want %q", wantURI, got, path)
		}
	}
}

func TestWorkspaceName(t *testing.T) {
	if got := workspaceName("/tmp/proj"); got != "proj" {
		t.Errorf("workspaceName = %q, want proj", got)
	}
}

func TestServerExtensionsUnion(t *testing.T) {
	s := ResolvedServer{
		Name:       "gopls",
		Languages:  []string{"go"},
		Extensions: []string{".gop", "GOP"},
	}
	exts := serverExtensions(s)
	for _, want := range []string{".go", ".gop"} {
		if !exts[want] {
			t.Errorf("missing extension %q in %v", want, exts)
		}
	}
	if exts[".rs"] || exts[".py"] {
		t.Errorf("foreign language extensions leaked into %v", exts)
	}

	// A language with no built-in extensions and no configured extensions is
	// undetectable.
	if got := serverExtensions(ResolvedServer{Name: "x", Languages: []string{"cobol"}}); len(got) != 0 {
		t.Errorf("undetectable server should yield empty set, got %v", got)
	}
}

func TestHasLanguageFiles(t *testing.T) {
	t.Run("matching file found", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(root, "sub", "main.go"))
		if !hasLanguageFiles(root, map[string]bool{".go": true}, 100) {
			t.Fatal("expected match")
		}
	})

	t.Run("no match", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "notes.txt"))
		if hasLanguageFiles(root, map[string]bool{".go": true}, 100) {
			t.Fatal("unexpected match")
		}
	})

	t.Run("cap reached without match", func(t *testing.T) {
		root := t.TempDir()
		// Fill entries up to the cap, then place a match beyond it.
		for i := range 10 {
			mustWrite(t, filepath.Join(root, string(rune('a'+i))+".txt"))
		}
		deep := filepath.Join(root, "deep")
		if err := os.Mkdir(deep, 0o700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(deep, "late.go"))
		if hasLanguageFiles(root, map[string]bool{".go": true}, 5) {
			t.Fatal("inconclusive scan must not report a match")
		}
	})

	t.Run("noise dirs skipped", func(t *testing.T) {
		root := t.TempDir()
		for _, dir := range []string{".git", "node_modules"} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(root, dir, "pkg.go"))
		}
		if hasLanguageFiles(root, map[string]bool{".go": true}, 100) {
			t.Fatal("matches inside noise dirs must be ignored")
		}
	})

	t.Run("configured extension matches", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "module.zig"))
		if !hasLanguageFiles(root, map[string]bool{".zig": true}, 100) {
			t.Fatal("expected configured-extension match")
		}
	})

	t.Run("case-insensitive extension", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "MAIN.GO"))
		if !hasLanguageFiles(root, map[string]bool{".go": true}, 100) {
			t.Fatal("expected case-insensitive match")
		}
	})

	t.Run("empty extension set", func(t *testing.T) {
		if hasLanguageFiles(t.TempDir(), nil, 100) {
			t.Fatal("empty extension set must not match")
		}
	})
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}
