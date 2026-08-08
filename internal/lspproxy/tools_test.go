package lspproxy

import (
	"slices"
	"testing"
)

// TestCoreToolsIsTrimmedDefaultSet guards the default tool surface against
// silent drift: the core set must stay exactly the six high-value navigation,
// outline, and diagnostics tools. Everything else remains available via an
// explicit lsp.tools allowlist or ["all"].
func TestToolDescriptionsFitBudget(t *testing.T) {
	for _, spec := range toolSpecs {
		if len(spec.description) > 80 {
			t.Errorf("lsp_%s description = %d bytes, budget 80: %q", spec.name, len(spec.description), spec.description)
		}
		for i := 0; i < len(spec.description); i++ {
			if spec.description[i] == '\n' {
				t.Errorf("lsp_%s description is not one line: %q", spec.name, spec.description)
				break
			}
		}
	}
}

func TestCoreToolsIsTrimmedDefaultSet(t *testing.T) {
	want := []string{
		"definition",
		"references",
		"diagnostics",
		"document_symbols",
		"workspace_symbols",
		"implementation",
	}
	if !slices.Equal(CoreTools, want) {
		t.Fatalf("CoreTools = %v, want %v", CoreTools, want)
	}
	for _, name := range want {
		if !IsCoreTool(name) {
			t.Errorf("IsCoreTool(%q) = false, want true", name)
		}
	}
	// Dropped cursor-oriented aids must not linger in coreToolSet.
	for _, name := range []string{"declaration", "hover", "signature_help", "document_highlights", "type_definition", "code_actions"} {
		if IsCoreTool(name) {
			t.Errorf("IsCoreTool(%q) = true, want false (dropped from core)", name)
		}
	}
	// CoreTools and coreToolSet must agree with no extras in the map.
	if len(coreToolSet) != len(CoreTools) {
		t.Fatalf("coreToolSet has %d entries, CoreTools has %d", len(coreToolSet), len(CoreTools))
	}
}
