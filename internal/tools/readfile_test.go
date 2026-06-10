package tools

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runReadFile(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, readFile{}, args)
}

func TestReadFileNumbering(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// cat -n style: right-aligned number, tab, line.
	want := "     1\talpha\n     2\tbeta\n     3\tgamma"
	if out != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p, "offset": 3, "limit": 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Line numbers reflect the true line position, not the window position.
	want := "     3\tL3\n     4\tL4"
	if out != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}
}

func TestReadFileOffsetPastEOF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runReadFile(t, map[string]any{"path": p, "offset": 99})
	if err == nil {
		t.Fatal("expected error for offset past EOF")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error should state the file's line count (3): %v", err)
	}
}

func TestReadFileMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := runReadFile(t, map[string]any{"path": filepath.Join(dir, "nope.txt")})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadFileDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := runReadFile(t, map[string]any{"path": dir})
	if err == nil {
		t.Fatal("expected error for directory")
	}
	if !strings.Contains(err.Error(), "use list_dir") {
		t.Errorf("directory error should direct to list_dir: %v", err)
	}
}

func TestReadFileBinary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bin")
	if err := os.WriteFile(p, []byte("text\x00more"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runReadFile(t, map[string]any{"path": p})
	if err == nil {
		t.Fatal("expected binary rejection")
	}
	if !strings.Contains(err.Error(), "appears to be binary") {
		t.Errorf("binary error text wrong: %v", err)
	}
}

// Regression: the NUL sniff must scan the full 8KB head (design §9.1), not just
// the first 4KB. A NUL at byte 6000 (no earlier NUL) lies past bufio.Reader's
// default 4096-byte buffer, so Peek(8192) would return only 4096 bytes and the
// file would be misclassified as text (review issue: readfile.go).
func TestReadFileBinaryDeepNUL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deep.bin")
	buf := make([]byte, 8000)
	for i := range buf {
		buf[i] = 'a'
	}
	buf[6000] = 0
	if err := os.WriteFile(p, buf, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runReadFile(t, map[string]any{"path": p})
	if err == nil {
		t.Fatal("expected binary rejection for NUL at byte 6000")
	}
	if !strings.Contains(err.Error(), "appears to be binary") {
		t.Errorf("binary error text wrong: %v", err)
	}
}

func TestReadFileEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(p, nil, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "(empty file)" {
		t.Errorf("empty file marker wrong: %q", out)
	}
}

func TestReadFileMissingPathArg(t *testing.T) {
	_, err := runReadFile(t, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path arg")
	}
}

func TestReadFileDefaultLineCap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	var b strings.Builder
	for i := 1; i <= 1500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 1000 {
		t.Errorf("default cap should yield 1000 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "     1\tline 1") {
		t.Errorf("first line wrong: %q", lines[0])
	}
}

// Regression: a windowed read (offset/limit set) must not load the whole file
// into memory. Previously the windowed path used io.ReadAll regardless of size,
// so a 2-line window of a multi-GB file would OOM (review issue: readfile.go).
// We verify the window is read line-bounded by reading the first 2 lines of a
// file larger than the non-windowed >10MB guard and confirming only those lines
// come back (the whole-file read would still be correct here, so we also assert
// the read stops early via the bounded helper below).
func TestReadFileWindowedLargeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "large.txt")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// ~12MB: 12000 lines of 1000 'x' chars, exceeding readFileMaxBytes.
	line := strings.Repeat("x", 999) + "\n"
	w := bufio.NewWriter(f)
	for i := 0; i < 12000; i++ {
		if _, err := w.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runReadFile(t, map[string]any{"path": p, "offset": 2, "limit": 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.Split(out, "\n")
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(got), out)
	}
	if !strings.HasPrefix(got[0], "     2\t") || !strings.HasPrefix(got[1], "     3\t") {
		t.Errorf("wrong window lines: %q", out)
	}
}

// readWindowLines must not consume the whole reader: after reading the
// requested window it should stop, leaving later bytes unread. We assert this
// by giving it a reader that records how far it advanced.
func TestReadWindowLinesStopsEarly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "w.txt")
	var b strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	full := b.String()
	if err := os.WriteFile(p, []byte(full), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cr := &countingReader{r: f}
	lines, total, err := readWindowLines(cr, 1, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total < 3 {
		t.Fatalf("expected at least 3 lines counted, got %d", total)
	}
	if len(lines) != 3 || lines[0] != "L1" || lines[2] != "L3" {
		t.Errorf("wrong window: %v", lines)
	}
	// Reading a 3-line window must not have pulled the whole ~5KB file.
	if cr.n >= len(full) {
		t.Errorf("read consumed entire file (%d of %d bytes); window read is unbounded", cr.n, len(full))
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadFileUnknownArgsTolerated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p, "bogus": 1})
	if err != nil {
		t.Fatalf("unknown key should be tolerated: %v", err)
	}
	if out != "     1\tx" {
		t.Errorf("got %q", out)
	}
}
