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

// defaultReadLimit is the default number of lines returned (design §9.1).
const defaultReadLimit = 500

// readDirectoryCap bounds directory-form read output.
const readDirectoryCap = 200

const readFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path to read."},
    "offset": {"type": "integer", "description": "1-based starting line."},
    "limit": {"type": "integer", "description": "Maximum number of lines (default 500)."}
  },
  "required": ["path"]
}`

type readFile struct {
	defaultLimit int
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`

	// Accepted aliases deliberately stay out of the model-facing schema.
	FilePath      string `json:"file_path"`
	FilePathCamel string `json:"filePath"`
	File          string `json:"file"`
	Filename      string `json:"filename"`
	FilepathAlt   string `json:"filepath"`
	AbsolutePath  string `json:"absolute_path"`
	TargetFile    string `json:"target_file"`
}

func (readFile) Name() string { return "read" }

func (readFile) Description() string {
	return "Read a file; a directory lists entries."
}

func (readFile) Schema() json.RawMessage { return json.RawMessage(readFileSchema) }

func (readFile) ReadOnly(json.RawMessage) bool { return true }

func decodeReadFileArgs(input json.RawMessage) (readFileArgs, error) {
	var args readFileArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return readFileArgs{}, err
	}
	if args.Path == "" {
		args.Path = firstNonEmpty(args.FilePath, args.FilePathCamel, args.File, args.Filename, args.FilepathAlt, args.AbsolutePath, args.TargetFile)
	}
	if args.Offset < 0 {
		return readFileArgs{}, badArgs("offset must be >= 1")
	}
	if args.Limit < 0 {
		return readFileArgs{}, badArgs("limit must be >= 0")
	}
	if args.Path == "" {
		return readFileArgs{}, badArgs("path is required")
	}
	return args, nil
}

func (readFile) ReadPaths(input json.RawMessage) ([]string, error) {
	args, err := decodeReadFileArgs(input)
	if err != nil {
		return nil, err
	}
	return []string{args.Path}, nil
}

func (r readFile) Run(ctx context.Context, input json.RawMessage) (string, error) {
	args, err := decodeReadFileArgs(input)
	if err != nil {
		return "", err
	}

	defaultLimit := r.defaultLimit
	if defaultLimit == 0 {
		defaultLimit = defaultReadLimit
	}

	offset := args.Offset
	if offset == 0 {
		offset = 1
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	out, err := readOneFile(args.Path, offset, limit)
	if err != nil {
		return "", notExistingPathError(args.Path, err)
	}
	return out, nil
}

// firstNonEmpty returns the first argument whose value is non-empty after
// trimming surrounding whitespace, or "" if none qualify. It resolves
// read's path aliases in a fixed precedence order.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// readOneFile reads the [offset, offset+limit) window of a single file and
// returns its line-numbered body, including the truncation notice (r14) when the
// file continues past the window.
func readOneFile(path string, offset, limit int) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		listing, err := renderDirectory(path, readDirectoryCap)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[directory listing: %s]\n%s", path, listing), nil
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
		// Emit a truncation notice so the model knows line N is not EOF
		// and can resume from the next line instead of assuming it read the whole file.
		last := offset + len(lines) - 1
		out += fmt.Sprintf("\n[file truncated at line %d; continue with offset=%d]", last, last+1)
	}
	return out, nil
}

// numberLines renders lines as "<n>\t<line>"; startLine is the 1-based number of
// the first line. The number is emitted with no column padding: the model parses
// the integer, not its alignment, and a fixed-width pad wastes bytes on large
// reads.
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
