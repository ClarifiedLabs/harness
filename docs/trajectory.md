# Trajectory, stagnation, and evidence

This is the deep dive on harness's host-owned trajectory machinery: the
shadow trajectory projection and default-on stagnation nudge
(`internal/trajectory`) and the human-only session evidence catalog
(`internal/session/evidence.go`). [design.md](design.md) §10 carries the short
summary; [usage.md](usage.md) covers the user-facing flags and REPL commands.
Everything here is host-owned: none of it adds a model-facing tool.

## Runtime stagnation projection (`internal/trajectory`)

Harness derives bounded, host-owned stagnation control state from facts it
already observes. The state is not general request context, is not rendered
into a prompt, and does not add a model-facing tool. It retains only the active
evaluator handler and score direction, best ordered score and remaining-count
minimum, the previous bounded evaluator observation needed for conservative
comparisons, the current and maximum no-improvement streak, one-shot nudge
state, branch epoch, and aggregate current-schema counters. It does not retain
an evaluation history, candidate lifecycle, evidence archive, or mutation
paths. An evaluator score remains observational unless that result supplies
`score_direction`.

The policy's canonical transition stream is `raw.ndjson`, written through
`internal/sessionrec`:

- `evaluator_result` advances the active evaluator lane and aggregate result
  counters;
- `stagnation_nudge` marks a delivered one-shot intervention; and
- `branch` clears active lane state while retaining aggregate counters in the
  same physical session.

`tool_mutation` and `tool_diff` remain canonical session telemetry, but they do
not enter runtime policy state. `tool_mutation` records bounded possible paths
attributed to a successful mutation-reporting tool, independently of whether
diff display is enabled; these are tool-attributed paths, not proven filesystem
deltas. `tool_diff` confirms paths for which a non-empty rendered diff was
observed.

The runtime projection is not serialized into `state.json`. On load, Harness
streams current evaluator, nudge, and branch events from `raw.ndjson` without
retaining the full log in memory. A missing or malformed replay does not make a
healthy conversation unloadable, but Harness discards any cached projection and
disables nudge policy for that process rather than acting on unverifiable state.
A valid policy-free stream starts with a fresh lane for future evaluator events.

Projection schema v4 maintains one conservative active evaluator lane and a
no-improvement streak capped at 1,000,000. A new lane establishes a baseline.
Acceptance, a better explicitly ordered score, or fewer remaining requirements
resets the streak. A tied ordered score, a score worse than the best observed
in that lane, an unchanged unordered score, or an exact repeated rejected
candidate increments it. A new unscored candidate and a changed score without
a direction are indeterminate and do not increment the streak. Handler or
explicit direction changes reset the active lane; resets are counted because
alternating independent evaluators cannot safely share a scalar comparison.
These classifications do not change candidate selection or termination; the
policy below consumes only the active streak to augment an existing evaluator
repair turn.

## Stagnation nudge (default on)

The default-on `stagnation_nudge` policy uses that host-owned state only
during an already blocking Stop-hook continuation. At a no-improvement streak
of two it first persists a payload-free `stagnation_nudge` event, then appends
one generic strategy-reset instruction to the internal corrective turn. The
instruction does not expose host-only score direction or copy handler,
candidate, evidence, or hook output. A lane can receive at most one reset:
improvement does not re-arm it, while a handler/direction lane change or
branch reset does. Failed event persistence suppresses delivery so
model-visible control flow cannot run ahead of replayable state. The root agent
owns this policy; delegate children currently receive no hook runner and retain
only their ordinary raw tool telemetry. An explicit false flag, environment
value, or config value disables the policy.

Resume and an in-session implementation handoff reconstruct or retain the
root's active runtime state from the same physical event stream. `/tree`
navigation records a branch transition because the conversation changes while
the working tree does not. `/clear`, `/fork`, `/clone`, and
resume-to-a-distinct-session establish new ownership and start fresh; their
source session is unchanged. Delegate continuations retain conversation and
provider continuation state, but do not inherit stagnation policy state. Child
telemetry is never implicitly merged into the parent.

The projection is always host-only. It is never rendered into request context,
transcript history, tool results, or model-visible event text. The bounded
stagnation nudge above is the only policy that consumes it to affect model
control flow.

Analyzer schema v13 reconstructs evaluator and stagnation counters per physical
stream and independently derives mutation attribution from raw events. Its
private accumulator retains no more than 32 active and confirmed paths and
clears them on a branch; paths themselves are never emitted. Reports include
transitions, branch resets, evaluation outcomes, mutation-path observations,
diff confirmations, active and unconfirmed path counts, active and maximum
no-improvement streaks, stagnation classifications, unordered scores,
evaluator-lane resets, and delivered strategy resets. They never emit handler
names, score values, score directions, candidates, evidence references, or
filesystem paths. An unconfirmed path is an attribution-audit signal, not proof
of a false mutation: it is expected when diff display is disabled or a
successful write produces no rendered textual diff.

## Human-only evidence catalog (`internal/session`)

The session evidence catalog is a read-only projection built on demand from
one physical session's canonical `raw.ndjson` plus current filesystem
metadata. Artifact status describes that current metadata only; it does not
establish the historical byte identity of a file. The catalog never reads
artifact bodies, contacts the model, or has a separately persisted index, so
its event-derived records cannot drift from the event stream. Catalog version
1 assigns stable chronological `eval-NNNNNN`
IDs to all typed evaluator results and `tool-NNNNNN` IDs according to every
tool-result event, while returning records only for evaluator results, tool
errors, and truncated tool outputs with expected archives. Filtering never
renumbers IDs. Lists retain only the newest matching page in memory, return 20
records by default, and reject limits above 100.

The projection contains bounded metadata only: source/outcome, prompt and
turn, event time, evaluator score/direction/candidate/reference, or the
canonical tool summary/error excerpt. It never reads artifact bodies. New
truncated-tool events carry their deterministic session-relative
`artifact_ref`; legacy events derive the same path from prompt, turn, and
tool-call ID. Filesystem inspection uses `Lstat`, rejects symlink components
and non-regular targets, never probes an evaluator reference outside the
persisted startup working directory or session, and never permits a tool
artifact outside the session. Missing, unreadable, unsafe, external, and
unreferenced states remain visible. A regular artifact whose modification time
is newer than its canonical event is marked stale; this is an explicit
freshness warning, not a claim that unchanged timestamps prove immutable
contents.

`/evidence` and `/evidence show <id>` expose the projection in the interactive
REPL without a model request. `harness session evidence` exposes the same data
afterward in text or a versioned JSON page. Both inspect exactly one physical
session stream; callers select a child session explicitly rather than silently
merging root and delegate evidence. The catalog does not copy evaluator files,
make evidence model-visible, or mutate the worktree.
