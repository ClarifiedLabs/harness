package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	searchDefaultLines      = 20
	searchDefaultMaxMatches = 40
	searchDefaultMaxFiles   = 8
	searchMaxQueries        = 16
	searchMaxLines          = 100
	searchMaxMatches        = 200
	searchMaxFiles          = 50
	searchMaxGlobs          = 16
	searchOutputLines       = 400
)

const searchSchema = `{
  "type": "object",
  "properties": {
    "queries": {
      "type": "array",
      "minItems": 1,
      "maxItems": 16,
      "items": {
        "type": "object",
        "properties": {
          "pattern": {"type": "string", "description": "Regular expression to search for."},
          "paths": {
            "type": "array",
            "items": {"type": "string"},
            "maxItems": 16,
            "description": "Files or directories to search (default current directory)."
          },
          "globs": {
            "type": "array",
            "items": {"type": "string"},
            "maxItems": 16,
            "description": "Optional include or exclude globs; prefix exclusions with !."
          },
          "fixed_strings": {"type": "boolean", "description": "Treat pattern as literal text."},
          "case": {"type": "string", "enum": ["smart", "sensitive", "insensitive"], "description": "Case policy (default smart)."},
          "output": {"type": "string", "enum": ["context", "matches", "files", "count", "exists"], "description": "Result shape (default context)."},
          "context_lines": {"type": "integer", "minimum": 0, "maximum": 100, "description": "Lines around matches for context output (default 20)."},
          "max_matches": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum matches (default 40)."},
          "max_files": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum matching files (default 8)."}
        },
        "required": ["pattern"]
      }
    }
  },
  "required": ["queries"]
}`

type searchTool struct {
	program string
}

type searchArgs struct {
	Queries []searchQuery `json:"queries"`
}

type searchQuery struct {
	Pattern      string   `json:"pattern"`
	Paths        []string `json:"paths"`
	Globs        []string `json:"globs"`
	FixedStrings bool     `json:"fixed_strings"`
	Case         string   `json:"case"`
	Output       string   `json:"output"`
	ContextLines int      `json:"context_lines"`
	MaxMatches   int      `json:"max_matches"`
	MaxFiles     int      `json:"max_files"`
}

type searchMatch struct {
	Path  string
	Start int
	End   int
}

type searchResult struct {
	matches      []searchMatch
	total        int
	omittedFiles int
	capped       bool
	err          error
}

type lineWindow struct {
	Start int
	End   int
}

type taggedLineWindow struct {
	Path     string
	Start    int
	End      int
	QueryIDs []int
}

type searchContextPlan struct {
	windows       []taggedLineWindow
	selectedLines int
	limited       bool
}

type rgJSONText struct {
	Text  string `json:"text"`
	Bytes string `json:"bytes"`
}

type rgJSONEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgJSONText `json:"path"`
		Lines      rgJSONText `json:"lines"`
		LineNumber int        `json:"line_number"`
	} `json:"data"`
}

func (searchTool) Name() string { return "search" }

func (searchTool) Description() string {
	return "Search files with up to 16 independent queries in one call. Returns bounded context, matching lines, file names, counts, or existence; use paths and globs to narrow work. Prefer one batched call for repository orientation."
}

func (searchTool) Schema() json.RawMessage { return json.RawMessage(searchSchema) }

func (searchTool) ReadOnly(json.RawMessage) bool { return true }

func (s searchTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := s.RunResult(ctx, input)
	return result.Text, err
}

func (s searchTool) RunResult(ctx context.Context, input json.RawMessage) (RunResult, error) {
	args, err := decodeSearchArgs(input)
	if err != nil {
		return RunResult{}, err
	}
	for i := range args.Queries {
		for _, path := range args.Queries[i].Paths {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				return RunResult{}, fmt.Errorf("queries[%d].paths: %w", i, notExistingPathError(path, err))
			}
		}
	}

	results := make([]searchResult, len(args.Queries))
	var wg sync.WaitGroup
	for i := range args.Queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.program != "" {
				results[i] = s.searchRG(ctx, args.Queries[i])
				return
			}
			results[i] = searchGo(ctx, args.Queries[i])
		}()
	}
	wg.Wait()

	for i, result := range results {
		if result.err != nil {
			return RunResult{}, fmt.Errorf("query %d: %w", i+1, result.err)
		}
	}
	if len(results) > 1 {
		text, metrics := renderBatchedSearchResults(args.Queries, results)
		return RunResult{Text: text, Metrics: metrics}, nil
	}
	return RunResult{Text: renderSearchResult(args.Queries[0], results[0])}, nil
}

func decodeSearchArgs(input json.RawMessage) (searchArgs, error) {
	var args searchArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return searchArgs{}, err
	}
	if len(args.Queries) == 0 {
		return searchArgs{}, badArgs("queries is required and must be a non-empty array")
	}
	if len(args.Queries) > searchMaxQueries {
		return searchArgs{}, badArgs("queries must contain at most %d items", searchMaxQueries)
	}

	var raw struct {
		Queries []map[string]json.RawMessage `json:"queries"`
	}
	_ = json.Unmarshal(input, &raw)
	for i := range args.Queries {
		query := &args.Queries[i]
		if query.Pattern == "" {
			return searchArgs{}, badArgs("queries[%d].pattern is required", i)
		}
		if len(query.Paths) == 0 {
			query.Paths = []string{"."}
		}
		if len(query.Paths) > searchMaxGlobs {
			return searchArgs{}, badArgs("queries[%d].paths must contain at most %d items", i, searchMaxGlobs)
		}
		for j, path := range query.Paths {
			if strings.TrimSpace(path) == "" {
				return searchArgs{}, badArgs("queries[%d].paths[%d] must not be empty", i, j)
			}
		}
		if len(query.Globs) > searchMaxGlobs {
			return searchArgs{}, badArgs("queries[%d].globs must contain at most %d items", i, searchMaxGlobs)
		}
		for j, glob := range query.Globs {
			if strings.TrimSpace(glob) == "" {
				return searchArgs{}, badArgs("queries[%d].globs[%d] must not be empty", i, j)
			}
		}
		switch query.Case {
		case "":
			query.Case = "smart"
		case "smart", "sensitive", "insensitive":
		default:
			return searchArgs{}, badArgs("queries[%d].case must be smart, sensitive, or insensitive", i)
		}
		switch query.Output {
		case "":
			query.Output = "context"
		case "context", "matches", "files", "count", "exists":
		default:
			return searchArgs{}, badArgs("queries[%d].output must be context, matches, files, count, or exists", i)
		}
		switch {
		case query.ContextLines < 0 || query.ContextLines > searchMaxLines:
			return searchArgs{}, badArgs("queries[%d].context_lines must be between 0 and %d", i, searchMaxLines)
		case query.MaxMatches < 0 || query.MaxMatches > searchMaxMatches:
			return searchArgs{}, badArgs("queries[%d].max_matches must be between 1 and %d", i, searchMaxMatches)
		case query.MaxFiles < 0 || query.MaxFiles > searchMaxFiles:
			return searchArgs{}, badArgs("queries[%d].max_files must be between 1 and %d", i, searchMaxFiles)
		}
		if query.ContextLines == 0 {
			if i >= len(raw.Queries) {
				query.ContextLines = searchDefaultLines
			} else if _, ok := raw.Queries[i]["context_lines"]; !ok {
				query.ContextLines = searchDefaultLines
			}
		}
		if query.MaxMatches == 0 {
			query.MaxMatches = searchDefaultMaxMatches
		}
		if query.MaxFiles == 0 {
			query.MaxFiles = searchDefaultMaxFiles
		}
	}
	return args, nil
}

func (s searchTool) searchRG(ctx context.Context, args searchQuery) searchResult {
	argv := []string{
		"--json",
		"--line-number",
		"--sort=path",
		"--max-columns=" + ripgrepDefaultMaxColumns,
		"--max-columns-preview",
		"--max-filesize=" + ripgrepDefaultMaxFilesize,
	}
	if args.FixedStrings {
		argv = append(argv, "--fixed-strings")
	}
	switch args.Case {
	case "smart":
		argv = append(argv, "--smart-case")
	case "sensitive":
		argv = append(argv, "--case-sensitive")
	case "insensitive":
		argv = append(argv, "--ignore-case")
	}
	for _, glob := range args.Globs {
		argv = append(argv, "--glob", glob)
	}
	argv = append(argv, "--", args.Pattern)
	argv = append(argv, args.Paths...)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.program, argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return searchResult{err: err}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return searchResult{err: fmt.Errorf("start rg: %w", err)}
	}

	files := map[string]bool{}
	omitted := map[string]bool{}
	var matches []searchMatch
	total := 0
	capped := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event rgJSONEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			cancel()
			_ = cmd.Wait()
			return searchResult{matches: matches, total: total, omittedFiles: len(omitted), capped: capped, err: fmt.Errorf("decode rg JSON: %w", err)}
		}
		if event.Type != "match" {
			continue
		}
		total++
		path, err := decodeRGJSONText(event.Data.Path)
		if err != nil {
			cancel()
			_ = cmd.Wait()
			return searchResult{err: fmt.Errorf("decode rg path: %w", err)}
		}
		if !files[path] && len(files) >= args.MaxFiles {
			omitted[path] = true
			continue
		}
		files[path] = true
		lineText, err := decodeRGJSONText(event.Data.Lines)
		if err != nil {
			cancel()
			_ = cmd.Wait()
			return searchResult{err: fmt.Errorf("decode rg lines: %w", err)}
		}
		span := strings.Count(lineText, "\n")
		if span == 0 {
			span = 1
		}
		start := max(1, event.Data.LineNumber)
		matches = append(matches, searchMatch{Path: path, Start: start, End: start + span - 1})
		if len(matches) >= args.MaxMatches || args.Output == "exists" {
			capped = true
			cancel()
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil && ctx.Err() == nil && !capped {
		return searchResult{err: scanErr}
	}
	if ctx.Err() != nil {
		return searchResult{err: ctx.Err()}
	}
	if waitErr != nil && !capped {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			return searchResult{}
		}
		return searchResult{err: fmt.Errorf("rg failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))}
	}
	sortSearchMatches(matches)
	return searchResult{matches: matches, total: total, omittedFiles: len(omitted), capped: capped}
}

func searchGo(ctx context.Context, args searchQuery) searchResult {
	pattern := args.Pattern
	if args.FixedStrings {
		pattern = regexp.QuoteMeta(pattern)
	}
	insensitive := args.Case == "insensitive" || args.Case == "smart" && strings.ToLower(args.Pattern) == args.Pattern
	if insensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return searchResult{err: fmt.Errorf("compile pattern: %w", err)}
	}

	files := map[string]bool{}
	omitted := map[string]bool{}
	var matches []searchMatch
	total := 0
	capped := false
	stop := errors.New("search complete")
	visit := func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !matchesSearchGlobs(path, args.Globs) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		line := 1
		for _, text := range strings.Split(string(data), "\n") {
			if re.MatchString(text) {
				total++
				if !files[path] && len(files) >= args.MaxFiles {
					omitted[path] = true
				} else {
					files[path] = true
					matches = append(matches, searchMatch{Path: path, Start: line, End: line})
					if len(matches) >= args.MaxMatches || args.Output == "exists" {
						capped = true
						return stop
					}
				}
			}
			line++
		}
		return nil
	}
	for _, root := range args.Paths {
		err := filepath.WalkDir(root, visit)
		if errors.Is(err, stop) {
			break
		}
		if err != nil {
			return searchResult{err: err}
		}
	}
	sortSearchMatches(matches)
	return searchResult{matches: matches, total: total, omittedFiles: len(omitted), capped: capped}
}

func matchesSearchGlobs(path string, globs []string) bool {
	if len(globs) == 0 {
		return true
	}
	slashPath := filepath.ToSlash(path)
	included := false
	haveInclude := false
	for _, glob := range globs {
		exclude := strings.HasPrefix(glob, "!")
		if exclude {
			glob = strings.TrimPrefix(glob, "!")
		} else {
			haveInclude = true
		}
		matched, _ := filepath.Match(glob, slashPath)
		if !matched {
			matched, _ = filepath.Match(glob, filepath.Base(slashPath))
		}
		if matched && exclude {
			return false
		}
		if matched {
			included = true
		}
	}
	return included || !haveInclude
}

const (
	searchMetricContextLinesBeforeDedupe = "context_lines_before_dedupe"
	searchMetricUniqueContextLines       = "unique_context_lines"
)

func renderBatchedSearchResults(queries []searchQuery, results []searchResult) (string, map[string]int) {
	var b strings.Builder
	var shared []taggedLineWindow
	selectedLines := 0
	for i, result := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## query %d: %s\n", i+1, queries[i].Pattern)
		query := queries[i]
		if query.Output != "context" && query.Output != "matches" || result.total == 0 {
			b.WriteString(renderSearchResult(query, result))
			continue
		}
		contextLines := query.ContextLines
		if query.Output == "matches" {
			contextLines = 0
		}
		b.WriteString(renderSearchContextSummary(result.matches, result.total, result.omittedFiles, result.capped))
		fmt.Fprintf(&b, "\ncontext: included in shared source below (query %d)", i+1)
		plan := planSearchContext(result.matches, contextLines, i+1)
		if plan.limited {
			fmt.Fprintf(&b, "\n[query %d context truncated at %d source lines; narrow the pattern or bounds]", i+1, searchOutputLines)
		}
		shared = append(shared, plan.windows...)
		selectedLines += plan.selectedLines
	}
	if len(shared) == 0 {
		return b.String(), nil
	}

	b.WriteString("\n\n## shared source context")
	uniqueLines := 0
	for _, window := range mergeTaggedLineWindows(shared) {
		body, lines, err := readSearchContextWindow(window.Path, lineWindow{Start: window.Start, End: window.End})
		fmt.Fprintf(&b, "\n\n==> %s:%d-%d (queries: %s) <==\n",
			window.Path, window.Start, window.End, formatQueryIDs(window.QueryIDs))
		if err != nil {
			fmt.Fprintf(&b, "error: %v", err)
			continue
		}
		b.WriteString(body)
		uniqueLines += lines
	}
	duplicates := max(selectedLines-uniqueLines, 0)
	fmt.Fprintf(&b, "\n\n[shared context: %d unique source lines; %d duplicate lines suppressed across queries]",
		uniqueLines, duplicates)
	return b.String(), map[string]int{
		searchMetricContextLinesBeforeDedupe: selectedLines,
		searchMetricUniqueContextLines:       uniqueLines,
	}
}

func renderSearchContextSummary(matches []searchMatch, total, omittedFiles int, capped bool) string {
	paths := uniqueSearchPaths(matches)
	var b strings.Builder
	fmt.Fprintf(&b, "matches: %d shown", len(matches))
	if capped {
		fmt.Fprintf(&b, " of at least %d", total)
	} else {
		fmt.Fprintf(&b, " of %d", total)
	}
	fmt.Fprintf(&b, " across %d files", len(paths))
	if omittedFiles > 0 {
		fmt.Fprintf(&b, " (%d additional files omitted)", omittedFiles)
	}
	return b.String()
}

func planSearchContext(matches []searchMatch, contextLines, queryID int) searchContextPlan {
	grouped := map[string][]lineWindow{}
	for _, match := range matches {
		start := max(1, match.Start-contextLines)
		grouped[match.Path] = append(grouped[match.Path], lineWindow{Start: start, End: match.End + contextLines})
	}
	paths := make([]string, 0, len(grouped))
	for path := range grouped {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	remaining := searchOutputLines
	var plan searchContextPlan
	for _, path := range paths {
		for _, window := range mergeLineWindows(grouped[path]) {
			if remaining <= 0 {
				plan.limited = true
				return plan
			}
			if size := window.End - window.Start + 1; size > remaining {
				window.End = window.Start + remaining - 1
				plan.limited = true
			}
			_, lines, _ := readSearchContextWindow(path, window)
			plan.windows = append(plan.windows, taggedLineWindow{
				Path:     path,
				Start:    window.Start,
				End:      window.End,
				QueryIDs: []int{queryID},
			})
			plan.selectedLines += lines
			remaining -= lines
		}
	}
	return plan
}

func mergeTaggedLineWindows(windows []taggedLineWindow) []taggedLineWindow {
	if len(windows) == 0 {
		return nil
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Path != windows[j].Path {
			return windows[i].Path < windows[j].Path
		}
		if windows[i].Start != windows[j].Start {
			return windows[i].Start < windows[j].Start
		}
		return windows[i].End < windows[j].End
	})
	out := []taggedLineWindow{cloneTaggedLineWindow(windows[0])}
	for _, next := range windows[1:] {
		last := &out[len(out)-1]
		if next.Path == last.Path && next.Start <= last.End+1 {
			if next.End > last.End {
				last.End = next.End
			}
			last.QueryIDs = mergeQueryIDs(last.QueryIDs, next.QueryIDs)
			continue
		}
		out = append(out, cloneTaggedLineWindow(next))
	}
	return out
}

func cloneTaggedLineWindow(window taggedLineWindow) taggedLineWindow {
	window.QueryIDs = append([]int(nil), window.QueryIDs...)
	return window
}

func mergeQueryIDs(left, right []int) []int {
	seen := make(map[int]bool, len(left)+len(right))
	for _, id := range left {
		seen[id] = true
	}
	for _, id := range right {
		seen[id] = true
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func formatQueryIDs(ids []int) string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(values, ", ")
}

func renderSearchResult(args searchQuery, result searchResult) string {
	if result.total == 0 {
		if args.Output == "exists" {
			return "false"
		}
		return "(no matches)"
	}
	switch args.Output {
	case "context":
		return renderSearchContext(result.matches, result.total, result.omittedFiles, result.capped, args.ContextLines)
	case "matches":
		return renderSearchContext(result.matches, result.total, result.omittedFiles, result.capped, 0)
	case "files":
		return renderSearchFiles(result)
	case "count":
		return renderSearchCounts(result)
	case "exists":
		match := result.matches[0]
		return fmt.Sprintf("true: %s:%d", match.Path, match.Start)
	default:
		panic("validated search output")
	}
}

func renderSearchFiles(result searchResult) string {
	paths := uniqueSearchPaths(result.matches)
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte('\n')
	}
	if result.omittedFiles > 0 || result.capped {
		fmt.Fprintf(&b, "[results bounded; %d matches observed", result.total)
		if result.omittedFiles > 0 {
			fmt.Fprintf(&b, ", %d additional files omitted", result.omittedFiles)
		}
		b.WriteString("]\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderSearchCounts(result searchResult) string {
	counts := map[string]int{}
	for _, match := range result.matches {
		counts[match.Path]++
	}
	paths := make([]string, 0, len(counts))
	for path := range counts {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&b, "%d\t%s\n", counts[path], path)
	}
	if result.capped || result.omittedFiles > 0 {
		fmt.Fprintf(&b, "[partial counts; %d matches observed]\n", result.total)
	}
	return strings.TrimRight(b.String(), "\n")
}

func uniqueSearchPaths(matches []searchMatch) []string {
	set := map[string]bool{}
	for _, match := range matches {
		set[match.Path] = true
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sortSearchMatches(matches []searchMatch) {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		if matches[i].Start != matches[j].Start {
			return matches[i].Start < matches[j].Start
		}
		return matches[i].End < matches[j].End
	})
}

func decodeRGJSONText(value rgJSONText) (string, error) {
	if value.Text != "" {
		return value.Text, nil
	}
	if value.Bytes == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value.Bytes)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func renderSearchContext(matches []searchMatch, total, omittedFiles int, capped bool, contextLines int) string {
	grouped := map[string][]lineWindow{}
	for _, match := range matches {
		start := max(1, match.Start-contextLines)
		grouped[match.Path] = append(grouped[match.Path], lineWindow{Start: start, End: match.End + contextLines})
	}
	paths := make([]string, 0, len(grouped))
	for path := range grouped {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString(renderSearchContextSummary(matches, total, omittedFiles, capped))
	b.WriteByte('\n')

	remaining := searchOutputLines
	outputLimited := false
	for _, path := range paths {
		windows := mergeLineWindows(grouped[path])
		for _, window := range windows {
			if remaining <= 0 {
				outputLimited = true
				break
			}
			if size := window.End - window.Start + 1; size > remaining {
				window.End = window.Start + remaining - 1
				outputLimited = true
			}
			body, lines, err := readSearchContextWindow(path, window)
			fmt.Fprintf(&b, "\n==> %s:%d-%d <==\n", path, window.Start, window.End)
			if err != nil {
				fmt.Fprintf(&b, "error: %v\n", err)
				continue
			}
			b.WriteString(body)
			b.WriteByte('\n')
			remaining -= lines
		}
		if remaining <= 0 {
			break
		}
	}
	if outputLimited {
		fmt.Fprintf(&b, "\n[context truncated at %d source lines; narrow the pattern or bounds]", searchOutputLines)
	}
	return strings.TrimRight(b.String(), "\n")
}

func mergeLineWindows(windows []lineWindow) []lineWindow {
	if len(windows) == 0 {
		return nil
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Start != windows[j].Start {
			return windows[i].Start < windows[j].Start
		}
		return windows[i].End < windows[j].End
	})
	out := []lineWindow{windows[0]}
	for _, next := range windows[1:] {
		last := &out[len(out)-1]
		if next.Start <= last.End+1 {
			if next.End > last.End {
				last.End = next.End
			}
			continue
		}
		out = append(out, next)
	}
	return out
}

func readSearchContextWindow(path string, window lineWindow) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	limit := window.End - window.Start + 1
	lines, _, _, err := readWindowLines(f, window.Start, limit)
	if err != nil {
		return "", 0, err
	}
	if len(lines) == 0 {
		return "(no source lines in requested window)", 0, nil
	}
	return numberLines(lines, window.Start), len(lines), nil
}
