package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// binarySniffBytes is how many leading bytes are scanned for NUL to classify a
// file as binary (design §9.1).
const binarySniffBytes = 8 * 1024

// defaultReadFileLimit is the default number of lines returned (design §9.1).
const defaultReadFileLimit = 1000

const readFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path to read."},
    "offset": {"type": "integer", "description": "1-based starting line."},
    "limit": {"type": "integer", "description": "Maximum number of lines (default 1000)."}
  },
  "required": ["path"]
}`

type readFile struct {
	defaultLimit int
}

func (readFile) Name() string { return "read_file" }

func (readFile) Description() string {
	return "Read a file from disk. Returns line-numbered content; supports offset/limit for large files."
}

func (readFile) Schema() json.RawMessage { return json.RawMessage(readFileSchema) }

func (readFile) ReadOnly(json.RawMessage) bool { return true }

func (r readFile) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", badArgs("path is required")
	}
	if args.Offset < 0 {
		return "", badArgs("offset must be >= 1")
	}
	if args.Limit < 0 {
		return "", badArgs("limit must be >= 0")
	}

	info, err := os.Stat(args.Path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_dir", args.Path)
	}

	f, err := os.Open(args.Path)
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
		return "", fmt.Errorf("%s appears to be binary", args.Path)
	}

	offset := args.Offset
	if offset == 0 {
		offset = 1
	}
	limit := args.Limit
	if limit == 0 {
		limit = r.defaultLimit
		if limit == 0 {
			limit = defaultReadFileLimit
		}
	}

	// Always read line-by-line and stop after the window so a small window (or
	// the default line cap) of a huge file never loads the whole thing
	// into memory. This subsumes the design's >10MB guard: an unwindowed read
	// returns at most the configured default limit regardless of file size.
	lines, total, err := readWindowLines(br, offset, limit)
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "(empty file)", nil
	}
	if offset > total {
		return "", fmt.Errorf("offset %d is past end of file (%s has %d lines)", offset, args.Path, total)
	}
	return numberLines(lines, offset), nil
}

// numberLines renders lines in cat -n style; startLine is the 1-based number of
// the first line.
func numberLines(lines []string, startLine int) string {
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%6d\t%s", startLine+i, ln)
	}
	return b.String()
}

// readWindowLines streams r line by line, returning the lines in
// [offset, offset+limit) and the count of lines seen. It stops as soon as the
// window is fully collected; it reads to EOF only when the window starts past
// the end of input (so the caller can report the true line count). Memory use
// is bounded by the window size and the longest line, never the whole file.
func readWindowLines(r io.Reader, offset, limit int) ([]string, int, error) {
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
			// Stop once the window is filled; no need to read further.
			if lineno >= end-1 && len(window) == limit {
				return window, lineno, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return window, lineno, nil
			}
			return nil, lineno, err
		}
	}
}
