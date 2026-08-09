package lsptools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Target file."}}}`),
		Annotations: json.RawMessage(`{"readOnlyHint":true}`),
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
	if !strings.Contains(string(spec.Parameters), `"description":"Target file."`) {
		t.Fatalf("model-facing schema lost LSP argument guidance: %s", spec.Parameters)
	}
}

func threeToolProvider() *fakeProvider {
	return &fakeProvider{tools: []mcp.Tool{
		{Name: "definition", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "references", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "diagnostics", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
	}}
}

// stubNavTool is a minimal tools.Tool used to seed navigation anchors.
type stubNavTool struct{ name string }

func (s stubNavTool) Name() string                  { return s.name }
func (s stubNavTool) Description() string           { return s.name }
func (s stubNavTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (s stubNavTool) ReadOnly(json.RawMessage) bool { return true }
func (s stubNavTool) Run(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

func TestRegisterInsertsAfterImageInspection(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(stubNavTool{name: "read"})
	reg.Register(stubNavTool{name: "view_image"})
	reg.Register(stubNavTool{name: "edit"})

	if _, err := Register(context.Background(), reg, threeToolProvider()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	want := []string{"read", "view_image", "lsp_definition", "lsp_references", "lsp_diagnostics", "edit"}
	if !slices.Equal(reg.Names(), want) {
		t.Fatalf("registry names = %v, want %v", reg.Names(), want)
	}
}

func TestRegisterAllowlistRegistersSubset(t *testing.T) {
	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, threeToolProvider(), "definition", "diagnostics")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(reg.Names(), []string{"lsp_definition", "lsp_diagnostics"}) {
		t.Fatalf("registry names = %v, want [lsp_definition lsp_diagnostics]", reg.Names())
	}
	if !slices.Equal(sum.Names, []string{"lsp_definition", "lsp_diagnostics"}) {
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
	// All-blank entries behave like an unset allowlist: register the core set.
	// threeToolProvider only has core tools (definition/references/diagnostics), so all 3 remain.
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
		Annotations: json.RawMessage(`{"readOnlyHint":true}`),
	}}}
	reg := &tools.Registry{}
	// hover is no longer in the default core set; pass an explicit allowlist
	// so the bare-name dispatch test still exercises a real non-core tool.
	if _, err := Register(context.Background(), reg, provider, "hover"); err != nil {
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

func TestRegisterRespectsReadOnlyAnnotations(t *testing.T) {
	provider := &fakeProvider{tools: []mcp.Tool{
		{Name: "definition", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "rename", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":false}`)},
	}}
	// Default (no allowlist) registers the core set only: definition but not rename.
	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, provider)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(sum.Names, []string{"lsp_definition"}) {
		t.Fatalf("Names = %v, want only core tool definition", sum.Names)
	}
	if !slices.Equal(sum.ReadOnlyNames, []string{"lsp_definition"}) {
		t.Fatalf("ReadOnlyNames = %v, want only definition", sum.ReadOnlyNames)
	}
	if !reg.CallReadOnly(llm.ToolCall{ID: "1", Name: "lsp_definition", Input: json.RawMessage(`{}`)}) {
		t.Fatal("definition should be read-only")
	}
	// Full surface via "all" includes the mutating rename tool with correct annotations.
	reg2 := &tools.Registry{}
	sum2, err := Register(context.Background(), reg2, provider, "all")
	if err != nil {
		t.Fatalf("Register all: %v", err)
	}
	if !slices.Equal(sum2.Names, []string{"lsp_definition", "lsp_rename"}) {
		t.Fatalf("Names (all) = %v, want definition and rename", sum2.Names)
	}
	if !slices.Equal(sum2.ReadOnlyNames, []string{"lsp_definition"}) {
		t.Fatalf("ReadOnlyNames (all) = %v, want only definition", sum2.ReadOnlyNames)
	}
	if reg2.CallReadOnly(llm.ToolCall{ID: "2", Name: "lsp_rename", Input: json.RawMessage(`{}`)}) {
		t.Fatal("rename should not be read-only")
	}
}

func TestRegisterDefaultIsCoreSet(t *testing.T) {
	// Provider with one core (definition) and one non-core (completion) tool.
	// Empty allowlist should register only the core tool.
	provider := &fakeProvider{tools: []mcp.Tool{
		{Name: "definition", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "completion", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "rename", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":false}`)},
	}}
	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, provider)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(sum.Names, []string{"lsp_definition"}) {
		t.Fatalf("default core set: Names = %v, want [lsp_definition]", sum.Names)
	}
	if len(reg.Names()) != 1 || reg.Names()[0] != "lsp_definition" {
		t.Fatalf("default core set registry = %v", reg.Names())
	}
}

func TestRegisterAllSentinelRegistersFullSet(t *testing.T) {
	provider := &fakeProvider{tools: []mcp.Tool{
		{Name: "definition", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "completion", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "rename", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":false}`)},
	}}
	for _, allow := range [][]string{{"all"}, {"ALL"}, {"lsp_all"}, {"  all  "}} {
		reg := &tools.Registry{}
		sum, err := Register(context.Background(), reg, provider, allow...)
		if err != nil {
			t.Fatalf("Register %v: %v", allow, err)
		}
		if len(sum.Names) != 3 {
			t.Fatalf("all sentinel %v: Names = %v, want 3", allow, sum.Names)
		}
	}
}

func TestRegisterExplicitAllowlistBeyondCore(t *testing.T) {
	provider := &fakeProvider{tools: []mcp.Tool{
		{Name: "definition", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "completion", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
		{Name: "rename", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":false}`)},
	}}
	reg := &tools.Registry{}
	sum, err := Register(context.Background(), reg, provider, "completion", "rename")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(sum.Names, []string{"lsp_completion", "lsp_rename"}) {
		t.Fatalf("explicit non-core allowlist: Names = %v, want [lsp_completion lsp_rename]", sum.Names)
	}
}
