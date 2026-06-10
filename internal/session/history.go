package session

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HistoryPath returns the default REPL history file path for the given state
// directory: <stateDir>/harness/history. Empty stateDir yields "".
func HistoryPath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "harness", "history")
}

// LoadHistory reads path and returns up to memSize deduplicated entries in
// chronological order (oldest first). Blanks and lines containing \r or \n are
// dropped; duplicates keep the last occurrence.
//
// Semantics mirror bash HISTFILESIZE/HISTSIZE:
//   - fileSize is the on-disk cap; when the kept set exceeds it, the file is
//     atomically rewritten with its last fileSize entries (self-healing trim).
//   - memSize is the in-memory cap; the returned slice is at most memSize of
//     the (possibly trimmed) kept set, taken from the tail.
//
// Missing files return (nil, nil). An empty path, or both sizes <= 0, also
// returns (nil, nil) without touching the filesystem. When fileSize <= 0 the
// file is not loaded at all — persistence is disabled.
func LoadHistory(path string, fileSize, memSize int) ([]string, error) {
	if path == "" || fileSize <= 0 {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Scan line-by-line, dropping invalid entries.
	var raw []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !historyLineValid(line) {
			continue
		}
		raw = append(raw, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: history scan: %w", err)
	}
	// Close before potentially rewriting so rename is safe on all platforms.
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("session: history close: %w", err)
	}

	// Dedupe keeping the last occurrence of each entry.
	kept := dedupeLast(raw)

	// Apply HISTFILESIZE cap: rewrite the file with its last fileSize entries if
	// it exceeded the limit. Rewriting also reconciles duplicates and dropped
	// invalid lines, so the file always reflects the loaded set.
	if len(kept) > fileSize {
		kept = kept[len(kept)-fileSize:]
	}
	if needsRewrite(raw, kept) {
		if err := rewriteHistory(path, kept); err != nil {
			return nil, err
		}
	}

	// Apply HISTSIZE cap: return the last memSize entries.
	if memSize <= 0 {
		return nil, nil
	}
	if len(kept) > memSize {
		kept = kept[len(kept)-memSize:]
	}
	return kept, nil
}

// AppendHistory appends entry as a single line to the history file at path,
// creating it and its parent directories if needed. It uses O_APPEND for
// POSIX atomicity on short writes so concurrent sessions can share a file
// without corruption.
//
// Entries containing \r or \n and blank/whitespace-only entries are ignored.
// An empty path is a no-op.
func AppendHistory(path, entry string) error {
	if path == "" {
		return nil
	}
	if !historyLineValid(entry) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("session: history dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("session: history open: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, entry); err != nil {
		return fmt.Errorf("session: history write: %w", err)
	}
	return nil
}

func historyLineValid(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	if strings.ContainsAny(line, "\r\n") {
		return false
	}
	return true
}

// dedupeLast returns a copy of in with duplicates removed, keeping for each
// distinct string its last occurrence. The returned order matches in.
func dedupeLast(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		if _, dup := seen[in[i]]; dup {
			continue
		}
		seen[in[i]] = struct{}{}
		out = append(out, in[i])
	}
	// Reverse to restore original chronological order.
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// needsRewrite reports whether writing kept to path would differ from raw.
// It's true whenever dropped invalid lines, duplicates, or a length trim
// occurred during Load.
func needsRewrite(raw, kept []string) bool {
	if len(raw) != len(kept) {
		return true
	}
	for i, s := range raw {
		if s != kept[i] {
			return true
		}
	}
	return false
}

// rewriteHistory atomically replaces path with kept joined by newlines. Parents
// must already exist (LoadHistory is only called after a successful Open).
func rewriteHistory(path string, kept []string) error {
	var b strings.Builder
	b.Grow(len(kept) * 32)
	for _, s := range kept {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("session: history rewrite temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: history rewrite rename: %w", err)
	}
	return nil
}
