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

func TestSearchSchemaExposesFlatParams(t *testing.T) {
	schema := string((searchTool{}).Schema())
	for _, want := range []string{`"pattern"`, `"path"`, `"globs"`, `"case"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("search schema missing %s: %s", want, schema)
		}
	}
	for _, removed := range []string{`"queries"`, `"fixed_strings"`, `"output"`, `"context_lines"`, `"max_matches"`, `"max_files"`} {
		if strings.Contains(schema, removed) {
			t.Errorf("search schema still exposes %s: %s", removed, schema)
		}
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
		"path":"."
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"matches: 2 shown of 2 across 1 files",
		source + ":1-9",
		"1\tone",
		"7\tseven",
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
	paths := make([]string, searchMaxFiles+1)
	events := make([]string, searchMaxFiles+1)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("%02d.go", i))
		events[i] = rgMatchJSON(t, paths[i], 1, "match\n")
		path := paths[i]
		if err := os.WriteFile(path, []byte("match\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	program := fakeRG(t, events, 0)
	tool := searchTool{program: program}
	result, err := tool.RunResult(context.Background(), searchTestInput(`{"pattern":"match"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "1 additional files omitted") || strings.Contains(result.Text, paths[len(paths)-1]+":") {
		t.Fatalf("file bound not reported:\n%s", result.Text)
	}
	if result.Metrics["files_shown"] != searchMaxFiles || result.Metrics["results_bounded"] != 1 {
		t.Fatalf("metrics = %+v", result.Metrics)
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
	out, err := tool.Run(context.Background(), searchTestInput(`{"pattern":"match"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, source+":1-5") {
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
	for _, want := range []string{"pattern", "invalid regex", "escape regex punctuation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
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
	if !strings.Contains(res.Text, "invalid regex") || !strings.Contains(res.Text, "escape regex punctuation") {
		t.Fatalf("dispatch text = %q", res.Text)
	}
}

func TestSearchRejectsFilesystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	input, err := json.Marshal(searchArgs{Pattern: "marker", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSearchArgs(input); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("decode root error = %v, want filesystem-root rejection", err)
	}

	reg := &Registry{}
	reg.Register(searchTool{})
	res := reg.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "search", Input: input})
	if !res.IsError || res.ErrorKind != llm.ToolErrorInvalidArgs || !strings.Contains(res.Text, "filesystem root") {
		t.Fatalf("dispatch result = %+v, want invalid_args filesystem-root rejection", res)
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
	lines := make([]string, searchMaxMatches+1)
	events := make([]string, searchMaxMatches+1)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%d", i+1)
		events[i] = rgMatchJSON(t, source, i+1, lines[i]+"\n")
	}
	if err := os.WriteFile(source, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := searchTool{program: fakeRG(t, events, 0)}
	result, err := tool.RunResult(context.Background(), searchTestInput(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "60 shown of at least 60") || result.Metrics["matches_shown"] != searchMaxMatches || result.Metrics["results_bounded"] != 1 {
		t.Fatalf("match bound not applied:\n%s\nmetrics=%+v", result.Text, result.Metrics)
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
	input, err := json.Marshal(searchArgs{Pattern: "Needle", Path: dir})
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

func TestSearchFallbackSearchesExplicitHiddenRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".fixture")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("Needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := (searchTool{}).Run(context.Background(), json.RawMessage(`{"pattern":"Needle","path":`+quoteJSON(root)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Needle") {
		t.Fatalf("explicit hidden root was skipped:\n%s", out)
	}
}

func TestDecodeSearchArgsDefaultsAndValidation(t *testing.T) {
	args, err := decodeSearchArgs(searchTestInput(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if args.Path != "." || args.Case != "smart" {
		t.Fatalf("defaults = %+v", args)
	}
	for _, input := range []string{
		`{}`,
		`{"pattern":" "}`,
		`{"pattern":"x","path":" "}`,
		`{"pattern":"x","case":"folded"}`,
		`{"pattern":"x","globs":[""]}`,
	} {
		if _, err := decodeSearchArgs(json.RawMessage(input)); err == nil {
			t.Errorf("decode %s: expected error", input)
		}
	}
}

func TestSearchContextUsesHostOwnedLineBudget(t *testing.T) {
	source := filepath.Join(t.TempDir(), "large.go")
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	for i := 4; i < len(lines); i += 9 {
		lines[i] = "marker"
	}
	if err := os.WriteFile(source, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(searchArgs{Pattern: "marker", Path: source})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (searchTool{}).RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metrics["context_lines"] != searchOutputLines || result.Metrics["context_bounded"] != 1 {
		t.Fatalf("context metrics = %+v", result.Metrics)
	}
	if !strings.Contains(result.Text, "context bounded at 200 source lines") {
		t.Fatalf("bounded receipt missing:\n%s", result.Text)
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

// A pattern that can match a newline is rejected by rg's default
// line-oriented mode; search retries with --multiline and reports the
// spanning match with correct start/end lines.
func TestSearchMultilineRetryOnNewlineRejection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	content := "package sample\n\nfunc Needle() string {\n\treturn \"x\"\n}\n\nfunc six() {}\nfunc seven() {}\n"
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	match := rgMatchJSON(t, source, 3, "func Needle() string {\n\treturn \"x\"\n")
	tool := searchTool{program: fakeRGLineMode(t, match)}
	input, err := json.Marshal(searchArgs{Pattern: `\{\n\s*return`, Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	// The match spans lines 3-4, so the window is 1-8.
	for _, want := range []string{source + ":1-8", "3\tfunc Needle() string {", "4\t\treturn \"x\""} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// The multiline retry is reserved for patterns rg rejects: negated classes
// like [^x] must keep line-bound semantics and never see --multiline.
func TestSearchNegatedClassNeverEnablesMultiline(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	t.Setenv("RG_ARGV_LOG", log)
	tool := searchTool{program: fakeRGRecordingArgv(t)}
	input, err := json.Marshal(searchArgs{Pattern: `o[^x]*o`, Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)" {
		t.Fatalf("output = %q", out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "--multiline") {
		t.Fatalf("--multiline passed for line-mode pattern:\n%s", data)
	}
}

// The stdlib fallback matches newline patterns across lines with the same
// span contract as the rg path.
func TestSearchGoFallbackMatchesAcrossLines(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	content := "package sample\n\nfunc Needle() string {\n\treturn \"x\"\n}\nfunc six() {}\n"
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(searchArgs{Pattern: `\{\n\s*return`, Path: source})
	if err != nil {
		t.Fatal(err)
	}
	out, err := (searchTool{}).Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{source + ":1-8", "3\tfunc Needle() string {", "4\t\treturn \"x\""} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// An all-newline class like [\n] also opts the stdlib fallback into multiline
// matching; each newline reports the line it terminates.
func TestSearchGoFallbackNewlineClass(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(source, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(searchArgs{Pattern: `[\n]`, Path: source})
	if err != nil {
		t.Fatal(err)
	}
	out, err := (searchTool{}).Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"matches: 2 shown of 2", "1\ta", "2\tb"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// End to end against the host rg: a newline pattern is retried with
// --multiline and the JSON span maps to the right source lines.
func TestSearchMultilineAgainstHostRG(t *testing.T) {
	program, ok := ripgrepProgram()
	if !ok {
		t.Skip("rg unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	content := "package sample\n\nfunc Needle() string {\n\treturn \"x\"\n}\n"
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := searchTool{program: program}
	input, err := json.Marshal(searchArgs{Pattern: `\{\n\s*return`, Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3\tfunc Needle() string {", "4\t\treturn \"x\""} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
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
	return json.RawMessage(query)
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
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

// fakeRGLineMode emulates rg's line-oriented rejection: it exits 2 with the
// literal-newline diagnostic unless invoked with --multiline, in which case it
// emits the given match events. A succeeding search therefore proves the
// retry happened and carried the flag.
func fakeRGLineMode(t *testing.T, matchEvents ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rg")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`case " $* " in *" --multiline "*)` + "\n")
	for _, line := range matchEvents {
		b.WriteString("printf '%s\\n' ")
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(encoded)
		b.WriteByte('\n')
	}
	b.WriteString("exit 0\n;;\nesac\n")
	b.WriteString(`printf '%s\n' 'rg: the literal "\n" is not allowed in a regex' >&2` + "\n")
	b.WriteString("exit 2\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeRGRecordingArgv appends its argv to $RG_ARGV_LOG and reports no
// matches, so a test can assert which flags search passed.
func fakeRGRecordingArgv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rg")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RG_ARGV_LOG\"\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
