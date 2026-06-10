// Package patch implements patch parsers and a fuzzy applier. The tool-facing
// parser accepts Codex apply_patch envelopes; the older unified-diff parser is
// retained for internal callers.
package patch

import (
	"fmt"
	"strconv"
	"strings"
)

// LineKind tags a hunk body line as context, addition, or deletion.
type LineKind int

const (
	Context LineKind = iota
	Add
	Del
)

// Line is one line of a hunk body. Text excludes the leading marker character
// and the trailing newline. NoNewline records a following "\ No newline at end
// of file" marker, meaning this line is the file's last line with no EOL.
type Line struct {
	Kind      LineKind
	Text      string
	NoNewline bool
}

// Hunk is one "@@ -OldStart,OldCount +NewStart,NewCount @@" block. Starts are
// 1-based; for a pure-insertion hunk against an empty region OldStart may be 0.
type Hunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []Line
}

// FilePatch is the set of changes to one file. Old and New are the prefix-
// stripped paths from the headers (equal for an in-place edit). For a create
// Old is empty; for a delete New is empty.
type FilePatch struct {
	Old      string
	New      string
	Hunks    []Hunk
	IsCreate bool
	IsDelete bool
	IsRename bool
}

// Path returns the file path the patch acts on: the new path unless this is a
// delete, in which case the old path.
func (f FilePatch) Path() string {
	if f.IsDelete {
		return f.Old
	}
	return f.New
}

// Parse turns unified-diff text into one FilePatch per file. It accepts git
// extended headers (diff --git, rename from/to, similarity index, index, new
// file mode, deleted file mode) and bare "---"/"+++"/"@@" diffs. A patch with
// no file headers, or a malformed hunk header, is an error.
func Parse(text string) ([]FilePatch, error) {
	lines := splitKeepingContent(text)
	p := &parser{lines: lines}
	files, err := p.run()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("empty patch: no file headers found")
	}
	return files, nil
}

// splitKeepingContent splits text into lines without a trailing newline on each
// line, dropping a single trailing empty element produced by a final newline.
func splitKeepingContent(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

type parser struct {
	lines []string
	i     int
}

func (p *parser) run() ([]FilePatch, error) {
	var files []FilePatch
	for p.i < len(p.lines) {
		line := p.lines[p.i]
		switch {
		case strings.HasPrefix(line, "diff --git "):
			f, err := p.parseGitFile()
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		case strings.HasPrefix(line, "--- "):
			f, err := p.parseTraditionalFile()
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		default:
			// Skip stray lines before the first file header.
			p.i++
		}
	}
	return files, nil
}

// parseGitFile consumes a "diff --git" block: extended headers, then either an
// optional "---"/"+++" pair with hunks, or (for a pure rename/mode change)
// nothing more.
func (p *parser) parseGitFile() (FilePatch, error) {
	var f FilePatch
	p.i++ // consume "diff --git" line

	for p.i < len(p.lines) {
		line := p.lines[p.i]
		switch {
		case strings.HasPrefix(line, "rename from "):
			f.IsRename = true
			f.Old = strings.TrimPrefix(line, "rename from ")
			p.i++
		case strings.HasPrefix(line, "rename to "):
			f.IsRename = true
			f.New = strings.TrimPrefix(line, "rename to ")
			p.i++
		case strings.HasPrefix(line, "new file mode"):
			f.IsCreate = true
			p.i++
		case strings.HasPrefix(line, "deleted file mode"):
			f.IsDelete = true
			p.i++
		case strings.HasPrefix(line, "similarity index "),
			strings.HasPrefix(line, "dissimilarity index "),
			strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "old mode "),
			strings.HasPrefix(line, "new mode "),
			strings.HasPrefix(line, "copy from "),
			strings.HasPrefix(line, "copy to "),
			strings.HasPrefix(line, "GIT binary patch"):
			p.i++
		case strings.HasPrefix(line, "--- "):
			old, gnew, err := p.parseFileHeaders()
			if err != nil {
				return f, err
			}
			p.applyHeaderPaths(&f, old, gnew)
			hunks, err := p.parseHunks()
			if err != nil {
				return f, err
			}
			f.Hunks = hunks
			return f, nil
		default:
			// End of this file's extended headers (pure rename/mode change).
			return f, nil
		}
	}
	return f, nil
}

// parseTraditionalFile consumes a bare "--- / +++" file with its hunks.
func (p *parser) parseTraditionalFile() (FilePatch, error) {
	var f FilePatch
	old, gnew, err := p.parseFileHeaders()
	if err != nil {
		return f, err
	}
	p.applyHeaderPaths(&f, old, gnew)
	hunks, err := p.parseHunks()
	if err != nil {
		return f, err
	}
	f.Hunks = hunks
	return f, nil
}

// applyHeaderPaths records the "---"/"+++" paths on f without clobbering paths
// already set by rename headers, and infers create/delete from /dev/null.
func (p *parser) applyHeaderPaths(f *FilePatch, old, gnew string) {
	if old == devNull {
		f.IsCreate = true
	} else if f.Old == "" {
		f.Old = old
	}
	if gnew == devNull {
		f.IsDelete = true
	} else if f.New == "" {
		f.New = gnew
	}
}

const devNull = "/dev/null"

// parseFileHeaders consumes the "--- old" and "+++ new" lines, returning the
// stripped paths (devNull preserved verbatim).
func (p *parser) parseFileHeaders() (old, gnew string, err error) {
	if p.i >= len(p.lines) || !strings.HasPrefix(p.lines[p.i], "--- ") {
		return "", "", fmt.Errorf("expected '--- ' file header, got %q", p.peek())
	}
	old = stripPath(strings.TrimPrefix(p.lines[p.i], "--- "))
	p.i++
	if p.i >= len(p.lines) || !strings.HasPrefix(p.lines[p.i], "+++ ") {
		return "", "", fmt.Errorf("expected '+++ ' file header after %q", "--- "+old)
	}
	gnew = stripPath(strings.TrimPrefix(p.lines[p.i], "+++ "))
	p.i++
	return old, gnew, nil
}

// stripPath drops a trailing tab-delimited timestamp, an "a/"/"b/" prefix, and
// surrounding quotes. "/dev/null" is returned unchanged.
func stripPath(s string) string {
	if tab := strings.IndexByte(s, '\t'); tab >= 0 {
		s = s[:tab]
	}
	s = strings.TrimSpace(s)
	if s == devNull {
		return s
	}
	s = strings.Trim(s, `"`)
	switch {
	case strings.HasPrefix(s, "a/"):
		s = s[2:]
	case strings.HasPrefix(s, "b/"):
		s = s[2:]
	}
	return s
}

// parseHunks consumes consecutive "@@" hunks until the next file header or EOF.
func (p *parser) parseHunks() ([]Hunk, error) {
	var hunks []Hunk
	for p.i < len(p.lines) {
		line := p.lines[p.i]
		if !strings.HasPrefix(line, "@@") {
			break
		}
		h, err := p.parseHunk()
		if err != nil {
			return nil, err
		}
		hunks = append(hunks, h)
	}
	return hunks, nil
}

func (p *parser) parseHunk() (Hunk, error) {
	header := p.lines[p.i]
	h, err := parseHunkHeader(header)
	if err != nil {
		return Hunk{}, err
	}
	p.i++

	var oldSeen, newSeen int
	for p.i < len(p.lines) {
		line := p.lines[p.i]
		if line == "" {
			// A bare empty line is a context line with empty text. But a blank
			// line that precedes a new file header / hunk header is genuine
			// content too; we treat every "" inside a hunk body as context
			// while the declared counts are not yet satisfied.
			if oldSeen >= h.OldCount && newSeen >= h.NewCount {
				break
			}
			h.Lines = append(h.Lines, Line{Kind: Context, Text: ""})
			oldSeen++
			newSeen++
			p.i++
			continue
		}
		marker := line[0]
		switch marker {
		case ' ':
			h.Lines = append(h.Lines, Line{Kind: Context, Text: line[1:]})
			oldSeen++
			newSeen++
		case '+':
			h.Lines = append(h.Lines, Line{Kind: Add, Text: line[1:]})
			newSeen++
		case '-':
			h.Lines = append(h.Lines, Line{Kind: Del, Text: line[1:]})
			oldSeen++
		case '\\':
			// "\ No newline at end of file" — annotate the preceding body line.
			if n := len(h.Lines); n > 0 {
				h.Lines[n-1].NoNewline = true
			}
		default:
			// Not a hunk body line: end of this hunk.
			goto done
		}
		p.i++
		if oldSeen >= h.OldCount && newSeen >= h.NewCount {
			// Header counts satisfied; a trailing no-newline marker may follow.
			if p.i < len(p.lines) && strings.HasPrefix(p.lines[p.i], "\\") {
				if n := len(h.Lines); n > 0 {
					h.Lines[n-1].NoNewline = true
				}
				p.i++
			}
			break
		}
	}
done:
	return h, nil
}

// parseHunkHeader parses "@@ -l,s +l,s @@ optional section heading". Counts may
// be omitted, meaning 1.
func parseHunkHeader(line string) (Hunk, error) {
	if !strings.HasPrefix(line, "@@") {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	rest := line[2:]
	close := strings.Index(rest, "@@")
	if close < 0 {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	mid := strings.TrimSpace(rest[:close])
	fields := strings.Fields(mid)
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	oldStart, oldCount, err := parseRange(strings.TrimPrefix(fields[0], "-"))
	if err != nil {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q: %w", line, err)
	}
	newStart, newCount, err := parseRange(strings.TrimPrefix(fields[1], "+"))
	if err != nil {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q: %w", line, err)
	}
	return Hunk{
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
	}, nil
}

// parseRange parses "start,count" or bare "start" (count defaults to 1).
func parseRange(s string) (start, count int, err error) {
	if s == "" {
		return 0, 0, fmt.Errorf("empty range")
	}
	if comma := strings.IndexByte(s, ','); comma >= 0 {
		start, err = strconv.Atoi(s[:comma])
		if err != nil {
			return 0, 0, err
		}
		count, err = strconv.Atoi(s[comma+1:])
		if err != nil {
			return 0, 0, err
		}
		return start, count, nil
	}
	start, err = strconv.Atoi(s)
	if err != nil {
		return 0, 0, err
	}
	return start, 1, nil
}

func (p *parser) peek() string {
	if p.i < len(p.lines) {
		return p.lines[p.i]
	}
	return "<EOF>"
}
