package lspproxy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

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

type parameterSchema struct {
	Properties map[string]struct {
		Description string   `json:"description"`
		Enum        []string `json:"enum"`
	} `json:"properties"`
}

func parameterSchemas(t *testing.T) map[string]parameterSchema {
	t.Helper()
	out := make(map[string]parameterSchema, len(toolSpecs))
	for _, spec := range toolSpecs {
		var schema parameterSchema
		if err := json.Unmarshal([]byte(spec.schema), &schema); err != nil {
			t.Fatalf("parse lsp_%s schema: %v", spec.name, err)
		}
		out[spec.name] = schema
	}
	return out
}

func TestToolParameterDescriptionsFitBudget(t *testing.T) {
	for toolName, schema := range parameterSchemas(t) {
		for parameterName, parameter := range schema.Properties {
			description := parameter.Description
			if description == "" {
				t.Errorf("lsp_%s.%s has no parameter description", toolName, parameterName)
			}
			if len(description) > 80 {
				t.Errorf("lsp_%s.%s description = %d bytes, budget 80: %q", toolName, parameterName, len(description), description)
			}
			if strings.ContainsAny(description, "\r\n") {
				t.Errorf("lsp_%s.%s description is not one line: %q", toolName, parameterName, description)
			}

			lower := strings.ToLower(description)
			propertyPhrase := strings.ReplaceAll(parameterName, "_", " ")
			if strings.Contains(lower, propertyPhrase) {
				t.Errorf("lsp_%s.%s description restates its name: %q", toolName, parameterName, description)
			}
			for _, word := range []string{"optional", "required", "string", "integer", "boolean"} {
				if strings.Contains(lower, word) {
					t.Errorf("lsp_%s.%s description restates requiredness/type %q: %q", toolName, parameterName, word, description)
				}
			}
			for _, enumValue := range parameter.Enum {
				if strings.Contains(lower, strings.ToLower(enumValue)) {
					t.Errorf("lsp_%s.%s description restates enum value %q: %q", toolName, parameterName, enumValue, description)
				}
			}
		}
	}
}

func TestToolParameterDescriptionsRetainOperationalGuidance(t *testing.T) {
	schemas := parameterSchemas(t)
	tests := []struct {
		tool      string
		parameter string
		want      []string
	}{
		{tool: "definition", parameter: "line", want: []string{"1-based"}},
		{tool: "definition", parameter: "symbol", want: []string{"preferred", "column math"}},
		{tool: "definition", parameter: "column", want: []string{"1-based", "absent or repeated"}},
		{tool: "references", parameter: "include_declaration", want: []string{"true"}},
		{tool: "references", parameter: "max_results", want: []string{"100"}},
		{tool: "completion", parameter: "column", want: []string{"1-based", "defaults"}},
		{tool: "completion", parameter: "symbol", want: []string{"immediately after"}},
		{tool: "workspace_symbols", parameter: "path", want: []string{"language server", "workspace"}},
		{tool: "diagnostics", parameter: "timeout_ms", want: []string{"3000 ms"}},
		{tool: "inlay_hints", parameter: "start_line", want: []string{"1-based", "omit both bounds"}},
		{tool: "format_document", parameter: "start_line", want: []string{"1-based", "range formatting"}},
		{tool: "format_document", parameter: "tab_size", want: []string{"4"}},
		{tool: "format_document", parameter: "insert_spaces", want: []string{"true"}},
		{tool: "code_action", parameter: "timeout_ms", want: []string{"0 uses the latest"}},
		{tool: "code_action", parameter: "title", want: []string{"exactly match", "when applying"}},
	}
	for _, tt := range tests {
		description := schemas[tt.tool].Properties[tt.parameter].Description
		for _, want := range tt.want {
			if !strings.Contains(description, want) {
				t.Errorf("lsp_%s.%s description = %q, want guidance %q", tt.tool, tt.parameter, description, want)
			}
		}
	}
}

// TestCoreToolsIsTrimmedDefaultSet guards the default tool surface against
// silent drift: the core set must stay exactly the six high-value navigation,
// outline, and diagnostics tools. Everything else remains available via an
// explicit lsp.tools allowlist or ["all"].
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
