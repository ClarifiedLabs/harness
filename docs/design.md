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
- **Unix philosophy for tools.** When the job is already owned by a mature host CLI
  (`grep`, `rg`, `git`, shell commands), expose a thin argv wrapper instead of
  reimplementing optimized search or command semantics in the harness.
- **Provider access is isolated.** `harness` uses one provider-neutral
  message/streaming model and talks to `harness-model-proxy` over HTTP. The proxy
  owns API keys, provider configs, model metadata, and the Anthropic/OpenAI
  dialects; the main CLI sees only a catalog and normalized stream events.
- **No sandboxing or permission prompts.** The harness assumes it is launched inside an
  already-sandboxed environment. Tools run with the process's privileges, immediately.
- **First-class git.** A dedicated `git` tool plus git context in the system prompt.

## 2. Constraints

| Constraint | Choice |
|---|---|
| Language | Go 1.26 (`iter` / range-over-func used) |
| Dependencies | stdlib only |
| Module / binary | `module harness`, binary built from `cmd/harness` |
| Interface | line-oriented plain text; basic Markdown on terminal output; optional ANSI color only when stdout is a TTY; `NO_COLOR` and `-no-color` disable color |
| Secrets | API keys live in `harness-model-proxy`; the `harness` process talks to it over HTTP |

## 3. Architecture

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
cmd/harness/main.go      flags, config load, proxy catalog wiring, signal setup, REPL-vs-oneshot dispatch (also hosts `harness lsp serve`, `harness session replay|timings`)
cmd/harness-model-proxy  provider setup/refresh and HTTP model proxy server; `auth` subcommand (login/logout/status)
internal/modelproxy      proxy protocol, client Provider, server handler
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
internal/session         session state, replay log, compaction archives, tool artifacts
internal/config          flags > env > config-file resolution
internal/modelsdev       optional models.dev catalog reduction for proxy setup/pricing metadata
internal/ui              REPL, streaming renderer, tool summaries, usage line
internal/sysprompt       embedded prompt files + environment context + AGENTS.md sections
internal/agentdef        agent definitions (allowed tools, MCP exposure, prompt/provider/model) (§14)
internal/hooks           command-only lifecycle hooks (SessionStart/UserPromptSubmit/Pre+PostToolUse/Pre+PostCompact/Stop)
internal/skills          skill discovery + `$skillName` prompt expansion
internal/todo            update_todos store + render (§9.13)
internal/plan            record_plan store + handoff request holder (§9.17, §9.18, §14)
internal/auth            provider auth sources (token_command, oauth2, codex_oauth) for the model proxy
cmd/harness-mcp-proxy  optional MCP proxy daemon + debug client (serve / tools / auth / version)
internal/mcp             tools-only MCP slice: schema, client, server, stdio + streamable-HTTP transports
internal/mcp/jsonrpc     JSON-RPC 2.0 framing and bidirectional request/response correlation
internal/mcpproxy      proxy internals: config, supervisors, tool registry, daemon
internal/mcptools        harness-side adapter: tools.Tool over a reconnecting proxy Conn (§15)
internal/lspproxy      LSP manager: language-server supervisors, Content-Length JSON-RPC, navigation tools (§15a)
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

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    // No tool role: tool results are content blocks on a user message.
    // No system role: the system prompt is a Request field, not a message.
)

type Message struct {
    Role    Role           `json:"role"`
    Time    time.Time      `json:"time,omitempty"`
    Phase   string         `json:"phase,omitempty"` // assistant only: commentary | final_answer
    Content []ContentBlock `json:"content"`
}

type BlockKind string

const (
    BlockText       BlockKind = "text"
    BlockImage      BlockKind = "image"
    BlockToolUse    BlockKind = "tool_use"
    BlockToolResult BlockKind = "tool_result"
)

// ContentBlock is a tagged union; exactly the fields for Kind are set.
type ContentBlock struct {
    Kind BlockKind `json:"kind"`

    // BlockText
    Text string `json:"text,omitempty"`

    // BlockImage (user-provided visual input)
    ImageMediaType string `json:"image_media_type,omitempty"`
    ImageData      string `json:"image_data,omitempty"` // base64, without data: prefix
    ImageDetail    string `json:"image_detail,omitempty"`
    ImageName      string `json:"image_name,omitempty"`
    ImageWidth     int    `json:"image_width,omitempty"`
    ImageHeight    int    `json:"image_height,omitempty"`

    // BlockToolUse (assistant calls a tool)
    ToolUseID string          `json:"tool_use_id,omitempty"` // provider-issued call id
    ToolName  string          `json:"tool_name,omitempty"`
    ToolInput json.RawMessage `json:"tool_input,omitempty"`  // complete JSON object

    // BlockToolResult (we answer a tool call)
    ResultForID string `json:"result_for_id,omitempty"` // matches a ToolUseID
    ResultText  string `json:"result_text,omitempty"`
    ResultError bool   `json:"result_error,omitempty"`
}
```

Design notes:

- **System prompt lives on `Request.System`,** not in the message list. This is the
  natural Anthropic shape, trivially becomes a leading `role:"system"` message for
  OpenAI, and means compaction can never accidentally summarize it away.
- **`ToolInput` is `json.RawMessage`,** not `map[string]any`: it arrives as a byte stream,
  the tool layer decodes it into its own typed struct anyway, and raw bytes round-trip
  through session files without re-encoding surprises.
- **JSON tags are provider-neutral** (`kind`, `tool_use_id`, …). Session files never
  contain raw provider wire JSON, so a session started against Anthropic resumes
  against an OpenAI-compatible server and vice versa.

Two small seam types carry tool traffic between the agent loop and the tool layer;
they are flat views of the corresponding content blocks:

```go
type ToolCall struct { // from a BlockToolUse
    ID    string
    Name  string
    Input json.RawMessage
}

type ToolResult struct { // becomes a BlockToolResult
    ForID         string
    Text          string
    IsError       bool
    Truncated     bool   // central cap (§8.3) trimmed the result
    OriginalText  string // full pre-truncation text, archived to artifacts/
    OriginalBytes int    // size before truncation
    ShownBytes    int    // size after truncation
    Usage         Usage  // metered tools (e.g. delegate) report child token usage
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

| Internal | OpenAI Chat Completions | Anthropic Messages |
|---|---|---|
| `Request.System` | leading `{"role":"system","content":…}` message | top-level `"system"` string |
| user text | `{"role":"user","content":"…"}` | `{"role":"user","content":[{"type":"text",…}]}` |
| user image | `{"type":"image_url","image_url":{"url":"data:<media>;base64,<data>","detail":…}}` inside structured user content | `{"type":"image","source":{"type":"base64","media_type":…,"data":…}}` |
| assistant text + tool_use | `{"role":"assistant","content":"…","tool_calls":[{"id","type":"function","function":{"name","arguments":<JSON-string>}}]}` | `{"role":"assistant","content":[{"type":"text",…},{"type":"tool_use","id","name","input":<object>}]}` |
| tool_result | separate `{"role":"tool","tool_call_id":…,"content":…}` message per result | `{"type":"tool_result","tool_use_id":…,"content":…,"is_error":…}` block inside a user message |

Mapping subtleties that must be handled:

- OpenAI `function.arguments` is a JSON **string** (`"{\"path\":\"x\"}"`); Anthropic
  `input` is a JSON **object**. A call with no arguments must serialize as `"{}"` for
  OpenAI, never `""`.
- OpenAI tool results are **sibling messages, not blocks**: each `BlockToolResult` is
  hoisted into its own `role:"tool"` message, placed immediately after the assistant
  message that issued the calls, in call order.
- OpenAI has no `is_error` field on tool messages; error results are prefixed
  `ERROR: ` in the content string. Anthropic gets `is_error: true`.
- An assistant message with tool calls but no text serializes with `content` omitted
  (OpenAI) / no text block (Anthropic).
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

type Request struct {
    Model       string
    System      string
    Messages    []Message
    Tools       []ToolSchema
    MaxTokens   int      // 0 = provider policy (see §5.4)
    Temperature *float64 // nil = omit
    Reasoning   ReasoningConfig
    StopSeqs    []string
    StoreResponse      bool
    PreviousResponseID string
    RequestContext     []string // request-only hook/todo/background context
    PromptCacheKey     string   // stable per-agent cache key (OpenAI/Responses); see §5.4
    LongCacheTTL       bool     // Anthropic 1h cache TTL; set only for interactive sessions
}

type ReasoningConfig struct {
    Effort       string // empty = provider default
    Enabled      *bool  // nil = provider default
    BudgetTokens *int   // nil = provider default
    Summary      string // Responses API summary: auto, concise, detailed; empty = omit
}

type ToolSchema struct {
    Name        string
    Description string
    Parameters  json.RawMessage // JSON Schema object, owned by the tool layer
}
```

`RequestContext` is rendered as a trailing synthetic user message for stateless
Chat Completions, Anthropic, and Responses calls. For stored Responses calls it is
merged into request instructions so fresh todo/background/hook context applies to
the current request without becoming part of the OpenAI stored response chain.
Responses streams surface `response.id` on terminal `EventDone.ResponseID`; the
agent stores that with the local transcript anchor for optional
`previous_response_id` continuation.

`iter.Seq2[StreamEvent, error]` (range-over-func) was chosen over channels: the consumer
is a plain `for ev, err := range stream` with natural early-`break` cancellation, and the
producer keeps stream state on its own stack — no goroutine lifecycle to leak.

```go
type EventKind int

const (
    EventTextDelta     EventKind = iota // incremental assistant text
    EventToolCallStart                  // tool_use began: ID + Name known
    EventToolCallDelta                  // partial JSON args (rendering only)
    EventToolCallDone                   // one call fully assembled
    EventUsage                          // usage snapshot (may arrive >1x)
    EventDone                           // turn end: StopReason + final Usage
    EventReasoningSummary               // display-ready provider-visible reasoning summary text
    EventAssistantPhase                 // assistant message phase metadata
)

type StreamEvent struct {
    Kind EventKind

    Text  string // EventTextDelta / EventReasoningSummary
    Phase string // EventAssistantPhase

    // EventToolCall*; Index disambiguates parallel calls within one turn.
    Index     int
    ToolID    string          // Start/Done
    ToolName  string          // Start/Done
    ArgsDelta string          // Delta
    ToolInput json.RawMessage // Done only: complete, valid JSON

    Usage      *Usage     // EventUsage / EventDone
    StopReason StopReason // EventDone
    ResponseID string     // EventDone, Responses API stored-response anchor
}

type StopReason string

const (
    StopEndTurn   StopReason = "end_turn"
    StopToolUse   StopReason = "tool_use"
    StopMaxTokens StopReason = "max_tokens"
    StopStop      StopReason = "stop" // stop sequence matched
)
```

StopReason normalization: OpenAI `stop|length|tool_calls` and Anthropic
`end_turn|max_tokens|tool_use|stop_sequence` map onto the four constants. Unknown or
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
    `data: [DONE]` terminates.
  - **OpenAI Responses:** typed frames such as `response.output_text.delta`,
    `response.output_text.done`, `response.refusal.delta`, `response.refusal.done`,
    `response.content_part.done`, `response.output_item.added`,
    `response.output_item.done`, `response.reasoning_summary_text.delta`,
    `response.reasoning_summary_text.done`, `response.reasoning_summary_part.done`,
    `response.function_call_arguments.delta`, `response.function_call_arguments.done`,
    `response.completed`, `response.incomplete`, `response.failed`, and a bare
    terminal `error` frame. Assistant message `phase` metadata from output items is
    preserved on transcript messages.
  - **Anthropic:** typed frames — `message_start`, `content_block_start`,
    `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`,
    `ping` (ignored), `error` (terminal stream error; retryability follows type).
- **Truncated stream:** body EOF without the dialect terminator (`[DONE]`,
  `response.completed` / `response.incomplete` / `response.failed`, or
  `message_stop`) → `ErrTruncatedStream`. The agent may re-request the step from
  scratch when the terminal error is retryable; failed-attempt usage still counts.
- Cancellation rides on the HTTP request context: cancelling unblocks the body read and
  the iterator yields `ctx.Err()` as its terminal error.

### 5.3 Streaming tool-call assembly

Providers emit granular `Start`/`Delta` events for live rendering **and** guarantee that
`EventToolCallDone.ToolInput` is complete, valid JSON. The agent loop forwards
`Start`/`Delta` to the renderer, but only `Done` affects transcript mutation and tool
dispatch. Assembly is per-turn state inside each provider's `Stream`:

- **OpenAI:** `choices[].delta.tool_calls[]` arrive with an `index`; the first delta for
  an index carries `id` + `function.name` (emit `Start`), subsequent deltas carry
  `function.arguments` string fragments (emit `Delta`). All buffered calls flush as
  `Done` when `finish_reason: "tool_calls"` arrives.
- **Anthropic:** `content_block_start` with `type:"tool_use"` gives `id` + `name` at a
  block index (`Start`); `content_block_delta` with `input_json_delta` carries
  `partial_json` fragments (`Delta`); `content_block_stop` flushes that call (`Done`).

Edge cases:

- **Empty arguments:** OpenAI may send zero fragments; an empty buffer flushes as `{}`.
- **Validation on flush:** `json.Valid` is checked before emitting `Done`; invalid
  accumulated JSON is a retryable terminal stream error, never a garbage `Done`.
- **Parallel calls:** both dialects interleave multiple calls; `Index` keeps them
  distinct and emission order is preserved into the transcript.
- **Interleaved text and tool_use** (Anthropic): text blocks share the index space but
  bypass the assembler.

### 5.4 Request building

| Concern | OpenAI Responses | OpenAI Chat Completions | Anthropic Messages |
|---|---|---|---|
| Endpoint default | `https://api.openai.com/v1/responses` | `https://api.openai.com/v1/chat/completions` | `https://api.anthropic.com/v1/messages` |
| Auth | `Authorization: Bearer <key>` | same | `x-api-key: <key>` + `anthropic-version: 2023-06-01` |
| Tool schemas | `tools[] = {type:"function", name, description, parameters, strict:false}` | `tools[].function = {name, description, parameters}` (`type:"function"`) | `tools[] = {name, description, input_schema}` |
| Parallel tool hint | `parallel_tool_calls:true` when tools are present | `parallel_tool_calls:true` when tools are present | not sent |
| Prompt cache key | `prompt_cache_key` from `Request.PromptCacheKey` | `prompt_cache_key` from `Request.PromptCacheKey` | not sent (explicit `cache_control` breakpoints instead) |
| Stateful continuation | `store` is always sent — `store:true` plus `previous_response_id` when proxy catalog reports `responses_stateful:true` (tools/system still sent each request), `store:false` for the stateless default | ignored | ignored |
| Assistant phase | assistant `message` input items include stored `phase` (`commentary` or `final_answer`) when present | ignored | ignored |
| Token cap | `max_output_tokens = outputLimit when known, else min(32768, contextWindow/4)` when unset (omitted if disabled or window unknown) | `max_tokens = outputLimit when known, else min(32768, contextWindow/4)` when unset (omitted if the window is unknown) | `max_tokens` is required; if unset, `outputLimit when known, else min(32768, contextWindow/4)` |
| Streaming usage | final `response.usage` on terminal events | `"stream_options":{"include_usage":true}` (always set) | automatic: input tokens in `message_start`, output in `message_delta` |
| Stop sequences | not sent | `stop` | `stop_sequences` |
| Temperature | omitted when nil (never send a spurious 0) | same | same |
| Reasoning controls | effort: `reasoning.effort`; summary: `reasoning.summary`; budget/toggle not sent | OpenAI: `reasoning_effort`; OpenRouter: `reasoning.effort`, `reasoning.max_tokens`, `reasoning.enabled` | effort: `output_config.effort`; budget: `thinking={type:"enabled", budget_tokens}`; explicit reasoning-off sends `thinking={type:"disabled"}` |

The same model-facing `ToolSchema.Parameters` bytes go into `parameters` vs
`input_schema`. Harness strips nested JSON Schema `description` fields before
advertising tools; each tool's top-level description remains the explanatory text.

**Default `max_tokens` cap (`defaultMaxTokensCap = 32768`).** When the user does
not set `MaxTokens`, all three dialects bound a single response at the model's
catalog `output_limit` when known, otherwise at `min(32768, contextWindow/4)`.
This is a client-side runaway brake, separate from the turn-level budgets.
Anthropic always sends the computed value (`max_tokens` is required); OpenAI Chat
Completions and Responses send `max_tokens` / `max_output_tokens` only when the
value is known. Responses providers can set `omit_max_output_tokens` when a
compatible backend rejects the standard parameter.

**Prompt cache key (`prompt_cache_key`).** OpenAI Chat Completions and Responses
emit `Request.PromptCacheKey`, a stable per-agent value: an FNV-64a hash of the
system prompt plus the advertised tool names, rendered `harness-<hex>`. It is
identical across a session's turns and its startup prewarm, and changes on an
agent/model switch that alters the system prompt or tool set, so the provider's
automatic prefix cache keys consistently. Anthropic does not use it (it pins
explicit `cache_control` breakpoints).

**Responses reasoning persistence.** In the default stateless (`store:false`) mode
the provider would otherwise re-derive chain-of-thought on every tool turn. For a
reasoning request harness sends `include: ["reasoning.encrypted_content"]`,
captures each reasoning item's id and `encrypted_content`, and persists it on the
transcript as a `BlockReasoning` content block (§4). On the next request
`buildInput` re-emits that as a `reasoning` input item immediately before its
`function_call`, so reasoning is replayed rather than recomputed. The replay is
gated on the request itself being a reasoning request — a reasoning-off call
(compaction summary, prewarm) drops the encrypted items, since a reasoning input
item without the matching `include` is rejected.

**Anthropic prompt caching (v2):** `cache_control: {"type":"ephemeral"}` breakpoints on
all **four** allowed positions, refreshed every call: the last entry of the tool-schema
array (the static prefix, so it survives a system-prompt/agent switch), the system block,
and the last two content blocks of the persisted transcript — the last real message (the
rolling write point read back next turn) and the previous real message (a stable anchor
that lags a turn, keeping reads within the 20-block lookback on long tool-heavy steps).
The two stable anchors (system + last tool) use a 1-hour TTL **only for interactive
(TTY) sessions** (`Request.LongCacheTTL`, set from `Options.Interactive`) — written
~once and read every turn, so the long TTL survives multi-minute pauses for a
one-time doubled write — while one-shot and delegate runs keep the 5-minute default
on all four breakpoints to avoid paying the 1h write premium on a short-lived
session. The rolling message anchors always keep the 5-minute default. An interactive (TTY) session
also fires a background `max_tokens:1` warm-up at startup so the first real request reads
a warm prefix instead of paying the cold write.
Crucially, the message breakpoints land on the persisted transcript, **not** on the
volatile request-only context (todo/hook reminders) appended after it: pinning the
breakpoint to per-turn content — as v1 did — meant the message prefix never matched across
turns, so only the system and tool anchors ever cache-read while the whole transcript was
re-billed at full rate. An agentic loop re-sends a growing prefix every step; caching makes
that prefix ~10× cheaper. OpenAI caches automatically (longest stable prefix), so its
trailing volatile context is harmless and no opt-in exists or is needed.

### 5.5 Errors and retries (`internal/retry`)

```go
type APIError struct {
    StatusCode int
    Code       string        // provider error code/type if parseable
    Message    string
    Retryable  bool
    RetryAfter time.Duration // parsed Retry-After, 0 if absent
}
```

- **Retryable:** HTTP 429, 500, 502, 503, 529 (Anthropic overloaded), and transport
  errors (timeouts, resets, DNS).
- **Fatal, no retry:** 400, 401, 403, 404, 422 — surfaced immediately with the
  provider's error message.
- **Backoff:** full jitter — `sleep = rand(0, min(30s, 500ms·2^attempt))`, 5 attempts.
  `Retry-After` (seconds or HTTP-date) is honored as a floor. The policy is a pure
  function (`retry.Next(attempt, retryAfter) time.Duration`); the retry loop itself is
  the shared `llm.Connect`, which every dialect calls with its endpoint, auth headers,
  and error-body parser, and which takes an injected `sleep` so tests run instantly.
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
  available. This hint parsing applies uniformly to terminal stream errors, including
  Responses `response.failed` and bare `error` frames.
- **Cancellation wins:** `ctx.Err()` is checked before every attempt and every backoff
  sleep, and is distinguished from `APIError` so the UI renders "cancelled" vs "failed".
  A failed attempt that will be retried is marked as discarded in `raw.ndjson`, so
  replay and editor resume helpers do not treat its streamed text as durable output.

## 6. Usage, cost, and the model registry

```go
// Usage lives in internal/llm/provider.go.
type Usage struct {
    InputTokens      int // uncached input, billed at full rate
    OutputTokens     int
    CacheReadTokens  int
    CacheWriteTokens int
    ReasoningTokens  int // Responses reasoning tokens; 0 for Anthropic (counted in output)
}
```

Normalization: OpenAI's `prompt_tokens` **includes** cached tokens
(`prompt_tokens_details.cached_tokens` is subtracted); Anthropic's `input_tokens`
already excludes them. After normalization `InputTokens` means the same thing on both.

`internal/llm/registry.go` holds a small registry. The structs carry JSON tags so they
double as the proxy catalog's on-disk schema (`Price`, `ModelInfo`, `ProviderConfig`,
`ModelEntry`), and `Cost`/`ContextWindow`/`Models` are methods on `*Registry`:

```go
type Price struct{ Input, Output, CacheRead, CacheWrite float64 } // USD per 1M tokens
type ModelInfo struct {
    ContextWindow int
    Price         Price
    Reasoning     *ReasoningInfo
}
func (r *Registry) Cost(model string, u Usage) (usd float64, known bool)
func (r *Registry) ContextWindow(model string) int // registry hit, else default 256_000
func (r *Registry) Models() []string               // sorted configured model ids
```

Model metadata originates from the public **models.dev** catalog
(`internal/modelsdev`); `harness-model-proxy --setup` and `--refresh-models` reduce it
to the provider/context/input-modality/reasoning fields harness needs. The proxy
caches the full catalog JSON as `models.dev.api.json` in the proxy config directory.
`--setup` prefers that cache over the vendored snapshot, but fetches and writes it
when it is missing or invalid. A running proxy refreshes the cache when it is older
than `models_dev_cache_ttl` (`24h` by default; `0` disables periodic refresh), and
`--refresh-models` fetches and caches the full catalog before rewriting configured
provider allowlists. The vendored snapshot is used only when there is no parseable
cache and a live fetch fails.

### Managed vs manual provider configs

Provider config files are either **managed** or **manual**:

- **Managed** configs are written by `--setup`/`--refresh-models` and carry
  `"managed": true`. They store **no per-model `price`**; instead the proxy
  resolves each managed model's price and input modalities from the in-memory
  models.dev cache at request time. Because the background refresher (above)
  reloads that cache and the serving handler swaps in the new metadata live,
  refreshed prices and modality support reach the running server **without** a
  `--setup` + restart. Re-running `--setup` never clobbers hand-edited prices
  because managed configs hold none.
- **Manual** configs are any provider file lacking `"managed": true` — typically
  hand-written. The proxy never touches them and serves their own `price` and
  `input_modalities` entries verbatim. A pre-existing price-bearing config
  without the flag is treated as manual and keeps its metadata (there is no
  migration). Running `--refresh-models` against such a provider rewrites it as
  a managed, price-less config.

A managed config may also carry `"price_source"` — a models.dev provider id to
resolve its prices from when that differs from the config's own `name`. This
exists for `openai-codex`, whose models are OpenAI models re-exposed under the
codex base URL and billed at OpenAI per-token rates: `--setup` writes
`"price_source": "openai"` so codex prices track the OpenAI rates in the cache.
It also writes `"omit_max_output_tokens": true` because the ChatGPT Codex
backend rejects the Responses `max_output_tokens` parameter; the proxy infers
the same omit behavior for older `codex_oauth` Responses configs.
The server otherwise stays provider-neutral for pricing — it just honors
whatever `price_source` a managed config names.

The serving handler holds its registry + served catalog behind an atomic
snapshot. The initial snapshot is built at startup from the loaded provider
configs plus the cached models.dev catalog; after each successful cache refresh
the refresher rebuilds the snapshot (managed prices/modalities from the new
catalog, manual metadata unchanged) and atomically swaps it in, so `/v1/models`
responses and per-request `cost_usd` accounting always reflect the freshest
managed metadata. `internal/llm` stays free of any `internal/modelsdev` import —
the server is the only layer that bridges models.dev metadata into `llm`.
Candidate cache updates must parse as models.dev JSON and contain at least one
provider and model. When a previous cache is parseable, replacement is rejected
if provider or model counts change by more than 4x and the absolute delta is
large enough to rule out normal small-catalog churn; the old cache remains in
place. Successful replacements first save the previous cache as
`models.dev.api.json.bak`, overwriting that single backup on each update.

Harness only uses models exposed by `harness-model-proxy`; arbitrary
provider-local model names are rejected unless they are configured in the proxy
catalog. Configured models without pricing metadata display token counts without
a dollar figure. Configured models without context-window metadata use a
conservative 256k default, configurable with `-default-context-window` and
overridable for a run with `-context-window`. Model prices, context windows,
output limits, and reasoning metadata are loaded from the model proxy catalog. When reasoning
controls are set, that metadata is used to validate provider/model reasoning
support, effort values, and budget ranges.
Responses API reasoning summaries default off, can be set to `auto`, `concise`,
`detailed`, or `none`, and are displayed only when explicitly enabled. Quiet mode
(`-q`/`--quiet`) suppresses reasoning summary output unless `-reasoning-summary`
is explicitly set on the CLI.

## 7. Configuration and provider selection

Precedence: **flags > environment > config file > built-in defaults** — for settings
that *have* a flag. A few knobs have no flag and resolve **env > config file > default**:
MCP/LSP enable, `mcp.proxy`, `mcp.local.enable`, and the tool-result caps. Others
(agent definitions, compaction knobs, `read_file_default_limit`, `agents_md_warn_bytes`,
`delegate_max_turns`) are config-file-only (listed below).

- Environment: `HARNESS_MODEL_PROXY_URL`, `HARNESS_PROVIDER`, `HARNESS_MODEL`, plus
  most `HARNESS_*` equivalents for user-facing flags. `--log-level` uses
  `LOG_LEVEL`. `HARNESS_TIMESTAMPS` accepts `short`, `full`/`long`, or `none` (with
  `off`/`false`/`disabled` as further aliases for `none`); `HARNESS_NO_TIMESTAMPS=true`
  is also an alias for `none`. Provider API keys and provider base URLs are resolved
  only by `harness-model-proxy`.
- REPL history (bash-style): `HARNESS_HISTFILE` (path; default
  `<stateDir>/harness/history`), `HARNESS_HISTFILESIZE` (on-disk entry cap;
  default 1000, 0 disables persistence), `HARNESS_HISTSIZE` (in-memory recall
  cap; default 1000, 0 disables recall). All three also have config-file keys
  (`histfile`, `histfilesize`, `histsize`) and flags (`-histfile`, `-histfilesize`,
  `-histsize`).
- Config file (optional): `~/.config/harness/config.json` — provider, model,
  model_proxy_url, agent definitions, hooks, flag defaults, and
  context-efficiency knobs.
  `agents_md_warn_bytes` (applied to each AGENTS.md file independently),
  `read_file_default_limit`, `compact_keep_turns`, `compact_summary_max_tokens`,
  and `compact_tool_result_max_bytes` are config-only.
  Tool-result truncation uses config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. `delegate_max_turns` (default `20`) is
  config-only for the delegate tool.
- Hooks use inline `hooks` plus config-relative `hook_configs` files. They are
  additive in order: inline first, then each listed file. `--hooks <file>`
  replaces the configured hook set for one launch.
- `harness-model-proxy --setup` creates a proxy config in the default proxy directory,
  appends a new provider config to an existing proxy config, or updates an existing
  configured provider. It reads cached models.dev provider metadata, fetching and
  caching the full catalog when needed, falls back to a vendored models.dev
  snapshot only when no parseable cache is available and live fetch fails, lists
  harness-supported providers, marks existing providers with bold text and `*`,
  derives missing first-party API URLs from exact `@ai-sdk/openai`,
  `@ai-sdk/anthropic`, and plain `@ai-sdk/google` package metadata, prompts for
  the API key when the provider needs one, pages the selected provider's
  models newest-first, and asks which models should be locally available. The
  synthetic `openai-codex` provider is listed when the catalog has OpenAI models;
  it writes the ChatGPT Codex backend URL and a `codex_oauth` auth block instead
  of API-key fields, plus `omit_max_output_tokens:true`. The proxy exposes this
  provider as Responses-compatible but not stateful because the Codex backend
  requires `store:false`. New providers start with no models enabled; existing
  providers start with their configured models enabled and all other catalog
  models disabled. Enabled rows are bold and marked with `*`; the
  selector accepts number/id toggles plus global `all`, global `none`, `save`,
  `/search`, `n`, `p`, and `cancel`. The provider config is
  generated from models.dev with only enabled models for that provider: base URL,
  api_type (`responses`, `openai`, or `anthropic`), key env vars, context windows,
  output limits, input modalities, and reasoning metadata. It is written as a **managed** config (`"managed": true`)
  with **no per-model prices** — the proxy resolves managed prices live from the
  models.dev cache (see *Managed vs manual provider configs*). Without `--force`,
  setup refuses to overwrite provider files that are not already referenced by the
  proxy config.
- `harness-model-proxy --refresh-models` fetches and caches the latest live
  models.dev catalog and refreshes each configured provider file's current model
  allowlist, preserving stored API keys and `auth` blocks. Refreshed files are
  rewritten as managed, price-less configs. If live fetch fails, it
  uses a parseable local cache before using the vendored fallback snapshot. It
  errors if a configured provider or model is missing/unsupported in the selected
  catalog.
- Provider config auth: `api_key` / `api_key_env` remain the default secret path.
  When a provider config supplies none of `api_key`/`api_key_env`/`auth`, the proxy
  falls back to a hardcoded env var keyed on the provider's `api_type`:
  `ANTHROPIC_API_KEY` (anthropic), `RESPONSES_API_KEY` then `OPENAI_API_KEY`
  (responses), and `OPENAI_API_KEY` (otherwise). An optional `auth` block takes
  precedence and resolves dynamic request headers:
  `token_command` executes an argv command, parses plain-token or JSON
  `access_token` output, caches it in memory until expiry/TTL, and sends
  `Authorization: Bearer ...` by default; `oauth2` reads a stored access token,
  refreshes it when needed (the client secret comes from `client_secret` or the env
  var named by `client_secret_env`), and is managed with
  `harness-model-proxy auth login|logout|status <provider>`. Browser PKCE and
  device-code flows are supported. `codex_oauth` is the OpenAI Codex ChatGPT
  subscription auth path: login uses OpenAI's device-code endpoints, refresh uses
  the Codex JSON refresh exchange, terminal 4xx refresh failures are marked in the
  token file so the proxy stops replaying the same dead refresh token, and request
  headers include `Authorization`, `ChatGPT-Account-ID`, and `X-OpenAI-Fedramp`
  when required. Token files are written under the proxy config dir via temp-file
  then rename.
- The model proxy logs one structured record per `/v1/stream` request with
  requester, provider, model, request/response bytes, duration, token usage, stop
  reason, tool-call count, and `cost_usd` when the model has a known price
  (from the config for manual providers, or the models.dev cache for managed ones).
  Proxy config accepts `log_level` (`debug|info|warn|error`) and `log_format`
  (`json` default, or `text`), with serve flags overriding config. Proxy config
  also accepts `models_dev_cache_ttl` as a duration string such as `"24h"` or
  numeric `0`; the `-models-dev-cache-ttl` serve/setup/refresh flag overrides it.
- **Usage aggregation.** The proxy keeps a mutex-guarded `{provider, model}` usage
  map and serves it read-only at `GET /v1/usage` as `{"models": [ {provider, model,
  requests, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  reasoning_tokens, cost_usd}, … ]}`, sorted by `provider:model`. Because every priced
  `/v1/stream` request is recorded, delegate child-agent spend that flows through the
  proxy is included.
- **Pricing staleness.** The `GET /v1/models` catalog response carries an optional
  `pricing` object — `{source_date, max_age_seconds}` — and `max_age_seconds` is the
  configured models.dev refresh interval. `source_date` dates the served prices:
  when any provider is managed and a models.dev cache is loaded, it is the cache's
  source date (the cache file's mtime, kept fresh by the background refresher);
  for a manual-only catalog it is the newest modification time among the
  configured provider config files (the date those prices were last written). A
  client can compare them to detect stale prices.
- **Selection rule:** `harness` fetches `GET /v1/models` from the proxy. A
  `provider:model` value always strips the `provider:` prefix from the model, but only
  sets the provider when one was not already chosen (an explicit `-provider` /
  `HARNESS_PROVIDER` wins). Otherwise an explicit `-provider` selects a proxy provider,
  and model selection must come from `harness` flags, environment, config, or `/model`.
- `harness --check-model-proxy` reuses the catalog request as a bounded
  reachability check and exits before session creation, tool setup, hooks, model
  selection prompts, or `/v1/stream`.
- `internal/config` resolves only user-facing settings. Provider connection settings
  are resolved by `harness-model-proxy` from its config and environment.
- The optional config-file `mcp` and `lsp` blocks (proxy/local MCP and LSP servers)
  are documented with their subsystems in §15 and §15a.

## 8. Agent loop (`internal/agent`)

### 8.1 Turn loop

One user prompt runs model turns until the model stops asking for tools:

```
append user message
for modelTurn := 0; maxTurns <= 0 || modelTurn < maxTurns; modelTurn++ { // -max-turns, default 250; <=0 unlimited
    stream := provider.Stream(ctx, request)
    accumulate: print text deltas live; collect assembled tool calls;
                capture usage + stop reason
    append assistant message (text blocks + tool_use blocks, emission order)
    if stopReason != tool_use { break }
    for each tool call, in order:                  // consecutive read-only islands may run concurrently
        result := registry.Dispatch(ctx, call)     // always returns a result
        print one-line tool summary
    append ONE user message carrying all tool_result blocks, in call order
}
emit turn-usage event   // the REPL / one-shot caller prints it and saves the session (§11)
```

- **Mostly-sequential tool execution.** Coding tools mutate a shared filesystem; deterministic
  ordering matching the model's emission order is worth far more than parallelism. Consecutive
  read-only islands with 2+ calls dispatch concurrently, bounded at 8, unless
  `PreToolUse` or `PostToolUse` hooks are configured; mutating calls remain ordering
  barriers. Results, sink events, and transcript blocks stay in emission order.
- **One result per call, always.** Required by both APIs (§4 invariant). `Dispatch`
  produces a result even on panic.
- **Metered tools:** tools may optionally report token usage (currently synchronous
  `delegate`). The agent adds that usage to the turn/session total, while the normal
  tool result remains the only child output added to the parent transcript.
- **Optional file diffs:** when `show_diffs` is enabled, the agent asks built-in
  file mutation tools for their affected paths, snapshots those files immediately
  before and after each sequential tool call, and emits a user-facing unified diff
  event after the normal tool summary. The diff is generated by a stdlib-only line
  renderer, not by repository `git diff`, so it works in non-git projects and shows
  incremental per-call changes when the same file is edited repeatedly.
- **Background jobs:** tools with `background:true` start process-local jobs and
  return a job id immediately. `delegate` uses the same flag for background child
  agents. Completed job summaries are delivered once as request-only context on a
  later parent model request; they are not appended to the parent transcript.
- **Max-turns guard:** when `max_turns` is positive, on hit print
  `[stopped: reached max turns (250)]`, keep the transcript (it is valid — the
  last model turn's results are appended), and return to the prompt. A
  non-positive `max_turns` disables this guard. One model turn before the limit
  (`modelTurns == maxTurns-1`) the loop injects a one-shot RoleUser wrap-up steer
  ("stop calling tools now and reply with a final message").
- **Runaway guards (`internal/agent/loopguard.go`).** A per-run `turnGuard` (loop
  frame only, never on the shared registry) watches each tool turn:
  - *Repeated identical calls.* Each turn's call-set is reduced to an
    order-insensitive signature of `name + canonical(JSON input) + result`. After
    3 identical signatures in a row it injects one RoleUser steering message; at 8
    it hard-stops with `[stopped: N identical tool turns repeated with no change]`.
  - *Error storm.* It counts consecutive turns in which **every** tool result is an
    error. At 5 it steers ("re-read the latest error output and change your
    approach"); at 10 it breaks with `[stopped: N consecutive tool turns all
    failed]`. (Repetition and error-storm steers share one slot, so a turn is
    nudged at most once.)
  - *Turn-token budget.* When `-max-turn-tokens` is positive, before each next
    (paid) model request it compares the turn's cumulative usage
    (input + cache-read + cache-write + output + reasoning) against the budget and
    breaks with `[stopped: turn token budget N exceeded]`. `0` is unlimited. This
    path deliberately skips the final summary — the point is to stop spending.
- **Graceful wrap-up on hard stop.** The error-storm, repeat-loop, and
  max-turns-reached breaks (but not the token-budget break) end with one final
  request that has `Tools` cleared, so the model produces a text-only wind-down
  appended as an assistant `final_answer` message instead of leaving a dangling
  `tool_result`. It is best-effort: a failed or empty summary leaves the
  already-valid transcript untouched, and any tool calls the model emits there are
  ignored.
- **Non-normal model stops:** `max_tokens` and stop-sequence finishes end the turn
  but emit a visible notice, so a truncated or externally stopped assistant answer
  does not look like an ordinary completion.
- **Mid-stream retries:** each model turn is wrapped in `streamWithRetry`, which
  re-requests the step from scratch on a retryable terminal stream error up to
  `streamRetries` (2) times. These attempts do **not** count against `max-turns`;
  failed-attempt usage is still billed and tracked (xref §5.5).
- **Stateful-Responses fallback:** when a turn that reused a `previous_response_id`
  fails because that response is no longer available (and nothing streamed), the agent
  resets the stored Responses state, notes it, and retries once with the full context.
- **Stop hook:** a configured `Stop` hook fires when the model would end the turn; it
  may block the break and force one more model turn. `stop_hook_active` guards it so it
  fires at most once per turn (`agent.go`).

### 8.2 Tool failure handling

`Dispatch` never lets the loop crash. Each failure mode becomes an `is_error` result
string fed back to the model so it can self-correct:

| Failure | Result text |
|---|---|
| unknown tool name | `error: unknown tool "<name>"` |
| invalid JSON args | `error: invalid arguments: <detail>` |
| tool returned error | `error: <message>` |
| tool panicked | `error: tool panicked: <recovered>` (also logged to stderr) |
| tool exceeded the dispatch timeout | `error: tool timed out after <dur>` |

**Per-tool dispatch timeout backstop (`-tool-timeout`, default 600s, `<=0`
disables).** `Dispatch` runs each tool under a derived `context.WithTimeout` so a
hung tool that ignores cancellation cannot stall a turn; on expiry it returns the
`error: tool timed out after <dur>` result above. It applies to both the
sequential path and the concurrent read-only batch. A tool that reports its own
deadline via `SelfTimeouter` only **raises** the ceiling, never lowers it, so
`run_command`'s `timeout_seconds` stays authoritative. An outer cancellation
(`^C`) is reported as cancellation, not a dispatch timeout.

### 8.3 Output truncation

A central cap in `Dispatch` (backstop for every tool): **64 KB or 1000 lines per
result** by default, configurable with `tool_result_max_bytes` and
`tool_result_max_lines`, or env `HARNESS_TOOL_RESULT_MAX_BYTES` and
`HARNESS_TOOL_RESULT_MAX_LINES`. The first cap hit adds a teaching marker:

```
[truncated: showing first 1000 of 4213 lines; use read_file offset/limit or grep to narrow]
```

Individual tools may also apply their own natural limits, but the central cap is the
backstop for every result. Truncated results carry metadata so the UI can warn and write
the full output to the session's `artifacts/tool-results/` directory. When an artifact
is written, the model-visible tool result includes the absolute artifact path and
advises using `read_file` with `offset`/`limit` or `rg` for targeted inspection.

### 8.4 Interrupts

A single SIGINT handler plus a per-turn `context.CancelFunc`:

- **^C during a turn** → cancel the turn context (aborts the HTTP stream; kills
  `run_command` process groups). Apply the cancel repair rule (§4): keep streamed
  partial text, strip un-executed tool calls. Print `[cancelled]`, return to prompt.
- **Esc-Esc during a REPL turn** → same turn cancellation as the first ^C, without
  the second-^C exit behavior.
- **Second ^C within ~1 s, or ^C at the idle prompt** → save session, print the
  session token summary, exit 130.
- **^C during startup or helper-command network work** → cancel the in-flight
  request and exit 130.
- **^D at the prompt** → save session, print the session token summary, exit 0.

### 8.5 System prompt (`internal/sysprompt`)

`system = staticPrompt + "\n\n" + envContext + runtimeSections`

- **Builtin instructions** (`prompts/system.txt`): concise agentic-coding guidance — read before
  editing, prefer `edit` with unique context, use tools rather than guessing file
  contents, use available search tools or `list_dir`, run builds/tests via
  `run_command`, stop when done.
- **Environment context**, computed at startup:

  ```
  Environment:
  cwd: /Users/twt/project
  os: darwin/arm64
  date: 2026-06-09
  git: branch=main, 2 modified, 1 untracked
  ```

  Git summary via `git branch --show-current` + parsed `git status --porcelain`;
  `git: (not a git repository)` otherwise.
- Flag/config override: `-system-prompt <text|@file>`,
  `HARNESS_SYSTEM_PROMPT`, or config `system_prompt` replaces only the static
  built-in instructions. Runtime sections such as env context, user/project
  `AGENTS.md`, skills, and agent prompts are still composed around it.
  `~/.agents/AGENTS.md` is appended before the current working directory's
  `AGENTS.md`; missing files are ignored and other read failures fail startup.
  `@~/path` expands through the current user's home directory; relative `@file`
  references in the config file resolve from that config file's directory.
  `-no-env` drops the env block.

## 9. Tool set (`internal/tools`)

```go
type Tool interface {
    Name() string
    Description() string     // model-facing, one line
    Schema() json.RawMessage // JSON Schema for the input object
    ReadOnly(input json.RawMessage) bool
    Run(ctx context.Context, input json.RawMessage) (string, error)
}

type MeteredTool interface {
    RunMetered(ctx context.Context, input json.RawMessage) (MeteredResult, error)
}

type MeteredResult struct {
    Text  string
    Usage llm.Usage
}

type Registry struct{ /* ordered map */ }
func (r *Registry) Register(t Tool)
func (r *Registry) Specs() []llm.ToolSchema
func (r *Registry) Dispatch(ctx context.Context, call llm.ToolCall) llm.ToolResult
```

- **Schemas are hand-written JSON Schema constants.** The raw schema is the
  implementation contract, but model-facing specs strip schema `description` fields
  to reduce repeated prompt text. Enums and required-ness still deserve hand tuning,
  and reflection fights you on exactly those fields.
- **Tools self-validate args** after `json.Unmarshal` into a private struct (no stdlib
  JSON Schema validator; unknown extra keys are tolerated — models hallucinate them).
- **Metered tools** optionally implement `RunMetered`; `Dispatch` prefers it and
  preserves the reported `llm.Usage` on `ToolResult`.
- **Read-only classification is per call.** Static read-only tools ignore the input;
  argv-style tools can parse their arguments and return true only for safe subcommands
  (for example `git status`, `git diff`, `git log`).
- Relative paths resolve against the process cwd. No path restrictions — the harness is
  honest about its no-sandbox assumption.

### 9.1 `read_file`

> Read a file from disk. Provide a JSON object with path (single file; supports offset/limit), or paths[] to read several files at once, each under a "==> path <==" header. Returns line-numbered content.

| param | type | notes |
|---|---|---|
| `path` | string | single file; required unless `paths` is given |
| `paths` | array of strings | multi-file mode; each file rendered under a `==> path <==` header with its own per-file line budget; `offset` is ignored |
| `offset` | int | 1-based starting line (single-file mode only) |
| `limit` | int | max lines, default 1000 or `read_file_default_limit` |

- Output is line-numbered (`cat -n` style: right-aligned number, tab, line). Line
  numbers make `edit` targeting and grep cross-referencing far more reliable.
- **Truncation notice:** when a single-file read is cut off at its line window the
  result ends with `[file truncated at line N; continue with offset=N+1]`, so the
  model knows to page rather than assuming it saw the whole file.
- **Multi-file mode (`paths[]`):** each file is read from line 1 under its
  `==> path <==` header. With no explicit `limit` the default window is split across
  the files (`max(defaultLimit/len(paths), 50)` lines each); an explicit `limit`
  applies per file. A per-file read error is reported inline and the batch continues.
- Binary sniff: first 8 KB containing NUL → `error: <path> appears to be binary`.
- Files are streamed line-by-line and stop after the requested/default line
  window, so memory is bounded by the window and longest line regardless of file
  size.
- Directory → `error: <path> is a directory; use list_dir`. Offset past EOF → error
  stating the file's line count. Empty file → `(empty file)`.

### 9.2 `list_dir`

> List directory entries with type and size. Non-recursive; pass a glob to filter.

| param | type | notes |
|---|---|---|
| `path` | string | default `"."` |
| `glob` | string | `path.Match` filter on base names |

- Non-recursive by design — recursion belongs to `glob` (§9.2a, by name) and
  `grep`/`rg`/host commands (by content), with `run_command` (`find`) as the escape
  hatch. No separate `find` tool: fewer tools means better model reliability.
- One entry per line: type char, human-readable size, name (`/` suffix for dirs);
  dirs-first, then alphabetical. 1000-entry cap with truncation marker.
- Unreadable entries shown with `?` size; listing continues.

### 9.2a `glob`

> Recursively find files and directories by glob. Provide a JSON object with pattern (e.g. {"pattern":"**/*config*.go"}) and optional root. Read-only; ** matches across directories. Returns matching paths with type and size, one per line, sorted by path.

| param | type | notes |
|---|---|---|
| `pattern` | string, required | glob relative to `root` |
| `root` | string | directory to search from, default `"."` |

- Read-only recursive name search, complementing `list_dir` (one level) and
  `grep`/`rg` (by content). `**` matches any number of path segments (including
  zero); `*`/`?`/`[…]` match within a single segment via `filepath.Match`. Consecutive
  `**` collapse, and a trailing `**` matches the remainder.
- One entry per line — type char, human size, root-relative path (`/` suffix for
  dirs) — sorted ascending by path. Empty result → `(no matches)`.
- Two caps: the walk stops collecting after `globScanCap` (10000) matches, and the
  sorted output is truncated to the first `listDirCap` (1000) with a
  `[truncated: showing first 1000 of <N> matches; narrow the pattern or root]`
  marker (the total gains a `+` when the scan cap was hit).
- In the default tool set (auto/independent), not in the read-only `plan` agent's
  explicit list.

### 9.3 `grep` and optional `rg`

> `grep`: Run the host grep command directly. Provide a JSON object with args as an array of strings, e.g. {"args":["-R","-n","TODO","."]}; do not pass args as a string or JSON-encoded array. No shell; binary files are skipped (-I) unless you set a binary policy or pass --; overlong matched lines are clamped in output. Returns combined stdout+stderr and the exit code, or returns a background job id immediately when background is true. (Under `-search-tools both` grep's description gains a "prefer rg" steer.)

> `rg`: Run the host rg (ripgrep) command directly. Provide a JSON object with args as an array of strings, e.g. {"args":["-n","TODO","."]}; do not pass args as a string or JSON-encoded array. No shell; normal searches default to --max-columns=1024 --max-columns-preview --max-filesize=10M unless args set those native rg options. Returns combined stdout+stderr and the exit code, or returns a background job id immediately when background is true.

| param | type | notes |
|---|---|---|
| `args` | array of strings, required | arguments passed after the program name; must be a JSON array, not a string or JSON-encoded array |
| `stdin` | string | written to the program's standard input |
| `cwd` | string | default process cwd |
| `timeout_seconds` | int | default 120, no maximum |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |

- Search exposure is configurable with `search_tools` / `HARNESS_SEARCH_TOOLS` /
  `-search-tools`: `auto` (default), `grep`, `rg`, or `both`.
- In `auto`, harness registers `rg` when `exec.LookPath("rg")` succeeds and
  otherwise registers `grep`; it does not warn for the automatic fallback.
- `grep` always invokes `grep` from the harness process PATH. Explicit `rg` or
  `both` registers `rg` only when `exec.LookPath("rg")` succeeds; otherwise that
  tool name is hidden and a disabled-tool diagnostic is emitted. If explicit `rg`
  is requested but unavailable, harness still registers `grep` so the agent keeps
  one search tool.
- Missing explicitly requested optional CLI-backed tools are reported once at startup
  through the plaintext slog handler, e.g.
  `[warn] [cli_tools] Tool "rg" is disabled. Reason: "rg" binary not found.`
  `--log-level`/`LOG_LEVEL` filters these diagnostics by level.
- The advertised shape is `{"args":[...]}`. `args` must be a JSON array of
  strings, not a string or JSON-encoded array. The decoder also accepts a bare
  string array because earlier wording told models to provide that shape.
- Both tools use `exec.Command(program, args...)`: no shell, glob expansion, pipes,
  redirection, `$VAR`, or `~` expansion. Each argument arrives byte-for-byte.
- Search semantics are the host tool's semantics. Regex syntax, recursion,
  gitignore/default ignore behavior, binary handling, hidden files, and output shape are
  selected with native CLI flags (`grep -R -n`, `grep -F`, `rg -n`, `rg --hidden`,
  `rg --no-ignore`, etc.), not reimplemented by the harness.
- Normal `rg` searches are guarded with `--max-columns=1024 --max-columns-preview`
  and `--max-filesize=10M` to avoid huge single-line matches and accidental searches
  through very large text files. Explicit native `rg` args win: pass `-M`,
  `--max-columns`, or `--max-filesize` to override those defaults. Raw/introspection
  modes such as `--json`, `--files`, `--type-list`, `--help`, and `--version` are
  passed through unchanged.
- Host `grep` has no portable `--max-columns`, so it is guarded in-process. `-I`
  (skip binary files) is prepended before any `--` operand separator unless the call
  already sets a binary policy (`-I`/`-a`/`--text`/`--binary-files`) or is a
  help/version invocation. Matched output lines longer than `grepMaxLineLen` (1024
  bytes) are cut on a rune boundary and suffixed with `… [N chars clamped]`; the
  `[exit code: N]` trailer and short lines pass through unchanged.
- Under `-search-tools both`, both `grep` and `rg` are registered and `grep`'s
  `Description()` gains a suffix steering the model to prefer `rg` as the faster
  default.
- Same process conventions as `run_command` (§9.7): own process group, timeout or ^C
  kills the group, combined stdout+stderr, `[exit code: N]` trailer, and non-zero exit
  is NOT an error result. For search this matters because no matches is commonly exit
  code 1.

### 9.4 `edit`

> Edit one or more files with exact-text replacements. Each file has edits[];
> oldText must be unique and non-overlapping in the original file.

| param | type | notes |
|---|---|---|
| `files` | array, required | one entry per file; each target file must already exist |
| `files[].path` | string, required | must exist (use write_file to create) |
| `files[].edits` | array, required | one or more replacements for that file |
| `files[].edits[].oldText` | string, required | exact text to replace; must be unique in the original file unless `replaceAll` is set |
| `files[].edits[].newText` | string, required | replacement text; empty string deletes oldText |
| `files[].edits[].replaceAll` | bool | optional; replace every occurrence of `oldText` instead of requiring a unique match (default false) |

- All edits for a file match against that file's original content, not against
  content after earlier edits in the same call.
- With `replaceAll`, every non-overlapping occurrence of `oldText` is replaced and
  each counts toward the reported replacement count; the uniqueness check is skipped
  but zero matches is still a not-found error. The overlap guard is relaxed only
  between spans of the **same** `replaceAll` block — a `replaceAll` span overlapping
  a different edit still raises the overlap error.
- Duplicate file entries are rejected; combine a file's replacements in one
  `files[]` entry.
- 0 occurrences → error naming the missing `oldText`.
- N>1 occurrences → error asking for more context to make `oldText` unique.
- Overlapping replacements in one file → error asking the model to merge or
  retarget the edits.
- Replacements that produce content identical to the original file → error
  (`replacements produced identical content`); a no-op edit is rejected rather than
  rewriting the file unchanged.
- The tool preserves an existing UTF-8 BOM and the file's first observed line
  ending style. If exact matching fails, it retries after normalizing trailing
  whitespace, smart quotes, Unicode dashes, and special spaces.
- Success reports `edited <file-count> file(s), <replacement-count> replacement(s)`
  followed by one line per file.

### 9.5 `write_file`

> Create or overwrite a file with the given content. Creates parent directories.

| param | type | notes |
|---|---|---|
| `path` | string, required | |
| `content` | string, required | empty allowed |

- `os.MkdirAll` parents (0755), write 0644, overwrite without ceremony (no permission
  system by design). Reports `created`/`overwrote`, bytes, lines.
- Existing directory at path, or trailing `/` → error.

### 9.6 `apply_patch`

> Apply a Codex-format patch. Supports add, delete, update, and move.

| param | type | notes |
|---|---|---|
| `patch` | string | full `*** Begin Patch` / `*** End Patch` text; preferred field name |
| `patchText` | string | compatibility alias for `patch` |
| `patch_text` | string | compatibility alias for `patch` |

- **Catalog-only, not in the default tool set.** `edit` and `write_file` subsume
  `apply_patch`, so `registerFileTools` omits it; it is registered only by
  `CatalogWithOptions`. It is therefore absent from the `auto`/`independent` default
  lists (derived from `DefaultNamesWithOptions`) and an agent opts back in by naming
  `apply_patch` in its `allowed_tools` whitelist, which resolves against the full
  catalog.
- Parser accepts Codex patch operations only: `*** Add File: <path>`,
  `*** Delete File: <path>`, `*** Update File: <path>`, and optional
  `*** Move to: <path>` immediately after an update header. Classic `---` / `+++`
  unified diffs are not accepted by this tool.
- Tool input also accepts a bare JSON string containing the patch text, for
  compatibility with callers that model `apply_patch` as a freeform argument.
  At least one non-empty patch value is required. If multiple non-empty patch
  fields are provided, they must contain identical text.
- Parse failures are reported as invalid arguments with a format hint: provide
  one raw patch envelope, avoid markdown fences, and prefix blank context lines
  in update hunks with a space.
- Update hunks use Codex's headerless body lines: `@@` chunk markers are optional,
  context lines start with a space, deletions with `-`, and additions with `+`.
- Matching tries exact lines first, then whitespace-normalized comparison, scanning
  forward from the current file cursor. Pure insertion update hunks insert at EOF.
- Patches apply in file order and stop at the first rejected file. Files applied
  before the rejection remain changed; the rejected file is left untouched.
- Success reports `Success. Updated the following files:` followed by `A`, `M`, or
  `D` status lines.

### 9.7 `run_command`

> Run a shell command with `command` or a program directly with `argv`. Provide exactly one of command or argv. When using argv, pass it as an array of strings, not a shell string or JSON-encoded array. Returns combined stdout+stderr and exit code, or a background job id when background is true.

| param | type | notes |
|---|---|---|
| `command` | string | shell command line; mutually exclusive with `argv` |
| `argv` | array of strings | program + literal arguments; mutually exclusive with `command`; must not be a shell string or JSON-encoded array |
| `stdin` | string | written to the command's standard input |
| `cwd` | string | default process cwd |
| `timeout_seconds` | int | default 120, no maximum |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |

- Exactly one of `command` or `argv` is required.
- `command` is executed via a **non-login** `bash -c` (fallback `sh -c` if bash is
  absent). Sourcing the full login-profile chain on every call added ~50-300ms
  (nvm/rbenv/conda) and risked banner noise in results, so it was dropped. The PATH
  enrichment a login shell would have added is recovered once per process: a single
  `bash -lc` probe at first use resolves the login PATH, and those extra directories
  are appended (current PATH keeps precedence) into the command environment.
- When using `argv`, pass a JSON array of strings such as `["go","test","./..."]`,
  not a shell command string or JSON-encoded array.
- `argv` is executed with `exec.Command(argv[0], argv[1:]...)`: no shell, glob
  expansion, redirection, `$VAR`, or `~` expansion. Each argument arrives
  byte-for-byte.
- **Combined stdout+stderr** — the model reads a terminal transcript the way a human
  does; interleaving beats separation.
- `[exit code: N]` always appended. **Non-zero exit is NOT an error result** — a failing
  build is exactly the signal the model needs; only infrastructure failures (shell
  couldn't start) set `is_error`.
- Runs in its own process group/session with no controlling TTY under the turn
  context; timeout or ^C kills the group (children included) and reports output
  captured so far.
- If the timeout/^C path cannot finish reaping promptly, the tool still returns a
  snapshot of captured output and the status line notes that the wait did not finish.
- Foreground calls finish when the direct shell/program exits, not when every
  descendant closes inherited stdout/stderr; any remaining same-group descendants
  are killed after that direct exit. Long-lived commands should use
  `background:true`.
- With `background:true`, the command uses the same process-group, timeout, and
  output formatting rules, but runs under the background job manager instead of
  blocking the current tool call. Use `background_jobs` or `/background` to inspect
  or cancel it; completed output is delivered once as request-only context.
- Environment inherited unmodified.
- `stdin`, when provided, is written verbatim to the command's standard input; absent
  means `/dev/null` (programs see immediate EOF, never hang on input). Prefer it over
  `echo`/heredocs when feeding content to a command (`git commit -F -`, `python -`,
  `tee file`) — content travels with zero shell escaping.

### 9.8 Shared process execution (`runProcess`)

`run_command` (§9.7), `grep`/`rg` (§9.3), and `git`/`git_readonly` (§9.9, §9.11) all
run their subprocess through one shared `runProcess` helper, so they share identical
process semantics. The §9.7 schema/description above describe `run_command`'s surface;
this subsection records the common runner those argv tools point at.

- **Own process group/session, no controlling TTY.** The child leads its own group, so
  a timeout or `^C` can signal the whole group (negative-pid `SIGKILL`) and reap
  descendants, not just the direct child.
- **Timeout.** `timeout_seconds` defaults to **120** (`0` means the default; there is no
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

> Run a git command. Provide a JSON object with args as an array of strings, e.g. {"args":["status","--porcelain"]}; do not pass args as a string or JSON-encoded array. No shell; no pager.

| param | type | notes |
|---|---|---|
| `args` | array of strings, required | argv after `git`; must not be a string or JSON-encoded array |
| `cwd` | string | default process cwd |

- `git` is registered only when `exec.LookPath("git")` succeeds at registry
  construction time. If git is not installed, the model never sees the `git` tool name.
- The advertised shape is `{"args":[...]}`. `args` must be a JSON array of
  strings, not a string or JSON-encoded array. The decoder also accepts a bare
  string array because earlier wording told models to provide that shape.
- `exec.CommandContext(ctx, <resolved-git-path>, append([]string{"--no-pager"}, args...)...)`
  passed through the shared process runner — no shell, so no quoting ambiguity.
  `GIT_TERMINAL_PROMPT=0` prevents auth hangs.
- **One argv tool, not narrow per-subcommand tools:** a single stable schema covers the
  entire git surface (status, diff, log, blame, stash, rebase, commit) that the model
  already knows from training; enumerating subcommands multiplies schemas and still
  misses the long tail.
- Combined output + exit code, same conventions as `run_command`: no controlling
  TTY, group kill on timeout/^C, default 120 s timeout, and non-zero exit is not
  a tool error. Interactive flows (`rebase -i`) fail fast rather than hang.

### 9.10 `web_fetch`

> Fetch a URL (http/https) and return its text content. HTML is reduced to readable text. Returns a background job id immediately when background is true.

| param | type | notes |
|---|---|---|
| `url` | string, required | http/https only |
| `max_bytes` | int | default 1 MB, cap 5 MB |
| `timeout_seconds` | int | default 30, no maximum |
| `timeout` | int | alias for `timeout_seconds` |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |

- Default 30 s timeout, configurable without a maximum; up to 5 redirects, each
  hop re-validated as http/https.
- `text/html` → hand-rolled reduction that preserves links and block structure:
  drop `<script>`/`<style>` blocks; render `<a href>` as `text (url)`; emit a newline
  at block boundaries (`<br>` and the closing tags of `p`/`div`/`li`/`tr`/`h1`–`h6`);
  strip remaining tags; `html.UnescapeString` (stdlib); collapse whitespace per line
  while keeping the inserted line breaks. Explicitly "readable-ish text", not a
  renderer — good enough for docs and articles. Other `text/*`,
  `application/json`, `application/xml`, and any `+json`/`+xml` suffix type → raw; an
  absent `Content-Type` is treated as text. Binary content types → error.
- Output prefixed `# <final-url> (<status>, <content-type>)`. Non-2xx responses return
  status + body as content (not `is_error` — the model may want the error page).

### 9.11 `git_readonly`

> Run a restricted git command: status, log, diff, show, grep, blame, or bisect (bisect checks out commits; run/view/visualize are rejected). Provide a JSON object with args as an array of strings starting with the subcommand, e.g. {"args":["log","--oneline"]}; do not pass args as a string or JSON-encoded array. No shell; no pager.

| param | type | notes |
|---|---|---|
| `args` | array of strings, required | argv after `git`, starting with the subcommand; must not be a string or JSON-encoded array |
| `cwd` | string | default process cwd |

- A restricted sibling of `git` (§9.9) used by restricted agents (§14). It is
  registered only when git is installed and reuses the same `--no-pager` /
  `GIT_TERMINAL_PROMPT=0` plumbing. It is scheduled as read-only, but allowed
  `bisect` operations can move `HEAD`.
- The advertised shape is `{"args":[...]}`. `args` must be a JSON array of
  strings, not a string or JSON-encoded array. The decoder also accepts a bare
  string array because earlier wording told models to provide that shape.
- **Allowlist by bare subcommand:** `args[0]` must be one of `status`, `log`, `diff`,
  `show`, `grep`, `blame`, `bisect` and must not start with `-`. Because global git
  options (`-c`, `-C`, `--git-dir`, `--exec-path`, `--paginate`) precede the
  subcommand, requiring a non-flag first argument blocks every global-option
  injection. Subcommand-local flags after `args[0]` pass through.
- A few local flags still break the restricted boundary and are rejected:
  `--output`/`--output-directory` (write a file) and `-O`/`--open-files-in-pager`
  (launch a pager/editor). `bisect run <cmd>` is rejected because it executes
  commands, and `bisect view` / `bisect visualize` are rejected because they
  launch a viewer; other `bisect` operations are allowed even though they move
  `HEAD`.

### 9.12 `write_tmp_file`

> Write a scratch file under this run's private temp directory and return its absolute path. Files are kept after exit.

| param | type | notes |
|---|---|---|
| `name` | string, required | relative file name (subdirectories allowed) |
| `content` | string, required | full file content (empty allowed) |

- Gives read-only agents (§14, `plan`) a place to draft notes without project
  write access. Files are written under one `os.MkdirTemp` directory created lazily on
  first use and shared across calls; they are kept after exit.
- `name` must be relative and stay inside the temp directory: absolute paths and any
  `..` escape (after `filepath.Clean`) are rejected. Returns the absolute path written.

### 9.13 `update_todos`

> Maintain the current plan for nontrivial work. Replace the full todo list; keep at most one item in_progress.

| param | type | notes |
|---|---|---|
| `todos` | array, required | the complete list; replaces the previous one entirely |
| `todos[].content` | string, required | what needs to be done; keep each item concise and action-oriented |
| `todos[].status` | string, required | `pending`, `in_progress`, or `completed` |
| `todos[].active_form` | string | optional present-tense label shown while in progress |

- **Whole-list replace semantics** (like Claude Code's TodoWrite): each call carries
  the complete list, so there is no per-item merge and no IDs. The transcript already
  records the latest list; the in-memory store is a convenience for rendering and resume.
- Validates non-empty `content`, a known `status`, and at most one `in_progress` item;
  returns a rendered checklist as the tool result: a `Todos (<done>/<total> done):`
  header followed by one `[x]`/`[~]`/`[ ]` line per item (an `in_progress` item shows
  its `active_form` label when set). An empty list renders `Todo list cleared.`
- Implemented in `internal/todo`, not `internal/tools`, so `internal/session` can persist
  `todo.Item` without importing the tools package. A single `todo.Store` is constructed
  per process (like `write_tmp_file`); the list is saved in `state.json` (`Session.Todos`),
  reseeded on resume, and cleared by `/clear`.
- When `update_todos` is available, the REPL/one-shot drivers add a short request-only
  reminder showing the current list (or noting that none exists). This context is not
  saved into the transcript.
- In the interactive REPL, the visible session's non-empty todo list is also printed
  before the idle prompt when the current visible agent has `update_todos`, and
  the visible todo status is printed after each successful `update_todos` call.
  One-shot runs and child-agent private todo stores do not print there.

### 9.14 `delegate`

> Run a configured delegate agent on a self-contained task and return its final report.

| param | type | notes |
|---|---|---|
| `task` | string, required | complete task for the child agent |
| `agent` | string | optional configured agent name; omitted uses the current active agent; schema enum contains only agents delegatable from the current parent tools |
| `max_turns` | int | optional per-call model-turn cap; capped at `delegate_max_turns` |

- Implemented in `internal/delegate`, not `internal/tools`, to avoid an import cycle:
  the delegate tool starts a child `agent.Agent`, while `internal/agent` already
  depends on `internal/tools` for dispatch.
- Child agents start with an empty transcript and use the requested agent
  definition's prompt, subset-validated tools, and optional provider/model. If no
  `agent` is provided, the child uses exactly the current parent agent's active
  tools.
- A named child agent may only run when its configured tools are a subset of the
  current parent agent's active tools. Non-subset calls return a tool error before
  any child model request is made.
- If the child receives `delegate`, that delegate tool is rebound to the child's
  runtime so recursive delegation is checked against the immediate parent's tools.
- Child agents receive a private `update_todos` store when that tool is available;
  child todo updates do not affect the parent session's todo list.
- The parent transcript records only the normal `delegate` tool call and compact result.
  Child transcripts are saved under `children/<child-id>/` in the parent session
  directory for forensics. Child token usage is reported through `MeteredTool` and
  folded into the parent turn/session usage totals.

### 9.15 background jobs

Tools that opt into the reusable background job contract hand the manager a job
kind, description, and cancellable runner. The manager owns ids, status, list/get/cancel,
one-shot notices, and request-only context delivery. `run_command`, `grep`, `rg`,
`web_fetch`, and `delegate` support this path via `background:true`; background
delegate jobs still use the same launch validation, child transcript, private todo,
and token-accounting behavior as synchronous delegate.

`background_jobs` accepts:

| param | type | notes |
|---|---|---|
| `action` | string | `list`, `get`, or `cancel`; omitted means `list` |
| `id` | string | required for `get` and `cancel` |

- Jobs live only in the current harness process. Running jobs are abandoned on process
  exit and cleared on `/clear`.
- Completed job summaries are delivered once as request-only context to the parent
  agent, including the transcript path when one exists. They are not inserted into
  the parent transcript.
- Background jobs run in the same cwd/tool policy as ordinary tools. Harness
  serializes session/job metadata, not concurrent filesystem edits.

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
  1024 bytes on a UTF-8 rune boundary, with an ellipsis when truncated.
- **Schema** is the MCP `inputSchema` passed through verbatim; an absent schema
  (nil/empty/`null`) becomes `{"type":"object"}` so the model always sees a valid
  object schema.
- **`ReadOnly(input)` is policy-controlled.** Harness trusts
  `annotations.readOnlyHint:true` for enabled MCP registrations, so advertised
  read-only tools can join read-only parallel islands (§8.1) and can be exposed
  to agents whose `mcp_tools` mode is `read_only` (§14).
- **Result mapping** flattens the MCP `CallToolResult` to one string for the model:
  `text` blocks pass through; other blocks become bracketed placeholders —
  `[image: <mime>]`, `[audio: <mime>]`, `[resource_link: <uri> (<name>)]`,
  `[resource: <uri>]` (bare `[resource]` if no uri), `[unsupported content block: <type>]`.
  Blocks join with `\n` in order. If nothing renders but `structuredContent` is
  present, the raw structured JSON is the fallback.
- **Errors:** a transport/protocol error returns `("", err)` so `Dispatch` renders
  `error: <err>`. A successful result with `isError` true returns the rendered
  text as an `error` (empty text gets a stand-in), so the failure flows through the
  normal tool-error path.

The shared `*mcptools.Conn` is a lazily-reconnecting wrapper around one
`mcp.Client` session to the proxy. It spawns no goroutines; reconnection is
synchronous on the calling goroutine under a backoff gate, so a down proxy
fast-fails subsequent calls rather than storming reconnects. A proxy crash
mid-session surfaces as error tool results; the next call reconnects when the
backoff allows.

### 9.17 `record_plan` (`internal/plan`)

- Persists an implementation plan as markdown under the live session directory:
  `<session>/plans/NNNN-<slug>.plan.md`, written temp-then-rename. Input is
  `{title, plan, steps?, files?, verification?}`; `plan` is the free-form markdown
  body. Returns the absolute path.
- The store is one `*plan.Store` per process (like `update_todos`); the list is
  saved in `state.json` (`Session.Plans`), reseeded on resume, and reset on
  `/clear`. `internal/plan` is a leaf package so `internal/session` can persist the
  `Plan` type without importing tools.
- Available to every default agent (in `defaultTools`), so plans are a first-class
  artifact even outside plan mode. The session directory is read at call time, so
  it errors clearly when none exists (one-shot mode).

### 9.18 `request_implementation` (`internal/plan` + `internal/tools`)

- The plan agent's request to hand the recorded plan to an implementation agent.
  Input is `{brief, agent?, plan_path?, model?}`. It requires a recorded plan
  (defaults to the most recent); the implementation agent reads the plan as its
  task spec rather than being handed only the brief.
- Tools cannot prompt, so it only records a `plan.HandoffRequest` in a shared
  `*plan.Pending` holder and returns. The REPL prints a one-time notice after the
  turn and performs the approval + switch via `/handoff` (§10). It errors in
  one-shot mode (no interactive approval).

## 10. CLI / REPL (`internal/ui`)

### Rendering

- Assistant text and reasoning summaries use a small stdlib-only Markdown
  renderer on terminal output: emphasis becomes ANSI bold/italic when color is
  enabled, headings keep their `#` markers and render bold, lists normalize and
  indent continuations, paragraphs and list bodies wrap to terminal width (80
  columns when the width is unavailable), tables are padded, and URLs render
  visibly with cyan highlighting. Redirected one-shot stdout remains raw model
  text.
- The built-in system prompt asks tool-using models for brief user-facing
  commentary before tool calls and at meaningful work milestones. These
  commentary messages are normal assistant text; Responses `phase` metadata is
  preserved in transcript history when the provider supplies it.
- When Responses phase metadata marks visible commentary or reasoning output
  before `final_answer` text, live rendering and session replay insert a
  Markdown `---` delimiter with blank lines around it before the final answer.
  Providers without phase metadata keep their assistant text stream unchanged.
- Responses API reasoning summaries are semantic model-to-user output events,
  not notices and not transcript messages. They default off. When explicitly
  enabled, interactive runs render them to stdout as a compact two-space indented
  block headed by a timestamp line such as `[16:15:34 reasoning]` (the header
  drops the timestamp and reads `[reasoning]` when status timestamps are
  disabled) and closed by an `[end reasoning]` footer. Non-interactive runs render
  explicitly enabled summaries to stderr.
- Model progress renders as plain stderr lines, e.g. `[model: turn 1 waiting]`.
  When pricing is known, the returned provider request also emits a checkpoint:
  `[model: turn 1 cost: $0.0012 · totals: $0.0034 prompt · $0.0456 session]`.
  Model start/completion events are always recorded in `raw.ndjson` for timing
  diagnostics, even when pricing is unknown and no cost line is shown. Retried
  streamed attempts also record a discard marker so replay can omit the abandoned
  attempt's assistant/reasoning deltas while keeping the retry notice.
- **Live wait counter (TTY, non-quiet).** While a model request or a tool call is
  outstanding, the static waiting line is replaced by a single in-place line painted
  with `\r\x1b[2K` and repainted ~once a second by a `time.Ticker` goroutine (with a
  mutex + stop-and-drain handshake so it never interleaves with streamed bytes):
  `[model: turn 1 · 12s]` (or `[tool: grep · 3s]`), with the running context-window
  percentage appended for model waits (`· ctx 30%`). It is erased the instant real
  output or a tool line scrolls in — not a sticky bar or scroll region.
- **During-turn input line.** Keystrokes typed during a turn are read in raw,
  echo-off mode and shown on that wait line after a `>` marker
  (`[model: turn 1 · 12s] > draft`). During-turn input is never auto-submitted: Enter
  inserts a newline into the buffer rather than starting a turn. On both normal turn
  completion and interrupt (`^C`/Esc-Esc), the accumulated buffer is deposited into
  the next REPL prompt as editable, pre-filled text (cursor at end) instead of being
  drained straight to the model. `^C`/Esc-Esc still cancel the turn.
- Live tool-call construction renders progress to stderr by default:
  `[tool-call: name id=...]`. Disable with `-tool-stream=false`,
  `HARNESS_TOOL_STREAM=false`, or `"tool_stream": false`. Partial argument deltas
  are not printed; session replay keeps completed tool calls and results.
- Tool calls render as one-liners:
  `[grep] args=["-R","-n","func main","."] → 14 lines, 2.1KB`
  built from the tool name, key args, and a result summary. `-v` adds the first ~5 lines
  of each result, dimmed.
- Large estimated contexts, payloads, or tool schemas print one warning per user
  turn because they can materially slow first response latency.
- Per-turn usage line:
  `[turn: 3 model turns · 12.4k (18.0k) in / 1.8k (2.6k) out · $0.071 ($0.101) · 4.3s]`
  (cost omitted for configured models without pricing metadata). When non-zero it
  also appends cache-read tokens with the cache-hit ratio (`· cache 3.0k read (75%)`)
  and reasoning tokens (`· 450 reasoning`). A model with no configured price prints a
  one-time-per-model `[note: no price configured for "<model>"; …]` notice instead of
  silently dropping cost.
- Bracketed status lines are prefixed inside the bracket with local time by
  default, for example `[16:15:34 tool-call: name id=...]`.
  `-timestamps=full` (or `long`) uses `yyyy-mm-dd hh:mm:ss`; `-timestamps=none`
  or `-no-timestamps` disables status timestamps.
- ANSI color only when stdout is a TTY (`os.Stdout.Stat()` mode check);
  `NO_COLOR` env or `-no-color` disables color. Structural Markdown rendering
  remains legible without color.
- Startup diagnostics use `log/slog` with a plaintext handler: `[level] [category]
  message`. Default level is `info`; `--log-level` or `LOG_LEVEL` accepts `debug`,
  `info`, `warn`, or `error`.
- `-q`/`--quiet` suppresses bracketed status messages (tool calls, model turns,
  notices), disables live tool-stream progress and the live wait counter, suppresses
  reasoning summary output unless `-reasoning-summary` is explicitly set on the
  CLI, and suppresses status lines in `harness session replay`; it does not filter
  slog diagnostics. The per-turn usage/cost line is governed by a separate
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
cluster and emoji-width handling are out of scope. Bracketed paste markers are parsed so
a multi-line paste into an empty prompt is submitted as one literal user prompt,
preserving embedded newlines and preventing pasted `/commands` from dispatching as
meta-commands. For non-TTY input the REPL keeps the `bufio.Reader` line path, so long
scripted prompt lines are not capped by Scanner's token limit.

At an interactive TTY prompt only, a non-pasted line starting with `!` is a local
shell escape. The command text after `!` runs via the user's shell (`$SHELL -lc`,
falling back to `bash -lc` then `sh -c`), prints directly to the terminal, and
returns to the prompt without a model request, prompt-submit hook, transcript
message, or replay event. `!!` escapes a literal leading `!`; one-shot mode,
non-TTY/scripted input, bracketed paste, and external-editor prompt content treat
`!text` as ordinary prompt text.

Tab completion is intentionally small and stdlib-only. It is active only in raw
prompt-editor buffers that start with `!`: the first word completes executable names
from `PATH` unless it starts with `/`, `~/`, `./`, `../`, or otherwise contains `/`;
path words complete filesystem entries from cwd, absolute paths, or the current
user's home directory while preserving the typed prefix.

`repl_prompt` (also `-repl-prompt` / `HARNESS_REPL_PROMPT`) is a format string
rendered at every idle prompt boundary, so dynamic values reflect runtime
changes before each read. The default is `[{agent}] > `. Supported placeholders
are `{agent}`, `{cwd}`, `{git_branch}`, `{provider}`, `{model}`, and
`{model_info}`. Literal escapes `\n`, `\t`, `\\`, `\{`, and `\}` are decoded
for config, env, and flag values; unknown placeholders or invalid escapes are
configuration errors.

`-repl-edit-mode=vi` (or `HARNESS_REPL_EDIT_MODE=vi` / config `repl_edit_mode`)
switches the raw prompt editor to a small vi keymap. The prompt starts in insert
mode; bare Escape enters normal mode, while terminal escape sequences such as arrow
keys, bracketed paste, and CSI-u key events remain parsed as terminal input. Normal
mode supports `h/l`, `0/^/$`, `w/W/b/B/e/E`, `i/a/I/A`, `x/X`, `D/C/S`, `Y` (yank the
whole line), `k/j` history navigation, `d`/`c`/`y` operators with those motions plus
doubled line operators (`dd`, `cc`, `yy`), and local `p`/`P` paste from the prompt
editor's yank buffer. Counts, registers, search, visual mode, macros, and full Vim text objects are
out of scope.

Ctrl-G opens the external prompt editor from the raw-mode prompt with the current draft.
During an active REPL turn, harness restores the prompt terminal mode and temporarily
configures Escape as the second canonical-mode line delimiter so Esc-Esc can cancel the
turn; typeahead lines are queued for the next prompt. Bracketed paste is disabled while
Escape is armed, then restored when the prompt returns. Before launching the editor,
harness restores the original termios and disables bracketed paste so the editor owns a
normal TTY; after it exits, the REPL reapplies its prompt settings. `!command`
shell escapes use the same terminal handoff.

External editor prompt files use `$VISUAL`, then `$EDITOR`, then `vi`, attached to
`/dev/tty`. The temp file contains the visible output from the latest recorded turn,
then a delimiter line (`--- HARNESS EDIT ... ---`), then any draft text. Only content
after the exact delimiter is submitted as the next prompt; edits above the delimiter are
context for the user only. Missing delimiters abort the edit and keep the temp file.
Empty edited content returns to the prompt without running a turn.

### Meta-commands

Lines starting with `/` are commands; `//` escapes a literal slash. At an
interactive TTY prompt, lines starting with `!` run a local shell command; `!!`
escapes a literal bang. In a normal typed prompt, `$skillName` mentions an
available skill anywhere in the text; the next model turn gets request-only
context telling it to read that skill's `SKILL.md` before acting. `$$` escapes a
literal `$`.

| command | effect |
|---|---|
| `/help` | list commands |
| `/exit`, `/quit` | save, print a session token summary, and exit |
| `/clear` | echo discarded session token/cost totals, then reset conversation and rotate to a fresh session file |
| `/compact` | force compaction now |
| `/context` | dump the current provider-neutral model context as JSON |
| `/context <file>` | save the current provider-neutral model context as JSON |
| `/usage` | cumulative input, cached input, output, reasoning tokens, and cost (also cache-write tokens when present). Usage is bucketed per `provider/model`: with one model it is a single line; after a model change it breaks down per model and always ends with the session-total cost. The live per-turn line shows the active model's cumulative tokens with the session-total cost; a model-changing `/agent`, `/model`, or handoff prints the breakdown before the active counters reset for the new model. |
| `/tools` | list enabled built-in and MCP tools with descriptions, plus disabled optional tools |
| `/image` | list images queued for the next prompt |
| `/image <path>` | attach an image to the next prompt |
| `/image --detail <level> <path>` | attach an image with per-image detail |
| `/image --clear` | clear queued images |
| `/edit [draft]` | open an external editor for the next prompt |
| `/save [file]` | force save (optionally elsewhere) |
| `/model` | choose a configured provider/model; interactive runs can optionally save it as the default |
| `/model <id>` | switch subsequent turns to model `<id>`; a near-miss falls back to a unique prefix/substring match before erroring; interactive runs can optionally save it as the default |
| `/model <provider>:<id>` | switch to `<id>` on a specific configured provider; interactive runs can optionally save it as the default |
| `/reasoning` | list reasoning controls for the current model |
| `/reasoning on`, `/reasoning off`, `/reasoning default` | set explicit reasoning toggle or return to provider defaults |
| `/reasoning budget <n>` | set reasoning budget tokens for subsequent turns |
| `/reasoning effort <level>` | switch reasoning effort for subsequent turns |
| `/reasoning summary <auto\|concise\|detailed\|none>` | switch Responses API reasoning summaries for subsequent turns |
| `/effort` | list reasoning effort levels for the current model, marking the selected one |
| `/effort <level>` | switch reasoning effort for subsequent turns |
| `/agent` | list agents and descriptions, marking the current one and agents delegatable from it |
| `/agent <name>` | switch the active agent |
| `/mode`, `/mode <name>` | alias for `/agent` |
| `/plan` | alias for `/agent plan` |
| `/auto` | alias for `/agent auto` |
| `/handoff [agent]` | hand the recorded plan to an implementation agent after y/N approval: archive the planning transcript, switch agent (and model when requested), and reseed a clean context with the plan pointer plus the brief (§14) |
| `/background` | list background jobs |
| `/background <id>` | show a background job's status, result, and transcript path |
| `/background cancel <id>` | cancel a running background job |
| `/skills` | list available skills |
| `/vi on\|off` | enable or disable vi-style prompt editing |
| `!command` | run a local shell command at an interactive TTY prompt |

Anthropic usage does not currently expose a separate reasoning-token field;
extended thinking is counted in output tokens, so the reasoning total remains
zero for Anthropic sessions.

`/model <name>` resolves exactly first, then falls back to a case-insensitive
unique prefix and then unique substring match over the catalog; an ambiguous match
lists the candidates rather than switching. An unknown `/command` prints
`unknown command "<cmd>"; did you mean <suggestion>? (type /help)`, where the
suggestion is the nearest known command by a stdlib Levenshtein distance (shared
prefix wins, threshold `1 + len(cmd)/3`).

### Flags

```
-p <prompt|->     one-shot mode; "-" or piped stdin reads the prompt from stdin
-provider <name>  model proxy provider id
-model <id>
-model-proxy-url <url>
-system-prompt <text|@file>    replace the static system prompt
-no-env           omit environment context block
-resume <file>    load a session transcript and continue
-session <file>   explicit session save path
-max-turns <n>    model turns per user prompt; <=0 means unlimited (default 250)
-max-turn-tokens <n>   stop a user turn after this many accumulated tokens; 0 = unlimited (default 0)
-tool-timeout <s>      per-tool-call timeout backstop in seconds; <=0 disables (default 600)
-histfile <path>      REPL history file path (default <stateDir>/harness/history)
-histfilesize <n>     max REPL history entries stored on disk (default 1000, 0 disables)
-histsize <n>         max REPL history entries loaded into memory (default 1000, 0 disables)
-default-context-window <n>
-context-window <n>
-reasoning-effort <level>
-reasoning-enabled <bool>
-reasoning-budget-tokens <n>
-reasoning-summary <auto|concise|detailed|none>
-responses-stateful   Responses previous_response_id continuation (default true)
-image-detail <level>   default image detail: auto, low, high, or original
-image <path|detail:path>   attach an image in one-shot mode; repeatable
-agent <name>
-search-tools <auto|grep|rg|both>
-v                show tool result snippets
-tool-stream      show live tool-call progress (default true)
-show-diffs       show per-tool-call file diffs for built-in file edits
-q, --quiet       suppress status messages and reasoning output unless -reasoning-summary is set
--log-level <level>  diagnostic log level: debug, info, warn, error (also LOG_LEVEL)
-no-color
-timestamps <mode>  status timestamps: short (default), full/long, or none
-no-timestamps      alias for -timestamps=none
-repl-prompt <text>    REPL input prompt format
-repl-edit-mode <mode> REPL prompt edit mode: emacs (default) or vi
-format <text|json>  output format for informational commands (default text)
-show-config     dump resolved config, including defaults, as JSON and exit
-agents          list configured agents and exit
-models          list configured providers and models and exit
-check-model-proxy  check harness-model-proxy reachability and exit
-hooks <file>    replace configured hooks with this hook config file
-config <file>    alternate config path
```

`-show-config` includes the effective merged agent definitions and static
`system_prompt`; it exits before contacting the model proxy. Dynamic runtime
prompt sections such as env context, user/project `AGENTS.md`, skills, and the
active agent prompt are not included in the `system_prompt` field.

`-agents` prints a readable resolved agent list without contacting the model
proxy. `-models` reuses the bounded proxy catalog request and prints configured
provider/model rows before session creation. `-format json` is supported by
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
        "matcher": "run_command|apply_patch",
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

`SessionStart` hooks are the supported path for dynamic prompt context, such as
detecting whether GNU sed is available as `gsed` or reporting the active bash
version. Static personal preferences belong in `~/.agents/AGENTS.md`; command
output belongs in hook context.

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
- Runs exactly one user turn, saves the session, exits.

## 11. Session persistence (`internal/session`)

```go
type Session struct {
    Version       int                `json:"version"` // 2 (adds plans + per-model usage)
    Provider      string             `json:"provider"`
    Model         string             `json:"model"`
    Created       time.Time          `json:"created"`
    Updated       time.Time          `json:"updated"`
    System        string             `json:"system"`
    Agent         string             `json:"agent,omitempty"`
    Turn          int                `json:"turn,omitempty"`
    Messages      []llm.Message      `json:"messages"`
    ResponseState *llm.ResponseState `json:"response_state,omitempty"` // Responses stateful continuation anchor
    Todos         []todo.Item        `json:"todos,omitempty"`          // update_todos list, reseeded on resume
    Plans         []plan.Plan        `json:"plans,omitempty"`          // record_plan list, reseeded on resume
    Usage         UsageTotals        `json:"usage"`                    // session aggregate (back-compat + resume seed)
    UsageByModel  map[string]UsageTotals `json:"usage_by_model,omitempty"` // per "provider/model" cost; resume seeds a single bucket from Usage when absent
}

type UsageTotals struct {
    llm.Usage         // cumulative token counts
    CostUSD   float64 `json:"cost_usd"` // 0 when the model has no price entry
}
```

- A session path is a directory. `state.json` is the compact resumable state,
  `raw.ndjson` is append-only replay data, `compactions/` stores raw messages removed
  from active context, and `artifacts/tool-results/` stores full truncated tool output.
- **Saved after every turn**, atomically (write `state.json.tmp`, `os.Rename`). Cheap
  relative to a model call; crash-safe for long sessions.
- Every saved message and append-only replay event carries a timestamp.
- When Responses stateful continuation is active, `state.json` stores the last
  `previous_response_id` and the number of local messages represented by it.
  Resume only restores this state when the active provider/model still match and
  the selected provider reports `api_type: "responses"` and
  `responses_stateful:true`; compaction, `/clear`, provider/model/tool/system
  changes, and rejected prior response ids clear it.
- Image bytes are embedded in `state.json` as provider-neutral base64 blocks so
  resume is self-contained; `raw.ndjson` records only image metadata for replay.
- Auto-save to `~/.local/state/harness/sessions/<timestamp>`; the path is printed at
  startup. `-session` chooses a directory; `-resume` loads `state.json` (applying the
  dangling-tool-use repair, §4). `/clear` rotates to a fresh directory.
- Child-agent runs are stored below `children/<child-id>/` with their own
  `state.json`, `raw.ndjson`, `meta.json`, and artifacts. Parent resume ignores these
  child transcripts; they are forensic sidecars. `meta.json` is a `ChildMeta` index —
  id, parent id, kind, agent, provider/model, status, task preview, transcript/replay
  paths, error, usage, and message count.
- `harness session replay <session-dir>` prints `raw.ndjson` as the familiar
  user-facing terminal view, filtering assistant/reasoning deltas from retry
  attempts that were explicitly discarded before a later successful attempt.
  Raw assistant deltas remain unchanged on disk; replay renders Markdown at
  display time.
- `harness session timings <session-dir>` reads `raw.ndjson` timestamps and
  prints turn totals, model attempt durations, tool durations, largest event gaps,
  and context/payload estimates.
- Transcripts are provider-neutral; resuming under a different provider/model works.
  When flags disagree with the state, flags win with a warning.

### REPL history

Global REPL history persists across sessions, mirroring bash's familiar model:

- **Location:** `<stateDir>/harness/history` (one entry per line, plain text), or the
  path in `HARNESS_HISTFILE` / `-histfile` / config `histfile`.
- **`HARNESS_HISTFILESIZE` / `-histfilesize` / `histfilesize`:** max entries stored
  on disk (default `1000`, `0` disables persistence).
- **`HARNESS_HISTSIZE` / `-histsize` / `histsize`:** max entries loaded into memory
  at REPL start (default `1000`, `0` disables recall).
- **Behavior:** entries are appended on each non-empty, non-multiline input submission.
  On REPL start, the file is loaded, deduplicated (keeping the last occurrence), and
  rewritten if it exceeds `HISTFILESIZE` (self-healing). At most `HISTSIZE` recent
  entries are surfaced for up-arrow recall.
- **Concurrency:** uses `O_APPEND`, so multiple parallel REPLs sharing one file stay
  safe on POSIX systems.
- **Scope:** REPL sessions only; one-shot (`-p`) does not load or save history.

## 12. Compaction (`internal/agent/compact.go`)

- **Trigger:** when `max(reported input tokens, estimated full-request footprint)`
  reaches **78%** of the model's context window (headroom for the summary call plus the
  next turn). The reported-input figure counts cache-read/cache-write tokens too, since
  cached context still occupies the window. This fires after a turn **and proactively
  mid-turn** — before the next model request within a turn, when tool results balloon the
  estimate — so a single turn's tool output cannot overflow the window before the next
  request. Also manual `/compact`. The estimate side is the last **measured** input
  tokens (`lastInput`) plus a bytes/4 estimate of only the messages appended since
  that measurement (the append boundary), so the trigger tracks real usage instead of
  re-estimating the whole transcript; the raw bytes/4 estimate is reserved for the
  degradation ladder.
- **Live-transcript retention pass.** Before each model request the agent runs a
  pure-local retention pass (no model round-trip) over aged history: read-only
  tool-result blocks older than `compact_keep_turns` and larger than
  `defaultSummaryToolResultSize` (4096 bytes) are trimmed to a head slice plus a hint
  (`[older tool output trimmed …]`, or an archive pointer when the sink can archive
  the full output), and `BlockImage` blocks two or more turns old are swapped for a
  text placeholder. It only ever shortens text or turns an image into text, so the §4
  transcript invariant still holds, and it is idempotent (already-trimmed/placeholder
  blocks are skipped). This keeps the window smaller between full compactions.
- **Mechanism:** keep the system prompt and the configured number of recent turns
  verbatim (`compact_keep_turns`, default 4; a turn = a user message through the
  following end-turn). Send everything older to the model with the summarization
  instruction in `prompts/compaction-summary.txt`: preserve the task/goal, decisions
  made, files created/modified and their current state, key facts learned, open TODOs;
  do not invent. Summary output is capped by `compact_summary_max_tokens` (default
  2048). Replace the old messages with a single user message:
  `=== Summary of earlier conversation ===\n<summary>`.
- Before summarization, large old tool results and tool inputs are reduced to
  previews (`compact_tool_result_max_bytes`, default 4096; a **negative** value disables
  this reduction entirely), and old images are replaced with text placeholders. If older
  history is too large for one summary request, it is summarized in chunks, then the
  chunk summaries are summarized.
- **Hooks.** A configured `PreCompact` hook runs before summarization; if it blocks,
  compaction is skipped with a `[compact skipped: <reason>]` notice. A `PostCompact`
  hook runs after the transcript is replaced; its `additionalContext` is added as
  request-only context for the next model request, and a block surfaces a
  `[post-compact hook blocked after compaction: <reason>]` notice. Both receive a
  `trigger` field (`auto` or `manual`).
- Before replacing active history, raw removed messages are archived under
  `compactions/`; the active summary includes the archive reference.
- **Summary call hardening (`summarizeOne`).** The summarization request runs with
  reasoning disabled (`Reasoning: llm.ReasoningConfig{}`) regardless of the session's
  effort, so compaction never spends a thinking budget. It retries transient
  mid-stream errors with the shared `retry.Next` backoff (up to `streamRetries`) so a
  429 at 78% does not abort compaction. If the summary itself is truncated
  (`StopMaxTokens`) it doubles the token budget and retries once, then accepts the
  result.
- **Image-aware estimation.** `estimateTokens`/`estimateRequest` weight each
  `BlockImage` at a flat `imageTokenEstimate` (1600 tokens) rather than counting its
  base64 bytes at bytes/4, which wildly overstated images. Correspondingly,
  `truncateLargestBlock` ranks an image by that token weight, so a large text result
  is truncated before an image.
- The summary call's tokens and cost are added to session totals and reported:
  `[compacted: 38 messages → summary · 9.1k in / 0.4k out · $0.05]`. The `· $X.XX`
  cost segment is omitted for models with no price entry.
- **Degradation:** if still over budget, keep only the last turn; if still over,
  hard-truncate the largest tool result/input/image blocks in place with markers.
  When there is nothing older than `compact_keep_turns` to summarize but the
  transcript is still over budget, the same ladder degrades the **oversized single
  turn** in place. Each degrade pass deep-copies before mutating (so a post-degrade
  `ValidateTranscript` failure rolls back to the live transcript) and skips a rewrite
  that would not actually shrink (`[compact: transcript over budget but nothing left
  to shrink]`). Never wedge.
- **Failure:** if the summary or archive step errors, abort compaction, warn, and keep
  the full transcript — the next call may fail visibly on context length, which beats
  silent data loss.
- Compacted transcripts must still satisfy the §4 invariant (kept turns are whole turns,
  so no tool_use/tool_result pair is ever split).

## 13. Testing strategy

Seams that make the system testable: the `Provider` interface (scripted `FakeProvider`),
the `Tool` interface + registry, REPL via injected `io.Reader`/`io.Writer` (TTY detection
injectable), the retry clock, and `ValidateTranscript`.

| Layer | Tests |
|---|---|
| `internal/sse` | frame parsing tables; huge frames; truncated input |
| providers | `httptest.Server` replaying `.sse` golden fixtures per dialect → assert ordered events; golden request-JSON tests (Responses input items, Chat role:tool hoisting, args-string vs object, system placement, `stream_options`, cache_control); tool-call reassembly tables (fragment splits, empty args → `{}`, interleaved parallel calls, invalid tail → retryable stream error); truncated stream; mid-stream cancellation; retry loop via injected sleeper (429-then-200, 400 immediate failure, budget exhaustion) |
| `internal/retry` | `Next`: jitter bounds, 30s cap, Retry-After floor |
| tools | table-driven against `t.TempDir()`; `grep` wrapper against the host CLI; optional `rg` registration with a fake executable on PATH; `git` against a scratch `git init` repo (skipped if git absent); `run_command` timeout via `sleep`; `apply_patch` at the tool level covers the Codex Add envelope, the `patch`/`patchText`/`patch_text`/bare-string input aliases, and conflicting-alias / parse-error format-hint paths, while `internal/tools/patch` covers parse + apply for create/update/delete/rename and first-rejection-leaves-file-untouched |
| agent loop | `FakeProvider` scripts: multi-tool batches, error-result feedback (next request carries the error), max-turns stop, cancellation → transcript still re-sendable |
| delegate | child-agent request shape, model-visible delegatable agent enum, parent-tool subset rejection, recursive delegate rebinding, private child todo stores, child transcript persistence, metered usage folded into parent turn totals |
| background | job start/completion, one-shot context delivery, notices, cancellation/errors, child transcript path preservation |
| session | save→load→save round-trip; atomic rename leaves no `.tmp`; resume repair; cross-provider resume |
| compaction | canned summary via FakeProvider; old messages collapse, last 4 turns kept; invariant holds |
| ui | scripted REPL input (`/help`, prompt, `/exit`); rendering goldens with fake clock/usage |

Cross-cutting: `ValidateTranscript` is asserted after every transcript mutation in every
test that touches one.

Beyond the unit tables, `//go:build integration` suites build the real binaries and drive
them as subprocesses against hermetic local mock servers (no API keys, no network):
`cmd/harness` exercises tool round-trip, `^C` mid-stream (exit 130 + valid resumable
transcript), resume-of-interrupted-session, and the LSP shim end to end. Run the fast
unit tests with `make test` (`go test ./...`) and the integration legs with
`make test-integration` (`go test -tags=integration ./cmd/harness`).

## 14. Agent definitions (`internal/agentdef`)

An **agent definition** is a named bundle of an allowed-tool set, optional
provider/model, description, and extra system-prompt instructions. It lets one
harness behave as a collaborative planner, autonomous worker, specialized
reviewer, or the wide-open default without separate binaries.

- **Selection** follows the standard precedence (§7): `-agent` flag >
  `HARNESS_AGENT` > `agent` in the config file > the built-in default `auto`. An
  empty value means "unspecified", so a resumed session's saved agent (§11) can
  supply it before the `auto` fallback. `/agent <name>` switches at runtime;
  `/agent` lists inside the REPL; `harness --agents` lists from the CLI. `/mode`
  is a REPL alias only.
- **Built-ins:** `auto` (all available built-in tools plus discovered MCP tools,
  including `record_plan`, `delegate` and background job tools; its
  `prompts/agents/auto.txt` is a one-byte file — a single newline — that trims to
  empty, so it contributes no prompt body), `plan` (inspection tools including the
  configured search tool(s), optional `git_readonly` when git is installed,
  read-only MCP tools, `write_tmp_file`, `record_plan`, `request_implementation`,
  `delegate`, and background job tools, plus a planning prompt from
  `prompts/agents/plan.txt`), and `independent` (all available built-in tools plus
  discovered MCP tools, including `record_plan`, `delegate` and background job
  tools, a complete-without-asking prompt from `prompts/agents/independent.txt`).
  `record_plan` (§9.17) is in every default agent's set; `request_implementation`
  (§9.18) is plan-only.
- **Config `agents`** entries **field-level merge** onto a built-in of the same name:
  a non-empty `description`, `allowed_tools`, `mcp_tools`, `prompt`, `provider`,
  `model`, or `reasoning` replaces, and an omitted field inherits. A new name
  defines a new agent (no `allowed_tools` ⇒ the full default set). Agent prompts
  accept `@file` and are expanded once at startup (fail-fast); relative config-file
  references resolve from the config file directory.
- **MCP exposure:** `mcp_tools` is one of `disabled`, `read_only`, or `all` (with
  `read-only`/`readonly` accepted as aliases for `read_only`) and controls automatic
  exposure of discovered MCP tools. An invalid value is a fail-fast validation error
  (surfaced by `main`/`--show-config` after field-level merging). Built-ins default to
  `all` for `auto`/`independent` and `read_only` for `plan`; a new agent with no
  explicit `allowed_tools` defaults to `all`, while an explicit `allowed_tools`
  whitelist defaults to `disabled` unless `mcp_tools` opts it back in. Explicit
  `mcp__...` names in `allowed_tools` remain strict whitelist entries.
- **Provider/model:** an agent without provider/model uses the current session
  provider/model. An agent with `model` only resolves using the current provider as
  preference. An agent with `provider` and `model` requires that exact catalog pair.
  `/agent <name>` prints the provider/model line and warns when the switch changes
  provider or model because prompt cache may start cold and increase token usage or
  cost.
- **Per-agent reasoning:** an agent's optional `reasoning` field pins its thinking
  effort. It overrides the session base effort whenever that agent is selected
  (startup, `/agent`, delegate, or a handoff target) and is then made
  model-compatible and validated like any effort. This lets a cheap implementation
  agent pair a smaller `model` with a lower `reasoning`.
- **Plan → implementation handoff:** the `plan` agent records plans with
  `record_plan` (§9.17) and requests a handoff with `request_implementation` (§9.18).
  On `/handoff` (§10) the REPL prompts for approval, archives the planning
  transcript via `SaveCompaction`, switches to the target agent — default `auto`,
  overridable by `--handoff-agent`/`HARNESS_HANDOFF_AGENT`/`handoff_agent` or the
  `/handoff <agent>` argument — optionally swaps the model, then reseeds a clean
  transcript with a pointer to the recorded plan plus the brief and clears the
  planning todos. Reusing the same in-session switch (not `delegate`) avoids the
  `delegate` subset gate, so a read-only `plan` agent can hand off to a
  write-capable implementation agent. Interactive REPL only.
- **Tool gating** is the harness's one departure from the no-sandbox stance (§2): the
  agent's tool set is realized by `tools.Registry.Subset`, building a registry that
  holds only the allowed tools. Because the agent advertises (`Specs`) and dispatches
  from the same registry, an excluded tool is neither offered nor callable. The
  underlying tools still assume an external sandbox for real isolation; gating only
  shapes what each agent exposes. `Agent.SetTools` swaps the registry for `/agent`.
- The agent prompt is appended to the composed system prompt as the final
  section, so it layers on top of the static instructions, env block, and
  user/project AGENTS.md sections. A configured `system_prompt` replaces the
  static instructions before the runtime sections are added. The active agent is
  saved with the session and restored on `-resume` (flags win).

## 15. MCP proxy (optional)

Remote MCP support is opt-in (`mcp.enable`, §7) and lives behind a **second
binary**, `harness-mcp-proxy`. Harness does not talk to remote downstream MCP
servers directly in that path: the proxy owns them and presents their merged
tools to harness as a single MCP server over streamable HTTP. Harness and the
proxy therefore speak MCP to each other — JSON-RPC 2.0, protocol revision
`2025-06-18` (`internal/mcp`, `internal/mcp/jsonrpc`). Separately,
`mcp.local` is an explicit local-stdio-MCP slot where harness itself can spawn
one configured command and register its tools.

**Why a separate process.** The remote daemon decouples downstream-server
lifetime from any one harness session: stdio children configured in the proxy are
spawned once and shared across every concurrent harness session, surviving REPL
restarts, instead of being re-spawned per process. The harness side still depends
on the thin `internal/mcptools` adapter for tool dispatch (§9.16).

- **Proxy config** (`internal/mcpproxy`) is Claude Code-compatible:
  `{"mcpServers": {name: {command,args,env} | {type:"http"|"streamable-http",url,headers,auth}}, "proxy":
  {listen,logFile,logLevel,logFormat}}`, at `$XDG_CONFIG_HOME/harness-mcp-proxy/config.json`
  (else `~/.config/...`). `${NAME}` and `${NAME:-default}` references are expanded
  strictly (literal `$`, `$5`, `$$`, or unterminated `${` is preserved verbatim;
  an unset strict var warns and expands to empty). Invalid servers are skipped
  with a warning, never fatal. `proxy.listen` defaults to `127.0.0.1:8766`;
  `proxy.logFormat` defaults to built-in slog JSON and also accepts `text`.
  Library code returns warnings; the CLI logs them.
- **Downstream supervision.** Each server gets a `Supervisor`. A **stdio** child is
  spawned in its own process group, initialized + `tools/list`ed under a 30 s
  timeout, its stderr drained to the proxy log; a crash restarts with backoff,
  and 5 consecutive failed (re)starts disables it permanently. A **streamable-HTTP**
  server is connected lazily with the user's headers plus optional dynamic auth
  headers; there is no restart loop (the process is not ours), and a server-side
  session expiry (HTTP 404) triggers one transparent re-initialize-and-retry. A
  not-ready server returns an `isError`
  result whose text is `mcp server <name> is unavailable (<state>)` (the
  parenthesized `<state>` is the supervisor's lifecycle state, e.g. `starting`,
  `restarting`, or `failed`), not a JSON-RPC error, so the failure reaches the
  model as a normal tool failure.
- **Aggregation** (`Registry`). Tools merge under `mcp__<server>__<tool>`, sorted by
  name, with a reverse route map (so a server name may itself contain `__`). The check
  is applied to the **entire** qualified string — the `mcp__` prefix, server, `__`
  separator, and tool together must match `[a-zA-Z0-9_-]{1,64}`, so the 64-character
  budget is shared across all of them. A name that is not provider-safe is **dropped
  with a warning**, never truncated (truncation could collide and misroute).
  `tools/list` is cursor-paginated.
- **Lifecycle / manual start.** Harness **never starts the remote HTTP proxy**; the
  operator runs `harness-mcp-proxy serve` themselves (from a shell, a launchd
  agent, or a systemd user unit) and the daemon outlives harness, shared across
  sessions. A second `serve` on the same HTTP address fails with the normal bind
  error, matching `harness-model-proxy`. One-shot runs connect directly to the
  proxy and register tools under a 5 s timeout; on failure they emit exactly one
  warning (`mcp: cannot connect to proxy at <url>: <err>; MCP tools unavailable`)
  and continue with no MCP tools. Interactive REPL runs start remote registration
  in the background; the first failure warns with `retrying in background`, later
  attempts continue with backoff, and a successful discovery is applied at a
  prompt boundary. **Any** failure warns and continues — MCP never fails harness
  startup. There is no remote proxy spawn/auto-start budget.
- **HTTP server.** The proxy serves its merged surface over **streamable HTTP** on
  `proxy.listen` (or `serve -listen`). It is **plain HTTP** — TLS and any
  stronger auth belong to a reverse proxy in front. The handler (`internal/mcp`
  `NewHTTPHandler`, spec revision `2025-06-18`) is tools-only and JSON-only:
  responses are always `application/json` (never `text/event-stream`), a `GET` is
  `405` (no server-push stream), `DELETE` ends a session (`204`), and sessions are
  created on `initialize`, carried by the `Mcp-Session-Id` header, and purged
  lazily after a 30-minute idle TTL. This 30-minute MCP **session** TTL is distinct
  from the HTTP server's 120-second connection `IdleTimeout`, which only closes idle
  keep-alive TCP connections, not MCP sessions. Because there is no server-push channel,
  `ListChanged` is reported **false** and clients re-list rather than being
  notified. A bind failure is fatal and the server is shut down gracefully on
  SIGINT/SIGTERM. Harness reaches the proxy by setting `mcp.proxy` to the URL
  plus an optional config-file-only `mcp.headers` map (sent on every request, for a
  reverse proxy's auth). Header values expand `${NAME}` and `${NAME:-default}`;
  unset strict refs are config errors. The `tools` subcommand debugs one with
  `tools -proxy <url>` or the configured/default URL.
- **Request logging.** The MCP proxy logs one structured record per routed
  `tools/call` with requester/clientInfo, downstream MCP server name, bare and
  qualified tool name, request/response bytes, duration, `is_error`, and any
  protocol error. Unknown tools are warning records.
- **Refresh semantics.** One-shot runs use the tool list discovered before the
  model request. Interactive REPL runs may gain remote HTTP MCP tools after the
  background registration succeeds; the prompt-boundary refresh hook applies that
  first successful discovery and can also consume a dirty flag when the underlying
  connection receives a list-changed notification on transports that support one.
  The HTTP proxy transport itself has no server-push channel, and a downstream
  streamable-HTTP server behind the proxy likewise refreshes only on
  session-expiry reconnect.
- **Harness-side exposure caps (restrict-only, config-file-only).** Where harness
  assembles the auto-exposed remote MCP tool names (`cmd/harness/mcp.go`), two
  optional `mcp` keys narrow the surface: `mcp.max_tools` caps how many discovered
  remote tools are auto-exposed (`0` = unlimited; negative rejected; overflow is
  truncated in discovery order with a warning), and `mcp.disabled_servers` drops
  named servers (the segment between `mcp__` and the next `__`) from auto-exposure.
  Neither counts local-MCP or LSP tools, and both affect **automatic** exposure only —
  an explicit `mcp__…` entry in an agent's `allowed_tools` still resolves against the
  full catalog.
- **Shutdown.** SIGINT/SIGTERM cancel the daemon: HTTP sessions close with the
  server, and each stdio child is reaped gracefully (close stdin → SIGTERM → SIGKILL
  on the process group, bounded by per-stage timeouts).
- **Auth and security.** HTTP downstream servers may set static `headers` and/or
  `auth`. Static headers are applied first, dynamic auth headers next, then MCP
  protocol headers override both. `token_command` delegates login/refresh to an
  external command and caches returned bearer tokens in memory. `oauth2` supports
  explicit `harness-mcp-proxy auth login|logout|status <server>` for browser PKCE
  or device-code flow, storing refreshable tokens under the proxy config dir. The
  proxy listener is a TCP endpoint with no transport security
  of its own, so it relies on the assumed local/front-proxy trust boundary (bind it
  to loopback and front it with a proxy for TLS/auth). The proxy loads its own
  config from the user's config dir; harness only learns the proxy URL. **Stdio
  servers inherit the proxy's full environment** — whatever
  environment the `serve` process was started with — plus the per-server `env`
  overrides, so do not configure untrusted stdio servers when secrets live in the
  environment.

The harness-side adapter contract (naming, description, schema, result and error
mapping, the reconnecting `Conn`) is §9.16. The CLI wrapper has four subcommands —
`serve` (the daemon), `tools` (connect to a running HTTP proxy and print the
aggregated table), `auth`, and `version` — with serve flags
`-config`/`-listen`/`-stdio`/`-log`/`-log-level`/`-log-format`.

## 15a. LSP code intelligence (optional)

The **LSP manager** (`internal/lspproxy`) launches already-installed language
servers on demand and exposes seven read-only navigation tools
(`lsp_definition`, `lsp_references`, `lsp_hover`, `lsp_document_symbols`,
`lsp_workspace_symbols`, `lsp_diagnostics`, and the read-only
`lsp_rename_plan`). The normal path is first-class, not generic MCP:
`lsp.enable=true` registers short `lsp_*` tools directly through
`internal/lsptools`, while `internal/lspproxy` still owns the language-server
supervisors. This is distinct from the secrets-isolated remote
`harness-mcp-proxy`, because a language server needs local
filesystem/workspace access.

**Chain.** `harness → internal LSP manager → N language servers (LSP over
Content-Length stdio)`. LSP config is top-level `lsp` with `enable` plus inline
`servers` and an optional `tools` allowlist; a same-name server entry replaces the
entire embedded default entry, so overrides must include all required fields. The
built-in path uses `lspproxy.NewManager(..., namespace="")` and adapts the manager's
bare tools to short `lsp_*` names. `lsp.tools` (config-file-only, bare names with or
without the `lsp_` prefix) registers only the listed subset of the seven tools; an
empty or unset list registers all, and unknown entries warn and are ignored. These tools are trusted read-only and join the same
automatic exposure gate as other read-only discovered tools. This is independent
of both `mcp.enable` and `mcp.local`: a user can enable LSP and a separate local
stdio MCP service at the same time.

Harness still has a generic local-stdio-MCP capability (`mcp.local`,
`internal/mcpchild` + `setupLocalMCP`): when explicitly enabled with
`mcp.local.enable=true`, it spawns the configured command, connects over the
child's stdio via `mcptools.Conn`'s `Dial` seam, and registers `mcp__`-prefixed
tools using their `readOnlyHint:true` annotations. Because a service can register
tools asynchronously, `setupLocalMCP` polls registration until they appear
(bounded by the 5 s budget). Logs go to stderr (never stdout, the MCP channel)
and drain up the chain into harness's log.

**Advanced — proxy hosting.** `harness lsp serve` is a compatibility stdio MCP
shim over the same `internal/lspproxy` manager. Its default namespace exposes
`mcp__lsp__<tool>` names. `harness-mcp-proxy serve -stdio` (`Daemon.RunStdio`,
enabled by `jsonrpc.NewPeerWithCodec` reusing `mcp.Serve`) can host
`harness lsp serve -namespace ""` and aggregate it with other local services,
doing the `mcp__<server>__` namespacing itself. Pointed at via
`mcp.local.command` with `mcp.local.enable=true`.

**Shim internals.** `internal/lspproxy` is stdlib-only and hand-rolls the LSP
client: a Content-Length JSON-RPC codec (the MCP newline codec rejects header
framing) reusing `jsonrpc.Message`/`Peer`; a per-`(server, workspace-root)`
supervisor (lazy launch, exponential backoff, `StateFailed` cap with
cooldown-revive-on-next-call, graceful `shutdown`+`exit` then SIGTERM/SIGKILL);
extension→language selection with nearest-marker root detection; on-demand
`didOpen`/`didChange` (full-text sync) with mtime tracking; and async
`publishDiagnostics` synchronization. Position tools require file + 1-based line;
optional symbol text is converted to UTF-16 columns by the shim, optional
1-based `column` overrides symbol lookup, and line-only positions use column 0.
`lsp_workspace_symbols` requires `path` to select the target workspace unless exactly
one server is **configured** (the count is of configured servers, not running ones).
Because the embedded defaults ship five servers (Go/Rust/Python/TS-JS/C-C++), `path` is
effectively always required unless the config narrows the set to one. Per-tool optional
params beyond the shared position shape: `lsp_diagnostics` takes `timeout_ms` (default
3000); `lsp_references` takes `include_declaration` (default true) and `max_results`
(default 100); `lsp_workspace_symbols` takes `max_results` (default 100); and
`lsp_rename_plan` additionally **requires** `new_name`. Built-in config uses top-level
`{"lsp":{"servers":{...}}}`; the compatibility `harness lsp serve -config` path
still accepts the legacy `{"version":1,"servers":{...}}` file shape. Both paths
replace embedded defaults (Go/Rust/Python/TS-JS/C-C++) by server name rather than
field-merging them; each tool description advertises the languages whose binary
is on `PATH` via a suffix — ` Langs: <comma-separated list>.` when any are present,
or ` No LSP servers on PATH.` when none are.

**v1 non-goals:** read-only navigation only (no write/edit tools; rename is the
read-only plan); full-text sync (no incremental); one root per language-server
process, with separate processes for other roots; push diagnostics only; the
built-in LSP tool surface is static, and the harness refresh hook is only wired
to external MCP connections.

## 16. Future work

- CLI-subprocess backends (codex / claude) behind a separate process-worker abstraction.
- Explicit workspace isolation or conflict control for read/write delegate agents.
- MCP resources and prompts, the legacy HTTP+SSE downstream transport (the deprecated
  2024 GET-stream MCP transport — distinct from the already-implemented streamable-HTTP
  transport in `internal/mcp`), and OAuth discovery/dynamic client registration for
  remote servers.
- Smarter prompt-cache breakpoint placement: all four breakpoints are now used (§5.4 v2),
  but placement is still static. Splitting the volatile env block (date/git) out of the
  cached system prefix would improve cross-session/agent-switch reuse (within a session the
  system prompt is frozen per process, so it already cache-reads); content-aware anchoring
  could further help compaction-heavy sessions.
