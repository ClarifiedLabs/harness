# Compaction internals

This is the deep dive on context compaction and live transcript retention.
[design.md](design.md) §12 carries the short summary and the model-facing
checkpoint contract; [usage.md](usage.md#compaction) covers the user-facing
settings and `/compact`. The implementation is `internal/agent/compact.go`.

## Provider-native foreground path

When the selected proxy target advertises native compaction, ordinary
automatic compaction and an unfocused `/compact` send the complete current
provider-visible window plus `Request.System` to the provider's native
compaction operation. The request also carries the active tool schemas, server
tools, reasoning controls, cache key, and service tier so the provider sees
the same model contract as an ordinary turn. The input must already fit the
provider context window.

The canonical OpenAI API and ChatGPT Codex use compaction v2: Harness sends a
streamed `POST /responses` request whose final input item is
`{"type":"compaction_trigger"}` and enables the `remote_compaction_v2` feature.
It requires `response.completed` and exactly one `compaction` output item, then
installs recent retained user messages followed by that item. The opaque
provider item is never parsed, altered, or summarized. Other Responses
providers that explicitly enable native compaction retain the standalone v1
`POST /responses/compact` contract; Harness persists that operation's complete
returned item array.
Subsequent same-domain requests send that canonical window followed by newer
transcript messages. The semantic transcript remains intact, so a
model/provider switch simply omits the opaque checkpoint and replays normal
history. TODO, goal, hook, and background state remain request-only overlays
and are added fresh to every active model round; the system prompt remains on
`Request.System`.

Native compaction resets any stored-response continuation anchor and starts a
fresh stateless baseline. A native operation failure disables the path for that
replay domain for the current process and falls through to textual compaction.
If a later request rejects a persisted checkpoint's encrypted content, Harness
disables that checkpoint and retries once from the preserved semantic
transcript. Focused manual compaction, continuation handoff, all-history child
compaction, and speculative idle work keep using textual checkpoints because
they need Harness-owned summary semantics or detached application. Live
retention is skipped while native compaction is available, preserving the
semantic history needed for cross-provider fallback.

## Trigger policy

When `max(reported input tokens, estimated full-request footprint)` reaches
`compact_trigger_percent` (default **78%**) of the model's effective/learned
context window. Successful compaction targets `compact_target_percent`
(default **65%**) after fixed system/tool-schema overhead, providing
hysteresis instead of re-compacting after each small result. Reported input
counts cache-read/cache-write tokens because cached context still occupies the
window. The trigger runs after a prompt and proactively between turns, before
the next request when tool results balloon the estimate. Also manual
`/compact`. The estimate side is the last **measured** input tokens
(`lastInput`) plus a bytes/4 estimate of only the messages appended since that
measurement (the append boundary), so the trigger tracks real usage instead of
re-estimating the whole transcript; the raw bytes/4 estimate is reserved for
the degradation ladder. `compact_auto_enabled: false` gates the proactive,
exact-count, and post-prompt checks only; explicit compaction and the single
provider-overflow recovery retry remain enabled. The terminal warning follows
the configured trigger and is suppressed with automatic compaction.

## Live-transcript retention pass

Before each model request the agent runs a pure-local retention pass (no model
round-trip). The default `auto` policy and explicit `pressure` policy use the
larger of the full local estimate and the last provider measurement plus
appended-message delta. At 60% of the effective window an armed pressure epoch
trims every eligible result and aged image in one pass, then disarms until
context falls to 50% or below. Neither policy rewrites history below pressure;
compaction remains the safety net.

Any retention edit clears the CLI-owned `previous_response_id` exactly once,
while a below-pressure stateful request preserves it and sends only the
appended delta. Stateless providers get the same batched transcript bounding
without continuation reset semantics.

The `retention_policy` experiment control can force `age`, `pressure`, or
`disabled`. Auto and pressure share pressure-only epoch semantics, including
the optional absolute floor. Age mode trims eligible results older than
`retention_keep_turns` completed turns (default 4, decoupled from the
compaction suffix) and images two or more turns old before each request.
Disabling the local pass does not disable compaction or provider-overflow
recovery. Delegate continuation fingerprints include the selected policy.

**Eligibility.** A tool result is trimmable when it is read-only
(re-derivable on demand) and older than the retention boundary, or when its
exact bytes are durably archived and it is older than 2× that boundary. A
mutating result with no durable archive is never trimmed. A `write`/`edit`
tool input is replaced with a compact path receipt once a newer successful
mutation to the same path exists after the boundary; a failed later call never
supersedes. Receipts stay complete JSON objects and keep
`MutatedPaths`/`ReadPaths` decodable so compaction file indexing keeps
working. Every trim still requires a successful archive of the exact original,
or the block is left untouched.

Both retention policies preserve the transcript invariant and are idempotent.
Idempotency markers (`[older tool output trimmed` in result bodies, `content
omitted` in tool inputs) make repeated passes no-ops. `raw.ndjson` `retention`
events record policy/trigger, blocks and bytes trimmed, estimated context
before/after, whether Responses state was reset, and whether the next request
used stateful continuation or full context. `session stats` summarizes those
epochs and request shapes.

Before replacing an eligible result body, retention atomically archives the
exact original through the tool-result artifact path. The live transcript
keeps a typed receipt with tool name, success/error status, shown/original
byte counts, a bounded head, and the targeted recovery hint. The head is
`retention_result_head_bytes` (default 800 bytes, clamped to the 4096-byte
eligibility threshold); the compaction summary preview keeps its own 4096-byte
budget. If no session archiver is available, the receipt retains the generic
recovery guidance; if a configured archiver fails, retention leaves the
original body untouched.

The cache-stable prefix is computed against the same boundaries: it stops
before the first result a future epoch could trim, the first image it could
degrade, and the first superseded tool input it could replace, so a promised
prefix is never rewritten later.

## Summarization algorithm

**Turn boundary:** a completed turn begins at an assistant response and
includes its immediately following tool-result message when that response
requested tools. User prompts, steering messages, and synthetic context are
inputs to a turn, not boundaries that merge all round trips since one prompt.

**Mechanism:** keep the system prompt and select a raw whole-turn suffix
newest first until it reaches `compact_keep_tokens` or the
`compact_keep_turns` cap (default 8). When `compact_keep_tokens` is unset the
budget is window-adaptive: `clamp(window/5, 4000, 20000)`, so a small-window
model does not keep 20k verbatim while a 1M-window model still caps at 20k.
Always retain at least the newest completed turn. Under low-water pressure,
move the oldest retained round behind the boundary, regenerate the summary
over the enlarged removed set, and repeat until the target is met or only the
newest turn remains. Send everything older to the model with the summarization
instruction in `prompts/compaction-summary.txt`: preserve the task/goal and
acceptance criteria, still-constraining decisions, semantic state for
meaningful file changes, active workspace/blocker/verification state, key
facts, and unresolved intent; do not invent. The model does not enumerate
read-only inspected files or reproduce the separately injected TODO list.
Summary output is capped by `compact_summary_max_tokens` (default 2048).

Both summary prompts explicitly classify supplied history, tool output,
metadata, and the previous summary as untrusted data: embedded commands, role
changes, output-format requests, and claims of authority must be summarized as
evidence when relevant, never followed. The only permitted output is the
replacement summary.

A first compaction uses `prompts/compaction-summary.txt`. A repeated
compaction removes the structured prior checkpoint from ordinary summary
history, passes its exact prior summary once as data, and uses
`prompts/compaction-update.txt` to produce a complete replacement. Map/reduce
phases summarize only newly aged chunks; the prior summary appears only in the
final update call. Replace the old messages with one synthetic **user**
checkpoint headed `=== Compaction checkpoint ===`. It carries the active
prompt and any steering instructions verbatim, then the progress summary and
raw archive reference. This preserves current instructions explicitly without
pretending the summary was a conversational assistant turn. `/compact [focus]`
adds one-shot emphasis to all summary phases and records that focus only on
the resulting checkpoint/archive.

**Typed compacted-history state:** the advisory TODO list is persisted
independently and re-injected once after a transcript rewrite when unresolved.
Compaction summaries therefore omit that list and retain only unresolved
intent or blockers not represented by it.

**Deterministic compacted-history files:** correlate each validated assistant
tool-use message with its immediate result message. Successful supported
`read`, `write`, and `edit` calls contribute normalized, sorted cumulative
read/modified paths; modified wins over read. Commands, Git, MCP, malformed
inputs, failed calls, and unsupported/custom tools are skipped. The read list
is capped at the 200 most recent first-touches (oldest dropped first) and the
checkpoint JSON reports the omitted count as `read_files_omitted`, so a
thousand-path exploration does not re-send tens of KB in every checkpoint and
every later summary request. Modified paths are never capped. The JSON index
is the authoritative recognized file-activity inventory in the active
checkpoint and summary request. The model records semantic state only for
meaningful changes and unfinished mutation intent; it does not duplicate
read-only inspected paths.

Before summarization, a successful tool result carrying the local
`ResultUseless` hint and its matching call input are replaced in the summary
request copy with small semantic-empty placeholders; rich result images are
dropped. The active transcript and provider-visible live result remain
unchanged. Large remaining old tool results and tool inputs are reduced to
previews (`compact_tool_result_max_bytes`, default 4096; a **negative** value
disables this reduction entirely), and old images are replaced with text
placeholders. If older history is too large for one summary request, it is
summarized in chunks, then the chunk summaries are summarized.

**Hooks.** A configured `PreCompact` hook runs before summarization; if it
blocks, compaction is skipped with a `[compact skipped: <reason>]` notice. A
`PostCompact` hook runs after the transcript is replaced; its
`additionalContext` is added as request-only context for the next model
request, and a block surfaces a `[post-compact hook blocked after compaction:
<reason>]` notice. Both receive a `trigger` field (`auto` or `manual`); a
focused manual compaction also supplies `focus` to both hooks.

## Speculative idle compaction

The interactive REPL can opt in with `compact_idle_after_seconds` (zero
disables) and a lower `compact_idle_trigger_percent`. At the timer boundary
the owning goroutine captures a deep transcript copy plus a SHA-256
fingerprint covering the transcript and compaction-relevant runtime. A private
Agent performs the model work against that snapshot; it shares only the
provider and cannot call the live archiver or mutate session state. Submitted
input cancels the worker and marks any late result for discard, so prompt
execution never waits for speculative summarization. The owning goroutine
applies a completed candidate only when that fingerprint still matches, then
runs the live archive callback, installs the checkpoint, saves, and prewarms.
Configured `PreCompact` or `PostCompact` hooks make the idle path ineligible
because their external side effects cannot be rolled back.

Each started idle attempt emits an `idle_compaction` replay event with
outcome, wall time, threshold, message counts, and context before/after when
applied. Summary tokens returned by the worker are `maintenance_usage` under
purpose `idle_compaction`, including canceled or stale work. `session stats`
reports attempt outcomes, average/maximum wall time, and average applied
context reduction.

## Persistence, hardening, and fallback

Before replacing active history, raw removed messages are archived under
`compactions/`; the active summary includes the archive reference.

**Summary call hardening (`summarizeOne`).** The summarization request runs
with reasoning disabled (`Reasoning: llm.ReasoningConfig{}`) regardless of the
session's effort, so compaction never spends a thinking budget. It retries
transient mid-stream errors with the shared `retry.Next` backoff (up to
`streamRetries`) so a 429 at 78% does not abort compaction. If the summary
itself is truncated (`StopMaxTokens`) it doubles the token budget and retries
once, then accepts the result. The complete model-backed phase—including every
chunk, retry, and final reduction—shares one `compact_timeout_seconds`
deadline (default **300 seconds**). Hooks, local validation, and archive
persistence use the caller context rather than the summary deadline. The whole
operation remains covered by one transient `context: compacting` progress
phase.

**Image-aware estimation.** `estimateTokens`/`estimateRequest` weight each
`BlockImage` at a flat `imageTokenEstimate` (1600 tokens) rather than counting
its base64 bytes at bytes/4, which wildly overstated images. Correspondingly,
`truncateLargestBlock` ranks an image by that token weight, so a large text
result is truncated before an image.

The summary call's tokens and cost are added to session totals as maintenance,
never as a turn. Replay records a `maintenance_usage` event. The visible
notice reports the full request estimate before and after: `[compacted: 4
turns → checkpoint · ctx ~18.2k → ~12.7k]`.

**Tree/archive persistence:** archive the final enlarged removed set unchanged
and persist the exact summary, current focus, and cumulative file lists in
additive checkpoint/archive/tree metadata. `FirstKeptEntryID` remains an
atomic original-tree boundary when retained history can be linked directly. If
degradation rewrites retained content, or a valid boundary falls inside a
wholesale context-reset entry, mark the compaction entry and materialize the
retained suffix as new atomic segment entries so save/resume cannot resurrect
or reject it. Old entries with omitted fields reconstruct metadata from their
canonical tree summary without a schema-version bump.

**Degradation:** once only the last turn remains, hard-truncate the largest
tool result/input/image blocks in place with markers. When there is no older
turn to summarize but the transcript is still over budget, the same ladder
degrades the **oversized single turn** in place. Each degrade pass deep-copies
before mutating (so a post-degrade `ValidateTranscript` failure rolls back to
the live transcript) and skips a rewrite that would not actually shrink
(`[compact: transcript over budget but nothing left to shrink]`). Automatic
current-turn fallback notices are throttled within a prompt so repeated no-op
or tiny-shrink attempts do not flood the UI. Never wedge.

**Failure and deterministic fallback:** foreground automatic,
provider-overflow, manual, and continuation compactions replace a failed or
timed-out model summary with a deliberately sparse deterministic checkpoint.
It points the next model to unresolved TODO state when present, preserved
instructions, recognized file activity, recent verbatim turns, and the raw
archive; it never infers completed work. The rewrite occurs only when an
archive callback is available and exact removed messages are persisted
successfully. Missing/failed archiving keeps the full transcript and returns
the error. Explicit caller cancellation aborts without a fallback or rewrite.
Speculative idle compaction also never falls back: a timeout or provider error
discards that candidate so a later foreground path can decide. Checkpoint,
tree, and archive metadata record `summary_source` (`model` or
`deterministic`) and the bounded fallback reason (`timeout` or
`provider_error`).

Compacted transcripts must still satisfy the transcript invariant: kept turns
are whole turns, so no tool_use/tool_result pair is ever split.
