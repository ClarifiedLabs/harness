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
