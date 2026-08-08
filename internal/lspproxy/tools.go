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

var coreToolSet = map[string]bool{
	"definition":        true,
	"references":        true,
	"diagnostics":       true,
	"document_symbols":  true,
	"workspace_symbols": true,
	"implementation":    true,
}

// IsCoreTool reports whether name (bare, without lsp_ prefix) is in the core set.
func IsCoreTool(name string) bool { return coreToolSet[name] }

// AllToolNames returns the bare names of all registered LSP tools in stable order.
func AllToolNames() []string {
	out := make([]string, 0, len(toolSpecs))
	for _, s := range toolSpecs {
		out = append(out, s.name)
	}
	return out
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number the symbol is on."},
    "symbol": {"type": "string", "description": "The identifier text on that line to locate (preferred; avoids column math)."},
    "column": {"type": "integer", "description": "Optional 1-based column override to use when symbol is absent or repeated."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number the symbol is on."},
    "symbol": {"type": "string", "description": "The identifier text on that line to locate."},
    "column": {"type": "integer", "description": "Optional 1-based column override when symbol is absent or repeated."},
    "include_declaration": {"type": "boolean", "description": "Include the declaration itself (default true)."},
    "max_results": {"type": "integer", "description": "Maximum references to return (default 100)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number at the completion position."},
    "column": {"type": "integer", "description": "Optional 1-based column; defaults to the start of the line."},
    "symbol": {"type": "string", "description": "Optional prefix text on the line; the completion cursor is placed immediately after it."},
    "max_results": {"type": "integer", "description": "Maximum candidates to return (default 100)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."}
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
    "query": {"type": "string", "description": "Symbol name or fragment to search for."},
    "path": {"type": "string", "description": "Any file in the target project, used to pick the language server/workspace."},
    "max_results": {"type": "integer", "description": "Maximum symbols to return (default 100)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "timeout_ms": {"type": "integer", "description": "How long to wait for the server to publish diagnostics (default 3000)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number the callable is on."},
    "symbol": {"type": "string", "description": "Callable identifier text on that line."},
    "column": {"type": "integer", "description": "Optional 1-based column override."},
    "direction": {"type": "string", "enum": ["incoming", "outgoing"], "description": "Whether to return callers or callees."},
    "max_results": {"type": "integer", "description": "Maximum calls to return (default 100)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number the type is on."},
    "symbol": {"type": "string", "description": "Type identifier text on that line."},
    "column": {"type": "integer", "description": "Optional 1-based column override."},
    "direction": {"type": "string", "enum": ["supertypes", "subtypes"], "description": "Which side of the type hierarchy to return."},
    "max_results": {"type": "integer", "description": "Maximum types to return (default 100)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "start_line": {"type": "integer", "description": "Optional 1-based inclusive start line; omit both bounds for the whole file."},
    "end_line": {"type": "integer", "description": "Optional 1-based inclusive end line."},
    "max_results": {"type": "integer", "description": "Maximum hints to return (default 100)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number the symbol is on."},
    "symbol": {"type": "string", "description": "The identifier text on that line to rename."},
    "column": {"type": "integer", "description": "Optional 1-based column override when symbol is absent or repeated."},
    "new_name": {"type": "string", "description": "The new name for the symbol."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "line": {"type": "integer", "description": "1-based line number the symbol is on."},
    "symbol": {"type": "string", "description": "The identifier text on that line to rename."},
    "column": {"type": "integer", "description": "Optional 1-based column override when symbol is absent or repeated."},
    "new_name": {"type": "string", "description": "The new name for the symbol."}
  },
  "required": ["path", "line", "new_name"]
}`,
	},
}

const formattingSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "start_line": {"type": "integer", "description": "Optional 1-based inclusive start line; when set, range formatting is requested."},
    "end_line": {"type": "integer", "description": "Optional 1-based inclusive end line."},
    "tab_size": {"type": "integer", "description": "Formatting tab width (default 4)."},
    "insert_spaces": {"type": "boolean", "description": "Use spaces rather than tabs (default true)."}
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
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "start_line": {"type": "integer", "description": "Optional 1-based inclusive start line; omit both bounds for the whole file."},
    "end_line": {"type": "integer", "description": "Optional 1-based inclusive end line."},
    "kind": {"type": "string", "description": "Optional LSP action-kind filter such as quickfix, refactor, or source.organizeImports."},
    "timeout_ms": {"type": "integer", "description": "Optional time to wait for fresh pushed diagnostics before requesting actions; default 0 uses the latest available diagnostics."},
    "title": {"type": "string", "description": "Exact offered action title; required only when applying an action."}
  },
  "required": [` + required + `]
}`
}
