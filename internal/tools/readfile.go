package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// binarySniffBytes is how many leading bytes are scanned for NUL to classify a
// file as binary (design §9.1).
const binarySniffBytes = 8 * 1024

// defaultReadFileLimit is the default number of lines returned (design §9.1).
const defaultReadFileLimit = 1000

// multiReadMinPerPathLimit floors the per-file budget in a multi-file read so
// each file still shows a useful head even when many paths share the line cap.
const multiReadMinPerPathLimit = 50

const readFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path to read. Provide path or paths."},
    "paths": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Multiple files to read in one call, each under a \"==> path <==\" header with its own per-file line budget. Use instead of path to batch reads during orientation; offset is ignored in this mode."
    },
    "offset": {"type": "integer", "description": "1-based starting line (single-path reads only)."},
    "limit": {"type": "integer", "description": "Maximum number of lines (default 1000); with paths it is the per-file budget."}
  }
}`

type readFile struct {
	defaultLimit int
}

func (readFile) Name() string { return "read_file" }

func (readFile) Description() string {
	return "Read a file from disk. Provide a JSON object with path (single file; supports offset/limit), or paths[] to read several files at once, each under a \"==> path <==\" header. Returns line-numbered content."
}

func (readFile) Schema() json.RawMessage { return json.RawMessage(readFileSchema) }

func (readFile) ReadOnly(json.RawMessage) bool { return true }

func (r readFile) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path   string   `json:"path"`
		Paths  []string `json:"paths"`
		Offset int      `json:"offset"`
		Limit  int      `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if args.Offset < 0 {
		return "", badArgs("offset must be >= 1")
	}
	if args.Limit < 0 {
		return "", badArgs("limit must be >= 0")
	}

	defaultLimit := r.defaultLimit
	if defaultLimit == 0 {
		defaultLimit = defaultReadFileLimit
	}

	if len(args.Paths) > 0 {
		return readManyFiles(args.Paths, args.Limit, defaultLimit)
	}

	if args.Path == "" {
		return "", badArgs("path or paths is required")
	}
	offset := args.Offset
	if offset == 0 {
		offset = 1
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	return readOneFile(args.Path, offset, limit)
}

// readManyFiles reads each path from line 1 under its own "==> path <==" header,
// applying a per-file line budget so the central dispatch cap does not truncate
// later files. A per-file read error is reported inline so one bad path does not
// waste the whole batch.
func readManyFiles(paths []string, explicitLimit, defaultLimit int) (string, error) {
	for i, p := range paths {
		if strings.TrimSpace(p) == "" {
			return "", badArgs("paths[%d] must not be empty", i)
		}
	}
	perPath := perPathLineBudget(explicitLimit, len(paths), defaultLimit)

	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "==> %s <==\n", p)
		body, err := readOneFile(p, 1, perPath)
		if err != nil {
			fmt.Fprintf(&b, "error: %s", err.Error())
			continue
		}
		b.WriteString(body)
	}
	return b.String(), nil
}

// perPathLineBudget chooses the line budget for each file in a multi-file read.
// An explicit limit is honored as-is; otherwise the default budget is divided
// across the paths so the combined output stays near the dispatch line cap and
// later files are not dropped, with a floor so each file still shows a head.
func perPathLineBudget(explicitLimit, numPaths, defaultLimit int) int {
	if explicitLimit > 0 {
		return explicitLimit
	}
	if numPaths <= 1 {
		return defaultLimit
	}
	return max(defaultLimit/numPaths, multiReadMinPerPathLimit)
}

// readOneFile reads the [offset, offset+limit) window of a single file and
// returns its line-numbered body, including the truncation notice (r14) when the
// file continues past the window. It is shared by the single- and multi-path
// read paths.
func readOneFile(path string, offset, limit int) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_dir", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Buffer must hold the full sniff window; bufio.NewReader's default 4096-byte
	// buffer would make Peek(binarySniffBytes) return only 4096 bytes and miss a
	// NUL deeper in the head.
	br := bufio.NewReaderSize(f, binarySniffBytes)
	head, _ := br.Peek(binarySniffBytes)
	if bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Errorf("%s appears to be binary", path)
	}

	// Always read line-by-line and stop after the window so a small window (or
	// the default line cap) of a huge file never loads the whole thing
	// into memory. This subsumes the design's >10MB guard: an unwindowed read
	// returns at most the configured default limit regardless of file size.
	lines, total, truncated, err := readWindowLines(br, offset, limit)
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "(empty file)", nil
	}
	if offset > total {
		return "", fmt.Errorf("offset %d is past end of file (%s has %d lines)", offset, path, total)
	}
	out := numberLines(lines, offset)
	if truncated {
		// Mirror list_dir's truncation notice so the model knows line N is not EOF
		// and can resume from the next line instead of assuming it read the whole file.
		last := offset + len(lines) - 1
		out += fmt.Sprintf("\n[file truncated at line %d; continue with offset=%d]", last, last+1)
	}
	return out, nil
}

// numberLines renders lines as "<n>\t<line>"; startLine is the 1-based number of
// the first line. The number is emitted with no column padding: the model parses
// the integer, not its alignment, and a fixed-width pad wastes ~3-5KB on a
// default 1000-line read.
func numberLines(lines []string, startLine int) string {
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(startLine + i))
		b.WriteByte('\t')
		b.WriteString(ln)
	}
	return b.String()
}

// readWindowLines streams r line by line, returning the lines in
// [offset, offset+limit), the count of lines seen, and whether the file
// continues past the window (so the caller can flag truncation). It stops as
// soon as the window is fully collected — after peeking one byte to detect
// trailing content — and reads to EOF only when the window starts past the end
// of input (so the caller can report the true line count). Memory use is bounded
// by the window size and the longest line, never the whole file.
func readWindowLines(r io.Reader, offset, limit int) ([]string, int, bool, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	var window []string
	lineno := 0
	end := offset + limit // first line number past the window (1-based exclusive)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 || err == nil {
			lineno++
			if lineno >= offset && lineno < end {
				window = append(window, strings.TrimSuffix(line, "\n"))
			}
			// Stop once the window is filled. Peek one byte (without consuming) to
			// learn whether more content follows, so Run can emit a truncation notice.
			if lineno >= end-1 && len(window) == limit {
				_, peekErr := br.Peek(1)
				return window, lineno, peekErr == nil, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return window, lineno, false, nil
			}
			return nil, lineno, false, err
		}
	}
}
