package patch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// offsetSearchRadius bounds the level-2 search: a hunk's context may be found
// up to this many lines away from its header-stated position (design §9.6).
const offsetSearchRadius = 200

// Rejection records one file the patch could not be applied to and why.
type Rejection struct {
	Path   string
	Reason string
}

// Result reports the outcome of applying a multi-file patch: paths that were
// written and files that were rejected (left untouched).
type Result struct {
	Applied  []string
	Rejected []Rejection
}

// Apply applies each FilePatch to the filesystem. Per file the work is atomic:
// every hunk applies to an in-memory copy and the file is written only if all
// hunks match, so a failing hunk leaves the file untouched. Files are otherwise
// independent — a rejection on one does not stop the others (design §9.6).
func Apply(files []FilePatch) Result {
	var res Result
	for _, f := range files {
		path, err := applyFile(f)
		if err != nil {
			res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: err.Error()})
			continue
		}
		res.Applied = append(res.Applied, path)
	}
	return res
}

// applyFile applies one FilePatch and returns the path it acted on. On any
// failure it returns an error and makes no filesystem change for that file.
func applyFile(f FilePatch) (string, error) {
	path := f.Path()

	switch {
	case f.IsCreate:
		return applyCreate(f)
	case f.IsDelete:
		return applyDelete(f)
	}

	// Edit or rename-with-edit: read source, apply hunks, write destination.
	src := f.Old
	if src == "" {
		src = f.New
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return path, fmt.Errorf("target %s does not exist", src)
		}
		return path, err
	}

	newContent := string(data)
	if len(f.Hunks) > 0 {
		newContent, err = applyHunks(string(data), f.Hunks)
		if err != nil {
			return path, err
		}
	}

	dst := f.New
	if dst == "" {
		dst = f.Old
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return path, err
	}
	if err := os.WriteFile(dst, []byte(newContent), filePerm(src)); err != nil {
		return path, err
	}
	if f.IsRename && f.Old != "" && f.New != "" && f.Old != f.New {
		if err := os.Remove(f.Old); err != nil {
			return path, err
		}
	}
	return path, nil
}

func applyCreate(f FilePatch) (string, error) {
	path := f.New
	if _, err := os.Stat(path); err == nil {
		return path, fmt.Errorf("cannot create %s: file already exists", path)
	}
	var b strings.Builder
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == Del {
				continue
			}
			b.WriteString(l.Text)
			if !l.NoNewline {
				b.WriteByte('\n')
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return path, err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return path, err
	}
	return path, nil
}

func applyDelete(f FilePatch) (string, error) {
	path := f.Old
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, fmt.Errorf("cannot delete %s: file does not exist", path)
		}
		return path, err
	}
	// Verify the deletion hunks match the file's content before removing it.
	if len(f.Hunks) > 0 {
		if _, err := applyHunks(string(data), f.Hunks); err != nil {
			return path, err
		}
	}
	if err := os.Remove(path); err != nil {
		return path, err
	}
	return path, nil
}

// applyHunks applies all hunks to content in order, threading a running offset
// so later hunks locate their context after earlier insertions/deletions. It
// returns the new content or an error naming the first hunk that did not match.
func applyHunks(content string, hunks []Hunk) (string, error) {
	lines, trailingNewline := splitLines(content)

	// out is built incrementally; cursor is the index into lines up to which
	// output has been copied. offset tracks the net shift applied so far.
	var out []string
	cursor := 0
	offset := 0

	for idx, h := range hunks {
		from := hunkOldLines(h)

		// Candidate start in the current file (0-based), from the header
		// position adjusted by the running offset.
		want := h.OldStart - 1 + offset
		switch {
		case h.OldStart < 0 && len(from) == 0:
			want = len(lines)
		case h.OldStart < 0:
			want = cursor
		case h.OldStart == 0:
			want = len(lines) // pure insertion at EOF for an empty old side
		case len(from) == 0:
			// Pure insertion with OldStart >= 1: unified-diff semantics place
			// the new lines AFTER existing line OldStart (0-based index
			// OldStart), not before it.
			want = h.OldStart + offset
		}

		pos, ok := locate(lines, from, want, cursor, h.OldStart < 0)
		if !ok {
			return "", fmt.Errorf("hunk %d of %d did not match", idx+1, len(hunks))
		}

		// Copy unchanged lines between the cursor and the match.
		out = append(out, lines[cursor:pos]...)

		// Emit the hunk: preserve the file's actual lines for context (so
		// whitespace-normalized matches keep the file's real whitespace).
		filePos := pos
		for _, l := range h.Lines {
			switch l.Kind {
			case Context:
				out = append(out, lines[filePos])
				filePos++
			case Del:
				filePos++
			case Add:
				out = append(out, l.Text)
			}
		}
		cursor = filePos
		offset += (h.NewCount - h.OldCount)
	}

	out = append(out, lines[cursor:]...)
	return joinLines(out, trailingNewline), nil
}

// hunkOldLines returns the old-side (context + deleted) line texts of a hunk,
// in order — the sequence that must be located in the file.
func hunkOldLines(h Hunk) []string {
	var from []string
	for _, l := range h.Lines {
		if l.Kind == Context || l.Kind == Del {
			from = append(from, l.Text)
		}
	}
	return from
}

// locate finds the index in lines (>= cursor) where from matches, trying three
// levels in order: exact at want, exact within ±radius of want, then
// whitespace-normalized within ±radius. It returns the match index and whether
// one was found. An empty from (pure insertion) matches at want clamped into
// range.
func locate(lines, from []string, want, cursor int, unbounded bool) (int, bool) {
	if len(from) == 0 {
		pos := want
		if pos < cursor {
			pos = cursor
		}
		if pos > len(lines) {
			pos = len(lines)
		}
		return pos, true
	}

	if want < cursor {
		want = cursor
	}

	// Level 1: exact match at the stated position.
	if matchAt(lines, from, want, false) {
		return want, true
	}

	// Level 2: exact match within ±radius, nearest first.
	if unbounded {
		if pos, ok := searchForward(lines, from, cursor, false); ok {
			return pos, true
		}
	} else {
		if pos, ok := searchNearest(lines, from, want, cursor, false); ok {
			return pos, true
		}
	}

	// Level 3: whitespace-normalized match within ±radius, nearest first.
	if matchAt(lines, from, want, true) {
		return want, true
	}
	if unbounded {
		if pos, ok := searchForward(lines, from, cursor, true); ok {
			return pos, true
		}
	} else {
		if pos, ok := searchNearest(lines, from, want, cursor, true); ok {
			return pos, true
		}
	}

	return 0, false
}

// searchNearest scans outward from want (within ±offsetSearchRadius, not below
// cursor) for a position where from matches, preferring the closest.
func searchNearest(lines, from []string, want, cursor int, normalize bool) (int, bool) {
	for d := 1; d <= offsetSearchRadius; d++ {
		if p := want - d; p >= cursor && matchAt(lines, from, p, normalize) {
			return p, true
		}
		if p := want + d; matchAt(lines, from, p, normalize) {
			return p, true
		}
	}
	return 0, false
}

func searchForward(lines, from []string, cursor int, normalize bool) (int, bool) {
	for pos := cursor; pos+len(from) <= len(lines); pos++ {
		if matchAt(lines, from, pos, normalize) {
			return pos, true
		}
	}
	return 0, false
}

// matchAt reports whether from occurs in lines starting at pos. When normalize
// is set, comparison strips leading/trailing whitespace from both sides.
func matchAt(lines, from []string, pos int, normalize bool) bool {
	if pos < 0 || pos+len(from) > len(lines) {
		return false
	}
	for i, want := range from {
		got := lines[pos+i]
		if normalize {
			if strings.TrimSpace(got) != strings.TrimSpace(want) {
				return false
			}
		} else if got != want {
			return false
		}
	}
	return true
}

// splitLines splits content into lines (without the line terminator), reporting
// whether the content ended with a newline. Empty content yields no lines.
func splitLines(content string) (lines []string, trailingNewline bool) {
	if content == "" {
		return nil, false
	}
	trailingNewline = strings.HasSuffix(content, "\n")
	body := content
	if trailingNewline {
		body = body[:len(body)-1]
	}
	return strings.Split(body, "\n"), trailingNewline
}

// joinLines is the inverse of splitLines.
func joinLines(lines []string, trailingNewline bool) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, "\n")
	if trailingNewline {
		s += "\n"
	}
	return s
}

// filePerm returns the existing file's permission bits, or 0644 if it cannot be
// stat'd, so a rewrite preserves the original mode.
func filePerm(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0644
}
