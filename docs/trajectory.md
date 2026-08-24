# Trajectory projection, stagnation nudge, lineage, and evidence

This is the deep dive on harness's host-owned trajectory machinery: the
shadow trajectory projection and default-on stagnation nudge
(`internal/trajectory`), the explicit candidate lineage archive
(`internal/lineage`), and the human-only session evidence catalog
(`internal/session/evidence.go`). [design.md](design.md) §10 carries the short
summary; [usage.md](usage.md) covers the user-facing flags and REPL commands.
Everything here is host-owned: none of it adds a model-facing tool.

## Trajectory projection (`internal/trajectory`)

Harness derives a bounded, host-owned trajectory from facts it already
observes. The projection itself is not general request context, is not
rendered into a prompt, and does not add a model-facing tool. Only the
default-on bounded stagnation policy described below consumes its active
no-improvement streak, and only during an already blocking evaluator repair.
The projection assigns its own evaluation and fallback candidate IDs; models
do not manage revisions, evidence IDs, steps, or a lifecycle graph. An
evaluator score remains observational unless that result supplies
`score_direction`. "Best accepted" still means the latest distinct accepted
candidate, not the numerically best score; ordered score comparison is
confined to host-owned stagnation telemetry.

The canonical transition stream is `raw.ndjson`, written through
`internal/sessionrec`:

- `evaluator_result` adds one bounded accepted/rejected evaluation;
- `tool_mutation` records bounded paths reported by a successful
  mutation-reporting tool, independently of whether diff display is enabled;
- `tool_diff` confirms paths for which a non-empty rendered diff was observed
  and backfills older logs;
- `branch` resets active candidate, evaluation, and path details while
  retaining cumulative counters in the same physical session; and
- `trajectory_seed` makes an explicitly inherited delegate continuation
  replayable in its fresh physical child session.

The current projection in `state.json` retains at most 16 evaluations and 32
paths. Candidate IDs are capped at 256 bytes, evidence references at 1024
bytes, and paths at 512 bytes. When an evaluator supplies an evidence
reference, detailed compiler, test, benchmark, profiler, or delegate evidence
stays in its artifact; the projection stores only the bounded reference.
Session saves use the existing temp-file-then-rename path. On load, Harness
streams the canonical raw events into the bounded projection without retaining
the full log in memory. A malformed or unavailable shadow replay does not make
a healthy session unloadable; the last atomically saved projection is the
fallback.

Projection schema v3 also maintains one conservative active evaluator lane and
a no-improvement streak capped at 1,000,000. A new lane establishes a
baseline. Acceptance, a better explicitly ordered score, or fewer remaining
requirements resets the streak. A tied ordered score, a score worse than the
best observed in that lane, an unchanged unordered score, or an exact repeated
rejected candidate increments it. A new unscored candidate and a changed score
without a direction are indeterminate and do not increment the streak. Handler
or explicit direction changes reset the active lane; resets are counted
because alternating independent evaluators cannot safely share a scalar
comparison. These classifications are persisted/replayed as projection state.
They do not change candidate selection or termination; the policy below
consumes only the active streak to augment an existing evaluator repair turn.

## Stagnation nudge (default on)

The default-on `stagnation_nudge` policy uses that host-owned state only
during an already blocking Stop-hook continuation. At a no-improvement streak
of two it first persists a payload-free `stagnation_nudge` event, then appends
one generic strategy-reset instruction to the internal corrective turn. The
instruction does not expose host-only score direction or copy handler,
candidate, evidence, or hook output. A lane can receive at most one reset:
improvement does not re-arm it, while a handler/direction lane change or
branch reset does. Failed event persistence suppresses delivery so
model-visible control flow cannot run ahead of replayable state. Root and
delegate sessions use the same policy and canonical recorder. An explicit
false flag, environment value, or config value disables the policy.

Resume, active-turn recovery, compaction, and an in-session implementation
handoff retain the projection. `/tree` navigation records a branch transition
because the conversation changes while the working tree does not. `/clear`,
`/fork`, `/clone`, and resume-to-a-distinct-session establish new trajectory
ownership and start fresh; their source session is unchanged. New delegates
also start fresh, while an explicit continuation inherits only its prior child
state through `trajectory_seed`. Child state is never implicitly merged into
the parent.

The projection is always host-only. It is never rendered into request context,
transcript history, tool results, or model-visible event text. The bounded
stagnation nudge above is the only policy that consumes it to affect model
control flow.

Analyzer schema v12 reconstructs the projection per physical stream and
reports only counts and sizes: encoded projection bytes, transitions, branch
resets, evaluation outcomes, missing candidate/evidence fields, dropped
bounded data, mutation-path observations, diff confirmations, unconfirmed
paths, active and maximum no-improvement streaks, stagnation classifications,
unordered scores, evaluator-lane resets, and delivered strategy resets. It
never emits handler names, score values, score directions, candidates, or
evidence. An unconfirmed path is an attribution-audit signal, not proof of a
false mutation: it is expected when diff display is disabled or a successful
write produces no rendered textual diff.

## Explicit candidate lineage (`internal/lineage`)

`-candidate-lineage` authorizes a specialized, root-only archive for an
evaluator-driven Git session. "Root-only" refers to the root agent: delegate
sessions never receive the archive observer. The option is intentionally an
invocation flag, not a config or environment setting, so every new or resumed
process opts in explicitly. Interactive and one-shot runs, primary checkouts,
attached or detached linked worktrees, repository subdirectories, implicit
session paths, and resume-clones are supported. The repository must have a
commit, and its session directory must be outside the worktree so capture
cannot recurse into its own archive.

On first open, the host snapshots the Git-visible prepared workspace as a base
tree and stores a binary `base.patch` against the initial `HEAD`. Capture uses
an alternate temporary index and never changes the real index or refs. Ignored
untracked files are not part of that tree. A semantic evaluator result is
eligible only when it is accepted and supplies a finite score, explicit
`maximize` or `minimize` direction, handler, candidate identifier, and
evidence reference. The first eligible result establishes one
handler/direction lane. Later results advance only on a strict ordered
improvement in that same lane; rejections, ties, regressions, incomplete
results, and other lanes remain in the canonical evaluator/trajectory log but
do not replace the best entry.

Each advance captures the current workspace in a temporary Git index seeded
from the current commit, writes a binary patch from the prior accepted tree,
and copies the referenced evidence file into the session archive. This keeps
one continuous accepted lineage across user or agent commits and checkouts.
Evidence must be a repository-root-relative regular worktree file whose
resolved path stays inside the worktree. An entry patch is capped at 16 MiB,
copied evidence at 1 MiB, and a lineage at 128 entries. The manifest, patches,
and evidence use temp-file-then-rename installation under
`<session>/lineage/`; reopening verifies artifact digests and reconstructs the
recorded tree chain before continuing. `/clear`, `/fork`, and `/clone` bind
the observer to a fresh archive for the new physical session, as does a
startup resume-clone. Reopening the same archive requires its original
physical worktree. A failed rebind disables observation so events can never
leak into the prior session.

The human-only `/lineage` command reads the manager directly and is absent
from model context and tool schemas. Listing is read-only. Export reconstructs
an entry through the verified patch chain and uses a newly created,
non-overwriting directory outside the source worktree and session. Restore is
the only lineage operation that changes worktree files. It rejects dirty state
without explicit `--force`, first writes a reverse binary patch under
`lineage/restore-backups/`, applies a checked tree-to-tree patch without
`--index`, and verifies the resulting Git-visible tree. Restore patches retain
the 16 MiB artifact bound and a session keeps at most 128. Neither export nor
restore changes the real index, creates commits or refs, or changes history.

Harness never automatically checks out, restores, commits, or promotes an
accepted tree. The archive remains a recoverable record and promotion remains
a separate user decision. The detailed manifest owns candidate IDs, scores,
tree hashes, evidence references, and artifact digests. The canonical
`lineage_advance` event contains only sequence/parent numbers and
patch/evidence byte counts, follows the corresponding `evaluator_result`, and
never enters model context or display text. Explicit export/restore notices
use ordinary human-visible session notices. Analyzer schema v12 exposes only
aggregate advance and artifact-size telemetry. A corrupt/incomplete archive,
unsafe evidence path, oversize artifact, or recorder failure is visible
immediately; one-shot mode still exits nonzero, while interactive mode keeps
the conversation available for recovery.

## Human-only evidence catalog (`internal/session`)

The session evidence catalog is a read-only projection built on demand from
one physical session's canonical `raw.ndjson` plus current filesystem
metadata. It has no separately persisted index and therefore cannot drift from
the event stream. Catalog version 1 assigns stable chronological `eval-NNNNNN`
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
make evidence model-visible, mutate the worktree, or broaden the candidate
lineage authorization boundary.
