package patch

import (
	"fmt"
	"strings"
)

const (
	codexBeginPatch = "*** Begin Patch"
	codexEndPatch   = "*** End Patch"
)

// ParseCodex parses the Codex apply_patch envelope:
//
//	*** Begin Patch
//	*** Add File: path
//	+content
//	*** Update File: path
//	@@
//	 old
//	-old
//	+new
//	*** Delete File: path
//	*** End Patch
//
// Update hunks are intentionally headerless. They are located against the
// target file at apply time, in order, rather than by unified-diff line ranges.
func ParseCodex(text string) ([]FilePatch, error) {
	lines := splitKeepingContent(strings.ReplaceAll(text, "\r\n", "\n"))
	p := &codexParser{lines: lines}
	files, err := p.run()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("empty patch: no file operations found")
	}
	return files, nil
}

type codexParser struct {
	lines []string
	i     int
}

func (p *codexParser) run() ([]FilePatch, error) {
	if p.i >= len(p.lines) || p.lines[p.i] != codexBeginPatch {
		return nil, fmt.Errorf("invalid patch: first line must be %q", codexBeginPatch)
	}
	p.i++

	var files []FilePatch
	for p.i < len(p.lines) {
		line := p.lines[p.i]
		switch {
		case line == codexEndPatch:
			p.i++
			if p.i != len(p.lines) {
				return nil, fmt.Errorf("invalid patch: trailing content after %q", codexEndPatch)
			}
			return files, nil
		case strings.HasPrefix(line, "*** Add File: "):
			f, err := p.parseCodexAdd(strings.TrimPrefix(line, "*** Add File: "))
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		case strings.HasPrefix(line, "*** Delete File: "):
			f, err := p.parseCodexDelete(strings.TrimPrefix(line, "*** Delete File: "))
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		case strings.HasPrefix(line, "*** Update File: "):
			f, err := p.parseCodexUpdate(strings.TrimPrefix(line, "*** Update File: "))
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		default:
			return nil, fmt.Errorf("invalid patch hunk on line %d: %q", p.i+1, line)
		}
	}
	return nil, fmt.Errorf("invalid patch: missing %q", codexEndPatch)
}

func (p *codexParser) parseCodexAdd(path string) (FilePatch, error) {
	if path == "" {
		return FilePatch{}, fmt.Errorf("invalid add file header on line %d: path is required", p.i+1)
	}
	p.i++

	var lines []Line
	for p.i < len(p.lines) && !p.atOperation() {
		line := p.lines[p.i]
		if !strings.HasPrefix(line, "+") {
			return FilePatch{}, fmt.Errorf("invalid add file line %d: lines must start with '+'", p.i+1)
		}
		lines = append(lines, Line{Kind: Add, Text: line[1:]})
		p.i++
	}
	if len(lines) == 0 {
		return FilePatch{}, fmt.Errorf("invalid add file hunk for path %q: no lines", path)
	}

	return FilePatch{
		New:      path,
		IsCreate: true,
		Hunks: []Hunk{{
			OldStart: 0,
			OldCount: 0,
			NewStart: 1,
			NewCount: len(lines),
			Lines:    lines,
		}},
	}, nil
}

func (p *codexParser) parseCodexDelete(path string) (FilePatch, error) {
	if path == "" {
		return FilePatch{}, fmt.Errorf("invalid delete file header on line %d: path is required", p.i+1)
	}
	p.i++
	return FilePatch{Old: path, IsDelete: true}, nil
}

func (p *codexParser) parseCodexUpdate(path string) (FilePatch, error) {
	if path == "" {
		return FilePatch{}, fmt.Errorf("invalid update file header on line %d: path is required", p.i+1)
	}
	f := FilePatch{Old: path, New: path}
	p.i++

	if p.i < len(p.lines) && strings.HasPrefix(p.lines[p.i], "*** Move to: ") {
		f.New = strings.TrimPrefix(p.lines[p.i], "*** Move to: ")
		if f.New == "" {
			return FilePatch{}, fmt.Errorf("invalid move header on line %d: path is required", p.i+1)
		}
		f.IsRename = true
		p.i++
	}

	var hunks []Hunk
	for p.i < len(p.lines) && !p.atOperation() {
		if strings.HasPrefix(p.lines[p.i], "@@") {
			p.i++
			if p.i < len(p.lines) && p.lines[p.i] == "*** End of File" {
				return FilePatch{}, fmt.Errorf("invalid patch hunk on line %d: Update hunk does not contain any lines", p.i)
			}
		}

		h, err := p.parseCodexChange()
		if err != nil {
			return FilePatch{}, err
		}
		if len(h.Lines) == 0 {
			return FilePatch{}, fmt.Errorf("invalid patch hunk on line %d: Update hunk does not contain any lines", p.i+1)
		}
		hunks = append(hunks, h)
	}
	if len(hunks) == 0 {
		return FilePatch{}, fmt.Errorf("invalid patch hunk on line %d: Update file hunk for path %q is empty", p.i, path)
	}
	f.Hunks = hunks
	return f, nil
}

func (p *codexParser) parseCodexChange() (Hunk, error) {
	var h Hunk
	h.OldStart = -1 // Headerless Codex hunk: locate from the current cursor.
	for p.i < len(p.lines) && !p.atOperation() {
		line := p.lines[p.i]
		if strings.HasPrefix(line, "@@") {
			break
		}
		if line == "*** End of File" {
			p.i++
			break
		}
		if line == "" {
			return Hunk{}, fmt.Errorf("invalid patch hunk on line %d: empty lines must be prefixed with ' '", p.i+1)
		}
		switch line[0] {
		case ' ':
			h.Lines = append(h.Lines, Line{Kind: Context, Text: line[1:]})
			h.OldCount++
			h.NewCount++
		case '-':
			h.Lines = append(h.Lines, Line{Kind: Del, Text: line[1:]})
			h.OldCount++
		case '+':
			h.Lines = append(h.Lines, Line{Kind: Add, Text: line[1:]})
			h.NewCount++
		default:
			return Hunk{}, fmt.Errorf("invalid patch hunk on line %d: %q", p.i+1, line)
		}
		p.i++
	}
	return h, nil
}

func (p *codexParser) atOperation() bool {
	if p.i >= len(p.lines) {
		return false
	}
	line := p.lines[p.i]
	return line == codexEndPatch ||
		strings.HasPrefix(line, "*** Add File: ") ||
		strings.HasPrefix(line, "*** Delete File: ") ||
		strings.HasPrefix(line, "*** Update File: ")
}

// ApplyCodex applies files in patch order and stops at the first rejected file,
// matching Codex's partial-before-failure behavior.
func ApplyCodex(files []FilePatch) Result {
	var res Result
	for _, f := range files {
		path, err := applyFile(f)
		if err != nil {
			res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: err.Error()})
			break
		}
		res.Applied = append(res.Applied, path)
	}
	return res
}
