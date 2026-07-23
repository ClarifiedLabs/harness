package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchDescriptionsRouteCodeInvestigationToSearchContext(t *testing.T) {
	if description := (searchContext{}).Description(); !strings.Contains(description, "Targeted code lookup after broad discovery") || !strings.Contains(description, "instead of rg followed by read_file") || !strings.Contains(description, "one raw rg with a combined pattern") {
		t.Fatalf("search_context description does not state the preferred flow: %q", description)
	}
	if description := (ripgrep{}).Description(); !strings.Contains(description, "use search_context instead") || !strings.Contains(description, "broad repository discovery") || !strings.Contains(description, "combined patterns") {
		t.Fatalf("rg description does not route surrounding-source work: %q", description)
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
	tool := searchContext{program: program}
	out, err := tool.Run(context.Background(), json.RawMessage(`{
		"pattern":"needle",
		"path":".",
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
	tool := searchContext{program: program}
	out, err := tool.Run(context.Background(), json.RawMessage(`{
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
	tool := searchContext{program: fakeRG(t, []string{event}, 0)}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"pattern":"match","context_lines":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, source+":1-1") {
		t.Fatalf("decoded path absent:\n%s", out)
	}
}

func TestSearchContextNoMatchesAndRGFailure(t *testing.T) {
	t.Run("no matches", func(t *testing.T) {
		tool := searchContext{program: fakeRG(t, nil, 1)}
		out, err := tool.Run(context.Background(), json.RawMessage(`{"pattern":"none"}`))
		if err != nil || out != "(no matches)" {
			t.Fatalf("Run = %q, %v", out, err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		tool := searchContext{program: fakeRG(t, []string{"not json"}, 2)}
		if _, err := tool.Run(context.Background(), json.RawMessage(`{"pattern":"x"}`)); err == nil {
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
	tool := searchContext{program: fakeRG(t, []string{
		rgMatchJSON(t, source, 1, "one\n"),
		rgMatchJSON(t, source, 2, "two\n"),
	}, 0)}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"pattern":"x","context_lines":0,"max_matches":1}`))
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
	tool := searchContext{program: program}
	input, err := json.Marshal(map[string]any{
		"pattern":       "Needle",
		"path":          dir,
		"context_lines": 1,
	})
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

func TestDecodeSearchContextArgsDefaultsAndValidation(t *testing.T) {
	args, err := decodeSearchContextArgs(json.RawMessage(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if args.Path != "." || args.ContextLines != 20 || args.MaxMatches != 40 || args.MaxFiles != 8 {
		t.Fatalf("defaults = %+v", args)
	}
	zero, err := decodeSearchContextArgs(json.RawMessage(`{"pattern":"x","context_lines":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if zero.ContextLines != 0 {
		t.Fatalf("explicit zero context = %d", zero.ContextLines)
	}
	for _, input := range []string{
		`{}`,
		`{"pattern":"x","context_lines":101}`,
		`{"pattern":"x","max_matches":201}`,
		`{"pattern":"x","max_files":51}`,
		`{"pattern":"x","globs":[""]}`,
	} {
		if _, err := decodeSearchContextArgs(json.RawMessage(input)); err == nil {
			t.Errorf("decode %s: expected error", input)
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
