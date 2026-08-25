# Benchmarks

## Paired live-model benchmark (`pairedbench`)

Pairedbench is a paired live-model benchmark that compares two harness revisions or configurations using alternating AB/BA arms on immutable worktrees with freshly built binaries. The paired protocol reduces ordering and provider-cache bias so prompt, tool, or configuration changes that merely look efficient are not promoted without measured evidence. Results go to a directory outside the repository. Historical campaign results were removed from this document; they remain recoverable from git history.

Pairedbench exists only under `scripts/`; there is no `harness pairedbench` subcommand.

### Protocol

- Freeze a real target checkout and a before/after harness revision; both
  arms share the same task fixture, correctness oracle, agent/model route,
  and limits.
- Run five alternating pairs per model (`AB`, `BA`, …) for promotion to
  reduce ordering and provider-cache bias. A one-pair smoke is an
  infrastructure preflight, not decisive efficiency evidence.
- Promotion requires correctness, actual feature adoption, the targeted
  interaction reduction, and paired token savings. Preserve failed evidence;
  never edit scores; iterate on an oracle or routing issue only when the
  persisted transcript demonstrates it.

### Quickstart

```sh
go run ./scripts/pairedbench -case background_wait \
  -baseline <before-revision> -candidate <after-revision> \
  -results /tmp/harness-pairedbench-results
```

### Flags

Source of truth: `scripts/pairedbench/main.go`.

| Flag | Default | Purpose |
|---|---|---|
| `-case` | — | benchmark case name (exactly one of `-case`/`-suite` required) |
| `-suite` | — | benchmark suite name (`tool_accuracy`) |
| `-profile` | `promotion` | suite profile: `smoke` or `promotion` |
| `-baseline` | — | baseline harness git revision (required) |
| `-candidate` | — | candidate harness git revision (required) |
| `-repo` | `.` | harness repository |
| `-results` | fresh temp dir | result directory outside the repository |
| `-models` | default matrix below | comma-separated model target ids |
| `-repetitions` | `5` | baseline/candidate pairs per model |
| `-reasoning` | `medium` | reasoning profile pinned for every run; recorded per run |
| `-parallel-models` | `false` | run one instance of each model concurrently per AB/BA round |
| `-dry-run` | `false` | print the resolved run matrix without building or calling models |
| `-resume` | `false` | reuse valid completed records; rerun interrupted unrecorded runs |
| `-import-baseline-runs` | — | reuse validated baseline records from another case's runs JSON |

Pairedbench waits for the whole arm before starting the next AB/BA arm, never
runs two instances of the same model at once, serializes Git worktree
lifecycle operations, and remains the sole writer of resumable result
records.

### Cases

Defined in `scripts/pairedbench/cases.go` (plus `stagnation.go` and
`recovery.go`).

Efficiency cases, each gated on a primary interaction metric:

- `search_context` — trace a cross-cutting mechanism with bounded `rg` searches followed by targeted reads (rg→read transitions).
- `command_steps` — fix a seeded bug, then run the ordered verification commands via `shell` `steps` (command→command transitions).
- `todo_coissue` — keep an `update_todos` checklist without dedicating turns to bookkeeping (avoidable todo-only turns).
- `git_workspace_summary` — review an uncommitted workspace with one `git {"workflow":"workspace_summary"}` call (git calls).
- `background_wait` — wait on a background test run with `background_jobs {"action":"wait"}` instead of polling (background polls).

Tool-accuracy cases against synthetic, evidence-backed fixtures:

- `edit_precision` — five exact `edit` replacements with byte-for-byte sentinel preservation.
- `edit_drift_recovery` — exact edit after the file drifts between the plan and edit prompts. A structured `edit_oldtext_not_found` miss leaves the effective gate only when a later edit succeeds within two turns and the exact file/workspace oracle plus required reread pass; ambiguous edits, timeouts, unresolved or late misses, and unrelated errors are never forgiven.
- `known_path_batching` — read all 18 enumerated fixture paths and run three directory-scoped argv-form `rg` searches; adoption requires a turn coissuing multiple direct reads and one coissuing independent lookups.
- `unknown_path_discovery` — discover paths with `rg --files` or `find` through `shell` before any read, then coissue reads of the first and last discovered paths.
- `read_scale_002`, `read_scale_008`, `read_scale_018`, `read_scale_036`, `read_scale_072` — standalone known-path reading ladder at increasing tool-call fan-out; adoption requires successful coissued reads covering every fixture path. Run directly with `-case`, not via the suite.

Host-feature cases:

- `stagnation_detection` — shadow-telemetry oracle for the trajectory projection's ordered scoring. The baseline must predate ordered-score support (for example `ec6dd98`). Twelve fresh processes resume one session and must reply tool-free while a hidden Stop evaluator replays a fixed score trace; each arm must match its projection oracle. Token/turn deltas are reported but not gated — the experiment is shadow-only.
- `stagnation_recovery` — same-revision arms whose isolated configs differ only in `stagnation_nudge:false` versus `true`. The candidate must persist exactly one payload-free strategy-reset event at no-improvement streak two, then make one exact fixture repair; the gate requires exact-recovery improvement, exactly one reset per candidate run, clean reset-driven recovery in at least 8/9 candidate runs, 2/3 adoption per model, and zero baseline resets.

### Suites

`-suite tool_accuracy` runs `edit_precision`, `edit_drift_recovery`,
`known_path_batching`, and `unknown_path_discovery` together. The `smoke`
profile uses Qwen 3.8 Max for one paired repetition (eight runs);
`promotion` uses the ten default model targets for five paired repetitions
(400 runs). Explicit `-models` and `-repetitions` override a profile.

### Default matrix and gates

Default models (source: `defaultModels` in `scripts/pairedbench/runner.go`):
`deepseek:deepseek-v4-pro`, `deepseek:deepseek-v4-flash`,
`alibaba-token-plan:qwen3.8-max`, `openai-codex:gpt-5.6-terra`,
`openrouter:moonshotai/kimi-k2.7-code`, `openrouter:moonshotai/kimi-k3`,
`openrouter:x-ai/grok-4.5`, `xiaomi:mimo-v2.5`, `openrouter:z-ai/glm-5.2`,
`openrouter:anthropic/claude-sonnet-5`.

Standard cases use medium reasoning, the independent agent, an empty explicit
config, no web/MCP/LSP/Serena augmentation, an immutable target revision, and
intentionally high limits: no prompt-token or prompt-cost caps, 200 turns, and
five repetitions per model. Runs are rejected
when recorded telemetry names a model target other than the requested one.
Efficiency-case acceptance requires:

- at least 8/9 candidate correctness and no correctness loss versus baseline;
- at least 2/3 adoption for every model;
- no model worse than a 10% paired-median token regression;
- at least 50% reduction in the case's primary interaction metric;
- positive aggregate paired-median token savings.

Each model summary lists every paired token-saving percentage and turn delta
in repetition order with sign counts, so a favorable median cannot conceal an
unstable distribution. Alibaba Token Plan and OpenAI Codex report as
subscription cost `N/A`; DeepSeek uses the provider-reported dollar amount.

### Records and resume

Run records hash prompts, fixtures, binaries, and raw events and version their scoring oracle (`runRecordVersion`, `oracleContractVersion` in `scripts/pairedbench/runner.go`). The current scoring contract is `pairedbench-oracle-2026-08-24-v35`. Restart-backed cases also bind their phase sequence and helper exposure into the prompt contract. `-resume` and `-import-baseline-runs` reject stale record, prompt, oracle, or event-stream versions/hashes rather than reusing an unverified prior score. Invalid infrastructure samples are retained as immutable evidence, left unscored, and a replacement is appended on `-resume`.

### Decision rule

- Rerun the affected case against its immediate parent revision; treat a one-pair smoke failure as a trigger for a fresh five-pair focused confirmation before tuning or reverting.
- Preserve every valid sample; predeclare any seven-pair extension for borderline lanes.
- Do not substitute unpaired token medians for paired savings, treat a tool being present as adoption, or waive correctness failures without transcript evidence of an oracle or infrastructure defect.

## Retention policy benchmark

`scripts/retentionbench` is a separate live-model matrix for the transcript retention experiment. It compares `age`, `disabled`, and `pressure` policies under stateful and stateless request shapes. Every run must read more than ten deterministic files in order, exactly one `read` call per model turn, then reproduce every marker — an objective correctness oracle that also creates old tool results large enough to exercise retention.

```sh
go build -o /tmp/harness-retention-candidate ./cmd/harness

# Inspect the resolved matrix, then run it:
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

The default scored matrix has 18 runs: three policies × two request shapes × three rotated repetitions. Use `-stateful true` for a smaller stateful-only smoke. MCP, LSP, and Serena are disabled in child processes. All fixtures require current `api_type` continuation-control metadata from the benchmark Harness and model proxy (stateful fixtures additionally require `continuation_stateful:true`); preflight rejects older proxy protocols instead of silently measuring the wrong request shape. A policy is recommended only when every run is correct, every run actually exercised that policy, and it has the lowest median post-turn-10 uncached input among the eligible policies.

## Reliability corpus comparison

Use `scripts/reliabilitybench` after a paired baseline/candidate run when the question is behavioral reliability rather than one paired benchmark case's oracle. Give it two immutable session directories or corpus snapshots produced by the same task/model matrix:

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

The command runs the same recursive, transcript-free analyzer as `harness session analyze` and reports candidate-minus-baseline deltas plus usage, storage, distribution, and cohort summaries; every metric retains an availability bit, so missing telemetry, a zero denominator, or an incomplete corpus is not converted into a success or failure. A fixture is one root session hierarchy, paired across corpora by root directory basename; basenames must be unique within each corpus. Outcome files are strict JSON maps keyed by that basename (capped at 8 MiB):

```json
{
  "fixture-a": {"task_completed": true, "expected_state_matches": true}
}
```

The verdict is deliberately conservative:

- The default minimum is three matched fixtures; below that, and for any `-before` cutoff report, the verdict is `insufficient_data`.
- Automatic promotion requires exactly equal baseline/candidate fixture sets with complete usage, explicit outcomes, inclusive pricing, and matching attempt-start provider/model/agent identity per pair.
- A candidate task or expected-state failure, or a candidate reconciliation failure, rejects promotion before efficiency is considered.
- Only then may the candidate promote, and only when median and nearest-rank p90 inclusive tokens and known USD cost do not regress; missing coverage remains `insufficient_data`, never a pass.

Keep the two raw analyzer JSON reports with the benchmark evidence when reproducibility matters: they contain the byte counts and SHA-256 values that identify exactly which records were analyzed. The semantic comparison complements the paired benchmark; it does not replace transcript-backed correctness scoring.
