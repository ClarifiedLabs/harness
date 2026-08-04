# Deterministic-flow analysis and live benchmark

This report records the July 23, 2026 analysis of historic harness sessions,
the implementation plan derived from it, and the paired live-model results. It
is intended to prevent prompt or tool changes that merely look efficient from
being promoted without measured evidence.

## Historic session census

The analyzed snapshot contained 286 `raw.ndjson` transcripts under
`~/.local/state/harness/sessions/`, including child sessions, with 20,980 tool
calls. A turn-level scan found these recurring patterns:

| Pattern | Occurrences | Sessions | Candidate deterministic flow |
|---|---:|---:|---|
| `rg` turn followed by a `read_file` turn | 688 | 160 | bounded contextual code search |
| `run_command` followed by another `run_command` | 434 | 65 | ordered verification steps with compact receipts |
| nonterminal `update_todos`-only turn | 60 | 32 | coissue progress with useful work |
| repeated git inspection | 2,318 git calls | 179 sessions with at least two | one workspace-summary workflow |
| `background_jobs` `get`/`list` polling | 95 polls | 20 | event-driven wait |

The most common git inspections were `status --short` (430 calls across `git`
and `git_readonly`), `diff --stat` (171), and `diff --check` (125). The corpus
also contained 752 verification-oriented command calls mentioning such commands
as `gofmt`, `go test`, `go vet`, `make test`, or equivalent test runners.

These counts identify opportunities, not proof of savings. A deterministic
flow can add schema tokens, prompt text, or extra model behavior, so each item
must pass a live before/after test.

## Applied implementation and test plan

For each item:

1. Freeze a real target checkout and a before/after harness revision.
2. Use the same task fixture and correctness oracle for both revisions.
3. Run five alternating pairs per model (`AB`, `BA`, `AB`, `BA`, `AB`) for
   promotion to reduce ordering and provider-cache bias. A one-pair smoke is an
   infrastructure preflight, not decisive efficiency evidence.
4. Require correctness, actual feature adoption, the targeted interaction
   reduction, and paired token results before promotion.
5. Iterate on an oracle or routing issue only when the persisted transcript
   demonstrates that issue. Preserve failed evidence; do not edit scores.
6. Retain a useful primitive as optional when safe, but revert steering that
   causes a measured regression.

The implementations were:

- `search` (the benchmark case retains its historical `search_context` name):
  bounded ripgrep JSON parsing, grouped context windows, deterministic limits,
  truncation, and artifact-compatible citations.
- `run_command.steps`: 1–16 serial commands, stop-on-failure behavior, compact
  `PASS` receipts, bounded failure output, and archived suppressed output.
- Todo coissuing guidance in the system, independent, plan, and tool prompts.
- `git {"workflow":"workspace_summary"}`: branch/HEAD, porcelain status,
  staged and unstaged stats, and both whitespace checks in one read-only call.
- `background_jobs {"action":"wait"}`: event-driven manager notification,
  targeted or any-job waiting, normal timeout results, and one-shot delivery.
- `scripts/flowbench`: immutable worktrees/binaries, fixture digests, session
  validation, AB/BA ordering, resumable/importable records, scoring, cost
  reports, and isolated Go caches.

## Live protocol

The default matrix uses:

- `deepseek:deepseek-v4-pro`
- `alibaba-token-plan:qwen3.8-max-preview`
- `openai-codex:gpt-5.6-terra`

Each run uses medium reasoning, the independent agent, no web/MCP/LSP/Serena
augmentation, an immutable target at
`8f76b0b0fb7751a8f7b067fa7f88e4df564f9560`, and these intentionally high
limits:

- unlimited prompt-token and prompt-cost caps (`0`);
- 200 turns;
- a 45-minute per-run deadline;
- five repetitions per model.

Acceptance requires:

- at least 8/9 candidate correctness and no correctness loss versus baseline;
- at least 2/3 adoption for every model;
- no model worse than a 10% paired-median token regression;
- at least 50% reduction in the case's primary interaction metric;
- positive aggregate paired-median token savings.

Every configured pair runs. Each model summary includes every paired
token-saving percentage and turn delta in repetition order, sign counts, the
token range, and the median
turn delta so a favorable median cannot conceal an unstable distribution.
Alibaba Token Plan and OpenAI Codex are reported as subscription cost `N/A`;
DeepSeek uses the provider-reported dollar amount.

Run a matrix from a clean checkout:

```sh
go run ./scripts/flowbench \
  -case background_wait \
  -baseline <before-revision> \
  -candidate <after-revision> \
  -results /tmp/harness-flowbench-results
```

Available cases are `search_context`, `command_steps`, `todo_coissue`,
`git_workspace_summary`, `background_wait`, `edit_precision`,
`edit_drift_recovery`, `known_path_batching`, and `unknown_path_discovery`. Use
`-dry-run` to inspect ordering, `-resume` for validated completed records, and
`-import-baseline-runs <runs.json>` to reuse a matching immutable baseline.

The tool-accuracy suite runs its four synthetic, exact-oracle cases together:

```sh
go run ./scripts/flowbench -suite tool_accuracy -profile smoke \
  -baseline <before-revision> -candidate <after-revision> \
  -results /tmp/harness-tool-accuracy

go run ./scripts/flowbench -suite tool_accuracy -profile promotion \
  -baseline <before-revision> -candidate <after-revision> \
  -results /tmp/harness-tool-accuracy
```

`smoke` uses Qwen 3.8 for one paired repetition (eight runs). `promotion` uses
the three default model targets for five paired repetitions (120 runs).
Explicit `-models` and `-repetitions` override a profile. `edit_precision`
checks five replacements and byte-for-byte sentinel preservation.
`edit_drift_recovery` mutates context only after the first interactive
`prompt_end`; raw tool errors remain recorded, while a structured
`edit_oldtext_not_found` miss is removed from the effective gate only when a
later edit succeeds within two turns and the exact file/workspace oracle plus
required reread pass. Misses are classified individually, so one timely recovery
can be exempted while another late or unresolved miss remains effective.
Ambiguous/invalid edits, timeouts, panics, unresolved misses, over-budget
recovery, and unrelated top-level or nested errors are never forgiven.

`known_path_batching` enumerates all 18 fixture paths and requires one successful
`inspect` call that reads each exact path once. It also requires one successful
three-query search batch with the exact two literal patterns, regex pattern, and
known-directory scopes, plus one successful full-output `run_command` batch with
the exact two non-empty argv steps. Assistant text and call counts alone do not
satisfy these oracles. The search queries scope the fixture directory instead of
listing 18 paths, which would exceed the per-query path limit.
`unknown_path_discovery` supplies only a root, requires a successful error-free `glob`, `list_dir`, or `search`
whose scope, pattern, and limits enumerate all fixture paths before any read,
then requires one successful `inspect` call that reads each of the 18 exact
discovered paths once. Both reject empty or partial discovery, duplicate or substituted
paths, operation-local read errors, serial direct reads, all-failed inspect calls,
missing marker evidence, and fixture changes.

Run records hash prompts, fixtures, binaries, and raw events and version their
scoring oracle. Resume and baseline import reject stale record, prompt, oracle,
or event-stream versions/hashes rather than reusing an unverified prior score.
They retain invalid infrastructure samples as immutable evidence, leave their
matrix keys incomplete, and append a replacement on `-resume` rather than
scoring or deleting the invalid sample. Child runs use an empty explicit Harness
config and are rejected when recorded telemetry names a model target other than the requested target,
preventing local agent model pins from silently contaminating the matrix. Live
provider receipts must keep direct Anthropic
`anthropic:claude-haiku-4-5-20251001` distinct from OpenRouter
`openrouter:anthropic/claude-haiku-4.5`; an alias or cross-route substitution does
not satisfy the corresponding lane.

## Live results and disposition

| Item | Correctness / adoption | Primary result | Paired token result | Disposition |
|---|---|---|---|---|
| Contextual search preference | 8/8 completed candidates correct and adopted; stopped before OpenAI repetition 3 | Median search/read transitions 4→1.5 (62.5%) | DeepSeek −1.8%, Alibaba −5.2%; OpenAI regressed 85% and 143% in its two pairs | Historical result; superseded by the typed batched `search` design |
| Ordered command steps | 9/9 correct, 8/9 adopted | Median command transitions 1→0 (100%) | Aggregate +8.5%; DeepSeek +8.1%, Alibaba +20.9%, OpenAI −10.4% | Formal gate missed by 0.4 points on OpenAI; retained as an optional structured verification primitive |
| Todo coissuing | 2/2 completed candidates correct, 0/2 adopted | Both candidates still had two todo-only turns | First pair regressed 33.8% | Early rejection; prompt/tool-description steering reverted |
| Git workspace summary | 8/8 completed candidates correct and adopted | Median git interactions 12→8 (33%); 50% became unreachable | DeepSeek −8.3%, Alibaba +4.6%, OpenAI −24.0% on completed pairs | Automatic promotion rejected; safe read-only workflow retained as optional |
| Background wait | Candidate 9/9 vs baseline 7/9; 8/9 adopted | Median polls 2→0 (100%) | Aggregate +5.7%; DeepSeek +32.2%, Alibaba +5.7%, OpenAI +1.3% | Accepted after routing descriptions discouraged `get`/`list` and short probe waits |
| Initial edit precision smoke (`013255c`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median effective errors 0→0, but OpenAI 0→1 | Aggregate +5.7%; Alibaba −47.7%, OpenAI −20.9% | Rejected: Alibaba turns increased 4→6 and OpenAI turns increased 4→5 in addition to the token/error regressions |
| Initial edit drift recovery smoke (`013255c`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median effective errors 0→0 | Aggregate +6.8% | Accepted at smoke |
| Initial known-path batching smoke (`013255c`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median tool errors 0→0 | Aggregate +8.4% | Historical v2 score; superseded by the exact v3 search/command oracle and not current promotion evidence |
| Initial unknown-path discovery smoke (`013255c`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median tool errors 0→0 | Aggregate +5.8% | Accepted at smoke |
| Revised edit-precision diagnostic (`9e5cabf`) | Baseline 6/6, candidate 6/6; 6/6 adopted | Median errors 0→0; Alibaba turns 6→6, OpenAI 4→4 | Aggregate +7.6%; Alibaba +6.7%, OpenAI +8.5% | Accepted over three alternating pairs on both previously regressed routes |
| Revised edit-precision smoke (`9e5cabf`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median errors 0→0 | Aggregate +7.2% | Accepted at smoke |
| Revised edit-drift smoke (`9e5cabf`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median errors 0→0; Sonnet turns 3→4 | Aggregate +7.1%; Sonnet −15.4% | Rejected on the Sonnet turn and token no-regression gates |
| Revised known-path smoke (`9e5cabf`) | Baseline 3/5, candidate 5/5; 5/5 adopted | Median errors 0→0; OpenAI turns 2→4 | Aggregate +6.8%; OpenAI −90.6% | Historical v2 score; superseded by v3 and still failed its v2 efficiency gates |
| Revised unknown-path smoke (`9e5cabf`) | Baseline 4/5, candidate 5/5; 5/5 adopted | Median errors 0→0 | Aggregate +5.1% | Accepted at smoke |
| Five-pair Sonnet edit-drift confirmation (`9e5cabf`) | Baseline 5/5, candidate 5/5; 5/5 adopted | Median errors 0→0; turns 4→4 | +5.6%; five positive deltas | Accepted |
| Seven-pair OpenAI known-path confirmation (`9e5cabf`) | Baseline 7/7, candidate 7/7; 7/7 adopted | Median errors 0→0; turns 4→4 | +5.4%; four positive and three negative deltas | Historical v2 score; superseded by v3 and requires a fresh run |
| Five-pair promotion edit precision (`9e5cabf`) | Baseline 25/25, candidate 25/25; 25/25 adopted | Median errors 0→0 | Aggregate −4.1%; OpenAI +16.5% | Rejected: aggregate paired-median tokens increased |
| Five-pair promotion edit drift (`9e5cabf`) | Baseline 25/25, candidate 25/25; 25/25 adopted | Median errors 0→0; aggregate turns unchanged | Aggregate −4.6%; every model's median regressed 4.2–8.0% | Rejected: aggregate paired-median tokens increased |
| Five-pair promotion known-path batching (`9e5cabf`) | Baseline 20/25, candidate 20/25; 25/25 adopted | Alibaba candidate 1/5 and turns 2→3; OpenAI 4/5 | Aggregate −3.7% | Historical v2 score; superseded by v3 and not used as current evidence |
| Five-pair promotion unknown-path discovery (`9e5cabf`) | Baseline 25/25, candidate 23/25; 23/25 adopted | Alibaba candidate/adoption 3/5 | Aggregate −4.3% | Rejected: correctness fell and Alibaba adoption was below 4/5 |

The 2026-08-03 tool-accuracy smoke compared baseline `446e00c` with product
candidate `013255c` over five configured provider routes, one alternating pair
per route and case (40 valid runs). Because edit precision failed its per-model
no-regression gates, the package did not advance to the three-pair promotion
matrix against `0594353`. An interrupted OpenRouter pair exposed and received a regression fix for resume
filtering; the final smoke contains no invalid records.

The follow-up edit-guidance candidate `9e5cabf` restored recently-read,
non-overlapping batch guidance while keeping the default catalog under its
existing schema budget. It cleared a 12-run, three-pair edit-precision diagnostic
on Alibaba and OpenAI, then cleared edit precision across all five routes in a
fresh 40-run smoke. That smoke exposed one-pair Sonnet edit-drift and OpenAI
known-path regressions. Longer confirmation showed the Sonnet result was not
stable: it passed five pairs decisively. OpenAI's preserved seven-pair known-path extension passed the
v2 oracle, but that result is superseded by v3 and requires a fresh run.

Those focused gates allowed the full five-route, five-pair promotion matrix
against `0594353` to run. Its edit cases were correct but rejected because
aggregate tokens regressed 4.1% and 4.6%, while unknown-path correctness fell
from 25/25 to 23/25 with Alibaba adoption at 3/5. Those independent failures
reject the package. The matrix's known-path scores used v2 and are no longer
promotion evidence under the stronger v3 oracle.
Candidate `9e5cabf` therefore fixes the focused edit behavior but does not qualify
the full change package for promotion.

The historical focused results are in
`/tmp/harness-flowbench-edit-drift-446e00c-9e5cabf-r5` and
`/tmp/harness-flowbench-known-path-446e00c-9e5cabf-r5`; the historical promotion
receipt is in `/tmp/harness-flowbench-0594353-9e5cabf-promotion-r5`.

Positive percentages mean token savings; negative percentages mean regressions.

The cited final/decisive matrices reported about $1.04 of DeepSeek usage. The
entire analysis and iteration campaign—including discarded smoke matrices and
reruns—contained 54 unique DeepSeek sessions totaling $2.473311 in
provider-reported cost. Alibaba Token Plan and OpenAI Codex incurred no
per-token charge because both configured providers are subscription based.

## Decision rule going forward

In the earlier workflow campaign, only background wait cleared the complete
promotion gate. The optional search, steps, and git primitives remain available
because they are bounded, useful, and do not force the model down the rejected
path. Todo coissuing was pure steering and was removed after its measured
regression. The typed tool-accuracy package stopped at smoke first because edit
precision failed; a focused guidance revision fixed that case. Longer focused
confirmation cleared the smoke's Sonnet anomaly, but the resulting five-pair
promotion matrix exposed discovery correctness failures and aggregate token
regressions in every case, so the package still did not advance. Historical
known-path scores require a fresh run under the v3 oracle.

Future changes should rerun the affected case against its immediate parent
revision. Treat a one-pair smoke failure as a trigger for a fresh five-pair
focused confirmation before tuning or reverting; preserve every valid sample
and predeclare any seven-pair extension for borderline lanes. Do not substitute
unpaired token medians for paired savings, treat a tool being present as
adoption, or waive correctness failures without transcript evidence of an
oracle/infrastructure defect.

## Retention policy benchmark

`scripts/retentionbench` is a separate live-model matrix for the transcript
retention experiment. It compares `age`, `disabled`, and `pressure` policies
under both stateful and stateless request shapes. Every run must read more than
ten deterministic files in order, exactly one `read_file` call per model turn,
then reproduce every marker. This provides an objective correctness oracle
while creating old tool results large enough to exercise retention.

Build the candidate Harness, inspect the six-run smoke matrix, then run it:

```sh
go build -o /tmp/harness-retention-candidate ./cmd/harness

go run ./scripts/retentionbench \
  -harness /tmp/harness-retention-candidate \
  -model openai-codex:gpt-5.6-terra \
  -repetitions 1 \
  -dry-run

go run ./scripts/retentionbench \
  -harness /tmp/harness-retention-candidate \
  -model openai-codex:gpt-5.6-terra \
  -repetitions 3 \
  -results /tmp/harness-retention-results
```

The default scored matrix has 18 runs: three policies, stateful and stateless
fixtures, and three rotated repetitions. Use `-stateful true` for a smaller
stateful-only smoke run. MCP, LSP, and Serena are disabled in child processes;
the configured model proxy and credentials are inherited. All fixtures require
current `api_type` continuation-control metadata from both the benchmark
Harness and model proxy, and stateful fixtures additionally require
`continuation_stateful:true`; preflight rejects older proxy protocols instead
of silently measuring the wrong request shape.

`runs.json` preserves per-run correctness, policy exercise, total and
post-turn-10 uncached/cache tokens, maximum processed input for any request,
the configured context window, retention epochs and resets, compactions,
termination, cost, and paths to the complete sessions and command output.
`summary.json` and `summary.md` report medians by policy and request shape,
including maximum request input as a share of the configured window. A policy
is recommended only when every run is correct, every run actually exercised
that policy, and it has the lowest median post-turn-10 uncached input among the
eligible policies. Provider-backed results remain evidence for a default
decision; they do not rewrite configuration automatically.

## Reliability corpus comparison

Use `scripts/reliabilitybench` after a paired baseline/candidate run when the
question is behavioral reliability rather than one flowbench case's token and
interaction oracle. Give it two immutable session directories or corpus
snapshots produced by the same task/model matrix:

```sh
go run ./scripts/reliabilitybench \
  -baseline /tmp/reliability-baseline-sessions \
  -candidate /tmp/reliability-candidate-sessions \
  -baseline-outcomes /tmp/baseline-outcomes.json \
  -candidate-outcomes /tmp/candidate-outcomes.json \
  -min-matched 3 \
  -format text

# Stable machine-readable comparison; the cutoff is inclusive.
go run ./scripts/reliabilitybench \
  -baseline /tmp/reliability-baseline-sessions \
  -candidate /tmp/reliability-candidate-sessions \
  -baseline-outcomes /tmp/baseline-outcomes.json \
  -candidate-outcomes /tmp/candidate-outcomes.json \
  -before 2026-08-01T12:00:00Z \
  -format json > /tmp/reliability-comparison.json
```

The command runs the same recursive, transcript-free analyzer as `harness
session analyze`, omits per-stream rows from the comparison output, and reports
candidate-minus-baseline deltas plus analyzer-v2 usage, storage, distribution,
and cohort summaries. Text and JSON include uncached input and cache-read tokens,
root/child usage, median/p90 known-complete cost, context-reset counts,
snapshot/legacy-delta bytes, disk bytes, and auditable build/runtime cohort
identity. Each metric retains an availability bit; missing legacy
telemetry, a zero denominator, an unobserved milestone, an incomplete execution
corpus, an incomplete storage component, or any cutoff report is not converted
into a success or failure. In particular, storage/reset metrics are unavailable
for cutoff reports rather than treating present-day files as prefix-time state. Lower is preferred for error/timeout/streak/violation and efficiency
metrics; higher is preferred for completion, workflow-supply, and
batching-compliance rates.

A fixture is one root hierarchy, paired by the root directory basename. Basenames
must be unique within each corpus; collisions are emitted as ambiguous unmatched
rows. Automatic promotion requires the baseline and candidate fixture sets to be
exactly equal, not merely a sufficiently large matched subset. The full root/descendant provider/model/agent multiset must be available,
stable for every recorded attempt, and match across a pair; missing, switched,
or differing identity is insufficient data. This uses immutable attempt-start
identity rather than the mutable final `state.json` snapshot. Analyzer parser-limit
counts (including `limit_exceeded_streams`) remain visible in corpus summaries.
Outcome files are strict JSON maps keyed by that basename and provide the
task/repository-state correctness identity. Both fields are required booleans,
and unknown fields or duplicate fixture/field keys are rejected. Outcome input is
capped at 8 MiB:

```json
{
  "fixture-a": {"task_completed": true, "expected_state_matches": true}
}
```

The v2 verdict is deliberately conservative. Its default minimum is three
matched fixtures. Below that it returns `insufficient_data`. A comparison made
with `-before` remains useful evidence but is always `insufficient_data` for
automatic promotion because state and correctness may exist after the prefix.
With enough full-session pairs, every pair must have complete physical usage, explicit baseline and candidate
task/repository-state outcomes, and complete inclusive pricing. Completion-source
coverage is reported when present but remains optional until structured child
completion metadata is universally emitted. A known baseline reconciliation
failure makes the comparison insufficient; a candidate reconciliation failure
rejects promotion. Candidate task or expected-state failure also rejects
promotion before efficiency is considered. Only then may the candidate promote,
and only when median and nearest-rank p90 inclusive tokens and known USD cost do
not regress; missing coverage remains `insufficient_data`, never a pass.

Keep the two raw analyzer JSON reports with the benchmark evidence when
reproducibility matters: they contain the complete-prefix byte counts and
SHA-256 values that identify exactly which records were analyzed. Match session
counts, models, task fixtures, ordering, and correctness oracles before treating
a delta as causal. The semantic comparison complements flowbench; it does not
replace transcript-backed correctness scoring.
