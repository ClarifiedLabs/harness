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
| `record_plan` | persist a self-contained markdown implementation plan in the session; the user is shown the plan file path |
| `request_implementation` | request an approved handoff of the latest recorded plan (plan agent only) |

`apply_patch` (Codex-format add/delete/update/move patches) is no longer in the
default tool set — `edit` and `write_file` subsume it. It still ships in the tool
catalog, so an agent can opt back in by naming `apply_patch` in its
`allowed_tools` whitelist.

`read_file` reads one file via `path` (with `offset`/`limit`), or several at once
via `paths[]` — each file is rendered under a `==> path <==` header with its own
per-file line budget. For cross-harness compatibility `path` also silently accepts
the aliases `file`, `file_path`, `filePath`, `filename`, `filepath`,
`absolute_path`, and `target_file` (and `paths` accepts `files`); these are
intentionally not listed in the tool schema, and the canonical name wins if both
are supplied. When a single read is cut off at the line limit it ends with
`[file truncated at line N; continue with offset=N+1]`. `glob` walks recursively
from an optional `root`, where `**` matches across directory segments (and `*`/`?`/
`[…]` match within one segment), returning matching paths with type and size sorted
by path. `edit` takes an optional per-edit `replaceAll` flag that replaces every
occurrence of `oldText` instead of requiring a unique match, reporting the
replacement count.

When [MCP](mcp.md) is enabled, downstream MCP tools also appear, namespaced as
`mcp__<server>__<tool>`. When [LSP](lsp.md) is enabled, native `lsp_*` code
intelligence tools are also registered; most are read-only, while `lsp_rename`
applies language-server text edits.

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
limits. The wrapper rejects short `-r` forms because replacement output must use
`--replace` explicitly.

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
the parent agent. Background delegates are join-required: after one useful parent
turn, harness waits for them and makes the parent synthesize their reports
before ending the prompt. Their model usage is included exactly once in parent
prompt/session totals. Ordinary background commands remain detached. Jobs live only
in the current harness process and are abandoned when that process exits.

## File Mutation

`edit`, `write_file`, and `apply_patch` are the built-in file mutation tools.
By default, harness prints a unified before/after diff for each built-in file
mutation tool call. Set `show_diffs`, `HARNESS_SHOW_DIFFS`, or `-show-diffs` to
false to disable diff output. Diffs are generated from per-call file snapshots,
so repeated edits to the same file show incremental changes rather than a
repository-wide diff.

## Delegation

`delegate` starts a fresh-context child agent using the requested agent
definition, or the current agent when omitted. The model-facing `agent` enum and
its deterministic `Available:` catalog include only agents whose tools
are a subset of the current parent's live tools. Agent descriptions are selection
policy, not cosmetic labels: every new custom agent must provide a nonblank
`description` stating when the parent should use it. Same-named built-in
overrides may inherit the built-in description.

Built-in child roles are:

| agent | when to use |
|---|---|
| `explore` | broad code search, architecture/dependency tracing, root-cause investigation, or questions spanning many files; read-only and not intended for a known-file lookup |
| `independent` | bounded end-to-end work that can proceed without parent or user input |
| `plan` | collaborative read-only planning; available only when its complete tool set is a subset of the parent |
| `auto` | the current general-purpose behavior |

A child always receives the selected agent's configured tool set. Delegate calls
cannot override or narrow it; select or define a different agent when a task needs
a different capability bundle.

A delegated task should include the objective, scope, constraints, expected
report, and verification. Children receive a child-only prompt reminding them
that they report to the parent, own only the delegated scope, should not ask the
user questions, and must return a concise evidence-backed report. The parent
should synthesize and verify that report. Prefer direct tools for known files or
symbols, one- or two-step tasks, immediate blockers, and tightly coupled work.

Foreground delegates run in the ordinary serialized tool loop because children
share the checkout and may write. Use `background:true` only for independent
read-only or disjoint work while useful parent work remains. Completion is delivered automatically as one-shot request context; do not poll or
duplicate a background child's work. Harness permits one subsequent useful parent
model round, then joins outstanding background delegates and continues the parent
for synthesis before allowing the turn to end.

Each call runs for at most `delegate_max_turns` turns (default `20`).
Recursion starts at root depth `0` and is limited by `delegate_max_depth`
(default `3`); the deepest child does not receive `delegate`, and an over-depth
launch fails before a model request. Children inherit the root
`max_prompt_tokens` and `max_prompt_cost_usd` per-prompt safety ceilings. These are
per-child ceilings, not a hierarchy-wide shared budget.

Child agents get private todo stores. Their transcripts are saved under
`children/<child-id>/` alongside the parent session. Foreground and background
child token/cost usage is included exactly once in parent prompt/session usage.

Because JSON-schema composition is rejected by some providers, `delegate.tools`
advertises only the conservative intersection of tool names supported by every
currently selectable agent. Omit `tools` to give the chosen child role its full
configured surface; runtime validation still rejects any explicit unsupported name.

## Parallelism

Independent read-only tool calls can run concurrently when the model batches them
in one tool turn. Mutating calls remain ordering barriers, and results are still
recorded in the model's original call order.

## Truncation And Artifacts

Tool results are centrally capped at 64 KB or 1000 lines by default. Configure
this with `tool_result_max_bytes` / `tool_result_max_lines`, or
`HARNESS_TOOL_RESULT_MAX_BYTES` / `HARNESS_TOOL_RESULT_MAX_LINES`. Noisy file
inspection tools have smaller defaults unless a global cap is configured:
`rg`/`grep` use 32 KB or 500 lines, and `read_file` uses a 500-line default
window plus a 32 KB result cap. Override them with `rg_result_max_bytes` /
`rg_result_max_lines`, `grep_result_max_bytes` / `grep_result_max_lines`,
`read_file_default_limit`, and `read_file_result_max_bytes` /
`read_file_result_max_lines`; each has a matching `HARNESS_*` environment
variable.

Truncated results include a marker in the model-visible text, a warning in the
UI, and the full output is archived under the session directory when available.
The model-visible tool result includes the absolute artifact path so the next
turn can inspect it with `read_file` or search it with `rg`.

Disabled optional CLI-backed tools are reported on stderr at startup. For
example, an explicit `-search-tools rg` request warns if `rg` is unavailable.
These warnings are suppressed by `-q` / `--quiet` or `--log-level error`.
