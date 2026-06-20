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
	// Unpadded "<n>\t<line>" form (no fixed-width column).
	want := "1\talpha\n2\tbeta\n3\tgamma"
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
	// Line numbers reflect the true line position, not the window position;
	// lines 5-10 follow the window, so the read is truncated (r14).
	want := "3\tL3\n4\tL4\n[file truncated at line 4; continue with offset=5]"
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

// r36: paths[] reads several files in one call, each under a "==> path <=="
// header.
func TestReadFileMultiplePaths(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	mustWrite(t, a, "alpha\nbeta\n")
	mustWrite(t, b, "one\n")

	out, err := runReadFile(t, map[string]any{"paths": []string{a, b}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "==> " + a + " <==\n1\talpha\n2\tbeta\n\n==> " + b + " <==\n1\tone"
	if out != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}
}

// A per-file error must appear inline so one bad path does not abort the batch.
func TestReadFileMultiplePathsInlineError(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	missing := filepath.Join(dir, "nope.txt")
	mustWrite(t, a, "alpha\n")

	out, err := runReadFile(t, map[string]any{"paths": []string{a, missing}})
	if err != nil {
		t.Fatalf("batch read should not fail on one missing file: %v", err)
	}
	if !strings.Contains(out, "==> "+a+" <==\n1\talpha") {
		t.Errorf("first file missing from output: %q", out)
	}
	if !strings.Contains(out, "==> "+missing+" <==\nerror:") {
		t.Errorf("missing file should report an inline error: %q", out)
	}
}

// With paths and no explicit limit, each file gets a divided budget so later
// files are not dropped by the dispatch cap; the truncation notice fires per file.
func TestReadFileMultiplePathsPerFileBudget(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for f := 0; f < 4; f++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", f))
		var b strings.Builder
		for i := 1; i <= 400; i++ {
			fmt.Fprintf(&b, "L%d\n", i)
		}
		mustWrite(t, p, b.String())
		paths = append(paths, p)
	}
	out, err := runReadFile(t, map[string]any{"paths": paths})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1000-line default / 4 files = 250 lines budget each; every file is present
	// and individually truncated.
	if got := strings.Count(out, "==> "); got != 4 {
		t.Errorf("expected 4 file headers, got %d", got)
	}
	if got := strings.Count(out, "[file truncated at line 250;"); got != 4 {
		t.Errorf("expected each file truncated at line 250, got %d notices:\n%s", got, out)
	}
}

func TestPerPathLineBudget(t *testing.T) {
	tests := []struct {
		explicit, numPaths, def, want int
	}{
		{explicit: 0, numPaths: 1, def: 1000, want: 1000},
		{explicit: 0, numPaths: 4, def: 1000, want: 250},
		{explicit: 0, numPaths: 100, def: 1000, want: multiReadMinPerPathLimit}, // floor
		{explicit: 30, numPaths: 5, def: 1000, want: 30},                        // explicit wins
	}
	for _, tt := range tests {
		if got := perPathLineBudget(tt.explicit, tt.numPaths, tt.def); got != tt.want {
			t.Errorf("perPathLineBudget(%d,%d,%d) = %d, want %d", tt.explicit, tt.numPaths, tt.def, got, tt.want)
		}
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
	// 1000 numbered lines plus a trailing truncation notice (see r14).
	if len(lines) != 1001 {
		t.Errorf("default cap should yield 1000 lines + notice, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "1\tline 1") {
		t.Errorf("first line wrong: %q", lines[0])
	}
	if want := "[file truncated at line 1000; continue with offset=1001]"; lines[len(lines)-1] != want {
		t.Errorf("missing truncation notice; got last line %q", lines[len(lines)-1])
	}
}

// r14: a windowed read that does not reach EOF must announce truncation so the
// model does not treat the last returned line as end-of-file.
func TestReadFileTruncationNotice(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p, "offset": 2, "limit": 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "[file truncated at line 4; continue with offset=5]"; !strings.HasSuffix(out, want) {
		t.Errorf("expected truncation notice %q at end of:\n%q", want, out)
	}
}

// A read that reaches EOF (window covers the rest of the file) must NOT emit a
// truncation notice.
func TestReadFileNoTruncationNoticeAtEOF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Exact-fit window (3 of 3 lines) and over-large window both reach EOF.
	for _, limit := range []int{3, 10} {
		out, err := runReadFile(t, map[string]any{"path": p, "limit": limit})
		if err != nil {
			t.Fatalf("limit %d: unexpected error: %v", limit, err)
		}
		if strings.Contains(out, "file truncated") {
			t.Errorf("limit %d: unexpected truncation notice: %q", limit, out)
		}
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
	// 2 window lines plus the r14 truncation notice (the file continues far past).
	if len(got) != 3 {
		t.Fatalf("want 2 lines + notice, got %d: %q", len(got), out)
	}
	if !strings.HasPrefix(got[0], "2\t") || !strings.HasPrefix(got[1], "3\t") {
		t.Errorf("wrong window lines: %q", out)
	}
	if !strings.HasPrefix(got[2], "[file truncated at line 3;") {
		t.Errorf("missing truncation notice: %q", got[2])
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
	lines, total, truncated, err := readWindowLines(cr, 1, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total < 3 {
		t.Fatalf("expected at least 3 lines counted, got %d", total)
	}
	if !truncated {
		t.Errorf("a 3-line window of a 1000-line file should report truncated=true")
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
	if out != "1\tx" {
		t.Errorf("got %q", out)
	}
}
