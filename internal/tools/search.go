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
	"regexp/syntax"
	"sort"
	"strings"

	"harness/internal/llm"
)

const (
	searchContextLines = 4
	searchMaxMatches   = 60
	searchMaxFiles     = 12
	searchMaxGlobs     = 16
	searchOutputLines  = 200
)

const searchSchema = `{
  "type": "object",
  "properties": {
	"pattern": {"type": "string", "description": "Regular expression to search for."},
	"path": {"type": "string", "description": "File or directory to search (default current directory); the filesystem root is rejected as excessively broad."},
	"globs": {
	  "type": "array",
	  "items": {"type": "string"},
	  "maxItems": 16,
	  "description": "Optional include or exclude globs; prefix exclusions with !."
	},
	"case": {"type": "string", "enum": ["smart", "sensitive", "insensitive"], "description": "Case policy (default smart)."}
  },
	"required": ["pattern"]
}`

type searchTool struct {
	program string
}

type searchArgs struct {
	Pattern string   `json:"pattern"`
	Path    string   `json:"path"`
	Globs   []string `json:"globs"`
	Case    string   `json:"case"`
}

type searchQuery struct {
	Pattern string
	Path    string
	Globs   []string
	Case    string
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
	return "Search one file or directory for an RE2 regular expression and return host-bounded matching context. Escape punctuation to match it literally; patterns containing \\n automatically match across lines."
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
	query := searchQuery{
		Pattern: args.Pattern, Path: args.Path, Globs: args.Globs, Case: args.Case,
	}
	if _, err := os.Stat(query.Path); errors.Is(err, os.ErrNotExist) {
		return RunResult{}, fmt.Errorf("path: %w", notExistingPathError(query.Path, err))
	}
	var result searchResult
	if s.program != "" {
		result = s.searchRG(ctx, query)
	} else {
		result = searchGo(ctx, query)
	}
	if result.err != nil {
		return RunResult{}, classifySearchRuntimeError(result.err)
	}
	text, contextLines, contextLimited := renderSearchResult(result)
	metrics := map[string]int{
		"matches_shown": len(result.matches),
		"files_shown":   len(uniqueSearchPaths(result.matches)),
		"context_lines": contextLines,
	}
	if result.capped || result.omittedFiles > 0 {
		metrics["results_bounded"] = 1
	}
	if contextLimited {
		metrics["context_bounded"] = 1
	}
	return RunResult{Text: text, Metrics: metrics}, nil
}

func decodeSearchArgs(input json.RawMessage) (searchArgs, error) {
	var args searchArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return searchArgs{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return searchArgs{}, badArgs("pattern is required")
	}
	if args.Path == "" {
		args.Path = "."
	} else if strings.TrimSpace(args.Path) == "" {
		return searchArgs{}, badArgs("path must not be empty")
	}
	if isFilesystemRoot(args.Path) {
		return searchArgs{}, badArgs("path must not be the filesystem root; choose a narrower file or directory")
	}
	if len(args.Globs) > searchMaxGlobs {
		return searchArgs{}, badArgs("globs must contain at most %d items", searchMaxGlobs)
	}
	for i, glob := range args.Globs {
		if strings.TrimSpace(glob) == "" {
			return searchArgs{}, badArgs("globs[%d] must not be empty", i)
		}
	}
	switch args.Case {
	case "":
		args.Case = "smart"
	case "smart", "sensitive", "insensitive":
	default:
		return searchArgs{}, badArgs("case must be smart, sensitive, or insensitive")
	}
	if err := validateSearchPattern(searchQuery{Pattern: args.Pattern, Case: args.Case}); err != nil {
		return searchArgs{}, err
	}
	return args, nil
}

func isFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	return filepath.IsAbs(clean) && filepath.Dir(clean) == clean
}

func classifySearchRuntimeError(err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "regex parse error") || strings.Contains(message, "invalid regex") {
		return WithKind(fmt.Errorf("pattern: invalid regex: %v; escape regex punctuation to match it literally", err), llm.ToolErrorRegexInvalid)
	}
	return err
}

// validateSearchPattern pre-compiles a query's effective regex (respecting the
// smart-case (?i) prefix applied by the stdlib walker) so an invalid pattern
// fails fast with an actionable error instead of surfacing an rg stderr dump.
// Go's RE2 and ripgrep's default engine are both RE2-class, so divergence is
// exotic. The kinded regex_invalid class keeps the failure out of the
// invalid-arguments bucket.
func validateSearchPattern(query searchQuery) error {
	pattern := query.Pattern
	if query.Case == "insensitive" || query.Case == "smart" && strings.ToLower(query.Pattern) == query.Pattern {
		pattern = "(?i)" + pattern
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return WithKind(fmt.Errorf("pattern: invalid regex: %w; escape regex punctuation to match it literally", err), llm.ToolErrorRegexInvalid)
	}
	return nil
}

func (s searchTool) searchRG(ctx context.Context, args searchQuery) searchResult {
	return s.searchRGMode(ctx, args, false)
}

// rg rejects patterns that can match a newline in its default line-oriented
// mode and suggests --multiline; honor that instead of failing. Multiline
// stays opt-in per call because it also lets negated classes like [^x] match
// across lines, which would silently change line-mode results.
func (s searchTool) searchRGMode(ctx context.Context, args searchQuery, multiline bool) searchResult {
	argv := []string{
		"--json",
		"--line-number",
		"--sort=path",
		"--max-columns=" + ripgrepDefaultMaxColumns,
		"--max-columns-preview",
		"--max-filesize=" + ripgrepDefaultMaxFilesize,
	}
	if multiline {
		argv = append(argv, "--multiline")
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
	argv = append(argv, args.Path)

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
		if !files[path] && len(files) >= searchMaxFiles {
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
		if len(matches) >= searchMaxMatches {
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
		if !multiline && isRGLineModeNewlineError(stderr.String()) {
			return s.searchRGMode(ctx, args, true)
		}
		return searchResult{err: fmt.Errorf("rg failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))}
	}
	sortSearchMatches(matches)
	return searchResult{matches: matches, total: total, omittedFiles: len(omitted), capped: capped}
}

func searchGo(ctx context.Context, args searchQuery) searchResult {
	pattern := args.Pattern
	insensitive := args.Case == "insensitive" || args.Case == "smart" && strings.ToLower(args.Pattern) == args.Pattern
	if insensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return searchResult{err: fmt.Errorf("compile pattern: %w", err)}
	}
	multiline := patternMatchesAcrossLines(pattern)

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
			if filepath.Clean(path) != filepath.Clean(args.Path) && strings.HasPrefix(entry.Name(), ".") {
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
		record := func(start, end int) error {
			total++
			if !files[path] && len(files) >= searchMaxFiles {
				omitted[path] = true
				return nil
			}
			files[path] = true
			matches = append(matches, searchMatch{Path: path, Start: start, End: end})
			if len(matches) >= searchMaxMatches {
				capped = true
				return stop
			}
			return nil
		}
		if multiline {
			return multilineFileMatches(data, re, record)
		}
		line := 1
		for _, text := range strings.Split(string(data), "\n") {
			if re.MatchString(text) {
				if err := record(line, line); err != nil {
					return err
				}
			}
			line++
		}
		return nil
	}
	err = filepath.WalkDir(args.Path, visit)
	if !errors.Is(err, stop) && err != nil {
		return searchResult{err: err}
	}
	sortSearchMatches(matches)
	return searchResult{matches: matches, total: total, omittedFiles: len(omitted), capped: capped}
}

// patternMatchesAcrossLines reports whether the pattern can match a newline
// explicitly: a literal newline (any escape spelling) or an all-newline class
// like [\n]. Negated classes like [^x] stay line-bound, mirroring rg's
// line-oriented default. The rg path learns the same from rg's own rejection;
// the stdlib fallback must decide up front, so it inspects the parsed syntax.
func patternMatchesAcrossLines(pattern string) bool {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return false // unreachable: the pattern already compiled
	}
	return syntaxNeedsNewline(parsed)
}

func syntaxNeedsNewline(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if r == '\n' {
				return true
			}
		}
	case syntax.OpCharClass:
		allNewline := len(re.Rune) > 0
		for _, r := range re.Rune {
			if r != '\n' {
				allNewline = false
				break
			}
		}
		if allNewline {
			return true
		}
	}
	for _, sub := range re.Sub {
		if syntaxNeedsNewline(sub) {
			return true
		}
	}
	return false
}

var newlineBytes = []byte{'\n'}

// multilineFileMatches matches re against the whole file content and reports
// 1-based inclusive line spans. Match offsets advance monotonically, so line
// numbers are tracked incrementally instead of rescanning from the top.
func multilineFileMatches(data []byte, re *regexp.Regexp, record func(start, end int) error) error {
	line, scanned := 1, 0
	lineAt := func(offset int) int {
		line += bytes.Count(data[scanned:offset], newlineBytes)
		scanned = offset
		return line
	}
	offset := 0
	for offset <= len(data) {
		loc := re.FindIndex(data[offset:])
		if loc == nil {
			return nil
		}
		start, end := offset+loc[0], offset+loc[1]
		startLine := lineAt(start)
		endLine := startLine
		if end > start {
			endLine = lineAt(end - 1)
		}
		if err := record(startLine, endLine); err != nil {
			return err
		}
		if loc[1] == 0 {
			offset++ // empty match: advance to avoid stalling
		} else {
			offset += loc[1]
		}
	}
	return nil
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

func renderSearchResult(result searchResult) (string, int, bool) {
	if result.total == 0 {
		return "(no matches)", 0, false
	}
	return renderSearchContext(result.matches, result.total, result.omittedFiles, result.capped)
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

// isRGLineModeNewlineError reports whether rg refused the pattern because it
// can match a literal newline (any spelling: \n, [\n], \x0a). rg exits 2 with
// this diagnostic on stderr and points at --multiline; it is the only channel
// rg provides, so the exact message is matched rather than an error class.
func isRGLineModeNewlineError(stderr string) bool {
	return strings.Contains(stderr, `the literal "\n" is not allowed in a regex`)
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

func renderSearchContext(matches []searchMatch, total, omittedFiles int, capped bool) (string, int, bool) {
	grouped := map[string][]lineWindow{}
	for _, match := range matches {
		start := max(1, match.Start-searchContextLines)
		grouped[match.Path] = append(grouped[match.Path], lineWindow{Start: start, End: match.End + searchContextLines})
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
	selectedLines := 0
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
			selectedLines += lines
		}
		if remaining <= 0 {
			break
		}
	}
	if outputLimited {
		fmt.Fprintf(&b, "\n[context bounded at %d source lines; narrow the pattern or path]", searchOutputLines)
	}
	return strings.TrimRight(b.String(), "\n"), selectedLines, outputLimited
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
