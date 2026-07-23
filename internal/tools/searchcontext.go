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
	"sort"
	"strings"
)

const (
	searchContextDefaultLines      = 20
	searchContextDefaultMaxMatches = 40
	searchContextDefaultMaxFiles   = 8
	searchContextMaxLines          = 100
	searchContextMaxMatches        = 200
	searchContextMaxFiles          = 50
	searchContextMaxGlobs          = 16
	searchContextOutputLines       = 400
)

const searchContextSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression to search for."},
    "path": {"type": "string", "description": "File or directory to search (default: current directory)."},
    "globs": {
      "type": "array",
      "items": {"type": "string"},
      "maxItems": 16,
      "description": "Optional rg glob filters."
    },
    "fixed_strings": {"type": "boolean", "description": "Treat pattern as a literal string instead of a regular expression."},
    "context_lines": {"type": "integer", "minimum": 0, "maximum": 100, "description": "Lines before and after each match (default 20)."},
    "max_matches": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum matches to render (default 40)."},
    "max_files": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum matching files to render (default 8)."}
  },
  "required": ["pattern"]
}`

type searchContext struct {
	program string
}

type searchContextArgs struct {
	Pattern      string   `json:"pattern"`
	Path         string   `json:"path"`
	Globs        []string `json:"globs"`
	FixedStrings bool     `json:"fixed_strings"`
	ContextLines int      `json:"context_lines"`
	MaxMatches   int      `json:"max_matches"`
	MaxFiles     int      `json:"max_files"`
}

type searchMatch struct {
	Path  string
	Start int
	End   int
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

func (searchContext) Name() string { return "search_context" }

func (searchContext) Description() string {
	return "Targeted code lookup after broad discovery: return bounded, merged line-numbered source around a known symbol, call site, or text match. Use this instead of rg followed by read_file. For broad multi-concept repository orientation, use one raw rg with a combined pattern first. Preserve returned paths and symbols as citations."
}

func (searchContext) Schema() json.RawMessage { return json.RawMessage(searchContextSchema) }

func (searchContext) ReadOnly(json.RawMessage) bool { return true }

func (s searchContext) Run(ctx context.Context, input json.RawMessage) (string, error) {
	args, err := decodeSearchContextArgs(input)
	if err != nil {
		return "", err
	}
	matches, total, omittedFiles, capped, err := s.search(ctx, args)
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "(no matches)", nil
	}
	return renderSearchContext(matches, total, omittedFiles, capped, args.ContextLines), nil
}

func decodeSearchContextArgs(input json.RawMessage) (searchContextArgs, error) {
	var args searchContextArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return searchContextArgs{}, err
	}
	if args.Pattern == "" {
		return searchContextArgs{}, badArgs("pattern is required")
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	if len(args.Globs) > searchContextMaxGlobs {
		return searchContextArgs{}, badArgs("globs must contain at most %d items", searchContextMaxGlobs)
	}
	for i, glob := range args.Globs {
		if strings.TrimSpace(glob) == "" {
			return searchContextArgs{}, badArgs("globs[%d] must not be empty", i)
		}
	}
	switch {
	case args.ContextLines < 0 || args.ContextLines > searchContextMaxLines:
		return searchContextArgs{}, badArgs("context_lines must be between 0 and %d", searchContextMaxLines)
	case args.MaxMatches < 0 || args.MaxMatches > searchContextMaxMatches:
		return searchContextArgs{}, badArgs("max_matches must be between 1 and %d", searchContextMaxMatches)
	case args.MaxFiles < 0 || args.MaxFiles > searchContextMaxFiles:
		return searchContextArgs{}, badArgs("max_files must be between 1 and %d", searchContextMaxFiles)
	}
	if args.ContextLines == 0 {
		// Zero is both the JSON zero value and a useful explicit setting. Detect
		// explicit zero separately so omission can retain the documented default.
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(input, &raw)
		if _, ok := raw["context_lines"]; !ok {
			args.ContextLines = searchContextDefaultLines
		}
	}
	if args.MaxMatches == 0 {
		args.MaxMatches = searchContextDefaultMaxMatches
	}
	if args.MaxFiles == 0 {
		args.MaxFiles = searchContextDefaultMaxFiles
	}
	return args, nil
}

func (s searchContext) search(ctx context.Context, args searchContextArgs) ([]searchMatch, int, int, bool, error) {
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
	for _, glob := range args.Globs {
		argv = append(argv, "--glob", glob)
	}
	argv = append(argv, "--", args.Pattern, args.Path)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.program, argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, 0, false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, 0, 0, false, fmt.Errorf("start rg: %w", err)
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
			return nil, total, len(omitted), capped, fmt.Errorf("decode rg JSON: %w", err)
		}
		if event.Type != "match" {
			continue
		}
		total++
		path, err := decodeRGJSONText(event.Data.Path)
		if err != nil {
			cancel()
			_ = cmd.Wait()
			return nil, total, len(omitted), capped, fmt.Errorf("decode rg path: %w", err)
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
			return nil, total, len(omitted), capped, fmt.Errorf("decode rg lines: %w", err)
		}
		span := strings.Count(lineText, "\n")
		if span == 0 {
			span = 1
		}
		start := event.Data.LineNumber
		if start <= 0 {
			start = 1
		}
		matches = append(matches, searchMatch{Path: path, Start: start, End: start + span - 1})
		if len(matches) >= args.MaxMatches {
			capped = true
			cancel()
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil && ctx.Err() == nil && !capped {
		return nil, total, len(omitted), capped, scanErr
	}
	if ctx.Err() != nil {
		return nil, total, len(omitted), capped, ctx.Err()
	}
	if waitErr != nil && !capped {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, total, len(omitted), false, nil
		}
		return nil, total, len(omitted), false, fmt.Errorf("rg failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		if matches[i].Start != matches[j].Start {
			return matches[i].Start < matches[j].Start
		}
		return matches[i].End < matches[j].End
	})
	return matches, total, len(omitted), capped, nil
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
	b.WriteByte('\n')

	remaining := searchContextOutputLines
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
		fmt.Fprintf(&b, "\n[context truncated at %d source lines; narrow the pattern or bounds]", searchContextOutputLines)
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
