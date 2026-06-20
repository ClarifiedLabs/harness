# Tools

Harness exposes a small built-in tool set to the model. Tool schemas and exact
implementation contracts are maintained in [design.md](design.md) section 9.
This page is the operational overview.

## Built-In Tools

| tool | purpose |
|---|---|
| `read_file` | read line-numbered file content; supports `offset`/`limit`, or `paths[]` for multi-file reads |
| `list_dir` | list directory entries with type and size, non-recursive |
| `glob` | recursively find files/dirs by glob, including `**` patterns; read-only |
| `grep` | run host `grep` with argv-style args |
| `rg` | run host ripgrep when available |
| `edit` | edit existing files with exact-text replacements; optional `replaceAll` |
| `write_file` | create or overwrite a file, creating parent directories |
| `run_command` | run a shell command or direct argv program |
| `git` | run host git with `--no-pager`, when git is installed |
| `git_readonly` | restricted git subcommands for read-only agents |
| `web_fetch` | fetch HTTP(S) content and reduce HTML to readable text, keeping block structure and rendering links as `text (url)` |
| `write_tmp_file` | write scratch files under a private temp directory |
| `update_todos` | replace the current todo list for multi-step work |
| `delegate` | run a configured child agent and return its final report |
| `background_jobs` | list, inspect, or cancel process-local background jobs |

`apply_patch` (Codex-format add/delete/update/move patches) is no longer in the
default tool set — `edit` and `write_file` subsume it. It still ships in the tool
catalog, so an agent can opt back in by naming `apply_patch` in its
`allowed_tools` whitelist.

`read_file` reads one file via `path` (with `offset`/`limit`), or several at once
via `paths[]` — each file is rendered under a `==> path <==` header with its own
per-file line budget. When a single read is cut off at the line limit it ends with
`[file truncated at line N; continue with offset=N+1]`. `glob` walks recursively
from an optional `root`, where `**` matches across directory segments (and `*`/`?`/
`[…]` match within one segment), returning matching paths with type and size sorted
by path. `edit` takes an optional per-edit `replaceAll` flag that replaces every
occurrence of `oldText` instead of requiring a unique match, reporting the
replacement count.

When [MCP](mcp.md) is enabled, downstream MCP tools also appear, namespaced as
`mcp__<server>__<tool>`. When [LSP](lsp.md) is enabled, read-only `lsp_*` code
navigation tools are also registered.

## Search Tools

Harness registers one search tool by default: `rg` when ripgrep is installed,
otherwise `grep`. Configure this with `search_tools`, `HARNESS_SEARCH_TOOLS`, or
`-search-tools`: `auto`, `grep`, `rg`, or `both`.

`grep`, `rg`, `git`, and direct-argv `run_command` calls expect JSON arrays of
strings for argv-style fields, not shell strings and not JSON-encoded arrays. The
tools are thin wrappers around host CLIs, so native CLI semantics decide regex
syntax, ignore behavior, output shape, and supported flags.

Normal `rg` searches add `--max-columns=1024 --max-columns-preview
--max-filesize=10M` unless the caller's native `rg` args already set those
limits.

The host `grep` tool injects `-I` (skip binary files) unless the call already sets
a binary policy (`-I`/`-a`/`--text`/`--binary-files`) or is a help/version
invocation; `-I` is placed before any `--` operand separator. Matched lines longer
than 1024 bytes are clamped in-process (host `grep` has no portable
`--max-columns`), trailing them with `… [N chars clamped]`. Under `-search-tools
both`, `grep` and `rg` are both registered and `grep`'s description steers the model
to prefer `rg`.

## Command Execution

`run_command` accepts exactly one of:

- `command`: executed through a non-login `bash -c` (with `sh -c` fallback). The
  login PATH a login shell would have added is resolved once at startup and merged
  into the command environment, so build/test toolchains are still found without
  paying the login-profile cost on every call.
- `argv`: direct program invocation with literal args and no shell

Foreground calls capture combined stdout/stderr and append `[exit code: N]`.
Non-zero exit is not a tool error; it is returned as ordinary command output so
the model can react to failing builds, tests, and searches.

`run_command`, `grep`, `rg`, and `web_fetch` can set `background:true` to return
a job id immediately. `delegate` can also run as a background child agent.
Completed background job summaries are delivered once as request-only context to
the parent agent. Jobs live only in the current harness process and are abandoned
when that process exits.

## File Mutation

`edit`, `write_file`, and `apply_patch` are the built-in file mutation tools.
By default, harness prints a unified before/after diff for each built-in file
mutation tool call. Set `show_diffs`, `HARNESS_SHOW_DIFFS`, or `-show-diffs` to
false to disable diff output. Diffs are generated from per-call file snapshots,
so repeated edits to the same file show incremental changes rather than a
repository-wide diff.

## Delegation

`delegate` starts a child agent using the requested agent definition, or the
current agent when omitted. The model-facing `agent` choices are limited to
agents whose tools are a subset of the current parent agent's tools. Each
delegate call runs for at most `delegate_max_turns` model turns by default.

Child agents get private todo stores. Their transcripts are saved under
`children/<child-id>/` alongside the parent session, and child token usage is
included in turn/session usage.

## Parallelism

Independent read-only tool calls can run concurrently when the model batches them
in one tool turn. Mutating calls remain ordering barriers, and results are still
recorded in the model's original call order.

## Truncation And Artifacts

Tool results are centrally capped at 64 KB or 1000 lines by default. Configure
this with `tool_result_max_bytes` / `tool_result_max_lines`, or
`HARNESS_TOOL_RESULT_MAX_BYTES` / `HARNESS_TOOL_RESULT_MAX_LINES`.

Truncated results include a marker in the model-visible text, a warning in the
UI, and the full output is archived under the session directory when available.
The model-visible tool result includes the absolute artifact path so the next
turn can inspect it with `read_file` or search it with `rg`.

Disabled optional CLI-backed tools are reported on stderr at startup. For
example, an explicit `-search-tools rg` request warns if `rg` is unavailable.
These warnings are suppressed by `-q` / `--quiet` or `--log-level error`.
