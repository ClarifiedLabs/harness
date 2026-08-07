package lspproxy

// This file defines the static MCP tool surface. Runtime language availability
// is disclosed once in the harness system hint and in /lsp status rather than
// repeated in every tool description.

const readOnlyToolAnnotations = `{"readOnlyHint":true,"openWorldHint":false}`
const mutatingToolAnnotations = `{"readOnlyHint":false,"openWorldHint":false}`

// CoreTools is the default trimmed set exposed when lsp.tools is empty or
// unset. It contains high-value read-only navigation and inspection tools;
// the full 21-tool surface is available via lsp.tools=["all"] or an
// explicit allowlist.
var CoreTools = []string{
	"declaration",
	"definition",
	"type_definition",
	"implementation",
	"references",
	"hover",
	"signature_help",
	"document_symbols",
	"workspace_symbols",
	"diagnostics",
	"document_highlights",
	"code_actions",
}

var coreToolSet = map[string]bool{
	"declaration":         true,
	"definition":          true,
	"type_definition":     true,
	"implementation":      true,
	"references":          true,
	"hover":               true,
	"signature_help":      true,
	"document_symbols":    true,
	"workspace_symbols":   true,
	"diagnostics":         true,
	"document_highlights": true,
	"code_actions":        true,
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
		description: "Find the declaration of a symbol at a file position; use symbol with a 1-based line when possible. Prefer over search.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "definition",
		description: "Find the definition of a symbol at a file position; use symbol with a 1-based line when possible. Prefer over search.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "type_definition",
		description: "Find the type definition of a symbol at a file position. Prefer over search.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "implementation",
		description: "Find implementations of an interface, abstract member, or symbol at a file position. Prefer over search.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "references",
		description: "Find cross-file references to a symbol at a file position. Prefer over search.",
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
		description: "Show language-server type, signature, and documentation for a symbol at a file position. Prefer over search.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "signature_help",
		description: "Show callable signatures and the active parameter at a file position.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "completion",
		description: "Ask the language server for completion candidates at a file position.",
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
		description: "Find read, write, and textual occurrences of a symbol in the current file. Prefer over search.",
		schema:      positionSchema,
		readOnly:    true,
	},
	{
		name:        "document_symbols",
		description: "Return the language-server symbol outline for a file. Prefer over search.",
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
		description: "Search symbols by name across one language-server workspace. Prefer over search.",
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
		description: "Open or refresh a file and return language-server diagnostics published for it.",
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
		description: "Find incoming callers or outgoing callees for a symbol at a file position. Prefer over search.",
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
		description: "Find direct supertypes or subtypes for a type at a file position. Prefer over search.",
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
		description: "Return inferred type and parameter hints for a file or inclusive 1-based line range.",
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
		description: "List language-server quick fixes and refactors for a file or inclusive 1-based line range; does not edit files.",
		schema:      codeActionSchema(false),
		readOnly:    true,
	},
	{
		name:        "code_action",
		description: "Apply one language-server code action selected by its exact title; text edits only, never server commands or file operations.",
		schema:      codeActionSchema(true),
	},
	{
		name:        "format_document_plan",
		description: "Preview language-server formatting edits for a document or inclusive 1-based line range without writing files.",
		schema:      formattingSchema,
		readOnly:    true,
	},
	{
		name:        "format_document",
		description: "Apply language-server formatting text edits to a document or inclusive 1-based line range.",
		schema:      formattingSchema,
	},
	{
		name:        "rename_plan",
		description: "Plan a safe cross-file rename; does not edit files.",
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
		description: "Apply a safe cross-file rename using language-server text edits.",
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
