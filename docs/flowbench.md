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
3. Run three alternating pairs per model (`AB`, `BA`, `AB`) to reduce ordering
   and provider-cache bias.
4. Require correctness, actual feature adoption, the targeted interaction
   reduction, and paired token results before promotion.
5. Iterate on an oracle or routing issue only when the persisted transcript
   demonstrates that issue. Preserve failed evidence; do not edit scores.
6. Retain a useful primitive as optional when safe, but revert steering that
   causes a measured regression.

The implementations were:

- `search_context`: bounded ripgrep JSON parsing, grouped context windows,
  deterministic limits, truncation, and artifact-compatible citations.
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
- three repetitions per model.

Acceptance requires:

- at least 8/9 candidate correctness and no correctness loss versus baseline;
- at least 2/3 adoption for every model;
- no model worse than a 10% paired-median token regression;
- at least 50% reduction in the case's primary interaction metric;
- positive aggregate paired-median token savings.

Matrices stop early only when a remaining run cannot mathematically restore a
failed gate. Alibaba Token Plan and OpenAI Codex are reported as subscription
cost `N/A`; DeepSeek uses the provider-reported dollar amount.

Run a matrix from a clean checkout:

```sh
go run ./scripts/flowbench \
  -case background_wait \
  -baseline <before-revision> \
  -candidate <after-revision> \
  -results /tmp/harness-flowbench-results
```

Available cases are `search_context`, `command_steps`, `todo_coissue`,
`git_workspace_summary`, and `background_wait`. Use `-dry-run` to inspect
ordering, `-resume` for validated completed records, and
`-import-baseline-runs <runs.json>` to reuse a matching immutable baseline.

## Live results and disposition

| Item | Correctness / adoption | Primary result | Paired token result | Disposition |
|---|---|---|---|---|
| Contextual search preference | 8/8 completed candidates correct and adopted; stopped before OpenAI repetition 3 | Median search/read transitions 4→1.5 (62.5%) | DeepSeek −1.8%, Alibaba −5.2%; OpenAI regressed 85% and 143% in its two pairs | Forced preference rejected; `search_context` retained as an optional targeted tool |
| Ordered command steps | 9/9 correct, 8/9 adopted | Median command transitions 1→0 (100%) | Aggregate +8.5%; DeepSeek +8.1%, Alibaba +20.9%, OpenAI −10.4% | Formal gate missed by 0.4 points on OpenAI; retained as an optional structured verification primitive |
| Todo coissuing | 2/2 completed candidates correct, 0/2 adopted | Both candidates still had two todo-only turns | First pair regressed 33.8% | Early rejection; prompt/tool-description steering reverted |
| Git workspace summary | 8/8 completed candidates correct and adopted | Median git interactions 12→8 (33%); 50% became unreachable | DeepSeek −8.3%, Alibaba +4.6%, OpenAI −24.0% on completed pairs | Automatic promotion rejected; safe read-only workflow retained as optional |
| Background wait | Candidate 9/9 vs baseline 7/9; 8/9 adopted | Median polls 2→0 (100%) | Aggregate +5.7%; DeepSeek +32.2%, Alibaba +5.7%, OpenAI +1.3% | Accepted after routing descriptions discouraged `get`/`list` and short probe waits |

Positive percentages mean token savings; negative percentages mean regressions.
The search, todo, and git matrices used mathematical early stopping rather than
spending the remaining provider calls after their gates became unreachable.

The cited final/decisive matrices reported about $1.04 of DeepSeek usage. The
entire analysis and iteration campaign—including discarded smoke matrices and
reruns—contained 54 unique DeepSeek sessions totaling $2.473311 in
provider-reported cost. Alibaba Token Plan and OpenAI Codex incurred no
per-token charge because both configured providers are subscription based.

## Decision rule going forward

Only background wait cleared the complete promotion gate. The optional search,
steps, and git primitives remain available because they are bounded, useful,
and do not force the model down the rejected path. Todo coissuing was pure
steering and was removed after its measured regression.

Future changes should rerun the affected case against its immediate parent
revision. Do not substitute unpaired token medians for paired savings, treat a
tool being present as adoption, or waive correctness failures without
transcript evidence of an oracle/infrastructure defect.

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
