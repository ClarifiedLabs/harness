// Package diff renders small, stdlib-only unified diffs for before/after file
// snapshots. It is intentionally independent of git so it works the same inside
// and outside repositories.
package diff

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultContextLines = 3

// Snapshot is one file's contents at an instant.
type Snapshot struct {
	Path   string
	Exists bool
	Data   []byte
	Err    error
}

// FileDiff is the rendered result for one path.
type FileDiff struct {
	Path          string
	Text          string
	BinarySkipped bool
	Err           error
}

// Options controls unified diff rendering.
type Options struct {
	Context int
}

// SnapshotPaths reads paths in order. Missing files are represented as
// non-existing snapshots rather than errors so creates/deletes can be diffed.
func SnapshotPaths(paths []string) []Snapshot {
	paths = uniquePaths(paths)
	out := make([]Snapshot, 0, len(paths))
	for _, path := range paths {
		out = append(out, snapshotPath(path))
	}
	return out
}

func uniquePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		key := filepath.Clean(path)
		if abs, err := filepath.Abs(path); err == nil {
			key = filepath.Clean(abs)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, path)
	}
	return out
}

func snapshotPath(path string) Snapshot {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{Path: path}
		}
		return Snapshot{Path: path, Err: err}
	}
	if info.IsDir() {
		return Snapshot{Path: path, Err: fmt.Errorf("is a directory")}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{Path: path, Err: err}
	}
	return Snapshot{Path: path, Exists: true, Data: data}
}

// RenderSnapshots renders one diff per changed path. The before order is
// preserved; any after-only paths are appended in after order.
func RenderSnapshots(before, after []Snapshot, opts Options) []FileDiff {
	afterByPath := make(map[string]Snapshot, len(after))
	afterOrder := make([]string, 0, len(after))
	for _, snap := range after {
		key := snapshotKey(snap.Path)
		if _, ok := afterByPath[key]; !ok {
			afterOrder = append(afterOrder, key)
		}
		afterByPath[key] = snap
	}

	var out []FileDiff
	seen := make(map[string]bool, len(before))
	for _, old := range before {
		key := snapshotKey(old.Path)
		seen[key] = true
		newer, ok := afterByPath[key]
		if !ok {
			newer = Snapshot{Path: old.Path}
		}
		if fd, ok := renderSnapshotPair(old, newer, opts); ok {
			out = append(out, fd)
		}
	}
	for _, key := range afterOrder {
		if seen[key] {
			continue
		}
		newer := afterByPath[key]
		if fd, ok := renderSnapshotPair(Snapshot{Path: newer.Path}, newer, opts); ok {
			out = append(out, fd)
		}
	}
	return out
}

func snapshotKey(path string) string {
	key := filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		key = filepath.Clean(abs)
	}
	return key
}

func renderSnapshotPair(old, newer Snapshot, opts Options) (FileDiff, bool) {
	path := old.Path
	if path == "" {
		path = newer.Path
	}
	fd := FileDiff{Path: path}
	if old.Err != nil {
		fd.Err = old.Err
		return fd, true
	}
	if newer.Err != nil {
		fd.Err = newer.Err
		return fd, true
	}
	if old.Exists == newer.Exists && bytes.Equal(old.Data, newer.Data) {
		return FileDiff{}, false
	}
	if isBinary(old.Data) || isBinary(newer.Data) {
		fd.BinarySkipped = true
		return fd, true
	}
	fd.Text = Unified(path, old.Exists, old.Data, newer.Exists, newer.Data, opts)
	return fd, strings.TrimSpace(fd.Text) != ""
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// Unified renders a unified diff for one path. Data is treated as UTF-8-ish
// text; callers should skip binary files before calling it.
func Unified(path string, oldExists bool, oldData []byte, newExists bool, newData []byte, opts Options) string {
	context := opts.Context
	if context <= 0 {
		context = defaultContextLines
	}
	oldLines := splitLines(oldData)
	newLines := splitLines(newData)
	ops := diffLines(oldLines, newLines)
	if !hasChanges(ops) {
		return ""
	}

	var b strings.Builder
	b.WriteString("--- ")
	b.WriteString(oldLabel(path, oldExists))
	b.WriteByte('\n')
	b.WriteString("+++ ")
	b.WriteString(newLabel(path, newExists))
	b.WriteByte('\n')

	for _, h := range hunks(ops, context) {
		oldStart, oldCount := h.rangeFor(oldSide)
		newStart, newCount := h.rangeFor(newSide)
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				writeDiffLine(&b, ' ', op.oldLine)
			case opDelete:
				writeDiffLine(&b, '-', op.oldLine)
			case opInsert:
				writeDiffLine(&b, '+', op.newLine)
			}
		}
	}
	return b.String()
}

func oldLabel(path string, exists bool) string {
	if !exists {
		return "/dev/null"
	}
	return "a/" + displayPath(path)
}

func newLabel(path string, exists bool) string {
	if !exists {
		return "/dev/null"
	}
	return "b/" + displayPath(path)
}

func displayPath(path string) string {
	path = filepath.ToSlash(path)
	volume := filepath.VolumeName(path)
	if volume != "" {
		path = strings.TrimPrefix(path, volume)
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "."
	}
	return path
}

type line struct {
	text       string
	hasNewline bool
}

func splitLines(data []byte) []line {
	if len(data) == 0 {
		return nil
	}
	lines := make([]line, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		lines = append(lines, line{text: string(data[start:i]), hasNewline: true})
		start = i + 1
	}
	if start < len(data) {
		lines = append(lines, line{text: string(data[start:])})
	}
	return lines
}

func writeDiffLine(b *strings.Builder, prefix byte, ln line) {
	b.WriteByte(prefix)
	b.WriteString(ln.text)
	b.WriteByte('\n')
	if !ln.hasNewline {
		b.WriteString("\\ No newline at end of file\n")
	}
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type op struct {
	kind    opKind
	oldLine line
	newLine line
	oldNo   int
	newNo   int
}

func diffLines(oldLines, newLines []line) []op {
	if len(oldLines) == 0 && len(newLines) == 0 {
		return nil
	}
	if len(oldLines) > 0 && len(newLines) > 4_000_000/len(oldLines) {
		return wholeFileDiff(oldLines, newLines)
	}
	return lcsDiff(oldLines, newLines)
}

func wholeFileDiff(oldLines, newLines []line) []op {
	ops := make([]op, 0, len(oldLines)+len(newLines))
	oldNo, newNo := 1, 1
	for _, ln := range oldLines {
		ops = append(ops, op{kind: opDelete, oldLine: ln, oldNo: oldNo, newNo: newNo})
		oldNo++
	}
	for _, ln := range newLines {
		ops = append(ops, op{kind: opInsert, newLine: ln, oldNo: oldNo, newNo: newNo})
		newNo++
	}
	return ops
}

func lcsDiff(oldLines, newLines []line) []op {
	n, m := len(oldLines), len(newLines)
	width := m + 1
	dp := make([]int, (n+1)*(m+1))
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if sameLine(oldLines[i], newLines[j]) {
				dp[i*width+j] = dp[(i+1)*width+j+1] + 1
			} else {
				dp[i*width+j] = max(dp[(i+1)*width+j], dp[i*width+j+1])
			}
		}
	}

	var ops []op
	oldNo, newNo := 1, 1
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && sameLine(oldLines[i], newLines[j]):
			ops = append(ops, op{kind: opEqual, oldLine: oldLines[i], newLine: newLines[j], oldNo: oldNo, newNo: newNo})
			i++
			j++
			oldNo++
			newNo++
		case i < n && (j >= m || dp[(i+1)*width+j] >= dp[i*width+j+1]):
			ops = append(ops, op{kind: opDelete, oldLine: oldLines[i], oldNo: oldNo, newNo: newNo})
			i++
			oldNo++
		default:
			ops = append(ops, op{kind: opInsert, newLine: newLines[j], oldNo: oldNo, newNo: newNo})
			j++
			newNo++
		}
	}
	return ops
}

func sameLine(a, b line) bool {
	return a.text == b.text && a.hasNewline == b.hasNewline
}

func hasChanges(ops []op) bool {
	for _, op := range ops {
		if op.kind != opEqual {
			return true
		}
	}
	return false
}

type hunk struct {
	ops []op
}

func hunks(ops []op, context int) []hunk {
	changes := changeIndexes(ops)
	if len(changes) == 0 {
		return nil
	}
	out := make([]hunk, 0, len(changes))
	for i := 0; i < len(changes); {
		first := changes[i]
		last := first
		i++
		for i < len(changes) && changes[i]-last <= context*2 {
			last = changes[i]
			i++
		}
		start := max(0, first-context)
		end := min(len(ops), last+context+1)
		out = append(out, hunk{ops: ops[start:end]})
	}
	return out
}

func changeIndexes(ops []op) []int {
	var out []int
	for i, op := range ops {
		if op.kind != opEqual {
			out = append(out, i)
		}
	}
	return out
}

type side int

const (
	oldSide side = iota
	newSide
)

func (h hunk) rangeFor(s side) (start, count int) {
	for _, op := range h.ops {
		if lineNo, ok := op.lineNumberFor(s); ok {
			start = lineNo
			break
		}
	}
	for _, op := range h.ops {
		if op.countsFor(s) {
			count++
		}
	}
	if count > 0 {
		return start, count
	}
	for _, op := range h.ops {
		switch s {
		case oldSide:
			return max(0, op.oldNo-1), 0
		case newSide:
			return max(0, op.newNo-1), 0
		}
	}
	return 0, 0
}

func (op op) lineNumberFor(s side) (int, bool) {
	switch s {
	case oldSide:
		if op.kind == opEqual || op.kind == opDelete {
			return op.oldNo, true
		}
	case newSide:
		if op.kind == opEqual || op.kind == opInsert {
			return op.newNo, true
		}
	}
	return 0, false
}

func (op op) countsFor(s side) bool {
	switch s {
	case oldSide:
		return op.kind == opEqual || op.kind == opDelete
	case newSide:
		return op.kind == opEqual || op.kind == opInsert
	default:
		return false
	}
}

// Colorize adds ANSI color to unified diff lines. Header and hunk lines are
// cyan, deletions red, and additions green.
func Colorize(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = colorizeLine(line)
	}
	return strings.Join(lines, "")
}

const (
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
	ansiReset = "\x1b[0m"
)

func colorizeLine(line string) string {
	body, suffix := line, ""
	if strings.HasSuffix(line, "\n") {
		body = strings.TrimSuffix(line, "\n")
		suffix = "\n"
	}
	switch {
	case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "@@ "):
		return ansiCyan + body + ansiReset + suffix
	case strings.HasPrefix(line, "-"):
		return ansiRed + body + ansiReset + suffix
	case strings.HasPrefix(line, "+"):
		return ansiGreen + body + ansiReset + suffix
	default:
		return line
	}
}
