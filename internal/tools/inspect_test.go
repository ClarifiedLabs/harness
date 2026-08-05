package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestInspectBatchesReadOnlyOperationsInInputOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(first, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := inspectTool{tools: map[string]Tool{
		"read_file": readFile{},
		"search":    searchTool{},
	}}
	input, err := json.Marshal(inspectArgs{Operations: []inspectOperation{
		{Tool: "read_file", Input: json.RawMessage(`{"path":` + quoteJSON(first) + `}`)},
		{Tool: "search", Input: json.RawMessage(`{"pattern":"second","path":` + quoteJSON(dir) + `}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if firstHeader, secondHeader := strings.Index(out, "## 1. read_file"), strings.Index(out, "## 2. search"); firstHeader < 0 || secondHeader <= firstHeader {
		t.Fatalf("operation order not preserved:\n%s", out)
	}
	for _, want := range []string{"1\tfirst", second + ":1-5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInspectAllRejectedOperationsAreStructuredBatchFailure(t *testing.T) {
	registry := &Registry{}
	registry.Register(inspectTool{tools: map[string]Tool{"edit": edit{}}})
	input := json.RawMessage(`{"operations":[{"tool":"missing","input":{}},{"tool":"edit","input":{}}]}`)
	result := registry.Dispatch(context.Background(), llm.ToolCall{ID: "inspect-1", Name: "inspect", Input: input})
	if !result.IsError || result.ErrorKind != llm.ToolErrorBatchFailed {
		t.Fatalf("result = %+v, want is_error with kind %q", result, llm.ToolErrorBatchFailed)
	}
	if result.Metrics["operation_errors"] != 2 || result.Metrics["operation_count"] != 2 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
	for _, want := range []string{"## 1. missing", `tool "missing" is not available`, "## 2. edit", "was not executed"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("batch failure missing %q:\n%s", want, result.Text)
		}
	}
}

func TestInspectAllRuntimeFailuresAreStructuredBatchFailure(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.txt")
	two := filepath.Join(dir, "two.txt")
	registry := &Registry{}
	registry.Register(inspectTool{tools: map[string]Tool{"read_file": readFile{}}})
	input := json.RawMessage(`{"operations":[{"tool":"read_file","input":{"path":` + quoteJSON(one) + `}},{"tool":"read_file","input":{"path":` + quoteJSON(two) + `}}]}`)
	result := registry.Dispatch(context.Background(), llm.ToolCall{ID: "inspect-1", Name: "inspect", Input: input})
	if !result.IsError || result.ErrorKind != llm.ToolErrorBatchFailed {
		t.Fatalf("result = %+v, want is_error with kind %q", result, llm.ToolErrorBatchFailed)
	}
	if result.Metrics["operation_errors"] != 2 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
	for _, want := range []string{"## 1. read_file", "## 2. read_file", one, two} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("batch failure missing %q:\n%s", want, result.Text)
		}
	}
}

func TestInspectOmitsGitOperationsWhenGitUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	registry, _ := CatalogWithOptions(Options{})
	tool, ok := registry.Lookup("inspect")
	if !ok {
		t.Fatal("inspect was not registered")
	}

	for _, surface := range []struct {
		name  string
		value string
	}{
		{name: "schema", value: string(tool.Schema())},
		{name: "description", value: tool.Description()},
	} {
		if !strings.Contains(surface.value, "read_file") {
			t.Errorf("inspect %s omits available read_file operation: %s", surface.name, surface.value)
		}
		for _, unavailable := range []string{"workspace_summary", "git_readonly"} {
			if strings.Contains(surface.value, unavailable) {
				t.Errorf("inspect %s advertises unavailable %s operation: %s", surface.name, unavailable, surface.value)
			}
		}
	}
}

func TestInspectSupportsGitReadonlyAndRejectsMutation(t *testing.T) {
	gitAvailable(t)
	dir := scratchRepo(t)
	git, ok := newGitTool()
	if !ok {
		t.Fatal("git became unavailable after availability check")
	}
	registry := &Registry{}
	registry.Register(git)
	registerInspectTool(registry)
	tool, ok := registry.Lookup("inspect")
	if !ok {
		t.Fatal("inspect was not registered")
	}
	if !strings.Contains(string(tool.Schema()), `"git_readonly"`) {
		t.Fatalf("inspect schema does not advertise git_readonly: %s", tool.Schema())
	}

	run := func(args ...string) (string, error) {
		operationInput, err := json.Marshal(map[string]any{"args": args, "cwd": dir})
		if err != nil {
			t.Fatal(err)
		}
		input, err := json.Marshal(inspectArgs{Operations: []inspectOperation{{
			Tool:  "git_readonly",
			Input: json.RawMessage(operationInput),
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return tool.Run(context.Background(), input)
	}

	out, err := run("rev-parse", "--is-inside-work-tree")
	if err != nil {
		t.Fatalf("read-only git operation failed: %v", err)
	}
	for _, want := range []string{"## 1. git_readonly", "true", "[exit code: 0]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	if _, err := run("commit", "-m", "must not run"); err == nil {
		t.Fatal("mutating git operation succeeded through inspect")
	} else if !strings.Contains(err.Error(), "was not executed") {
		t.Fatalf("mutating git operation returned unexpected error: %v", err)
	}
}

func TestInspectRunsValidOperationsAlongsideRejectedOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.txt")
	if err := os.WriteFile(path, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := &Registry{}
	registry.Register(inspectTool{tools: map[string]Tool{"read_file": readFile{}}})
	input := json.RawMessage(`{"operations":[{"tool":"missing","input":{}},{"tool":"read_file","input":{"path":` + quoteJSON(path) + `}}]}`)
	result := registry.Dispatch(context.Background(), llm.ToolCall{ID: "inspect-1", Name: "inspect", Input: input})
	if result.IsError || result.ErrorKind != "" {
		t.Fatalf("partial batch was marked failed: %+v", result)
	}
	if !strings.Contains(result.Text, `tool "missing" is not available`) || !strings.Contains(result.Text, "1\tok") {
		t.Fatalf("partial output:\n%s", result.Text)
	}
	if result.Metrics["operation_errors"] != 1 || result.Metrics["operation_count"] != 2 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
}

func TestInspectAcceptsMoreThanOneConcurrencyWave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.txt")
	if err := os.WriteFile(path, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	operations := make([]inspectOperation, 18)
	for i := range operations {
		operations[i] = inspectOperation{Tool: "read_file", Input: json.RawMessage(`{"path":` + quoteJSON(path) + `}`)}
	}
	input, err := json.Marshal(inspectArgs{Operations: operations})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (inspectTool{tools: map[string]Tool{"read_file": readFile{}}}).RunResult(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metrics["operation_count"] != 18 || strings.Count(result.Text, "## ") != 18 {
		t.Fatalf("result metrics/output = %+v, headers=%d", result.Metrics, strings.Count(result.Text, "## "))
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
