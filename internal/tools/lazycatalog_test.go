package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestEnableLazyToolSpecsListsDescribesAndActivates(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("read", "core"))
	r.Register(newOK("mcp__demo__search", "search"))
	r.Register(newOK("lsp_definition", "definition"))
	if !EnableLazyToolSpecs(r, []string{"mcp__demo__search", "lsp_definition"}, 1) {
		t.Fatal("EnableLazyToolSpecs = false")
	}
	if got := specNamesForTest(r.Specs()); !slices.Equal(got, []string{"read", ToolCatalogName}) {
		t.Fatalf("initial specs = %v", got)
	}

	listed := r.Dispatch(context.Background(), llm.ToolCall{
		ID: "list", Name: ToolCatalogName, Input: json.RawMessage(`{"action":"list","query":"search"}`),
	})
	if listed.IsError || !strings.Contains(listed.Text, "mcp__demo__search") || strings.Contains(listed.Text, "lsp_definition") {
		t.Fatalf("list result = %+v", listed)
	}
	described := r.Dispatch(context.Background(), llm.ToolCall{
		ID: "describe", Name: ToolCatalogName, Input: json.RawMessage(`{"action":"describe","names":["lsp_definition"]}`),
	})
	if described.IsError || !strings.Contains(described.Text, `"parameters"`) || !strings.Contains(described.Text, `"_stage"`) {
		t.Fatalf("describe result = %+v", described)
	}
	activated := r.Dispatch(context.Background(), llm.ToolCall{
		ID: "activate", Name: ToolCatalogName, Input: json.RawMessage(`{"action":"activate","names":["lsp_definition"]}`),
	})
	if activated.IsError {
		t.Fatalf("activate result = %+v", activated)
	}
	if got := specNamesForTest(r.Specs()); !slices.Equal(got, []string{"read", "lsp_definition", ToolCatalogName}) {
		t.Fatalf("activated specs = %v", got)
	}
	if _, ok := r.Lookup("mcp__demo__search"); !ok {
		t.Fatal("virtualization changed dispatch registry")
	}
}

func TestEnableLazyToolSpecsKeepsSmallCatalogNative(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("mcp__demo__small", "ok"))
	if EnableLazyToolSpecs(r, []string{"mcp__demo__small"}, 1<<20) {
		t.Fatal("small optional catalog was virtualized")
	}
	if got := r.Names(); !slices.Equal(got, []string{"mcp__demo__small"}) {
		t.Fatalf("names = %v", got)
	}
}

func specNamesForTest(specs []llm.ToolSchema) []string {
	out := make([]string, len(specs))
	for i, spec := range specs {
		out[i] = spec.Name
	}
	return out
}
