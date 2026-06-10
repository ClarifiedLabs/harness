package lspproxy

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// uriForPath converts an absolute filesystem path to a file:// URI, percent-
// encoding as needed. LSP identifies documents by URI.
func uriForPath(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// uriToPath converts a file:// URI back to a filesystem path (percent-decoded),
// used to read a snippet from a result location. A non-file or malformed URI
// yields the empty string.
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

// workspaceName is the display name for a workspace root (its base directory).
func workspaceName(root string) string {
	return filepath.Base(root)
}

// extLanguage maps a file extension (lowercase, including the leading dot) to
// the LSP language id sent in textDocument/didOpen. It covers the shipped
// default servers; a server config can claim extra extensions via its
// Extensions field, handled in the Manager's selection logic.
var extLanguage = map[string]string{
	".go":  "go",
	".rs":  "rust",
	".py":  "python",
	".pyi": "python",
	".ts":  "typescript",
	".mts": "typescript",
	".cts": "typescript",
	".tsx": "typescriptreact",
	".js":  "javascript",
	".mjs": "javascript",
	".cjs": "javascript",
	".jsx": "javascriptreact",
	".c":   "c",
	".h":   "c",
	".cc":  "cpp",
	".cpp": "cpp",
	".cxx": "cpp",
	".hpp": "cpp",
	".hh":  "cpp",
}

// languageForExt returns the built-in language id for a file extension (the
// leading-dot form from filepath.Ext), case-insensitively.
func languageForExt(ext string) (string, bool) {
	lang, ok := extLanguage[strings.ToLower(ext)]
	return lang, ok
}

// detectRoot finds the workspace root for a file living in fileDir. Markers are
// tried in order (highest priority first): for each marker it returns the
// nearest ancestor directory (fileDir upward) that contains it, so a
// higher-priority marker far up the tree (e.g. go.work) wins over a
// lower-priority one nearer the file (e.g. go.mod). When no marker is found
// anywhere, it falls back to fileDir (single-file mode) with found=false.
func detectRoot(fileDir string, markers []string) (string, bool) {
	for _, marker := range markers {
		for dir := fileDir; ; {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break // reached the filesystem root
			}
			dir = parent
		}
	}
	return fileDir, false
}

// utf16Len returns the number of UTF-16 code units in s. LSP positions are
// UTF-16 code-unit offsets within a line by default, so this (not byte or rune
// count) is what columns are measured in. A rune above the BMP encodes as a
// surrogate pair (two units).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// symbolColumnUTF16 returns the UTF-16 column of the first occurrence of symbol
// within lineText. It lets the model name a symbol on a line instead of
// computing a fragile column itself; the shim resolves the exact LSP position.
func symbolColumnUTF16(lineText, symbol string) (int, bool) {
	before, _, found := strings.Cut(lineText, symbol)
	if !found {
		return 0, false
	}
	return utf16Len(before), true
}

// runeColToUTF16 converts a 1-based rune column (the escape hatch when no symbol
// is given) to a UTF-16 column. A column past the end clamps to the line's
// UTF-16 length.
func runeColToUTF16(lineText string, oneBasedRuneCol int) int {
	if oneBasedRuneCol <= 1 {
		return 0
	}
	target := oneBasedRuneCol - 1
	runes := 0
	for i := range lineText {
		if runes == target {
			return utf16Len(lineText[:i])
		}
		runes++
	}
	return utf16Len(lineText)
}

// utf16ColToByteOffset converts a UTF-16 column back to a byte offset within
// lineText, used to slice a snippet around an LSP-returned position. A column at
// or past the end clamps to len(lineText).
func utf16ColToByteOffset(lineText string, utf16Col int) int {
	if utf16Col <= 0 {
		return 0
	}
	units := 0
	for i, r := range lineText {
		if units >= utf16Col {
			return i
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return len(lineText)
}
