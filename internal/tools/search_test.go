package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestSearchDescriptionsPreferTypedSearch(t *testing.T) {
	if description := (searchTool{}).Description(); !strings.Contains(description, "16 independent queries") || !strings.Contains(description, "batched call") {
		t.Fatalf("search description does not state the preferred flow: %q", description)
	}
	if description := (ripgrep{}).Description(); !strings.Contains(description, "Prefer the typed search tool") {
		t.Fatalf("rg description does not route ordinary lookup: %q", description)
	}
}

func TestSearchContextReturnsMergedNumberedWindows(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(source, []byte("one\ntwo\nthree\nfour\nfive\nsix\nseven\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	program := fakeRG(t, []string{
		rgMatchJSON(t, source, 3, "three\n"),
		rgMatchJSON(t, source, 5, "five\n"),
	}, 0)
	tool := searchTool{program: program}
	out, err := tool.Run(context.Background(), searchTestInput(`{
		"pattern":"needle",
		"paths":["."],
		"context_lines":1
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"matches: 2 shown of 2 across 1 files",
		source + ":2-6",
		"2\ttwo",
		"6\tsix",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Count(out, "==>") != 1 {
		t.Fatalf("overlapping windows were not merged:\n%s", out)
	}
}

func TestSearchContextBoundsFilesAndMatches(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.go")
	second := filepath.Join(dir, "b.go")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("match\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	program := fakeRG(t, []string{
		rgMatchJSON(t, first, 1, "match\n"),
		rgMatchJSON(t, second, 1, "match\n"),
	}, 0)
	tool := searchTool{program: program}
	out, err := tool.Run(context.Background(), searchTestInput(`{
		"pattern":"match",
		"context_lines":0,
		"max_files":1,
		"max_matches":10
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 additional files omitted") || strings.Contains(out, second+":") {
		t.Fatalf("file bound not reported:\n%s", out)
	}
}

func TestSearchContextDecodesBytePaths(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(source, []byte("match\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path64 := base64.StdEncoding.EncodeToString([]byte(source))
	event := `{"type":"match","data":{"path":{"bytes":"` + path64 + `"},"lines":{"text":"match\n"},"line_number":1}}`
	tool := searchTool{program: fakeRG(t, []string{event}, 0)}
	out, err := tool.Run(context.Background(), searchTestInput(`{"pattern":"match","context_lines":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, source+":1-1") {
		t.Fatalf("decoded path absent:\n%s", out)
	}
}

// An invalid regex fails fast at argument decode with an actionable message
// and the regex_invalid kind; it never reaches rg or the stdlib walker.
func TestSearchInvalidRegexPrevalidated(t *testing.T) {
	_, err := (searchTool{}).Run(context.Background(), searchTestInput(`{"pattern":"(["}`))
	if err == nil {
		t.Fatal("expected invalid regex error")
	}
	if got := KindOf(err); got != llm.ToolErrorRegexInvalid {
		t.Errorf("kind = %q, want %q", got, llm.ToolErrorRegexInvalid)
	}
	for _, want := range []string{"queries[0].pattern", "invalid regex", "fixed_strings: true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// The same metacharacters are legal under fixed_strings and must not be
// pre-validated as a regex.
func TestSearchFixedStringsSkipsRegexValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a ([ b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{
		"queries": []any{map[string]any{
			"pattern":       "([",
			"paths":         []string{dir},
			"fixed_strings": true,
			"output":        "files",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := (searchTool{}).Run(context.Background(), input)
	if err != nil {
		t.Fatalf("fixed_strings search failed: %v", err)
	}
	if !strings.Contains(out, "f.txt") {
		t.Errorf("expected literal match, got %q", out)
	}
}

// The kinded decode error must keep its regex_invalid class through
// Registry.Dispatch rather than collapsing into invalid_args.
func TestSearchInvalidRegexKindSurvivesDispatch(t *testing.T) {
	reg := &Registry{}
	reg.Register(searchTool{})
	res := reg.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "search", Input: searchTestInput(`{"pattern":"(["}`)})
	if !res.IsError || res.ErrorKind != llm.ToolErrorRegexInvalid {
		t.Fatalf("result = %+v, want is_error with kind %q", res, llm.ToolErrorRegexInvalid)
	}
	if !strings.Contains(res.Text, "invalid regex") || !strings.Contains(res.Text, "fixed_strings: true") {
		t.Fatalf("dispatch text = %q", res.Text)
	}
}

func TestSearchContextNoMatchesAndRGFailure(t *testing.T) {
	t.Run("no matches", func(t *testing.T) {
		tool := searchTool{program: fakeRG(t, nil, 1)}
		out, err := tool.Run(context.Background(), searchTestInput(`{"pattern":"none"}`))
		if err != nil || out != "(no matches)" {
			t.Fatalf("Run = %q, %v", out, err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		tool := searchTool{program: fakeRG(t, []string{"not json"}, 2)}
		if _, err := tool.Run(context.Background(), searchTestInput(`{"pattern":"x"}`)); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestSearchContextStopsAtMatchBound(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(source, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := searchTool{program: fakeRG(t, []string{
		rgMatchJSON(t, source, 1, "one\n"),
		rgMatchJSON(t, source, 2, "two\n"),
	}, 0)}
	out, err := tool.Run(context.Background(), searchTestInput(`{"pattern":"x","context_lines":0,"max_matches":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 shown of at least 1") || strings.Contains(out, "2\ttwo") {
		t.Fatalf("match bound not applied:\n%s", out)
	}
}

func TestSearchContextAgainstHostRG(t *testing.T) {
	program, ok := ripgrepProgram()
	if !ok {
		t.Skip("rg unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(source, []byte("package sample\n\nfunc Needle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := searchTool{program: program}
	input, err := json.Marshal(searchArgs{Queries: []searchQuery{{
		Pattern:      "Needle",
		Paths:        []string{dir},
		ContextLines: 1,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "func Needle()") {
		t.Fatalf("real rg output missing match:\n%s", out)
	}
}

func TestDecodeSearchArgsDefaultsAndValidation(t *testing.T) {
	args, err := decodeSearchArgs(searchTestInput(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	query := args.Queries[0]
	if len(query.Paths) != 1 || query.Paths[0] != "." || query.ContextLines != 20 || query.MaxMatches != 40 || query.MaxFiles != 8 || query.Output != "context" || query.Case != "smart" {
		t.Fatalf("defaults = %+v", query)
	}
	zero, err := decodeSearchArgs(searchTestInput(`{"pattern":"x","context_lines":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if zero.Queries[0].ContextLines != 0 {
		t.Fatalf("explicit zero context = %d", zero.Queries[0].ContextLines)
	}
	for _, input := range []string{
		`{}`,
		`{"pattern":"x","context_lines":-1}`,
		`{"pattern":"x","max_matches":-1}`,
		`{"pattern":"x","max_files":-1}`,
		`{"pattern":"x","globs":[""]}`,
	} {
		if _, err := decodeSearchArgs(searchTestInput(input)); err == nil {
			t.Errorf("decode %s: expected error", input)
		}
	}
	clamped, failures, normalized, err := decodeSearchArgsPartial(searchTestInput(`{"pattern":"x","context_lines":101,"max_matches":201,"max_files":51}`))
	if err != nil || failures[0] != nil {
		t.Fatalf("clamped decode = %+v, %v, %v", clamped, failures, err)
	}
	if normalized != 3 || clamped.Queries[0].ContextLines != 100 || clamped.Queries[0].MaxMatches != 200 || clamped.Queries[0].MaxFiles != 50 {
		t.Fatalf("clamped = %+v normalized=%d", clamped.Queries[0], normalized)
	}
}

func TestSearchRunsValidQueriesAlongsideInvalidRegex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("literal Widget(\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"queries":[{"pattern":"(["},{"pattern":"Widget(","fixed_strings":true,"paths":[` + quoteJSON(path) + `],"output":"exists"}]}`)
	result, err := (searchTool{}).RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "invalid regex") || !strings.Contains(result.Text, "true: "+path) {
		t.Fatalf("partial search:\n%s", result.Text)
	}
	if result.Metrics["query_errors"] != 1 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
}

func TestSearchBatchesQueriesAndUsesStandardLibraryFallback(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(source, []byte("Alpha\nbeta\nALPHA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(searchArgs{Queries: []searchQuery{
		{Pattern: "alpha", Paths: []string{dir}, Output: "count"},
		{Pattern: "beta", Paths: []string{dir}, Output: "exists"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := (searchTool{}).Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## query 1: alpha", "2\t" + source, "## query 2: beta", "true: " + source + ":2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSearchBatchedContextDeduplicatesOverlapsAndTagsQueries(t *testing.T) {
	source := filepath.Join(t.TempDir(), "sample.go")
	if err := os.WriteFile(source, []byte("one\ntwo\nalpha\nbeta\nfive\nsix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(searchArgs{Queries: []searchQuery{
		{Pattern: "alpha", Paths: []string{source}, Output: "context", ContextLines: 2},
		{Pattern: "beta", Paths: []string{source}, Output: "context", ContextLines: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (searchTool{}).RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## query 1: alpha",
		"## query 2: beta",
		"## shared source context",
		source + ":1-6 (queries: 1, 2)",
		"[shared context: 6 unique source lines; 4 duplicate lines suppressed across queries]",
	} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("output missing %q:\n%s", want, result.Text)
		}
	}
	for _, line := range []string{"3\talpha", "4\tbeta"} {
		if count := strings.Count(result.Text, line); count != 1 {
			t.Errorf("%q rendered %d times, want once:\n%s", line, count, result.Text)
		}
	}
	if result.Metrics[searchMetricContextLinesBeforeDedupe] != 10 ||
		result.Metrics[searchMetricUniqueContextLines] != 6 {
		t.Fatalf("dedupe metrics = %+v, want 10 selected / 6 unique", result.Metrics)
	}
}

func TestSearchBatchedContextKeepsPerQueryLineBudgets(t *testing.T) {
	source := filepath.Join(t.TempDir(), "large.go")
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	lines[100] = "first-marker"
	lines[399] = "second-marker"
	if err := os.WriteFile(source, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(searchArgs{Queries: []searchQuery{
		{Pattern: "first-marker", Paths: []string{source}, Output: "context", ContextLines: 100},
		{Pattern: "second-marker", Paths: []string{source}, Output: "context", ContextLines: 100},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (searchTool{}).RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Metrics[searchMetricUniqueContextLines]; got != 402 {
		t.Fatalf("unique source lines = %d, want 402 (two independent per-query budgets)", got)
	}
	for _, want := range []string{
		source + ":1-201 (queries: 1)",
		source + ":300-500 (queries: 2)",
		"101\tfirst-marker",
		"400\tsecond-marker",
	} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("output missing %q:\n%s", want, result.Text)
		}
	}
}

func TestMergeLineWindows(t *testing.T) {
	got := mergeLineWindows([]lineWindow{{Start: 10, End: 12}, {Start: 1, End: 3}, {Start: 4, End: 8}, {Start: 20, End: 21}})
	want := []lineWindow{{Start: 1, End: 8}, {Start: 10, End: 12}, {Start: 20, End: 21}}
	if len(got) != len(want) {
		t.Fatalf("merge = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merge = %+v, want %+v", got, want)
		}
	}
}

func rgMatchJSON(t *testing.T, path string, line int, text string) string {
	t.Helper()
	event := map[string]any{
		"type": "match",
		"data": map[string]any{
			"path":        map[string]any{"text": path},
			"lines":       map[string]any{"text": text},
			"line_number": line,
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func searchTestInput(query string) json.RawMessage {
	return json.RawMessage(`{"queries":[` + query + `]}`)
}

func fakeRG(t *testing.T, lines []string, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rg")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, line := range lines {
		b.WriteString("printf '%s\\n' ")
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(encoded)
		b.WriteByte('\n')
	}
	b.WriteString("exit ")
	b.WriteString(string(rune('0' + exitCode)))
	b.WriteByte('\n')
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
