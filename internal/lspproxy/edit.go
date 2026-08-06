package lspproxy

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

type appliedRename struct {
	Files []appliedRenameFile
	Total int
}

type appliedRenameFile struct {
	Path  string
	Edits int
}

type plannedRenameFile struct {
	path    string
	content string
	mode    os.FileMode
	edits   int
}

type byteTextEdit struct {
	start   int
	end     int
	newText string
}

type lineBounds struct {
	start int
	end   int
}

func applyWorkspaceTextEdits(edits []FileEdits) (appliedRename, error) {
	if len(edits) == 0 {
		return appliedRename{}, nil
	}
	plans := make([]plannedRenameFile, 0, len(edits))
	total := 0
	for _, fe := range edits {
		path := uriToPath(fe.URI)
		if path == "" {
			return appliedRename{}, fmt.Errorf("unsupported non-file URI %q", fe.URI)
		}
		info, err := os.Stat(path)
		if err != nil {
			return appliedRename{}, err
		}
		if info.IsDir() {
			return appliedRename{}, fmt.Errorf("%s is a directory", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return appliedRename{}, err
		}
		text := string(data)
		byteEdits, err := normalizeTextEdits(text, fe.Edits, path)
		if err != nil {
			return appliedRename{}, err
		}
		if len(byteEdits) == 0 {
			continue
		}
		plans = append(plans, plannedRenameFile{
			path:    path,
			content: applyByteTextEdits(text, byteEdits),
			mode:    info.Mode(),
			edits:   len(byteEdits),
		})
		total += len(byteEdits)
	}

	for _, plan := range plans {
		if err := os.WriteFile(plan.path, []byte(plan.content), plan.mode.Perm()); err != nil {
			return appliedRename{}, err
		}
	}

	files := make([]appliedRenameFile, 0, len(plans))
	for _, plan := range plans {
		files = append(files, appliedRenameFile{Path: plan.path, Edits: plan.edits})
	}
	return appliedRename{Files: files, Total: total}, nil
}

func normalizeTextEdits(text string, edits []TextEdit, path string) ([]byteTextEdit, error) {
	out := make([]byteTextEdit, 0, len(edits))
	for _, edit := range edits {
		start, err := byteOffsetForPosition(text, edit.Range.Start)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		end, err := byteOffsetForPosition(text, edit.Range.End)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if start > end {
			return nil, fmt.Errorf("%s: edit range starts after it ends", path)
		}
		out = append(out, byteTextEdit{start: start, end: end, newText: edit.NewText})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	for i := 1; i < len(out); i++ {
		prev, cur := out[i-1], out[i]
		if cur.start < prev.end || (cur.start == prev.start && cur.end == prev.end) {
			return nil, fmt.Errorf("%s: language server returned overlapping edits", path)
		}
	}
	return out, nil
}

func applyByteTextEdits(text string, edits []byteTextEdit) string {
	var b strings.Builder
	b.Grow(len(text))
	cursor := 0
	for _, edit := range edits {
		b.WriteString(text[cursor:edit.start])
		b.WriteString(edit.newText)
		cursor = edit.end
	}
	b.WriteString(text[cursor:])
	return b.String()
}

func byteOffsetForPosition(text string, pos Position) (int, error) {
	if pos.Line < 0 {
		return 0, fmt.Errorf("negative line %d", pos.Line)
	}
	if pos.Character < 0 {
		return 0, fmt.Errorf("negative character %d", pos.Character)
	}
	lines := textLineBounds(text)
	if pos.Line >= len(lines) {
		return 0, fmt.Errorf("line %d is out of range (file has %d lines)", pos.Line+1, len(lines))
	}
	line := lines[pos.Line]
	lineText := text[line.start:line.end]
	if pos.Character > utf16Len(lineText) {
		return 0, fmt.Errorf("character %d is out of range on line %d", pos.Character+1, pos.Line+1)
	}
	return line.start + utf16ColToByteOffset(lineText, pos.Character), nil
}

func textLineBounds(text string) []lineBounds {
	out := []lineBounds{}
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '\n' {
			continue
		}
		end := i
		if end > start && text[end-1] == '\r' {
			end--
		}
		out = append(out, lineBounds{start: start, end: end})
		start = i + 1
	}
	end := len(text)
	if end > start && text[end-1] == '\r' {
		end--
	}
	return append(out, lineBounds{start: start, end: end})
}

func formatRenameApplied(result appliedRename) string {
	if result.Total == 0 {
		return "no rename edits applied (the symbol may not be renameable at this position)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "applied %d edit(s) across %d file(s)", result.Total, len(result.Files))
	for _, file := range result.Files {
		fmt.Fprintf(&b, "\n%s: %d edit(s)", file.Path, file.Edits)
	}
	return b.String()
}

func formatEditsApplied(label string, result appliedRename) string {
	if result.Total == 0 {
		return "no " + label + " edits applied"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "applied %s: %d edit(s) across %d file(s)", label, result.Total, len(result.Files))
	for _, file := range result.Files {
		fmt.Fprintf(&b, "\n%s: %d edit(s)", file.Path, file.Edits)
	}
	return b.String()
}
