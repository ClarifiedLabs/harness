# OpenTelemetry metrics

Harness has an opt-in, standard-library-only **OTLP/HTTP JSON metrics** exporter.
It observes execution independently of terminal rendering: one-shot, REPL,
inbound ACP roots, ordinary and reusable delegates, and background work share
the same typed observation path. It does not export traces, use a Go OTel SDK,
speak gRPC/protobuf, or spool metrics to CLI disk. Existing `trace_proxy` W3C
headers remain a separate way to correlate proxy diagnostics.

This is the canonical metric catalog and operational reference. See
[usage.md](usage.md#opentelemetry-metrics) for controls,
[design.md §3](design.md#3-architecture) for ownership, and
[session.md](session.md) for durable session records and offline analysis.
Metrics describe execution, not semantic task success: `model_completed` means
the model stopped, **not** that requirements were satisfied or verified.

## Enable and operate

For a local Collector accepting OTLP HTTP on loopback, merge this into your
Harness config:

```json
{
  "otel": {
    "enabled": true,
    "endpoint": "http://127.0.0.1:4318",
    "protocol": "http/json",
    "timeout_seconds": 5,
    "service_name": "harness",
    "hostname": "",
    "resource_attributes": {"deployment.environment": "development"}
  }
}
```

The exporter appends `/v1/metrics` unless the endpoint already ends with it.
Only absolute HTTP(S) URLs are accepted; URL user info and fragments are rejected.
Use headers for authentication, not URL credentials. The timeout is **1–30
seconds**, default 5, for an entire export, including waiting for serialization,
chunking, HTTP attempts, and retry delays—not a fresh budget per request.
Exports run every **30 seconds** and once more during orderly shutdown.
Collector errors are diagnostics, not prompt failures; invalid enabled exporter
configuration fails startup. Resource-envelope size validation also runs when
the exporter is constructed.

[`examples/harness/otel-config.json`](../examples/harness/otel-config.json) shows
an authenticated remote endpoint with an environment-expanded header. The
[parameter matrix](usage.md#harness-configuration-parameters) remains authoritative
for flags, environment variables, JSON fields, precedence, and defaults. There
are no interval, cardinality-budget, disk-spool, or trace-export flags.

### Delivery durability

For durable delivery beyond the CLI process, use a separately managed Collector
with persistent queueing and retries. Harness does not install or run one. Queue
limits and permanent backend rejections can still cause loss; this is not an
exactly-once guarantee.

CLI aggregation is in memory. A surviving process can re-export cumulative
values after a transient failure, but a crash can lose the last unexported
window (nominally up to 30 seconds with a healthy Collector, longer during
outages), in-flight final usage, and final session summaries. Durable queuing
helps **after the Collector accepts data**, not before it leaves the CLI.
There is no CLI disk spool. Uncooperative workers and expired cleanup deadlines
can also produce late observations after the final snapshot; those are not
guaranteed to survive.

## Accounting and lifecycle contract

### Physical requests, usage, and timing

`internal/llm` emits content-free physical attempt facts at their source.
`internal/modelproxy/server` prices complete usage snapshots using the resolved
provider/model and forwards the facts; `internal/modelproxy/client` forwards
them without treating the proxy HTTP connection as another upstream request.
`internal/execution.ModelCall` deduplicates by a per-call sequence (never an
exported label). Exclusive token/cost counters commit the final normalized
snapshot once per physical attempt, or the last available snapshot on client
loss. These are exact complete physical snapshots, not per-bucket high-water
marks or reconstructed logical prompt totals. Provisional snapshots are not
summed: later snapshots can reclassify output into reasoning or cache writes
into another TTL bucket.

Raw usage presence is preserved independently of a decoder's logical accounting.
`llm.StreamEvent.UsageReported=false` marks a synthetic placeholder or replay:
it neither establishes reported usage nor replaces an earlier real snapshot.
A non-nil `Usage` with `UsageReported` absent preserves the legacy reported-usage
contract; explicit `true` also accepts an authoritative all-zero snapshot.
Nil `Usage` is always absent. This metadata does not change transcripts.

- `scope=upstream` means real source observations. Attempts include actual
  connect/provider/proxy/agent retries and physical continuations. Optional
  operations such as compaction and prewarm retain their request purpose.
- An old proxy/provider without source facts yields **one**
  `scope=provider_call` fallback per invocation, with reported usage and caller
  wall time. At accepted native response boundaries, `ModelCall.Retain` seals
  independent response segments; each segment keeps its latest complete real
  snapshot, including bucket reclassification. Sealed segments and the remaining
  segment are combined into one fallback billing record—not invocation-wide
  high-water marks or extra fabricated request starts. API/transport are
  `unknown`; upstream request counts, retry counts, and TTFT cannot be
  reconstructed. Once any source facts arrive, aggregate stream usage is not
  also billed. Compare scopes separately rather than presenting fallback
  measurements as upstream statistics.
- Source duration and TTFT are optional. TTFT starts at the physical request
  boundary and ends at the first generated visible text, reasoning, or tool
  output—not headers or keepalives. Unknown starts (including some
  server-initiated continuations) do not manufacture timing. Lost finishes
  become `incomplete` without fabricated request duration. Unknown is absence
  of a histogram sample, not zero.
- `harness.retries.total` counts **actual additional starts** with `cause=retry`.
  A scheduled wait canceled before another start is not a retry request.
  Continuations have `cause=continuation`, not `retry`.
  `harness.model.retry.backoff` separately records planned delay and actual
  elapsed wait, including interrupted waits; it is not model request duration.
  Retry-start `reason`/`status` describe the new start's available facts, not
  necessarily the preceding failure (often unknown/none and `0`). Retry-wait
  observations carry the bounded reason for waiting; no request-chain
  correlation may be inferred by joining these labels.
- `outcome=cancelled` is distinct from `error`; timeouts have `reason=timeout`.
  An incomplete source can carry `reason=cancelled` after client loss, so exclude
  both explicit cancellations and that reason when constructing failure rates.
- `harness.model.usage.records{pricing=reported}` counts reported snapshots,
  including an explicit zero. Each reported record also increments exactly one
  of `pricing=known` or `pricing=unpriced`; `pricing=unreported` means no usage
  snapshot, not a free request. These four label values are **not four disjoint
  buckets**: `reported = known + unpriced`.
- `cost_known=true` means the full price is known, including authoritative zero.
  `cost_known=false` may still have a nonzero known partial USD amount, which is
  retained in `harness.cost.usd`; it is not silently repriced or discarded.
  `harness.cost.unpriced_calls` counts each reported-but-not-fully-priced request,
  not each prompt. Missing usage is tracked separately as `unreported`.

Ordinary `harness.tokens.*` and `harness.cost.usd` are **exclusive**: summing root
and delegate observations counts spend once, including maintenance, retries,
and known usage on failed/canceled attempts. Prompt/delegate completion does
not rebill inclusive usage. `harness.session.cost` and `harness.session.tokens`
are **inclusive root-session distributions**, not additional fleet spend. They
have no model/provider/agent label: a session may cross identities. They reflect
the root's session accounting, which need not include every physically billed
attempt omitted from logical results. Do not reconcile physical spend by adding
session or child summaries to it.

### Discarded work is a subset, not another bill

`harness.model.discard.*` records already-billed usage whose result was actually
suppressed. Source dispositions (`source_compatibility`, `source_proxy_retry`,
`source_unknown`) reference previously completed physical attempts. Logical
callers record disjoint discards for `stream_retry`, `request_rebuild`,
`summary_retry`, `summary_replaced`, `compaction_fallback`, `stale_idle`, or
`error`. `ModelCall.Discard` uses stored physical lineage with each slice's
original complete snapshot, price, and identity—not legacy aggregate/high-water
usage. It can include source-only failed usage that never escaped as a logical
stream usage event. Already-discarded source slices and accepted native response
prefixes marked by `ModelCall.Retain` are excluded from subsequent logical
discards; the same slice is not counted again at nested replacement boundaries.
In provider-call fallback, accepted response segments remain billed and only
the remaining rejected segment is discardable. A failed attempt alone does
**not** prove discard. These metrics are measured waste, not all possible
inefficiency, and are never added to billed totals.

### Tools, children, background work, and compaction

- `harness.work.*` separates scheduler queue time, actual run time, and delivery
  time when those boundaries are observed. Kinds are `tool`, `command`,
  `background`, `delegate`, `wait`, `parallel`, and `compaction`. Kinds overlap
  hierarchically: a delegate includes model/tool work; a parallel batch includes
  its tools. Do not sum their durations as non-overlapping wall time.
- A tool's logical result (`harness.tool.calls`, errors, truncation, result bytes)
  can precede its worker's actual finish after timeout/cancellation.
  `harness.work.duration{kind=tool}` ends at worker return, in **seconds**.
  Unknown tools, invalid inputs, and pre-dispatch rejection need not start a
  worker. Reusing an async cached result is not another physical execution.
- Commands count only commands/steps actually executed, not payload entries or
  launch receipts. Numeric process diagnostics are allowlisted and attached
  once to the logical foreground result or background job completion.
- A background launch receipt is not a completed job. Manager abandonment is a
  **logical** terminal outcome and may have no run duration. It does not invent
  a child finish; actual delegate/command work may finish later. Thus background
  in-flight can reach zero while underlying work remains in-flight.
- Delegate metrics use the child's resolved identity, not the parent's current
  model. Ordinary foreground/background delegates, reusable interactive prompt
  operations, and reusable runtime lifetimes have distinct modes; filter `mode`
  before interpreting counts. An external ACP runtime's lifecycle can be
  observed without access to its internal model usage—no upstream data is
  invented for it.
- Compaction runs and result dispositions are separate. Idle preparation can
  finish with `outcome=prepared` while changing **no live context**. Only actual
  application increments applied compactions and context reclamation. Stale or
  discarded candidates retain their billed work and record waste. Failed
  delivery may be retried without falsely discarding the candidate.

## Dimensions, cardinality, and privacy

The resource is stable for the process: `service.name`, `service.version`,
optional `host.name`, and a random **process-stable `service.instance.id`**.
The same process instance is used across exporters/roots; a new process gets a
new value. Keep it in backend series identity to avoid merging cumulative
streams from concurrent CLIs. `/clear` and model/agent switches do not relabel
old cumulative points. The session ID is internal deduplication state only;
there is **no `session_id` metric/resource attribute**.

In the catalog below, **I** means configured `provider`, `model`, `agent` when
present and `delegate="true"|"false"` (strings). Execution captures I at launch,
so late work is not reassigned after an identity switch. **M** adds `purpose`,
`scope`, `api_type`, `transport`, `cause`; **W** adds `kind`, `mode`, `tool`,
`trigger`; **C** adds `reason`, `policy`. Model API/transport, outcomes, reasons,
work modes/triggers, activity classes, and errors are bounded enums. Purpose is
`turn`, `compaction`, `prewarm`, `branch_summary`, or `unknown`. Status is an HTTP
code from 100–599 or `0` for unknown. Built-in tool labels use an allowlist;
MCP/LSP names collapse to `mcp`/`lsp`, unknown custom names to `other`.

The exporter admits up to **128 ordinary metric families**, each with at most
**64 ordinary series**, and **1,024 ordinary series globally**. Per-family
resident-size estimates are limited to 16 KiB (2 MiB across 128 families), with
space reserved for one overflow point per family. These estimates are not a
claim about exact Go heap usage. Large identities may reach the size budget
before the series-count budget. There is **no eviction or reset** of admitted
cumulative series during process lifetime.

New identities beyond a budget are merged into the family's reserved series
with the sole attribute **`otel.metric.overflow=true` (OTLP boolean)**. Sums and
histograms retain measurements but lose dimensions. Ordinary last-value gauges
keep the latest overflow sample; in-flight gauges remain additive, and
`parallel.largest_batch` retains a maximum. Family exhaustion, invalid metric
shape/numbers, or unsupported values are dropped and counted separately.
Self-health families bypass these ordinary budgets. Limits also include 16
attributes per point, 64-byte attribute keys, and 128 histogram boundaries.

Automatic metric labels contain no prompt/response/tool content, command
arguments, file paths, URLs, request IDs, session IDs, child/job IDs, or trace
IDs. Configured provider/agent names are truncated to 64 characters and model
names to 128; metric attribute values are trimmed and capped at 128 characters.
Service/version/hostname values are capped at 64 characters. Custom resource
attribute values are capped at 128 characters; **user-controlled keys/values are
not a secret scrubber**. Do not put sensitive data or unique IDs in them.
Reserved service/host/instance resource keys cannot be overridden by custom
attributes. Resource keys and JSON escaping still consume the wire budget.
Header values are redacted in config output. Exporter diagnostics omit endpoint,
headers, response bodies, transport error text, and Collector error messages;
partial responses report only counts and message presence. This metric privacy
contract is not a promise that independent debug logs/session files contain no
content; configure and retain those separately.

## Metric catalog

There are **81 ordinary metric families**, emitted when their observation path
runs, plus the 10 fixed self-health families below. Absence is not necessarily
zero or lack of work. **S** = cumulative monotonic sum; **H** = cumulative
explicit-bucket histogram; **G** = gauge. Units below are OTLP units. Sum/histogram
start times belong to the exporter, not the current session.

### Model requests and exclusive billing (24 families)

| OTLP name | Type / unit | Dimensions beyond I and meaning |
| --- | --- | --- |
| `harness.model.requests` | S / `{request}` | M; observed physical starts or provider-call fallback. |
| `harness.model.inflight` | G / `{operation}` | `scope`; active observed requests. |
| `harness.model.request.outcomes` | S / `{request}` | M + `outcome`, `reason`, `status`; one finish per request. |
| `harness.model.request.errors` | S / `{error}` | M + `outcome`, `reason`, `status`; `error` or `incomplete`. |
| `harness.model.request.cancellations` | S / `{cancellation}` | M + `outcome`, `reason`, `status`; explicit `cancelled` finishes. |
| `harness.model.request.duration` | H / `s` | M + `outcome`, `reason`, `status`; observed request duration only. |
| `harness.model.request.ttft` | H / `s` | M; known first-generation delay only. |
| `harness.model.output.throughput` | H / `{token}/s` | M; `(output + reasoning) / (duration - TTFT)` when both times and positive output are available. |
| `harness.model.usage.records` | S / `{record}` | M + `pricing`; `reported`, `known`, `unpriced`, `unreported` as described above. |
| `harness.retries.total` | S / `{retry}` | M + `layer`, `reason`, `status`; actual retry starts, not waits. Start reason/status need not describe the prior failure. |
| `harness.model.retry.backoff` | H / `s` | M + `layer`, `reason`, `status`, `outcome`, `measurement=planned\|actual`; retry-wait outcome, not request outcome. |
| `harness.tokens.input` | S / `{token}` | M + `cost_known`; uncached input. |
| `harness.tokens.output` | S / `{token}` | M + `cost_known`; output excluding separately classified reasoning. |
| `harness.tokens.cache_read` | S / `{token}` | M + `cost_known`; cached input read. |
| `harness.tokens.cache_write` | S / `{token}` | M + `cost_known`; default/short-TTL cache write bucket. |
| `harness.tokens.cache_write_1h` | S / `{token}` | M + `cost_known`; one-hour cache write bucket. |
| `harness.tokens.reasoning` | S / `{token}` | M + `cost_known`; separately classified reasoning. |
| `harness.tokens.prompt_input` | S / `{token}` | M + `cost_known`; input + cache_read + cache_write + cache_write_1h. |
| `harness.tokens.total` | S / `{token}` | M + `cost_known`; prompt_input + output + reasoning (do not add derived buckets again). |
| `harness.cost.usd` | S / `USD` | M + `cost_known`; full or known partial exclusive spend. |
| `harness.cost.unpriced_calls` | S / `{call}` | M; reported requests without full pricing. |
| `harness.model.discard.records` | S / `{record}` | `purpose`, `reason`; disjoint source/logical discard records, not necessarily requests. |
| `harness.model.discard.tokens` | S / `{token}` | `purpose`, `reason`, `bucket`; six disjoint token buckets, no derived prompt_input/total. |
| `harness.model.discard.cost` | S / `USD` | `purpose`, `reason`, `cost_known`; already-billed discarded spend. |

### Work, tools, commands, and children (25 families)

| OTLP name | Type / unit | Dimensions beyond I and meaning |
| --- | --- | --- |
| `harness.work.started` | S / `{operation}` | W; operations actually started. |
| `harness.work.finished` | S / `{operation}` | W + `outcome`; operation finishes, with the background abandonment exception above. |
| `harness.work.inflight` | G / `{operation}` | `kind`; tools also have `tool`; active lifecycle count. |
| `harness.work.queue.duration` | H / `s` | W; known scheduler admission-to-start wait. |
| `harness.work.duration` | H / `s` | W + `outcome`; known actual execution duration. |
| `harness.work.delivery.duration` | H / `s` | W + `outcome`; known delivery delay; compaction result points also have `fallback_reason`. |
| `harness.tool.calls` | S / `{call}` | W + `outcome`, `activity_class`; logical dispatch results. |
| `harness.tool.errors` | S / `{error}` | W + `outcome`, `activity_class`, `error_kind`; failed, cancelled, or timed-out results. |
| `harness.tool.truncations` | S / `{truncation}` | W + `outcome`, `activity_class`; truncated logical results. |
| `harness.tool.results.bytes` | H / `By` | W + `outcome`, `activity_class`, `measurement=shown\|original`; select one measurement before aggregating. |
| `harness.commands.total` | S / `{command}` | W + `outcome`; `kind=command`, `tool=argv\|shell`, `trigger=single\|step`; executed commands. |
| `harness.process.diagnostics` | H / `1` | Result/job dimensions + `measurement`; numeric process diagnostics (not labels derived from output). |
| `harness.background.jobs` | S / `{job}` | W + `outcome`; `started` and terminal outcomes are separate increments. |
| `harness.wait.total` | S / `{wait}` | W + `outcome`; explicit, prompt-join, or parent wait completions. |
| `harness.delegate.sessions` | S / `{session}` | W + `outcome`, `termination_reason`; child operation/runtime lifecycles, separated by `mode`. |
| `harness.delegate.turns` | H / `{turn}` | W + `outcome`, `termination_reason`; turns per completed lifecycle. |
| `harness.delegate.compactions` | H / `{compaction}` | W + `outcome`, `termination_reason`; inclusive per-child distribution, not exclusive applied count. |
| `harness.parallel.batches` | S / `{batch}` | W; batches started, `mode=sync\|async`. |
| `harness.parallel.calls` | S / `{call}` | W + `outcome`; actual batch size at finish. |
| `harness.parallel.batch_size` | H / `{call}` | W + `outcome`; batch-size distribution. |
| `harness.parallel.largest_batch` | G / `{call}` | W; process-lifetime maximum per admitted series. |
| `harness.parallel.results` | S / `{call}` | W + `outcome=completed\|failed\|cancelled`; component result counts. |
| `harness.compactions.runs` | S / `{run}` | W + `outcome`, `fallback_reason`; execution runs, including idle preparation/no-op/fallback. |
| `harness.compactions.dispositions` | S / `{result}` | W + `outcome`, `fallback_reason`; idle result application/staleness/discard/delivery error. |
| `harness.compactions.total` | S / `{compaction}` | W + `outcome`, `fallback_reason`; actual applications, never idle preparation alone. |

Process `measurement` values are `command_outcome_available`,
`command_succeeded`, `command_failed`, `command_cancelled`, `command_timed_out`,
`command_exit_code`, `command_wait_complete`, `command_steps_total`,
`command_steps_executed`, `command_steps_failed`, `command_steps_cancelled`,
`command_steps_timed_out`, and `command_steps_skipped`. Absence is unknown, not
success. Do not add diagnostics to executed-command or tool-result counts.

### Context and retention (16 families)

| OTLP name | Type / unit | Dimensions beyond I and meaning |
| --- | --- | --- |
| `harness.context.tokens` | H / `{token}` | C + `measurement=before\|after`; request estimate or retention/compaction boundary. |
| `harness.context.current.tokens` | G / `{token}` | Latest observed after-value for this identity, not a sum across active sessions. |
| `harness.context.limit` | G / `{token}` | Latest known positive context limit. |
| `harness.context.utilization` | H / `1` | C; after / limit when limit is known. |
| `harness.context.current.utilization` | G / `1` | Latest known after / limit. |
| `harness.context.removed.tokens` | S / `{token}` | C; nonnegative estimated tokens reclaimed by retention/actual compaction. |
| `harness.context.removed.bytes` | S / `By` | C; bytes reclaimed when supplied. |
| `harness.context.trimmed.blocks` | S / `{block}` | C; trimmed blocks when supplied. |
| `harness.context.retention.tokens` | H / `{token}` | C + `measurement=retained\|dropped`; reclamation observations. |
| `harness.context.retention.bytes` | H / `By` | C + `measurement=before\|after`; byte boundaries when supplied. |
| `harness.retention.epochs` | S / `{epoch}` | C + `decision_source=bytes\|provider_count\|response_usage_delta\|unknown`; retention events. |
| `harness.retention.transitions` | S / `{transition}` | Epoch dimensions + `previous_mode`, `next_mode`; full/stateful_suffix/stateless/unknown request-mode transitions. |
| `harness.retention.resets` | S / `{reset}` | Epoch dimensions + `kind=response_state\|measurement_anchor\|continuation_state`; performed resets. |
| `harness.context.messages` | G / `{message}` | Caller-visible request message count; system/request-only instructions are not history messages. |
| `harness.context.blocks` | G / `{block}` | Caller-visible content-block count, including nested tool-result content. |
| `harness.context.bytes` | G / `By` | `component=system\|user_text\|assistant_text\|tool_input\|tool_result\|tool_schema\|reasoning_text\|reasoning_opaque\|provider_state\|image`; supplied field lengths before dialect serialization. |

`execution.ComposeRequest` supplies numeric composition snapshots through
`ContextEvent.Composition` at core request boundaries: normal and
compatibility-rebuilt requests, native compaction, and maintenance including
prewarm and summary requests. The same path serves terminal/ACP roots and
foreground, background, and reusable delegate children. `Sink.ObserveContext`
uses the event's captured identity, not the live root's latest model. The older
`Sink.RecordContext` entry point remains only a compatibility adapter.

Composition measures **supplied request field lengths before dialect
serialization**, not encoded wire size, token occupancy, or implicit server-held
context. It does not serialize/copy payloads or decode images. Fixed byte
components mean:

- `system`: `Request.System` plus request-only instruction strings.
- `user_text`, `assistant_text`: text blocks of those roles; `tool_input`: tool
  argument payloads; `tool_result`: supplied result text and nested result text.
- `tool_schema`: supplied local/deferred tool names, descriptions, and parameter
  payloads, deferred group names/descriptions, and server-tool names, kinds, and
  parameter payloads, before the dialect chooses their representation.
- `reasoning_text`: thinking text and interaction-thought summaries;
  `reasoning_opaque`: supplied signatures, encrypted reasoning, and redacted data.
- `provider_state`: inline interaction-step, tool-search, and provider-compaction
  payloads—not routing/correlation IDs or remote retained context.
- `image`: supplied encoded image length, falling back to nonnegative recorded
  encoded-length metadata when image data is absent; not decoded pixel size.

Only numeric lengths/counts reach the observer; content, identifiers, schemas,
and image data are not retained. Context token/utilization values are separate
estimates/boundary diagnostics, not billable provider counts. Latest-value
gauges are not per-session gauges when roots/children share an identity. Every
composition sample writes all components, including zeros that replace old
values; absence of a new sample does not clear an earlier value.

### Prompts, sessions, turns, and skills (16 families)

| OTLP name | Type / unit | Dimensions beyond I and meaning |
| --- | --- | --- |
| `harness.prompt.total` | S / `{prompt}` | `termination_reason`, `closure_trigger`; completed prompt runs, root or child. |
| `harness.prompt.turns` | H / `{turn}` | Prompt dimensions; turns per prompt. |
| `harness.prompt.duration` | H / `s` | Prompt dimensions; wall time, not exclusive work time. |
| `harness.session.total` | S / `{session}` | **No I**; only `scope=root_session_inclusive`, `delegate=false`; deduplicated safe root-session snapshots. A timed-out owner settlement can omit the final snapshot. |
| `harness.session.cost` | H / `USD` | Same root-session-only dimensions; inclusive cost distribution. |
| `harness.session.tokens` | H / `{token}` | Same root-session-only dimensions; inclusive token distribution. |
| `harness.tools_per_turn` | H / `{tool}` | `activity_class`; positive tool-call counts per logical tool turn. |
| `harness.operations_per_turn` | H / `{operation}` | `activity_class`; positive operation counts per logical tool turn. |
| `harness.single_lookup_turns` | S / `{turn}` | One lookup and one tool call. |
| `harness.inspection_no_progress_streak` | H / `{turn}` | Positive observed inspection-only streak lengths. |
| `harness.guard.steers` | S / `{steer}` | `reason=repeat\|command_repeat\|batching\|phase_transition\|error_storm\|unknown`. |
| `harness.solo_todo_turns` | S / `{turn}` | Logical turns containing only `update_todos`. |
| `harness.single_inspect_turns` | S / `{turn}` | Logical turns containing only read/view_image/web_fetch. |
| `harness.skill.activations` | S / `{activation}` | `source`, `status=injected`; actual injections, not discovery, lookup, or repeated un-injected mentions. |
| `harness.skill.catalog_omitted` | S / `{skill}` | Catalog budget omissions, no skill names. |
| `harness.skill.catalog_truncated` | S / `{skill}` | Catalog budget truncations, no skill names. |

### Histogram interpretation

Shared duration bounds in seconds are `0.001, 0.005, 0.01, 0.05, 0.1, 0.25,
0.5, 1, 5, 30, 60, 300, 1800`; there is also an implicit +infinity bucket.
Shared token bounds are `0, 100, 1000, 10000, 50000, 100000, 500000, 1000000`,
count bounds `0, 1, 2, 3, 5, 10, 20, 50, 100`, and byte bounds
`0, 256, 1024, 4096, 16384, 65536, 262144, 1048576`. Specialized throughput,
utilization, session-cost, tool-turn, operation-turn, streak, and process
histograms use their own boundaries in `internal/otel/sink_*.go`.
Quantiles are bucket estimates, not exact order statistics. Never average
per-process percentiles; merge compatible bucket increments first.

## Export reliability and self-health

Exports are serialized with one overall context budget. Each wire request is
measured on its **actual encoded JSON**, limited to **64 KiB**, including the
resource and scope. Larger cumulative snapshots are split into sequential
batches at complete data-point boundaries, preserving start times and all
families. A single point that cannot fit is omitted and counted; resident
cumulative data is retained for future snapshots. The encoded resource/scope
envelope is validated at construction against **32 KiB**, leaving room for
points and self-health. This is not merely a point-count or estimated-size cap.

Each batch gets at most **three HTTP attempts**, for transient transport failures
or HTTP **429, 502, 503, 504** only. Backoff starts at 100 ms, doubles, adds bounded
jitter, and has a 5-second local cap. `Retry-After` delta-seconds or HTTP date is
a server minimum, even above that cap. If it cannot fit the remaining budget,
the exporter stops rather than retrying early. Cancellation/deadline expiration,
other status codes, and malformed/nontransient response failures are not
blindly retried. Retries after uncertain transport acceptance can duplicate a
cumulative snapshot; downstream handling must preserve cumulative semantics.

An empty successful response supports legacy Collectors. A JSON
`partialSuccess` is inspected, accepting numeric or decimal-string
`rejectedDataPoints`. A nonzero rejection count **or** nonempty `errorMessage`
is a partial-success error; message-only warnings do not invent rejected points.
Rejected points are counted, message content is suppressed, and the batch is
**not retried immediately** because some points may already have been accepted.
Malformed, negative-count, or oversized responses fail safely (response limit
8 KiB). Other chunks can still be sent within budget. A future periodic export
still contains the cumulative data; partial acceptance is not a queue ack that
removes local series.

`internal/otel.Exporter.Health()` returns the process-local cumulative `Health`
fields below. Its **10 fixed unlabeled OTLP families** are outside the ordinary
family/series limits and use the same process resource:

| OTLP name | Type / unit | `Health` field and exact meaning |
| --- | --- | --- |
| `harness.otel.export.attempts` | S / `{attempt}` | `Attempts`: HTTP attempts, including retries and partial acceptance. |
| `harness.otel.export.failures` | S / `{attempt}` | `Failures`: failed HTTP attempts, including partial acceptance/invalid successful responses; not every local export error. |
| `harness.otel.export.retries` | S / `{retry}` | `Retries`: extra HTTP attempts actually started. |
| `harness.otel.export.duration` | S / `s` | `Duration`: cumulative snapshot/chunk/network elapsed time, excluding time waiting for serialization. API field is `time.Duration`. |
| `harness.otel.export.payload_bytes` | S / `By` | `PayloadBytes`: attempted encoded bytes, including retries. |
| `harness.otel.export.rejected_data_points` | S / `{point}` | `Rejected`: Collector-reported rejected points (bounded signed-64-bit total). |
| `harness.otel.export.dropped_data_points` | S / `{point}` | `Dropped`: discarded measurements plus oversized wire-point omissions; omissions can recur on later exports. Not all network/crash loss. |
| `harness.otel.export.overflow_measurements` | S / `{measurement}` | `Overflow`: measurements merged into reserved overflow series; value retained, dimensions lost. |
| `harness.otel.export.active_series` | G / `{series}` | `ActiveSeries`: admitted ordinary + overflow points, excluding these self metrics. |
| `harness.otel.export.last_success` | G / `s` | `LastSuccess`: Unix seconds of the last fully successful export, 0 before any success. API field is `time.Time`. |

Health is included in the snapshot **before** that export's HTTP attempts finish;
the same payload cannot report its own final outcome. Periodic diagnostics warn
on errors and increased dropped/overflow counts.

### Orderly bounded shutdown

The process-owned `internal/execution.Group` tracks actual execution and complete
owners independently of returned timeout results or background-manager state.
Registry/background workers register before launch and release ownership after
their final observations. The group also covers complete UI/foreground owners,
model calls, prewarm, idle preparation, and transferred idle-result disposal.
Tracking lasts through the owner's last mutation/disposition, not just its last
model call. Owners stop admitting new user work before settlement; tracked
parents register detached children before returning. `Group` itself neither
cancels work nor rejects child registration.

- **Terminal CLI:** reusable-session cleanup, bounded background cleanup, and
  group settlement share the existing **five-second cleanup budget**. Background
  waiting consumes at most one second of that remaining budget, not an extra
  allowance. After successful settlement, final root-session accounting can be
  read safely. If settlement expires, Harness logs a warning and **skips the
  mutable session aggregate** rather than race a live owner. Already-recorded
  completed facts still receive the final export attempt.
- **ACP:** root construction uses a separate serialization gate, not the factory
  lifecycle mutex. Closing can stop admission and cancel pending construction
  without waiting indefinitely for config loading or a constructor holding that
  gate. This bounded lifecycle behavior also applies when OTel is disabled.
  After protocol close, the factory shares a **one-second settle budget** across
  actual root closure, pending constructors (including late-root disposal), and
  the execution group. It warns on expiry and proceeds to final export when
  enabled; a protocol timeout is not proof the root or worker actually finished.

Then `Exporter.Shutdown` cancels/joins its single periodic worker and attempts a
final export within **two seconds total** (or a shorter caller/export budget),
separate from the owner-settlement budget. It is idempotent; later ordinary
exports return `ErrExporterShutdown`. Final failures and cumulative
dropped/overflow/rejected counts are logged as operator diagnostics, never
protocol output or a change to the prompt result.

This improves orderly-exit coverage, not guaranteed survival of uncooperative
work. An execution-group timeout does not synthesize worker/child finishes or
known durations: unresolved work can remain in-flight in the final snapshot.
When a `ModelCall` actually finalizes without a source finish, it records that
source as `incomplete` using the last available usage and no fabricated request
duration; that is distinct from an observed physical finish. Work completing
beyond the bounded final snapshot can still lose its late data. Check final logs
as well as backend health: the last failed export cannot report itself to an
unreachable Collector.

## Dashboard and query recipes

The following is **provider-neutral algebra over OTLP names**, not copy/paste
PromQL. Let `D(metric{filters})` be the sum of reset-aware cumulative increases
over a window, preserving process-instance/start-time identity before combining
series. For a histogram, `D(H.count)`, `D(H.sum)`, and `D(H.buckets)` refer to its
count, sum, and bucket increases. Guard zero/missing denominators; show unknown
rather than 0% when no evidence exists. Choose aligned windows and scopes.
Prometheus translation can replace dots with underscores, append `_total` to
counters and unit suffixes such as `_seconds`, and expose histograms as
`_bucket`/`_sum`/`_count` depending on the Collector/backend translation strategy.
Inspect actual backend names/labels before writing PromQL; OTLP bucket counts
must also be converted to cumulative `le` buckets for Prometheus quantiles.

1. **Token-weighted cache read ratio:**
   `D(harness.tokens.cache_read) / D(harness.tokens.prompt_input)`.
   Include cache writes in the denominator; aggregate tokens before dividing,
   never average request ratios. Group by provider/model/purpose as needed.
2. **p50/p95 latency and TTFT:** merge bucket increases for
   `harness.model.request.duration{scope=upstream}` and separately
   `harness.model.request.ttft{scope=upstream}`, then estimate quantiles at .50
   and .95. Keep purpose/model/outcome panels separate where useful. Show sample
   counts alongside `D(harness.model.request.outcomes{scope=upstream})` to expose
   missing timing. A provider-call latency panel is useful but is not upstream
   latency. TTFT applies only where generation timing is observable.
3. **Request failure rate excluding cancellations:** for `scope=upstream`, divide
   outcome increments with `outcome in {error,incomplete}` and
   `reason != cancelled` by all outcome increments excluding
   `outcome=cancelled` and `reason=cancelled`. Show cancellations and incompletes
   separately; starts minus finishes is not a failure count.
4. **Spend, parent + delegates exactly once:** `D(harness.cost.usd)` across
   both delegate values and all purposes. Break down by actual provider/model
   and `cost_known`; display partial-priced spend as a lower-bound component.
   Do **not** add session-cost histogram sums, discard cost, or child summaries.
   Usage coverage is `reported / (reported + unreported)` from
   `harness.model.usage.records`; pricing coverage among reported requests is
   `known / reported`. Always display both coverage ratios beside spend.
5. **Measured waste rate:** `D(harness.model.discard.cost) /
   D(harness.cost.usd)` for matching identities/purposes and pricing filters;
   or sum discard-token increments across the six `bucket` values and divide
   by `D(harness.tokens.total)`. Discards can arrive after billing, so short
   windows can exceed 100%; use long aligned windows/cohorts and label this
   measured discarded work, not semantic task waste. No discard `scope` label
   exists, so do not filter only the denominator to upstream.
6. **Retry pressure and observed outcomes:**
   `D(harness.retries.total) / D(harness.model.requests)` for matching source
   scope, plus actual/planned retry-backoff histogram sums by layer. Use wait
   observations for bounded wait reasons; retry-start reason/status are not a
   reliable label for the preceding failure and cannot join a retry chain.
   A ratio of `outcome=success,cause=retry` to all `cause=retry` finishes is **retry-attempt
   success fraction**, not logical-request recovery. These low-cardinality
   metrics contain no request-chain correlation; eventual logical-request
   retry recovery is **not derivable**. Use correlated diagnostics if needed,
   not an invented recovered-request counter.
7. **Context efficiency and idle usefulness:** graph request-attempt utilization
   quantiles and applied reclamation. A boundary-weighted reclamation fraction
   is `D(harness.context.removed.tokens{reason=compaction}) /
   D(harness.context.tokens.sum{reason=compaction,measurement=before})`, not a
   claim of tokens saved on subsequent requests. Compare retained/dropped
   distributions by policy and retention reset/transition counts. For terminal
   idle candidate dispositions, use `applied / (applied + stale + discarded)`
   from `harness.compactions.dispositions{trigger=idle}`; exclude retriable
   delivery errors. Pair it with prepared run counts, delivery latency, and
   stale-idle discard spend. Cohort/window lag matters; prepared is not applied.
8. **Background outcomes and saturation:** graph
   `harness.background.jobs{outcome=started}` separately from terminal
   `completed|failed|cancelled|abandoned` counts. Break down by tool; compare
   `harness.work.inflight{kind=background}` with delegate/tool/command in-flight
   and their run/queue latency. An abandoned manager job is not evidence its
   subprocess or child has stopped. Filter delegate `mode` to avoid mixing
   runtime lifetime with finite prompt-operation counts.
9. **Exporter loss/health:** alert on increases of rejected/dropped points and
   overflow measurements, elevated `failures / attempts`, and
   `now - last_success` beyond several export intervals for **live processes**.
   Treat zero last-success as never successful. Chart active-series and payload
   bytes to diagnose saturation; compare to the ordinary caps, remembering
   reserved overflow series. Scraped stale gauges from exited CLIs are not
   active outages. Loss counters do not measure all data missing after a crash;
   use Collector health, process liveness, and final local diagnostics too.

Overflow points intentionally lack I/M/W/C dimensions. Include them in total
panels, and display their contribution separately; filtered breakdowns cannot
recover those lost dimensions. Do not claim coverage- or pricing-filtered
ratios account for overflow whose labels are no longer available.

## Migration from the earlier live-sink metrics

Treat this as a schema/meaning change even where names survive. Split dashboards
by deployment/version during rollout; avoid merging old gauges or old histogram
boundaries with the new instruments under one query.

| Earlier interpretation/name | Current contract / migration |
| --- | --- |
| `session_id` on points | Removed completely; process identity is `service.instance.id` on the resource. Use session records/diagnostics for per-session investigations, not metric labels. |
| Provider/model/agent identity inferred at completion | Execution captures identity at launch; physical facts can supply resolved provider/model. Root session distributions intentionally have no such labels. |
| `harness.session.cost`, `harness.session.tokens` latest gauges | Histograms with `scope=root_session_inclusive`, plus `harness.session.total`; inclusive per-root samples, not latest-model attribution or additional spend. |
| `harness.tool.duration` in milliseconds from returned results | Removed; use `harness.work.duration{kind=tool}` in seconds for actual worker execution. Convert thresholds by 1,000 and account for changed timing boundaries. Queue and delivery are separate. |
| `harness.retries.total` once for a retried prompt | Actual retry starts, with source/cause/layer dimensions; waits canceled before start and continuations are not retries. Planned and actual wait histograms are separate. |
| Prompt-level `harness.tokens.*`, `harness.cost.usd` | Exclusive physical-attempt accounting, including failed/discarded/maintenance work. Preserve scope and usage/pricing coverage; do not add summaries. |
| Prompt-level `harness.cost.unpriced_calls` / dropped unknown prices | Per reported request without full pricing. Known partial USD remains in cost with `cost_known=false`; reported zero and unreported usage differ. |
| `harness.delegate.tokens`, `harness.delegate.cost` inclusive counters | Removed; use exclusive `harness.tokens.*`, `harness.cost.usd` with `delegate=true`. Never add root inclusive and child usage for fleet billing. |
| `harness.delegate.compactions` sum | Per-lifecycle histogram; `harness.compactions.total` is the exclusive applied-compaction counter. |
| Diagnostic `harness.model.requests` / request errors | Source attempt starts/outcomes; old proxy/provider fallback is labeled `provider_call`, not invented upstream traffic. Canceled finishes have their own outcome/counter. |
| Assumed zero/aggregate request timing | Optional real physical duration/TTFT; unknown timing has no histogram sample. Provider-call duration is fallback wall time. |
| Retry/maintenance usage guessed to be waste | Stored physical lineage supplies exact disjoint source/logical discard snapshots, including source-only usage; retained native prefixes are excluded and failure alone is insufficient. |
| `harness.commands.total` from launch JSON; `harness.commands.steps_per_batch` | Count executed commands with `tool=argv\|shell`, `trigger=single\|step`. Steps-per-batch family removed; allowlisted `harness.process.diagnostics` exposes executed/skipped step diagnostics. |
| Tool-result byte histogram with one implicit value | Select `measurement=shown` or `original`; two samples describe the same result, not two results. |
| Compactions inferred from prompt/child totals | Separate runs, idle dispositions, and actual applications. Idle prepared work has not reclaimed live context. |
| Background launch/abandonment treated as actual child finish | Manager job lifecycle and actual delegate/command execution are separate. The process group tracks actual workers and complete owners; bounded settlement can skip a mutable root aggregate and lose late facts, never invent completion. |
| Retention/turn/skill observations depended on UI callbacks | Core typed observers cover these execution paths across frontends/children; catalog pressure is not a skill activation. Composition gauges now come from core request snapshots with captured identity, including system/tool-schema/provider-state lengths; they are not wire-byte or token-occupancy measurements. |

## Source and verification pointers

- `internal/otel/sink_model.go`: `Sink.ObserveModel` (exclusive requests, usage,
  timing, retry waits, and discards).
- `internal/otel/sink_work.go`, `sink_context.go`, `sink_auxiliary.go`:
  `Sink.ObserveWork`, `ObserveContext`, `ObservePrompt`, `ObserveTurn`,
  `ObserveSkill`, `recordContextComposition`; `sink.go`: `RecordSession` and the
  `RecordContext` compatibility adapter.
- `internal/execution/context.go`: `ComposeRequest`; numeric pre-dialect request
  composition, supplied by `internal/agent/execution.go`: `observeRequestContext`.
- `internal/otel/exporter.go`, `metrics.go`, `config.go`: budgets, encoded payloads,
  retry/partial-response semantics, `Exporter.Health`, `Exporter.Shutdown`.
- `internal/execution/model.go`: `ModelCall`; `internal/execution/discard.go`:
  `ModelCall.Retain` and `ModelCall.Discard`; `internal/llm/attempt.go` and
  `attempt_tracker.go`: source facts and disposition ownership;
  `internal/llm/provider.go`: `StreamEvent.HasReportedUsage`;
  `internal/modelproxy/server/attempt.go`: `Handler.priceAttempt`.
- `internal/execution/group.go`: `Group.Begin`, `Group.Wait`; whole-owner and
  actual-worker settlement, not a cancellation mechanism.
- `cmd/harness/root_telemetry.go`: process exporter/group ownership and
  `rootTelemetry.Finalize`; `cmd/harness/acp_lifecycle.go`: independent
  construction admission, root settlement, and bounded final export.

Regression coverage includes OTel wire/privacy/reporting/reliability tests,
execution attempt/retry/discard tests, source/proxy attempt tests, actual tool
and child lifecycle tests, and terminal/ACP root lifecycle tests. For local
checks run `go test ./internal/otel ./internal/execution`, then the repository's
required `make` and `go build ./... && go vet ./... && go test ./...`.
