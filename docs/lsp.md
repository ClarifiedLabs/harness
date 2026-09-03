# LSP Code Intelligence

Harness includes optional LSP code intelligence. It launches already-installed
language servers on demand and exposes an agent-oriented set of navigation,
inspection, diagnostics, refactoring, and formatting tools. The
normal built-in path registers short `lsp_*` tool names directly in harness.
Harness can also launch [Serena](https://github.com/oraios/serena) as an
independent local MCP server for symbol-aware coding tools.

A compatibility stdio MCP shim is also available as `harness lsp serve` for
proxy-hosting and other advanced MCP setups. `harness lsp --help` and
`harness lsp serve --help` are generated from the same scoped command/flag
catalog used for parsing, so the parent help lists subcommands while leaf help
lists the accepted shim flags.

## Architecture

```text
harness -> internal LSP manager -> gopls / rust-analyzer / pyright / ...
harness -> Serena MCP child -> Serena tools
```

With `lsp.enable=true`, harness registers the LSP tools at startup and launches
one language server per `(server, workspace-root)` lazily, on first use. All LSP
tools inherit default-parallel dispatch. `lsp_code_action`,
`lsp_format_document`, and `lsp_rename` are mutating policy-wise, but Harness does
not infer their remote file effects for scheduling; use separate turns when one
operation semantically depends on another.

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

In an interactive session, `/lsp enable` and `/lsp disable` override the initial
setting for that process only. `/lsp` and `/lsp status` show:

- whether native LSP tools are currently exposed to the active agent;
- installed languages whose effective server commands are currently resolvable
  (normally on `PATH`, with project-local TypeScript as described below);
- ready languages with at least one live, initialized server root; and
- each server's `missing`, `idle`, `initializing`, `ready`, or `failed` state,
  including failed roots, initialization errors, and active backoff retry times.

Enabling changes both dispatch and, when the active agent's tool policy permits
LSP, the tool schemas sent on the next model request. Disabling removes those
schemas, shuts down loaded language servers, and
removes the LSP runtime hint. It does not rewrite the config file. Servers remain
lazy after enabling, so `ready languages: none` and an `idle` server state are
expected until prewarm or an LSP tool initializes a server successfully. A
command being installed does not by itself make its languages ready.

When the selected agent's combined optional MCP/LSP declarations exceed 32 KiB,
Harness initially exposes them through `tool_catalog` instead of sending every
schema. Use its `list` or `describe` actions, then `activate` the needed `lsp_*`
names; their direct schemas appear on the next model turn.

### Prewarming

When `lsp.prewarm` is enabled (the default; disable with `lsp.prewarm: false` or
`HARNESS_LSP_PREWARM=false`), harness scans the workspace surrounding the process
working directory and background-launches each installed configured server
**only for languages with files present**. It finds a source file whose extension
matches the server's configured extensions or its languages' built-in extensions,
then derives that server's workspace root from the source file using the normal
configured marker priority. This lets a nested package, crate, module, or build
root use its own language-server process when its marker wins that selection,
while higher-priority enclosing markers such as `go.work` continue to take
precedence.

Servers whose root markers are not found, whose languages have no file evidence,
or whose effective command cannot be resolved stay lazy. The evidence scan is
bounded (20000
entries); an inconclusive scan skips prewarming rather than guessing, and files
that live only under skipped directories are not evidence. Skipped directories:
`.git`, `node_modules`, `vendor`, `target`, `dist`, `build`, `__pycache__`,
`.venv`. Prewarm failures are logged by the server instance once per launch
attempt and never affect startup or the exit code; ordinary lazy startup remains
the fallback. At most one source-derived root
per configured server is prewarmed, selected from the first matching source in
the bounded scan; other roots remain lazy.

## Enabling Serena

Serena is disabled by default and is independent of native LSP. To launch Serena
without enabling `gopls`, TypeScript, or any other native language server:

```json
{
  "lsp": {
    "serena": { "enable": true }
  }
}
```

`HARNESS_LSP_SERENA_ENABLE=true` is the matching environment override. By
default harness starts:

```text
serena start-mcp-server --context=ide --project-from-cwd --open-web-dashboard False
```

Override `lsp.serena.command`, `args`, or `env` in the config file when Serena is
installed through a wrapper such as `uvx`. Serena tools are exposed as
`mcp__serena__<tool>`. Harness trusts Serena's `readOnlyHint` annotations:
read-only agents see only annotated read-only Serena tools, while the default
agents can use the full Serena tool surface.

Harness does not install or run Serena hooks. When Serena tools are registered,
harness adds a short prompt hint steering the model to use `mcp__serena__*` for
symbol-aware navigation and refactors.

## Tools

Native LSP tools are exposed as `lsp_<tool>`. Position tools require a file path
and 1-based `line`. A `symbol` on that line lets the shim compute the exact LSP
position, including UTF-16 columns. An optional 1-based `column` overrides the
symbol lookup. If neither is supplied, the tool uses column 0.

| Tool | Purpose |
|---|---|
| `lsp_declaration` | find a symbol declaration |
| `lsp_definition` | find a symbol definition |
| `lsp_type_definition` | find the definition of a symbol's type |
| `lsp_implementation` | find implementations of an interface, abstract member, or symbol |
| `lsp_references` | find cross-file references |
| `lsp_hover` | show type, signature, and documentation |
| `lsp_signature_help` | show callable signatures and the active parameter |
| `lsp_completion` | return bounded completion candidates |
| `lsp_document_highlights` | find textual/read/write occurrences within one file |
| `lsp_document_symbols` | outline of a file |
| `lsp_workspace_symbols` | find symbols by name across a project |
| `lsp_diagnostics` | compiler/linter errors and warnings for a file |
| `lsp_call_hierarchy` | find incoming callers or outgoing callees |
| `lsp_type_hierarchy` | find direct supertypes or subtypes |
| `lsp_inlay_hints` | show inferred type and parameter hints |
| `lsp_code_actions` | list quick fixes/refactors; does not write files |
| `lsp_code_action` | apply one exact-title action's text edits |
| `lsp_format_document_plan` | preview document or range formatting edits |
| `lsp_format_document` | apply document or range formatting edits |
| `lsp_rename_plan` | compute a cross-file rename as a diff; does not write files |
| `lsp_rename` | apply a cross-file rename using language-server text edits |

The hierarchy tools require `direction` (`incoming`/`outgoing` or
`supertypes`/`subtypes`). Inlay hints, code actions, and formatting accept optional
1-based inclusive `start_line`/`end_line` bounds. Formatting defaults to tab size
4 and spaces. `max_results` bounds references, workspace symbols, completion,
hierarchies, and inlay hints where present.

`lsp_code_actions` lists the exact action titles the apply tool accepts.
`lsp_code_action` applies only `WorkspaceEdit` text edits. Harness does not execute
language-server commands and rejects create/delete/rename file operations before
writing. An optional `timeout_ms` waits for fresh pushed diagnostics before asking
for actions; otherwise the latest already-published diagnostics are used. The same
file-operation rule applies to rename; format operations are single-file text edits.

Native LSP also augments successful built-in `edit` and `write` calls. Harness
synchronizes up to eight unique supported mutation paths, waits concurrently up
to three seconds for fresh pushed diagnostics, and appends them to the mutation
result in path order. Unsupported paths and unavailable servers are skipped.
Diagnostics failures are supplemental and do not fail or roll back the mutation;
failed mutations do not run this follow-up.

A call on a file type with no configured server, an unresolved server command,
or a
server that does not implement that operation returns a normal tool error. The
model receives the complete enabled `lsp_*` tool list, capability-specific tool
descriptions, and the preserved schema descriptions explaining the 1-based
position/range conventions. A concise system hint separately lists languages
whose effective server command is resolvable and notes that initialization is
lazy; `/lsp status` is the
authoritative human-facing view of installed commands versus ready or failed
workspace roots.

To register only a subset of the native tools, set the config-file-only `lsp.tools`
allowlist (bare names, with or without the `lsp_` prefix):

```json
{ "lsp": { "enable": true, "tools": ["definition", "references", "diagnostics"] } }
```

An empty or unset `lsp.tools` registers the core set of six high-value
navigation, outline, and diagnostics tools (`definition`, `references`,
`diagnostics`, `document_symbols`, `workspace_symbols`, `implementation`).
Use `["all"]` to register the full 21-tool surface, or list explicit names to
add/trim the set. Unknown entries are warned about and ignored.

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
- TypeScript/JavaScript: project-local TypeScript 7+
  (`tsc --lsp --stdio`), otherwise `typescript-language-server`
- C/C++: `clangd`

The built-in `typescript-language-server` entry has version-aware
resolution. Harness searches the detected workspace root and its ancestors up
to the nearest Git root, then uses the nearest
`node_modules/typescript/package.json`. This supports hoisted npm, pnpm, and Yarn
`node_modules` installs inside a repository without selecting dependencies from
an unrelated shared parent. Without a Git boundary, only the detected root is
searched. For TypeScript 7
or newer it runs that package's declared `tsc` binary with `--lsp --stdio`. For
TypeScript 6 or older it retains `typescript-language-server`, which selects the
workspace TypeScript SDK for its tsserver-based protocol. A project-local
TypeScript package is authoritative and is never replaced by a different global
version.

When the project has no local TypeScript package, Harness checks a global `tsc`
and uses it directly only when a nearby TypeScript package manifest identifies
version 7 or newer. Discovery never executes `tsc` merely to inspect its version;
nonstandard global shims can be selected with an explicit command. Otherwise
Harness falls back to `typescript-language-server`.

Automatic selection applies exactly while the server name and command retain the
embedded defaults. Changing either the name or command disables it; this is also
the escape hatch for layouts without `node_modules`, such as Plug'n'Play SDK
paths. Because LSP prewarming is enabled by default, the selected project package
can be launched during startup when TypeScript/JavaScript file evidence exists;
treat installed project dependencies as executable code. TypeScript command
status is resolved relative to the process working directory; a nested project
root can select a different version. Installing or changing TypeScript after an
LSP root starts requires restarting Harness to replace that live server.

A server launches lazily on the first tool call for one of its files. To add languages, or
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

## Deliberate Limits

- Language-server commands and `WorkspaceEdit` file create/delete/rename
  operations are not executed. Mutating tools apply validated text edits only.
- Full-text document sync only; no incremental sync.
- Each language-server process has one workspace root, with separate processes
  for other roots.
- Push diagnostics only.
- Editor-rendering features that do not provide useful agent semantics—semantic
  tokens, folding/selection ranges, document colors, linked editing, and inline
  values—are not exposed as tools.
