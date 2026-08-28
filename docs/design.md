# harness — architecture and design

A minimal agentic coding harness in Go: a plain-text, line-oriented CLI that drives a
tool-using LLM loop against local files, shell commands, and git.

This is a living architecture document for the current system. It records how the
codebase works today and evolves as harness gains capabilities.

## 1. Goals

- **Small and legible.** The whole system should be readable in an afternoon. One purpose
  per package; no framework.
- **Zero third-party Go dependencies.** Go stdlib only. SSE, diff application, HTML
  stripping, and retries are all small enough to own.
- **Unix philosophy for tools.** When the job is already owned by a mature host
  CLI, run it through `shell` instead of reimplementing optimized search or
  command semantics. Keep a dedicated argv wrapper only where Harness owns a
  distinct workflow contract, such as `git`.
- **Provider and MCP access are isolated.** `harness` uses one provider-neutral
  message/streaming model and talks to `harness-model-proxy` over HTTP. The model
  proxy owns API keys, provider configs, model metadata, and concrete provider
  dialects. Optional remote MCP access similarly runs through
  `harness-mcp-proxy`, keeping provider and MCP credentials outside the agent
  process.
- **No sandboxing or permission prompts.** The harness assumes it is launched inside an
  already-sandboxed environment. Tools run with the process's privileges, immediately.
- **First-class git.** A dedicated `git` tool plus git context in the system prompt.

## 2. Constraints

| Constraint | Choice |
|---|---|
| Language | Go 1.26 (`iter` / range-over-func used) |
| Dependencies | stdlib only |
| Module / binary | `module harness`, binary built from `cmd/harness` |
| Interface | line-oriented plain text; basic Markdown on terminal output; optional ANSI color only when stdout is a TTY; `NO_COLOR` and `-no-color` disable color; `color_theme=dark|light` selects a palette without probing the terminal background |
| Secrets | API keys live in `harness-model-proxy`; the `harness` process talks to it over HTTP |

## 3. Architecture

At the deployment level, the unrestricted agent CLI can run in an isolated
workspace while separate model and MCP services retain credentials in a trusted
environment:

```text
                               +-----------------------+
+----------------------+       | Env with Credentials  |
| Sandboxed Workspace  |       |                       |
|                      |       |    +-------------+    |
|   +-------------+    |   +---+----> Model Proxy |    |
|   |             |    |   |   |    +-------------+    |
|   | Harness CLI + ---+---+   |                       |
|   |             |    |   |   |     +-----------+     |
|   +-------------+    |   +---+-----> MCP Proxy |     |
+----------------------+       |     +-----------+     |
                               +-----------------------+
```

Both proxies support optional API keys for CLI-to-proxy access. They are
intended to listen only on localhost or a trusted local network; proxy
authentication does not make an otherwise untrusted deployment safe.

Within the main CLI, prompts flow through the provider-neutral agent loop while
tool calls dispatch against the local registry:

```
                 ┌────────────────────────────────────────────┐
 stdin ──────►   │ internal/ui        REPL / one-shot driver  │
                 │   meta-commands, rendering, usage line     │
                 └──────────────┬─────────────────────────────┘
                                │ user prompt
                 ┌──────────────▼─────────────────────────────┐
                 │ internal/agent     turn loop               │
                 │   interrupt handling, compaction           │
                 └────┬──────────────────────────┬────────────┘
                      │ Request                  │ ToolCall
        ┌─────────────▼────────────┐   ┌─────────▼────────────┐
        │ modelproxy/client        │   │ internal/tools       │
        │   HTTP catalog + stream  │   │   registry+dispatch  │
        └─────────────┬────────────┘   │   built-in tools     │
                      │                └──────────────────────┘
        ┌─────────────▼────────────┐
        │ harness-model-proxy      │
        │   llm factory + dialects │
        └─────────────┬────────────┘
              │ provider HTTP + SSE (internal/sse, internal/retry)
              ▼
        provider endpoint
```

### Package layout

```
cmd/harness              config/session/LSP command dispatch, config load, proxy catalog wiring, signals, and REPL-vs-oneshot execution
cmd/harness-model-proxy  provider setup/refresh and HTTP model proxy server; generated command dispatch plus config inspection
internal/cli             immutable nested command/flag catalogs, presence-aware parsing, and deterministic scoped help
internal/modelproxy      proxy protocol, client Provider, server handler
internal/modelproxy/config model-proxy top-level setting catalog, source resolution, and safe projections
internal/modelproxy/pricing generic request-cost pricers: flat llm.Price plus provider-specific dynamic models
internal/llm             provider-agnostic types, Provider interface, model/price registry
internal/llm/openai      Chat Completions dialect: wire structs, request builder, stream decode, tool-call assembly
internal/llm/responses   OpenAI Responses dialect: same responsibilities
internal/llm/anthropic   Messages dialect: same responsibilities
internal/sse             generic SSE frame reader
internal/retry           backoff + jitter + Retry-After parsing
internal/agent           turn loop, interrupt state machine, compaction
internal/tools           Tool interface, registry, dispatch (recover + central truncation), built-in tools; the same registry also hosts the delegate, background-job, MCP (§15), and LSP (§15a) tools
internal/delegate        configured child-agent tool; starts child agents without an import cycle
internal/background      process-local background job manager + tools
internal/session         append-only conversation tree, mutable state, replay, archives, artifacts, and derived human-only evidence catalog
internal/sessionrec      one canonical raw.ndjson recorder shared by root and delegate sinks
internal/trajectory      bounded host-owned evaluator stagnation control state
internal/config          typed harness definitions, strict source resolution, provenance, and redacted projections
internal/configmeta      package-neutral parameter catalog, source vocabulary, provenance snapshots, and deterministic reference renderers
internal/modelcatalog    normalized models.dev/OpenAI Codex baseline catalogs
internal/modelproxy/modeldiscovery authenticated provider model adapters, caches, and catalog merging
internal/ui              REPL, streaming renderer, tool summaries, usage line
internal/sysprompt       embedded prompt files + environment context + AGENTS.md sections
internal/agentdef        agent definitions (allowed tools, MCP exposure, prompt/model target) (§14)
internal/hooks           command-only lifecycle hooks (SessionStart/UserPromptSubmit/Pre+PostToolUse/Pre+PostCompact/Stop)
internal/skills          skill discovery + `$skillName` prompt expansion
internal/todo            advisory update_todos store and renderer (§9.13)
internal/plan            immutable record_plan artifact and latest-plan store (§9.17)
internal/handoff         pending implementation-approval request holder (§9.17, §14)
internal/goal            `/goal` session state, prompt binding, and continuation rendering (§10)
internal/auth            provider auth sources (token_command, oauth2, codex_oauth) for the model proxy
cmd/harness-mcp-proxy  optional MCP proxy daemon + debug/config client with generated command dispatch
internal/mcp             tools-only MCP slice: schema, client, server, stdio + streamable-HTTP transports
internal/mcp/jsonrpc     JSON-RPC 2.0 framing and bidirectional request/response correlation
internal/mcpproxy      proxy internals: config, supervisors, tool registry, daemon
internal/metrics         shared Prometheus collectors/exposition plus endpoint config resolution and lifecycle
internal/otel            stdlib-only cumulative OTLP/HTTP JSON metrics exporter and live sink: `harness.prompt.*`, `harness.tokens.*`/`harness.cost.*`, `harness.tool.*`/`harness.tools_per_turn`/`harness.parallel.*`/`harness.commands.*`/`harness.skill.*`/`harness.retention.*`/`harness.model.*`, plus `harness.session.*`/`harness.context.*`/`harness.delegate.*` at session exit (fleet + debug telemetry; see `internal/otel/sink.go`)
internal/mcptools        harness-side adapter: tools.Tool over a reconnecting proxy Conn (§15)
internal/lspproxy      LSP manager: language-server supervisors, Content-Length JSON-RPC, agent-oriented code-intelligence tools (§15a)
internal/lsptools        harness-side adapter exposing short `lsp_*` tools over the LSP manager (§15a)
```

The block above lists the core data path plus the optional MCP/LSP surfaces; a few
small leaf packages (`inputimage`, `markdown`, `replprompt`, `httpserve`, `httpx`,
`mcpchild`, `term`) are omitted for brevity.

`internal/llm` is the shared contract between the agent loop and any model provider.
In the main CLI, the only runtime provider is `modelproxy/client.Provider`; concrete
OpenAI/Anthropic dialects are constructed inside `harness-model-proxy` via
`internal/llm/factory`.

Two optional capabilities run outside that core path. Remote MCP support lives behind
the `harness-mcp-proxy` daemon (§15); LSP code intelligence is served by the in-process
`internal/lspproxy` manager (registered as short `lsp_*` tools through `internal/lsptools`,
§15a) and is also exposed as a compatibility stdio MCP shim via `harness lsp serve`.

## 4. Message model (`internal/llm`)

The internal model is Anthropic-shaped — a content-block list — because it is a lossless
superset of OpenAI's flat fields: collapsing blocks into OpenAI's shape is mechanical,
while the reverse direction would lose structure.

The full types live in `internal/llm/message.go` (`Role`, `Message`,
`MessageOrigin`, `ParallelToolBatch`, `CompactionMetadata`, `BlockKind`,
`ContentBlock`) and are not mirrored here. The load-bearing rules:

- `Role` is only `user`/`assistant`: there is no tool role (tool results are
  content blocks on a user message) and no system role (the system prompt is a
  `Request` field, never a message).
- `ContentBlock` is a tagged union — exactly the fields documented for its
  `Kind` are set:

| `Kind` | Fields | Meaning |
|---|---|---|
| `text` | `Text` | ordinary text |
| `image` | `Image*` | user-provided visual input; `ImageData` is base64 without a `data:` prefix |
| `tool_use` | `ToolUseID`, `ToolName`, `ToolNamespace`, `ToolInput` | assistant calls a tool; `ToolInput` is the complete JSON object; `ToolNamespace` preserves a hosted Responses namespace for exact replay while local dispatch still uses `ToolName` |
| `tool_result` | `ResultForID`, `ResultText`, `ResultError`, `ResultUseless`, `ResultContent` | answers a tool call; `ResultContent` carries shallow image children only; `ResultUseless` is a local compaction hint |
| `thinking` | `Thinking`, `ThinkingSignature` | Anthropic extended thinking; the signature is echoed back verbatim |
| `redacted_thinking` | `RedactedData` | opaque Anthropic payload, echoed verbatim, never rendered |
| `reasoning` | `ReasoningID`, `ReasoningEncrypted` | OpenAI Responses encrypted reasoning item, replayed verbatim in stateless mode |
| `interaction_thought` | `InteractionThoughtSummary`, `InteractionThoughtSignature` | Gemini Interactions thought; distinct from Anthropic thinking so signed state cannot cross dialects |
| `interaction_step` | `InteractionStep` | provider-managed Interactions step for stateless replay; never rendered or dispatched |
| `responses_tool_search` | `ResponsesToolSearch`, `ReasoningReplayDomain` | complete hosted `tool_search_call` or `tool_search_output`, replayed verbatim on the same provider-local domain |
| `anthropic_tool_search` | `AnthropicToolSearch`, `ReasoningReplayDomain` | complete hosted `server_tool_use` or `tool_search_tool_result`, replayed verbatim and in block-index order on the same provider-local domain |
| `provider_compaction` | `ProviderCompaction` | provider-native canonical input items; valid only on a hidden compaction-checkpoint message |

Replay-provenance rules for the opaque reasoning kinds
(`ReasoningReplayDomain`, same-domain-only replay, transcript never rewritten)
and the `Origin`/`Compaction`/`ParallelToolBatches` metadata rules are spelled
out in the design notes below.

Design notes:

- **System prompt lives on `Request.System`,** not in the message list. This is the
  natural Anthropic shape, trivially becomes a leading `role:"system"` message for
  OpenAI, and means compaction can never accidentally summarize it away.
- **`ToolInput` is `json.RawMessage`,** not `map[string]any`: it arrives as a byte stream,
  the tool layer decodes it into its own typed struct anyway, and raw bytes round-trip
  through session files without re-encoding surprises. Assistant `tool_use` blocks and
  raw session transcripts retain the complete provider input, including a top-level
  `_stage`. Scheduling and execution use per-call copies with that reserved field
  removed; tool classifiers, hooks, dispatch guards, built-in implementations, and
  MCP/LSP adapters never receive it.
- **JSON tags are provider-neutral** (`kind`, `tool_use_id`, …). Opaque provider
  steps include Gemini Interactions Google Search replay, OpenAI Responses and
  Anthropic hosted tool-search replay, and a provider-native compaction window.
  Incompatible dialects ignore them.
- **`Origin` is transcript-only provenance.** It preserves prompt, steering,
  internal, and compaction-checkpoint boundaries; provider adapters ignore it.
- **Provider-owned blocks are opaque replay state.** Anthropic thinking,
  signatures, and hosted tool-search blocks; Responses encrypted reasoning and
  hosted tool-search items; and Gemini Interactions thought summaries/signatures
  use distinct kinds
  and record the active target's `ReasoningReplayDomain`. Request assembly
  replays them only when that domain matches the selected target; filtering is
  request-only and never rewrites the transcript. They are never treated as
  ordinary user-visible text.
- **Provider compaction checkpoints are opaque replay state.** A checkpoint is a
  hidden user message whose sole block contains the provider's complete canonical
  input-item array and the active `ReasoningReplayDomain`. Same-domain request
  assembly replays every item in order and then the transcript suffix; other
  domains omit the checkpoint without deleting the provider-neutral history.
- **`ParallelToolBatches` is local scheduling metadata.** It appears on the user message
  carrying tool results and is ignored by provider request builders. Each entry names,
  in emission order, every tool-use ID in one same-stage default-parallel island whose
  dependency graph permits concurrency. Some members may be queued behind earlier
  overlapping file mutations. A fully chained island is dispatched sequentially and
  omitted; no field therefore means no possible intra-island concurrency or a transcript
  written by an older harness version that did not record the distinction.
- **Provider-hosted tools are separate from local tools.** `Request.Tools` carries
  function schemas backed by `internal/tools`; `Request.ServerTools` carries neutral
  provider-hosted declarations such as `web_search`. The model proxy resolves those
  neutral declarations into provider-specific wire shapes before calling a dialect.
- **Rich tool results remain one logical result.** `ResultText` is always present for
  ordinary summaries and compatibility. Optional `ResultContent` is shallow and may
  contain only validated image blocks; error results are always text-only. The same
  shape is exposed as `ToolResult.Content` at the dispatch seam. `omitempty` preserves
  byte-for-byte text-only session JSON compatibility.
- **Rich blocks are validated before provider work.** Top-level images are user-only;
  top-level and nested images require supported MIME/detail values, valid base64 whose
  magic matches the declared type, nonnegative metadata, and no foreign union fields.
  Actual encoded lengths are authoritative. The agent gates the complete retained
  transcript before counting, compaction, prewarm, and provider streams, and both
  model-proxy request endpoints repeat the gate before resolving or constructing a
  provider.

Two small seam types carry tool traffic between the agent loop and the tool layer;
they are flat views of the corresponding content blocks:

```go
type ToolCall struct { // from a BlockToolUse
    ID                string
    Name              string
    Input             json.RawMessage
    InvalidInputError string // malformed streamed args; Input contains a valid diagnostic object
}

type ToolResult struct { // becomes a BlockToolResult
    ForID         string
    Text          string
    IsError       bool
    Useless       bool   // successful but semantically empty; live content is unchanged
    Truncated     bool   // central cap (§8.3) trimmed the result
    OriginalText  string // full pre-truncation text, archived to artifacts/
    OriginalBytes int    // size before truncation
    ShownBytes    int    // size after truncation
    Usage         Usage          // metered tools (e.g. delegate) report child token usage
    Content       []ContentBlock // optional shallow image children
    Metrics       map[string]int // diagnostics only; never copied into a content block
}
```

### Transcript invariant

> Every assistant `tool_use` block has exactly one matching `tool_result` block in the
> following user message, and no `tool_result` is orphaned.

Both APIs hard-reject conversations that violate this. A `ValidateTranscript([]Message) error`
helper encodes the invariant; tests assert it after every operation that mutates a
transcript (cancel, compact, resume, max-turns stop). `ValidateTranscript` also rejects
any assistant `Phase` outside `""`, `AssistantPhaseCommentary` (`commentary`), or
`AssistantPhaseFinal` (`final_answer`). Repair rules:

- **Cancel mid-turn:** keep streamed partial text as an assistant text-only message;
  strip un-executed `tool_use` blocks. If nothing streamed, drop the partial message.
- **Resume with a dangling `tool_use`** (session saved mid-turn): synthesize a
  `tool_result` with `ResultError: true`, `ResultText: "interrupted"`.

### Wire mapping

| Internal | OpenAI Chat Completions | OpenAI Responses | Anthropic Messages |
|---|---|---|---|
| `Request.System` | leading `{"role":"system","content":…}` message | top-level `instructions` | top-level `system` blocks |
| user text | `{"role":"user","content":"…"}` | `message` item with `input_text` content | user message with `text` content |
| user image | structured `image_url` content with a data URL and detail | `input_image` content with a data URL and detail | `image` content with a base64 source |
| assistant text | assistant message content | `message` item with `output_text` content and optional phase | assistant message with `text` content |
| tool_use | assistant `tool_calls[].function` with JSON-string arguments | `function_call` item with string arguments and the hosted namespace when present | `tool_use` content with object input |
| tool_result | sibling `role:"tool"` message; rich images follow in one neighboring user message | `function_call_output` item; rich images follow in one neighboring user message | `tool_result` content inside a user message, including nested image children |
| hosted tool-search replay | ignored | exact `tool_search_call` / `tool_search_output` items | exact `server_tool_use` / `tool_search_tool_result` blocks in original index order |
| opaque reasoning replay | ignored by default; `reasoning_content` on assistant messages when the provider opts in (`reasoning_replay`) | `reasoning` item with encrypted content | signed `thinking` or opaque `redacted_thinking` content; `reasoning_replay:"current_turn"` drops blocks older than the in-flight tool chain |

Reasoning replay is gated per dialect. Chat Completions replays persisted
thinking text as `reasoning_content` only when the provider config opts in
AND reasoning is enabled for the request (compaction/prewarm stay clean);
streamed `reasoning_content` is tagged for persistence only under the opt-in.
Anthropic replays signed thinking verbatim whenever thinking is enabled (the
protocol requires it on the trailing tool-use chain); the opt-in
`reasoning_replay:"current_turn"` mode trims everything before the last real
user turn for providers that document history dropping — wire-only, the
transcript keeps every block. `harness session stats` reports the active
branch's replay share as a "reasoning replay" line (blocks, bytes, estimated
tokens at the estimator's prose/opaque weights).

Mapping subtleties that must be handled:

- OpenAI `function.arguments` is a JSON **string** (`"{\"path\":\"x\"}"`); Anthropic
  `input` is a JSON **object**. A call with no arguments must serialize as `"{}"` for
  OpenAI, never `""`.
- OpenAI Chat Completions tool results are **sibling messages, not blocks**: each
  `BlockToolResult` is hoisted into its own `role:"tool"` message, placed immediately
  after the assistant message that issued the calls, in call order. OpenAI Responses
  emits a `function_call_output` item instead. For a batch with rich results, both
  OpenAI adapters emit every string/function output first and then one neighboring
  user message containing all images in original result/child order. Anthropic nests
  rich image children directly in the matching `tool_result.content` array.
- Internal error-result text contains only the explanation; `ResultError` is the
  canonical error signal. Neither OpenAI wire format has an `is_error` field, so both
  OpenAI adapters prefix exactly one `ERROR: ` in the result string. Anthropic sends
  the unprefixed explanation with `is_error: true`.
- An assistant message with tool calls but no text serializes with `content` omitted
  (OpenAI Chat Completions) or no text block (Anthropic). OpenAI Responses represents
  the tool calls as standalone `function_call` items.
- For Chat Completions, an assistant message that carries an image or multiple content
  blocks serializes `content` as a structured **parts array**; a plain text message keeps
  `content` as a bare string.
- OpenAI Responses maps image blocks to `input_image` parts with data URLs; the
  Anthropic dialect ignores `ImageDetail`.

## 5. Provider layer

### 5.1 Interface and stream events

```go
type Provider interface {
    Name() string // "openai" | "responses" | "anthropic"

    // Stream runs one model call. The iterator yields events until a Done
    // event or a terminal error (yielded at most once, last). Consumer break
    // or ctx cancellation aborts the underlying HTTP request.
    Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error]
}
```

The remaining contract types live in `internal/llm/provider.go` and are not
mirrored here: `InputTokenCounter` and `ContextCompactor`/`CompactedContext`
are optional provider extensions (input-token counting and provider-native
context compaction); `Request` is one model call's worth of provider-neutral
input; `ReasoningConfig` carries the portable reasoning profile/effort/budget;
`ToolSchema` is the model-facing tool declaration whose `Parameters` schema is
owned by the tool layer. Non-obvious `Request` field semantics:

| Field(s) | Rule |
|---|---|
| `Purpose` | `turn`\|`compaction`\|`prewarm`\|`branch_summary`; harness/proxy metadata, never forwarded upstream |
| `MaxTokens` | `0` = automatic policy (§5.4) |
| `ServiceTier`/`Speed`/`Betas` | resolved by the model proxy from the selected catalog target before dialect dispatch; dialects ignore beta identifiers they do not own |
| `EstimatedInputTokens` | caller estimate of model-visible input tokens (keeps max output within the window); `0` asks the dialect to estimate |
| `ContextWindowHint` | caller's effective window, including overrides or runtime-learned values; `0` = provider configuration |
| `RequestContext` | request-only hook/todo/goal/background context; per-dialect placement in the prose below |
| `ProxySessionID` | harness-local sticky-routing and transport-affinity key; never forwarded upstream |
| `CacheAffinityID` → `PromptCacheKey` | harness-local conversation key that survives continuation resets; the proxy derives the provider-facing cache key from it |
| `CachePolicy` | caller-owned semantic cache boundaries: `StaticTTL` (`5m`\|`1h`) for the stable system/tool breakpoint, `StableMessagePrefix` leading messages safe from future retention rewrites; dialects choose provider-specific breakpoint placement |
| `StoreResponse`/`PreviousResponseID` | Responses stateful continuation controls |
| `ResponseState` | resumable provider continuation state: previous response/interaction ID, anchor message count, and the lowercase SHA-256 digest of the represented prefix |

`RequestContext` is request-only instruction context: Chat Completions appends it
as a trailing system message after the transcript, Anthropic appends it as a
trailing user-role message (single uncached text block) placed after the
cache breakpoints are laid so the rolling tail breakpoint stays on the last
real transcript message, and Responses adds it as a late `role:"developer"`
input item immediately before the current user message or trailing tool-call/output
group. All three keep the stable system+tools+transcript prefix intact for prefix
caching. Fresh TODO/goal/background/hook context applies to the current request
without looking like the latest user prompt or becoming part of the persisted
transcript.
Responses streams surface `response.id` on terminal `EventDone.ResponseID`; the
agent stores that with the local transcript anchor for optional
`previous_response_id` continuation. `EventDone.ResponseIDAnchor` is an optional
`*int` used only by out-of-band prewarm: nil means the response ID must not be
installed, while Responses WebSocket `generate:false` reports an explicit zero
anchor.

`iter.Seq2[StreamEvent, error]` (range-over-func) was chosen over channels: the consumer
is a plain `for ev, err := range stream` with natural early-`break` cancellation, and the
producer keeps stream state on its own stack — no goroutine lifecycle to leak.

The stream-event types live in `internal/llm/provider.go` (`EventKind`,
`StreamEvent`, `StopReason`, `ModelRequestState`). `StreamEvent` is a flat
carrier for the tagged event kinds; `Index` disambiguates parallel tool calls
within one turn. One line per event kind:

| `EventKind` | Carries |
|---|---|
| `EventTextDelta` | incremental assistant text |
| `EventToolCallStart` | tool_use began: ID and name known |
| `EventToolCallDelta` | partial JSON args (rendering only) |
| `EventToolCallDone` | one call fully assembled; malformed streamed args are replaced with a diagnostic object plus `InvalidInputError` |
| `EventUsage` | usage snapshot (may arrive more than once) |
| `EventDone` | turn end: `StopReason`, final usage, Responses stored-response anchor |
| `EventReasoningSummary` | display-ready provider-visible reasoning summary text |
| `EventAssistantPhase` | assistant message phase metadata |
| `EventInteractionStep` | hidden complete Gemini server-managed step for stateless replay |
| `EventModelRequest` | diagnostics-only request lifecycle metadata; never model content |

`StopReason` has four constants: `end_turn`, `tool_use`, `max_tokens`, `stop`
(stop sequence matched).

StopReason normalization: OpenAI `stop|length|tool_calls` and Anthropic
`end_turn|max_tokens|model_context_window_exceeded|tool_use|stop_sequence` map
onto the four constants. Unknown or
provider-specific reasons (e.g. `content_filter`) map to `end_turn` — the turn is over
either way — and are noted on the rendered usage line.

### 5.2 SSE client (`internal/sse`)

A dialect-agnostic frame reader over `io.Reader`:

```go
type Event struct {
    Type string // from "event:" lines; "" when a dialect sends none
    Data string // "data:" lines joined with \n
}

func Read(ctx context.Context, r io.Reader) iter.Seq2[Event, error]
```

- `bufio.Scanner` with an enlarged buffer (max token ~1 MB — default 64 KB is too small
  for large tool-argument frames).
- Accumulates `event:`/`data:` lines; yields on blank line; strips one leading space
  after the colon per the SSE spec; ignores comment (`:`) lines.
- Dialect handling stays in the providers:
  - **OpenAI Chat Completions:** every frame is `data:` JSON; the literal
    `data: [DONE]` terminates. Text, refusal text, modern `tool_calls`, usage,
    and compatible reasoning fields are consumed. Audio and deprecated
    `function_call` output fail explicitly.
  - **OpenAI Responses:** typed frames such as `response.output_text.delta`,
    `response.output_text.done`, `response.refusal.delta`, `response.refusal.done`,
    `response.content_part.done`, `response.output_item.added`,
    `response.output_item.done`, `response.reasoning_summary_text.delta`,
    `response.reasoning_summary_text.done`, `response.reasoning_summary_part.done`,
    `response.function_call_arguments.delta`, `response.function_call_arguments.done`,
    `response.completed`, `response.incomplete`, `response.failed`, and a bare
    terminal `error` frame. Assistant message `phase` metadata from output items is
    preserved on transcript messages. Lifecycle, annotation, hosted web-search
    status, and raw-reasoning events are recognized without exposing an agent
    event. Known unsupported output-bearing events fail explicitly; unknown
    future event envelope types are ignored.
  - **Anthropic:** typed frames — `message_start`, `content_block_start`,
    `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`,
    `ping` (ignored), `error` (terminal stream error; retryability follows the
    normalized type and embedded retry-delay hints). Indexed blocks and legal
    delta types are validated; known unsupported output-bearing blocks fail
    explicitly, while unknown future event envelopes remain ignorable under
    Anthropic's additive versioning policy. Native tool search accumulates the
    indexed `server_tool_use` input, retains complete `tool_search_tool_result`
    blocks, and emits both as hidden provider-owned replay events.
  - **Gemini Interactions:** `interaction.created`, indexed `step.start` /
    `step.delta` / `step.stop`, and terminal `interaction.completed`. Model text,
    thought summaries/signatures, function arguments, and Google Search activity
    arrive as typed step deltas.
- **Truncated stream:** body EOF without the dialect terminator (`[DONE]`,
  `response.completed` / `response.incomplete` / `response.failed`,
  `interaction.completed`, or `message_stop`) → `ErrTruncatedStream`. The agent may re-request the step from
  scratch when the terminal error is retryable; failed-attempt usage still counts.
- Cancellation rides on the HTTP request context: cancelling unblocks the body read and
  the iterator yields `ctx.Err()` as its terminal error.

### 5.3 Streaming tool-call assembly

Providers emit granular `Start`/`Delta` events for live rendering **and** guarantee that
`EventToolCallDone.ToolInput` is always complete, valid JSON. The agent loop forwards
`Start`/`Delta` to the renderer, but only `Done` affects transcript mutation and tool
dispatch. When a provider streams malformed arguments, the assembler emits `Done` with
`InvalidInputError` set and `ToolInput` replaced by a small diagnostic object; dispatch
is short-circuited into an `is_error` tool result so the next turn can correct
it. Assembly is per-turn state inside each provider's `Stream`:

- **OpenAI:** `choices[].delta.tool_calls[]` arrive with an `index`; the first delta for
  an index carries `id` + `function.name` (emit `Start`), subsequent deltas carry
  `function.arguments` string fragments (emit `Delta`). All buffered calls flush as
  `Done` when `finish_reason: "tool_calls"` arrives.
- **Anthropic:** `content_block_start` with `type:"tool_use"` gives `id` + `name` at a
  block index (`Start`); `content_block_delta` with `input_json_delta` carries
  `partial_json` fragments (`Delta`); `content_block_stop` flushes that call
  (`Done`). Hosted `server_tool_use` search blocks consume the same JSON delta
  shape but never emit client tool-call events, and their matching
  `tool_search_tool_result` blocks never create local tool results.
- **Gemini Interactions:** `step.start` with `type:"function_call"` gives `id` +
  `name`; `step.delta` with `type:"arguments_delta"` carries JSON-string
  fragments; `step.stop` flushes the call.

Edge cases:

- **Empty arguments:** OpenAI may send zero fragments; an empty buffer flushes as `{}`.
- **Validation on flush:** malformed or non-object accumulated JSON is never dispatched
  to the real tool. The provider emits an invalid `Done` with a diagnostic JSON object,
  and the agent returns an error tool result that includes the parse detail.
- **Parallel calls:** dialects may interleave multiple calls; `Index` keeps them
  distinct and emission order is preserved into the transcript.
- **Interleaved text and tool_use** (Anthropic): text blocks share the index space but
  bypass the assembler.

### 5.4 Request building

| Concern | OpenAI Responses | OpenAI Chat Completions | Anthropic Messages | Gemini Interactions |
|---|---|---|---|---|
| Endpoint default | `https://api.openai.com/v1/responses` | `https://api.openai.com/v1/chat/completions` | `https://api.anthropic.com/v1/messages` | `https://generativelanguage.googleapis.com/v1beta/interactions` |
| Auth | `Authorization: Bearer <key>` | same | `x-api-key: <key>` + `anthropic-version: 2023-06-01` | `x-goog-api-key: <key>` |
| Transport | HTTP SSE by default; provider configs may set `responses_websocket:true`, and the proxy defaults it on for `codex_oauth` Responses providers | HTTP SSE | HTTP SSE | HTTP SSE |
| Tool schemas | `tools[] = {type:"function", name, description, parameters, strict:false}`; native search also uses deferred functions inside `namespace` tools plus `tool_search` | `tools[].function = {name, description, parameters}` (`type:"function"`) | `tools[] = {name, description, input_schema}`; native search flattens deferred functions at top level with `defer_loading:true` | `tools[] = {type:"function", name, description, parameters}` |
| Server tools | `web_search`, or OpenRouter `openrouter:web_search`; `tool_search` for native deferred namespaces | OpenRouter `openrouter:web_search`; MiMo `web_search`; Kimi `builtin_function.$web_search`; Z.AI nested `web_search` options | `web_search_20250305` named `web_search`; versioned BM25 or regex `tool_search_tool_*_20251119` | `google_search` |
| Parallel tool hint | `parallel_tool_calls:true` when tools are present | `parallel_tool_calls:true` when tools are present | not sent | not sent |
| Prompt cache key | provider-configured; OpenAI auto emits `prompt_cache_key`; managed `openai-codex` configs select it explicitly | provider-configured; OpenAI auto emits `prompt_cache_key`, OpenRouter auto emits `session_id` | not sent (explicit `cache_control` breakpoints instead) | not sent; stored continuation and Gemini implicit caching handle stable prefixes |
| Stateful continuation | CLI-owned `store:true` + `previous_response_id`, fingerprint-validated suffix trimming, and one full-history fallback; `responses_stateful:false` sends `store:false` | ignored | ignored | CLI-owned `store:true` + `previous_interaction_id`, fingerprint-validated suffix trimming, and signed full-history fallback; `interactions_stateful:false` sends `store:false` |
| Assistant phase | assistant input items include stored `phase` when present | ignored | ignored | ignored |
| Response format | provider default unless explicitly requested by a caller | provider default | provider default | forced to plain text; generated media is rejected |
| Token cap | input-aware `max_output_tokens` | input-aware `max_completion_tokens` for `api.openai.com`; `max_tokens` for compatible/custom endpoints | required input-aware `max_tokens` | input-aware `generation_config.max_output_tokens` |
| Input token count | `POST /responses/input_tokens`; `codex_oauth` uses a local `o200k_base` estimate | local `o200k_base` estimate | `POST /v1/messages/count_tokens` | provider-neutral local estimate |
| Standalone compaction | opt-in `POST /responses/compact`; returned items are replayed unchanged | not supported | not supported | not supported |
| Streaming usage | final `response.usage` | `stream_options.include_usage` | message start/delta | final interaction usage, including cached and thought tokens |
| Stop sequences | not sent | `stop` | `stop_sequences` | `generation_config.stop_sequences` |
| Temperature | omitted when nil | omitted when nil | omitted when nil | always omitted |
| Reasoning controls | `reasoning.effort` / summary | provider-specific effort/budget | effort/budget/toggle | `thinking_level` and `thinking_summaries` |

Anthropic provider `base_url` values are versioned API prefixes, matching
models.dev and `@ai-sdk/anthropic` semantics. The dialect appends `/messages` or
`/messages/count_tokens`; it does not add another `/v1`.

#### OpenAI Responses support boundary

Harness implements the model-and-tools subset of Responses needed by the coding
loop. A scoped snapshot of the official create, input-token-count, and streaming
references lives in `internal/llm/responses/testdata/api_surface.json`; its
contract test requires every captured field, item/content discriminator, tool,
event, and usage property to be classified as supported or intentionally
unsupported.

| Surface | Supported | Intentionally unsupported |
|---|---|---|
| Operations | streaming `POST /responses`; `POST /responses/input_tokens`; opt-in `POST /responses/compact` | retrieve/delete/cancel, conversations, background work |
| Request controls | model/input/instructions, functions, hosted web search, native deferred namespace tool search, output cap, temperature, reasoning summaries/encrypted replay, prompt cache key, service tier, stored continuation | arbitrary output formats, moderation, metadata, tool choice, sampling/logprob controls, safety/user identifiers |
| Content/items | text and image input; message text/refusal, reasoning summary, function calls/results, hosted web-search status, hosted tool-search call/output replay | file/audio input; generated media, computer/code/shell/patch/custom tools |
| Stream/usage | text/refusal/reasoning-summary/function deltas, terminal/error events, cached input, non-reasoning output, and reasoning tokens | raw reasoning disclosure, annotations, and unused total-token fields |

`function_call_output.output` is always present, including for an empty or
image-only result. Status-only and lifecycle events are recognized and ignored;
known unsupported content-bearing events and output item types fail instead of
silently disappearing. Unknown future event envelope types remain ignorable
because the OpenAI API treats new event types as additive.

#### OpenAI Chat Completions support boundary

The Chat dialect also serves OpenAI-compatible endpoints. Its scoped official
surface snapshot lives in `internal/llm/openai/testdata/api_surface.json`;
compatible request and usage extensions remain separately supported by the wire
implementation and are not presented as first-party OpenAI fields.

| Surface | Supported | Intentionally unsupported |
|---|---|---|
| Operation | streaming `POST /chat/completions` | stored completion CRUD and legacy `/v1/completions` |
| Request controls | messages, modern function tools, parallel calls, output cap, temperature, stop sequences, reasoning effort, opt-in `reasoning_content` replay (`reasoning_replay` provider quirk, gated on reasoning being enabled for the request), prompt cache key, service tier, usage streaming | audio/modalities, legacy functions, custom tools, arbitrary formats, prediction, moderation, sampling/logprob controls |
| Content | system/user/assistant/tool text, user images, refusal text | developer/function roles, file/audio input, generated audio |
| Stream/usage | text/refusal/tool-call deltas, compatible reasoning summaries, cached input, non-reasoning output, and reasoning tokens | legacy `function_call`, audio output, prediction/audio token details |

First-party endpoint selection is host-based: `api.openai.com` receives
`max_completion_tokens`, while custom and compatibility endpoints continue to
receive `max_tokens`; the two fields are never sent together.

#### Anthropic Messages support boundary

Harness implements the model-and-tools subset of Messages needed by the coding
loop. A source-hashed snapshot of the official create, token-count, streaming,
and versioning references lives in
`internal/llm/anthropic/testdata/api_surface.json`; its contract test requires
every captured request field, content/tool discriminator, event, delta, stop
reason, and usage property to be classified.

| Surface | Supported | Intentionally unsupported |
|---|---|---|
| Operations | streaming `POST /v1/messages`; `POST /v1/messages/count_tokens` | batches, files, model listing, and administrative APIs |
| Request controls | model/messages/system, required output cap, temperature, stop sequences, service tier, beta speed, effort, adaptive/budget thinking, explicit block caching | containers, inference geography, metadata, tool choice, sampling controls, structured output, context-management betas |
| Content/tools | text and base64 images; client function calls/results; signed and redacted thinking; `web_search_20250305`; versioned BM25/regex tool search with top-level deferred functions | documents/search-result input, container uploads, code/bash/text-editor execution, web fetch, memory, computer use, newer web-search variants |
| Stream/usage | text/tool/thinking/citation deltas; hosted web-search call/results for continuation; hosted tool-search call/results for exact replay; disjoint input/cache/output/reasoning usage; served tier/speed | server-tool request counts and per-query fees, inference geography, stop details |

Required union fields are emitted even when empty (`text`, `input`, and
`input_schema`). Hosted web-search blocks remain dialect-private and are retained
only for same-turn continuation. Hosted tool-search blocks instead become opaque,
same-domain provider-neutral replay blocks while remaining invisible and
non-dispatchable to the agent tool loop.

Anthropic `pause_turn` is continued inside the dialect. The complete assistant
content, including hosted search blocks, is replayed in original block-index
order, rolling cache breakpoints are refreshed without mutating opaque search
blocks, and usage snapshots remain cumulative across the resulting HTTP
requests. Five continuations are allowed; another pause returns the terminal
`pause_turn_limit` error instead of looping indefinitely. A pause response that
also requests a client tool is rejected as invalid.

#### Gemini Interactions support boundary

Harness implements the model-and-tools subset needed by the coding loop, not
the full Interactions product surface. The official OpenAPI document is
`https://ai.google.dev/static/api/interactions.openapi.json`. A scoped snapshot
of its request fields and union discriminators lives in
`internal/llm/interactions/testdata/openapi_surface.json`; its contract test
requires every captured field/type to remain explicitly classified as supported
or intentionally unsupported when that snapshot is refreshed.

| Surface | Supported | Intentionally unsupported |
|---|---|---|
| Operation | streaming `POST /v1beta/interactions` for a model | read/delete/cancel, background work, managed agents, environments, webhooks |
| Request controls | model/input, system instruction, stored continuation, functions, Google Search, service tier, output cap, stop sequences, thinking level/summaries; response format is fixed to plain text | labels, safety settings, arbitrary response modalities/formats, deprecated MIME shortcut, seed, tool choice, media/transcription configuration |
| Content | text and inline image input; text output | audio, video, and document input; generated media |
| Steps/tools | user/model text, thought signatures/summaries, function call/result, Google Search call/result | code execution, URL context, computer use, MCP server, file search, Maps, and retrieval steps/tools |
| Stream/usage | all documented SSE envelope events; total input, cached, output, and thought tokens | modality breakdowns, tool-use totals, grounding query counts |

Some backend events observed in practice (`interaction.failed` and
`interaction.cancelled`) are accepted as compatible terminal extensions.
Unsupported generated media and step types fail explicitly rather than being
silently discarded. Status-only events are recognized and ignored because the
coding loop has no intermediate state to expose.

The same model-facing `ToolSchema.Parameters` bytes go into `parameters` vs
`input_schema`. Harness strips nested JSON Schema `description` fields before
advertising tools; each tool's top-level description remains the explanatory text.

**Default `max_tokens` cap (`defaultMaxTokensCap = 1_000_000`).** When the user
does not set `MaxTokens`, all four dialects start from
`min(1_000_000, contextWindow/4)` when the context window is known. The model
catalog's `output_limit` is a ceiling, not the default, so full-window limits
such as `output_limit == context_window` do not reserve the whole remaining
context. Explicit `MaxTokens` is treated as an upper bound, not permission to
exceed the window. The chosen cap is clamped to
`contextWindow - inputTokens - reserve`, where the reserve is
`min(max(512, contextWindow*3/100), contextWindow/4)`.
Before normal model requests, `inputTokens` comes from provider count APIs for
OpenAI Responses and Anthropic when available through the proxy, then from a
local `o200k_base` estimate for OpenAI/OpenRouter Chat Completions, then from
the coarse request byte estimate. Anthropic always sends the computed value
(`max_tokens` is required); OpenAI Chat Completions and Responses send
their endpoint-appropriate completion cap / `max_output_tokens` only when the
value is known.
Responses providers can set `omit_max_output_tokens` when a compatible backend
rejects the standard parameter. Responses providers default to stateful
continuation; provider configs can set `responses_stateful:false` for compatible
endpoints that require `store:false`, or `responses_stateful:true` to explicitly
advertise support. If a provider rejects `store:true` before streaming output,
the agent disables stateful continuation for that agent, rebuilds the request,
and retries once stateless. Responses providers can set `responses_websocket:true`
to use the Responses WebSocket transport; the proxy defaults that on for
`codex_oauth` Responses configs and preserves explicit true/false overrides.
The Codex WebSocket request always carries `store:false`; its response IDs are
continuation handles scoped to the originating live socket rather than durable
stored Responses objects. If a WebSocket request without a previous response
fails before output, the provider falls back to stateless HTTP with `store:false`
and suppresses that HTTP response ID so the next request resends full context and
can re-establish WebSocket continuation. A request carrying a socket-scoped
previous response never crosses transports.
If a provider rejects a request with a parseable
context-overflow error, the agent records the smaller reported window for the
session, rebuilds the request, and retries once before surfacing the error.

**Prompt cache and proxy session mapping.** Harness deliberately uses two opaque
local keys. `Request.ProxySessionID` is a transport-affinity identity: the proxy
uses it in the bounded Responses WebSocket connection key, and the proxy client
sends it as `X-Harness-Session` so a load balancer can consistently hash stream
traffic. It is not authoritative continuation state. Compaction, branch
navigation, and real model/agent changes rotate it; a base ↔ fast variant switch
preserves it and a still-valid continuation anchor. `Request.CacheAffinityID`
is the longer-lived cache-routing identity: it is persisted in `state.json`,
restored on resume, and preserved across continuation resets. Delegate agents
derive their own distinct affinity key from the parent's and their fixed child id
(`harness-cache-` + `sha256(parentID + "\x00" + childID)`), so each child routes
to its own cache shard — a child's system prompt and tool subset differ from the
parent’s, so it never reads the parent's cached prefix, and sharing the key would
only thrash the shared shard under concurrency. A genuinely new logical session
(`/clear`, clone/fork extraction, or a
new process session) rotates both keys.

Before calling any concrete provider, the proxy strips both raw keys and sets
`Request.PromptCacheKey` to `hex(sha256(CacheAffinityID))`, so API providers see
a stable cache-affinity value without a harness-specific prefix. Provider
configs can control where that provider-facing key goes with
`prompt_cache.key_field`: `auto` (default), `none`, `prompt_cache_key`, or
`session_id`. `auto` sends `prompt_cache_key` to first-party OpenAI and ChatGPT
Codex endpoints, `session_id` to OpenRouter Chat Completions, and no cache key
field to other OpenAI-compatible custom base URLs. Managed `openai-codex`
configs explicitly select `prompt_cache_key` so that behavior is self-describing.
`prompt_cache.affinity_headers` can also copy the provider-facing
stable key into non-auth routing headers such as `x-session-id`.
Anthropic does not use this key directly (it pins explicit `cache_control`
breakpoints).

Compaction and branch-summary requests derive deterministic
purpose-separated cache and proxy IDs from the owning session identity with
SHA-256 and fixed domain strings. This keeps maintenance traffic on a consistent
cache shard without letting it reuse or compete with the main conversation's
WebSocket/continuation chain. Prewarm intentionally retains the main session
identities.

The proxy catalog advertises `prewarm:true` only for Responses WebSocket
targets that can send an empty `response.create` with `generate:false`; the CLI
does not issue speculative generated completions to other targets. Only this
path marks its terminal event with an explicit response-ID anchor at transcript
index zero. The background closure captures the provider, response-state epoch,
proxy session, and transcript snapshot; its result returns through the REPL
maintenance queue and is installed on the owner goroutine only if the epoch,
session, transcript length/fingerprint, and empty-current-anchor checks still
hold. A real turn therefore wins the race. The WebSocket client continuously
drains frames and answers ping heartbeats between model requests; a close frame
or transport EOF makes its last response ID unavailable.

The CLI owns every continuation as a previous response/interaction ID, anchor
message count, and SHA-256 of the exact provider-neutral anchor prefix.
`FingerprintMessages` prefixes the encoded input with the message count, zeroes
only `Message.Time`, JSON-encodes every other message/content field (including
nested rich tool results), and emits lowercase hexadecimal. Empty, malformed,
or non-matching fingerprints never trim. Before each request the agent validates
the saved index and fingerprint, then sends only
`transcript[AnchorMessages:]`; after appending the assistant message it hashes
the complete new transcript and updates the anchor. Session resume additionally
requires exact saved/current provider and model identity plus a
continuation-capable catalog target. Clone/fork extraction deliberately clears
the anchor.

The model proxy holds no authoritative per-session continuation or cache-policy
state. It rejects continuation on an unsupported target, leases a provider,
optionally rejects a connection-affine previous ID with 409
`previous_response_unavailable` before writing status/events, and otherwise
forwards messages, `store`, and the previous ID unchanged. It does not retry
previous/store rejections. The agent recognizes local or upstream previous-ID
rejections, clears its anchor, and performs one full-history resend. Thus HTTP
stored continuation is replica-independent, while Codex `store:false` uses
sticky routing only as a hit-rate optimization.

On catalog targets that advertise prewarm support, Shift-Tab agent switches
defer prewarm behind a 500ms idle debounce;
each additional cycle replaces the pending target, so only the final settled
agent warms. Equality of model IDs must not suppress that warmup because the
agent's system/tool prefix and response continuation can differ. Startup,
explicit `/agent`, handoff and model changes, and standalone `/compact` retain
immediate prewarming. Submitting a real prompt cancels a pending delayed warmup
before the turn starts.

**Responses reasoning persistence.** On stateless or full-history fallback
requests, the provider would otherwise re-derive chain-of-thought on every tool
turn. For a reasoning request harness sends
`include: ["reasoning.encrypted_content"]`,
captures each reasoning item's id and `encrypted_content`, and persists it on the
transcript as a `BlockReasoning` content block (§4). On the next request
`buildInput` re-emits that as a `reasoning` input item immediately before its
`function_call`, so reasoning is replayed rather than recomputed. The replay is
gated on the request itself being a reasoning request — a reasoning-off call
(compaction summary, prewarm) drops the encrypted items, since a reasoning input
item without the matching `include` is rejected.

**Opaque reasoning replay domains.** The model catalog gives every base target
a provider-local replay domain. The default is the exact base target ID;
service-tier variants inherit it. A model entry can set
`reasoning_replay_domain` to a shared provider-local label when the provider
explicitly guarantees that multiple model IDs accept the same opaque reasoning
state. Prompt-cache affinity remains a separate mechanism and does not widen
this boundary. The agent stamps captured thinking, redacted thinking, Responses
reasoning items, and Interactions thought/step blocks with the active domain.
A single provider-visible message path removes mismatched blocks from a
request-only copy for normal turns, context/debug snapshots, prewarm,
compaction, handoff and branch summaries, and detached idle compaction workers.
Token estimates, summary chunking, and cache policy are all derived from that
same filtered view. Blocks from transcripts predating provenance metadata
therefore do not cross into a configured target. A structured
`invalid_encrypted_content` API error disables replay for the active domain for
the rest of that agent and triggers one retry without opaque reasoning. The
unfiltered transcript remains available to `Agent.Transcript()` and session
persistence; switching models never erases historical reasoning.

**Anthropic prompt caching:** the CLI declares semantic policy; the dialect
chooses wire placement within Anthropic's four-breakpoint budget. The last tool
schema and stable system block use `CachePolicy.StaticTTL`: interactive turns
request `1h`, while one-shot, delegate, prewarm, compaction, and
branch-summary requests use `5m`. Native tool search appends its non-deferred
search declaration last, so this tool breakpoint covers the complete resident
and deferred schema prefix; deferred declarations themselves never carry
`cache_control`. Message breakpoints always retain the provider-default
five-minute TTL and skip opaque hosted-search replay blocks rather than
re-encoding them.

After applying retention and before building a request, the agent computes
`StableMessagePrefix`: the count of leading actual request messages that a
future retention pass cannot rewrite. It stops before a recent large,
read-only, untrimmed tool result or a recent undegraded top-level/tool-result
image. Text, tool-call metadata, mutating results, already-trimmed results, and
already-degraded images are stable. For continuation suffixes, the count is
translated relative to the sliced `Request.Messages`; invalid external counts
are clamped by the Anthropic dialect.

The two message breakpoints land on the last request message and the end of the
declared stable prefix. Positions are deduplicated; when the stable position is
zero/absent or equals the tail, the previous real message is the lagging
fallback. Volatile request-only context remains an uncached system block.
Retention can therefore invalidate the rolling tail without ever mutating the
declared stable-prefix breakpoint. OpenAI Chat Completions remains automatic.
On supported first-party Responses models, the dialect also maps the declared
stable prefix to at most one explicit `input_text` or `input_image` breakpoint
while retaining OpenAI's implicit tail breakpoint. It scans backward across
ineligible assistant/function/reasoning items, never rewrites opaque provider
items or string-shaped function outputs, and caps placement before volatile
request context. Top-level `instructions` remains unchanged. Compatible
Responses backends are opt-in through `prompt_cache.explicit_breakpoints`, and
count/compaction requests omit the marker.

### 5.5 Errors and retries (`internal/retry`)

```go
type APIError struct {
    StatusCode      int
    Code            string          // provider error code/type if parseable
    Message         string
    ResponsePayload DiagnosticPayload // bounded, redacted upstream response
    Retryable       bool
    RetryAfter      time.Duration     // parsed Retry-After, 0 if absent
}
```

- Provider error codes accept JSON strings, numbers, or null. OpenRouter's
  canonical `error.metadata.error_type` wins over its numeric compatibility
  code when present; numeric HTTP-like codes still participate in retry
  classification.
- **Retryable:** HTTP 429, 500, 502, 503, 529 (Anthropic overloaded), and transport
  errors (timeouts, resets, DNS).
- **Fatal, no retry:** 400, 401, 403, 404, 422 — surfaced immediately with the
  provider's error message.
- **Backoff:** full jitter — `sleep = rand(0, min(30s, 500ms·2^attempt))`, 5 attempts.
  `Retry-After` (seconds or HTTP-date) is honored as a floor, except that an explicit
  429/529 wait over 60 seconds is surfaced immediately rather than sleeping. The
  policy is a pure function (`retry.Next(attempt, retryAfter) time.Duration`); the
  retry loop itself is the shared `llm.Connect`, which every dialect calls with its
  endpoint, auth headers, and error-body parser, and which takes an injected `sleep`
  so tests run instantly.
  (The loop originally lived in each provider; the three copies were byte-identical
  apart from those inputs, so they were consolidated.)
- **Provider retries apply only before the first response byte.** Once tokens have
  streamed, the provider treats failure as terminal — mid-stream Anthropic `error`
  frames and truncated bodies fail the provider call. The agent loop re-requests the
  step from scratch when such a failure is retryable (§8.1; spec
  `docs/superpowers/specs/2026-06-11-roadmap-items-design.md` §2), so a transient
  mid-stream failure no longer ends the turn. If the terminal stream error is an
  `APIError` with `RetryAfter`, the agent honors it as the retry floor; a retry-delay
  hint embedded in the error message (e.g. OpenAI streaming rate-limit text like "try
  again in 1.025s") is parsed into that field when no HTTP `Retry-After` header is
  available. A parsed streaming delay over 60 seconds is likewise surfaced immediately
  instead of parking the prompt. This hint parsing applies uniformly to terminal stream
  errors, including Responses `response.failed` and bare `error` frames.
- **Cancellation wins:** `ctx.Err()` is checked before every attempt and every backoff
  sleep, and is distinguished from `APIError` so the UI renders "cancelled" vs "failed".
  The first Ctrl-C or double-Esc requests graceful cancellation. If the same prompt
  remains active, any later Ctrl-C force-exits with status 130; it is not restricted
  to a short double-press window.
  A failed attempt that will be retried is marked as discarded in `raw.ndjson`, so
  replay and editor resume helpers do not treat its streamed text as durable output.
  Physical attempt IDs remain monotonic across both these transport retries and
  request-shape rebuilds such as context-overflow, stored-response, previous-response,
  and encrypted-content compatibility fallbacks. Every discarded attempt's reported
  usage remains in billed totals and is also surfaced as wasted usage.

## 6. Usage, cost, and the model registry

```go
// Usage lives in internal/llm/provider.go.
type Usage struct {
    InputTokens      int // uncached input, billed at full rate
    OutputTokens     int // generated output excluding separately reported reasoning
    CacheReadTokens    int
    CacheWriteTokens   int // default-rate cache writes (Anthropic: 5m)
    CacheWrite1hTokens int // Anthropic 1h cache writes
    ReasoningTokens    int // separately reported reasoning
    CostUSD          float64
    CostKnown        bool
    ServiceTier      string // actually served tier, when reported
    Speed            string // actually served speed, when reported
}
```

Normalization: OpenAI-compatible `prompt_tokens` **includes** cached tokens, so
cache read/write fields (`prompt_tokens_details.cached_tokens`,
`prompt_tokens_details.cache_write_tokens`, DeepSeek's
`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`, and gateway
`cache_read_input_tokens` / `cache_creation_input_tokens`) are normalized into
`CacheReadTokens` / `CacheWriteTokens` and subtracted from `InputTokens`.
Anthropic's `input_tokens` already excludes cached tokens. Its cache-creation
aggregate is split using `cache_creation.ephemeral_5m_input_tokens` and
`ephemeral_1h_input_tokens`; malformed negative or inconsistent counts are
clamped without double-counting. A long-TTL request whose response omits that
breakdown retains its token usage but has unknown cost rather than being priced
at the cheaper default rate. After normalization `InputTokens` means the same
thing across dialects. The derived cache-read ratio is token-weighted after
aggregation: `CacheReadTokens / (InputTokens + CacheReadTokens +
CacheWriteTokens + CacheWrite1hTokens)`. It is unavailable when that denominator
is zero and is never persisted or averaged as an additive usage bucket.

Responses `output_tokens` and Chat Completions `completion_tokens` include the
`reasoning_tokens` detail. Their normalizers clamp that detail to the aggregate
and subtract it from `OutputTokens`, making the output and reasoning buckets
disjoint for UI totals, budgets, metrics, and pricing. Responses-compatible
orchestration output tokens remain an additional output bucket because those
extensions are reported outside the standard aggregate.

Anthropic `output_tokens` likewise includes
`output_tokens_details.thinking_tokens`. The Anthropic normalizer clamps and
subtracts that detail, so output and reasoning are disjoint. Its pricing adapter
uses the output rate for reasoning when no explicit reasoning price exists and
derives the documented 1-hour cache-write rate as `2 × input`; explicit base,
context-tier, service-tier, and speed-specific prices remain authoritative.

`internal/llm/registry.go` holds a small registry. Its structs (`Price`,
`PriceTier`, `ModelInfo`, `ProviderConfig`, `ModelEntry`) carry JSON tags so
they double as the proxy catalog's on-disk schema: `Price` is USD per 1M
tokens across input/output/cache/reasoning/audio dimensions, and `PriceTier`
adds context-length rate steps on the same dimensions. `Cost` prices a usage
record and reports whether the cost is known, `ContextWindow` falls back to a
default 256,000 on a registry miss, and `Models` returns the sorted
configured model ids.

Baseline model metadata originates from the public **models.dev** catalog. The
models.dev and public OpenAI Codex adapters, normalized baseline types, and
vendored fallback snapshots live in `internal/modelcatalog`.
`internal/modelproxy/modeldiscovery` owns authenticated provider catalogs,
normalization, capability filtering, ETags, pagination guards, and their
provider-local cache. Initial adapters cover generic OpenAI-compatible
`/models` endpoints (including trusted Sakana results), OpenRouter, Anthropic,
Gemini, and the ChatGPT Codex account catalog.

Normalized provider models retain field presence separately from zero values so
merge precedence is deterministic: fields explicitly returned by the provider,
then models.dev, then configured metadata, then safe runtime defaults. Generic
OpenAI ID-only results validate baseline/configured IDs; direct-only IDs require
generative capability evidence unless the endpoint or config is trusted.
models.dev `experimental.modes` entries are projected into bounded service-tier
request mappings and tier-specific prices.
The proxy caches a projected catalog as `models.dev.api.json`
in the proxy config directory, retaining every provider and model but only the
metadata fields harness consumes.
`setup` prefers that cache over the vendored snapshot, but fetches and writes it
when it is missing or invalid. A running proxy refreshes the cache when it is older
than `models_dev_cache_ttl` (`24h` by default; `0` disables periodic refresh), and
`refresh-models` fetches and caches that projected catalog before querying
authenticated providers and rewriting configured allowlists. The vendored
snapshot is used only when there is no parseable cache and a live fetch fails.

Provider snapshots are atomically stored mode `0600` under `provider-models/`.
A snapshot younger than `provider_models_cache_ttl` (`1h` by default) is
authoritative for availability; a stale snapshot can only enrich metadata. The
serving coordinator performs an immediate bounded-concurrency refresh followed
by hourly polling, then swaps one immutable catalog snapshot after each cycle.
Network/auth/decode failures retain the prior state. Auto-detected 404/405
responses mark discovery unsupported for that process and restore models.dev
availability authority.

The synthetic `openai-codex` provider uses the public OpenAI Codex catalog only
as baseline metadata. With working `codex_oauth`, the authenticated ChatGPT
account catalog controls the visible model set. Its list visibility is trusted;
the public catalog's `supported_in_api:false` flag does not remove a model the
account endpoint returns. That endpoint requires a numeric `client_version`.
Harness sends the official stable Codex CLI compatibility version embedded with
the vendored catalog, never the Harness build version. `make
refresh-model-catalogs` resolves the latest stable `rust-vX.Y.Z` release and
updates the version and catalog from that same release tag as one change.

### Managed vs manual provider configs

Provider config files are either **managed** or **manual**:

- **Managed** configs are written by `setup`/`refresh-models` and carry
  `"managed": true`. They store **no per-model `price`**; instead the proxy
  resolves each managed model from provider snapshots plus the in-memory
  models.dev cache. A fresh complete provider snapshot controls availability;
  direct metadata/pricing wins when present and models.dev fills gaps. A failed
  provider refresh preserves the configured allowlist. Confirmed absence hides
  a target from the immutable serving snapshot without editing its file, so a
  later successful response can restore it. Re-running `setup` never clobbers
  hand-edited prices because managed configs hold none.
- **Manual** configs are any provider file lacking `"managed": true` — typically
  hand-written. The proxy never touches them and serves their own `price` and
  `input_modalities` entries verbatim. A pre-existing price-bearing config
  without the flag is treated as manual and keeps its metadata (there is no
  migration). Running `refresh-models` against such a provider rewrites it as
  a managed, price-less config.

A managed config may also carry `"price_source"` — a models.dev provider id to
resolve its prices from when that differs from the config's own `name`; it is a
price-only override and does not control availability or capabilities. The
synthetic `openai-codex` provider does not use `price_source`: it is backed by a
ChatGPT subscription, so the proxy serves token counts without dollar costs and
ignores any stale Codex `price_source` left by older configs. It also writes
`"omit_max_output_tokens": true` because the ChatGPT Codex backend rejects the
Responses `max_output_tokens` parameter; the proxy infers the same omit behavior
for older `codex_oauth` Responses configs. The proxy also defaults
`responses_websocket` on for `codex_oauth` Responses configs so continuation can
use the Codex-compatible WebSocket path without writing another managed config
field. Managed config writes `prompt_cache.key_field:"prompt_cache_key"` so the
proxy's stable, hashed cache-affinity key reaches the Codex backend.

Provider configs may set `"min_output_tokens"` when an endpoint rejects tiny
output caps. Responses requests default this floor to 16 for
`max_output_tokens`; Chat Completions requests keep tiny caps such as the
`max_tokens:1` prewarm unless a provider config opts into a floor. If an
OpenAI-compatible provider rejects a pre-stream request with a parseable
minimum-token error, the proxy retries once at the inferred floor and logs a
`retrying model request with higher output token floor` warning with the
configured and inferred values so configs can be updated deliberately.

Managed prices honor `model.cost.tiers` (`context` threshold plus higher-rate
price) from the models.dev cache, so context-length price bands such as Sakana
Fugu Ultra's 272k-token tier are applied automatically. Static tier schedules are
preserved in `GET /v1/models`; setup and runtime model pickers render every band
as input/output USD per 1M tokens, for example
`$5/$30 ≤272k · $10/$45 >272k`.

Provider and model entries may set `server_tools:["web_search"]`. The proxy
serves the normalized list in `GET /v1/models`, and harness only declares hosted
web search when the selected target advertises it and the harness config sets
`web_search:"auto"` / `HARNESS_WEB_SEARCH=auto` / `-web-search auto`. The proxy
also infers `web_search` for known endpoints: OpenAI Responses, Anthropic,
Sakana, OpenRouter, MiMo, Kimi, Z.AI, and native Google Interactions. If a provider rejects a server-tool
field before streaming any events, the proxy retries the request once without
server tools so stale metadata does not fail the turn.

Provider and model entries may also set typed `service_tiers` objects to
advertise request-level scheduling modes. A model-level list overrides the
provider list. Each `llm.ServiceTier` carries a target-suffix ID, display
metadata, an optional `llm.Price`, and a bounded `ServiceTierRequest` containing
only `service_tier`, `speed`, and Anthropic beta identifiers. This deliberately
does not turn catalog data into arbitrary request bodies or headers.

Managed models get their tier options from models.dev mode metadata. The Codex
adapter consumes `service_tiers` and the deprecated `additional_speed_tiers`
catalog fields, canonicalizing legacy OpenAI `priority` Fast metadata to the
user-facing and wire value `fast`. No provider-wide tier inference is applied.
For every non-default mode, the served catalog expands the base model into a
separate target such as `provider:model:fast`, carrying `base_target_id`, `variant`, and
its own price. Target resolution—not a caller-supplied request field—selects the
bounded provider mapping. Anthropic encodes `speed` and merges required feature
identifiers into `anthropic-beta`; OpenAI dialects encode `service_tier`.
`/fast` switches between sibling targets while retaining the base model's proxy
continuation/cache identity.

The base `llm.Price` schedule represents standard processing; each service tier
may carry its own schedule. Request pricing prefers provider-reported served
`service_tier`/`speed` metadata, which accounts for graceful downgrades, then
falls back to the requested mapping. OpenAI may report its former `priority`
label for a Fast request; pricing recognizes that response label as Fast. A
mode without an accurate price is
handled-but-unknown, so usage tokens remain known while `CostKnown` stays false
and reject-unpriced proxy budgets fail closed.

Static catalog pricing schedules use `price_source` for managed configs when the
pricing is representable as `llm.Price`, including context tiers. Request costs
flow through the pricing package's generic interface: provider-specific pricers
can return handled-but-unknown for dynamic models, and the generic pricer handles
the existing per-1M-token `llm.Price` shape for all other configured models.
For Responses and Chat Completions API types, an API-specific pricer fills an
unset reasoning rate from the output rate after normalization splits the API's
aggregate output count into output and reasoning buckets. First-party Google
Interactions uses the same schedule transformation because it reports thought
tokens separately while Google bills them as output tokens. Anthropic Messages
also receives the output-rate reasoning fallback plus a `2 × input` fallback
for 1-hour cache writes. Explicit rates always win, and each fallback is applied
to base, context-tier, service-tier, and speed schedules. The rule is scoped to
these wire contracts rather than being generic. Numeric schedules come from a
direct provider response when it supplies a validated representable schedule
(currently OpenRouter), then models.dev, or a manual provider config. The
filtered models.dev snapshot preserves explicit `cost.reasoning` values when raw
catalog entries provide them.

Google Search per-query charges are not part of `llm.Price` and are not added to
`CostUSD`. Although the Interactions usage schema exposes
`grounding_tool_count`, the public pricing source is a human-facing,
model-dependent schedule rather than a stable model-ID keyed catalog. Harness
therefore reports and budgets the token portion only instead of presenting a
query fee as exact. Anthropic hosted web search follows the same policy: its
reported token usage is priced, but the per-search fee is excluded.

The serving handler holds its registry, pricer, and served catalog behind an
atomic snapshot. The initial snapshot is built from provider configs,
models.dev, and cached provider states. Either refresher updates its source
under a mutex, rebuilds the composite immutable snapshot, and atomically swaps
it, so `/v1/models` responses and per-request `cost_usd` accounting always
reflect one coherent catalog. Only a fresh complete provider state prunes live
availability; failed refreshes do not mutate handler state.
`internal/llm` stays free of any `internal/modelcatalog` import — the server is
the layer that bridges normalized catalog metadata into `llm`.
Candidate cache updates must parse as models.dev JSON and contain at least one
provider and model. When a previous cache is parseable, replacement is rejected
if provider or model counts change by more than 4x and the absolute delta is
large enough to rule out normal small-catalog churn; the old cache remains in
place. Successful replacements first save the previous cache as
`models.dev.api.json.bak`, overwriting that single backup on each update.

Harness only uses models exposed by `harness-model-proxy`; arbitrary
provider-local model names are rejected unless they are configured in the proxy
catalog. Configured models whose request usage has no known cost display token
counts without a dollar figure; dynamic pricers can still produce `cost_usd` for
models that omit a flat catalog `Target.Price`. Configured models without
context-window metadata use a
conservative 256k default, configurable with `-default-context-window` and
overridable for a run with `-context-window`. Model prices, context windows,
output limits, reasoning support, and provider-local opaque-reasoning replay
domains are loaded from the model proxy catalog.
Harness presents reasoning as the portable profile list `default`, `none`,
`minimal`, `low`, `medium`, `high`, `xhigh`, and `max`. The CLI sends the
selected profile to the model proxy, and the proxy maps it to the closest supported
provider/model control. For effort-based models, `minimal` is the lowest
supported non-`none` value and `max` is the highest. For budget-token models,
profiles map to percentages of the configured maximum budget: `minimal` 5%,
`low` 25%, `medium` 50%, `high` 75%, `xhigh` 90%, and `max` 100%, clamped to
the cataloged range. The proxy does not invent budget values when no maximum is
known. The public model catalog reports only whether reasoning profiles are
supported; provider-specific options stay private to the proxy.
Responses API reasoning summaries default off, can be set to `auto`, `concise`,
`detailed`, or `none`, and are displayed only when explicitly enabled. Quiet mode
(`-q`/`--quiet`) suppresses reasoning summary output unless `-reasoning-summary`
is explicitly set on the CLI.

## 7. Configuration and model selection

`internal/cli` and `internal/configmeta` deliberately describe different
surfaces. `internal/cli` owns immutable nested command trees, command-scoped flag
metadata, presence-aware ordered values, parser errors, and deterministic scoped
help; handlers and exit policy remain in each `cmd` package. `internal/configmeta`
defines the package-neutral configuration vocabulary: ordered `Catalog` entries
describe stable keys, types, flags, environment names, JSON paths, accepted
values, literal or derived defaults, descriptions, and sensitivity. It rejects
duplicate input surfaces and drives deterministic text, JSON, and Markdown
reference and snapshot renderers.

Configuration loading remains package-owned rather than generic:
`internal/config`, `internal/modelproxy/config`, and `internal/mcpproxy` each own
an independent catalog and preserve their domain's precedence, empty-value,
path, warning, and child-validation semantics. All three return safe resolved
snapshots with `default`, `derived`, `file`, `environment`, or `flag` provenance.
The catalog layer does not read configuration or secrets and never routes
commands.

For scalar settings, one generic definition registers flags, parses and normalizes
file/environment/flag candidates, applies **flag > environment > config file >
default** precedence, assigns the typed value, records provenance, and supplies
the projected inspection value. Every present source is validated before the
winner is chosen: malformed lower-precedence values still fail. `LookupEnv`
preserves absent versus explicitly empty values; an empty plain string can clear
a setting when meaningful, while required strings/enums reject it. Strict JSON
decoding rejects non-object input, unknown fields, trailing values, and explicit
`null`; omission is the inheritance mechanism. Structured values such as agents,
hooks, MCP headers/local configuration, LSP servers, and environment maps use
custom definitions because they have dynamic maps or multi-input semantics, but
they still contribute complete catalog metadata and provenance.

Loading returns a `config.Result`: `Config` contains only source-resolved user
settings, `RunOptions` contains invocation-only prompts, images, session/resume,
format, quiet, debug, and informational controls, `Sources` maps stable keys to
`default`, `derived`, `file`, `environment`, or `flag` origins, and `ConfigPath`
records the selected file. `cmd/harness` injects contextual `RuntimeDefaults` for
the model/MCP proxy URLs, history path, default agent, and tmux state; these are
resolved during loading and marked `derived` rather than patched in afterward.
Config path selection is centralized as `-config` > `HARNESS_CONFIG` > an
existing conventional path; explicit paths must be non-empty and exist.

`harness config list`, `show`, and `check` reuse this catalog and loader and are
strictly offline. `show` uses a catalog-driven `Projection`, not direct `Config`
serialization: it reconstructs nested JSON paths, optionally includes provenance,
and redacts non-empty proxy API keys, MCP headers, configured environment-map
values, and opaque LSP initialization options with no secret-display escape hatch.
The projection is deterministic, versioned in JSON, limited to user settings, and
does not embed prompts or materialize built-in agents. `check` additionally performs local semantic checks
for agents, hooks, and `@file` references.

`trace_proxy` / `HARNESS_TRACE_PROXY` / `-trace-proxy` opts in to W3C Trace
Context headers for harness-to-proxy requests. OTLP/HTTP JSON metrics export is
controlled by `otel.enabled`/`otel.endpoint` (`HARNESS_OTEL_*` /
`OTEL_EXPORTER_*` fallbacks), `otel.hostname` → the process-stable `host.name`
resource (`HARNESS_OTEL_HOSTNAME`/`OTEL_HOSTNAME` override, defaults to short
`os.Hostname()`; explicit empty disables it), and
`otel.headers`/`otel.resource_attributes`. Session, provider, model, agent, and
delegate identity are metric-point attributes so REPL switches and `/clear`
cannot relabel cumulative resource series. See `internal/otel/` and the parameter
matrix in [usage.md](usage.md#harness-configuration-parameters). `HARNESS_LOG_LEVEL` controls
harness diagnostics; `HARNESS_TIMESTAMPS` accepts only `short`, `full`, or `none`.
`HARNESS_NO_COLOR` is a strict boolean, while non-empty standard `NO_COLOR` is a
presence-based override. Provider API keys and provider base URLs are resolved
only by `harness-model-proxy`. The generated parameter matrix in
[usage.md](usage.md#harness-configuration-parameters) is the canonical source
for exact setting surfaces and defaults; `examples/harness/config.json` is a
strictly valid, intentionally concise example rather than a duplicate schema.
- `harness-model-proxy setup` creates a proxy config in the default proxy directory,
  appends a new provider config to an existing proxy config, or updates an existing
  configured provider. It reads cached models.dev provider metadata, fetching and
  caching all providers and models with unused upstream fields pruned when needed,
  falls back to a vendored models.dev
  snapshot only when no parseable cache is available and live fetch fails, lists
  harness-supported providers, marks existing providers with bold text and `*`,
  derives missing first-party API URLs from exact `@ai-sdk/openai`,
  `@ai-sdk/anthropic`, and plain `@ai-sdk/google` package metadata, prompts for
  the API key when the provider needs one, queries the selected provider's
  authenticated model endpoint when available, merges its results, pages models
  newest-first, and asks which models should be locally available. The
  synthetic `openai-codex` provider is listed when OpenAI provider metadata is
  available, with its baseline models from the vendored or cached Codex model
  catalog and its live choices from a usable account token; it
  writes the ChatGPT Codex backend URL and a `codex_oauth` auth block instead of
  API-key fields, plus `omit_max_output_tokens:true`. The proxy defaults the
  Responses WebSocket transport on for this `codex_oauth` provider at runtime,
  unless the config explicitly sets `responses_websocket:false`. Providers
  such as Sakana are listed when models.dev includes them; their
  `provider.shape` drives `api_type` and any required `responses_stateful`
  default is applied from provider-specific rules (e.g. Sakana is stateless).
  New providers start with no models enabled; existing providers start with
  their configured models enabled and all other catalog models disabled. Enabled
  rows are bold and marked with `*`; the
  selector accepts number/id toggles plus global `all`, global `none`, `save`,
  `/search`, `n`, `p`, and `cancel`. The provider config is
  generated from the selected catalog with only enabled models for that provider:
  base URL, api_type (`responses`, `openai`, `anthropic`, or `interactions`), key env vars, context windows,
  output limits, input modalities, and reasoning metadata. It is written as a **managed** config (`"managed": true`)
  with **no per-model prices** — the proxy resolves managed prices live from the
  models.dev cache (see *Managed vs manual provider configs*). Without `-force`,
  setup refuses to overwrite provider files that are not already referenced by the
  proxy config.
- `harness-model-proxy refresh-models` fetches and caches the latest live
  models.dev catalog, queries supported providers with working credentials, and
  refreshes each configured allowlist. All catalog fetches finish before provider
  files are changed. A complete direct response may remove absent configured
  models but never enables new IDs. Authentication, transport, decode, empty, or
  incomplete-pagination failures preserve that provider's allowlist; unsupported
  endpoints use models.dev authority. Refreshed files remain managed and
  price-less while stored API keys, auth, discovery overrides, and provider quirks
  are preserved. A provider with no remaining models is removed with its now-empty
  file/reference. Missing provider files are warned and dropped; malformed files
  still error.
- Successful `auth login` performs the same direct refresh for that provider.
  Catalog failure is a warning and does not roll back the stored token. The
  command reports that setup must be rerun to enable newly discovered IDs.
- **Model-proxy lifecycle and probes.** `cmd/harness-model-proxy` binds the API
  listener before announcing startup and uses separate background, handler-work,
  API-stop, and metrics lifetimes. An outer lifecycle handler routes unauthenticated
  `GET /healthz` and `GET /readyz` before API-key middleware; other methods return
  405. The first termination signal makes readiness fail, stops background
  catalog/API-key refresh, waits the drain-propagation delay, and invokes
  `http.Server.Shutdown` without cancelling request contexts. A bounded timeout
  force-closes remaining work. Only after the API drain does the command close the
  WebSocket pool, mark health false, and explicitly shut down metrics.
  `drain_delay`, `shutdown_timeout`, and `instance_id` use serve
  flag > environment > config > default precedence. Defaults are `5s`, `5m`, and
  a random 16-byte hex ID; instance IDs validate against
  `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`. The ownership flip is a coordinated
  breaking cutover: the initial deployment must not route CLI traffic through a
  mixed old/new proxy fleet. Once complete, the stateless wire contract supports
  normal proxy-only rolling updates.
- **Bounded Responses WebSocket pool.** The proxy keys pooled transports by
  separately hashed connection configuration and `ProxySessionID`. Defaults are
  64 connections, a one-hour idle TTL, no absolute-age cap, and a 30-second
  janitor tick. Acquisitions lease an entry; idle capacity pressure evicts LRU,
  while all-busy pressure creates an unpooled provider that closes on release.
  Expired active entries leave lookup immediately but close only after their last
  idempotent release. Shutdown rejects new leases, closes idle entries, retires
  active ones, and stops the janitor. No provider `Close` runs under the pool
  mutex.
- **API-key authentication between harness and the model proxy** is optional and
  disabled by default; it becomes required as soon as the first key is stored in
  the dedicated accepted-key file. Keys are generated with
  `harness-model-proxy generate-api-key [-api-keys-file path] [-ttl 720h]
  [-budget-usd 25 -budget-period 24h] [-budget-reject-unpriced] <name>` and
  stored as hashes under an `api_keys` array in that key file, not inline in the
  normal proxy config. The config may set `api_keys_file` (relative paths are
  resolved next to the config); otherwise the default is `api_keys.json` next to
  the model-proxy config. `serve -api-keys-file` overrides both. Entries are
  `{name, hash, added, expires_at?, cost_budget?}`; omitted/zero `expires_at`
  never expires, and optional `cost_budget` entries store per-key
  `{limit_usd, period_seconds, reject_unpriced?}` metadata.
  Running HTTP proxies poll only this key file and atomically swap accepted-key
  snapshots, preserving the previous good snapshot on reload errors. Inline
  top-level `api_keys` in the normal config is rejected with a migration error.
  Presented `Authorization: Bearer <key>` values are hashed with SHA-256 and
  compared via `crypto/subtle.ConstantTimeCompare`. Key prefixes (`hmp_` for the
  model proxy, `hmcpp_` for the MCP proxy) make them human-distinguishable. The
  plaintext key is printed exactly once at generation and never stored or logged.
  Harness supplies a key via `-model-proxy-api-key`, `HARNESS_MODEL_PROXY_API_KEY`,
  or the config-file `model_proxy_api_key` field, with flag > env > file precedence;
  this outgoing client key is loaded at process start and is not hot-reloaded.
- Provider config auth: `api_key` / `api_key_env` remain the default secret path.
  When a provider config supplies none of `api_key`/`api_key_env`/`auth`, the proxy
  falls back to a hardcoded env var keyed on the provider's `api_type`:
  `ANTHROPIC_API_KEY` (anthropic), `RESPONSES_API_KEY` then `OPENAI_API_KEY`
  (responses), `GEMINI_API_KEY` (interactions), and `OPENAI_API_KEY` (otherwise). An optional `auth` block takes
  precedence and resolves dynamic request headers:
  `token_command` executes an argv command, parses plain-token or JSON
  `access_token` output, caches it in memory until expiry/TTL, and sends
  `Authorization: Bearer ...` by default; `oauth2` reads a stored access token,
  refreshes it when needed (the client secret comes from `client_secret` or the env
  var named by `client_secret_env`), and is managed with
  `harness-model-proxy auth login|logout|status <provider>`. Browser PKCE and
  device-code flows are supported. `codex_oauth` is the OpenAI Codex ChatGPT
  subscription auth path: login uses OpenAI's device-code endpoints, refresh uses
  the Codex JSON refresh exchange, and request headers include `Authorization`,
  `ChatGPT-Account-ID`, and `X-OpenAI-Fedramp` when required. A terminal refresh
  failure is cached only in that `Source`, keyed by the SHA-256 digest of the
  failed refresh token. Before returning it—and immediately after receiving the
  terminal response—the process rereads the shared token file and adopts a valid
  access/refresh token rotated by a peer. Failure markers are never persisted;
  tolerant decoding ignores old marker fields. Successful token files are written
  under the proxy config dir via temp-file then rename.
- The model proxy logs a structured start and completion record per `/v1/stream`
  request with
  `proxy_instance_id`, requester, provider, model, request/response bytes,
  duration, token usage, stop
  reason, tool-call count, and `cost_usd` when the request has known cost
  (from the config for manual flat-priced providers, the models.dev cache for
  managed providers, or tiered prices derived from `model.cost.tiers`).
  When a request carries a valid W3C `traceparent`, stream completion logs append
  `trace_id`, `span_id`, `parent_span_id`, and `trace_sampled` (plus `tracestate`
  when present). `/v1/models` and `/v1/input_tokens` emit lightweight completion
  records only for traced requests, with the same trace fields. Invalid trace
  headers are ignored and never fail the request. Proxy config accepts `log_level` (`debug|info|warn|error`) and `log_format`
  (`json` default, or `text`), with serve flags overriding config. Proxy config
  also accepts `models_dev_cache_ttl` as a duration string such as `"24h"` or
  numeric `0`; the `-models-dev-cache-ttl` serve/setup/refresh flag overrides it.
  `provider_models_cache_ttl` and `-provider-models-cache-ttl` independently
  control authenticated provider polling (`1h` default, `0` disables background
  polling but not explicit setup/refresh/login discovery); the flag is accepted
  by serve/config inspection and setup.
  Every unsuccessful upstream attempt is additionally logged at WARN when it
  occurs, even when a later retry succeeds. These records include the proxy and
  upstream request ids, upstream attempt, status/code, parsed provider message,
  retryability, retry-after/delay, attempt duration, and request elapsed time.
  When the provider returned a JSON error or a JSON fragment failed typed
  decoding, `api_response_payload` contains a compact diagnostic copy capped at
  16 KiB. Common prompt, generated-content, reasoning, tool-argument, credential,
  and binary fields are redacted recursively; image-bearing requests omit the
  payload because error messages can echo image data. Non-JSON fragments record
  only byte length and SHA-256. The same `response_payload` crosses the proxy
  protocol in model-request lifecycle events.
  Model-request events and API error diagnostics carry the same instance ID;
  `(proxy_instance_id, proxy_request_id)` is the cross-replica correlation key.
  OpenRouter's `X-Generation-Id` is accepted as the upstream request ID, including
  for errors received after an HTTP 2xx stream has opened.
  Retry scheduling is logged separately at INFO; cancellation is distinguished
  from an upstream failure.
- `POST /v1/input_tokens` accepts `{provider, request}` and returns
  `{input_tokens, source, scope}` when the configured provider implements
  `InputTokenCounter`. `scope` is `effective_context` only when the count covers
  all logical context the next model call will process; it is `request_payload`
  when it covers only the submitted payload (for example, a stateful continuation
  whose prior context remains server-side), and `unknown` for a legacy response.
  The client preserves that distinction in context telemetry and never treats a
  payload-only count as the effective total. Legacy proxies that omit `scope`
  remain compatible but their count is diagnostic rather than authoritative. `codex_oauth` Responses targets return a local
  `source:"o200k_base"` estimate instead of forwarding to a non-Codex
  `/responses/input_tokens` endpoint. Unsupported providers return `501` with
  `code:"input_token_count_unsupported"`. Count requests are best-effort
  preflight diagnostics and are not added to usage or cost aggregation.
- `POST /v1/compact` accepts `{target_id, request}` and returns
  `{context:{items,usage}}`. The proxy exposes `native_compaction:true` in a
  target's catalog metadata when its Responses provider config explicitly sets
  `responses_compaction:true`, or when the capability is unset for a managed
  provider with the canonical OpenAI API or ChatGPT Codex identity and base URL.
  An explicit `false` always wins; API shape alone is not treated as evidence
  for manual or compatible providers.
  The handler resolves model/tier/auth exactly as a stream does, calls the
  optional `ContextCompactor`, prices and records returned usage, and applies the
  same API-key cost budget. The Responses dialect uses a streamed
  `POST /responses` with a trailing `compaction_trigger` (v2) for both canonical
  first-party backends; explicitly enabled compatible providers retain the
  standalone `POST /responses/compact` (v1) contract. Unsupported targets return
  `501` with
  `code:"context_compaction_unsupported"`. Managed setup enables the capability
  for both first-party backends and leaves compatible endpoints opt-in.
- **Usage aggregation and model-proxy budgets.** The proxy keeps a mutex-guarded
  `{provider, model}` usage map and serves it read-only at `GET /v1/usage` as
  `{"instance":..., "since":..., "models":[{provider, model, requests, input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens, reasoning_tokens, cost_usd}, … ]}`, sorted
  by `provider:model`; `since` is the handler construction time. Because every
  priced `/v1/stream` attempt with usage is
  recorded, delegate child-agent spend that flows through the proxy is included,
  including failed attempts that streamed usage before the error. Cost budgets are
  per API key: `harness-model-proxy generate-api-key -budget-usd 25 -budget-period
  24h <name>` stores `cost_budget:{limit_usd, period_seconds}` on that key-file
  entry, with optional `reject_unpriced:true` from `-budget-reject-unpriced`.
  Budget state is persisted per key under the model-proxy config directory
  (`state/api_key_budgets/<key>.json`) via temp-file + rename, so restarting does
  not reset spend. Enforcement is quota-style: once the authenticated key's
  current-window spend is at or above `limit_usd`, new priced streams are rejected
  with `429 code:"cost_budget_exceeded"`, `Retry-After`, and `retry_after_ms`. The
  request that pushes spend over the limit completes and is charged after
  successful known-cost completion; failed or unpriced requests do not mutate
  budget state. Unpriced targets are allowed by default and only rejected with
  `400 code:"cost_budget_unpriced_target"` when that key sets `reject_unpriced`.
  When the authenticated key has a budget, `/v1/usage` includes
  `budget:{limit_usd, period_seconds, window_start, window_end, spent_usd,
  remaining_usd}`. API-key files (including budget metadata) are hot-reloaded.
  Both rollup and enforcement are per pod. Strict enforcement therefore requires
  one replica; a shared budget directory across replicas is unsupported because
  its read-modify-write protocol is not coordinated.
- **Prometheus metrics.** The proxy also exposes `/metrics` (Prometheus text
  exposition 0.0.4) on a **separate port** (default `127.0.0.1:9090`) with **no
  API-key auth**, so a scraper can reach it off the harness CLI path. Counter
  families — `model_proxy_requests_total`, `model_proxy_errors_total`,
  `model_proxy_input_tokens_total`, `model_proxy_output_tokens_total`,
  `model_proxy_cache_read_tokens_total`, `model_proxy_prompt_input_tokens_total`,
  `model_proxy_cache_write_tokens_total`, `model_proxy_cache_write_1h_tokens_total`,
  `model_proxy_reasoning_tokens_total`, `model_proxy_cost_usd_total`, and
  `model_proxy_request_duration_seconds_total` — are labeled by `provider`,
  `model`, bounded request `purpose`, and `key`, while the
  `model_proxy_build_info` gauge is labeled by `version` only.
  `purpose` is `turn`, `compaction`, `prewarm`,
  `branch_summary`, or `unknown`;
  missing and unrecognized client values normalize to `unknown` to prevent label
  cardinality growth. The `key` label is the authorizing API key's stored `Name`
  (stashed in the request context by the auth middleware) or the sentinel
  `"anonymous"` when auth is disabled. Token counters
  are recorded for every `/v1/stream` or `/v1/compact` request that produced
  usage, priced or not — a deliberate superset of `/v1/usage`'s priced-only
  cost rollup — while
  `cost_usd_total` is recorded only when the model's price is known.
  `model_proxy_prompt_input_tokens_total` is the write-inclusive denominator for
  deriving a token-weighted cache-read ratio from
  `model_proxy_cache_read_tokens_total`; per-request percentages are not averaged.
  `requests_total`/`errors_total` cover every `/v1/stream` and `/v1/compact`
  attempt, including ones
  that fail before the target resolves (malformed/oversized/unknown `target_id`,
  with `provider`/`model` omitted) and ones rejected by API-key auth (401); a
  client disconnecting mid-stream is not counted as an error. An empty label value
  is treated as absent (Prometheus semantics), so an omitted `provider`/`model`
  collapses to a single aggregate series rather than `provider=""`.
  Operational continuation/transport families are intentionally separate:
  `model_proxy_continuation_total{result}` has the bounded values
  `not_offered`, `served`, `unavailable`, `rejected_upstream`, and `failed`;
  `model_proxy_ws_pool_events_total{event}` has `hit`, `miss`, `create`,
  `evict_lru`, `evict_idle`, `evict_age`, and `overflow`; gauges
  `model_proxy_ws_pool_connections` and `model_proxy_ws_pool_capacity` expose
  current pooled entries and the configured bound. Exactly one continuation
  result is recorded per stream request. These families carry neither API-key
  nor instance labels; Prometheus scrape-target identity supplies the replica,
  and unknown enum inputs normalize to bounded sentinels.
  `-no-metrics` disables the endpoint — and, via a nil registry, the per-request
  recording itself — and `-metrics-listen` moves it (both override the config-file
  `metrics` object's `enabled`/`listen`; the default is enabled). When the listen
  address is set explicitly (flag or config) a bind failure is fatal rather than
  silently disabling the endpoint.
  Histograms are out of scope; `requests_total` + `duration_seconds_total` give an
  average. The exposition is hand-rolled via the reusable stdlib-only
  `internal/metrics` package; config/flag resolution, build-info setup, separate
  endpoint lifecycle, bind-failure policy, and explicit idempotent shutdown
  handles are shared by both proxy binaries, while metric-family registration
  remains service-specific. The model proxy keeps this endpoint alive until the
  API drain and handler/pool teardown finish.
- **Pricing staleness.** The `GET /v1/models` catalog response carries an optional
  `pricing` object — `{source_date, max_age_seconds, expires_at}`. `expires_at`
  is the earliest expiry among provider-direct and models.dev sources that
  contributed prices; clients prefer it when present. For a manual-only catalog,
  `source_date` is the newest modification time among configured provider files.
  Static context-tier schedules
  are included in `Target.Price`; only dynamic or route-dependent models may omit
  it even when request-time `cost_usd` can be calculated by a provider-specific
  pricer. Replicated production deployments pin one identical
  `models.dev.api.json` in the image or a read-only volume and set
  `models_dev_cache_ttl:0`; a catalog change is a deployment, not an independent
  per-pod refresh.
- **Selection rule:** `harness` fetches `GET /v1/models` from the proxy. Model
  selection resolves provider-qualified proxy target IDs, not arbitrary
  provider-local names. The target comes from `-model`, `HARNESS_MODEL`, config
  `model`, an agent override, or `/model`.
- `harness --check-model-proxy` reuses the catalog request as a bounded
  reachability check and exits before session creation, tool setup, hooks, model
  selection prompts, or `/v1/stream`.
- `internal/config` resolves only user-facing settings. Provider connection settings
  are resolved by `harness-model-proxy` from its config and environment.
- The optional config-file `mcp` and `lsp` blocks (proxy/local MCP and LSP servers)
  are documented with their subsystems in §15 and §15a.

## 8. Agent loop (`internal/agent`)

### 8.1 Prompt and turn loop

A **prompt** is one top-level user interaction. Within it, a **turn** is one
completed model response plus the complete tool-result batch requested by that
response. An **attempt** is one physical provider request for a turn; transport retries
and rebuilt compatibility requests increment its ID but do not create additional
turns. Turn numbers restart at 1 for every prompt. Model-backed maintenance such
as compaction and prewarming is neither a prompt nor a turn.

```
append user prompt message (origin=prompt)
for turn := 1; maxTurns <= 0 || turn <= maxTurns; turn++ { // default 0 (unlimited)
    stream := provider.Stream(ctx, request) // attempt 1; retryable failures increment attempt
    accumulate: print text deltas live; collect assembled tool calls;
                capture usage + stop reason
    append assistant message (text blocks + tool_use blocks, emission order)
    if stopReason == tool_use {
        resolve effective stages: _stage absent = inherit (starting at 1);
            otherwise require a positive, non-decreasing integer in emission order
        if any stage is invalid: return one invalid-args result per call
            without running hooks or tools
        suppress exact-duplicate calls within each stage beyond the first and
            all calls past the per-turn dispatch limit // guard errors, no run;
            // suppressed calls never become dependency targets or writers
        for each effective stage in ascending order:
            wait until every earlier stage has returned a result
            partition same-stage calls around SequentialTool inputs, matching tool
                hooks, and suppressed calls
            for each default-parallel island:
                add latest-writer dependencies for normalized mutation path keys
                    // suppressed calls never become dependency targets or writers
                run workers after their predecessors complete, bounded by the
                    per-agent concurrent-execution limit // Dispatch always returns a result
                emit summaries/results in model order
        append ONE user message carrying all tool_result blocks, in call order
    }
    emit turn_complete(prompt, turn)
    if stopReason == max_tokens && no output continuation has been requested &&
       another paid request fits the explicit turn/token/cost budgets {
        append one internal user continuation instruction
        continue // ordinary pre-request compaction still applies
    }
    if stopReason != tool_use { break }
}
run post-prompt maintenance if needed
emit prompt_usage(prompt, completedTurns)
```

After a live REPL prompt terminates with a non-cancelled provider `*APIError`,
the host may call the same loop through `ContinuePromptWithContext`. That entry
point starts at the current validated, closed transcript boundary and appends no
message before the provider request. The REPL's `/continue` path assigns a new
prompt/accounting ID and restores the original request-only prompt context;
dynamic sink-provided context is still sampled on every request. Stateful
Responses/Interactions continuation and their full-history fallback therefore
follow the ordinary `modelRequest` rules rather than a dialect-specific recovery
path.

`EventSink` remains the required rendering contract. `SteerDeliveredSink` is an
optional sink interface: after a queued in-prompt steer is appended as a
validated `MessageOriginSteer` user message, the agent calls `SteerDelivered` so
TTY sinks can distinguish admission from actual delivery. It is not called for a
failed validation rollback, a disabled steer, or input recovered for the next
prompt.

- **Staged, dependency-aware tool execution.** `_stage` is a positive integer whose
  effective value starts at 1, is inherited when omitted, and must be non-decreasing
  in model emission order. A later stage waits for result completion of every earlier
  stage, but an error result does not suppress later stages. Within a stage, every
  registered call is parallel-eligible unless its tool's input-aware `SequentialTool`
  opt-out applies; unknown calls also join islands because they can only return an
  error. Matching `PreToolUse`/`PostToolUse` hooks remain per-target barriers. Within
  each island, the scheduler scans calls in emission order and tracks the latest call
  for every normalized `FileMutationReporter` key. A call depends on each distinct
  latest predecessor touching one of its paths, then becomes latest for all its paths.
  Thus overlapping `write`/`edit` calls, including multi-file calls and lexical
  relative/absolute aliases, cannot overtake one another while unrelated calls run.
  A timeout or cancellation result ordinarily releases the next stage; a reported
  mutation with a conflicting later-stage mutation additionally waits for the earlier
  execution goroutine's actual return before releasing that successor. Workers never
  call `EventSink` or mutate the rich-result budget: starts, results, progress cleanup,
  diffs/notices, usage, and transcript blocks are processed by the parent in emission
  order. All stages are part of one assistant turn and yield one ordered user result
  message. `parallel_tool_batches` records a complete ordered same-stage island only
  when its graph permits concurrency; fully chained islands omit metadata. A background
  launch receipt completes its call and stage but means queued, not finished. Hidden
  read/write, shell, git, symlink, hard-link, MCP, and LSP effects remain best-effort;
  use later stages when known calls require earlier side effects, `shell.steps` for a
  serial command list inside one call, background leases/jobs for detached work, or a
  separate model turn when later arguments depend on earlier output. No sandbox is added.
- **One result per call, always.** Required by both APIs (§4 invariant). `Dispatch`
  produces a result even on panic.
- **Metered tools:** tools may optionally report token usage (currently synchronous
  `delegate`). The agent adds that usage to the prompt/session total, while the normal
  tool result remains the only child output added to the parent transcript.
- **File diffs:** unless `show_diffs` is disabled, the agent asks built-in file
  mutation tools for their affected paths. Each worker snapshots immediately before
  its call and snapshots/renders immediately after dispatch, before releasing any
  same-path successor. The parent later emits the captured unified diff after the
  normal tool summary in emission order. The same capture/emission helpers serve
  sequential calls. The diff is generated by a stdlib-only line renderer, not by
  repository `git diff`, so it works in non-git projects and shows incremental
  per-call changes when the same file is edited repeatedly. Displayed
  diffs are colored by `internal/term/highlight` with the selected immutable
  dark/light palette: subdued red/green line backgrounds, deterministic truecolor
  `+`/`-` sigils, and content syntax-highlighted in the mutated file's language
  (terminal-default content for unknown languages). Old/new lexer states are
  independent, and a file-boundary reset retains the selected palette. The tint
  spans the full terminal row via erase-to-EOL under the active background
  (BCE) rather than padding spaces, so window-shrink reflow has nothing to
  wrap, stripping ANSI recovers the original bytes, and terminals without BCE
  show a text-width tint.
- **Background jobs:** tools with `background:true` start process-local jobs and
  return a job id immediately. `delegate` uses the same flag for background child
  agents. Local jobs carry canonical resource/access leases: read-only leases may
  coexist, while exclusive access conflicts with every unfinished lease on the
  same resource. Completed job summaries are delivered once as request-only
  context on a later parent model request; they are not appended to the parent
  transcript.
- **Max-turns guard:** `max_turns` defaults to `0` (unlimited). When it is
  positive, on hit print `[stopped: reached max turns (N)]`, keep the transcript
  (it is valid — the last turn's results are appended), and return to the prompt.
  A non-positive value disables this guard. The interactive `/max-turns` command
  changes the value for subsequent prompts in the current REPL session. One turn
  before a positive limit the loop injects a one-shot RoleUser wrap-up steer
  ("stop calling tools now and reply with a final message").
- **Structured termination:** every `PromptUsage` carries exactly one loop
  termination reason: `model_completed`, `turn_limit`, `token_limit`,
  `cost_limit`, `repeat_guard`, `error_guard`, `cancelled`, or `error`.
  Prompt replay events and delegate metadata persist it. The reason records
  loop control only; acceptance criteria and repository state remain the task
  completion oracle.
- **Intra-turn call sanity guards (`internal/agent/callguard.go`).** The
  turn-level and failure guards below react only after damage is observable
  across turns or failures; a single degenerate response can emit hundreds of
  identical or excessive calls at once (observed: a model repetition loop that
  streamed the same test command 2,252 times until the output-token cap, whose
  unbounded parallel dispatch exhausted the machine's process table). Before
  any dispatch, each turn's call set is planned against two limits:
  - *Exact duplicates within a stage.* Duplicate identity is tool name +
    normalized input hash (the failGuard key). The first occurrence runs; later
    identical calls in the same stage are suppressed. Suppression is
    stage-scoped because an identical call re-issued in a later stage is a
    legitimate re-run after mutations, while identical calls within one stage
    run concurrently and can only multiply load.
  - *Per-turn dispatch limit.* Surviving calls beyond 128 dispatches per turn
    (emission order) are suppressed; the model is told to split the work across
    turns.
  Suppressed calls are never run: like the invalid-stage preflight path they
  skip hooks and tools and return `blocked`-kinded guard errors, so the
  transcript stays closed and the result text steers the model; one summary
  notice is recorded per suppressed turn. Suppressed calls produce no side
  effects, so they never become cross-stage dependency targets or latest
  writers for their mutation keys. Independently, parallel dispatch is bounded
  by a per-agent semaphore of 32 concurrent tool executions — large legitimate
  batches still complete, just not all at once — and the bound is per-agent so
  a delegate child can never deadlock against permits held by its parent's
  dispatch.
- **Runaway guards (`internal/agent/loopguard.go`).** A per-run `turnGuard` (loop
  frame only, never on the shared registry) watches each tool turn:
  - *Repeated identical calls.* Each turn's call-set is reduced to an
    order-insensitive signature of `name + canonical(JSON input) + result`. After
    3 identical signatures in a row it injects one RoleUser steering message; at 8
    it hard-stops with `[stopped: N identical tool turns repeated with no change]`.
  - *Repeated shell-command families.* A single foreground `shell` shell
    turn is also fingerprinted by its working directory plus the command text before
    the first unquoted pipe. Four consecutive turns with the same pipeline head
    inject one steer even when downstream `grep`/`sed`/`awk` stages and results keep
    changing. At twelve it hard-stops under `repeat_guard`. A different base command,
    a non-command or multi-command turn, background execution, or user steering
    resets the streak. This intentionally does not group argv calls or commands
    without a pipeline, keeping the heuristic narrower than exact-repeat detection.
  - *Error storm.* It counts consecutive turns in which **every** tool result is an
    error. At 5 it steers ("re-read the latest error output and change your
    approach"); at 10 it breaks with `[stopped: N consecutive tool turns all
    failed]`. (All guard steers share one slot, so a turn is nudged at most once.)
  - *Bounded activity telemetry and advisory semantic guards.* Each completed tool
    turn is classified as inspect, mutate, verify, wait, coordinate, or other.
    Batched schemas contribute their operation count rather than looking like one
    operation. A successful mutation, verification attempt, successful wait, or
    successful coordination is explicit progress. Result fingerprints are held in
    a 256-entry FIFO set solely to mark bounded new evidence. Three consecutive
    one-call/one-lookup turns inject a batching steer; twelve consecutive
    inspection-only turns without explicit progress inject a phase-transition
    steer. These advisory streaks have no hard-stop condition, and a user steer
    resets them so the user's new direction gets a fresh window. The resulting
    `turn_progress` event is diagnostics-only and never enters model history; it
    includes both exact-repeat and shell-command-family streaks.
  - *Repeated identical failures (`internal/agent/failguard.go`).* A per-prompt
    `failureGuard` on the Agent (never on the shared registry) keys each
    dispatched call by tool name + normalized input hash and counts consecutive
    failures with the *same error text* — a fix-and-rerun loop whose errors
    differ never trips it. The 2nd identical failure appends a steering hint to
    the error; the 3rd identical attempt is hard-blocked before dispatch with a
    `blocked`-kinded error (blocked attempts are not recorded, so the streak
    does not grow while blocked). Any successful call reporting mutated paths
    resets the whole map; read-only successes deliberately do not. Guard state
    is created and dropped inside `RunAdmittedPromptWithContext`, so a fresh
    prompt always starts clean, and the map is mutex-protected because default-
    parallel workers consult it concurrently. Result folding is serialized in
    emission order so completion timing cannot change warnings or mutation resets.
  - *Prompt-token budget.* When `-max-prompt-tokens` is positive, before each next
    (paid) model request it compares the prompt's cumulative usage
    (input + cache-read + cache-write + output + reasoning) against the budget and
    breaks with `[stopped: prompt token budget N exceeded]`. `0` is unlimited. This
    path deliberately skips the final summary — the point is to stop spending.
- **Graceful wrap-up on hard stop.** The error-storm, repeat-loop, and
  max-turns-reached breaks (but not the token-budget break) end with one final
  request that has `Tools` cleared, so the model produces a text-only wind-down
  appended as an assistant `final_answer` message instead of leaving a dangling
  `tool_result`. It is best-effort: a failed or empty summary leaves the
  already-valid transcript untouched, and any tool calls the model emits there are
  ignored.
- **Bounded output continuation:** a normal response ending in `max_tokens` keeps
  its partial assistant message and automatically requests exactly one continuation
  from the stopping point, without repetition, when the next paid request remains
  inside the explicit turn, prompt-token, and prompt-cost budgets. The continuation
  instruction is an internal user message. Ordinary pre-request pressure checks may
  compact before it. A second `max_tokens` stop, or a blocked continuation budget,
  ends the prompt with the visible stopped notice. A stop-sequence finish still ends
  immediately with its own notice.
- **Mid-stream retries:** each request shape is wrapped in `streamWithRetry`, which
  re-requests the step from scratch on a retryable terminal stream error up to
  `streamRetries` (2) times. These attempts do **not** count against `max-turns`;
  failed-attempt usage is still billed and tracked (xref §5.5). A per-turn
  coordinator carries the next physical attempt ID and discarded usage across
  request-shape rebuilds while giving each rebuilt request its own retry allowance.
- **Compatibility fallbacks:** context-overflow rebuilds, stored-response rejection,
  an unavailable `previous_response_id`, and rejected encrypted reasoning each
  discard the prior physical attempt before one rebuilt request. Attempt IDs stay
  monotonic, and discarded billed usage is included once in the turn/prompt total
  and separately reported as wasted. Previous-response rejection resets the stored
  Responses state and resends the full context.
- **Stop hook:** a configured `Stop` hook fires once before an admitted prompt
  returns, including canceled prompts. At a normal model stop it carries
  `can_block:true` only when turn, token, and cost budgets permit another request,
  and may then block the break to force that turn. Provider failures,
  cancellation, exhausted hard budgets, and other terminal paths that cannot
  continue emit the same hook with `can_block:false` and ignore blocking output.
  Canceled prompts run the terminal notification in a detached context bounded
  to ten seconds. `stop_hook_active` guards the normal continuation path, while
  the terminal fallback covers exits that never reached it (`agent.go`).
- **In-prompt steering (default on; `-no-steer` off).** Input the user submits
  with Enter while a prompt is running is injected as a `RoleUser` message
  before the *next* model request (i.e. between tool rounds — the next time
  harness sends data back to the model), rather than waiting for the prompt to
  end. Only model-bound input is steered; `!shell`, `/commands`, and `/edit`
  submitted during a prompt stay queued for the next prompt. A steer does not
  consume a `max_turns` slot (it rides on the next request the loop was already
  going to make) and resets the runaway-guard streaks, so a deliberate redirect
  is not penalized for the repeat/error run that preceded it. If the prompt ends
  without another turn to inject into (a final answer or budget/cancel break),
  the unconsumed steer is recovered at prompt completion and run as the next
  prompt, so the input is never lost. `^C`/Esc-Esc cancel the in-flight prompt as
  usual; steering changes nothing about interrupt handling (§8.4).

### 8.2 Tool failure handling

`Dispatch` never lets the loop crash. Each failure mode becomes an `is_error` result
fed back to the model so it can self-correct. Internal result text deliberately
omits an `error:` marker because the error bit already carries that information;
the terminal renderer adds one lowercase `error: ` marker, while provider adapters
apply their wire-specific representation (§4 wire mapping).

| Failure | Internal result text |
|---|---|
| unknown tool name | `unknown tool "<name>"` |
| JSON type mismatch | `invalid arguments: invalid value for "<field>": expected <JSON type>; got <JSON type>` |
| invalid JSON syntax | `invalid arguments: invalid JSON at byte <offset>: <detail>` |
| malformed streamed tool-call args | `invalid tool call arguments for <name>: <detail>` plus a per-tool corrective hint: a truncated (unterminated-JSON) `write`/`edit` call is told to use a smaller `write`, then append with `edit` |
| tool returned error | `<message>` |
| tool panicked | `tool panicked: <recovered>` (also logged to stderr) |
| tool exceeded the dispatch timeout | `tool timed out after <dur>` |

`json.UnmarshalTypeError` and `json.SyntaxError` are recognized centrally, including
when wrapped. Type errors are translated into JSON terminology (for example,
`"args"` expected an array of strings but got a string) rather than exposing Go
struct/type details. Tool-specific semantic validation continues to use concise
`badArgs` messages under the same `invalid arguments: ` prefix.

Every `is_error` result also carries a diagnostics-only `ErrorKind`
(`llm.ToolErrorKind`) that, like `Metrics`, never enters model-visible content.
The dispatch/agent layers stamp `unknown_tool`, `invalid_args`, `timeout`,
`cancelled`, `panic`, `hook_blocked`, `blocked` (workflow and repetition guards),
`unsupported_modality` and `invalid_result` directly; historical records may
also contain `regex_invalid`. A tool declares a kind by wrapping its error with
`tools.WithKind` (used for `edit_oldtext_not_found` /
`edit_oldtext_ambiguous`), and the delegate tool stamps transient child
failures as `rate_limited` / `provider_error`. WithKind leaves the error
message unchanged. Outer cancellation is classified separately as `cancelled`. The
session recorder persists the kind plus a bounded, rune-safe `error_excerpt`
(2 lines / 240 runes) on failed `tool_result` events. Offline analysis
(`harness session stats`, `harness session errors`) uses the stored kind when
present and otherwise classifies legacy logs from the recorded display line,
additionally producing `path_not_found`, `regex_invalid`, and `other`; failed
`model_request` events are mapped from their structured status/code to
`rate_limited`, `provider_overloaded`, `provider_internal_error`,
`provider_auth`, `provider_request`, `provider_5xx`, or `provider_error`.
`shell` non-zero exits stay in-band results, not tool errors (§9.7–9.8),
so they never get an error kind; their diagnostics metrics feed the separate
command-failure and effective-failure summaries.

**Per-tool dispatch timeout backstop (`-tool-timeout`, default 1800s, `<=0`
disables).** `Dispatch` runs each tool under a derived `context.WithTimeout` so a
hung tool that ignores cancellation normally cannot stall a turn; on expiry it
returns the `tool timed out after <dur>` error result above. Registry dispatch
also exposes an actual-goroutine completion signal to the conflict scheduler. If
a later same-path mutation depends on the timed-out call, that successor remains
queued until actual completion so it cannot overtake a context-ignoring mutator;
this narrow safety case may outlive the result timeout. The ceiling applies to
both sequential and parallel scheduling. A tool that reports its own deadline
via `SelfTimeouter` only **raises** the ceiling, never lowers it, so
`shell`'s `timeout_seconds` stays authoritative and, while the backstop is
enabled, synchronous `delegate` calls raise it to two hours. Background
delegates return a launch receipt immediately and run with no manager-imposed
wall-clock deadline. An outer cancellation (`^C`) is reported as cancellation,
not a dispatch timeout.

### 8.3 Output truncation

`Dispatch` applies per-result caps. The global defaults are **64 KB or 1000
lines**, configurable with `tool_result_max_bytes` and `tool_result_max_lines`,
or env `HARNESS_TOOL_RESULT_MAX_BYTES` and `HARNESS_TOOL_RESULT_MAX_LINES`;
per-tool settings may replace either default. On each result-limit axis where the
corresponding global cap is not configured, the `read` tool installs its own
default: a 1000-line window plus 64 KB and 2000-line result caps. Its per-tool settings are
`read_default_limit`, `read_result_max_bytes`, and `read_result_max_lines`; the
bounded exact-line-count scan used by its pagination notice is configured with
`read_total_lines_max_bytes`. The first cap hit
adds a teaching marker:

```
[truncated: showing first 1000 of 4213 lines; use read with offset/limit or shell to narrow]
```

Individual tools may also apply their own natural limits. The resolved global or
per-tool Dispatch cap remains the output backstop for each result. When that cap
further clips a `read` result already carrying a file-pagination notice, Dispatch
preserves its optional SHA header when it fits, keeps complete numbered lines,
and rewrites the file-aware notice to the actual continuation offset instead of
adding the generic marker. If no complete line fits, it emits a file-aware
`before line` notice with targeted-shell guidance. Truncated results still carry
metadata so the UI can warn and write
the full output to the session's `artifacts/tool-results/` directory. When an artifact
is written, the model-visible result includes the absolute artifact path and advises
using `read` with `offset`/`limit` or a targeted `shell` command for inspection. Foreground tool
results and completed background-job context share the registry's result preparation
and the same archive-hint formatter, including per-tool limits, so this recovery
behavior stays consistent between execution modes.

A tool that implements `ResultTool` may supply separate concise `Text` and full
`OriginalText`. `Dispatch` caps the concise text normally and marks the supplied
original for this same artifact pipeline. This is used by `shell.steps`:
successful verification output remains recoverable without entering live model
context.

### 8.4 Interrupts

A single SIGINT handler plus a per-prompt `context.CancelFunc`:

- **^C during a prompt** → cancel the prompt context (aborts the HTTP stream; kills
  `shell` process groups). Apply the cancel repair rule (§4): keep streamed
  partial text, strip un-executed tool calls. Print `[cancelled]`, return to prompt.
- **Esc-Esc during a REPL prompt** → same prompt cancellation as the first ^C, without
  the second-^C exit behavior.
- **Second ^C within ~1 s, or ^C at the idle prompt** → save session, print the
  session token summary, exit 130.
- **^C during startup or helper-command network work** → cancel the in-flight
  request and exit 130. `session replay --follow` uses its own context on the
  early helper-command path and follows the same exit rule.
- **^D at the prompt** → save session, print the session token summary, exit 0.

### 8.5 System prompt (`internal/sysprompt`)

`system = staticPrompt + envContext + AGENTS.md + skills + runtimeHints + agentPrompt`

- **Builtin instructions** (`prompts/system.txt`): concise agentic-coding guidance — read before
  editing; emit all currently known calls in one tool turn; `_stage` orders separate tool
  calls: assign independent calls the same `_stage` and dependencies increasing stages
  (omissions inherit from stage 1); prefer one shell call's `steps[]` for an ordered run of
  shell commands (serial with stop_on_failure, one receipt), reserving increasing stages
  for ordering across different tools; rely on automatic same-file write/edit sequencing;
  defer calls with output-dependent arguments to the next model turn; run focused
  verification; and stop when done.
- **Environment context**, computed at startup:

  ```
  Environment:
  cwd: /Users/twt/project
  os: darwin/arm64
  date: 2026-06-09
  git: branch=main
  ```

  Git summary uses `git branch --show-current`; live dirty-state counts are
  omitted because they become stale after the first edit. It renders
  `git: (not a git repository)` outside a work tree.
- Flag/config override: `-system-prompt <text|@file>`,
  `HARNESS_SYSTEM_PROMPT`, or config `system_prompt` replaces only the static
  built-in instructions. Runtime sections such as env context, user/project
  `AGENTS.md`, skills, and agent prompts are still composed around it.
  `~/.agents/AGENTS.md` is appended before the current working directory's
  `AGENTS.md`; missing files are ignored and other read failures fail startup.
  The skills catalog is rendered as `## Skills` / `### Available skills` (`name: description (file: /abs/SKILL.md)`) / `### How to use skills` with discovery, trigger (`$Name` or matching description, multiple mentions → all), missing-skill, progressive-disclosure (read `SKILL.md` completely via `read`, resolve relatives from its directory, read only routed `references/`, prefer scripts), and coordination guidance; within the always-resident catalog each description is the first sentence or 160 runes and a character budget equal to 2% of the startup model's effective context window fair-shares remaining chars round-robin (omitting extras with an omission notice); unknown windows fall back to 8,000 characters. Unlike Codex's diffed in-history developer-message catalog, Harness deliberately keeps this catalog in the static system prompt: discovery is fixed at startup, so the prefix remains byte-stable and survives compaction without putting system instructions in transcript history. Process-specific `rg`/LSP/Serena hints follow it. When ripgrep is on `PATH`, a short runtime hint recommending
  `rg` / `rg --files` over `grep` for text and file search is appended to
  `RuntimeHints`. The
  active agent prompt is always the final section.
  `@~/path` expands through the current user's home directory; relative `@file`
  references in the config file resolve from that config file's directory.
  `-no-env` drops the env block.
- Explicit mentions are resolved before provider work: `$skillName`, exact
  word-bounded plain names, and `skill://` / `…/SKILL.md` path mentions
  (punctuation-tolerant, `$$` escapes to literal `$`) each select their skill.
  Harness reads the complete `SKILL.md`, wraps it as
  `<skill>\n<name>…</name>\n<path>…</path>\n\n{body}\n</skill>`, and prepends
  the block to the user's text. Multiple selected skills are separated by a
  blank line and deduplicated within that prompt. The resulting text is admitted
  as one ordinary user-role message and persisted in transcript history; it is
  neither request-only context nor reinjected on later requests. It remains in
  history until ordinary compaction summarizes or removes it. Re-mentioning the
  skill in a later prompt reads the current file and persists a new block; if a
  selected skill's `SKILL.md` cannot be read (for example the directory was
  removed mid-session), the read failure is non-fatal: Harness prints one
  stderr warning per unreadable skill naming the skill and its catalog path
  (`warning: skill not injected: read skill …`), omits that skill's block, and
  still runs the prompt; readable skills mentioned alongside it are injected
  normally and `skill_activation` telemetry counts only blocks actually built.
  Discovery remains fixed at startup, so a stale catalog entry keeps matching
  until restart. Unknown skills are a different failure class: a standalone
  `$typo` mention still prints `unknown skill` and rejects the prompt. Discovery
  scans existing `.agents/skills` roots from the Git project root through
  `cwd`, outer to inner, then the user root; project scope beats user scope and
  the nearest project root wins equal-scope name collisions.
- Implicit skill selection is plain progressive disclosure: the model sees the
  compact catalog and reads `SKILL.md` with the ordinary `read` tool. Complete
  and partial reads remain ordinary persisted, line-numbered tool results,
  subject only to the generic tool-result truncation and archival policies.
  Harness does not intercept them, replace them with activation receipts,
  digest their bodies, pin them in request context, or force archival. The
  catalog's "don't carry across turns" rule is prompt guidance only; when a
  later turn needs the skill, the model re-reads `SKILL.md`. Explicit injections
  emit `skill_activation` telemetry with source `explicit` and summary
  `injected`; implicit reads remain visible in normal tool-read statistics.
  Catalog omission and truncation counts are emitted as
  `harness.skill.catalog_omitted` and `harness.skill.catalog_truncated` when
  OpenTelemetry is enabled; startup also prints a concise budget warning.

## 9. Tool set (`internal/tools`)

The tool contract lives in `internal/tools/tool.go`. `Tool` is the required
core — `Name`, one-line model-facing `Description`, hand-written JSON-Schema
`Schema`, `ReadOnly(input)` policy classification, and `Run` — and `Registry`
is the ordered map with `Register`, `Specs`, `Dispatch`, and
`DispatchWithCompletion`. Optional satellite interfaces, detected by type
assertion at dispatch:

| Interface | Implement when | Effect |
|---|---|---|
| `RichTool` | the tool can return supplementary image content | dispatch prefers `RunRich` over every other execution path |
| `ResultTool` | the tool proactively summarizes successful output | `RunResult` separates concise model text from the archivable original; preferred over `MeteredTool` and `Run` |
| `MeteredTool` | `Run` consumes model tokens internally (e.g. delegate) | dispatch preserves `Usage` for prompt/session totals |
| `RequiredInputModality` | the tool proactively requires a non-text input modality | capability declaration kept separate from `RichTool` for dynamically rich MCP tools |
| `ProgressStarter` | `Run` may block behind a child run | surfaces live activity to the parent wait ticker |
| `SequentialTool` | specific inputs are ordering-sensitive | opts those inputs out of default-parallel dispatch |
| `FileMutationReporter` | mutated paths are knowable from the input | the agent orders overlapping mutations; optional before/after diffs |
| `FileReadReporter` | read paths are knowable from the input | call-level read telemetry |
| `InputTrimmer` | inputs embed whole file bodies (`write`, `edit`) | retention stores a compact receipt instead of the bulky input |
| `BackgroundJobStarter` | the tool can hand work to the background job manager | detached execution with manager-owned ids, status, and notices |
| `BackgroundJobDiagnosticIdentitySetter` | a background starter retains launch attribution | parent/delegate sinks record execution identity without importing the manager |
| `SchemaDescriptionPreserver` | a first-party tool carries budgeted schema guidance | `Registry.Specs` keeps its descriptions instead of stripping them |
| `SelfTimeouter` | the tool enforces its own per-call deadline | the dispatch ceiling only raises to that deadline, never lowers it |

- **Top-level model-facing descriptions are a single 80-byte functional minimum**
  (suffix-inclusive via `Registry.Specs`). First-party tools also opt into concise
  parameter descriptions, each at most 80 bytes. Parameter prose is reserved for
  behavior JSON Schema cannot express directly: defaults, ordering, cross-field
  exclusions, side effects, units, and selection guidance. It does not restate
  property names, types, enums, or requiredness. Operational detail lives in tool
  errors and this document. **Schemas are hand-written JSON Schema constants.**
  `Tool.Schema` remains the implementation contract for tool-owned fields.
  `Registry.Specs` removes `description` by default, which bounds arbitrary custom
  and MCP schemas; first-party tools implement `SchemaDescriptionPreserver` to retain
  their budgeted guidance. Other annotation keywords (`title`, `$comment`, `example`,
  `examples`) are always removed from actual schema nodes while validation keywords
  and property names that happen to equal annotation keywords remain intact. The
  registry then injects the optional positive-integer `_stage` property at the root
  of every model-facing local function-tool object schema.
  `_stage` is reserved Harness metadata: a custom, MCP, or LSP schema cannot override
  its root definition, and execution copies are stripped before any tool adapter sees
  them. Provider adapters receive the augmented `Request.Tools` schemas normally and
  do not interpret stages. Provider-hosted `Request.ServerTools` are excluded because
  they do not use the local registry or scheduler. `delegate` additionally retains
  its bounded dynamic compatible-agent catalog. Enums and requiredness still deserve
  hand tuning, and reflection fights you on exactly those fields.
- **Tools self-validate args** after `json.Unmarshal` into a private struct (no stdlib
  JSON Schema validator; unknown extra keys are tolerated — models hallucinate them).
- **Optional execution seams have one strict preference order:** rich, result,
  metered, then legacy `Run`. Dispatch invokes exactly one path. Rich children are
  validated before they enter a transcript; errors and centrally rejected results
  have content cleared. `RequiredInputModality` is a separate static capability signal,
  so a tool can require image input regardless of whether all successful calls are rich.
- **Semantic emptiness is explicit metadata.** A successful rich/result tool may set
  `Useless` when it produced no evidence (for example an empty MCP response or a
  timed-out wait). Dispatch preserves the visible result and records
  `ContentBlock.ResultUseless`; provider adapters ignore the field. Compaction
  preparation alone may replace the result and matching tool input with bounded
  placeholders. Errors are never marked useless.
- **Read-only classification is per call and independent from scheduling.** Static
  read-only tools ignore the input; argv-style tools can parse their arguments and
  return true only for safe subcommands (for example `git status`, `git diff`,
  `git log`). `ReadOnly` remains policy, retention, activity, and diagnostics metadata;
  it is not a concurrency gate.
- **Scheduling is opt-out.** Ordinary and unknown calls are parallel-eligible.
  `SequentialTool` may classify only concrete ordering-sensitive inputs as barriers;
  empty input is normalized to `{}` first. `FileMutationReporter` serves both diff
  observation and conflict scheduling. `MutatedPaths` preserves caller labels, while
  scheduling keys are deduplicated lexical `filepath.Abs` + `filepath.Clean` identities.
  Invalid/unreported calls have no keys; symlink and hard-link aliases are best-effort.
- Relative paths resolve against the process cwd. No path restrictions — the harness is
  honest about its no-sandbox assumption.
- **Registry surfaces are explicit.** `DefaultWithOptions` registers `read`,
  `view_image`, `edit`, `write`, `shell`, and `web_fetch`, in that order.
  `CatalogWithOptions` adds host-backed `git` and `git_readonly` when available
  plus `write_tmp_file`; agent coordination and discovered MCP/LSP tools are
  layered on separately. The removed names `read_file`, `write_file`,
  `list_dir`, `glob`, `grep`, `rg`, and `apply_patch` are not constructible and
  an `allowed_tools` entry naming one fails validation.
- **Large optional surfaces are lazy.** When the aggregate name, description, and
  model-facing schema bytes for an agent's MCP/LSP tools exceed 32 KiB, the CLI
  installs `tool_catalog`, filters those optional schemas out of `Registry.Specs`,
  and records an immutable deferred tool inventory. MCP tools group by server;
  native LSP tools group under `lsp`; each complete integration remains one
  group regardless of its tool count. The tools remain registered and
  allowlist-checked, and function names remain the exact local dispatch names.
  The neutral request carries both group metadata and the local fallback name.
  On the canonical OpenAI Responses API for GPT-5.4+ models except GPT-5.4 Nano,
  explicitly for GPT-5.3 Codex Spark, and on the canonical ChatGPT Codex
  Responses backend used by `openai-codex` with the same Nano exclusion, requests
  lower each group to one
  `namespace` tool containing `defer_loading:true` functions plus
  `{"type":"tool_search"}`, while omitting `tool_catalog` and activated
  duplicates. `responses_tool_search:false` opts out; `true` opts any compatible
  Responses endpoint/model in and overrides the default model gate. On the
  canonical Anthropic API, supported Claude 4.5+ and 5-family requests flatten
  every complete group into top-level custom tools marked `defer_loading:true`
  and append the non-deferred BM25 search tool.
  `anthropic_tool_search` can select `auto`, `bm25`, `regex`, or `off`; explicit
  BM25/regex also opts a compatible endpoint in. Anthropic's hosted search blocks
  and OpenAI's hosted items are persisted in original order for same-domain exact
  replay, while only selected ordinary function/tool calls enter local dispatch.
  Before any output, a structured status-400 Responses `invalid_request_error`
  whose parameter is exactly `tools`, from either HTTP or a WebSocket error frame,
  triggers one retry with native search disabled and the same neutral inventory
  exposed through `tool_catalog`. The per-model downgrade is retained on that
  provider instance; no provider-message matching or post-output retry is used.
  Anthropic does not perform this rejection downgrade. All other request paths
  use the same inventory through the local
  catalog: `list` and `describe` are read-only; sequential `activate` makes
  at most 16 selected schemas visible on the following request. The agent detects
  this spec change after dispatch, invalidates continuation/cache state, and
  rebuilds the request. Activation persists only for that registry lifetime.
  Native and local search are prompt optimizations, not authorization boundaries.

### 9.1 `read`

> Read a file; a directory lists entries.

| param | type | notes |
|---|---|---|
| `path` | string, required | file or directory to read |
| `offset` | int | 1-based starting line |
| `limit` | int | max lines, default 1000 or `read_default_limit` |
| `include_sha256` | bool | prepend the SHA-256 of the complete regular file; default false |

- A directory path returns a directories-first, non-recursive listing capped at
  200 entries, repairing an accidental file/directory mismatch in one call.
- **Parameter aliases (accepted silently; intentionally *not* in the schema):**
  `path` also accepts `file`, `file_path`, `filePath`, `filename`, `filepath`,
  `absolute_path`, and `target_file` — the spellings other harnesses use — so a
  model that emits another spelling succeeds on the first call. The canonical
  `path` wins when both names are set.
- Output is line-numbered (`cat -n` style: right-aligned number, tab, line) so
  `edit` targeting and grep cross-referencing stay reliable. Files are streamed
  line-by-line and output collection stops after the requested/default window,
  so memory is bounded by the window and longest line regardless of file size.
  A truncated regular file at or below `read_total_lines_max_bytes` (1 MiB by
  default) is then scanned to EOF without retaining its body so the notice can
  report an exact total line count. Larger files do not receive that extra scan.
- **Not-found suggestions:** an ENOENT failure appends
  `similar existing paths: <up to 3>` from a bounded same-directory
  name-similarity scan, plus a one-level parent scan when the directory itself
  is missing; the wider fallback is operational detail covered in tools.md.
- **Truncation notice:** a read cut off at its line window reports the exact stat
  byte size for ordinary regular files. Files without a trustworthy stat size,
  such as zero-size virtual regular files that yield content, report `file size
  unknown`. Ordinary regular files within the total-line scan threshold end with
  `[file truncated at line N of TOTAL; file size BYTES bytes; continue with
  offset=N+1]`; larger files omit `of TOTAL`. This lets the model coissue known
  remaining windows without forcing a full scan of very large files. Binary
  sniff: first 8 KB
  containing NUL → `error: <path> appears to be binary`. Offset past EOF →
  error stating the file's line count. Empty file → `(empty file)`.
- `include_sha256` makes one additional cancellable full-file pass and prepends
  `[sha256:<lowercase hex>]`. It is intentionally opt-in so a bounded read does
  not normally hash a multi-gigabyte file. Directory listings remain unhashed.

### 9.2 `view_image`

> Attach a local PNG, JPEG, WebP, or GIF image to inspect.

| param | type | notes |
|---|---|---|
| `path` | string, required | local PNG, JPEG, WebP, or non-animated GIF |
| `detail` | string | `auto`, `low`, `high` (default), or `original` |

- Read-only. Unknown JSON keys are tolerated. The result contains a bounded,
  basename-only text receipt plus exactly one rich image block; terminal summaries,
  hooks, artifacts, and diagnostics never contain its base64 payload.
- Shared `internal/inputimage` validation checks declared MIME and decoded format,
  preserves already-validated base64, extracts dimensions, rejects animated GIFs,
  and accepts only bounded regular files. Descriptor reads are cancellable, stop at
  10 MiB plus one detection byte, and reject directories, devices, and FIFOs.
  Each image also has a derived encoded ceiling; the 32 MiB encoded aggregate covers
  the complete retained request, including user and nested tool-result images.
- The tool declares the `image` input modality independently of rich-result support.
  The agent checks the current model immediately before dispatch, so `/model` changes
  take effect without rebuilding the registry and unsupported calls perform no file I/O.

### 9.3 Repository discovery and search

- There are no dedicated callable directory, glob, or repository-content search
  tools. Built-in agents use `read` for a known file or one non-recursive
  directory and `shell` with host commands such as `rg`, `rg --files`, `grep`,
  `find`, and `ls` for recursive discovery and search. Prefer argv form when
  quoting is unnecessary; use `command` only for shell syntax and `steps[]` for
  ordered commands.
- Three consecutive turns containing one unbatched orientation lookup trigger
  one soft steering message recommending coissued `read` calls or batched
  repository lookups in one `shell` call. It does not block execution.
- Host command semantics remain authoritative. Harness does not inject search
  flags, clamp search lines, parse search context, or reinterpret no-match exit
  status. As with every `shell` call, non-zero exit is model-visible command
  output rather than a tool error. A background search is still a `shell` job and
  defaults to an exclusive cwd lease unless the caller explicitly marks the
  command's lease `read_only`.
- Provider-hosted web search remains a separate `Request.ServerTools` capability
  controlled by `web_search`.

### 9.4 `edit`

> Replace exact unique oldText; the file must already exist.

| param | type | notes |
|---|---|---|
| `files` | array, required | one entry per file (repeats allowed; see below); each target file must already exist |
| `files[].path` | string, required | must exist (use write to create) |
| `files[].expected_sha256` | string | optional 64-hex digest returned by `read.include_sha256` |
| `files[].edits` | array, required | one or more replacements for that file |
| `files[].edits[].oldText` | string, required | exact text to replace; must be unique in the original file unless `replaceAll` is set |
| `files[].edits[].newText` | string, required | replacement text; empty string deletes oldText |
| `files[].edits[].replaceAll` | bool | optional; replace every occurrence of `oldText` instead of requiring a unique match (default false) |

- The decoder also accepts the common single-file `{path, edits}` shape as an
  unadvertised compatibility input and normalizes it to `files:[{path,edits}]`.
  The model-facing schema remains canonical and requires `files`.
- All edits within one entry match against that entry's base content, not
  against content after earlier edits in the same entry.
- Every supplied digest is validated during the all-file planning pass. A mismatch
  returns the typed `stale_file` error and writes nothing. For repeated path
  entries, every supplied digest applies to the same original on-disk base while
  the text replacements themselves continue to apply to the prior planned bytes.
- A repeated `files[].path` is accepted and applied in order against the earlier
  entry's planned result; all planning still precedes any write, so a stale or
  redundant `oldText` fails loudly with the ordinary not-found error and leaves
  every file untouched. Nothing is silently double-applied.
- **Model-visible retention receipt** (shared with `write`, §9.5): once a later
  successful `edit` to the same paths exists, the live transcript may replace
  this call's input with `{"files":[{"path":P,"edits":N,"old_text_bytes":A,
  "new_text_bytes":B}],"_superseded":"edit content omitted; later successful
  edit to this path exists; read the file if needed"}`. The receipt is not an
  instruction; the model reads the file when it needs the current bytes.
- With `replaceAll`, every non-overlapping occurrence of `oldText` is replaced
  and counted; the uniqueness check is skipped but zero matches is still a
  not-found error. The overlap guard is relaxed only between spans of the same
  `replaceAll` block — a `replaceAll` span overlapping a different edit still
  raises the overlap error.
- 0 occurrences → error quoting the first non-empty `oldText` line with a
  nearest-region hint (up to 3 numbered lines centered on the most similar
  content line, with the similarity score), telling the model to re-read the
  file and re-issue with exact `oldText` (or use `write` for whole-file intent).
  N>1 occurrences → error asking for more context. Overlapping replacements →
  error asking the model to merge or retarget. Replacements producing content
  identical to the original are rejected as a no-op
  (`replacements produced identical content`).
- Every block is preflighted before writing, so one response reports all
  missing/ambiguous blocks for the file. Ambiguous errors list up to five start
  lines, and close not-found candidates identify the first divergent line.
- The tool preserves an existing UTF-8 BOM and the file's first observed line
  ending style. If exact matching fails, it retries after normalizing trailing
  whitespace, smart quotes, Unicode dashes, and special spaces. The normalized
  match carries an offset map back to the original content; only that raw span
  is replaced, so normalization cannot rewrite unrelated bytes.
- A top-level `path` is the default base for every `files` entry that omits
  its own `path`; entries naming the same path are tolerated, and an entry
  naming a different path is rejected as ambiguous.
- Success reports `edited <file-count> file(s), <replacement-count> replacement(s)`
  followed by one line per file.

### 9.5 `write`

> Write a whole file, creating parents; edit for partial changes.

| param | type | notes |
|---|---|---|
| `path` | string, required | |
| `content` | string, required | empty allowed |
| `expected_sha256` | string | optional 64-hex digest returned by `read.include_sha256` |

- `os.MkdirAll` parents (0755), write 0644, overwrite without ceremony (no permission
  system by design). Reports `created`/`overwrote`, bytes, lines.
- When `expected_sha256` is present, `write` compares it with the current existing
  file before overwriting. A mismatch or a path that disappeared since the read
  returns `stale_file`; an omitted digest preserves unconditional create/overwrite.
- A very large `content` can arrive as truncated streamed arguments; that
  failure path (§8.2 malformed streamed args) tells the model to use a smaller
  `write`, then append additional content with `edit`.
- Existing directory at path, or trailing `/` → error.
- **Model-visible retention receipt** (shared with `edit`, §9.4): once a later
  successful `write` to the same path exists, the live transcript may replace
  this call's input with `{"path":P,"_superseded":"content omitted; later
  successful write to this path exists; read the file if needed",
  "original_bytes":N}`. The receipt is not an instruction; the model reads the
  file when it needs the current bytes.

### 9.7 `shell`

> Run a command or ordered steps; prefer argv; git tool for git.

| param | type | notes |
|---|---|---|
| `argv` | array of strings | program + literal arguments; mutually exclusive with `command`; must not be a shell string or JSON-encoded array |
| `command` | string | shell command line; mutually exclusive with `argv`; use only when shell syntax is required |
| `steps` | array | 1–16 ordered entries, mutually exclusive with top-level `command`/`argv`/`stdin`; supported in foreground and background |
| `steps[].name` | string | receipt label; omitted means `step N` |
| `steps[].command` / `steps[].argv` | string / array | exactly one per step |
| `steps[].stdin` | string | step-specific stdin |
| `steps[].cwd` | string | overrides the inherited top-level cwd |
| `steps[].timeout_seconds` | int | overrides the inherited top-level timeout |
| `stop_on_failure` | bool | default true |
| `name` | string | optional command or step-batch label |
| `output_mode` | string | `auto` (default), `receipt`, or `full`; for steps, `full` returns the combined transcript and the others return compact receipts |
| `stdin` | string | written to the command's standard input |
| `cwd` | string | default process cwd |
| `timeout_seconds` | int | foreground default 120, background default 1200, no maximum |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |
| `background_lease` | object | background-only scheduling lease; does not restrict command behavior |
| `background_lease.resource_key` | string | coordination resource; defaults to the canonical command cwd |
| `background_lease.access` | string | `read_only` or `exclusive` (default) sharing mode |

- Exactly one of top-level `argv`, top-level `command`, or `steps` is required.
  Prefer `argv` — most commands need no shell quoting or expansion; pass a JSON
  array of strings such as `["go","test","./..."]`, not a shell command string
  or JSON-encoded array. `argv` runs via `exec.Command(argv[0], argv[1:]...)`:
  no shell, glob expansion, redirection, `$VAR`, or `~` expansion.
- `command` is executed via a **non-login** `bash -c` (fallback `sh -c`). The
  login PATH a login shell would have added is resolved once per process by a
  single `bash -lc` probe and appended (current PATH keeps precedence), so
  build/test toolchains are found without paying the login-profile cost on
  every call.
- **Combined stdout+stderr** with `[exit code: N]` always appended. **Non-zero
  exit is not a tool error** — a failing build is exactly the signal the model
  needs; only infrastructure failures (shell couldn't start) set `is_error`.
  Diagnostics metrics record outcome, exit code, timeout, cancellation, wait
  completion, and step aggregates for session error analysis.
- Process semantics (own process group/session, group kill on timeout/^C,
  descendant cleanup, incomplete-reap snapshots) are shared with
  `git`/`git_readonly`; see §9.8. Foreground calls finish when the direct
  program exits; long-lived commands should use `background:true`.
- Receipt policy: `output_mode:"auto"` preserves successful output through
  8 KiB, then returns a compact `PASS` receipt and archives the complete
  result; non-success becomes a `FAIL` receipt with a bounded output tail.
  `receipt` always selects the compact success form; `full` keeps bounded full
  output. Exact caps and artifact recovery are operational detail in
  [tools.md](tools.md#command-execution).
- `stdin`, when provided, is written verbatim to the command's standard input;
  absent means `/dev/null` (immediate EOF, never a hang). Prefer it over
  `echo`/heredocs — content travels with zero shell escaping.
- `steps` runs related format/build/lint/test commands serially, foreground or
  background. Top-level `cwd` and `timeout_seconds` are inherited defaults each
  step may override; top-level stdin is rejected (use `steps[].stdin`). The
  first non-zero, timed-out, cancelled, or unstartable step stops the batch
  unless `stop_on_failure:false`; cancellation always stops it. Each
  successful step returns only `PASS <name> (<duration>)`; suppressed success
  output and clipped failure output are combined and archived via
  `ResultTool.OriginalText`. A foreground steps call reports the sum of its
  resolved per-step timeouts through `SelfTimeouter`, so the dispatch backstop
  never cuts below it; background batches apply the 1200-second background
  default to each step without an explicit timeout.
- With `background:true`, the same process/step/formatting rules run under the
  background job manager (§9.15): default 1200-second timeout, and a default
  exclusive lease on the canonical command cwd. `background_lease` is
  scheduling metadata only — it neither restricts execution nor makes a
  command read-only; legacy top-level `resource_key`/`access` aliases remain
  accepted, but mixing them with `background_lease` is rejected. Completion is
  delivered once as request-only context; when later work depends on it, use
  one `background_jobs` `wait` call rather than polling `get`/`list`.
  Completion metrics are persisted once in a diagnostics-only
  `background_job_result` event that retains the launch agent/model identity,
  drained exactly once at request, prompt/idle, rotation, or shutdown
  boundaries; graceful teardown waits briefly for context-responsive runners.
- Environment inherited unmodified.

### 9.8 Shared process execution (`runProcess`)

`shell` (§9.7) and `git`/`git_readonly` (§9.9, §9.11) run their subprocesses
through one shared `runProcess` helper, so they share identical process semantics. The §9.7 schema/description above describe `shell`'s surface;
this subsection records the common runner those argv tools point at.

- **Own process group/session, no controlling TTY.** The child leads its own group, so
  a timeout or `^C` can signal the whole group (negative-pid `SIGKILL`) and reap
  descendants, not just the direct child.
- **Timeout.** `timeout_seconds` defaults to **120** for foreground calls and
  **1200** (20 minutes) for background calls (`0` means the default; there is no
  maximum). A negative value is rejected as invalid arguments.
- **Combined stdout+stderr** are captured (interleaved, to a temp file) and returned with
  a trailing `[exit code: N]` line. **Non-zero exit is not a tool error** — only a
  failure to start or capture output is.
- **Timeout / cancellation are reported in-band**, never as a tool error: the output
  ends with `[timed out after Ns; process group killed]` or `[cancelled; process group
  killed]` plus `[exit code: -1]`. If reaping cannot finish promptly, the tool still
  returns the captured snapshot and the status line notes the wait did not finish.
- After the direct process exits, any remaining same-group descendants are killed so a
  foreground call does not leak backgrounded children.

### 9.9 `git`

> Run git without a shell or pager; args[] or a workflow.

| param | type | notes |
|---|---|---|
| `args` | array of strings | argv after `git`; mutually exclusive with `workflow`; must not be a string or JSON-encoded array |
| `workflow` | string | `workspace_summary` or `commit`; mutually exclusive with `args` |
| `cwd` | string | default process cwd |
| `paths` | array of strings | required for `commit`; exact repository-relative files or directories (max 100); a directory includes everything beneath it; `.` rejected |
| `message` | string | required for `commit`; conventional commit message |

- `git` is registered only when `exec.LookPath("git")` succeeds at registry
  construction time; without git the model never sees the tool name. The runner
  prepends `--no-pager` and sets `GIT_TERMINAL_PROMPT=0` so interactive flows
  fail fast rather than hang.
- The advertised shape is `{"args":[...]}` — a JSON array of strings, not a
  string or JSON-encoded array (a bare string array is also accepted for
  compatibility). Process semantics and output conventions are shared with
  `shell` (§9.7/§9.8).
- **One argv tool, not narrow per-subcommand tools:** a single stable schema
  covers the entire git surface the model already knows from training;
  enumerating subcommands multiplies schemas and still misses the long tail.
- `workspace_summary` is a read-only deterministic survey: porcelain
  branch/status, the latest oneline commit, staged and unstaged diff stats, and
  staged and unstaged `diff --check`, with empty sections omitted and an unborn
  repository handled explicitly. It does not include the full patch; the model
  uses a subsequent raw `git diff` when patch inspection is needed.
- `commit` validates an explicit list of repository-relative files and
  directories (a directory stages and commits everything beneath it), rejects
  `.`, `..`, trailing slashes, globs, pathspec magic, duplicates, and absolute
  paths, then runs `git add`, staged `diff --check`, a staged-change check, and
  `git commit` scoped by `--` to those paths. Unrelated staged changes remain
  staged. A failed whitespace check rejects with a corrective sentence naming
  the offending lines; failures leave the staging area recoverable. The compact
  receipt reports the new short hash/subject, committed files, and remaining
  status.

### 9.10 `web_fetch`

> Fetch a URL as readable text.

| param | type | notes |
|---|---|---|
| `url` | string, required | http/https only |
| `max_bytes` | int | default 256 KB, cap 5 MB |
| `timeout_seconds` | int | default 30, no maximum |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |

- Default 30 s timeout, configurable without a maximum; up to 5 redirects, each
  hop re-validated as http/https.
- `text/html` gets a hand-rolled reduction that preserves links and block
  structure (drop script/style/chrome, render `<a href>` as `text (url)`,
  newline at block boundaries, unescape entities); explicitly "readable-ish
  text", not a renderer. Other `text/*`, `application/json`, `application/xml`,
  and `+json`/`+xml` suffix types pass through raw; an absent `Content-Type` is
  treated as text; binary content types fail with an error.
- Output is prefixed `# <final-url> (<status>, <content-type>)`. Non-2xx
  responses return status + body as ordinary content (not `is_error` — the
  model may want the error page). The reader consumes one byte beyond
  `max_bytes` to distinguish a capped response and appends an explicit
  truncation marker. Model-visible output has a 32 KB/500-line default cap; the
  standard artifact path preserves omitted text.

### 9.11 `git_readonly`

> Read-only git queries only; no shell or pager.

| param | type | notes |
|---|---|---|
| `args` | array of strings, required | argv after `git`, starting with the subcommand; must not be a string or JSON-encoded array |
| `cwd` | string | default process cwd |

- A restricted sibling of `git` (§9.9) used by restricted agents (§14),
  registered only when git is installed. Its runner injects `--no-pager`,
  `--no-optional-locks`, and `-c core.fsmonitor=false`, plus
  `GIT_TERMINAL_PROMPT=0` and `GIT_OPTIONAL_LOCKS=0`; diff-capable commands
  additionally disable external diff and text-conversion helpers. Input shape
  and argv decoding match `git`.
- **Audited allowlist by bare subcommand:** only query-only verbs (`blame`,
  `cat-file`, `diff*`, `grep`, `log`, `ls-*`, `rev-*`, `show*`, `status`, and
  similar; the full list is maintained in [tools.md](tools.md#command-execution)).
  Mixed read/write verbs such as `branch`, `config`, `remote`, `reflog`,
  `submodule`, `tag`, and `worktree` stay excluded even when some invocations
  are observational; `bisect` is excluded because it mutates repository state.
- `args[0]` must be a bare allowlisted subcommand and cannot start with `-`,
  which blocks all global option injection. Subcommand flags that can write a
  file or launch configured programs are rejected: output-file flags, pager
  flags (including clustered `grep -O`), external diff/textconv/filter flags,
  signature display, and `%G` pretty/format placeholders.

### 9.12 `write_tmp_file`

> Write a scratch file in this run's temp dir; returns its path.

| param | type | notes |
|---|---|---|
| `name` | string, required | relative file name (subdirectories allowed) |
| `content` | string, required | full file content (empty allowed) |

- Gives read-only agents (§14, `plan`) a place to draft notes without project
  write access. Files are written under one `os.MkdirTemp` directory created lazily on
  first use and shared across calls; they are kept after exit.
- `name` must be relative and stay inside the temp directory: absolute paths and any
  `..` escape (after `filepath.Clean`) are rejected. Returns the absolute path written.

### 9.13 `update_todos` (`internal/todo`)

> Replace the whole advisory TODO list; one in_progress at most.

| param | type | notes |
|---|---|---|
| `todos` | array, required | complete replacement list; empty clears it |
| `todos[].step` | string, required | concise action label |
| `todos[].status` | string, required | `pending`, `in_progress`, or `completed` |

- Whole-list replacement keeps the contract intentionally small: no IDs,
  dependencies, evidence, receipts, implicit plans, or lifecycle transitions.
  Validation requires non-empty trimmed steps, known statuses, and at most one
  `in_progress` item. The store copies the list so callers cannot mutate it.
- Status is advisory only. It never completes work, stops the loop, blocks a
  tool, advances a phase, or changes goal state.
- The root store is persisted as `Session.Todos`, restored on resume, and
  cleared by `/clear`. A transcript rewrite or resume schedules one unresolved
  list reminder as ephemeral `RequestContext`; a normal `update_todos` call
  clears it because the tool call itself is already in the transcript. The
  stale-list reminder cadence is operational detail in
  [tools.md](tools.md#built-in-tools).
- Every built-in agent exposes `update_todos`, including `plan`. Each delegate
  gets a private store. Custom agent whitelists may omit it.

### 9.14 `delegate`

> Run a child agent on separable work; keep coupled work local.

| param | type | notes |
|---|---|---|
| `task` | string, required | complete child prompt: objective, scope, constraints, expected report, and verification |
| `agent` | string | optional configured agent name; omitted uses the current active agent; remains a simple enum for provider compatibility |
| `mode` | string | optional; `implementation` marks scoped mutating work and adds implementation-specific child instructions; omit for exploration/review |
| `max_turns` | int | optional tool-enabled loop budget; defaults to `delegate_max_turns`; the schema publishes that numeric maximum and over-cap values are rejected |
| `continue_child_id` | string | optional terminal sibling child ID; continues compatible retained state in a fresh child record |
| `background` | bool | only for independent non-overlapping work; after one useful parent model round, harness joins outstanding delegates and requires synthesis; do not poll or duplicate |
| `scope` | string | background only; workspace path scope, default process cwd |
| `access` | string | background only; `read_only` or `exclusive`; default inherited from the selected agent |

- Implemented in `internal/delegate`, not `internal/tools`, to avoid an import cycle:
  the delegate tool starts a child `agent.Agent`, while `internal/agent` already
  depends on `internal/tools` for dispatch.
- The `agent` schema description appends a deterministic catalog with exact shape
  `Available:\n- <name>: <one-line description>`. The enum and catalog
  contain only candidates whose configured tools are a subset of the current
  parent's live tools — the capability-escalation guard; non-subset calls fail
  before any child model request. Candidate descriptions are whitespace-normalized
  to one line and capped at 160 bytes. `delegate` opts into preserving schema
  descriptions in `Registry.Specs` (`SchemaDescriptionPreserver`).
- **Prompt construction:** `prompts/delegate-child.txt` is appended after the
  resolved system prompt only in `Runner.Run` — root prompts, including a
  configured custom static prompt, never receive it. The suffix says the child
  reports to the parent, owns only its delegated scope, does not ask the user
  questions, returns an evidence-backed report, and avoids recursive delegation
  by default. A second child-only block states the exact effective tool-enabled
  turn budget and discloses the possible tools-disabled wind-down request.
- `max_turns` has JSON Schema bound `minimum: 1`; its numeric `maximum` equals
  the active `delegate_max_turns`. The runner validates the same bound even when
  a provider emits invalid arguments despite the schema; it never silently
  clamps. Receipts state the effective budget.
- `mode` currently accepts only `implementation`, validated by both decoder and
  runner. It adds a static child-only instruction identifying scoped mutating
  work and requiring implementation, verification, and an exact handoff with
  changed paths, checks run, and any remaining work.
- **Completion contract:** the child may append exactly one terminal
  `harness-completion` fenced JSON footer with `outcome` (`complete`|`blocked`)
  and, for `blocked`, a bounded `blockers` array; substantive content stays in
  the Markdown report. Parsing examines only the final assistant response; a
  valid footer is stripped from parent-facing prose, while missing, malformed,
  duplicate, invalid, or oversized footers preserve the prose and become
  `unknown`/`unreported` outcomes rather than failing the run. Failures and
  cancellation receive host/unavailable provenance. `ChildMeta.Completion`
  stores outcome, optional blockers, contract/source provenance, and validation
  status beside lifecycle status. Each continuation writes an independent
  report. Receipts also report the root descendant budget (e.g. `3 of 16
  descendant slots remaining`); non-positive settings select the default
  4-active/16-total limits.
- `continue_child_id` names an already-terminal child of the same immediate
  parent. Continuation never appends to or overwrites the source directory: the
  Runner creates a fresh child seeded with the source transcript, private TODO
  list and latest plan, proxy session ID, cache-affinity ID, and provider
  response anchor, and the continuation prompt tells the child to re-check
  repository state. Source usage is not charged again; the new child's turn
  counter starts at zero with a fresh allowance equal to the source's effective
  budget. Omitted `agent`/`mode`/`max_turns` inherit; explicitly conflicting
  values are rejected. The source must carry terminal metadata, a resumable
  `state.json`, and a `runtime_fingerprint` matching the current runtime
  (provider, model, agent selection, mode, budgets, context/output settings,
  reasoning, server tools, stateful setting, system prompt, tool schemas,
  compaction policy). Pre-fingerprint children are not resumable this way.
- **Continuation pressure rule:** before the first continued request the Runner
  estimates the retained transcript plus continuation prompt against the current
  context window. At or below 60% it restores the complete transcript and
  compatible provider anchor; above 60% it makes one tool-less maintenance
  summary and replaces the new child's active history with a single typed
  compaction checkpoint (an all-history boundary, unlike normal compaction's
  recent-turn suffix), preserving active prompt/steering instructions verbatim,
  archiving removed messages under the new child, and resetting the remote
  anchor. A failed summary, blocked/no-op rewrite, or still-oversized checkpoint
  rejects the continuation instead of truncating exact instructions.
  `ChildMeta` records `continuation_mode` (`retained`|`compact_checkpoint`) and
  the before/after/window estimates. Foreground and background paths apply the
  same contract.
- The runner owns one root-shared budget coordinator that atomically enforces
  `delegate_max_active` and `delegate_max_descendants` across recursive and
  background launches; a continuation reuses its logical descendant slot and
  all terminal paths release active capacity.
- Background launches resolve scope before scheduling: built-in
  `explore`/`plan`/`review` default to shared `read_only` access,
  `auto`/`independent` to `exclusive`, `mode:"implementation"` is always
  exclusive, and custom agents set `workspace_access`. Lease conflicts fail
  before child/session creation; normalized scope and access are persisted in
  child metadata.
- Review groups deliberately compose ordinary background `delegate` calls with
  one `background_jobs` `ids`/`until:"all"` join; the parent owns synthesis. A
  dedicated `delegate_group` remains deferred until telemetry shows launch-call
  overhead or a typed verdict contract is the remaining bottleneck.
- A child run that fails only with transient provider classes (rate limit,
  overloaded, 5xx, provider-side timeout while the parent context is intact) is
  rewritten at the tool boundary (`internal/delegate/errors.go`): the error
  names the classes, tells the parent to retry once and then report the blocker,
  and stamps the diagnostics-only `rate_limited`/`provider_error` kind.
  Permanent failures pass through verbatim.
- Root depth is `0`; a launch is rejected when the current depth reaches
  `delegate_max_depth`, and the deepest allowed child has `delegate` removed
  before its registry/specs are built. Root `max_prompt_tokens` and
  `max_prompt_cost_usd` are copied into every child as per-prompt ceilings, not
  a hierarchy-wide budget. Children receive private TODO and plan stores;
  `handoff` is always removed from child agents. Foreground delegates remain
  serialized because children share the checkout and may write.
- The parent transcript records only the normal `delegate` call and compact
  result; child transcripts are saved under `children/<child-id>/` for
  forensics, and child usage is reported through `MeteredTool` and folded into
  parent totals exactly once.
- A process-local `delegate.ActivityRegistry` tracks every running child;
  the Runner's exactly-once terminalization closure flushes pending display
  text, persists final metadata, publishes the terminal lifecycle event, and
  removes the entry. Stable display labels (`d1`, `d2`, …) are allocated solely
  by the registry. Retained activity is bounded and ANSI/control-sanitized:
  turn/attempt, context/usage totals, retry state, reply state, and allowlisted
  path fields only — never model text, reasoning, raw tool results, commands,
  or URLs. The optional `ActivityFeed` behind `delegate_output=lines` retains
  at most 512 events/256 KiB and is never authoritative over the registry.
  The `delegate_tmux` views and inline-feed policy are operational behavior;
  see [tools.md](tools.md#delegation).

### 9.15 background jobs

Tools that opt into the reusable background job contract hand the manager a job
kind, description, optional canonical resource/access lease, and cancellable
runner. The manager owns ids, status, list/get/wait/cancel, lease enforcement,
exactly-once structured terminal-notice selection, and request-only context
delivery. `shell`, `web_fetch`, and `delegate` support this path via
`background:true`; background delegates keep the same launch validation, child
transcript, private coordination stores, and token accounting as synchronous
delegates.

`background_jobs` accepts:

| param | type | notes |
|---|---|---|
| `action` | string | `list`, `get`, `wait`, or `cancel`; omitted means `list` |
| `id` | string | required for `get` and `cancel`; optional for `wait` |
| `ids` | array of strings | optional multi-job selection for `wait`; mutually exclusive with `id` |
| `until` | string | `first` (default) or `all` for `wait` |
| `timeout_seconds` | integer | `wait` timeout; omit for ordinary dependency waits (default 120 seconds), rather than using a short timeout as a status probe |

- Jobs live only in the current harness process. Running jobs are abandoned on
  process exit and cleared on `/clear`.
- Resource keys are absolute paths with symlinks resolved through the longest
  existing prefix. Multiple `read_only` jobs may share an exact key;
  `exclusive` conflicts with every unfinished lease for that key; different
  keys do not conflict. Completion and failure release immediately;
  cancellation retains the lease until runner cleanup finishes; abandonment
  releases it immediately.
- `wait` is event-driven rather than polling. `id` selects one job, `ids` an
  explicit group, and omitting both snapshots the jobs currently running;
  selection is stable, so later launches never extend an in-flight wait. An
  untargeted wait with no running jobs returns the current list immediately; a
  timeout is a normal result containing the latest selected status. Jobs
  returned as completed are marked delivered so the same result is not
  injected again. An accepted user steer broadcasts to currently blocked waits
  only: such a wait returns a detached acknowledgement, keeps its stable
  selection and original timeout in a manager-owned observer, and publishes
  one detached aggregate as request-only context when the selection resolves.
  The interactive scheduler then starts a continuation with cause
  `detached_background_wait` only after delivered user input, drafts,
  approvals, EOF/shutdown, and interrupts; ordinary fire-and-forget completion
  context never starts an autonomous model call.
- The system prompt and background-capable tool schemas route a strict
  completion dependency to one `wait` call; `get` and `list` are for
  nonblocking inspection, not repeated status polling.
- Completed job summaries are delivered once as request-only context to the
  parent agent — never inserted into the parent transcript — including the
  transcript path when one exists. Background delegates are marked
  join-required: the parent may perform one subsequent useful turn but cannot
  complete the prompt until all such delegates finish and a model request has
  received their reports for synthesis. Ordinary background commands remain
  detached and may outlive the prompt. Truncated output is archived under
  `artifacts/tool-results/` with the same targeted recovery guidance as a
  foreground result.
- A job may carry compact `Text` plus complete `OriginalText`: automatic
  completion uses `Registry.PrepareResultWithOriginal`, and `background_jobs`
  implements `ResultTool`, so explicit get/wait output preserves the same
  archive opportunity.
- `BackgroundJobResult` carries child model usage plus a separate successful
  compaction count. Background delegate usage flows through the manager
  exactly once and is folded into parent prompt/session totals before
  completion, including partial usage from failed or canceled runs; child
  compactions remain child-session metadata.
- Terminal notice delivery, completion context, diagnostics, and usage drains
  are independent exactly-once channels. The UI applies exit-summary
  formatting only to delegates (with a `child session` label), then persists
  and mirrors the text; interactive JSON receives it as a normal `notice`
  event, and one-shot mode retains only its final aggregate session summary.
- Background jobs run in the same cwd/tool policy as ordinary tools. Resource
  leases coordinate opted-in local background work; they do not sandbox paths
  or serialize foreground filesystem edits.

### 9.16 MCP tools (`internal/mcptools`)

> Each tool discovered from the MCP proxy, proxying `tools/call` over a shared, reconnecting proxy connection.

These are not built-in tools: they are registered dynamically at startup when MCP
is enabled (§15), one `*mcptools.Tool` per proxy-advertised tool. The adapter
contract maps the MCP tool shape onto the `Tool` interface:

- **Name** is the proxy's full `mcp__<server>__<tool>` already. `Register`
  re-validates it against the provider charset `[a-zA-Z0-9_-]{1,64}` plus the
  required `mcp__` prefix; a name that fails is **skipped**, not rewritten (a
  truncated name could collide), and recorded in the registration summary.
- **Description** is reduced to one line: trimmed, first line only, byte-capped at
  512 bytes on a UTF-8 rune boundary, with an ellipsis when truncated.
- **Schema** keeps the MCP `inputSchema` on the adapter; an absent schema
  (nil/empty/`null`) becomes `{"type":"object"}`. Model-facing registration then
  applies the root `_stage` injection and annotation stripping described at the
  start of §9. The adapter does not interpret `_stage` and the remote MCP server
  never receives it.
- **`ReadOnly(input)` is policy-controlled.** Harness trusts
  `annotations.readOnlyHint:true` for enabled MCP registrations so those tools can
  be exposed to agents whose `mcp_tools` mode is `read_only` (§14). Scheduling is
  default-parallel regardless of this annotation (§8.1).
- **Result mapping** invokes the remote tool exactly once. Text blocks pass through;
  valid direct image blocks become provider-neutral rich image children while their
  text position is represented by `[image attached: <mime>]`. Audio, resource links,
  embedded resources, and unknown blocks remain bracketed textual placeholders.
  Blocks join with `\n` in order. If nothing renders but `structuredContent` is
  present, the raw structured JSON is the fallback. Invalid images and all MCP error
  results remain text-only.
- **Errors:** a transport/protocol error returns `("", err)` so `Dispatch` creates
  an error result containing `<err>`. A successful result with `isError` true
  returns the rendered text as an `error` (empty text gets a stand-in), so the
  failure flows through the normal tool-error path.

The shared `*mcptools.Conn` is a lazily-reconnecting wrapper around one
`mcp.Client` session to the proxy. It spawns no goroutines; reconnection is
synchronous on the calling goroutine under a backoff gate, so a down proxy
fast-fails subsequent calls rather than storming reconnects. A proxy crash
mid-session surfaces as error tool results; the next call reconnects when the
backoff allows.

### 9.16a Native LSP tools (`internal/lsptools`)

> Short `lsp_*` adapters over the in-process `internal/lspproxy.Manager`; the
> protocol operations and lifecycle are detailed in §15a.

The adapter deliberately does not weaken the generic MCP adapter's required
`mcp__` namespace. It lists the manager's bare static surface, prefixes names
with `lsp_`, honors the configured allowlist, and dispatches the original bare
name back to the manager. `readOnlyHint` annotations determine scheduling and
agent `mcp_tools` exposure; code-action apply, formatting apply, and rename apply
are mutating. The registry owns the reserved root `_stage` property; the LSP
adapter does not interpret it and the manager never receives it.

Unlike ordinary dynamic MCP tools, an LSP adapter implements
`SchemaDescriptionPreserver`. Position and range descriptions are part of the
model-facing contract: they explain that lines/columns are 1-based, `symbol` is
preferred when locating identifiers, range bounds are inclusive, and several
fields have Harness-owned defaults. `Registry.Specs` therefore retains those
field descriptions instead of applying its normal prose-stripping policy.

Registration is anchored, not appended: `Register` inserts each `lsp_*` tool
immediately after `view_image`, chaining each new tool after the previous one.
The LSP block therefore stays adjacent to file inspection in both the default
and complete catalogs; a missing anchor falls back to append. LSP tools keep
their concise operation descriptions without adding cross-tool search steering.

### 9.17 `record_plan` and `handoff`

`record_plan` (`internal/plan`) accepts `{title, plan}`, both required after
trimming, renders `# <title>` plus the self-contained Markdown body, and writes
`<session>/plans/NNNN-<slug>.plan.md` via temp-file-then-rename (0644). Each
call creates a new immutable artifact; the synchronized store keeps only the
latest `plan.Plan` pointer for persistence, display, and handoff. It is part of
the default coordination tool set (`auto`, `independent`, default-inheriting
custom agents, and `plan`), requires a live session directory, and is private
per delegate child.

`handoff` (`internal/tools` + `internal/handoff`) accepts an optional `{agent}`
and is enabled only for the interactive root `plan` agent. It rejects one-shot
mode, a missing plan, and unknown explicit agents. Only exclusive agents
populate the `agent` enum (`auto`, `independent`, custom agents with
`workspace_access: exclusive`); an omitted `agent` defaults to `auto`. The tool
records a synchronized pending `handoff.Request`; tools never prompt or switch
agents themselves. At the interactive boundary, Harness renders the complete
latest plan, asks for approval, and on approval switches agent/model, archives
the planning transcript, resets the conversation tree to one user message
containing the absolute plan path and body, appends optional `/handoff`
context, clears the implementation agent's TODO list, and starts the
implementation prompt.

## 10. CLI / REPL (`internal/ui`)

### Rendering

- Assistant text and reasoning summaries use a small stdlib-only Markdown renderer on terminal output. Emphasis becomes
  ANSI bold/italic when color is enabled, inline code and links use cyan,
  headings keep their `#` markers and render bold (with H1 headings also
  underlined), thematic breaks render as `────────────────────`, lists normalize
  and indent continuations, and tables remain padded grids when they fit. A grid's
  visible width is `visibleLen(prefix) + 1 + sum(column width + 3)`, so prefixes,
  borders, and cell padding all count. On overflow, columns start at the
  three-cell minimum and receive remaining capacity round-robin from left to
  right; rendered cells wrap ANSI-safely, closing styles before padding or a
  newline and reopening them in continuations. When even that minimum grid cannot
  fit, the renderer emits lossless labeled stacked records instead. Inline spans
  close only the bold, italic, underline, or foreground attribute they opened
  instead of issuing a blanket SGR reset, so nested code, links, and emphasis
  preserve their outer heading or emphasis style.
- Paragraphs and list bodies are rendered once and then wrapped by visible
  rendered width (80 columns when terminal width is unavailable), so Markdown
  delimiters do not consume columns and spans may cross a break. The wrapper
  tracks active SGR attributes, closes them before each newline, and reopens them
  after the continuation prefix so list indentation is never styled. Displayed
  implementation plan follows the same Markdown, color, and width policy, while the
  implementation-context copy retains the original Markdown
  source. Redirected one-shot stdout remains raw model text.
- Recognized language tags on fenced code blocks use the stateful, stdlib-only,
  additive highlighter in `internal/term/highlight` when ANSI is enabled. Each
  code or diff state carries an immutable copied `dark` or `light` palette; there
  are no mutable global colors. Aliases and decorated info strings are normalized;
  untagged, unknown, and `text` fences fall back to plain code. Opening and closing
  delimiters remain unstyled, code lines are never wrapped, and stripping ANSI
  reproduces the renderer's normalized plain output. Lexer state follows multiline
  comments and raw strings across completed streamed lines and arbitrary input
  deltas, then resets at the closing delimiter so it cannot leak into a later
  fence. Live assistant Markdown, reasoning-summary Markdown, displayed tool
  diffs, replay, and follow all receive the same selected palette. Inline code,
  links, headings, statuses, prose, and terminal-default plain identifiers are
  outside theme scope.

  Every fixed foreground has at least 4.5:1 contrast against both diff-row
  backgrounds in its theme. The exact truecolor roles are:

  | role | dark | light |
  |---|---|---|
  | comment | `#81B88B` | `#007700` |
  | keyword | `#65A9E0` | `#0000FF` |
  | string | `#CE9178` | `#A31515` |
  | number | `#B5CEA8` | `#087A50` |
  | builtin/type | `#4EC9B0` | `#23758D` |
  | function/call | `#DCDCAA` | `#795E26` |
  | added sigil/fenced-diff row | `#89D185` | `#007700` |
  | removed sigil/fenced-diff row | `#F48771` | `#A31515` |
  | added row background | `#213A2B` | `#DAFBE1` |
  | removed row background | `#4A221D` | `#FFEBE9` |

  `dark` remains the default. Harness does not infer or query the terminal
  background and does not paint a global background; the user selects the mode
  matching the terminal profile. ANSI suppression is independent of theme
  validation and leaves source/Markdown structure unchanged.
- The built-in system prompt asks tool-using models for brief user-facing
  commentary before tool calls and at meaningful work milestones. These
  commentary messages are normal assistant text; Responses `phase` metadata is
  preserved in transcript history when the provider supplies it.
- When Responses phase metadata marks visible commentary or reasoning output
  before `final_answer` text, live rendering and session replay insert a
  standalone `────────────────────` rule before the final answer, with no extra
  blank lines around it. The rule is dimmed when color is enabled, matching the
  submitted-prompt separator. Providers without phase metadata keep their
  assistant text stream unchanged.
- Responses API reasoning summaries are semantic model-to-user output events,
  not notices and not transcript messages. They default off. When explicitly
  enabled, interactive runs render them to stdout as a compact two-space indented
  block headed by a timestamp line such as `[16:15:34 reasoning]` (the header
  drops the timestamp and reads `[reasoning]` when status timestamps are
  disabled) and closed by an `[end reasoning]` footer. Non-interactive runs render
  explicitly enabled summaries to stderr.
- Turn progress renders as plain stderr lines, e.g. `[turn: 1 waiting]`. A
  completed conversational turn renders
  `[turn: 1 · 6s · $0.032 · ctx 74% 148.0k/200.0k │ prompt 18s]`, including the
  turn's dollar cost once the model stream has closed and the final
  input/output/cache totals are known (proxy-priced when the proxy supplies
  `CostUSD`, otherwise priced against the model registry; omitted when the
  model has no configured price). The enclosing prompt emits one
  aggregate `[prompt: 3 turns …]` usage/cost line. Attempt start/usage events are
  always recorded in `raw.ndjson` for timing and accounting diagnostics but do
  not produce separate visible cost checkpoints. Retried attempts record a
  discard marker so replay can omit abandoned assistant/reasoning deltas while
  keeping the retry notice.
- **Submitted-prompt separator (interactive).** When a REPL prompt is submitted,
  the entered line is left on screen and a single dim rule line is drawn before
  the model output, replacing the previous double blank line. Because the rule is a real scrolled
  line rather than the transient wait counter, it survives the counter's in-place
  erase and continues to separate the prompt from streamed output after the
  counter is replaced.
- **Live wait counter (TTY, non-quiet).** While a model request, a tool call, a
  model-backed compaction, or a join-required background
  delegate is outstanding, the static waiting line is replaced by a single in-place
  line painted with `\r\x1b[2K` and repainted ~once a second by a `time.Ticker`
  goroutine (with a stop-and-drain handshake): `[turn: 1 · 12s · ctx 30% 60.0k/200.0k │ prompt 18s]`,
  `[tool: shell argv=["rg","x","."] · 3s]`, `[context: compacting · 3s]`, or
  `[background: waiting for delegates · 12s │ prompt 30s]`, with the same compact key
  arguments as the completed tool summary and the running context-window percentage
  and compact used/window token counts appended for model waits
  (`· ctx 30% 60.0k/200.0k`). Active delegate registry state is appended to the
  same row as `· delegate d1 explore: turn 2 · thinking`; concurrent children use
  `· 3 delegates · latest d2 plan: tool read path="…"`. Join-required
  background work keeps the normal wait lifecycle active until it terminalizes;
  delegate state alone never repaints over streamed or idle prompt text.
  Field-aware clipping drops the activity body before the stable display ID, and
  during-prompt input reserves enough columns to keep its edit cursor visible.
  The registry is authoritative
  when configured; legacy foreground/background progress closures are only a
  compatibility fallback for isolated renderers. `delegate_output=off` omits
  delegate details without disabling unrelated status; `lines` keeps this row and
  additionally enables scrolling child lines. Quiet/non-TTY rendering suppresses
  the row. It is erased the
  instant real output or a tool line scrolls in — not a sticky bar or scroll region.
- **Inline delegate activity.** `delegate_output=lines` starts one prompt-scoped
  feed consumer independently of the live-status ticker. It discards pre-prompt
  feed history, reads one bounded batch, and advances its sole cursor only after
  a best-effort stderr write. An incomplete parent plain or Markdown source line,
  including a partial highlighted code line, holds the cursor in place;
  newline-complete fence and buffered-table lines are safe. Querying the boundary
  never flushes Markdown, advances or resets highlighter state, or adds a parent
  newline. Detaching delegate progress requests an acknowledged
  drain; prompt completion finishes the parent line, drains through a captured
  tail, and stops the consumer. Missing sequences render as
  `[delegate output] omitted N event` / `N events`. Inline lines are ANSI-free,
  timestamp-free, absent from parent persistence/context, and never write
  stdout.
- **Physical output coordination.** The CLI constructs one
  `OutputCoordinator` from the real stdout/stderr streams and gives its adapters
  to the renderer, app, logger, prompt editor/pickers, and inline consumer.
  Renderer assistant/status state uses one `renderMu`; registry/feed snapshots
  happen before it, and physical writes acquire the coordinator only afterward.
  The coordinator owns the drawn prompt snapshot and desired transient status
  bytes, so status erase/activity write/status repaint and prompt
  clear/log-or-activity/prompt redraw are atomic. Code holding its mutex does not
  acquire renderer, feed/registry, or logging locks or wait for goroutines.
  Wrappers expose the underlying file only for TTY capability checks; writes
  still pass through the coordinator. This preserves stdout bytes while
  preventing prompt, ticker, log, parent, and child terminal bytes from
  interleaving.
- **During-prompt input line.** Keystrokes typed during a prompt are read in raw,
  echo-off mode and shown on that wait line after a `>` marker
  (`[turn: 1 · 12s │ prompt 18s] > draft`). Pressing Enter during a prompt
  steers model-bound input before the next turn when possible; with steering
  disabled, or for commands/editor/shell input, it queues the next prompt.
  Shift-Enter/raw LF still inserts a newline for multi-line prompts. On normal
  prompt completion or interrupt (`^C`/Esc-Esc), any unsubmitted partial buffer is
  deposited into the next REPL prompt as editable, pre-filled text (cursor at end).
  `^C`/Esc-Esc cancel the current prompt.
- Tool-call progress details can render to stderr when explicitly enabled:
  `[tool-call: name id=...]` as the model builds the call and
  `[tool: name started ...]` when local execution starts. Enable with
  `-tool-stream`, `HARNESS_TOOL_STREAM=true`, `"tool_stream": true`, or `-v`.
  Partial argument deltas are not printed; session replay keeps completed tool
  calls and results.
- Tool results use detailed one-line summaries by default, for example
  `[grep] args=["-R","-n","func main","."] → 14 lines, 2.1KB`. Successful
  built-in `read` calls are the interactive exception: non-verbose live text
  shows `[read] path=docs/design.md` once per unique effective path in each
  conversational turn, so repeated offset/limit windows of one file do not
  repeat progress. Deduplication uses the full cwd-relative path before display
  clipping. Read failures, centrally truncated results, and inputs whose path
  cannot be decoded safely retain the detailed per-call summary. Built-in
  `shell` results get the same interactive projection: non-verbose live text
  shows the name or command (steps as `steps=N name: label; label; label`, at
  most three labels then `+N more`; labels are whitespace-collapsed and
  clipped), the outcome (`ok`, `failed (exit N)` for positive exit codes, bare
  `failed` for signal-killed or unclassified exits, `cancelled`, `timed out`),
  partial-run progress (`ran x/y, skipped m`), and background launches as
  `background job <id> (<access> @ <resource>) → started`; command failures
  keep the projection because the status segment is the point, while
  tool/dispatch errors (`IsError`) and undecodable inputs fall back to the
  detailed form. The outcome segment reads the diagnostics command-outcome
  metrics: every non-error `shell` result carries `command_outcome_available`
  (background launch receipts report the launch itself as `command_succeeded`),
  and results missing it fall back to the detailed form as well. `-v` restores
  every detailed read and shell summary, adds the first ~5 result lines (dimmed), and
  enables progress details. Absolute file-path argument values (`path`, `file`,
  `cwd`, and read's path aliases) are displayed relative to the session working
  directory when the path lies under it; paths outside it remain absolute. This
  projection is display-only: model-facing transcripts, recorded `Input` JSON,
  `raw.ndjson` display lines, and ordinary session replay retain every detailed
  tool call/result summary.
- Large estimated contexts, payloads, or tool schemas print one warning per
  prompt because they can materially slow first response latency.
- Per-prompt usage line:
  `[prompt: 3 turns · 12.4k (18.0k) in / 1.8k (2.6k) out · $0.071 ($0.101) · ctx 18.0k/128.0k · compactions 1 (2 total) · 4.3s]`
  (cost omitted for usage without known cost). The compaction segment follows
  context usage. It includes the prompt count only at 1 or more and the cumulative
  total only at 2 or more, and is omitted when neither qualifies. Counts include
  only successful transcript rewrites, not failed, blocked, no-op, or discarded
  idle attempts. When non-zero the line
  also appends cache-read tokens with the cache-hit ratio (`· cache 3.0k read (75%)`)
  and reasoning tokens (`· 450 reasoning`). A model with no known cost prints a
  one-time-per-model `[note: no price configured for "<model>"; ...]` notice instead of
  silently dropping cost.
- Bracketed status lines are prefixed inside the bracket with local time by
  default, for example `[16:15:34 tool-call: name id=...]`.
  `-timestamps=full` uses `yyyy-mm-dd hh:mm:ss`; `-timestamps=none` disables
  status timestamps.
- ANSI color only when stdout is a TTY (`os.Stdout.Stat()` mode check);
  `NO_COLOR` env or `-no-color` disables color. Structural Markdown rendering
  remains legible without color.
- Startup diagnostics use `log/slog` with a plaintext handler: `[level] [category]
  message`. Default level is `info`; `--log-level` or `HARNESS_LOG_LEVEL` accepts
  `debug`, `info`, `warn`, or `error`. Normal sessions also tee JSON diagnostics to
  `diagnostics.ndjson` in the session directory; that sink accepts debug records so
  child MCP/LSP stderr is preserved even though it is hidden from the terminal by
  default and only shown with `--log-level debug`.
- `-q`/`--quiet` suppresses bracketed status messages (tool calls, turns,
  notices), disables live tool-stream progress and the live wait counter, suppresses
  reasoning summary output unless `-reasoning-summary` is explicitly set on the
  CLI, and suppresses status lines in `harness session replay`; replay parses its
  own `-q`/`--quiet` flags rather than inheriting top-level argument scanning. Quiet
  mode does not filter slog diagnostics. The per-prompt usage/cost line is governed by a separate
  `RenderOptions.SuppressUsage` (default false; the wiring sets it only for
  `-q` **and** non-TTY output), so a quiet interactive run still prints one cost line.
  One-shot runs additionally print a final `[session summary: …]` cost line to stderr
  that bypasses `-q` entirely.

### Terminal reset on REPL start

Before the first prompt the REPL restores the controlling terminal to a usable state
(`internal/term`, stdlib-only): kernel termios to the platform's `stty sane` equivalent
(GNU semantics on Linux; BSD `f_sane` flag semantics plus the `cfmakesane` control-char
reset on macOS), then an emulator soft reset (DECSTR; mouse tracking, focus reporting,
and bracketed paste off; leave alt screen; show cursor; charset and SGR reset). This
repairs a terminal left in raw/no-echo/mouse-reporting state by a crashed program. It
targets `/dev/tty` directly, is a silent no-op without a controlling terminal, and —
unlike the RIS (`\033c`) it replaced — never clears the screen or scrollback.

After reset, the REPL enables bracketed-paste reporting for the session and disables it
on exit. On an interactive TTY, the idle prompt switches to a small raw-mode line editor
that supports left/right cursor movement, Backspace, Delete, insertion at the
cursor, Ctrl-A/Home and Ctrl-E/End movement, Ctrl-B/Ctrl-F left/right aliases,
Ctrl-C to interrupt, and Ctrl-D on an empty prompt. Shift-Enter inserts a newline without submitting,
so multi-line prompts can be typed directly in the REPL. The editor stores
cursor positions as Go runes; exact grapheme
cluster and emoji-width handling are out of scope. Paste fills the editable buffer
for review and does not auto-submit: Enter submits the buffered content. Each
paste event is classified independently after newline normalization. A range of
at most 1,000 normalized UTF-8 bytes renders inline, including multiline content;
a range over 1,000 bytes renders as a one-line `[N bytes of pasted content]`
placeholder wherever it occurs in the prompt. The real content remains in the
buffer and is submitted on Enter in either case. Collapsed ranges persist while
surrounding text is edited and behave atomically for cursor movement, replacement,
and deletion.
Ctrl-G / `/edit` opens the external editor with the full expanded content
(submitted as edited/typed); the returned prefill stays expanded because paste
range identity is intentionally not tracked through the editor. A paste that fills
an empty prompt is marked pure: submitted literally — no `!` shell escape, no
`/command` dispatch, no `$skill` resolution. The literal flag is carried on
the Enter path in every edit mode, including vi normal mode after Esc (so a
paste then Esc then Enter submits literally); only a manual keystroke clears
it. Any manual keystroke after the paste (insert, delete, or cursor motion,
in emacs mode or after entering vi normal mode with Esc) clears that mark, so the whole submitted line is
treated as typed (honoring `!`/`/`/`$`). Pasting into a non-empty prompt inserts
the range at the cursor and collapses it only when it exceeds the byte threshold.
Bracketed paste markers (`\x1b[200~`..`\x1b[201~`) are the primary mechanism; the
complete range is classified once its explicit end marker arrives. Reporting stays
enabled while a model turn is active, and the live steering buffer uses the same
parser, paste classification, display summaries, and timing fallback as the idle
prompt. When a terminal does not honor bracketed paste, a timing-based heuristic
on the interactive TTY
raw line editor detects a fast keystroke burst (bytes arriving within ~10ms of the
previous one, roughly 100 characters per second) and treats newlines in the burst
as inserts instead of submitting, exiting after a ~150ms gap. Because this
fallback is incremental, an inline range may collapse when it crosses 1,000 bytes.
Staying in paste mode too long is the safe failure direction (an extra inserted
newline, never a premature submit). The heuristic is interactive-only and on by
default; `HARNESS_REPL_PASTE_HEURISTIC=off` disables it. For non-TTY input the REPL
keeps the `bufio.Reader` line path, so long scripted prompt lines are not capped by
Scanner's token limit.

At an interactive TTY prompt only, a non-pasted line starting with `!` is a local
shell escape. The command text after `!` runs via the user's shell (`$SHELL -lc`,
falling back to `bash -lc` then `sh -c`), prints directly to the terminal, and
returns to the prompt without a model request, prompt-submit hook, transcript
message, or replay event. `!!` escapes a literal leading `!`; one-shot mode,
initial `-i` prompts, non-TTY/scripted input, bracketed paste, and external-editor
prompt content treat `!text` as ordinary prompt text.

Tab completion is intentionally small and stdlib-only. In normal raw prompt-editor
text, `@path` file references complete filesystem entries from cwd, absolute paths,
or the current user's home directory while preserving the typed prefix; paths that
need quoting are inserted as `@"..."`, and directories keep the cursor inside the
closing quote so completion can continue. `@` completion is skipped for slash-command
lines and single-bang shell escapes, but works on escaped prompt lines such as `//...`
and `!!...`. `$skillName` tokens in normal prompt text complete against the discovered
skill set with case-sensitive prefix matching: a unique match inserts the full name
plus a trailing space, multiple matches extend to the longest common prefix or list
`$name` candidates below the prompt, and no match is a no-op. `$` completion follows
the same line gating as `@` and mirrors the `resolveSkillMentions` escape rule, so an
even-length run of `$` characters (consumed as `$$` escapes) suppresses completion
while an odd-length run ends in a real mention start. When no skills are discovered,
Tab keeps its previous fall-through behavior. In raw prompt-editor buffers that start
with `!`, the first word still
completes executable names from `PATH` unless it starts with `/`, `~/`, `./`, `../`,
or otherwise contains `/`; path words complete filesystem entries with the same path
prefix rules.

The Shift-Tab binding is active only at the idle main interactive TTY
prompt. In emacs mode and in both vi insert and normal modes, it selects the next
configured agent in canonical name-sorted order with wraparound, preserves the
editable draft, and emits the existing `[agent switched: <name>]` notice followed
by the provider/model line. Ordinary Tab remains completion; Shift-Tab has no
agent-switch meaning in other input contexts.

`repl_prompt` (also `-repl-prompt` / `HARNESS_REPL_PROMPT`) is a format string
rendered at every idle prompt boundary, so dynamic values reflect runtime
changes before each read. The default is `[{agent}] > `. Supported placeholders
are `{agent}`, `{cwd}`, `{hostname}`, `{hostname:short}`, `{hostname:long}`,
`{git_branch}`, `{model}`, `{reasoning}`, `{vimode}`, `{vimode:short}`,
`{vimode:long}`, `{context}`, `{context_pct_used}`, `{context_tokens_used}`,
and `{context_tokens_total}`.
`{cwd}` abbreviates the user's home directory
prefix to `~` (for example `~/work`), so the rendered value may differ from the
raw working directory. `{reasoning}` renders the current reasoning profile, such
as `provider default` or `high`, and updates after `/reasoning` changes.
`{hostname}` and `{hostname:short}` render the short OS hostname (the leading
label before the first dot, equivalent to `hostname -s`), while `{hostname:long}`
renders the full OS hostname. Literal escapes `\n`,
`\t`, `\\`, `\{`, and `\}` are decoded
for config, env, and flag values; unknown placeholders or invalid escapes are
configuration errors.

`{vimode}` reflects the current raw-prompt vi edit mode and renders `INSERT`
(in insert mode) or `NORMAL` (in normal mode); `{vimode:long}` is the same,
and `{vimode:short}` renders `I`/`N` instead. In emacs mode (or any non-vi
edit mode) all variants render empty, so they are safe to leave in a shared
prompt. The label updates live as the mode flips mid-edit (Esc enters normal
mode; `i`/`a`/`A`/`I`/`c`/`s`/etc. return to insert mode), re-rendering the
prompt at each transition. The label is plain visible text with no color,
matching the other placeholders.

`{context}` renders the current context-window usage as `<pct>% <used>/<total>`
using the same shortened token display as the turn status line (e.g.
`7% 78.6k/1048.6k`), so it shares `sessionrec.HumanTokens` exactly;
`{context_pct_used}` renders just the integer percentage without the trailing
`%` (e.g. `25`), `{context_tokens_used}` renders the estimated input tokens
used, and `{context_tokens_total}` renders the context window size — both of
the latter use the same `HumanTokens` shortening (e.g. `78.6k`, `12.4k`).
Both `{context}` and `{context_pct_used}` render empty when no window is known
(e.g. no agent or unknown model); otherwise the percentage is
`used*100/total` truncated to an integer and capped to `0..100`. When the
window is unknown but tokens are present, `{context}` falls back to rendering
just the shortened `<used>`. Values come from `agent.EstimateContext()`
computed at prompt render time.

`-repl-edit-mode=vi` (or `HARNESS_REPL_EDIT_MODE=vi` / config `repl_edit_mode`)
switches the raw prompt editor to a small vi keymap. The prompt starts in insert
mode; bare Escape enters normal mode, while terminal escape sequences such as arrow
keys, bracketed paste, and CSI-u key events remain parsed as terminal input. Normal
mode supports `h/l`, `0/^/$`, `w/W/b/B/e/E`, `i/a/I/A`, `x/X`, `D/C/S`, `Y` (yank the
whole line), `k/j` and Up/Down line navigation with history fallback at input
boundaries, `d`/`c`/`y` operators with those motions plus doubled line operators
(`dd`, `cc`, `yy`), and local `p`/`P` paste from the prompt editor's yank buffer.
The `0`/`^`/`$` motions and the `I`/`A`/`D`/`C` shortcuts operate on the current
logical line rather than the whole multi-line buffer, so `d$`/`D`/`c$`/`C` join a
line with the one below it. Counts, registers, search, visual mode, macros, and
full Vim text objects are out of scope.

While a raw vi-mode idle prompt is active, harness also emits xterm DECSCUSR cursor-shape
sequences: a steady bar in insert mode and a steady block in normal mode. The cursor
shape is reset to the terminal default when leaving the prompt or REPL; terminals that
do not support DECSCUSR should ignore the sequences.

Ctrl-G opens the external prompt editor from the raw-mode prompt with the current draft.
During an active REPL turn, the raw editor stays enabled and captures a live steering
buffer with the idle prompt's editing and paste behavior. Its intentional input-routing
exceptions are Ctrl-C and double-Esc, which cancel the active turn; submitted commands
and other non-model-bound input remain queued. Bracketed-paste reporting stays enabled.
On the fallback path without the raw prompt editor, harness instead restores the prompt
terminal mode, configures Escape as the second canonical-mode line delimiter, and
disables bracketed paste until the prompt returns. Before launching the external editor,
harness restores the original termios and disables bracketed paste so the editor owns a
normal TTY; the REPL's input pump reads stdin only while a prompt read is waiting, so
keystrokes typed during the handoff reach the editor rather than resurfacing in the
prompt afterward. After the editor exits, the REPL reapplies its prompt settings.
`!command` shell escapes use the same terminal handoff.

External editor prompt files use `$VISUAL`, then `$EDITOR`, then `vi`, attached to
`/dev/tty`. The temp file contains visible output only from the latest completed
`(prompt, turn)` pair: assistant/reasoning output, that turn's tool results/diffs and
notices, and its `[turn: …]` completion line. It excludes the user prompt, prior turns,
attempt/maintenance accounting, and the aggregate `[prompt: …]` line. A delimiter
line (`--- HARNESS EDIT ... ---`) and any draft text follow. Only content
after the exact delimiter is submitted as the next prompt; edits above the delimiter are
context for the user only. Missing delimiters abort the edit and keep the temp file.
Empty edited content returns to the prompt without running a turn.

### Meta-commands

Lines starting with `/` are commands; `//` escapes a literal slash. At an
interactive TTY prompt, lines starting with `!` run a local shell command; `!!`
escapes a literal bang. In a normal typed prompt, `$skillName` mentions an
available skill anywhere in the text; Harness reads the complete `SKILL.md`,
wraps it in a Codex-style `<skill>` block, and prepends it to the prompt as
persisted user-role transcript content. Re-mention the skill to inject it again
on a later turn. `$$` escapes a literal `$`. Literal `@path` / `@"path with spaces"` references remain prompt text
and never expand file contents; when they point at supported image extensions,
typed REPL prompts, initial `-i` prompts, and one-shot prompts auto-attach the
image if the model supports image input. Pasted and external-editor prompts keep
literal-safety semantics and do not auto-attach from `@` references.

The canonical command inventory and operator-facing behavior live in the
[usage reference](usage.md#repl-commands). `internal/ui.App.command` dispatches
those commands. `/continue` is a dedicated asynchronous host action, not an
empty or synthetic prompt: it bypasses user-prompt hooks, skills, pending images,
goal admission, and user echo, and remains available only in the current process
after the latest model-bound run ends with a non-cancelled API error. Read-only
commands and runtime model/agent/reasoning switches preserve it; a newer admitted
model prompt or successful semantic context replacement clears it. `/compact`
also preserves this API-recovery eligibility because it is an invariant-preserving
rewrite of the same active work. Other state-changing commands update or rotate
their own goal, continuation, and prewarm state as described in their owning
sections. `/clear` starts a new cache affinity; context/model/tool changes
preserve the current conversation affinity.

`internal/goal` is a standard-library-only leaf package. One root `*goal.Store`
holds `{objective, status, continuations, set_at}`. `/goal` shows the current
goal; exact `clear`, `pause`, and `resume` arguments apply those transitions,
while every other non-empty argument sets/replaces the objective, trimming empty
input and capping it at 4,000 Unicode characters, and starts a rendered
continuation as a normal user prompt. Goal state is controlled exclusively by
these slash-command paths; no model-callable goal schemas are registered.

At prompt completion the REPL first drains already-submitted user input, then,
if none is queued and the goal is still active, appends a visible continuation
user prompt. Active goals pause when prompt handling returns `context.Canceled`
(but not on deadline expiry) or after `goal_max_continuations` (default 25, zero
unlimited). Resuming a paused goal resets the count; blocked or complete states
restored from older sessions are also resumable, while resuming an active goal is
rejected. Goal-command, cap, and rejected-continuation transitions checkpoint
immediately; a rejected synthetic prompt pauses without consuming the count.
Reporting completion or a blocker in model text does not mutate goal state.

Each rendered goal prompt carries its store revision. After MCP refresh and
prompt hooks pass, admission atomically revalidates that revision, active status,
and cap before consuming the count or recording the prompt. The agent appends the
prompt-origin user message synchronously under the same store lock, then executes
that admission, closing the validation-to-transcript-insertion race. A stale
prompt is skipped without changing a paused, cleared, or replaced goal.
Cancellation during pre-prompt MCP refresh or submission hooks pauses the
matching goal; deadline expiry does not. Each admitted root prompt also captures
a goal-identity generation in its context so interruption of stale work cannot
pause a user-replaced goal.

Restored active goals continue at the first idle boundary regardless of the
selected agent's tools. A compact XML-escaped reminder is regenerated into
`RequestContext` on every active model round, so compaction cannot erase goal
salience. `Session.Goal` persists status/count/time, `/clear` removes it, and
clone carries it with the other session state. The driver and `/goal` command
are disabled outside the interactive REPL.

Anthropic extended thinking and 1-hour prompt-cache writes appear in the
disjoint reasoning and cache-write buckets defined in §6.

`/model <name>` resolves exactly first, then falls back to a case-insensitive
unique prefix and then unique substring match over the catalog; an ambiguous match
lists the candidates rather than switching. An unknown `/command` prints
`unknown command "<cmd>"; did you mean <suggestion>? (type /help)`, where the
suggestion is the nearest known command by a stdlib Levenshtein distance (shared
prefix wins, threshold `1 + len(cmd)/3`).

### Flags

The exhaustive Harness setting inventory is maintained in the
[usage reference](usage.md#flags). `internal/cli` parses the same immutable,
command-scoped declarations used to generate `-h` output, while
`internal/config` projects its setting catalog into applicable command scopes and
owns source resolution. Configuration resolution remains flags > environment >
file > defaults, except for the documented config-only structured settings.

`harness config show` renders the redacted, source-resolved user-setting
projection and exits without contacting the model proxy. It does not materialize
built-in agents or embed the static or dynamic system prompt.

`-debug-request` resolves the same startup context as a real first turn, then
prints the provider-neutral `llm.Request`, context estimate, active tools,
reasoning settings, and request byte counts. It exits before prewarm,
`SessionStart` hooks, session writes, or any model stream.

`-agents` prints a readable resolved agent list without contacting the model
proxy. `-models` reuses the bounded proxy catalog request and prints configured
model target rows before session creation. `-format json` is supported by
`-agents`, `-models`, and `-check-model-proxy`; JSON output is versioned with
`"version": 1`.

### Hooks

`internal/hooks` implements command-only lifecycle hooks for `SessionStart`,
`UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PreCompact`, `PostCompact`,
and `Stop`.

Config accepts inline hooks:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "shell|edit",
        "hooks": [
          {"type": "command", "command": "./hooks/pre-tool.sh"}
        ]
      }
    ]
  }
}
```

and provider-config-style includes:

```json
{"hook_configs": ["hooks_config.json", "team_hooks.json"]}
```

Relative `hook_configs` paths resolve against the main harness config
directory. Inline hooks and `hook_configs` are additive; all matching hooks run
sequentially in deterministic order. `--hooks <file>` replaces both inline hooks
and `hook_configs` for the process.

Hook commands run in the harness cwd with a JSON event payload on stdin and are
killed as a process group on timeout/cancel. Default timeout is 120 seconds,
capped at 600. Exit code `2`, `{"decision":"block"}`, or `{"continue":false}`
blocks the current operation where the event supports blocking. Plain stdout or
`hookSpecificOutput.additionalContext` becomes hook context. Other non-zero
exits warn and continue.

`Stop` hook JSON may also carry the semantic evaluator fields `accepted`,
`score`, `score_direction`, `candidate`, `remaining_requirements`, and
`evidence_ref`. This result
is deliberately separate from the operational hook `Diagnostic`: process
success does not imply candidate acceptance. `accepted` is required when any
semantic field appears; rejection joins the existing block path and contributes
a bounded structured summary to corrective context. Score is an optional
scalar with presence represented by a pointer, so zero is not conflated with
absence. It remains unordered unless optional `score_direction` is `maximize`
or `minimize`; the direction requires a score and is retained only for
host-owned analysis, not corrective model context. Candidate/evidence identifiers
are single-line and byte-bounded, and remaining requirements are range-bounded.
Contradictory or invalid semantic fields are ignored and classify an otherwise
successful exit-0 command as `parse_failed`, while legacy control fields and
non-zero process outcomes retain their existing behavior.

The agent reports each semantic result through the optional
`EvaluatorResultSink`, and the root sink delegates persistence to
`internal/sessionrec`. Delegate children currently receive no hook runner. The
resulting displayless `evaluator_result` event
contains no reason or command output. If an orchestrator has not supplied
`WorkflowStatus`, an all-accepted set projects to `complete` and any rejection
to `in_progress`; a remaining count is projected only for a single result,
because independent evaluator counts have no generic aggregation rule. A block
not represented by a rejecting semantic result suppresses this projection.

`SessionStart` hooks are the supported path for dynamic prompt context, such as
detecting whether GNU sed is available as `gsed` or reporting the active bash
version. Static personal preferences belong in `~/.agents/AGENTS.md`; command
output belongs in hook context.

### Trajectory, stagnation, and evidence

Harness derives bounded, host-owned stagnation control state
(`internal/trajectory`) from current evaluator, nudge, and branch events; it is
runtime-only and never rendered into prompts, transcripts, tool results, or
model-visible text. Mutation attribution is analyzed separately from canonical
raw events and never enters policy state. The only model-control consumer is
the default-on `stagnation_nudge`
policy: at a no-improvement streak of two during an already blocking Stop-hook
continuation, it persists a payload-free event and appends one generic
strategy-reset instruction to the internal corrective turn. The session
evidence catalog (`internal/session/evidence.go`) is a read-only, human-only
projection over one physical session's events, exposed via `/evidence` and
`harness session evidence`. Deep dive: [trajectory.md](trajectory.md).

### Interactive initial prompt (`-i`)

- `-i` / `-initial-prompt` runs the provided prompt as the first REPL turn, then
  continues normally at the prompt. The initial prompt is literal user text, so
  leading `/` and `!` do not invoke REPL commands or shell escapes.
- `-i` does not read from stdin or append piped stdin; stdin remains available for
  scripted REPL input. `-i -` is a usage error.
- `-image` attaches local image files to the initial `-i` turn only; later REPL
  image attachments use `/image`.

### One-shot mode (`-p`)

- Prompt from the flag value; `-p -` or piped stdin reads stdin (both → flag text, then
  stdin — enables `harness -p "summarize:" < notes.txt`).
- `-image` attaches local PNG, JPEG, WebP, or non-animated GIF files to the
  one-shot prompt; repeat the flag for multiple images. `-image high:path.png`
  overrides the global `-image-detail` for that file.
- **Assistant text → stdout; model progress, tool-call progress, tool summaries,
  usage, errors → stderr.** Timestamps apply only to bracketed status lines on
  stderr, not to assistant text. Terminal stdout renders basic Markdown;
  redirected stdout stays raw model text.
- Exit codes: `0` completed, `1` runtime error, `2` usage error, `130` interrupted.
- Runs exactly one prompt interaction, saves the session, exits.

#### JSON run stream (`-format json`, `internal/runstream`)

`-format json` has two valid top-level stdout shapes. A failure after valid
JSON-mode selection but before stream construction emits exactly one versioned
`startup_error` object (`type`, `v`, `mode`, `exit_code`, `error`, `time`). A
successfully started run is a versioned NDJSON stream (`run_start.v`, currently
`4`): line 1 is a `run_start` envelope, the last line is a best-effort
`run_end` envelope (`exit_code` mirrors the process exit code), and each prompt
is bracketed by `prompt_start`/`prompt_end` (`prompt_end` carries `exit_code`,
`termination_reason`, a usage summary, and `final_text` — the last assistant
text message, extracted the way delegate child reports do). A host-created
boundary carries the same bounded `cause` on both envelopes; ordinary client
prompts omit it so their wire shape is unchanged.
Between the envelopes the stream carries the session's own `session.Event`
objects, mirrored post-coalescing, so stdout and `raw.ndjson` can never
diverge (§11). After valid JSON-mode selection, root wiring captures logical
stderr while startup is pending for a possible `startup_error`; immediately
before `run_start`, it clears that capture and discards both human renderer
paths. `internal/runstream` continues to write directly to raw stdout, while
`diagnostics.ndjson` persistence remains independent of display suppression.
`internal/runstream` owns the whole public schema in both directions — event
envelopes plus the NDJSON input decoder — with doc comments stating the
consumer/producer rules; stdout-only envelopes are never persisted.

Two run modes share the stream:

- **One-shot** (`-p -format json`, `run_start` `"mode":"oneshot"`): exactly
  one prompt; `-format json` without `-p` and TTY stdin is a usage error
  (exit 2), as is `-i` with `-format json`.
- **Interactive** (`-format json`, no `-p`, piped stdin, `"mode":"interactive"`):
  `ui.RunJSON` (`internal/ui/jsonrun.go`, package `ui`) is an alternative
  front end for the REPL's `App` machinery. A decoder goroutine feeds a
  driver state machine (idle / prompt-running / approval-pending); each
  prompt mirrors the REPL's sequence — optional agent/model switch,
  prompt-submit hooks, `beginPrompt`, `newREPLSink` with the recorder mirror
  set, `Agent.RunPromptContentWithContext` on a goroutine with a driver-owned
  ctx+cancel, `FlushEvents`, `prompt_end`, save, boundary work (background
  notices, MCP refresh, pending-handoff check). In-band control replaces
  keystrokes: a bare `prompt` message mid-run steers like Enter-during-prompt
  (late steers are recovered as the next prompt), `interrupt` is the in-band
  ^C, `shutdown` cancels and exits 0, and stdin EOF drains the active/queued
  prompts before exiting so pipe-shaped clients work. Control messages are
  never lost behind prompt completion: messages buffered when a prompt
  finishes are handled before the boundary can start the next queued prompt —
  a raced `shutdown` exits 0 and a raced `interrupt` exits 130 instead of
  cancelling the next prompt. `interrupt` with a pending handoff approval
  cancels the handoff and exits 130 (the TTY approval Ctrl-C path); stdin EOF
  with a pending approval declines the handoff (never auto-approves) and
  drains the queue. Steering is attempted only while no earlier input is
  queued, which keeps queued and recovered input in submission order without
  sequence numbers. A resolved detached background wait wakes an otherwise idle
  JSON driver and starts a `cause:"detached_background_wait"` continuation only
  when no client input, approval, or EOF is pending; its aggregate is drained as
  request-only context by that continuation's first model request. The input-reader
  goroutine aborts blocked sends on force-exit so it cannot leak. Handoff approval uses
  the protocol (`approval_request`/`approval_response`) with the same
  post-approval helper the TTY `/handoff` flow calls. TTY-coupled behaviors
  (goals, slash commands, idle compaction, pickers, history) stay off: main
  wires the mode through an explicit `machineInteractive` predicate, and bad
  input lines surface as `input_error` events without killing the session.

## 11. Session persistence (`internal/session`)

A session path is a directory: `tree.ndjson` is the canonical append-only
conversation tree (first record a session header, later records immutable
typed entries), `state.json` stores mutable runtime state and the active leaf,
`active-turn.json` is a transient atomic recovery record for the current
provider boundary, `raw.ndjson` is the chronological replay stream,
`diagnostics.ndjson` stores JSON slog diagnostics, `compactions/` archives
messages removed from active context, and `artifacts/tool-results/` stores
full truncated tool output. All session writes use temp-file then rename. The
`state.json` schema is versioned and intentionally breaking across versions.

`internal/sessionrec` is the one canonical `raw.ndjson` recorder, shared by
the parent UI sink and delegate child sessions; a parity test pins identical
output from both paths. Resume reconstructs active provider context by walking
parents from the active leaf, with `active-turn.json` recovery applied when
newer; branching never splits a tool_use/tool_result pair across segments, so
the §4 invariant survives navigation.

The storage layout, save/recovery ordering, replay event vocabulary, branching
and extraction semantics, child-session metadata, the
analyze/replay/timings/stats/errors inspection commands, and REPL history are
documented in [session.md](session.md).

## 12. Compaction (`internal/agent/compact.go`)

Compaction runs when `max(reported input tokens, estimated full-request
footprint)` reaches `compact_trigger_percent` (default 78%) of the effective
context window — proactively between turns, after a prompt, or manually via
`/compact`. A provider-native compaction endpoint is used when the selected
target advertises it; otherwise removed history is summarized by the model,
with a deterministic sparse checkpoint as the failure fallback.

The model-facing contract: removed history is replaced by one synthetic
**user** checkpoint message headed `=== Compaction checkpoint ===` carrying
the active prompt and any steering instructions verbatim, then the progress
summary, the recognized file-activity index, and the raw archive reference.
Kept turns are whole turns, so the §4 tool_use/tool_result invariant is never
split; the system prompt is never summarized because it lives on
`Request.System`. Before any model summarization, a pure-local retention pass
may trim eligible old tool results to archived receipts without a model
round-trip.

Trigger policy and hysteresis, retention eligibility and epochs, the
summarization algorithm and its hardening, checkpoint/tree persistence,
degradation, the deterministic fallback, and speculative idle compaction are
documented in [compaction.md](compaction.md).

## 13. Testing strategy

Seams that make the system testable: the `Provider` interface (scripted `FakeProvider`),
the `Tool` interface + registry, REPL via injected `io.Reader`/`io.Writer` (TTY detection
injectable), the retry clock, and `ValidateTranscript`.

| Layer | Tests |
|---|---|
| `internal/sse` | frame parsing tables; huge frames; truncated input |
| providers | `httptest.Server` replaying `.sse` golden fixtures per dialect → assert ordered events; golden request-JSON tests (Responses input items, Chat role:tool hoisting, args-string vs object, system placement, `stream_options`, cache_control); tool-call reassembly tables (fragment splits, empty args → `{}`, interleaved parallel calls, invalid tail → invalid `Done` diagnostic); truncated stream; mid-stream cancellation; retry loop via injected sleeper (429-then-200, 400 immediate failure, budget exhaustion) |
| `internal/retry` | `Next`: jitter bounds, 30s cap, Retry-After floor |
| tools | table-driven against `t.TempDir()` for `read`, directory reads, `edit`, and `write`; `git` against a scratch `git init` repo (skipped if git is absent); host discovery commands and timeout/cancellation through `shell` |
| agent loop | `FakeProvider` scripts: multi-tool batches, error-result feedback (next request carries the error), max-turns stop, cancellation → transcript still re-sendable |
| delegate | child-agent request shape and child-only prompt suffix, model-visible compatible-agent enum/catalog (ordering, normalization, caps), parent-tool subset rejection, depth transitions/deepest-child removal, recursive runtime rebinding, inherited token/cost budgets, private child TODO/plan stores, child transcript persistence, metered usage folded into parent prompt totals |
| background | job start/completion, one-shot context delivery, notices, cancellation/errors, child transcript path preservation |
| session | save→load→save round-trip; atomic rename leaves no `.tmp`; resume repair; cross-provider resume |
| compaction | canned summary via FakeProvider; old messages collapse, last 4 turns kept; invariant holds |
| ui | scripted REPL input (`/help`, prompt, `/exit`); rendering goldens with fake clock/usage |

Cross-cutting: `ValidateTranscript` is asserted after every transcript mutation in every
test that touches one.

Beyond the unit tables, `//go:build integration` suites build the real binaries and drive
them as subprocesses against hermetic local mock servers (no API keys, no network);
they live in `cmd/harness` and run with `make test-integration`. Opt-in upstream
model checks use the `livemodel` tag via `make test-live-models`. The smoke-test
matrix and its pass criteria are documented in [smoke.md](smoke.md).

## 14. Agent definitions (`internal/agentdef`)

An **agent definition** is a named bundle of an allowed-tool set, optional
model target, description, and extra system-prompt instructions. It lets one
harness behave as a collaborative planner, autonomous worker, specialized
reviewer, or a general-purpose agent without separate binaries.

- **Selection** follows the standard precedence (§7): `-agent` flag >
  `HARNESS_AGENT` > `agent` in the config file > the built-in default `auto`. An
  empty value means "unspecified", so a resumed session's saved agent (§11) can
  supply it before the `auto` fallback. `interactive_selectable` gates only root
  interactive startup and explicit switches — not one-shot runs, delegation,
  child continuation, or plan handoff targets — and Shift-Tab cycling invokes
  the same full runtime selection path as `/agent`, recomposing prompt, tools,
  model/reasoning selection, and continuation state. The operational guide,
  including the built-in agent table, is [usage.md "Agents"](usage.md#agents).
- **Built-in contract details:** `auto` and `independent` advertise `git`
  rather than the redundant `git_readonly`; delegation treats `git` as
  satisfying a child's `git_readonly` requirement. Child-local `update_todos`
  and `record_plan` are exempt from parent capability-subset matching.
  Interactive root `plan` sessions additionally expose `handoff`; delegated and
  one-shot plan agents do not.
- **Descriptions are required selection metadata:** after resolution, every agent
  must have a nonblank trimmed `description` stating when a parent should use it.
  A new custom name without one is a fail-fast startup/`--agents`/`harness config
  show`/`harness config check` error; there is no warning, generated fallback, or compatibility shim.
- **Config `agents`** entries **field-level merge** onto a built-in of the same name:
  a non-empty `description`, `allowed_tools`, `mcp_tools`, `prompt`, `model`, or
  `reasoning` replaces, and an omitted field inherits. `interactive_selectable`
  is presence-aware, so explicit false replaces while omission inherits. Thus
  an override
  of `auto`, `explore`, `plan`, `review`, or `independent` may inherit its built-in
  description and interactive-selection policy. A new name defines a new agent
  (`interactive_selectable` defaults to true; no `allowed_tools` ⇒ the full
  default set). Agent prompts
  accept `@file` and are expanded once at startup (fail-fast); relative config-file
  references resolve from the config file directory.
- **MCP exposure:** `mcp_tools` is one of `disabled`, `read_only`, or `all` (with
  `read-only`/`readonly` accepted as aliases for `read_only`) and controls automatic
  exposure of discovered MCP tools. An invalid value is a fail-fast validation error
  (surfaced by startup or `harness config show|check` after field-level merging). Built-ins
  default to `all` for `auto`/`independent` and `read_only` for
  `explore`/`plan`/`review`; a new agent with no explicit `allowed_tools`
  defaults to `all`, while an explicit `allowed_tools` whitelist defaults to
  `disabled` unless `mcp_tools` opts it back in. Explicit `mcp__...` names in
  `allowed_tools` remain strict whitelist entries.
- **Model:** an agent without `model` uses the current session target; setting
  `model` pins a complete `<provider>:<model>` catalog target ID. `/agent <name>`
  warns when a switch changes it because prompt cache may start cold.
- **Per-agent reasoning:** an agent's optional `reasoning` field pins its thinking
  effort. It overrides the session base effort whenever that agent is selected
  (startup, `/agent`, delegate, or a handoff target) and is then made
  model-compatible and validated like any effort. This lets a cheap implementation
  agent pair a smaller `model` with a lower `reasoning`.
- **Plan → implementation handoff:** the `plan` agent writes a self-contained
  artifact with `record_plan` (`Record a complete implementation plan for handoff to an implementation agent.`, §9.17) and requests a handoff with
  `handoff` (`Handoff the recorded plan to an implementation agent.`, §9.17). The `handoff` `agent` enum contains only exclusive agents (`auto`, `independent`, plus custom `workspace_access: exclusive`; `explore`/`plan`/`review` excluded); omit for default `auto`.
  At the next prompt boundary, or on manual `/handoff` (§10), the REPL prompts for
  approval, archives the planning transcript via `SaveCompaction`, switches to
  the target agent — default `auto`, overridable by
  `--handoff-agent`/`HARNESS_HANDOFF_AGENT`/`handoff_agent` or `/handoff -a
  <agent>` — optionally swaps the model for a manual `/handoff -m <model>`, then
  reseeds a clean transcript with the complete latest plan and any
  trailing `/handoff` user message as a separate section before submitting a
  fixed implementation-start prompt. Reusing
  the same in-session
  switch (not `delegate`) avoids the `delegate` subset gate, so the
  `plan` agent — which has no file-mutation tools — can hand off to a
  write-capable implementation agent. Interactive REPL only.
- **Tool gating** is the harness's one departure from the no-sandbox stance (§2): the
  agent's tool set is realized by `tools.Registry.Subset`, building a registry that
  holds only the allowed tools. Because the agent advertises (`Specs`) and dispatches
  from the same registry, an excluded tool is neither offered nor callable. The
  underlying tools still assume an external sandbox for real isolation; gating only
  shapes what each agent exposes. `Agent.SetTools` swaps the registry for `/agent`.
- The agent prompt is appended to the composed system prompt as the final
  section, so it layers on top of the static instructions, env block,
  user/project AGENTS.md, skills catalog, and runtime capability hints. A configured `system_prompt` replaces the
  static instructions before the runtime sections are added. The active agent is
  saved with the session and restored on `-resume` (flags win).

## 15. MCP proxy (optional)

Remote MCP support is opt-in (`mcp.enable`, §7) and lives behind a **second
binary**, `harness-mcp-proxy`. Harness does not talk to remote downstream MCP
servers directly in that path: the proxy owns them and presents their merged
tools to harness as a single MCP server over streamable HTTP (JSON-RPC 2.0,
protocol revision `2025-06-18`; `internal/mcp`, `internal/mcp/jsonrpc`).
Separately, `mcp.local` is an explicit local-stdio slot where harness itself
spawns one configured command and registers its tools.

**Why a separate process.** The remote daemon decouples downstream-server
lifetime from any one harness session: stdio children are spawned once and
shared across every concurrent harness session, surviving REPL restarts. The
harness side depends on the thin `internal/mcptools` adapter for tool dispatch
(§9.16).

Architecture rules:

- Harness **never starts the remote proxy** and MCP never fails harness
  startup: one-shot runs register under a 5 s timeout with one warning;
  interactive runs register in the background and apply at a prompt boundary.
- Each downstream server gets a supervisor: stdio children run in their own
  process group with a 30 s initialize/`tools/list` timeout, restart with
  backoff, and are permanently disabled after 5 consecutive failed (re)starts;
  streamable-HTTP servers connect lazily (no restart loop — the process is not
  ours) and get one transparent re-initialize on session expiry (HTTP 404). A
  not-ready server yields an `isError` tool result, not a JSON-RPC error, so
  the failure reaches the model as a normal tool failure.
- Tools merge under `mcp__<server>__<tool>` with a reverse route map; the
  provider-safe charset check applies to the entire qualified string, and a
  failing name is dropped with a warning, never truncated (truncation could
  collide and misroute).
- The proxy's HTTP handler is tools-only and JSON-only (never
  `text/event-stream`); sessions are carried by `Mcp-Session-Id` with a
  30-minute idle TTL (distinct from the HTTP server's 120 s connection
  IdleTimeout), `DELETE` ends a session, and with no server-push channel
  `ListChanged` is reported false.

Configuration, serving and lifecycle operations, metrics, authentication,
request logging, and exposure caps are documented in [mcp.md](mcp.md).

## 15a. LSP code intelligence (optional)

The **LSP manager** (`internal/lspproxy`) launches already-installed language
servers on demand and exposes 21 native LSP tools: navigation, structural
inspection, diagnostics, and change workflows (read-only action/format/rename
plans plus mutating text-edit application). The normal path is first-class,
not generic MCP: `lsp.enable=true` registers short `lsp_*` tools directly
through `internal/lsptools` (§9.16a) while `internal/lspproxy` owns the
language-server supervisors. This is distinct from the secrets-isolated remote
`harness-mcp-proxy`, because a language server needs local filesystem access.

- Tool annotations drive scheduling and agent exposure: read-only LSP tools
  join the read-only gate, while `lsp_code_action`, `lsp_format_document`, and
  `lsp_rename` are mutating.
- `/lsp enable|disable` is a process-local runtime override that re-derives
  every agent's allowed tools, updates registry and system prompt together,
  and resets provider continuation state.
- After a successful built-in `edit`/`write`, the CLI synchronizes up to eight
  changed files through the manager's ordinary didOpen/didChange path and
  appends fresh diagnostics to the tool result (supplemental, never failing
  the mutation).
- Serena support is independent of native LSP: `lsp.serena.enable=true` starts
  a local stdio MCP child registered as `mcp__serena__<tool>`.
- `harness lsp serve` is a compatibility stdio MCP shim over the same manager
  for proxy-hosted setups.
- `internal/lspproxy` is stdlib-only and hand-rolls the LSP client:
  Content-Length JSON-RPC over stdio, per-`(server, workspace-root)`
  supervisors with backoff and cooldown-revive, full-text sync, and async
  publishDiagnostics. Language-server commands are never executed;
  WorkspaceEdit file operations are rejected; mutating paths validate every
  text range before any write.

Setup, prewarming, the tool table and parameter conventions, server
configuration, and hosting topologies are documented in [lsp.md](lsp.md).

## 16. Future work

- CLI-subprocess backends (codex / claude) behind a separate process-worker abstraction.
- Benchmark provider-native compaction against typed textual checkpoints on
  long tool-heavy sessions, including latency, token cost, cache behavior, and
  answer quality after provider switches.
- Explicit workspace isolation or conflict control for read/write delegate agents.
- MCP resources and prompts, the legacy HTTP+SSE downstream transport (the deprecated
  2024 GET-stream MCP transport — distinct from the already-implemented streamable-HTTP
  transport in `internal/mcp`), and OAuth discovery/dynamic client registration for
  remote servers.
- Smarter prompt-cache breakpoint placement: placement is static today. Splitting the
  volatile env block (date/git) out of the cached system prefix would improve
  cross-session reuse; content-aware anchoring could further help compaction-heavy
  sessions.
