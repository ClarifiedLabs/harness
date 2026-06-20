package lsptools

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"harness/internal/llm"
	"harness/internal/mcp"
	"harness/internal/tools"
)

type fakeProvider struct {
	tools      []mcp.Tool
	calledName string
	result     *mcp.CallToolResult
}

func (f *fakeProvider) ListTools(context.Context, string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: f.tools}, nil
}

func (f *fakeProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	f.calledName = name
	if f.result != nil {
		return f.result, nil
	}
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}}, nil
}

func TestRegisterUsesShortLSPNames(t *testing.T) {
	provider := &fakeProvider{tools: []mcp.Tool{{
		Name:        "definition",
		Description: "Go to definition.\nMore detail.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	reg := &tools.Registry{}

	sum, err := Register(context.Background(), reg, provider)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(sum.Names, []string{"lsp_definition"}) {
		t.Fatalf("summary names = %v, want [lsp_definition]", sum.Names)
	}
	if !slices.Equal(sum.ReadOnlyNames, []string{"lsp_definition"}) {
		t.Fatalf("read-only names = %v, want [lsp_definition]", sum.ReadOnlyNames)
	}
	if !slices.Equal(reg.Names(), []string{"lsp_definition"}) {
		t.Fatalf("registry names = %v, want [lsp_definition]", reg.Names())
	}
	spec := reg.Specs()[0]
	if spec.Name != "lsp_definition" || spec.Description != "Go to definition." {
		t.Fatalf("spec = %+v", spec)
	}
}

func threeToolProvider() *fakeProvider {
	return &fakeProvider{tools: []mcp.Tool{
		{Name: "definition", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "references", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "hover", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
}

func TestRegisterAllowlistRegistersSubset(t *testing.T) {
	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, threeToolProvider(), "definition", "hover")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(reg.Names(), []string{"lsp_definition", "lsp_hover"}) {
		t.Fatalf("registry names = %v, want [lsp_definition lsp_hover]", reg.Names())
	}
	if !slices.Equal(sum.Names, []string{"lsp_definition", "lsp_hover"}) {
		t.Fatalf("summary names = %v, want subset", sum.Names)
	}
	if sum.Total != 2 || sum.Servers["lsp"] != 2 {
		t.Fatalf("summary totals wrong: total=%d servers=%v", sum.Total, sum.Servers)
	}
}

func TestRegisterAllowlistToleratesLSPPrefix(t *testing.T) {
	reg := &tools.Registry{}
	if _, err := Register(context.Background(), reg, threeToolProvider(), "lsp_references"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(reg.Names(), []string{"lsp_references"}) {
		t.Fatalf("registry names = %v, want [lsp_references]", reg.Names())
	}
}

func TestRegisterEmptyAllowlistRegistersAll(t *testing.T) {
	reg := &tools.Registry{}
	// All-blank entries behave like an unset allowlist: register everything.
	if _, err := Register(context.Background(), reg, threeToolProvider(), "  ", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := len(reg.Names()); got != 3 {
		t.Fatalf("registry names = %v, want all 3", reg.Names())
	}
}

func TestRegisterUnknownAllowlistEntryRegistersNothingExtra(t *testing.T) {
	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, threeToolProvider(), "definition", "bogus")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Unknown entries simply match nothing; the rest still register.
	if !slices.Equal(sum.Names, []string{"lsp_definition"}) {
		t.Fatalf("summary names = %v, want [lsp_definition]", sum.Names)
	}
}

func TestToolCallsBareProviderNameAndIsReadOnly(t *testing.T) {
	provider := &fakeProvider{tools: []mcp.Tool{{
		Name:        "hover",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}}
	reg := &tools.Registry{}
	if _, err := Register(context.Background(), reg, provider); err != nil {
		t.Fatalf("Register: %v", err)
	}

	call := llm.ToolCall{ID: "1", Name: "lsp_hover", Input: json.RawMessage(`{"path":"x.go"}`)}
	if !reg.CallReadOnly(call) {
		t.Fatal("lsp tool should be read-only")
	}
	res := reg.Dispatch(context.Background(), call)
	if res.IsError || res.Text != "ok" {
		t.Fatalf("dispatch = %+v, want ok text", res)
	}
	if provider.calledName != "hover" {
		t.Fatalf("provider called with %q, want bare name hover", provider.calledName)
	}
}
