package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{Tool: "search", Input: json.RawMessage(`{"queries":[{"pattern":"second","paths":[` + quoteJSON(dir) + `],"output":"exists"}]}`)},
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
	for _, want := range []string{"1\tfirst", "true: " + second + ":1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInspectRejectsUnavailableAndMutatingOperations(t *testing.T) {
	tool := inspectTool{tools: map[string]Tool{"edit": edit{}}}
	for _, input := range []string{
		`{"operations":[{"tool":"missing","input":{}}]}`,
		`{"operations":[{"tool":"edit","input":{"path":"x","old_text":"a","new_text":"b"}}]}`,
	} {
		if _, err := tool.Run(context.Background(), json.RawMessage(input)); err == nil {
			t.Errorf("Run(%s) succeeded, want validation error", input)
		}
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
