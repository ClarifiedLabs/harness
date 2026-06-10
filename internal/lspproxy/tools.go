package lspproxy

// This file defines the static MCP tool surface. The set is fixed; the Manager
// appends a dynamic "available languages" line to each description at runtime
// (see ListTools). All tools are read-only.

// toolAnnotations marks every tool read-only and closed-world.
const toolAnnotations = `{"readOnlyHint":true,"openWorldHint":false}`

// toolSpec is the static definition of one exposed tool.
type toolSpec struct {
	name        string
	description string
	schema      string
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
		name:        "definition",
		description: "Go to definition.",
		schema:      positionSchema,
	},
	{
		name:        "references",
		description: "Find references.",
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
	},
	{
		name:        "hover",
		description: "Show type/signature/docs.",
		schema:      positionSchema,
	},
	{
		name:        "document_symbols",
		description: "Outline a file.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."}
  },
  "required": ["path"]
}`,
	},
	{
		name:        "workspace_symbols",
		description: "Search workspace symbols.",
		schema: `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Symbol name or fragment to search for."},
    "path": {"type": "string", "description": "Any file in the target project, used to pick the language server/workspace."},
    "max_results": {"type": "integer", "description": "Maximum symbols to return (default 100)."}
  },
  "required": ["query"]
}`,
	},
	{
		name:        "diagnostics",
		description: "Show file diagnostics.",
		schema: `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the working directory."},
    "timeout_ms": {"type": "integer", "description": "How long to wait for the server to publish diagnostics (default 3000)."}
  },
  "required": ["path"]
}`,
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
	},
}
