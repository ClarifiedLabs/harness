package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const editSchema = `{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "minItems": 1,
      "description": "Files to edit. Each file must appear once; all edits are matched against that file's original content.",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "File to edit; must already exist (use write_file to create)."},
          "edits": {
            "type": "array",
            "minItems": 1,
            "description": "One or more targeted replacements. Each oldText must be unique in the original file and edits must not overlap.",
            "items": {
              "type": "object",
              "properties": {
                "oldText": {"type": "string", "description": "Exact text to replace. Must be unique in the original file."},
                "newText": {"type": "string", "description": "Replacement text; empty string deletes oldText."}
              },
              "required": ["oldText", "newText"],
              "additionalProperties": false
            }
          }
        },
        "required": ["path", "edits"],
        "additionalProperties": false
      }
    }
  },
  "required": ["files"],
  "additionalProperties": false
}`

type edit struct{}

type editArgs struct {
	Files []editFile `json:"files"`
}

type editFile struct {
	Path  string      `json:"path"`
	Edits []editBlock `json:"edits"`
}

type editBlock struct {
	OldText string
	NewText string
}

type rawEditFile struct {
	Path  string         `json:"path"`
	Edits []rawEditBlock `json:"edits"`
}

type rawEditBlock struct {
	OldText *string `json:"oldText"`
	NewText *string `json:"newText"`
}

type plannedEditFile struct {
	path         string
	content      string
	mode         fs.FileMode
	replacements int
}

type textMatch struct {
	index          int
	length         int
	usedFuzzyMatch bool
}

type matchedEdit struct {
	index  int
	length int
	text   string
	order  int
}

func (edit) Name() string { return "edit" }

func (edit) Description() string {
	return "Edit one or more files with exact-text replacements. Each file has edits[]; oldText must be unique and non-overlapping in the original file."
}

func (edit) Schema() json.RawMessage { return json.RawMessage(editSchema) }

func (edit) ReadOnly(json.RawMessage) bool { return false }

func (edit) MutatedPaths(input json.RawMessage) ([]string, error) {
	args, err := decodeEditArgs(input)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(args.Files))
	for _, file := range args.Files {
		paths = append(paths, file.Path)
	}
	return paths, nil
}

func (edit) Run(_ context.Context, input json.RawMessage) (string, error) {
	args, err := decodeEditArgs(input)
	if err != nil {
		return "", err
	}

	plans, replacements, err := planEditFiles(args.Files)
	if err != nil {
		return "", err
	}
	for _, plan := range plans {
		if err := os.WriteFile(plan.path, []byte(plan.content), plan.mode.Perm()); err != nil {
			return "", err
		}
	}
	return formatEditSuccess(plans, replacements), nil
}

func decodeEditArgs(input json.RawMessage) (editArgs, error) {
	var raw struct {
		Files []rawEditFile `json:"files"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return editArgs{}, err
	}
	if len(raw.Files) == 0 {
		return editArgs{}, badArgs("files is required and must contain at least one file")
	}

	files := make([]editFile, 0, len(raw.Files))
	seen := make(map[string]string, len(raw.Files))
	for i, file := range raw.Files {
		if strings.TrimSpace(file.Path) == "" {
			return editArgs{}, badArgs("files[%d].path is required", i)
		}
		key := duplicatePathKey(file.Path)
		if first, ok := seen[key]; ok {
			return editArgs{}, badArgs("files[%d].path duplicates %q; combine edits for each file in one entry", i, first)
		}
		seen[key] = file.Path

		if len(file.Edits) == 0 {
			return editArgs{}, badArgs("files[%d].edits must contain at least one edit", i)
		}
		edits := make([]editBlock, 0, len(file.Edits))
		for j, block := range file.Edits {
			if block.OldText == nil {
				return editArgs{}, badArgs("files[%d].edits[%d].oldText is required", i, j)
			}
			if block.NewText == nil {
				return editArgs{}, badArgs("files[%d].edits[%d].newText is required", i, j)
			}
			if *block.OldText == "" {
				return editArgs{}, badArgs("files[%d].edits[%d].oldText must not be empty", i, j)
			}
			edits = append(edits, editBlock{OldText: *block.OldText, NewText: *block.NewText})
		}
		files = append(files, editFile{Path: file.Path, Edits: edits})
	}
	return editArgs{Files: files}, nil
}

func duplicatePathKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

func planEditFiles(files []editFile) ([]plannedEditFile, int, error) {
	plans := make([]plannedEditFile, 0, len(files))
	totalReplacements := 0
	for _, file := range files {
		info, err := os.Stat(file.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, 0, fmt.Errorf("%s does not exist; use write_file to create it", file.Path)
			}
			return nil, 0, err
		}
		if info.IsDir() {
			return nil, 0, fmt.Errorf("%s is a directory; use list_dir", file.Path)
		}
		data, err := os.ReadFile(file.Path)
		if err != nil {
			return nil, 0, err
		}

		bom, text := stripUTF8BOM(string(data))
		ending := detectLineEnding(text)
		updated, replacements, err := applyEditBlocks(text, file.Edits, file.Path)
		if err != nil {
			return nil, 0, err
		}
		plans = append(plans, plannedEditFile{
			path:         file.Path,
			content:      bom + restoreLineEndings(updated, ending),
			mode:         info.Mode(),
			replacements: replacements,
		})
		totalReplacements += replacements
	}
	return plans, totalReplacements, nil
}

func stripUTF8BOM(content string) (bom, text string) {
	if strings.HasPrefix(content, "\uFEFF") {
		return "\uFEFF", strings.TrimPrefix(content, "\uFEFF")
	}
	return "", content
}

func detectLineEnding(content string) string {
	crlf := strings.Index(content, "\r\n")
	lf := strings.Index(content, "\n")
	if lf < 0 {
		return "\n"
	}
	if crlf >= 0 && crlf < lf {
		return "\r\n"
	}
	return "\n"
}

func normalizeToLF(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

func restoreLineEndings(content, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(content, "\n", "\r\n")
	}
	return content
}

func applyEditBlocks(content string, edits []editBlock, path string) (string, int, error) {
	normalizedContent := normalizeToLF(content)
	normalizedEdits := make([]editBlock, len(edits))
	for i, block := range edits {
		normalizedEdits[i] = editBlock{
			OldText: normalizeToLF(block.OldText),
			NewText: normalizeToLF(block.NewText),
		}
	}

	useFuzzyBase := false
	for _, block := range normalizedEdits {
		match, ok := findEditText(normalizedContent, block.OldText)
		if ok && match.usedFuzzyMatch {
			useFuzzyBase = true
			break
		}
	}

	base := normalizedContent
	if useFuzzyBase {
		base = normalizeForFuzzyMatch(normalizedContent)
	}

	matches := make([]matchedEdit, 0, len(normalizedEdits))
	for i, block := range normalizedEdits {
		match, ok := findEditText(base, block.OldText)
		if !ok {
			return "", 0, editNotFoundError(path, i, len(normalizedEdits))
		}
		count := countEditOccurrences(base, block.OldText)
		if count > 1 {
			return "", 0, editDuplicateError(path, i, len(normalizedEdits), count)
		}
		matches = append(matches, matchedEdit{
			index:  match.index,
			length: match.length,
			text:   block.NewText,
			order:  i,
		})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].index < matches[j].index
	})
	for i := 1; i < len(matches); i++ {
		prev := matches[i-1]
		next := matches[i]
		if prev.index+prev.length > next.index {
			return "", 0, fmt.Errorf("edits[%d] and edits[%d] overlap in %s; merge them into one edit or target disjoint regions", prev.order, next.order, path)
		}
	}

	updated := base
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		updated = updated[:match.index] + match.text + updated[match.index+match.length:]
	}
	if updated == base {
		return "", 0, fmt.Errorf("no changes made to %s; replacements produced identical content", path)
	}
	return updated, len(matches), nil
}

func findEditText(content, oldText string) (textMatch, bool) {
	if idx := strings.Index(content, oldText); idx >= 0 {
		return textMatch{index: idx, length: len(oldText)}, true
	}
	fuzzyContent := normalizeForFuzzyMatch(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	if idx := strings.Index(fuzzyContent, fuzzyOldText); idx >= 0 {
		return textMatch{index: idx, length: len(fuzzyOldText), usedFuzzyMatch: true}, true
	}
	return textMatch{}, false
}

func normalizeForFuzzyMatch(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	content = strings.Join(lines, "\n")
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u2018', '\u2019', '\u201A', '\u201B':
			return '\''
		case '\u201C', '\u201D', '\u201E', '\u201F':
			return '"'
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return '-'
		case '\u00A0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200A', '\u202F', '\u205F', '\u3000':
			return ' '
		default:
			return r
		}
	}, content)
}

func countEditOccurrences(content, oldText string) int {
	return strings.Count(normalizeForFuzzyMatch(content), normalizeForFuzzyMatch(oldText))
}

func editNotFoundError(path string, editIndex, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("could not find oldText in %s; oldText must match exactly including whitespace and newlines", path)
	}
	return fmt.Errorf("could not find edits[%d].oldText in %s; oldText must match exactly including whitespace and newlines", editIndex, path)
}

func editDuplicateError(path string, editIndex, totalEdits, occurrences int) error {
	if totalEdits == 1 {
		return fmt.Errorf("found %d occurrences of oldText in %s; provide more context to make it unique", occurrences, path)
	}
	return fmt.Errorf("found %d occurrences of edits[%d].oldText in %s; each oldText must be unique", occurrences, editIndex, path)
}

func formatEditSuccess(plans []plannedEditFile, replacements int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "edited %d file(s), %d replacement(s)", len(plans), replacements)
	for _, plan := range plans {
		fmt.Fprintf(&b, "\nM %s (%d replacement(s))", plan.path, plan.replacements)
	}
	return b.String()
}
