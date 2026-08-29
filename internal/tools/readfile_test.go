package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileReportsCanonicalAndAliasPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{name: "single", input: `{"path":"a.txt"}`, want: []string{"a.txt"}},
		{name: "single alias", input: `{"file_path":"alias.txt"}`, want: []string{"alias.txt"}},
		{name: "canonical wins", input: `{"path":"canonical.txt","file_path":"alias.txt"}`, want: []string{"canonical.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (readFile{}).ReadPaths([]byte(tc.input))
			if err != nil {
				t.Fatalf("ReadPaths: %v", err)
			}
			if len(got) != 1 || got[0] != tc.want[0] {
				t.Fatalf("ReadPaths = %v, want %v", got, tc.want)
			}
		})
	}
}

func runReadFile(t *testing.T, args map[string]any) (string, error) {
	return runTool(t, readFile{}, args)
}

func TestReadFileSchemaOnlyAdvertisesSingularPath(t *testing.T) {
	if strings.Contains(readFileSchema, `"paths"`) || strings.Contains(readFileSchema, `"files"`) {
		t.Fatalf("read schema advertises removed multi-path input: %s", readFileSchema)
	}
	if !strings.Contains(readFileSchema, `"required": ["path"]`) {
		t.Fatalf("read schema does not require singular path: %s", readFileSchema)
	}
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

func TestReadFileOptionalSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	content := "alpha\nbeta\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p, "include_sha256": true})
	if err != nil {
		t.Fatalf("read with digest: %v", err)
	}
	wantPrefix := "[sha256:" + sha256Hex([]byte(content)) + "]\n"
	if !strings.HasPrefix(out, wantPrefix) || !strings.Contains(out, "1\talpha") {
		t.Fatalf("digest read = %q, want prefix %q and numbered content", out, wantPrefix)
	}
}

func TestReadFileSHA256IsOptIn(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sha256:") {
		t.Fatalf("ordinary read unexpectedly hashed the whole file: %q", out)
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
	want := "3\tL3\n4\tL4\n[file truncated at line 4 of 10; file size 31 bytes; continue with offset=5]"
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
	mustWrite(t, filepath.Join(dir, "a.txt"), "a")
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[directory listing: " + dir + "]", "nested/", "a.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("directory listing missing %q: %s", want, out)
		}
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
	// 1000 numbered lines plus a trailing truncation notice (see r14).
	if len(lines) != 1001 {
		t.Errorf("default cap should yield 1000 lines + notice, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "1\tline 1") {
		t.Errorf("first line wrong: %q", lines[0])
	}
	if want := fmt.Sprintf("[file truncated at line 1000 of 1500; file size %d bytes; continue with offset=1001]", b.Len()); lines[len(lines)-1] != want {
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
	if want := "[file truncated at line 4 of 10; file size 31 bytes; continue with offset=5]"; !strings.HasSuffix(out, want) {
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
	if !strings.HasPrefix(got[2], "[file truncated at line 3; file size 12000000 bytes;") {
		t.Errorf("missing truncation notice: %q", got[2])
	}
	if strings.Contains(got[2], " of 12000;") {
		t.Errorf("large-file notice unexpectedly counted every line: %q", got[2])
	}
}

func TestReadFileTruncationNoticeCountsUnterminatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": p, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[file truncated at line 1 of 3; file size 5 bytes; continue with offset=2]"; !strings.HasSuffix(out, want) {
		t.Fatalf("truncation notice = %q, want suffix %q", out, want)
	}
}

func TestReadFileTotalLineThresholdIsConfigurable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, readFile{totalLinesMaxBytes: 1}, map[string]any{"path": p, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[file truncated at line 1; file size 6 bytes; continue with offset=2]"; !strings.HasSuffix(out, want) {
		t.Fatalf("truncation notice = %q, want suffix %q", out, want)
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

func TestReadWindowLinesBoundedDoesNotRetainOversizedPhysicalLine(t *testing.T) {
	const oversized = 2 * 1024 * 1024
	source := io.MultiReader(
		&repeatedByteReader{remaining: oversized, value: 'x'},
		strings.NewReader("\nsecond\n"),
	)
	lines, total, truncated, budgetLimited, err := readWindowLinesBounded(context.Background(), source, 1, 2, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 || total != 1 || !truncated || !budgetLimited {
		t.Fatalf("bounded oversized read = lines %d, total %d, truncated %v, budget-limited %v", len(lines), total, truncated, budgetLimited)
	}
}

type repeatedByteReader struct {
	remaining int
	value     byte
}

func (r *repeatedByteReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = r.value
	}
	r.remaining -= n
	return n, nil
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

func TestReadPaginationNoticeFormatsAndParsesUnknownSize(t *testing.T) {
	marker := (readPaginationNotice{}).format(3)
	want := "[file truncated at line 3; file size unknown; continue with offset=4]"
	if marker != want {
		t.Fatalf("unknown-size marker = %q, want %q", marker, want)
	}
	parsed, ok := parseReadPaginationNotice(marker)
	if !ok || parsed.fileSizeKnown || parsed.fileSize != 0 || parsed.totalLines != 0 {
		t.Fatalf("parsed unknown-size marker = %+v, %v", parsed, ok)
	}
}

func TestBoundedRegularFileReaderDoesNotTrustZeroStatSizeAsEOF(t *testing.T) {
	source, snapshot := boundedRegularFileReader(strings.NewReader("virtual content\n"), true, 0)
	if snapshot != nil {
		t.Fatal("zero-size regular reader unexpectedly received a snapshot boundary")
	}
	got, err := io.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "virtual content\n" {
		t.Fatalf("zero-size regular reader returned %q", got)
	}

	source, snapshot = boundedRegularFileReader(strings.NewReader("abcdef"), true, 3)
	if snapshot == nil {
		t.Fatal("positive-size regular reader did not receive a snapshot boundary")
	}
	got, err = io.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abc" || snapshot.N != 0 {
		t.Fatalf("bounded regular reader returned %q with %d bytes remaining, want abc/0", got, snapshot.N)
	}
}

func TestCountRemainingLinesHonorsReaderBoundAndCancellation(t *testing.T) {
	source := strings.NewReader("b\nc\nextra\n")
	limited := &io.LimitedReader{R: source, N: 4}
	got, err := countRemainingLines(context.Background(), limited)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 || limited.N != 0 {
		t.Fatalf("bounded line count = %d with %d bytes remaining, want 2/0", got, limited.N)
	}
	if rest, err := io.ReadAll(source); err != nil || string(rest) != "extra\n" {
		t.Fatalf("source remainder = %q, %v; want %q", rest, err, "extra\\n")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &countingReader{r: strings.NewReader("ignored\n")}
	if _, err := countRemainingLines(ctx, reader); err != context.Canceled {
		t.Fatalf("cancelled count error = %v, want context.Canceled", err)
	}
	if reader.n != 0 {
		t.Fatalf("cancelled count read %d bytes, want 0", reader.n)
	}
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

// Models trained on other harnesses sometimes name the path parameter
// differently (Claude Code/Gemini file_path, opencode filePath, Cursor
// target_file, etc.). Each accepted alias must resolve to a single-file read so
// the call succeeds instead of erroring with "path or paths is required".
func TestReadFilePathAliases(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("alpha\n"), 0644); err != nil {
		t.Fatal(err)
	}
	aliases := []string{"file", "file_path", "filePath", "filename", "filepath", "absolute_path", "target_file"}
	for _, key := range aliases {
		t.Run(key, func(t *testing.T) {
			out, err := runReadFile(t, map[string]any{key: p})
			if err != nil {
				t.Fatalf("alias %q should read the file: %v", key, err)
			}
			if out != "1\talpha" {
				t.Errorf("alias %q got %q", key, out)
			}
		})
	}
}

// The canonical `path` must win when both it and an alias are supplied, so a
// model that sends both does not read the wrong file.
func TestReadFileCanonicalPathBeatsAlias(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("real\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"path": real, "file": filepath.Join(dir, "nope.txt")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "1\treal" {
		t.Errorf("path should win over alias, got %q", out)
	}
}

// When only aliases are present they resolve in a fixed precedence order
// (file_path before file), so a mix of names is deterministic.
func TestReadFileAliasPrecedence(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "want.txt")
	if err := os.WriteFile(want, []byte("want\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(t, map[string]any{"file_path": want, "target_file": filepath.Join(dir, "nope.txt")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "1\twant" {
		t.Errorf("file_path should win over target_file, got %q", out)
	}
}

func TestReadFileRejectsRemovedMultiPathArguments(t *testing.T) {
	for _, args := range []map[string]any{
		{"paths": []string{"a.txt", "b.txt"}},
		{"files": []string{"a.txt", "b.txt"}},
	} {
		_, err := runReadFile(t, args)
		if err == nil || !strings.Contains(err.Error(), "path is required") {
			t.Errorf("removed multi-path input error = %v", err)
		}
	}
}

// With neither path nor any singular alias, the required-arg error still fires.
func TestReadFileNoPathError(t *testing.T) {
	_, err := runReadFile(t, map[string]any{"limit": 10})
	if err == nil {
		t.Fatal("expected error when no path or alias is given")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("unexpected error: %v", err)
	}
}
