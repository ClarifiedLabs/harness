# Tools

Harness exposes a small built-in tool set to the model. Tool schemas and exact
implementation contracts are maintained in [design.md](design.md) section 9.
This page is the operational overview.

## Built-In Tools

| tool | purpose |
|---|---|
| `read_file` | read line-numbered file content; supports `offset`/`limit`, or `paths[]` for multi-file reads |
| `view_image` | attach a local PNG, JPEG, WebP, or non-animated GIF to the next model request |
| `list_dir` | list directory entries with type and size, non-recursive |
| `glob` | recursively find files/dirs by glob, including `**` patterns; read-only |
| `search` | run up to 16 bounded content queries with context, lines, files, counts, or existence output |
| `inspect` | run up to 16 independent read/search/glob/list/workspace-summary operations concurrently |
| `edit` | edit existing files with exact-text replacements; optional `replaceAll` |
| `write_file` | create or overwrite a file, creating parent directories |
| `run_command` | run a shell command or direct argv program |
| `git` | run host git with `--no-pager`, including a compact `workspace_summary` workflow |
| `git_readonly` | restricted git subcommands for read-only agents |
| `web_fetch` | fetch bounded HTTP(S) text, removing common HTML chrome while preserving block structure and links |
| `write_tmp_file` | write scratch files under a private temp directory |
| `update_todos` | replace the current todo list for multi-step work |
| `delegate` | run a configured child agent and return its final report |
| `background_jobs` | list, inspect, wait for, or cancel process-local background jobs |
| `record_plan` | persist a self-contained markdown implementation plan in the session; the user is shown the plan file path |
| `request_implementation` | request an approved handoff of the latest recorded plan (plan and interactive auto agents) |

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
are supplied. `view_image` is the read-only binary-image counterpart: it accepts
`path` plus optional `detail` (`auto`, `low`, `high`, or `original`, default
`high`), sniffs and validates the file content, and returns a rich image tool
result without putting base64
in terminal summaries, hooks, or diagnostics. It rejects models that do not
advertise image input before reading the file. The path must identify a regular
file; directories, devices, and FIFOs are rejected. Reads are cancellable and
bounded to 10 MiB decoded per image before base64 encoding. Each encoded image
has the corresponding base64 ceiling, and the complete retained request—manual
user images plus nested rich tool-result images across prior rounds—is limited
to 32 MiB encoded.
When a single read is cut off at the line limit it ends with
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

## Search and Inspection

The default model surface exposes one typed `search` tool. It accepts a
`queries[]` array of up to 16 independent searches. Each query has a pattern,
optional `paths[]` and `globs[]`, literal/regex and case controls, bounds, and an
`output` mode: `context`, `matches`, `files`, `count`, or `exists`. Independent
queries execute concurrently and results stay in input order. Harness uses
ripgrep when it is installed and a bounded standard-library walker otherwise,
so the tool contract does not depend on the host CLI.

For a batch with more than one `context` or `matches` query, Harness renders
each query's match summary followed by one shared source-context section.
Overlapping or adjacent source windows are merged and labeled with the query
numbers they serve, so the same lines do not consume the result repeatedly.
Each query still receives its own existing 400-source-line allowance; batching
does not impose a new aggregate cap.

`inspect` batches heterogeneous repository orientation in the same way. Its
`operations[]` may invoke `read_file`, `search`, `glob`, `list_dir`, or
`workspace_summary`; operations execute concurrently and render under indexed
headers. Prefer it to one read-only lookup per model turn. After three
consecutive single-lookup turns, harness adds a one-time soft reminder to batch.

Raw `grep` and optional `rg` wrappers remain in the constructible catalog for a
custom agent that explicitly names them in `allowed_tools`; built-in agents do
not advertise them. `grep`, `rg`, `git`, and direct-argv `run_command` calls expect JSON arrays of
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
`--max-columns`), trailing them with `… [N chars clamped]`.

## Command Execution

`run_command` accepts either one command or an ordered `steps` array:

- `command`: executed through a non-login `bash -c` (with `sh -c` fallback). The
  login PATH a login shell would have added is resolved once at startup and merged
  into the command environment, so build/test toolchains are still found without
  paying the login-profile cost on every call.
- `argv`: direct program invocation with literal args and no shell
- `name`: optional top-level receipt label; unavailable with `steps`
- `output_mode`: top-level `auto` (default), `receipt`, or `full`; unavailable
  with `steps`
- `steps`: up to 16 named `command`/`argv` entries, run serially. Top-level
  `cwd` and `timeout_seconds` are inherited unless a step overrides them.

Foreground calls capture combined stdout/stderr and append `[exit code: N]`.
Non-zero exit is not a tool error; it is returned as ordinary command output so
the model can react to failing builds, tests, and searches.

For an ordinary top-level command, `output_mode:"auto"` preserves successful
output through 8 KiB. Above that threshold it returns a compact `PASS` receipt
with the command label, duration, exit status, output byte count, and a bounded
tail; the complete result is archived through the normal tool-result artifact
path. Failures return a `FAIL` receipt with bounded diagnostics in `auto`; a
clipped original is archived. `receipt` always uses the compact successful
form, while `full` keeps the prior bounded full-output behavior. Every form
retains the `[exit code: N]` trailer. The same policy and artifact recovery
apply to background command completion and explicit `background_jobs` get/wait
results.

Steps stop on the first failure by default (`stop_on_failure:false` continues).
Successful output is replaced with one `PASS <name> (<duration>)` receipt per
step. Failures include a bounded output excerpt and skip count. Any suppressed
successful output or clipped failure output is archived through the normal
session artifact path, so the model can inspect it without carrying it in every
later request. `steps` is foreground-only.

`run_command`, `grep`, `rg`, and `web_fetch` can set `background:true` to return
a job id immediately. `delegate` can also run as a background child agent.
Local background work carries a canonical resource lease. `run_command`
defaults to an `exclusive` lease on its canonical cwd; callers may set
`background_lease:{"resource_key":"...","access":"read_only"}` only when the
command will not mutate that resource. The lease is scheduling metadata: it
coordinates concurrent jobs but neither restricts what the command can do nor
makes it read-only. Legacy top-level `resource_key` and `access` inputs remain
accepted but are not advertised. A delegate's access defaults from its selected
agent, and its `scope` defaults to the canonical cwd. Built-in `explore`,
`plan`, and `review` agents are read-only; `auto`, `independent`, custom, and
implementation-mode delegates are exclusive by default. Background `grep` and
`rg` automatically use a `read_only` lease on their cwd. Multiple read-only jobs may share a resource,
while an exclusive job conflicts with every active lease for the same resource
and reports the existing job id. Jobs on different resources remain concurrent.
The lease is an exact-key match on the canonical path: it does not protect the
whole workspace, so two jobs on sibling or nested directories do not conflict.
`web_fetch` does not lease the local workspace.
Completed background job summaries are delivered once as request-only context to
the parent agent. Background delegates are join-required: after one useful parent
turn, harness waits for them and makes the parent synthesize their reports
before ending the prompt. Their model usage is included exactly once in parent
prompt/session totals. Ordinary background commands remain detached. Jobs live only
in the current harness process and are abandoned when that process exits.
Completion is normally delivered automatically. When later work has a strict
dependency, `background_jobs {"action":"wait"}` waits on manager notifications
instead of polling `get` or `list`; add `id` to target one job, use `ids` with
`until:"all"` to join a group, or omit both to select the jobs currently
running. `until` defaults to `first`. The selected set is stable: jobs launched
after the wait starts are never added. Its timeout defaults to 120 seconds and
a timeout returns the latest selected status as a normal result. Omit
`timeout_seconds` for ordinary dependency waits; do not use a short timeout as
a status probe.

For repository orientation, `git {"workflow":"workspace_summary"}` combines
branch/porcelain status, HEAD, staged and unstaged diff stats, and both whitespace
checks into one read-only result. Use ordinary `git {"args":[...]}` afterward
when the actual patch or another native subcommand is needed. To record finished
work, `git {"workflow":"commit","paths":[...],"message":"type: subject"}` stages
only the exact repository-relative file paths, runs the staged whitespace check,
commits only those paths, and returns the new commit plus remaining workspace
status. It rejects `.`, directories, globs, and pathspec magic.

`git_readonly` exposes an audited query-only allowlist for restricted agents:
`blame`, `cat-file`, `check-attr`, `check-ignore`, `check-mailmap`,
`check-ref-format`, `cherry`, `count-objects`, `describe`, `diff`,
`diff-files`, `diff-index`, `diff-tree`, `for-each-ref`, `grep`, `log`,
`ls-files`, `ls-tree`, `merge-base`, `name-rev`, `range-diff`, `rev-list`,
`rev-parse`, `shortlog`, `show`, `show-branch`, `show-ref`, and `status`.
Mixed read/write commands such as `branch`, `config`, `remote`, `reflog`,
`submodule`, `tag`, and `worktree` are intentionally excluded. The runner
disables pagers, optional locks, filesystem monitors, external diff/textconv
helpers, prompts, output-file flags, and signature helpers.

`update_todos` is available to every built-in agent. Use it for meaningful
multi-step work, update it at phase boundaries, and do not spend a turn only on
bookkeeping. Each call replaces the whole list. Harness returns a compact
completion-count acknowledgment to the model while the interactive REPL renders
the full checklist for the user. Custom agents with an explicit `allowed_tools`
list may omit it.

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
| `review` | findings-first read-only review of a concrete code change |
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

Set `mode:"implementation"` only for scoped mutating implementation. It adds an
implementation-mode system block directing the child to make the requested
changes, verify them, and return an exact handoff with changed paths, checks
run, and any remaining work. Omit `mode` for exploration and review delegates.

Set `continue_child_id` to a terminal sibling delegate ID when the same child
runtime should continue retained work. Harness leaves the source child
unchanged and creates a fresh child record containing the prior transcript,
todos, prompt-cache/proxy identifiers, and provider continuation anchor. The
new prompt explicitly tells the child to re-check repository state. Omitted
`agent`, `mode`, and `max_turns` values inherit the source contract; explicitly
supplied values must match it. The continuation receives a fresh loop allowance
equal to that effective budget; prior physical turns and usage are not counted
again. When the retained request is already at or below 60% of the current
context window, the fresh child reuses the complete transcript and safe remote
continuation anchor directly.

Continuation is intentionally strict. The source must belong to the immediate
parent, have terminal metadata and resumable `state.json`, and carry the same
runtime fingerprint as the current provider, model, system prompt, tools,
reasoning and server-tool settings, safety budgets, and compaction policy.
When retained context plus the new prompt exceeds 60%, Harness makes one
tool-less maintenance call that summarizes the complete source transcript into
a typed compaction checkpoint. The checkpoint keeps the active prompt and
steering instructions verbatim, carries typed file activity, preserves the raw
source messages in the new child's `compactions/` archive, and resets the remote
provider anchor. Harness re-estimates the checkpoint request and continues only
when it is at or below 60%; a failed summary or still-oversized exact
instructions reject the continuation instead of dropping state. Checkpoint
usage is charged to the new child and returned even when that final pressure
check rejects it.

Legacy children without a fingerprint, children from another parent, changed
runtimes, and missing state remain ineligible. Foreground and background
delegates use the same checks; background launch receipts show the inherited
budget, mode, and source child. The final delegate receipt distinguishes a
compact-checkpoint continuation. Child metadata and `session stats` record the
continuation mode plus before/after/window token estimates.

Foreground delegates run in the ordinary serialized tool loop because children
share the checkout and may write. Use `background:true` only for independent
read-only or disjoint work while useful parent work remains. Background
`explore`, `plan`, and `review` calls default to shared `read_only` access;
`auto`/`independent` and implementation mode default to `exclusive`. Set
`scope` to a narrower workspace path for mutating siblings that own disjoint
areas.
Lease conflicts fail before a child starts and identify the active job.
Completion is delivered automatically as one-shot request context; do not poll or
duplicate a background child's work. Harness permits one subsequent useful parent
model round, then joins outstanding background delegates and continues the parent
for synthesis before allowing the turn to end.

For an independent review group, launch the reviewer delegates directly from
the parent in one assistant turn; read-only agent defaults let them share the
same scope. Synthesize their reports in the parent. If an immediate dependency
requires an explicit join, pass the returned
job IDs to one `background_jobs` call with `until:"all"`. Do not create a
coordinator child whose only work is launching reviewers and waiting for them;
that adds a model loop without adding review independence or evidence.

The `max_turns` schema publishes the active numeric `delegate_max_turns`
maximum (default `20`). Omitting `max_turns` uses that maximum; a lower value
selects a smaller tool-enabled loop budget, and an over-cap value is rejected
before a child is launched. Children receive the exact effective budget in
their system context. If the final budgeted turn still requests tools, Harness
may make one additional tools-disabled wind-down request so the transcript ends
with a concise handoff; `turns_used` records that physical request too.
Recursion starts at root depth `0` and is limited by `delegate_max_depth`
(default `3`); the deepest child does not receive `delegate`, and an over-depth
launch fails before a model request. Children inherit the root
`max_prompt_tokens` and `max_prompt_cost_usd` per-prompt safety ceilings. These are
per-child ceilings, not a hierarchy-wide shared budget.

Child agents get private todo stores. Their transcripts are saved under
`children/<child-id>/` alongside the parent session. Foreground and background
child token/cost usage is included exactly once in parent prompt/session usage.
Child metadata records background resource/access leases, requested and effective
turn budgets, physical turns used, and a structured termination reason:
`model_completed`, `turn_limit`,
`token_limit`, `cost_limit`, `repeat_guard`, `error_guard`, `cancelled`, or
`error`. A termination reason describes why Harness stopped the loop; it does
not claim that the delegated task is semantically complete.

`delegate_output=lines` adds a curated prompt-scoped view of foreground,
background, concurrent, and nested child activity to parent stderr. Direct
children use `[delegate dN agent]`; nested children add `depth=N`. The view
includes lifecycle, line-coalesced assistant output, safe tool summaries,
strictly allowlisted harness notices, structured retry/HTTP status fields, and
provider-visible reasoning summaries only when the resolved child reasoning
summary mode permits them. It excludes the delegated task, raw tool inputs and
results, commands, URLs, provider messages/IDs, error strings, and opaque
reasoning. `-v` and `-tool-stream` affect only parent diagnostics.

Inline output is bounded and best-effort. It may wait for a natural parent
assistant line boundary, and feed eviction appears as
`[delegate output] omitted N events`. It never changes parent stdout, transcript,
model context, delegate results, or usage. The child `raw.ndjson` remains the
complete source; use `harness session replay --follow` when full-fidelity output
is required. `delegate_output=status` keeps only the compact TTY row, `off`
disables delegate UI, and quiet disables both status and lines.

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
turn can inspect it with `read_file` or `search`. When live retention later
removes an old read-only result body, Harness leaves a typed receipt with the
tool, status, byte counts, bounded head, and recovery path; the exact original
is preserved in the same artifact store. If that artifact write fails, the
original stays in live context instead of being discarded.

Disabled optional CLI-backed default tools are reported on stderr at startup.
These warnings are suppressed by `-q` / `--quiet` or `--log-level error`.
