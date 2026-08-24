# Tools

Harness exposes a small built-in tool set to the model. Tool schemas and exact
implementation contracts are maintained in [design.md](design.md) section 9.
This page is the operational overview.

## Built-In Tools

| tool | purpose |
|---|---|
| `read` | read line-numbered file content with `path` and optional `offset`/`limit`, or list a directory |
| `view_image` | attach a local PNG, JPEG, WebP, or non-animated GIF to the next model request |
| `edit` | edit existing files with exact-text replacements; optional `replaceAll` |
| `write` | create or overwrite a file, creating parent directories |
| `shell` | run a shell command or direct argv program; co-issued calls may run concurrently (best-effort) |
| `git` | run host git with `--no-pager`, including a compact `workspace_summary` workflow |
| `git_readonly` | restricted git subcommands for read-only agents |
| `web_fetch` | fetch bounded HTTP(S) text, removing common HTML chrome while preserving block structure and links |
| `write_tmp_file` | write scratch files under a private temp directory |
| `update_todos` | replace the current advisory TODO list for multi-step work |
| `delegate` | run a configured child agent and return its final report |
| `background_jobs` | list, inspect, wait for, or cancel process-local background jobs |
| `tool_catalog` | conditionally list, describe, and activate optional MCP/LSP tools |
| `record_plan` | Record a complete implementation plan for handoff to an implementation agent |
| `handoff` | Handoff the recorded plan to an implementation agent (interactive plan agent; `agent` enum = exclusive agents `auto`, `independent`, plus custom `workspace_access: exclusive`; omit for default `auto`) |

The default registry is `read`, `view_image`, `edit`, `write`, `shell`, and
`web_fetch`, in that order. The complete constructible catalog adds `git` and
`git_readonly` when the host `git` binary is available, plus `write_tmp_file`.
Coordination tools and discovered MCP/LSP tools are registered separately for
the agents that expose them.

When the aggregate model-facing declarations for an agent's optional MCP/LSP
tools exceed 32 KiB, Harness keeps those tools registered but initially hides
their schemas behind `tool_catalog`. `list` filters the available catalog,
`describe` returns exact schemas for up to 16 names, and `activate` publishes
selected schemas on the next model turn. Activation lasts for that agent
runtime. This is a context optimization, not an authorization boundary; agent
tool allowlists are still enforced when the registry is constructed.

The former callable names `read_file`, `write_file`, `list_dir`, `glob`, `grep`,
`rg`, and `apply_patch` are removed from the catalog and cannot be restored with
`allowed_tools`. Use `read` for files and directories, `write` for whole-file
writes, `edit` for replacements, and `shell` with host commands such as `rg`,
`rg --files`, `grep`, `find`, and `git --no-pager` for repository inspection.

Models should issue independent schema-visible calls together in one tool turn.
Registered calls are parallel-eligible by default, regardless of `ReadOnly`; a
small set of workflow tools explicitly opts ordering-sensitive inputs out.
Harness automatically queues overlapping built-in `write`/`edit` mutations in
model emission order using normalized lexical paths, while unrelated calls may
continue. Results and transcript blocks always remain in emission order.
`shell.steps` still run serially inside one call. No sandbox is added; Harness
inherits the host sandbox.

`read` reads one file via `path` with optional `offset`/`limit`. A directory
path returns a bounded, non-recursive listing instead of a tool error. For
cross-harness compatibility `path` also silently accepts the aliases `file`,
`file_path`, `filePath`, `filename`, `filepath`, `absolute_path`, and
`target_file`; these are intentionally not listed in the tool schema, and the
canonical name wins if both are supplied. `view_image` is the read-only binary-image counterpart: it accepts
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
`[file truncated at line N; continue with offset=N+1]`. Recursive path discovery
belongs to `shell` with a host command such as `rg --files` or `find`. `edit`
takes an optional per-edit `replaceAll` flag that replaces every
occurrence of `oldText` instead of requiring a unique match, reporting the
replacement count.

Set `read.include_sha256` to prepend the full file's SHA-256 digest. Pass that
digest as `files[].expected_sha256` to `edit` or `expected_sha256` to `write` to
reject a mutation when the file changed after it was read. Digest checks happen
during mutation preflight, so a stale multi-file edit writes nothing and reports
the `stale_file` error kind. Hashing the full file is opt-in; ordinary bounded
reads retain their streaming behavior.

When [MCP](mcp.md) is enabled, downstream MCP tools also appear, namespaced as
`mcp__<server>__<tool>`. When [LSP](lsp.md) is enabled, native `lsp_*` code
intelligence tools are registered as a contiguous block immediately after
`view_image`. Most are read-only; `lsp_code_action`,
`lsp_format_document`, and `lsp_rename` apply validated language-server text
edits. All inherit default-parallel dispatch; Harness does not infer the remote
paths an LSP operation may affect. Interactive sessions can inspect or change
this process-local exposure with `/lsp`.

## Session Goals

Session goals are deliberately not model-callable tools. They are created and
controlled exclusively through the interactive `/goal` command. See
[Session goals](usage.md#session-goals) for setting, pausing, resuming, clearing,
persistence, and continuation-cap behavior.

## Search and Inspection

Built-in agents intentionally use the general `shell` tool for repository
search instead of a typed content-search tool. Prefer argv-form `rg` for content,
`rg --files` or `find` for path discovery, and `shell.steps[]` when several
ordered lookups belong in one process call. Independent `shell` or `read` calls can instead be coissued in one model turn. Native command semantics decide
regex syntax, ignore behavior, output shape, and exit status; scope the command
to the relevant repository path and use `read` for targeted source context.

After three consecutive turns containing one unbatched repository lookup,
Harness adds a one-time soft reminder to coissue independent `read` calls or
batch repository lookups in one `shell` call. The reminder names only tools
present in the built-in surface.

A `read` path that does not exist fails with `similar existing paths: <up
to 3>` appended. Harness first scans the same directory and one parent level for
a mistyped directory, then uses a bounded four-level/1024-entry recursive
fallback from the nearest existing directory, so a misplaced or mistyped path
can be retargeted without another lookup round trip.

Search commands run through `shell`, not dedicated wrappers. Direct-argv
`shell` calls expect a JSON array of strings; `command` accepts a shell string
when pipes, redirection, expansion, or other shell syntax is required. Harness
does not rewrite host `rg`/`grep` arguments or results, so native CLI semantics
decide regex syntax, ignore behavior, limits, output shape, and exit status.

## Command Execution

`shell` accepts either one command or an ordered `steps` array:

- `argv`: preferred direct program invocation with literal args and no shell
- `command`: executed through a non-login `bash -c` (with `sh -c` fallback). The
  login PATH a login shell would have added is resolved once at startup and merged
  into the command environment, so build/test toolchains are still found without
  paying the login-profile cost on every call.
- `name`: optional command or step-batch label
- `output_mode`: `auto` (default), `receipt`, or `full`; with `steps`, `auto`
  and `receipt` return compact receipts while `full` returns the combined step transcript
- `steps`: up to 16 named `command`/`argv` entries, run serially in either
  foreground or background mode. Top-level `cwd` and `timeout_seconds` are
  inherited unless a step overrides them.

Shell calls capture combined stdout/stderr and append `[exit code: N]`.
Non-zero exit is not a tool error; it is returned as ordinary command output so
the model can react to failing builds, tests, and searches. Structured result
metrics separately record exit status, timeout, cancellation, and step failures;
`session errors` consumes those metrics rather than parsing text. Completed
background shell jobs persist the same metrics exactly once in a diagnostics-only
`background_job_result` session event when completion is observed at a request,
prompt/idle boundary, session rotation, or graceful shutdown. The event retains
the agent/model identity that launched the job even if the session switched before
completion.

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
later request. Background steps preserve the same receipts, original transcript,
per-step overrides, stop/continue behavior, and aggregate diagnostics; unresolved
step timeouts default to 1200 seconds in background mode instead of 120 seconds.
`stop_on_failure:false` can continue after an ordinary failure or timeout, but
cancellation always stops the batch before any later step starts. An incomplete
process-group reap always retains the full transcript for artifact recovery.

`shell` commands or ordered step batches and `web_fetch` can set
`background:true` to return a job id immediately. `delegate` can also run as a
background child agent. Graceful process exit and session rotation cancel active
jobs, wait briefly for context-responsive runners to publish final diagnostics,
and then continue teardown; forced exit remains immediate.
Local background work carries a canonical resource lease. `shell`
defaults to an `exclusive` lease on its canonical cwd; callers may set
`background_lease:{"resource_key":"...","access":"read_only"}` only when the
command will not mutate that resource. The lease is scheduling metadata: it
coordinates concurrent jobs but neither restricts what the command can do nor
makes it read-only. Legacy top-level `resource_key` and `access` inputs remain
accepted but are not advertised. A delegate's access defaults from its selected
agent, and its `scope` defaults to the canonical cwd. Built-in `explore`,
`plan`, and `review` agents are read-only; `auto`, `independent`, custom, and
implementation-mode delegates are exclusive by default. A background `shell`
call defaults to an exclusive lease even when it runs a search command; callers
may explicitly select `read_only` when the command cannot mutate the resource.
Multiple read-only jobs may share a resource,
while an exclusive job conflicts with every active lease for the same resource
and reports the existing job id. Jobs on different resources remain concurrent.
The lease is an exact-key match on the canonical path: it does not protect the
whole workspace, so two jobs on sibling or nested directories do not conflict.
`web_fetch` does not lease the local workspace. `web_fetch` returns text
only; non-textual responses fail with an error that points at downloading the
archive or binary with `shell` (for example `curl`) for inspection.
Completed background job summaries are delivered once as request-only context to
the parent agent. Interactive completion notices for background delegates also show
the child session's token buckets, cost when the existing session-summary formatter
shows it, and successful compaction count.
Those token/cost totals are display-only and are already included exactly once in
the parent prompt/session totals. Interactive JSON mirrors the same enriched text
as a normal `notice` event; one-shot mode keeps only its existing final aggregate
summary. Background delegates are join-required: after one useful parent turn,
harness waits for them and makes the parent synthesize their reports before ending
the prompt. Ordinary background commands remain detached. Jobs live only in the
current harness process and are abandoned when that process exits.
Completion is normally delivered automatically. When later work has a strict
dependency, `background_jobs {"action":"wait"}` waits on manager notifications
instead of polling `get` or `list`; add `id` to target one job, use `ids` with
`until:"all"` to join a group, or omit both to select the jobs currently
running. `until` defaults to `first`. The selected set is stable: jobs launched
after the wait starts are never added. Its timeout defaults to 120 seconds and
a timeout returns the latest selected status as a normal result. Omit
`timeout_seconds` for ordinary dependency waits; do not use a short timeout as
a status probe. An accepted user steer while that wait is blocked detaches the
wait immediately: the selected jobs continue, and the tool reports that its
final aggregate result will arrive automatically. Detachment retains the
call-time selection, `until` condition, launch order, and already-running timeout
rather than starting a new wait. When the selected completion or timeout later
resolves, harness delivers that aggregate exactly once as request-only context
and starts an interactive continuation only after already-delivered user work,
drafts, approvals, EOF, shutdown, and interrupts have priority. Do not replace
one dependency wait with `get`/`list` polling.

For repository orientation, `git {"workflow":"workspace_summary"}` combines
branch/porcelain status, HEAD, staged and unstaged diff stats, and both whitespace
checks into one read-only result. Use ordinary `git {"args":[...]}` afterward
when the actual patch or another native subcommand is needed. To record finished
work, `git {"workflow":"commit","paths":[...],"message":"type: subject"}` stages
only the exact repository-relative file or directory paths (a directory
includes everything beneath it), runs the staged whitespace check,
commits only those paths, and returns the new commit plus remaining workspace
status. It rejects `.`, `..`, globs, and pathspec magic. A failed whitespace
check rejects with the offending lines plus a corrective sentence (strip the
trailing whitespace and re-stage before retrying).

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

`update_todos` is available to every built-in agent, including `plan`. Use it as
an advisory checklist for meaningful multi-step work, update it at phase
boundaries, and do not spend a turn only on bookkeeping. Each call replaces
the complete list with `{step,status}` entries. Status is `pending`,
`in_progress`, or `completed`, and at most one item may be `in_progress`.
Harness assigns no lifecycle or completion authority to the list.

The root TODO list is saved in `state.json`, restored by `-resume`, and cleared
by `/clear`. Resume and transcript rewrites inject the unresolved list once as
request-only recovery context. Otherwise, an unresolved list with no successful
update is reminded after 12 conversational model rounds; ignored reminders back
off after another 24, 48, and 96 rounds, capped at 96 thereafter. A successful
update or delivered recovery reminder resets the cadence, and transport retries
do not advance it. Child agents receive private TODO stores.

`record_plan` is part of the default coordination tool set. The `plan` agent
receives it alongside `update_todos`; `auto`, `independent`, and
default-inheriting custom agents receive it in addition to `update_todos`, so a
session can capture a durable implementation plan and later run `/handoff`
without switching agents first. `record_plan` writes a
self-contained Markdown artifact to `<session>/plans/NNNN-<slug>.plan.md` with
temp-file-then-rename durability and makes that artifact the session's latest
plan. Older plan files remain immutable. In an interactive root session, the
plan agent may call `handoff`; Harness displays the full latest
plan, asks for approval, archives the planning transcript, and seeds the
implementation agent with the complete plan. Delegated plan agents may record
private child plans but cannot request an interactive handoff. Agents with
`record_plan` but not `handoff` (`auto`, `independent`, default-inheriting
custom) can record plans; use `/handoff` to review and approve one.

## File Mutation

`edit` and `write` are the built-in file mutation tools.
A mutation may carry the SHA-256 precondition returned by
`read.include_sha256`; a mismatch or a previously read file that disappeared
fails as `stale_file` before any write. `edit` applies this check to every file
as part of its existing all-files preflight. Omitting the digest preserves the
unconditional behavior.
A repeated `edit` `files[].path` is applied in order against the earlier
entry's result rather than rejected; a stale `oldText` in a repeated entry
fails with the ordinary not-found error and nothing is written.
`edit` validates every replacement in a file before writing and reports all
missing or ambiguous blocks together. Ambiguous errors include up to five
occurrence line numbers; close not-found candidates include the first divergent
line. Fuzzy matching uses a normalized comparison view but maps the selected
span back to the original bytes, so unrelated whitespace and typographic
punctuation are preserved. A top-level `path` is the default base for every
`files` entry that omits its own `path`; entries naming the same path are
tolerated, and an entry naming a different path is rejected as ambiguous.
By default, harness prints a unified before/after diff for each built-in file
mutation tool call. Set `show_diffs`, `HARNESS_SHOW_DIFFS`, or `-show-diffs` to
false to disable diff output. Diffs are generated from per-call file snapshots,
so repeated edits to the same file show incremental changes rather than a
repository-wide diff. On a color terminal, displayed diff content and added/
removed rows use the configured `color_theme` dark/light code palette; changing
the palette does not change `show_diffs`, snapshot generation, diff text, or
full-row background-erase behavior.

With native LSP enabled, a successful built-in `edit` or `write` also synchronizes
each changed supported file with its configured language server and appends the
fresh published diagnostics to the tool result. Up to eight unique paths are
queried concurrently with a three-second wait per path, then rendered in mutation
order. Unsupported paths and unavailable servers remain silent; a diagnostics
failure is reported as supplemental text and never changes a successful mutation
into a failed one. Failed mutations do not request diagnostics.

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
| `auto` | the default general-purpose behavior |

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

When a child run fails only for transient provider reasons (rate limit,
overloaded, 5xx, provider-side timeout), the delegate error names those classes
and tells the parent to retry the delegate call once and then report the
blocker rather than retrying further; it is recorded with the `rate_limited` or
`provider_error` error kind. Permanent failures (unknown agent, tool-subset
rejection, non-retryable 4xx) are returned unchanged.

Set `continue_child_id` to a terminal sibling delegate ID when the same child
runtime should continue retained work. Harness leaves the source child
unchanged and creates a fresh child record containing the prior transcript,
private TODO list and latest plan, prompt-cache/proxy identifiers, and provider continuation anchor. The
new prompt explicitly tells the child to re-check repository state. Omitted
`agent`, `mode`, and `max_turns` values inherit the source contract; explicitly
supplied values must match it. The continuation receives a fresh loop allowance
equal to that effective budget; prior physical turns and usage are not counted
again. When the retained request is already at or below 60% of the current
context window, the fresh child reuses the complete transcript and safe remote
continuation anchor directly.

Root-wide delegate safety is separate from the model-facing `max_turns` field.
`delegate_max_active` and `delegate_max_descendants` bound simultaneous fan-out
and unique logical descendants. Continuations reuse the source logical
descendant rather than consuming another total slot.

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
their system context and are told to finish early once scoped work and focused
verification are done. If the final budgeted turn still requests tools, Harness
may make one additional tools-disabled wind-down request so the transcript ends
with a concise handoff; `turns_used` records that physical request too.
Recursion starts at root depth `0` and is limited by `delegate_max_depth`
(default `3`); the deepest child does not receive `delegate`, and an over-depth
launch fails before a model request. Children inherit the root
`max_prompt_tokens` and `max_prompt_cost_usd` per-prompt safety ceilings. These are
per-child ceilings, not a hierarchy-wide shared budget.

Child agents get private TODO and plan stores. Their transcripts are saved under
`children/<child-id>/` alongside the parent session. Foreground and background
child token/cost usage is included exactly once in parent prompt/session usage.
Interactive background-delegate terminal notices label the same already-accounted
usage, cost, and child compaction total as `child session` statistics.
Child metadata records background resource/access leases, requested and effective
turn budgets, physical turns used, and a structured termination reason:
`model_completed`, `turn_limit`,
`token_limit`, `cost_limit`, `repeat_guard`, `error_guard`, `cancelled`, or
`error`. A termination reason describes why Harness stopped the loop; it does
not claim that the delegated task is semantically complete.

The child's primary completion artifact is its final Markdown report. If it
knows the task outcome, it may append one terminal `harness-completion` fenced
JSON footer in one of these forms:

````text
```harness-completion
{"outcome":"complete"}
```
````

````text
```harness-completion
{"outcome":"blocked","blockers":["what prevents completion"]}
```
````

Harness strips a valid footer before returning the Markdown report to the
parent. Substantive details—findings, changed files, verification, evidence,
unreviewed scope, and remaining work—belong in Markdown, not status metadata.
New child metadata persists only outcome, optional bounded blockers,
source/contract provenance, and validation status. Missing, malformed,
duplicate, invalid, or oversized footers do not discard useful prose. A missing
footer produces an `unreported` receipt; other unusable footers remain explicit
unknown outcomes. Failed or canceled children record host/unavailable
provenance, and completion is never inferred from lifecycle termination.
Completion metadata is schema-local: use the Harness 0.5.11 binary to analyze
sessions created before 0.5.12. Every continuation produces a fresh report.
Delegate receipts also state how many root descendant slots remain (for example `3 of 16
descendant slots remaining`). Non-positive budget settings select the default
4-active/16-total limits rather than disabling the budget.

`delegate_output=lines` adds a curated prompt-scoped view of foreground,
background, concurrent, and nested child activity to parent stderr. Direct
children use `[delegate dN agent]`; nested children add `depth=N`. The view
includes lifecycle, line-coalesced assistant output, safe tool summaries,
strictly allowlisted harness notices, structured retry/HTTP status fields, and
provider-visible reasoning summaries only when the resolved child reasoning
summary mode permits them. It excludes the delegated task, raw tool inputs and
results, commands, URLs, provider messages/IDs, error strings, and opaque
reasoning. `-v` and `-tool-stream` affect only parent diagnostics.

With `delegate_tmux` on (default: on inside tmux, where `$TMUX` is set; any
explicit flag, env, or config setting — including `false` — overrides it)
inside a tmux session, each
delegate child also opens a display-only tmux view running
`harness session replay --follow` on the child session directory — the
full-fidelity live view, without the feed's curation bounds.
`delegate_tmux_layout` selects `pane` (default) or `window`: pane splits a
right-hand stack from the harness pane, while window keeps the historical
behavior of one detached window per child. Every child closes its view when it
ends, including failed or canceled children, and any still-tracked views close
when harness exits. At most `delegate_tmux_max_windows` (default 4) views are
open at once; additional children simply run without one. The views are
display-only: outside tmux the feature degrades to a single startup warning,
pane layout degrades to windows if `TMUX_PANE` is missing, and no
view failure ever affects the delegate run. `delegate_output` continues to
govern the inline status/lines display independently.

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

Every local tool accepts the reserved optional top-level `_stage` integer. Within
one assistant tool-use turn, execution starts at stage 1. An omitted `_stage`
inherits the current stage, while an explicit value sets it without moving
backward; explicit stages must therefore be positive and non-decreasing in model
emission order. Gaps are accepted as labels; only the relative order matters.
Harness validates the complete plan before dispatch, so an invalid value or a
backward stage rejects every call in the batch before tools or hooks run. For
example, these reads are independent and eligible to overlap:

```json
{"path":"go.mod","_stage":1}
{"path":"README.md","_stage":1}
```

A dependent edit and verification can stay in that assistant turn by using later
stages:

```json
{"path":"version.txt","content":"2\n","_stage":2}
{"argv":["go","test","./..."],"_stage":3}
```

Harness does not start a stage until all earlier stages have returned results.
Within one stage, co-issued registered calls are parallel-eligible by default,
including mutating `shell`, git, MCP/LSP, `write_tmp_file`, and background
delegate launches. A tool may implement an input-aware sequential opt-out for
concrete shared-state ordering; built-in examples include `update_todos`,
`record_plan`, `handoff`, foreground delegates, and `background_jobs`
cancellation. Such calls are barriers inside their stage. A matching
`PreToolUse` or `PostToolUse` hook also makes its target call a barrier.

Built-in `write` and `edit` report mutation paths. Harness normalizes those paths
lexically (`Abs` + `Clean`) and queues every overlapping same-stage mutation
behind the latest earlier call, including mixed write/edit and multi-file edits.
Unrelated members of the stage still overlap. All stages remain one assistant
turn and produce one user tool-result message; results, usage, sink events, and
transcript blocks appear in the model's original call order even when execution
completes differently.

A timeout or cancellation result normally satisfies a stage barrier. When a
reported mutation conflicts with a later-stage mutation, however, Harness also
waits for the earlier tool's execution goroutine to return before starting the
conflicting call. This prevents a context-ignoring timed-out mutation from
finishing on top of its successor. Errors otherwise do not skip or cancel later
stages. Likewise, a background launch result means that work was queued, not
that the background job finished; use `background_jobs wait` and leases when
later work must wait for completion.

Conflict detection is intentionally best-effort. It does not infer read-versus-
write dependencies, shell-hidden paths, git effects, symlink/hard-link aliases,
or remote MCP/LSP effects. `_stage` orders separate tool calls whose arguments
are already known. By contrast, `shell.steps` runs a tightly coupled serial
command list inside one `shell` call. If a later call's arguments depend on an
earlier result, wait for the next model turn rather than guessing them in a later
stage. Unlike generic stages, `shell.steps` also owns command-batch behavior such
as `stop_on_failure` and compact receipts. Use explicit background leases for
coordinated jobs.

`_stage` is Harness scheduling metadata, not a tool argument. Its top-level
property name is reserved for built-in, custom, MCP, and LSP tools and is removed
before execution, so custom and discovered tools must not assign it another
meaning.

## Truncation And Artifacts

Tool results are centrally capped at 64 KB or 1000 lines by default. Configure
this with `tool_result_max_bytes` / `tool_result_max_lines`, or
`HARNESS_TOOL_RESULT_MAX_BYTES` / `HARNESS_TOOL_RESULT_MAX_LINES`. Noisy file
inspection tools have smaller defaults unless a global cap is configured:
`read` uses a 500-line default window plus a 32 KB result cap. Override it with
`read_default_limit`, `read_result_max_bytes`, and `read_result_max_lines`, or
`HARNESS_READ_DEFAULT_LIMIT`, `HARNESS_READ_RESULT_MAX_BYTES`, and
`HARNESS_READ_RESULT_MAX_LINES`.

Truncated results include a marker in the model-visible text, a warning in the
UI, and the full output is archived under the session directory when available.
The model-visible tool result includes the absolute artifact path so the next
turn can inspect it with `read` or a targeted `shell` command. When live retention later
removes an old read-only result body, Harness leaves a typed receipt with the
tool, status, byte counts, bounded head, and recovery path; the exact original
is preserved in the same artifact store. If that artifact write fails, the
original stays in live context instead of being discarded.

Tool adapters can mark a successful result as semantically empty (currently an
empty MCP result, an empty background-job list, or a timed-out background wait).
The hint does not alter the live result or provider request. It only lets a later
compaction summary replace that result and its matching tool input with small
placeholders, avoiding summary budget spent on a call that produced no evidence.

Disabled optional CLI-backed default tools are reported on stderr at startup.
These warnings are suppressed by `-q` / `--quiet` or `--log-level error`.
