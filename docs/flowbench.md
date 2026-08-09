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
| `shell` followed by another `shell` | 434 | 65 | ordered verification steps with compact receipts |
| nonterminal `update_work`-only turn | 60 | 32 | coissue progress with useful work |
| repeated git inspection | 2,318 git calls | 179 sessions with at least two | one workspace-summary workflow |
| `background_jobs` `get`/`list` polling | 95 polls | 20 | event-driven wait |

The most common git inspections were `status --short` (430 calls across `git`
and `git_readonly`), `diff --stat` (171), and `diff --check` (125). The corpus
also contained 752 verification-oriented command calls mentioning such commands
as `gofmt`, `go test`, `go vet`, `make test`, or equivalent test runners.

These counts identify opportunities, not proof of savings. A deterministic
flow can add schema tokens, prompt text, or extra model behavior, so each item
must pass a live before/after test.

## Legacy WorkState orientation baseline

Analyzer v4 was also run against the complete `20260804T152107Z` hierarchy. Its
recorded build cohort identifies Harness v0.4.6. This is the historic session
that motivated structured WorkState; it is a reference target, not a paired
causal comparison, because the old event stream predates WorkState attribution.

| Reference metric | v0.4.6 hierarchy |
|---|---:|
| Physical sessions | 30 (1 root, 29 descendants) |
| Completed/observed prompts | 37 / 37 |
| Model turns | 1,389 |
| Tool calls | 1,522 |
| Tool/model errors | 46 / 9 |
| Prompt turn budgets exhausted | 24 / 37 covered prompts |
| Delegate closures caused by turn budget | 29 |
| Maximum inspection/no-progress streak | 16 tool turns |
| First observed successful mutation | 7 tool turns |
| First observed successful verification | 4 tool turns |
| Retention epochs / continuation resets | 254 / 182 |

WorkState candidate sessions should be compared on the same repository task,
agent/model route, limits, and outcome oracle. Analyzer v4 provides the primary
orientation measures: per-step time and tool turns to first mutation and first
verification, inspection operations, evidence batches, context resets,
identity switches, delegate results, and terminal outcome. Ordinary turn/tool
counts and token usage remain guardrails. A candidate is not better merely
because it resets context more often or records more WorkState activity; it
must preserve correctness while shortening orientation and avoiding new
turn-limit failures. Run live provider comparisons only as an explicit paired
benchmark because replaying the historic transcript cannot exercise the new
request-context behavior.

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
- `shell.steps`: 1–16 serial commands, stop-on-failure behavior, compact
  `PASS` receipts, bounded failure output, and archived suppressed output.
- Advisory `update_todos` coissuing guidance in the system and non-plan agent prompts.
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
- `deepseek:deepseek-v4-flash`
- `alibaba-token-plan:qwen3.8-max`
- `openai-codex:gpt-5.6-terra`
- `openrouter:moonshotai/kimi-k2.7-code`
- `openrouter:moonshotai/kimi-k3`
- `openrouter:x-ai/grok-4.5`
- `xiaomi:mimo-v2.5`
- `openrouter:z-ai/glm-5.2`
- `openrouter:anthropic/claude-sonnet-5`

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

The tool-accuracy suite runs its four synthetic, evidence-backed cases together:

```sh
go run ./scripts/flowbench -suite tool_accuracy -profile smoke \
  -baseline <before-revision> -candidate <after-revision> \
  -results /tmp/harness-tool-accuracy

go run ./scripts/flowbench -suite tool_accuracy -profile promotion \
  -baseline <before-revision> -candidate <after-revision> \
  -results /tmp/harness-tool-accuracy
```

`smoke` uses Qwen 3.8 for one paired repetition (eight runs). `promotion` uses
the ten default model targets for five paired repetitions (400 runs).
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

The orientation oracle uses the same minimal surface as the built-in agents.
`known_path_batching` enumerates all 18 fixture paths and requires successful
reads covering them, three successful directory-scoped argv-form `rg` searches
through `shell` (two escaped literals and one regex), the exact two-step command
result, requested marker evidence, and an unchanged fixture. Cosmetic command-
step names are ignored. Candidate adoption additionally requires either a
batched `read_file` or a turn coissuing multiple direct reads, plus at least one
turn that coissues independent repository lookups.
`unknown_path_discovery` supplies only a root, recognizes successful scoped
`rg --files` or `find` discovery through `shell` before any read, and requires
successful batched or coissued reads of the first and last discovered paths,
requested marker evidence, and an unchanged fixture. Metrics also record
successful read paths, direct read
operations, coissued read/lookup turns, search context before and after shared
batch shaping, duplicate and budget-omitted lines, low-yield search calls,
batch bytes before/after, and bounded-search calls.

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
| Legacy todo coissuing | 2/2 completed candidates correct, 0/2 adopted | Both candidates still had two todo-only turns | First pair regressed 33.8% | Early rejection; superseded by WorkState coissuing |
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
| WorkState DeepSeek V4 Flash confirmation (`1530a62`) | Baseline 3/3, candidate 2/3; candidate adopted 2/3 | Turn deltas +7, −2, −4 (median −2); primary interaction reduction 0 | −19.2%, +18.7%, +17.7% paired savings (median +17.7%) | Rejected: candidate correctness fell despite favorable median turns and tokens |
| Simplified `update_work` Flash diagnostic (`830fa50`) | Baseline 1/1, candidate 1/1; candidate adopted | Turns 22→13; `update_work` calls/errors 11/10→1/0 | +65.9% (739,287→252,061 tokens) | One-pair issue confirmation; formal case gate still rejects because its legacy work-only-turn metric was 0→0 |
| Simplified `update_work` ten-model matrix (`eed0f68`) | Baseline 30/30, candidate 24/30; adoption 13/30 | Correctness fell on V4 Pro, Qwen, Kimi K2.7, and all Xiaomi candidates | Aggregate paired-median −6.5%; only Flash and Kimi K3 cleared the per-model correctness, adoption, token, and turn gates | Rejected; retain the simpler progress contract but redesign the remaining plan/receipt flow before promotion |
| Flat-plan focused smoke (`d6bb9a8`) | Baseline 2/4, candidate 2/4; adoption 3/4 | Turns totaled 45→42; `update_work` errors fell 4→1 | Paired-median −16.7%; Qwen +20.8%, Mimo +14.1%, V4 Pro −95.2%, Kimi K2.7 −47.5% | Rejected at smoke; plan-validation loops disappeared, but efficiency did not clear the focused gate |

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

A later WorkState completion snapshot (`1530a62`, baseline `b010639`) added
`deepseek:deepseek-v4-flash` as a focused candidate route. Its three
`work_coissue` pairs used 16/23, 16/14, and 16/12 baseline/candidate turns and
492,419/586,866, 286,026/232,662, and 488,303/401,739 tokens. The first
candidate missed required output content, so the lane was rejected even though
the other two candidate runs were correct and the paired median favored the
candidate.

The same Flash route ran the four-case tool-accuracy smoke. `edit_precision`
passed both sides but regressed from 22,600 to 33,692 tokens and 4 to 6 turns;
`edit_drift_recovery` passed both sides and improved from 40,708 to 33,557
tokens; `known_path_batching` failed both sides while improving from 23,535 to
20,154 tokens; and `unknown_path_discovery` passed both sides with 18,749 to
18,215 tokens. The WorkState and tool-accuracy Flash runs reported $0.169569 in
total provider cost. Transcripts showed that model-authored work, revision,
step, and evidence/result IDs were the dominant new coordination failure, which
motivated moving that bookkeeping behind the model-facing `update_work`
contract.

A one-pair Flash diagnostic then compared that simplified contract (`830fa50`)
with `b010639`. Both runs passed the content oracle. The baseline used 22 turns,
739,287 tokens, and 11 `update_work` calls with 10 tool errors; the candidate
used 13 turns, 252,061 tokens, and one successful `update_work` call. This is
strong issue-level evidence, but not promotion evidence: it is one pair, and the
existing `work_coissue` primary metric only counts avoidable work-only turns,
which were zero on both sides. The pair reported $0.064874, bringing the Flash
campaign total to $0.234443.

The subsequent three-pair, ten-model matrix (`eed0f68`, baseline `b010639`)
ran 60 valid sessions. Baseline correctness was 30/30; candidate correctness
fell to 24/30, adoption was 13/30, and aggregate paired-median tokens regressed
6.5%. Recorded per-run cost fields summed to $8.580135; Qwen and Terra used
subscription routes. Per-model results were:

| Model | Correctness baseline→candidate | Adoption | Paired-median tokens | Median turn delta |
|---|---:|---:|---:|---:|
| DeepSeek V4 Pro | 3/3→2/3 | 0/3 | −117.2% | +10 |
| DeepSeek V4 Flash | 3/3→3/3 | 2/3 | +45.7% | −5 |
| Qwen 3.8 Max | 3/3→2/3 | 2/3 | +10.2% | 0 |
| GPT-5.6 Terra | 3/3→3/3 | 0/3 | +29.8% | 0 |
| Kimi K2.7 Code | 3/3→2/3 | 1/3 | −79.8% | +6 |
| Kimi K3 | 3/3→3/3 | 3/3 | +61.1% | −6 |
| Grok 4.5 | 3/3→3/3 | 0/3 | −27.0% | +3 |
| Mimo V2.5 | 3/3→0/3 | 1/3 | +15.2% | +1 |
| GLM 5.2 | 3/3→3/3 | 1/3 | +65.4% | −5 |
| Claude Sonnet 5 | 3/3→3/3 | 3/3 | −24.2% | +1 |

Positive token percentages are savings. The median hides material tails: GLM's
third candidate took 52 turns and regressed 173.4%, while Qwen's third candidate
took 50 turns, changed the prepared workspace, and omitted required output.
Across the matrix, `update_work` calls/errors improved substantially for Flash,
Terra, both Kimi routes, Sonnet, and Xiaomi, but V4 Pro, Qwen, and Grok still
looped on plan validation or work coordination. Several models also tried to
read the session-relative plan artifact path from the target checkout. This
separates the successful removal of opaque progress IDs from the still-complex
model-facing plan schema and artifact receipt. The canonical combined receipt
is `/tmp/harness-updatework-all-r3/work_coissue-summary.md`.

The next candidate therefore kept the simplified progress path while replacing
model-authored nodes, phases, IDs, plan state, and active selection with a flat
ordered step list. Harness generates the durable structure and withholds the
session-relative artifact path from model receipts and ordinary recovery
capsules.

A one-pair focused smoke (`d6bb9a8`, baseline `eed0f68`) then ran the four routes
that had lost correctness: V4 Pro, Qwen, Kimi K2.7 Code, and Mimo V2.5. Baseline
and candidate correctness were both 2/4. V4 Pro omitted `internal/config` on both
sides; Mimo omitted `internal/config` and `cmd/harness` on both sides. Qwen and
Kimi remained correct. The candidate eliminated V4 Pro's four consecutive
plan-validation errors and Mimo's malformed inspection error; its only
`update_work` error was a Qwen call that put progress fields beside `mode`
instead of under `progress`, then self-corrected. Total turns improved 45→42,
but candidate tokens increased from 794,382 to 1,076,989 and paired-median token
savings were −16.7%. The V4 Pro and Kimi token pairs regressed 95.2% and 47.5%
despite neither candidate hitting a plan-validation error. The focused receipt
is `/tmp/harness-flat-plan-focused-r1/work_coissue-summary.md`; reported run cost
fields sum to $0.170756, with Qwen on a subscription route. Stop before a full
matrix: the smoke supports the interface diagnosis but not an efficiency
promotion, so the next experiment should isolate why V4 Pro and Kimi expand
their investigation after receiving the smaller schema.

The search-surface experiment then compared a flat, host-bounded `search` and
top-level read-only coissuing against `d6bb9a8`. Its initial four-model
`work_coissue` trace (`4d3ee23`) kept baseline and candidate correctness at 4/4,
removed all nine built-in `inspect` calls, reduced nested operation errors from
7 to 0, and reduced shown tool-result bytes from 597,160 to 456,033 (23.6%).
Paired-median token savings were 5.2%, but the model lanes were mixed: V4 Pro
saved 72.0% and 12 turns, Terra saved 21.4% and two turns, Kimi K2.7 regressed
10.9% and three turns, and Qwen regressed 55.5% and six turns. Qwen's last two
turns were `update_work` validation failures rather than search failures; Kimi
continued searching without tool errors. This supports removing the nested
wrapper, but not a count-only search throttle that would also penalize Terra's
successful search-heavy run.

The final regex-only surface (`5a486c3`) removed the remaining
`fixed_strings` ambiguity. Under the v7 orientation oracle, one-pair focused
preflights were correct and adopted on both sides: Kimi's known-path lane used
10,238/9,851 baseline/candidate tokens (3.8% saving) in two turns each, and
Qwen's unknown-path lane used 16,082/15,055 tokens (6.4% saving) in three turns
each. Neither candidate had a tool error. These are focused validation samples,
not a multi-pair promotion matrix; a future search-saturation experiment should
first add yield/overlap telemetry and rerun the natural code case rather than
hard-capping calls from call count alone.

An exact-final eight-model parallel preflight (`eb8c32f`, baseline `d6bb9a8`)
then ran one pair each for `work_coissue`, `known_path_batching`, and
`unknown_path_discovery` through one proxy and eight isolated Harness workers
(48 sessions). Across all 24 pairs, correctness improved from 18 to 22,
effective tool errors fell from 30 to 6, built-in `inspect` calls fell from 10
to 0, turns fell from 135 to 132, and shown tool-result bytes fell from 649,141
to 627,376. Paired-median token saving was 5.6%; total tokens fell from
2,147,080 to 1,946,230 (9.4%). Recorded per-run cost fields summed to $3.065152;
Terra used a subscription route.

| Case | Correctness baseline→candidate | Paired-median tokens | Turns | Effective errors | Result bytes |
|---|---:|---:|---:|---:|---:|
| `known_path_batching` | 4/8→7/8 | +0.3% | 20→18 | 5→0 | 22,086→39,826 |
| `unknown_path_discovery` | 8/8→8/8 | +6.4% | 26→24 | 0→0 | 12,135→11,351 |
| `work_coissue` | 6/8→7/8 | −32.7% | 89→90 | 25→6 | 614,920→576,199 |

The natural-code case remained heterogeneous:

| Model | Correctness baseline→candidate | Token saving | Turn delta |
|---|---:|---:|---:|
| DeepSeek V4 Flash | 1→1 | +55.4% | −7 |
| DeepSeek V4 Pro | 0→1 | +10.8% | +1 |
| GPT-5.6 Terra | 1→1 | +59.7% | −3 |
| Claude Sonnet 5 | 1→1 | −22.6% | −1 |
| Kimi K3 | 1→1 | −85.2% | +2 |
| Grok 4.5 | 1→1 | −87.3% | +2 |
| GLM 5.2 | 1→1 | −46.9% | +2 |
| Mimo V2.5 | 0→0 | −42.9% | +5 |

The candidate natural runs accumulated 509–1,481 rendered search-context lines
per model across 7–16 flat calls. The known-path case exposed the same mechanism
more deterministically: three independent searches returned overlapping source
separately, increasing shown result bytes even though the old nested renderer
could deduplicate them. The next candidate should preserve the flat top-level
surface while enforcing a per-turn aggregate search budget and merging
overlapping source windows across coissued search results. A count-only search
throttle is still unsupported: Terra and V4 Flash were search-heavy winners,
while several regressors issued fewer searches than their baselines. The v7
unknown-path adoption metric also undercounts valid direct coissuing: Sonnet
issued two `read_file` calls together in one turn, but the metric recognizes
only one `read_file paths[]` call.

A shared-search-batch candidate (`cdce7da`, immediate-parent baseline
`eb8c32f`) then ran the same eight-model, three-case matrix under the v8 oracle:
48 valid sessions through one proxy and eight parallel isolated workers. All
records completed with exit zero, unique matrix keys, exact requested-model
attribution, and the expected baseline/candidate hashes. Candidate adoption was
22/24: 8/8 in both orientation cases and 6/8 in `work_coissue`.

| Case | Correctness baseline→candidate | Paired-median tokens | Turns | Effective errors | Result bytes |
|---|---:|---:|---:|---:|---:|
| `known_path_batching` | 6/8→6/8 | +5.0% | 19→19 | 1→2 | 39,235→23,897 |
| `unknown_path_discovery` | 7/8→8/8 | −0.3% | 24→24 | 0→0 | 10,495→11,223 |
| `work_coissue` | 8/8→6/8 | +5.6% | 92→92 | 12→7 | 608,075→544,383 |
| **Overall** | **21/24→20/24** | **−0.1%** | **135→135** | **13→9** | **657,805→579,503** |

Aggregate tokens fell 2,175,523→2,036,298 (6.4%), despite the approximately
flat median pair, because natural-case gains and regressions were strongly
model-dependent. Recorded cost fields summed to $3.189264; Terra used a
subscription route. Across 40 candidate batches containing 102 search calls,
7,559 candidate source lines became 5,769 shown lines (−23.7%): 772 duplicate
lines were suppressed and 1,018 unique lines hit the shared limit. Batch result
bytes fell 364,279→257,284 (−29.4%). The deterministic known-path workload
showed the mechanism most clearly: all eight candidates reduced 36 lines to 12
and 3,300 bytes to 1,374 per run, with no unique lines omitted.

The correctness audit did not establish a batching-caused failure. Both failed
candidate known-path runs issued an extra `shell`; their search batches
omitted no unique lines, and the baseline also had two failures on the same
command contract. The failed V4 Pro natural run had no budget-omitted search
lines. Mimo omitted 200 lines in later broad searches, but its initial
unomitted batch included the `cmd/harness` wiring that its otherwise accurate
final answer failed to name. The unknown-path control activated no shared
search batch yet still changed one correctness result and several token totals,
confirming material one-pair sampling variance.

This is a mechanism win and a promising context-efficiency candidate, but not a
formal promotion result: correctness fell by one overall and natural-model
token results remained heterogeneous. This triggered the predeclared fresh
five-pair focused confirmation below. The preflight receipt is in
`/tmp/harness-search-batch-eight-20260805-r1`.

The unchanged candidate then completed five fresh `work_coissue` pairs for all
eight models: 80/80 valid sessions with exit zero, exact model attribution,
five unique repetition keys, and the expected v8 oracle and revision hashes.
Aggregate correctness tied at 31/40. Candidate adoption was 27/40; avoidable
work-only turns changed 15→16, so the unrelated WorkState primary metric did not
improve. Paired-median token saving was 10.3%, total tokens fell
8,484,102→7,562,020 (10.9%), turns changed 420→419, effective errors fell
52→50, and shown result bytes fell 2,958,522→2,742,530 (7.3%). Recorded cost
fields fell $5.908371→$4.497822, largely through the Sonnet lane; Terra used a
subscription route.

| Model | Correctness baseline→candidate | Paired-median tokens | Total-token saving | Median turn delta | Effective errors |
|---|---:|---:|---:|---:|---:|
| DeepSeek V4 Flash | 5/5→5/5 | −1.7% | −6.7% | +2 | 6→8 |
| DeepSeek V4 Pro | 2/5→2/5 | −4.4% | −9.2% | 0 | 10→22 |
| GPT-5.6 Terra | 5/5→5/5 | −2.8% | −14.6% | +1 | 1→3 |
| Claude Sonnet 5 | 5/5→5/5 | +13.9% | +29.2% | −2 | 6→1 |
| Kimi K3 | 5/5→5/5 | −19.8% | −15.2% | +1 | 5→8 |
| Grok 4.5 | 5/5→5/5 | +23.5% | +14.3% | −1 | 2→2 |
| GLM 5.2 | 4/5→3/5 | +9.1% | +26.6% | 0 | 4→1 |
| Mimo V2.5 | 0/5→1/5 | +31.4% | +37.7% | −2 | 18→5 |

The mechanism remained deterministic: 133 candidate batches covering 326
search calls reduced 30,928 candidate lines to 24,594 shown lines (−20.5%),
suppressing 2,534 duplicates and omitting 3,800 unique lines. Batch bytes fell
1,551,178→1,158,050 (−25.3%). The model response was bifurcated, however: only
21/40 pairs saved tokens, four model lanes improved on paired-median tokens,
and four regressed. Candidate correctness failures still consisted only of
missing required path labels, but GLM lost one pass while Mimo gained one.

The exact aggregate-budget candidate therefore does not advance as the
provider-neutral default. The next causal ablation should keep cross-call
`(path, line)` deduplication and telemetry but remove the additional shared cap
on unique context, retaining each call's existing host-owned bounds. This
preserves the deterministic overlap reduction while testing whether unique-line
omission causes the extra turns seen in Flash, V4 Pro, Terra, and Kimi. The
focused receipt is in
`/tmp/harness-search-batch-work-confirm-20260805-r1`.

One baseline Mimo sample also exposed an independent runtime-bound gap: it
issued three searches with `path:"/"`, causing sorted ripgrep processes to scan
the filesystem root for more than seven minutes before completing. Output caps
did not provide a runtime cap. This was a limitation of the historical typed
search tool, which has since been removed; current built-in agents search through
`shell` and its command timeout contract.

The causal dedupe-only ablation then compared `01e272a` with its immediate
root-safe aggregate-cap parent `9f49dbe`. The filesystem-root rejection was
present in both revisions, so the only candidate difference was removing the
additional batch line/byte cap while retaining cross-call `(path, line)`
deduplication and telemetry. One fresh `work_coissue` pair ran for each of the
same eight models through one proxy and eight parallel isolated workers. All
16 records were valid v8 sessions with exit zero, unique matrix keys, exact
model attribution, and the expected revisions.

| Model | Correctness baseline→candidate | Paired tokens | Turn delta | Effective errors | Unique lines omitted |
|---|---:|---:|---:|---:|---:|
| DeepSeek V4 Flash | 1/1→1/1 | +7.1% | −2 | 2→0 | 100→0 |
| DeepSeek V4 Pro | 0/1→1/1 | +30.3% | −4 | 2→2 | 0→0 |
| GPT-5.6 Terra | 1/1→1/1 | +24.2% | −1 | 1→1 | 713→0 |
| Claude Sonnet 5 | 1/1→1/1 | +4.2% | 0 | 0→1 | 0→0 |
| Kimi K3 | 1/1→1/1 | −7.6% | −1 | 1→1 | 0→0 |
| Grok 4.5 | 1/1→1/1 | +9.2% | −1 | 0→0 | 305→0 |
| GLM 5.2 | 1/1→1/1 | +2.6% | 0 | 0→1 | 60→0 |
| Mimo V2.5 | 0/1→1/1 | +5.5% | +1 | 0→2 | 116→0 |

Correctness improved 6/8→8/8. Seven of eight pairs saved tokens; the sole
regression was Kimi at 7.6%, below the 10% focused-confirmation trigger. The
paired-model median saving was 6.3%, total tokens fell 1,991,857→1,700,312
(14.6%), turns fell 90→82, and shown result bytes fell 641,331→575,099
(10.3%). All candidates called `update_work`; five satisfied the stricter
coissuing adoption metric, unchanged from the root-safe baseline. Effective
errors changed 6→8, but two candidate errors were intentional filesystem-root
rejections described below.

The candidate's 24 batches covered 54 parseable search results within 80 total
search calls. Dedupe alone reduced 5,735 individually bounded context lines to
5,248 shown lines (−8.5%) and batch bytes 292,126→258,851 (−11.4%), suppressing
487 duplicates while omitting zero unique lines. This preserves the
deterministic overlap benefit without the aggregate-cap candidate's 1,294
omitted unique lines in the paired baseline sample. No correctness loss and no
model-level token regression above 10% means the decision rule does not require
a five-pair escalation. Advance dedupe-only shaping as the provider-neutral
default; retain individual search bounds as the only unique-context budget.

The root guard also received a direct live exercise. Candidate Mimo coissued
two first-turn searches with `path:"/"`; both returned `invalid_args` in under
one millisecond, after which the model retried with `path:"."` on the next turn
and passed the correctness oracle. The prior seven-minute root scan is therefore
closed by argument validation rather than a runtime timeout. The ablation
receipt is in `/tmp/harness-search-dedupe-only-eight-20260805-r1`.

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
promotion gate. Structured command steps and catalog-only raw search/git
wrappers remain available because they are useful and do not force built-in
agents down the rejected path. The legacy todo-coissuing experiment was pure steering and was removed after its measured
regression. The typed tool-accuracy package stopped at smoke first because edit
precision failed; a focused guidance revision fixed that case. Longer focused
confirmation cleared the smoke's Sonnet anomaly, but the resulting five-pair
promotion matrix exposed discovery correctness failures and aggregate token
regressions in every case, so the package still did not advance. Historical
known-path scores require a fresh run under the v7 orientation oracle.

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
