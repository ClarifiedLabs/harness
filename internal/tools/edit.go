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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"harness/internal/llm"
)

const editSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Optional default path inherited by files entries that omit path."},
    "files": {
      "type": "array",
      "minItems": 1,
      "description": "Applied in order; duplicate paths see prior results. One entry shares a base.",
      "items": {
        "type": "object",
		"properties": {
			"path": {"type": "string", "description": "Must exist."},
			"expected_sha256": {"type": "string", "description": "Optional digest from read; reject if the file changed."},
          "edits": {
            "type": "array",
            "minItems": 1,
            "description": "Non-overlapping replacements against the entry base.",
            "items": {
              "type": "object",
              "properties": {
                "oldText": {"type": "string", "description": "Must be unique unless replaceAll."},
                "newText": {"type": "string", "description": "Empty deletes."},
                "replaceAll": {"type": "boolean", "description": "Replace every match; default false."}
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
	Path           string      `json:"path"`
	ExpectedSHA256 string      `json:"expected_sha256"`
	Edits          []editBlock `json:"edits"`
}

type editBlock struct {
	OldText    string
	NewText    string
	ReplaceAll bool
}

type rawEditFile struct {
	Path           string         `json:"path"`
	ExpectedSHA256 string         `json:"expected_sha256"`
	Edits          []rawEditBlock `json:"edits"`
}

type rawEditBlock struct {
	OldText    *string `json:"oldText"`
	NewText    *string `json:"newText"`
	ReplaceAll bool    `json:"replaceAll"`
}

type plannedEditFile struct {
	path         string
	originalSHA  string
	content      string
	body         string // LF-normalized updated content, used to render the snippet
	bom          string
	ending       string // detected line-ending style to restore on write
	mode         fs.FileMode
	replacements int
	fuzzyMatches int
	regions      []editRegion
}

// editRegion is a 1-based inclusive line range in the updated file that an edit
// touched. Used to render a small post-edit verification snippet.
type editRegion struct {
	startLine int
	endLine   int
}

type textMatch struct {
	index          int
	length         int
	usedFuzzyMatch bool
}

type matchedEdit struct {
	index      int
	length     int
	text       string
	order      int
	replaceAll bool
}

func (edit) Name() string { return "edit" }

func (edit) Description() string { return "Replace exact unique oldText; the file must already exist." }

func (edit) Schema() json.RawMessage { return json.RawMessage(editSchema) }

func (edit) PreserveSchemaDescriptions() bool { return true }

func (edit) ReadOnly(json.RawMessage) bool { return false }

func (edit) MutatedPaths(input json.RawMessage) ([]string, error) {
	// A retention receipt keeps only files[].path (plus counts); decode leniently
	// so compaction file indexing keeps working on trimmed inputs.
	var args struct {
		Path  string `json:"path"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	if len(args.Files) == 0 && strings.TrimSpace(args.Path) != "" {
		return []string{args.Path}, nil
	}
	paths := make([]string, 0, len(args.Files))
	for _, file := range args.Files {
		paths = append(paths, file.Path)
	}
	return paths, nil
}

func (edit) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := (edit{}).RunResult(ctx, input)
	return result.Text, err
}

// RetentionInputReceipt replaces superseded edit bodies with a path-only
// receipt. The files[].path shape keeps MutatedPaths decodable so compaction
// still indexes the mutation; the sentinel marks the input as already trimmed.
func (edit) RetentionInputReceipt(input json.RawMessage) (json.RawMessage, bool) {
	args, err := decodeEditArgs(input)
	if err != nil {
		return nil, false
	}
	files := make([]struct {
		Path     string `json:"path"`
		Edits    int    `json:"edits"`
		OldBytes int    `json:"old_text_bytes"`
		NewBytes int    `json:"new_text_bytes"`
	}, 0, len(args.Files))
	for _, file := range args.Files {
		old, new := 0, 0
		for _, block := range file.Edits {
			old += len(block.OldText)
			new += len(block.NewText)
		}
		files = append(files, struct {
			Path     string `json:"path"`
			Edits    int    `json:"edits"`
			OldBytes int    `json:"old_text_bytes"`
			NewBytes int    `json:"new_text_bytes"`
		}{Path: file.Path, Edits: len(file.Edits), OldBytes: old, NewBytes: new})
	}
	receipt, err := json.Marshal(struct {
		Files []struct {
			Path     string `json:"path"`
			Edits    int    `json:"edits"`
			OldBytes int    `json:"old_text_bytes"`
			NewBytes int    `json:"new_text_bytes"`
		} `json:"files"`
		Superseded string `json:"_superseded"`
	}{
		Files:      files,
		Superseded: "edit content omitted; later successful edit to this path exists; read the file if needed",
	})
	if err != nil {
		return nil, false
	}
	return receipt, true
}

func (edit) RunResult(_ context.Context, input json.RawMessage) (RunResult, error) {
	args, err := decodeEditArgs(input)
	if err != nil {
		return RunResult{}, err
	}

	plans, replacements, fuzzyMatches, err := planEditFiles(args.Files)
	if err != nil {
		return RunResult{}, err
	}
	for _, plan := range plans {
		if err := os.WriteFile(plan.path, []byte(plan.content), plan.mode.Perm()); err != nil {
			return RunResult{}, err
		}
	}
	text := formatEditSuccess(plans, replacements)
	if fuzzyMatches > 0 {
		text += fmt.Sprintf("\nMatched %d replacement(s) after normalizing trailing whitespace or typographic punctuation; untouched bytes were preserved.", fuzzyMatches)
	}
	return RunResult{Text: text, Metrics: map[string]int{"fuzzy_matches": fuzzyMatches}}, nil
}

func decodeEditArgs(input json.RawMessage) (editArgs, error) {
	var raw struct {
		Files []rawEditFile  `json:"files"`
		Path  string         `json:"path"`
		Edits []rawEditBlock `json:"edits"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return editArgs{}, err
	}
	if len(raw.Files) == 0 && len(raw.Edits) > 0 {
		// Accept the common single-file {path, edits} shape for compatibility,
		// while keeping the model-facing schema canonical and files-based.
		raw.Files = []rawEditFile{{Path: raw.Path, Edits: raw.Edits}}
	}
	if len(raw.Files) == 0 {
		return editArgs{}, badArgs("files is required and must contain at least one file")
	}
	if strings.TrimSpace(raw.Path) != "" {
		// Top-level path is the default base for every entry that omits its
		// own path. An entry that names a different path is ambiguous and
		// still rejected; naming the same path is tolerated.
		for i := range raw.Files {
			if strings.TrimSpace(raw.Files[i].Path) == "" {
				raw.Files[i].Path = raw.Path
				continue
			}
			if filepath.Clean(raw.Files[i].Path) != filepath.Clean(raw.Path) {
				return editArgs{}, badArgs("files[%d].path %q differs from top-level path %q; use {files:[{path,edits}]} when files diverge", i, raw.Files[i].Path, raw.Path)
			}
		}
	}

	files := make([]editFile, 0, len(raw.Files))
	for i, file := range raw.Files {
		if strings.TrimSpace(file.Path) == "" {
			return editArgs{}, badArgs("files[%d].path is required", i)
		}

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
			edits = append(edits, editBlock{OldText: *block.OldText, NewText: *block.NewText, ReplaceAll: block.ReplaceAll})
		}
		expected, err := normalizeSHA256(file.ExpectedSHA256, fmt.Sprintf("files[%d].expected_sha256", i))
		if err != nil {
			return editArgs{}, err
		}
		files = append(files, editFile{Path: file.Path, ExpectedSHA256: expected, Edits: edits})
	}
	return editArgs{Files: files}, nil
}

func duplicatePathKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

func planEditFiles(files []editFile) ([]plannedEditFile, int, int, error) {
	plans := make([]plannedEditFile, 0, len(files))
	byPath := make(map[string]int, len(files)) // duplicatePathKey → plans index
	totalReplacements := 0
	totalFuzzyMatches := 0
	for _, file := range files {
		key := duplicatePathKey(file.Path)
		if idx, ok := byPath[key]; ok {
			// A repeated files[].path entry applies in order against the earlier
			// entry's planned result, not the on-disk content (design §9.4). A
			// stale or redundant oldText fails loudly with the ordinary not-found
			// error before anything is written; nothing is silently double-applied.
			prev := &plans[idx]
			if err := checkExpectedSHA256Digest(file.Path, file.ExpectedSHA256, prev.originalSHA); err != nil {
				return nil, 0, 0, err
			}
			updated, replacements, fuzzyMatches, regions, err := applyEditBlocks(prev.body, file.Edits, file.Path)
			if err != nil {
				return nil, 0, 0, err
			}
			prev.content = prev.bom + restoreLineEndings(updated, prev.ending)
			prev.body = updated
			prev.replacements += replacements
			prev.fuzzyMatches += fuzzyMatches
			prev.regions = append(prev.regions, regions...)
			totalReplacements += replacements
			totalFuzzyMatches += fuzzyMatches
			continue
		}
		info, err := os.Stat(file.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, 0, 0, fmt.Errorf("%s does not exist; use write to create it", file.Path)
			}
			return nil, 0, 0, err
		}
		if info.IsDir() {
			return nil, 0, 0, fmt.Errorf("%s is a directory; use read with a directory path or an appropriate shell lookup", file.Path)
		}
		data, err := os.ReadFile(file.Path)
		if err != nil {
			return nil, 0, 0, err
		}
		if err := checkExpectedSHA256(file.Path, file.ExpectedSHA256, data); err != nil {
			return nil, 0, 0, err
		}

		bom, text := stripUTF8BOM(string(data))
		ending := detectLineEnding(text)
		updated, replacements, fuzzyMatches, regions, err := applyEditBlocks(text, file.Edits, file.Path)
		if err != nil {
			return nil, 0, 0, err
		}
		plans = append(plans, plannedEditFile{
			path:         file.Path,
			originalSHA:  sha256Hex(data),
			content:      bom + restoreLineEndings(updated, ending),
			body:         updated,
			bom:          bom,
			ending:       ending,
			mode:         info.Mode(),
			replacements: replacements,
			fuzzyMatches: fuzzyMatches,
			regions:      regions,
		})
		byPath[key] = len(plans) - 1
		totalReplacements += replacements
		totalFuzzyMatches += fuzzyMatches
	}
	return plans, totalReplacements, totalFuzzyMatches, nil
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

func applyEditBlocks(content string, edits []editBlock, path string) (string, int, int, []editRegion, error) {
	normalizedContent := normalizeToLF(content)
	normalizedEdits := make([]editBlock, len(edits))
	for i, block := range edits {
		normalizedEdits[i] = editBlock{
			OldText:    normalizeToLF(block.OldText),
			NewText:    normalizeToLF(block.NewText),
			ReplaceAll: block.ReplaceAll,
		}
	}

	matches := make([]matchedEdit, 0, len(normalizedEdits))
	fuzzyMatches := 0
	var validationErrors []error
	validationKind := llm.ToolErrorEditOldTextAmbiguous
	for i, block := range normalizedEdits {
		if block.ReplaceAll {
			// Replace every occurrence: bypass the uniqueness check and record one
			// span per non-overlapping match. Zero matches is still a not-found.
			found := findAllEditText(normalizedContent, block.OldText)
			if len(found) == 0 {
				validationErrors = append(validationErrors, editNotFoundError(path, normalizedContent, block.OldText, i, len(normalizedEdits)))
				validationKind = llm.ToolErrorEditOldTextNotFound
				continue
			}
			for _, m := range found {
				if m.usedFuzzyMatch {
					fuzzyMatches++
				}
				matches = append(matches, matchedEdit{
					index:      m.index,
					length:     m.length,
					text:       block.NewText,
					order:      i,
					replaceAll: true,
				})
			}
			continue
		}
		match, ok := findEditText(normalizedContent, block.OldText)
		if !ok {
			validationErrors = append(validationErrors, editNotFoundError(path, normalizedContent, block.OldText, i, len(normalizedEdits)))
			validationKind = llm.ToolErrorEditOldTextNotFound
			continue
		}
		count := len(findAllEditText(normalizedContent, block.OldText))
		if count > 1 {
			validationErrors = append(validationErrors, editDuplicateError(path, normalizedContent, block.OldText, i, len(normalizedEdits), count))
			continue
		}
		if match.usedFuzzyMatch {
			fuzzyMatches++
		}
		matches = append(matches, matchedEdit{
			index:  match.index,
			length: match.length,
			text:   block.NewText,
			order:  i,
		})
	}
	if len(validationErrors) > 0 {
		if len(validationErrors) == 1 {
			return "", 0, 0, nil, validationErrors[0]
		}
		messages := make([]string, len(validationErrors))
		for i, err := range validationErrors {
			messages[i] = err.Error()
		}
		return "", 0, 0, nil, WithKind(fmt.Errorf("edit validation failed for %s:\n- %s", path, strings.Join(messages, "\n- ")), validationKind)
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].index < matches[j].index
	})
	for i := 1; i < len(matches); i++ {
		prev := matches[i-1]
		next := matches[i]
		if prev.index+prev.length > next.index {
			// Exempt the overlap guard only when both spans come from the SAME
			// replaceAll block: indexAllEditText advances disjointly, so a block's
			// own occurrences never overlap each other. Spans from different edits —
			// a replaceAll span overlapping another replaceAll or a normal edit —
			// must still raise the overlap error, or the right-to-left splice below
			// would silently double-apply/clobber the shared range.
			if (prev.replaceAll || next.replaceAll) && prev.order == next.order {
				continue
			}
			return "", 0, 0, nil, fmt.Errorf("edits[%d] and edits[%d] overlap in %s; merge them into one edit or target disjoint regions", prev.order, next.order, path)
		}
	}

	updated := normalizedContent
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		updated = updated[:match.index] + match.text + updated[match.index+match.length:]
	}
	if updated == normalizedContent {
		return "", 0, 0, nil, fmt.Errorf("no changes made to %s; replacements produced identical content", path)
	}
	return updated, len(matches), fuzzyMatches, changedRegions(updated, matches), nil
}

// changedRegions maps the applied edits to 1-based inclusive line ranges in the
// updated content. matches are ascending and non-overlapping (replaceAll spans
// included), so the new byte offset of each span is its base index shifted by
// the cumulative length delta of earlier spans.
func changedRegions(updated string, matches []matchedEdit) []editRegion {
	regions := make([]editRegion, 0, len(matches))
	delta := 0
	for _, m := range matches {
		newStart := m.index + delta
		newEnd := newStart + len(m.text) // exclusive
		delta += len(m.text) - m.length
		lastByte := newEnd
		if lastByte > newStart {
			lastByte = newEnd - 1
		}
		regions = append(regions, editRegion{
			startLine: lineNumberAt(updated, newStart),
			endLine:   lineNumberAt(updated, lastByte),
		})
	}
	return regions
}

// lineNumberAt returns the 1-based line number containing byte offset in s.
func lineNumberAt(s string, offset int) int {
	if offset > len(s) {
		offset = len(s)
	}
	if offset < 0 {
		offset = 0
	}
	return strings.Count(s[:offset], "\n") + 1
}

func findEditText(content, oldText string) (textMatch, bool) {
	if idx := strings.Index(content, oldText); idx >= 0 {
		return textMatch{index: idx, length: len(oldText)}, true
	}
	fuzzyContent := normalizeForFuzzyMatchWithOffsets(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	if idx := strings.Index(fuzzyContent.text, fuzzyOldText); idx >= 0 {
		start, end := fuzzyContent.offsets[idx], fuzzyContent.offsets[idx+len(fuzzyOldText)]
		return textMatch{index: start, length: end - start, usedFuzzyMatch: true}, true
	}
	return textMatch{}, false
}

// findAllEditText returns every non-overlapping occurrence of oldText in content
// for replaceAll. It mirrors findEditText's direct-then-fuzzy strategy: a direct
// match wins (positions and length in raw content); otherwise it falls back to
// fuzzy-normalized matching, whose positions stay valid because the caller's base
// is already fuzzy-normalized whenever fuzzy matching is in play.
func findAllEditText(content, oldText string) []textMatch {
	if matches := indexAllEditText(content, oldText, false); len(matches) > 0 {
		return matches
	}
	fuzzyContent := normalizeForFuzzyMatchWithOffsets(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	normalized := indexAllEditText(fuzzyContent.text, fuzzyOldText, true)
	matches := make([]textMatch, 0, len(normalized))
	for _, match := range normalized {
		start := fuzzyContent.offsets[match.index]
		end := fuzzyContent.offsets[match.index+match.length]
		matches = append(matches, textMatch{index: start, length: end - start, usedFuzzyMatch: true})
	}
	return matches
}

// indexAllEditText collects non-overlapping match spans of oldText in content.
func indexAllEditText(content, oldText string, fuzzy bool) []textMatch {
	if oldText == "" {
		return nil
	}
	var matches []textMatch
	for off := 0; off < len(content); {
		idx := strings.Index(content[off:], oldText)
		if idx < 0 {
			break
		}
		matches = append(matches, textMatch{index: off + idx, length: len(oldText), usedFuzzyMatch: fuzzy})
		off += idx + len(oldText)
	}
	return matches
}

func normalizeForFuzzyMatch(content string) string {
	return normalizeForFuzzyMatchWithOffsets(content).text
}

type normalizedText struct {
	text    string
	offsets []int // normalized byte boundary -> original byte boundary
}

// normalizeForFuzzyMatchWithOffsets builds the comparison form plus a byte
// boundary map back to the untouched source. Fuzzy replacement can therefore
// splice only the matched region instead of rewriting the normalized file.
func normalizeForFuzzyMatchWithOffsets(content string) normalizedText {
	var out strings.Builder
	offsets := []int{0}
	appendRune := func(r rune, rawStart, rawEnd int) {
		normalized := normalizeFuzzyRune(r)
		encoded := []byte(string(normalized))
		out.Write(encoded)
		for i := range encoded {
			boundary := rawStart
			if i == len(encoded)-1 {
				boundary = rawEnd
			}
			offsets = append(offsets, boundary)
		}
	}
	lineStart := 0
	for lineStart <= len(content) {
		relNewline := strings.IndexByte(content[lineStart:], '\n')
		lineEnd := len(content)
		hasNewline := relNewline >= 0
		if hasNewline {
			lineEnd = lineStart + relNewline
		}
		trimmedEnd := lineEnd
		for trimmedEnd > lineStart {
			r, size := utf8.DecodeLastRuneInString(content[lineStart:trimmedEnd])
			if !unicode.IsSpace(r) {
				break
			}
			trimmedEnd -= size
		}
		for raw := lineStart; raw < trimmedEnd; {
			r, size := utf8.DecodeRuneInString(content[raw:trimmedEnd])
			appendRune(r, raw, raw+size)
			raw += size
		}
		// The normalized boundary at line end consumes ignored trailing space
		// if the match ends there.
		offsets[len(offsets)-1] = lineEnd
		if !hasNewline {
			break
		}
		appendRune('\n', lineEnd, lineEnd+1)
		lineStart = lineEnd + 1
	}
	return normalizedText{text: out.String(), offsets: offsets}
}

func normalizeFuzzyRune(r rune) rune {
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
}

func editNotFoundError(path, content, oldText string, editIndex, totalEdits int) error {
	var msg string
	if totalEdits == 1 {
		msg = fmt.Sprintf("could not find oldText in %s; oldText must match exactly including whitespace and newlines", path)
	} else {
		msg = fmt.Sprintf("could not find edits[%d].oldText in %s; oldText must match exactly including whitespace and newlines", editIndex, path)
	}
	if needle := firstNonEmptyLine(oldText); needle != "" {
		msg += fmt.Sprintf("; searched for %q", truncateHintText(needle))
	}
	if hint := nearestRegionHint(content, oldText); hint != "" {
		msg += "; " + hint
	}
	if hint := firstDivergentLineHint(content, oldText); hint != "" {
		msg += "; " + hint
	}
	msg += "; re-read the file, then re-issue with exact oldText; if the intent is to append or create, use write instead"
	return WithKind(fmt.Errorf("%s", msg), llm.ToolErrorEditOldTextNotFound)
}

func firstDivergentLineHint(content, oldText string) string {
	lineNo, _, score, ok := nearestSimilarLine(content, oldText)
	if !ok || score < 0.8 {
		return ""
	}
	expected := strings.Split(oldText, "\n")
	first := 0
	for first < len(expected) && strings.TrimSpace(expected[first]) == "" {
		first++
	}
	actual := strings.Split(content, "\n")
	for i := first; i < len(expected); i++ {
		actualIndex := lineNo - 1 + i - first
		if actualIndex >= len(actual) {
			return fmt.Sprintf("first divergence at expected line %d: expected %q, actual <end of file>", i+1, truncateHintText(expected[i]))
		}
		if normalizeForFuzzyMatch(expected[i]) != normalizeForFuzzyMatch(actual[actualIndex]) {
			return fmt.Sprintf("first divergence at file line %d: expected %q, actual %q", actualIndex+1, truncateHintText(expected[i]), truncateHintText(actual[actualIndex]))
		}
	}
	return ""
}

// nearestRegionHint renders up to 3 numbered lines centered on the content line
// most similar to oldText's first non-empty line, so the model can retarget the
// edit without a full re-read. Line numbers match read's numbering (the
// content is LF-normalized, which preserves line count). Returns "" when no
// line is similar enough to be useful.
func nearestRegionHint(content, oldText string) string {
	lineNo, _, score, ok := nearestSimilarLine(content, oldText)
	if !ok {
		return ""
	}
	lines := strings.Split(content, "\n")
	start := max(lineNo-editSnippetContextLines, 1)
	end := min(lineNo+editSnippetContextLines, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "nearest region (similarity %.2f) at L%d:", score, lineNo)
	for n := start; n <= end; n++ {
		fmt.Fprintf(&b, "\n%d\t%s", n, truncateHintText(strings.TrimRightFunc(lines[n-1], unicode.IsSpace)))
	}
	return b.String()
}

// nearestEditHintMaxLineLen skips candidate lines longer than this when scoring
// similarity: a minified/JSON line is never a useful "nearest line" and scoring
// it is wasteful.
const nearestEditHintMaxLineLen = 400

// nearestEditHintDisplayLen caps how much of the matched line is echoed back, so
// the hint stays a small addition to the error rather than re-dumping a line.
const nearestEditHintDisplayLen = 160

// nearestEditHintMinScore is the minimum bigram-Dice similarity a candidate line
// must reach to be reported, so unrelated lines are not offered as "similar".
const nearestEditHintMinScore = 0.34

// nearestSimilarLine finds the content line most similar to the first non-empty
// line of oldText, used to give edit's not-found error a recovery hint instead
// of forcing a re-read. Similarity is character-bigram Dice (stdlib only); the
// returned line number is 1-based and aligns with read's numbering because
// content is LF-normalized (line count preserved).
func nearestSimilarLine(content, oldText string) (lineNo int, text string, score float64, ok bool) {
	needle := firstNonEmptyLine(oldText)
	if needle == "" || content == "" {
		return 0, "", 0, false
	}
	needleBigrams := charBigrams(strings.ToLower(needle))
	if len(needleBigrams) == 0 {
		return 0, "", 0, false
	}

	lines := strings.Split(content, "\n")
	bestScore := 0.0
	bestIdx := -1
	for i, ln := range lines {
		cand := strings.TrimSpace(ln)
		if cand == "" || len(cand) > nearestEditHintMaxLineLen {
			continue
		}
		score := diceCoefficient(needleBigrams, charBigrams(strings.ToLower(cand)))
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestScore < nearestEditHintMinScore {
		return 0, "", 0, false
	}
	return bestIdx + 1, truncateHintText(strings.TrimSpace(lines[bestIdx])), bestScore, true
}

func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

func truncateHintText(s string) string {
	if len(s) <= nearestEditHintDisplayLen {
		return s
	}
	return s[:nearestEditHintDisplayLen] + "…"
}

// charBigrams returns the multiset of adjacent character pairs in s.
func charBigrams(s string) map[string]int {
	r := []rune(s)
	if len(r) < 2 {
		return nil
	}
	m := make(map[string]int, len(r)-1)
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])]++
	}
	return m
}

// diceCoefficient is the Sørensen–Dice similarity of two character-bigram
// multisets: 2*|intersection| / (|a|+|b|), in [0,1].
func diceCoefficient(a, b map[string]int) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	total := 0
	for _, n := range a {
		total += n
	}
	for _, n := range b {
		total += n
	}
	inter := 0
	for bg, na := range a {
		if nb, ok := b[bg]; ok {
			inter += min(na, nb)
		}
	}
	return 2 * float64(inter) / float64(total)
}

func editDuplicateError(path, content, oldText string, editIndex, totalEdits, occurrences int) error {
	lines := make([]string, 0, 5)
	for _, match := range findAllEditText(content, oldText) {
		if len(lines) == 5 {
			break
		}
		lines = append(lines, strconv.Itoa(lineNumberAt(content, match.index)))
	}
	lineHint := ""
	if len(lines) > 0 {
		lineHint = "; occurrences start at lines " + strings.Join(lines, ", ")
	}
	if totalEdits == 1 {
		return WithKind(fmt.Errorf("found %d occurrences of oldText in %s%s; provide more context to make it unique", occurrences, path, lineHint), llm.ToolErrorEditOldTextAmbiguous)
	}
	return WithKind(fmt.Errorf("found %d occurrences of edits[%d].oldText in %s%s; each oldText must be unique", occurrences, editIndex, path, lineHint), llm.ToolErrorEditOldTextAmbiguous)
}

// editSnippetContextLines is how many lines of context are shown above and below
// each changed line; editSnippetMaxRegions and editSnippetMaxBytes keep the
// verification snippet tightly capped so it nets positive against the bytes r57
// saved on reads (a small snippet here replaces a confirmatory full re-read).
const (
	editSnippetContextLines = 1
	editSnippetMaxRegions   = 3
	editSnippetMaxBytes     = 400
)

func formatEditSuccess(plans []plannedEditFile, replacements int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "edited %d file(s), %d replacement(s)", len(plans), replacements)
	for _, plan := range plans {
		fmt.Fprintf(&b, "\nM %s (%d replacement(s))", plan.path, plan.replacements)
		if snip := editSnippet(plan.body, plan.regions); snip != "" {
			b.WriteByte('\n')
			b.WriteString(snip)
		}
	}
	return b.String()
}

// editSnippet renders a small numbered snippet of the changed regions so the
// model can confirm the edit landed without a follow-up read. It expands
// each region with context, merges adjacent ones, caps the region count, and
// trims to a byte budget at a line boundary.
func editSnippet(body string, regions []editRegion) string {
	if len(regions) == 0 {
		return ""
	}
	lines := strings.Split(body, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop the trailing newline's empty segment
	}
	if len(lines) == 0 {
		return ""
	}

	type span struct{ start, end int } // 1-based inclusive
	var spans []span
	for _, r := range regions {
		s := max(r.startLine-editSnippetContextLines, 1)
		e := min(r.endLine+editSnippetContextLines, len(lines))
		if s > len(lines) || e < s {
			continue
		}
		spans = append(spans, span{s, e})
	}
	if len(spans) == 0 {
		return ""
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:1]
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp.start <= last.end+1 {
			if sp.end > last.end {
				last.end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}
	if len(merged) > editSnippetMaxRegions {
		merged = merged[:editSnippetMaxRegions]
	}

	var b strings.Builder
	for i, sp := range merged {
		if i > 0 {
			b.WriteString("\n…\n")
		}
		for n := sp.start; n <= sp.end; n++ {
			if n > sp.start {
				b.WriteByte('\n')
			}
			b.WriteString(strconv.Itoa(n))
			b.WriteByte('\t')
			b.WriteString(lines[n-1])
		}
	}
	out := b.String()
	if len(out) > editSnippetMaxBytes {
		cut := strings.LastIndexByte(out[:editSnippetMaxBytes], '\n')
		if cut <= 0 {
			cut = editSnippetMaxBytes
		}
		out = out[:cut] + "\n…"
	}
	return out
}
