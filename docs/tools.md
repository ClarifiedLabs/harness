# Tools

Harness exposes a small built-in tool set to the model. Tool schemas and exact
implementation contracts are maintained in [design.md](design.md) section 9.
This page is the operational overview.

## Built-In Tools

| tool | purpose |
|---|---|
| `read_file` | read line-numbered file content; supports `offset` and `limit` |
| `list_dir` | list directory entries with type and size, non-recursive |
| `grep` | run host `grep` with argv-style args |
| `rg` | run host ripgrep when available |
| `edit` | edit existing files with exact-text replacements |
| `write_file` | create or overwrite a file, creating parent directories |
| `apply_patch` | apply Codex-format add/delete/update/move patches |
| `run_command` | run a shell command or direct argv program |
| `git` | run host git with `--no-pager`, when git is installed |
| `git_readonly` | restricted git subcommands for read-only agents |
| `web_fetch` | fetch HTTP(S) content and reduce HTML to readable text |
| `write_tmp_file` | write scratch files under a private temp directory |
| `update_todos` | replace the current todo list for multi-step work |
| `delegate` | run a configured child agent and return its final report |
| `background_jobs` | list, inspect, or cancel process-local background jobs |

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

## Command Execution

`run_command` accepts exactly one of:

- `command`: executed through `bash -lc` with `sh -c` fallback
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
When `show_diffs`, `HARNESS_SHOW_DIFFS`, or `-show-diffs` is enabled, harness
prints a unified before/after diff for each built-in file mutation tool call.
Diffs are generated from per-call file snapshots, so repeated edits to the same
file show incremental changes rather than a repository-wide diff.

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
