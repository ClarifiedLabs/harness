package lspproxy

// This file defines the static MCP tool surface. Runtime language availability
// is disclosed once in the harness system hint and in /lsp status rather than
// repeated in every tool description.

const readOnlyToolAnnotations = `{"readOnlyHint":true,"openWorldHint":false}`
const mutatingToolAnnotations = `{"readOnlyHint":false,"openWorldHint":false}`

// CoreTools is the default trimmed set exposed when lsp.tools is empty or
// unset. It contains high-value read-only navigation, outline, and
// diagnostics tools; the full 21-tool surface (including hover, signature
// help, highlights, hierarchies, rename, and formatting) is available via
// lsp.tools=["all"] or an explicit allowlist.
var CoreTools = []string{
	"definition",
	"references",
	"diagnostics",
	"document_symbols",
	"workspace_symbols",
	"implementation",
}

// toolSpec is the static definition of one exposed tool.
type toolSpec struct {
	name        string
	description string
	schema      string
	readOnly    bool
}

// positionSchema is the shared input shape for position-bearing tools: a file
// path plus a 1-based line and the symbol name on it (the shim computes the
// exact LSP column, including UTF-16 conversion). column is an optional override
// for repeated symbols.
const positionSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based coordinate containing the target."},
    "symbol": {"type": "string", "description": "Identifier text preferred; avoids column math."},
    "column": {"type": "integer", "description": "1-based override when the identifier is absent or repeated."}
  },
  "required": ["path", "line"]
}`

var toolSpecs = []toolSpec{
	{
		name:        "declaration",
		description: "Find where a symbol is declared.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "definition",
		description: "Find where a symbol is defined.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "type_definition",
		description: "Find where a symbol's type is defined.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "implementation",
		description: "Find implementations of an interface or abstract member.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "references",
		description: "Find references to a symbol across files.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based coordinate containing the target."},
    "symbol": {"type": "string", "description": "Identifier text preferred; avoids column math."},
    "column": {"type": "integer", "description": "1-based override when the identifier is absent or repeated."},
    "include_declaration": {"type": "boolean", "description": "Defaults to true."},
    "max_results": {"type": "integer", "description": "Defaults to 100."}
  },
  "required": ["path", "line"]
}`,
		readOnly: true,
	},
	{
		name:        "hover",
		description: "Show a symbol's type, signature, and docs.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "signature_help",
		description: "Show callable signatures and the active parameter.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "completion",
		description: "List completion candidates at a position.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based completion coordinate."},
    "column": {"type": "integer", "description": "1-based; defaults to the start of the line."},
    "symbol": {"type": "string", "description": "Places the cursor immediately after this prefix."},
    "max_results": {"type": "integer", "description": "Defaults to 100."}
  },
  "required": ["path", "line"]
}`,
		readOnly: true,
	},
	{
		name:        "document_highlights",
		description: "Find occurrences of a symbol in one file.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "document_symbols",
		description: "Outline one file's symbols.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."}
  },
  "required": ["path"]
}`,
		readOnly: true,
	},
	{
		name:        "workspace_symbols",
		description: "Find workspace symbols by name.",
		schema: `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Name or fragment to match."},
    "path": {"type": "string", "description": "Selects the target language server and workspace."},
    "max_results": {"type": "integer", "description": "Defaults to 100."}
  },
  "required": ["query"]
}`,
		readOnly: true,
	},
	{
		name:        "diagnostics",
		description: "Report a file's language-server diagnostics.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "timeout_ms": {"type": "integer", "description": "Wait for pushed diagnostics; defaults to 3000 ms."}
  },
  "required": ["path"]
}`,
		readOnly: true,
	},
	{
		name:        "call_hierarchy",
		description: "Find a symbol's callers or callees.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based coordinate containing the callable."},
    "symbol": {"type": "string", "description": "Identifier text preferred; avoids column math."},
    "column": {"type": "integer", "description": "1-based override when the identifier is absent or repeated."},
    "direction": {"type": "string", "enum": ["incoming", "outgoing"], "description": "Chooses which side of the call graph to return."},
    "max_results": {"type": "integer", "description": "Defaults to 100."}
  },
  "required": ["path", "line", "direction"]
}`,
		readOnly: true,
	},
	{
		name:        "type_hierarchy",
		description: "Find a type's supertypes or subtypes.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based coordinate containing the target."},
    "symbol": {"type": "string", "description": "Identifier text preferred; avoids column math."},
    "column": {"type": "integer", "description": "1-based override when the identifier is absent or repeated."},
    "direction": {"type": "string", "enum": ["supertypes", "subtypes"], "description": "Chooses which side of the hierarchy to return."},
    "max_results": {"type": "integer", "description": "Defaults to 100."}
  },
  "required": ["path", "line", "direction"]
}`,
		readOnly: true,
	},
	{
		name:        "inlay_hints",
		description: "Show inferred type and parameter hints for a file or line range.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "start_line": {"type": "integer", "description": "1-based inclusive bound; omit both bounds for the whole file."},
    "end_line": {"type": "integer", "description": "1-based inclusive bound."},
    "max_results": {"type": "integer", "description": "Defaults to 100."}
  },
  "required": ["path"]
}`,
		readOnly: true,
	},
	{
		name:        "code_actions",
		description: "List quick fixes and refactors; does not edit.",
		schema:      codeActionSchema(false),
		readOnly:    true,
	},
	{
		name:        "code_action",
		description: "Apply one code action by exact title; text edits only.",
		schema:      codeActionSchema(true),
	},
	{
		name:        "format_document_plan",
		description: "Preview formatting edits without writing.",
		schema:      formattingSchema,
		readOnly:    true,
	},
	{
		name:        "format_document",
		description: "Apply formatting edits to a file or line range.",
		schema:      formattingSchema,
	},
	{
		name:        "rename_plan",
		description: "Plan a cross-file rename; does not edit.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based coordinate containing the target."},
    "symbol": {"type": "string", "description": "Identifier text preferred; avoids column math."},
    "column": {"type": "integer", "description": "1-based override when the identifier is absent or repeated."},
    "new_name": {"type": "string", "description": "Replacement text sent to the language server."}
  },
  "required": ["path", "line", "new_name"]
}`,
		readOnly: true,
	},
	{
		name:        "rename",
		description: "Apply a cross-file rename via language-server edits.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based coordinate containing the target."},
    "symbol": {"type": "string", "description": "Identifier text preferred; avoids column math."},
    "column": {"type": "integer", "description": "1-based override when the identifier is absent or repeated."},
    "new_name": {"type": "string", "description": "Replacement text sent to the language server."}
  },
  "required": ["path", "line", "new_name"]
}`,
	},
}

const formattingSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "start_line": {"type": "integer", "description": "1-based inclusive bound; setting it requests range formatting."},
    "end_line": {"type": "integer", "description": "1-based inclusive bound."},
    "tab_size": {"type": "integer", "description": "Defaults to 4 columns."},
    "insert_spaces": {"type": "boolean", "description": "Defaults to true."}
  },
  "required": ["path"]
}`

func codeActionSchema(requireTitle bool) string {
	required := `"path"`
	if requireTitle {
		required += `, "title"`
	}
	return `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute or relative to the working directory."},
    "start_line": {"type": "integer", "description": "1-based inclusive bound; omit both bounds for the whole file."},
    "end_line": {"type": "integer", "description": "1-based inclusive bound."},
    "kind": {"type": "string", "description": "LSP filter such as quickfix, refactor, or source.organizeImports."},
    "timeout_ms": {"type": "integer", "description": "Wait for fresh pushed diagnostics; 0 uses the latest available."},
    "title": {"type": "string", "description": "Must exactly match one offered action when applying."}
  },
  "required": [` + required + `]
}`
}
