package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
	groups := r.DeferredToolGroups()
	if len(groups) != 2 || groups[0].Name != "mcp_demo" || groups[1].Name != "lsp" ||
		!slices.Equal(specNamesForTest(groups[0].Tools), []string{"mcp__demo__search"}) ||
		!slices.Equal(specNamesForTest(groups[1].Tools), []string{"lsp_definition"}) {
		t.Fatalf("deferred groups = %+v", groups)
	}
	// Returned schemas are immutable snapshots.
	groups[0].Tools[0].Parameters[0] = 'x'
	if r.DeferredToolGroups()[0].Tools[0].Parameters[0] == 'x' {
		t.Fatal("DeferredToolGroups returned mutable parameter bytes")
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

func TestDeferredToolGroupsKeepLargeMCPServiceTogether(t *testing.T) {
	r := &Registry{}
	var names []string
	for i := range 25 {
		name := fmt.Sprintf("mcp__demo__tool_%02d", i)
		names = append(names, name)
		r.Register(newOK(name, "optional"))
	}
	if !EnableLazyToolSpecs(r, names, 1) {
		t.Fatal("EnableLazyToolSpecs = false")
	}
	groups := r.DeferredToolGroups()
	if len(groups) != 1 || groups[0].Name != "mcp_demo" || len(groups[0].Tools) != len(names) {
		t.Fatalf("deferred groups = %+v", groups)
	}
}

func TestDeferredToolGroupsKeepMCPServicesSeparate(t *testing.T) {
	r := &Registry{}
	names := []string{"mcp__foo__one", "mcp__foo__two", "mcp__foo_2__only"}
	for _, name := range names {
		r.Register(newOK(name, "optional"))
	}
	if !EnableLazyToolSpecs(r, names, 1) {
		t.Fatal("EnableLazyToolSpecs = false")
	}
	groups := r.DeferredToolGroups()
	if len(groups) != 2 || groups[0].Name != "mcp_foo" || groups[1].Name != "mcp_foo_2" ||
		!slices.Equal(specNamesForTest(groups[0].Tools), names[:2]) ||
		!slices.Equal(specNamesForTest(groups[1].Tools), names[2:]) {
		t.Fatalf("deferred groups = %+v", groups)
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
