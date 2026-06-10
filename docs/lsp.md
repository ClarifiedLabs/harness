# LSP Code Intelligence

Harness includes optional LSP code intelligence. It launches already-installed
language servers on demand and exposes a small read-only set of code-navigation
tools. The normal built-in path registers short `lsp_*` tool names directly in
harness.

A compatibility stdio MCP shim is also available as `harness lsp serve` for
proxy-hosting and other advanced MCP setups.

## Architecture

```text
harness -> internal LSP manager -> gopls / rust-analyzer / pyright / ...
```

With `lsp.enable=true`, harness registers the LSP tools at startup and launches
one language server per `(server, workspace-root)` lazily, on first use. LSP
tools are trusted read-only, so independent lookups can join read-only parallel
batches.

This is independent of `mcp.enable` and `mcp.local`; a custom local stdio MCP
service can run at the same time.

## Enabling LSP

LSP is disabled by default. To enable it, set `lsp.enable` or
`HARNESS_LSP_ENABLE`:

```json
{ "lsp": { "enable": true } }
```

`enable: true` turns it on everywhere, including one-shot. `false` or an unset
value leaves it off. This does not enable the remote MCP proxy or consume the
generic `mcp.local` slot.

## Tools

All LSP tools are read-only and exposed as `lsp_<tool>`. Position tools require
a file path and 1-based `line`. A `symbol` on that line lets the shim compute
the exact LSP position, including UTF-16 columns. An optional 1-based `column`
overrides the symbol lookup. If neither is supplied, the tool uses column 0.

| Tool | Purpose |
|---|---|
| `lsp_definition` | go to definition |
| `lsp_references` | find references |
| `lsp_hover` | type signature and docs |
| `lsp_document_symbols` | outline of a file |
| `lsp_workspace_symbols` | find symbols by name across a project |
| `lsp_diagnostics` | compiler/linter errors and warnings for a file |
| `lsp_rename_plan` | compute a cross-file rename as a diff; does not write files |

When LSP is enabled, its tools are registered. A call on a file type with no
configured server, or whose server binary is not installed, returns a normal tool
error. Each tool's description advertises which configured languages are actually
installed, probed via `PATH` at startup.

## Hosting Behind A Proxy

To aggregate the shim with other local MCP services, set `mcp.local.enable=true`
and point `mcp.local.command` at a local
`harness-mcp-proxy serve -stdio -config <file>` whose config lists
`harness lsp serve -namespace ""`. The proxy then does the `mcp__lsp__`
namespacing.

This advanced path exposes MCP-prefixed names; the built-in `lsp.enable` path
exposes short `lsp_*` names. See `examples/lsp-shim/local-proxy-config.json`.

## Language Servers

The shim ships embedded default configs for:

- Go: `gopls`
- Rust: `rust-analyzer`
- Python: `pyright`, launched as `pyright-langserver --stdio`
- TypeScript/JavaScript: `typescript-language-server`
- C/C++: `clangd`

A server activates lazily when its binary is on `PATH`. To add languages, or
replace a default server definition by name, add inline `lsp.servers` entries to
the harness config. A same-name entry replaces the whole default server
definition, so include all required fields when overriding:

```json
{
  "lsp": {
    "enable": true,
    "servers": {
      "gopls": {
        "languages": ["go"],
        "root_markers": ["go.work", "go.mod", ".git"],
        "command": ["gopls"]
      },
      "ruby-lsp": {
        "languages": ["ruby"],
        "extensions": [".rb"],
        "root_markers": ["Gemfile", ".git"],
        "command": ["ruby-lsp"]
      }
    }
  }
}
```

A crash-looping server backs off exponentially and, after repeated failures,
stops retrying until a cooldown. A later tool call revives it, so installing the
binary or fixing config mid-session recovers without restarting harness.

## V1 Non-Goals

- Read-only navigation only; no completion, formatting, code actions, or write
  tools.
- Rename is exposed as the read-only `lsp_rename_plan`.
- Full-text document sync only; no incremental sync.
- Each language-server process has one workspace root, with separate processes
  for other roots.
- Push diagnostics only.
