# Session persistence internals

This is the deep dive on how harness sessions are stored, recovered, branched,
and analyzed. [design.md](design.md) §11 carries the short architecture
summary; [usage.md](usage.md#sessions) covers the user-facing session
commands. The canonical types are not mirrored here: `Session` and
`UsageTotals` live in `internal/session/session.go`, and the tree node types
(`Entry`, `ContextDelta`, `ContextSplice`) live in `internal/session/tree.go`.

## State files

A session path is a directory:

- `tree.ndjson` — canonical append-only conversation data; the first record is
  a session header and later records are immutable typed entries (`segment`,
  `compaction`, `branch`, `context_reset`).
- `state.json` — mutable runtime state and the active leaf. The schema is
  versioned (`internal/session` `Version`, currently 7) and intentionally
  breaking: loading or replaying an older `state.json` returns a clear
  unsupported-version error; there are no aliases, migrations, or legacy
  linear-session fallback.
- `active-turn.json` — transient atomic recovery record for the current
  provider boundary.
- `raw.ndjson` — chronological replay data (the event stream below).
- `diagnostics.ndjson` — JSON slog diagnostics.
- `compactions/` — raw messages removed from active context.
- `artifacts/tool-results/` — full truncated tool output.

All session writes use temp-file then rename. The fields that drive resume and
branching: `ActiveLeaf` anchors the active provider context, which is
reconstructed by walking parents from it; `ParentSession`/`ParentEntryID` link
extracted sessions; `ResponseState`, `ProxySessionID`, and `CacheAffinityID`
carry provider continuation and cache identity; `Plan`, `Todos`, and `Goal`
persist coordination state; `Usage`/`UsageByModel` aggregate spend.

## Saving and recovery

- Segment entries are safe navigation boundaries. An assistant tool-use
  message and its immediately following tool-result message share one segment,
  so a branch cannot split the §4 transcript invariant.
- Before each provider request and before dispatching emitted tool calls,
  Harness atomically replaces `active-turn.json` with the complete resumable
  runtime state: transcript, latest plan, TODO list, usage, cache/proxy IDs,
  and safe Responses continuation anchor. An open tool-use is stored with
  synthetic `interrupted` results. Recovery therefore never automatically
  re-executes a tool whose process-local completion is unknown.
- After every validated closed conversational turn, root and child sinks first
  write the recovery record, then append/sync the tree and atomically replace
  `state.json`. Only after that canonical save succeeds is `active-turn.json`
  removed. Prompt-end/manual saves use the same consolidation order.
- Saves append and `fsync` new tree entries before atomically replacing
  `state.json` via temp-file plus rename. Context rewrites are recorded as
  complete message snapshots. Previously written splice-delta reset entries
  remain readable for compatibility; both forms can coexist without a schema
  migration. A malformed final tree record is treated as an interrupted
  append; malformed non-final records, missing parents, duplicate IDs, and
  invalid segments are hard errors.
- A context-reset snapshot or compaction with a materialized kept suffix can
  anchor replay; later segments, branches, compactions, and legacy delta
  resets are then applied in path order. Legacy delta resets verify
  parent/result message counts and provider-neutral transcript fingerprints
  before use, validate the materialized transcript, and preserve message
  ownership references outside changed splice runs.
- Sessions store the CLI-owned previous response/interaction ID, anchored
  message count, and transcript fingerprint in `state.json`. Resume restores
  it only when continuation is enabled for the exact current target,
  saved/current provider and model match, the index is in range, and the
  fingerprint matches the materialized active prefix. `active-turn.json`
  enforces the same invariant; invalid recovery state is discarded.
  `-responses-stateful=false` installs no anchor and sends complete history on
  every request.
- Gemini Interactions uses the same CLI-owned continuation contract.
  `interactions_stateful` controls the target's catalog capability; sessions
  retain signed thought and Google Search steps so a missing/rejected stored
  interaction can be replayed from complete history through any proxy replica.
- Image bytes are embedded in `tree.ndjson` as provider-neutral base64 blocks
  so resume is self-contained; `raw.ndjson` records only image metadata for
  replay.

## The replay event stream (`raw.ndjson`)

`internal/sessionrec` is the one canonical `raw.ndjson` recorder, shared by
the parent UI sink and delegate child sessions: both sinks call the same
recorder, which owns the pending-tool map, turn/prompt duration math,
unpriced-usage pricing hooks, and the shared Display-line formatters (tool
summaries, `[turn: …]` and `[prompt: …]` lines, model API issue lines) that
the live renderer also uses. Child sessions therefore store the same
tool-result summaries, turn/prompt usage lines, model-request display lines,
and `tool_diff` events (when diffs are enabled) as the parent by construction;
a parity test drives one scripted run through both sink paths and pins
identical `raw.ndjson` output.

- Every saved message and append-only replay event carries a timestamp. Replay
  events identify `prompt`, `turn`, and (for provider requests) `attempt`
  separately. `turn_attempt_start` also snapshots agent/provider/model
  execution identity so analysis can detect switches without trusting mutable
  final session metadata. `turn_attempt_start`, `turn_attempt_abandoned`, and
  `turn_attempt_usage` describe provider calls; `turn_complete` closes a
  conversational turn; `prompt_usage` closes the top-level prompt and carries
  its successful compaction count. A host `/continue` recovery receives a
  fresh prompt ID, so its notice, attempts, checkpoints, turns, and one
  terminal `prompt_usage` record are independent of the failed prompt. It
  deliberately emits no `EventUser` and contributes no new transcript block;
  saving synchronizes only the recovered assistant/tool suffix onto the
  existing active leaf. The eligibility and original request-only hook context
  are process-local and are not reconstructed by resume.
- `maintenance_usage` accounts for compaction, prewarming and branch-summary
  calls without creating turns. `model_request` records proxy/request
  lifecycle and every API issue with timing, parsed error, and correlation
  metadata; these events are replay/analysis data only and never become
  conversation-tree entries or model context. `checkpoint` records the
  boundary kind, save duration, and message count; stats use closed-turn
  records to expose save overhead and lag. `branch` records navigation
  source/target IDs in chronological replay.
- Tool-result events may carry diagnostics-only integer `result_metrics`
  supplied by the tool; those values are never copied into transcript blocks.
  Failed tool-result events additionally carry the structured `error_kind` and
  a bounded, rune-safe `error_excerpt` (2 lines / 240 runes) consumed by the
  stats Errors section and `harness session errors`; tool starts/results
  snapshot the active `model_target`, `provider`, `api_type`, and `model` for
  stable attribution. Legacy logs without those fields use the preceding
  `model_request`, then session metadata. Legacy failures without a kind are
  text-classified from the recorded display line.
- `skill_activation` records preserve the legacy event type but new events
  carry only the explicit/injected source and summary, not instruction
  contents; the `<skill>` body itself is ordinary user transcript content.
- Reliability telemetry schema v1 adds bounded structured fields only: prompt
  closure trigger and workflow status; per-tool-turn activity, first
  mutation/verification state, inspection/no-progress run, batching activity,
  and optional steer reason; hook outcome/duration/timeout/circuit state;
  bounded semantic evaluator result and evidence reference; host-derived
  trajectory mutations, seeds, and branch resets; context arithmetic and
  provider-count scope; and retention/reset decisions. A `telemetry_version`
  capability marker makes an observed zero distinguishable from a legacy
  unavailable signal. None of these fields contains prompt text, assistant
  text, tool arguments/results, hook payloads, or environment values.
- The recorder's `Mirror` hook (`sessionrec.Config.Mirror` →
  `session.EventAppender.Mirror`) receives every event after it has been
  durably written, post-coalescing; `-format json` run modes install it to
  mirror the identical event stream to stdout, so the live JSON stream and the
  replay log can never diverge.

## Resume, branching, and navigation

- Auto-save goes to `~/.local/state/harness/sessions/<timestamp>`; the path is
  printed at startup. `-session` chooses a directory; `-resume` loads
  `state.json` plus its active tree path, or applies a newer
  `active-turn.json` recovery record before continuing. Resume reports the
  recovered phase and prints a stderr recap of the last durable exchange
  before the first prompt, classified from the transcript alone (recovery
  marker, tail message role/phase/origin, and synthetic `interrupted` tool
  results) as clean, recovered, interrupted mid-reply, interrupted during tool
  execution, or an unanswered prompt. It also marks child metadata left
  `running` by the prior process as `abandoned`; such children are terminal
  and may be continued by child ID when their saved runtime contract still
  matches. Distinct `-resume <source>` and `-session <destination>` clone the
  active path with parent lineage and fresh usage. `/clear` rotates to a fresh
  directory.
- `/tree` renders a harness-native searchable/paged line picker over tree
  nodes. The renderer keeps unary paths in one graph lane, adds lanes only for
  sibling branches, labels checkpoint kinds, condenses repeated tools, and
  clips rows to the live terminal width. Selecting a human prompt targets its
  parent and returns its text/images as editable prompt prefill. Other entries
  are selected directly. Before moving, the user chooses no summary (default),
  a model-written summary, or a summary with custom focus; summary failure
  leaves the active leaf untouched.
- `/fork` uses the same compact graph projected onto human prompts, so hidden
  assistant and tool entries do not affect indentation, then extracts the
  selected pre-prompt path into a new session. `/clone` extracts the current
  path. Extracted sessions receive a new session ID,
  `ParentSession`/`ParentEntryID`, prompt number zero, fresh lifetime usage,
  and cleared Responses/proxy continuation anchors. Model, provider, agent,
  reasoning, latest plan, TODO list, hooks, and working directory stay
  global/current.
- Conversation navigation does not alter filesystem or Git state. Every branch
  adds a model-visible internal warning to inspect current files before
  assuming their state. Optional branch summaries describe only the divergent
  old suffix.
- Transcripts are provider-neutral; resuming under a different provider/model
  works. When flags disagree with the state, flags win with a warning.
  Tool-result messages may include local-only `parallel_tool_batches`
  metadata; provider adapters ignore it.

## Child sessions

Child-agent runs are stored below `children/<child-id>/` with their own
`state.json`, `raw.ndjson`, `meta.json`, and artifacts. Parent resume ignores
these child transcripts; they are forensic sidecars. `meta.json` is a
`ChildMeta` index — id, parent id, kind, agent, provider/model, status, task
preview, transcript/replay paths, error, usage, message count, mode,
resolved/requested agent, background resource/access lease, continuation
source/runtime fingerprint, requested/effective turn budgets, physical turns
used, and termination reason. The delegate Runner creates `running` metadata,
then owns one terminal transition to `completed`, `failed`, or `canceled`; a
later parent resume changes stale `running` metadata to `abandoned`.
State-save failure does not skip the terminal metadata attempt. Terminal
reasons are `model_completed`, `turn_limit`, `token_limit`, `cost_limit`,
`repeat_guard`, `error_guard`, `cancelled`, or `error`; `turn_limit` does not
imply semantic incompleteness, and `model_completed` does not prove
acceptance. `prompt_usage` remains the final normal child event and carries
the same reason plus its successful compaction count. Inline delegate lines
are process-local display events only: they are not appended to either parent
or child persistence. Child `raw.ndjson` retains the complete replay content
and remains the full-fidelity source for `session replay --follow`.

## Analysis and inspection commands

- `harness session analyze [--since D|--all] [--before RFC3339] [--format
  text|json] [--] [path]` recursively analyzes one session or a corpus. With
  no path, `--since` limits recent root discovery (default `24h`) and `--all`
  removes that lookback; neither applies to an explicit path. It treats every
  physical root or delegate log as a separate stream, recursively descends
  real `children/` directories without following symlinks, hashes the
  immutable complete-record prefix, and keeps
  missing/truncated/malformed/limit-exceeded sources explicit. Raw parsing is
  streaming and bounded by snapshot bytes, line size, and record count;
  retained analysis events discard transcript/body fields after hashing.
  Availability is denominator-aware: schema-supported zeroes are available,
  while legacy fields and absent streams are not inferred. A cutoff includes
  events at or before the requested instant and disables untimestamped
  child-metadata fallback. Aggregate execution completeness has a stable
  severity order (`incomplete` over `unavailable` over `unknown` over
  `complete`). Analyzer schema v12 groups every physical root plus descendants
  as one experimental hierarchy. Items retain their own
  provider/model/agent/build/runtime identity, derive immutable per-attempt
  identity availability/stability, and carry root ownership; cohort
  distributions use a deterministic key from the root build (with modified and
  clean builds distinct) and behavior-changing runtime profile.
- Inclusive accounting sums only physical `turn_attempt_usage` and
  `maintenance_usage` records, split across root/descendant and
  conversational/maintenance slices. Every normalized token class is retained.
  Calls without known pricing contribute tokens and increment unpriced
  coverage, so known cost remains visible with `cost_complete:false`.
  Persisted root usage is reconciliation-only and is consulted only for
  complete, non-cutoff hierarchies; it is never added to child usage.
  Hierarchy and cohort summaries carry sample counts with nearest-rank
  median/p90 token values and known-complete cost. Child completion
  aggregation counts only outcomes, validation states, and contract
  provenance. It never emits blocker text or child report prose, and schema
  v12 has no contract-specific completion-field analytics. Completion metadata
  is schema-local: current analysis rejects retired rich completion records,
  and sessions created before 0.5.12 should be analyzed with the Harness
  0.5.11 binary. Children without reports remain useful but contribute an
  explicit `unknown`/missing coverage failure. Parent rework remains
  unavailable unless a future host-owned signal can observe it. Semantic
  completion remains independent of lifecycle termination.
- Corpus/session discovery and storage analysis use chunked, capped
  entry/depth walks; metadata reads are size-bounded and symlinks are not
  followed. Storage reports files/directories/bytes for state/tree/raw logs,
  compactions, and tool-result artifacts, plus context-reset
  snapshot/legacy-delta counts and payload bytes. Raw storage bytes use the
  physical snapshotted file size, not only the event prefix selected by a
  cutoff. Missing, incomplete, malformed, symlinked, cutoff-incomplete, and
  limit-exceeded sources remain explicit. Transcript bodies, tool inputs, and
  artifact contents never enter output. Context aggregation preserves provider
  count scope: payload and effective maxima stay separate, incompatible scopes
  are not subtracted, negative public values are clamped, and compatible-scope
  arithmetic violations remain counted. `session stats` reuses analyzer-v3
  usage/storage vocabulary for a single root plus every physically nested
  delegate.
- `harness session replay <session-dir>` prints `raw.ndjson` as the familiar
  user-facing terminal view, filtering assistant/reasoning deltas from retry
  attempts that were explicitly discarded before a later successful attempt.
  Consecutive assistant stream fragments for one prompt/turn/attempt are
  coalesced into bounded 4 KiB/250ms records before they reach disk; a
  non-delta event flushes pending text first, so replay ordering and output
  are unchanged while per-token append/open/encode overhead is avoided. Replay
  renders Markdown at display time. On a color terminal, stored status Display
  lines are dimmed, `turn_attempt_start` renders the dim `[turn: N waiting]` /
  `[turn: N attempt M waiting]` markers the live non-status path prints, a dim
  horizontal rule separates each prompt block, and `tool_diff` events are
  colorized through the same highlighter as live diffs using the recorded file
  path for language detection; diffs are content and are never dimmed. Replay
  resolves `color_theme` at display time using the current flag > environment
  > config-file > dark-default precedence, through a focused loader that does
  not require model/provider configuration. Theme and ANSI choices are never
  recorded in `raw.ndjson`, so the same session may be replayed under either
  palette. `session replay --follow` keeps one stateful renderer with that
  palette (including across split appended tokens) and consumes only
  newline-complete append-only records with the ordinary 16 MiB record limit.
  It filters discarded attempts in the initial batch; a later live discard
  marker is printed rather than retracting visible output. Terminal child
  metadata triggers one final drain and a child `prompt_usage` record is a
  fallback completion marker. Root sessions have no terminal marker and follow
  until their context is canceled. Log rotation is not supported.
- `harness session timings <session-dir>` reads `raw.ndjson` timestamps and
  prints prompt totals, turn-attempt durations, tool durations, largest event
  gaps, context/payload estimates, and model API issue counts/provider
  time/scheduled retry wait. A prompt without `prompt_usage` is labeled
  `in progress`, and its elapsed time ends at the latest recorded event rather
  than being reported as zero.
- `harness session stats <session-dir>` reads the existing root and child
  `state.json` and `raw.ndjson` files, `compactions/*.meta.json` plus their
  input transcripts, and `children/*/meta.json`. It reports turns, direct tool
  and command activity, lifetime parallel batches, compactions, tree
  entries/branches/leaves/depth, navigation events, authoritative token/cost
  totals, calls per tool-bearing turn, standalone TODO/single-inspection
  turns, result truncation/byte/timing totals and per-tool volume, redacted
  aggregates of repeated normalized calls, command-step shape, skill
  reads/activations, active transcript composition, the latest request
  estimate, and a hierarchical delegate breakdown with the five highest
  direct-token children. The session header includes build identity and the
  non-secret runtime profile used for efficiency comparisons. A child without
  a completed `state.json` checkpoint is reconstructed from its metadata and
  replay for analysis and marked `checkpoint: unavailable`. Conversation
  statistics distinguish prompts, turns, model calls, retries, and maintenance
  calls. Sessions with closed-turn checkpoint events also report checkpoint
  count, average/maximum save time, and lag in completed turns and seconds.
  Root usage already includes delegate and maintenance spend and is never
  summed with child usage; each child usage total likewise includes its nested
  delegates. Direct tool activity is instead summed once from every replay
  log. The non-overlapping direct model-activity total likewise sums physical
  `turn_attempt_usage` and `maintenance_usage` records once from every root
  and child replay and prints a root/delegate split. Normalized call reporting
  hashes canonical JSON and emits counts only; it never prints tool arguments
  or skill paths. Compaction metadata uses the writer's canonical field shape;
  the stats reader accepts unknown additive fields while still rejecting
  malformed JSON and trailing values. `--format json` is versioned and
  additive: it emits the transcript-free tool/error subset with calls,
  results, failures, rates, and partial composite-operation metrics, plus
  build/runtime identity and analyzer-v3 usage/storage sections.
- `harness session errors [dir]` lists classified failures — failed tool
  results and failed model requests — for one session (root plus delegate
  children), or scans recent sessions under the default sessions root when no
  directory is given (`--since <dur>`, default `24h`; `--all` disables the
  window). `--tool`, `--kind`, `--model`, and `--agent` filter rows, and
  `--before <RFC3339>` applies an event-time cutoff. `--format json` emits
  scope, per-session rows, aggregate summary data, and per-physical-stream
  hashes/byte/event counts for the complete records analyzed. Each stream is
  snapshotted at its starting byte length so concurrent appends cannot change
  the scan. Multi-session scans report and skip unsupported/corrupt roots;
  explicitly named invalid roots remain errors. The summary additionally
  reports in-band `shell` execution failures, cancellations, and an effective
  failure total without converting them into tool-error rows. The stats report
  renders the same collector's aggregate as its Errors section, including
  result denominators/rates and repeat loops calculated from complete physical
  tool-result streams. A success or different failure ends a streak, and
  root/child streams never join.

## Transcript invariants

`ValidateTranscript` (`internal/llm/validate.go`) encodes the §4 invariant —
every assistant `tool_use` block has exactly one matching `tool_result` block
in the following user message, and no `tool_result` is orphaned — plus the
assistant-phase whitelist. Tests assert it after every operation that mutates
a transcript (cancel, compact, resume, max-turns stop). Repair rules: a cancel
mid-turn keeps streamed partial text as an assistant text-only message and
strips un-executed `tool_use` blocks (dropping the partial message if nothing
streamed); resuming with a dangling `tool_use` synthesizes an `interrupted`
error result.

## REPL history

Global REPL history persists across sessions, mirroring bash's familiar model:

- **Location:** `<stateDir>/harness/history` (one entry per line, plain text),
  or the path in `HARNESS_HISTFILE` / `-histfile` / config `histfile`.
- **`HARNESS_HISTFILESIZE` / `-histfilesize` / `histfilesize`:** max entries
  stored on disk (default `1000`, `0` disables persistence).
- **`HARNESS_HISTSIZE` / `-histsize` / `histsize`:** max entries loaded into
  memory at REPL start (default `1000`, `0` disables recall).
- **Behavior:** entries are appended on each non-empty, non-multiline input
  submission. On REPL start, the file is loaded, deduplicated (keeping the
  last occurrence), and rewritten if it exceeds `HISTFILESIZE` (self-healing).
  At most `HISTSIZE` recent entries are surfaced for up-arrow recall.
- **Concurrency:** uses `O_APPEND`, so multiple parallel REPLs sharing one
  file stay safe on POSIX systems.
- **Scope:** REPL sessions only; one-shot (`-p`) does not load or save
  history.
