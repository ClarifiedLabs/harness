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
            "description": "One or more targeted replacements. Each oldText must be unique in the original file and edits must not overlap, unless replaceAll is set.",
            "items": {
              "type": "object",
              "properties": {
                "oldText": {"type": "string", "description": "Exact text to replace. Must be unique in the original file unless replaceAll is true."},
                "newText": {"type": "string", "description": "Replacement text; empty string deletes oldText."},
                "replaceAll": {"type": "boolean", "description": "When true, replace every occurrence of oldText instead of requiring a unique match (default false)."}
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
	OldText    string
	NewText    string
	ReplaceAll bool
}

type rawEditFile struct {
	Path  string         `json:"path"`
	Edits []rawEditBlock `json:"edits"`
}

type rawEditBlock struct {
	OldText    *string `json:"oldText"`
	NewText    *string `json:"newText"`
	ReplaceAll bool    `json:"replaceAll"`
}

type plannedEditFile struct {
	path         string
	content      string
	body         string // LF-normalized updated content, used to render the snippet
	mode         fs.FileMode
	replacements int
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

func (edit) Description() string {
	return "Edit one or more files with exact-text replacements. Provide a JSON object with files[]. Each oldText must be unique and non-overlapping in the original file."
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
			edits = append(edits, editBlock{OldText: *block.OldText, NewText: *block.NewText, ReplaceAll: block.ReplaceAll})
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
		updated, replacements, regions, err := applyEditBlocks(text, file.Edits, file.Path)
		if err != nil {
			return nil, 0, err
		}
		plans = append(plans, plannedEditFile{
			path:         file.Path,
			content:      bom + restoreLineEndings(updated, ending),
			body:         updated,
			mode:         info.Mode(),
			replacements: replacements,
			regions:      regions,
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

func applyEditBlocks(content string, edits []editBlock, path string) (string, int, []editRegion, error) {
	normalizedContent := normalizeToLF(content)
	normalizedEdits := make([]editBlock, len(edits))
	for i, block := range edits {
		normalizedEdits[i] = editBlock{
			OldText:    normalizeToLF(block.OldText),
			NewText:    normalizeToLF(block.NewText),
			ReplaceAll: block.ReplaceAll,
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
		if block.ReplaceAll {
			// Replace every occurrence: bypass the uniqueness check and record one
			// span per non-overlapping match. Zero matches is still a not-found.
			found := findAllEditText(base, block.OldText)
			if len(found) == 0 {
				return "", 0, nil, editNotFoundError(path, normalizedContent, block.OldText, i, len(normalizedEdits))
			}
			for _, m := range found {
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
		match, ok := findEditText(base, block.OldText)
		if !ok {
			return "", 0, nil, editNotFoundError(path, normalizedContent, block.OldText, i, len(normalizedEdits))
		}
		count := countEditOccurrences(base, block.OldText)
		if count > 1 {
			return "", 0, nil, editDuplicateError(path, i, len(normalizedEdits), count)
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
			// Exempt the overlap guard only when both spans come from the SAME
			// replaceAll block: indexAllEditText advances disjointly, so a block's
			// own occurrences never overlap each other. Spans from different edits —
			// a replaceAll span overlapping another replaceAll or a normal edit —
			// must still raise the overlap error, or the right-to-left splice below
			// would silently double-apply/clobber the shared range.
			if (prev.replaceAll || next.replaceAll) && prev.order == next.order {
				continue
			}
			return "", 0, nil, fmt.Errorf("edits[%d] and edits[%d] overlap in %s; merge them into one edit or target disjoint regions", prev.order, next.order, path)
		}
	}

	updated := base
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		updated = updated[:match.index] + match.text + updated[match.index+match.length:]
	}
	if updated == base {
		return "", 0, nil, fmt.Errorf("no changes made to %s; replacements produced identical content", path)
	}
	return updated, len(matches), changedRegions(updated, matches), nil
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
	fuzzyContent := normalizeForFuzzyMatch(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	if idx := strings.Index(fuzzyContent, fuzzyOldText); idx >= 0 {
		return textMatch{index: idx, length: len(fuzzyOldText), usedFuzzyMatch: true}, true
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
	fuzzyContent := normalizeForFuzzyMatch(content)
	fuzzyOldText := normalizeForFuzzyMatch(oldText)
	return indexAllEditText(fuzzyContent, fuzzyOldText, true)
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

func editNotFoundError(path, content, oldText string, editIndex, totalEdits int) error {
	var msg string
	if totalEdits == 1 {
		msg = fmt.Sprintf("could not find oldText in %s; oldText must match exactly including whitespace and newlines", path)
	} else {
		msg = fmt.Sprintf("could not find edits[%d].oldText in %s; oldText must match exactly including whitespace and newlines", editIndex, path)
	}
	if n, text, ok := nearestSimilarLine(content, oldText); ok {
		msg += fmt.Sprintf("; nearest similar line is L%d: %s", n, text)
	}
	return fmt.Errorf("%s", msg)
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
// returned line number is 1-based and aligns with read_file's numbering because
// content is LF-normalized (line count preserved).
func nearestSimilarLine(content, oldText string) (lineNo int, text string, ok bool) {
	needle := firstNonEmptyLine(oldText)
	if needle == "" || content == "" {
		return 0, "", false
	}
	needleBigrams := charBigrams(strings.ToLower(needle))
	if len(needleBigrams) == 0 {
		return 0, "", false
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
		return 0, "", false
	}
	return bestIdx + 1, truncateHintText(strings.TrimSpace(lines[bestIdx])), true
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

func editDuplicateError(path string, editIndex, totalEdits, occurrences int) error {
	if totalEdits == 1 {
		return fmt.Errorf("found %d occurrences of oldText in %s; provide more context to make it unique", occurrences, path)
	}
	return fmt.Errorf("found %d occurrences of edits[%d].oldText in %s; each oldText must be unique", occurrences, editIndex, path)
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
// model can confirm the edit landed without a follow-up read_file. It expands
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
