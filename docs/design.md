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
| Interface | line-oriented plain text; basic Markdown on terminal output; optional ANSI color only when stdout is a TTY; `NO_COLOR` and `-no-color` disable color |
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
cmd/harness/main.go      flags, config load, proxy catalog wiring, signal setup, REPL-vs-oneshot dispatch (also hosts `harness lsp serve`, `harness session replay|timings|stats`)
cmd/harness-model-proxy  provider setup/refresh and HTTP model proxy server; subcommands serve (default), setup, refresh-models, auth (login/logout/status), generate-api-key, version
internal/modelproxy      proxy protocol, client Provider, server handler
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
internal/session         append-only conversation tree, mutable state, replay, archives, artifacts
internal/config          flags > env > config-file resolution
internal/modelcatalog    normalized models.dev/OpenAI Codex catalogs for proxy setup/pricing metadata
internal/ui              REPL, streaming renderer, tool summaries, usage line
internal/sysprompt       embedded prompt files + environment context + AGENTS.md sections
internal/agentdef        agent definitions (allowed tools, MCP exposure, prompt/model target) (§14)
internal/hooks           command-only lifecycle hooks (SessionStart/UserPromptSubmit/Pre+PostToolUse/Pre+PostCompact/Stop)
internal/skills          skill discovery + `$skillName` prompt expansion
internal/todo            update_todos store + render (§9.13)
internal/plan            record_plan store + handoff request holder (§9.17, §9.18, §14)
internal/auth            provider auth sources (token_command, oauth2, codex_oauth) for the model proxy
cmd/harness-mcp-proxy  optional MCP proxy daemon + debug client (serve / tools / auth / version)
internal/mcp             tools-only MCP slice: schema, client, server, stdio + streamable-HTTP transports
internal/mcp/jsonrpc     JSON-RPC 2.0 framing and bidirectional request/response correlation
internal/mcpproxy      proxy internals: config, supervisors, tool registry, daemon
internal/metrics         shared Prometheus collectors/exposition plus endpoint config resolution and lifecycle
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

type ParallelToolBatch struct {
    ToolUseIDs []string `json:"tool_use_ids"` // model emission order
}

type Message struct {
    Role                Role                `json:"role"`
    Time                time.Time           `json:"time,omitempty"`
    Phase               string              `json:"phase,omitempty"` // assistant only: commentary | final_answer
    Origin              MessageOrigin       `json:"origin,omitempty"` // prompt | steer | internal | compaction_checkpoint
    Content             []ContentBlock      `json:"content"`
    ParallelToolBatches []ParallelToolBatch `json:"parallel_tool_batches,omitempty"`
}

type BlockKind string

const (
    BlockText       BlockKind = "text"
    BlockImage      BlockKind = "image"
    BlockToolUse    BlockKind = "tool_use"
    BlockToolResult BlockKind = "tool_result"
    BlockThinking         BlockKind = "thinking"
    BlockRedactedThinking BlockKind = "redacted_thinking"
    BlockReasoning        BlockKind = "reasoning"
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
    ImageName         string `json:"image_name,omitempty"`
    ImageWidth        int    `json:"image_width,omitempty"`
    ImageHeight       int    `json:"image_height,omitempty"`
    ImageBytes        int    `json:"image_bytes,omitempty"`
    ImageEncodedBytes int    `json:"image_encoded_bytes,omitempty"`

    // BlockToolUse (assistant calls a tool)
    ToolUseID string          `json:"tool_use_id,omitempty"` // provider-issued call id
    ToolName  string          `json:"tool_name,omitempty"`
    ToolInput json.RawMessage `json:"tool_input,omitempty"`  // complete JSON object

    // BlockToolResult (we answer a tool call)
    ResultForID string `json:"result_for_id,omitempty"` // matches a ToolUseID
    ResultText    string         `json:"result_text,omitempty"`
    ResultError   bool           `json:"result_error,omitempty"`
    ResultContent []ContentBlock `json:"result_content,omitempty"` // shallow image children only

    // Anthropic thinking and redacted-thinking replay
    Thinking          string `json:"thinking,omitempty"`
    ThinkingSignature string `json:"thinking_signature,omitempty"`
    RedactedData      string `json:"redacted_data,omitempty"`

    // Responses encrypted reasoning replay
    ReasoningID        string `json:"reasoning_id,omitempty"`
    ReasoningEncrypted string `json:"reasoning_encrypted,omitempty"`
}
```

Design notes:

- **System prompt lives on `Request.System`,** not in the message list. This is the
  natural Anthropic shape, trivially becomes a leading `role:"system"` message for
  OpenAI, and means compaction can never accidentally summarize it away.
- **`ToolInput` is `json.RawMessage`,** not `map[string]any`: it arrives as a byte stream,
  the tool layer decodes it into its own typed struct anyway, and raw bytes round-trip
  through session files without re-encoding surprises.
- **JSON tags are provider-neutral** (`kind`, `tool_use_id`, …). The sole opaque
  provider step is a hidden Gemini Interactions Google Search call/result needed
  for stateless signature replay; incompatible dialects ignore it.
- **`Origin` is transcript-only provenance.** It preserves prompt, steering,
  internal, and compaction-checkpoint boundaries; provider adapters ignore it.
- **Reasoning blocks are opaque replay state.** Anthropic thinking/signatures,
  Responses encrypted reasoning items, and Gemini Interactions thought
  summaries/signatures use distinct kinds, are replayed only to compatible
  requests, and are never treated as ordinary user-visible text.
- **`ParallelToolBatches` is local execution metadata.** It appears on the user message
  carrying tool results and is ignored by provider request builders. Each entry names,
  in emission order, every tool-use ID in one group selected for concurrent dispatch.
  Only groups of 2+ calls are recorded; no field means sequential execution or a
  transcript written by an older harness version that did not record the distinction.
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
    Truncated     bool   // central cap (§8.3) trimmed the result
    OriginalText  string // full pre-truncation text, archived to artifacts/
    OriginalBytes int    // size before truncation
    ShownBytes    int    // size after truncation
    Usage         Usage          // metered tools (e.g. delegate) report child token usage
    Content       []ContentBlock // optional shallow image children
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
| tool_use | assistant `tool_calls[].function` with JSON-string arguments | `function_call` item with string arguments | `tool_use` content with object input |
| tool_result | sibling `role:"tool"` message; rich images follow in one neighboring user message | `function_call_output` item; rich images follow in one neighboring user message | `tool_result` content inside a user message, including nested image children |
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

type InputTokenCounter interface {
    CountInputTokens(ctx context.Context, req Request) (InputTokenCount, error)
}

type Request struct {
    Model       string
    Purpose     RequestPurpose // turn|compaction|prewarm|handoff_summary|branch_summary
    System      string
    Messages    []Message
    Tools       []ToolSchema
    ServerTools []ServerTool
    MaxTokens   int      // 0 = automatic policy (see §5.4)
    Temperature *float64 // nil = omit
    Reasoning   ReasoningConfig
    StopSeqs    []string
    ServiceTier string // model-advertised request tier; empty = provider default
    Speed       string // resolved provider speed control (Anthropic fast mode)
    Betas       []string // bounded provider beta identifiers resolved by the proxy
    EstimatedInputTokens int // caller estimate; 0 asks the dialect to estimate
    ContextWindowHint    int // effective override or provider-learned window
    StoreResponse      bool
    PreviousResponseID string
    RequestContext     []string // request-only hook/todo/background context
    ProxySessionID     string   // harness-local sticky-routing/transport-affinity key
    CacheAffinityID    string   // harness-local conversation key for stable prompt-cache routing
    PromptCacheKey     string   // provider-facing cache-affinity key; proxy derives from CacheAffinityID
    CachePolicy        CachePolicy
}

type CacheTTL string

const (
    CacheTTLDefault  CacheTTL = "5m"
    CacheTTLExtended CacheTTL = "1h"
)

type CachePolicy struct {
    StaticTTL           CacheTTL // stable system/tool breakpoint TTL
    StableMessagePrefix int      // leading Request.Messages safe from future retention rewrites
}

type ResponseState struct {
    PreviousResponseID string
    AnchorMessages     int
    AnchorDigest       string // lowercase SHA-256 fingerprint of the represented prefix
}

type ReasoningConfig struct {
    Profile      string // portable proxy profile; empty/default = provider default
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

`RequestContext` is request-only instruction context: Chat Completions appends it
as a trailing system message after the transcript, Anthropic appends it as a
trailing user-role message (single uncached text block) placed after the
cache breakpoints are laid so the rolling tail breakpoint stays on the last
real transcript message, and Responses adds it as a late `role:"developer"`
input item immediately before the current user message or trailing tool-call/output
group. All three keep the stable system+tools+transcript prefix intact for prefix
caching. Fresh todo/background/hook context applies to the current request without
looking like the latest user prompt or becoming part of the persisted transcript.
Responses streams surface `response.id` on terminal `EventDone.ResponseID`; the
agent stores that with the local transcript anchor for optional
`previous_response_id` continuation. `EventDone.ResponseIDAnchor` is an optional
`*int` used only by out-of-band prewarm: nil means the response ID must not be
installed, while Responses WebSocket `generate:false` reports an explicit zero
anchor.

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
    Signature    string // Anthropic signed-thinking replay
    RedactedData string // Anthropic opaque redacted thinking
    ReasoningID        string // Responses reasoning item id
    ReasoningEncrypted string // Responses encrypted reasoning replay

    // EventToolCall*; Index disambiguates parallel calls within one turn.
    Index     int
    ToolID    string          // Start/Done
    ToolName  string          // Start/Done
    ArgsDelta string          // Delta
    ToolInput json.RawMessage // Done only: complete, valid JSON
    InvalidInputError string  // Done only: malformed streamed args were replaced with a diagnostic object

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
    Anthropic's additive versioning policy.
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
  `partial_json` fragments (`Delta`); `content_block_stop` flushes that call (`Done`).
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
| Tool schemas | `tools[] = {type:"function", name, description, parameters, strict:false}` | `tools[].function = {name, description, parameters}` (`type:"function"`) | `tools[] = {name, description, input_schema}` | `tools[] = {type:"function", name, description, parameters}` |
| Server tools | `web_search`, or OpenRouter `openrouter:web_search` | OpenRouter `openrouter:web_search`; MiMo `web_search`; Kimi `builtin_function.$web_search`; Z.AI nested `web_search` options | `web_search_20250305` named `web_search` | `google_search` |
| Parallel tool hint | `parallel_tool_calls:true` when tools are present | `parallel_tool_calls:true` when tools are present | not sent | not sent |
| Prompt cache key | provider-configured; OpenAI auto emits `prompt_cache_key`; managed `openai-codex` configs select it explicitly | provider-configured; OpenAI auto emits `prompt_cache_key`, OpenRouter auto emits `session_id` | not sent (explicit `cache_control` breakpoints instead) | not sent; stored continuation and Gemini implicit caching handle stable prefixes |
| Stateful continuation | CLI-owned `store:true` + `previous_response_id`, fingerprint-validated suffix trimming, and one full-history fallback; `responses_stateful:false` sends `store:false` | ignored | ignored | CLI-owned `store:true` + `previous_interaction_id`, fingerprint-validated suffix trimming, and signed full-history fallback; `interactions_stateful:false` sends `store:false` |
| Assistant phase | assistant input items include stored `phase` when present | ignored | ignored | ignored |
| Response format | provider default unless explicitly requested by a caller | provider default | provider default | forced to plain text; generated media is rejected |
| Token cap | input-aware `max_output_tokens` | input-aware `max_completion_tokens` for `api.openai.com`; `max_tokens` for compatible/custom endpoints | required input-aware `max_tokens` | input-aware `generation_config.max_output_tokens` |
| Input token count | `POST /responses/input_tokens`; `codex_oauth` uses a local `o200k_base` estimate | local `o200k_base` estimate | `POST /v1/messages/count_tokens` | provider-neutral local estimate |
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
| Operations | streaming `POST /responses`; `POST /responses/input_tokens` | retrieve/delete/cancel/compact, conversations, background work |
| Request controls | model/input/instructions, functions and hosted web search, output cap, temperature, reasoning summaries/encrypted replay, prompt cache key, service tier, stored continuation | arbitrary output formats, moderation, metadata, tool choice, sampling/logprob controls, safety/user identifiers |
| Content/items | text and image input; message text/refusal, reasoning summary, function calls/results, hosted web-search status | file/audio input; generated media, computer/code/shell/patch/MCP/custom tools |
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
| Content/tools | text and base64 images; client function calls/results; signed and redacted thinking; `web_search_20250305` | documents/search-result input, container uploads, code/bash/text-editor execution, web fetch, memory, computer use, tool search, newer web-search variants |
| Stream/usage | text/tool/thinking/citation deltas; hosted web-search call/results for continuation; disjoint input/cache/output/reasoning usage; served tier/speed | server-tool request counts and per-query fees, inference geography, stop details |

Required union fields are emitted even when empty (`text`, `input`, and
`input_schema`). Hosted web-search blocks remain provider-owned: they are
recognized and retained for same-turn continuation but are not added to the
provider-neutral transcript.

Anthropic `pause_turn` is continued inside the dialect. The complete assistant
content is replayed in original block-index order, rolling cache breakpoints are
refreshed, and usage snapshots remain cumulative across the resulting HTTP
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
stored Responses objects.
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

Compaction, handoff-summary, and branch-summary requests derive deterministic
purpose-separated cache and proxy IDs from the owning session identity with
SHA-256 and fixed domain strings. This keeps maintenance traffic on a consistent
cache shard without letting it reuse or compete with the main conversation's
WebSocket/continuation chain. Prewarm intentionally retains the main session
identities.

For ChatGPT Codex over Responses WebSocket, prewarm sends an empty
`response.create` with `generate:false`. Only this path marks its terminal event
with an explicit response-ID anchor at transcript index zero. The background
closure captures the provider, response-state epoch, proxy session, and
transcript snapshot; its result returns through the REPL maintenance queue and
is installed on the owner goroutine only if the epoch, session, transcript
length/fingerprint, and empty-current-anchor checks still hold. A real turn
therefore wins the race. HTTP prewarm leaves the anchor nil and contributes
usage only. The WebSocket client continuously drains frames and answers ping
heartbeats between model requests; a close frame or transport EOF makes its last
response ID unavailable.

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

Shift-Tab agent switches defer prewarm behind a 500ms idle debounce;
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

**Anthropic prompt caching:** the CLI declares semantic policy; the dialect
chooses wire placement within Anthropic's four-breakpoint budget. The last tool
schema and stable system block use `CachePolicy.StaticTTL`: interactive turns
request `1h`, while one-shot, delegate, prewarm, compaction, handoff-summary, and
branch-summary requests use `5m`. Message breakpoints always retain the
provider-default five-minute TTL.

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
declared stable-prefix breakpoint. OpenAI caching remains automatic, so its
dialects ignore this breakpoint policy.

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
thing across dialects.

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

`internal/llm/registry.go` holds a small registry. The structs carry JSON tags so they
double as the proxy catalog's on-disk schema (`Price`, `ModelInfo`, `ProviderConfig`,
`ModelEntry`), and `Cost`/`ContextWindow`/`Models` are methods on `*Registry`:

```go
type Price struct {
    Input, Output, CacheRead, CacheWrite, CacheWrite1h float64 // USD per 1M tokens
    Reasoning, InputAudio, OutputAudio   float64
    Tiers []PriceTier // context-length rate steps with the same price dimensions
}
type PriceTier struct {
    Threshold int
    Input, Output, CacheRead, CacheWrite, CacheWrite1h float64
    Reasoning, InputAudio, OutputAudio   float64
}
type ModelInfo struct {
    ContextWindow   int
    OutputLimit     int
    InputModalities []string
    ServerTools     []string
    ServiceTiers   []ServiceTier
    Price           Price
    Shape           string
    Reasoning       *ReasoningInfo
}
func (r *Registry) Cost(model string, u Usage) (usd float64, known bool)
func (r *Registry) ContextWindow(model string) int // registry hit, else default 256_000
func (r *Registry) Models() []string               // sorted configured model ids
```

Model metadata normally originates from the public **models.dev** catalog. The
models.dev and OpenAI Codex adapters, normalized catalog types, and vendored
fallback snapshots live in `internal/modelcatalog`; `harness-model-proxy setup`
and `refresh-models` consume the normalized provider/context/input-modality/
reasoning/service-tier fields. models.dev `experimental.modes` entries are
projected into bounded service-tier request mappings and tier-specific prices.
The proxy caches a projected catalog as `models.dev.api.json`
in the proxy config directory, retaining every provider and model but only the
metadata fields harness consumes.
`setup` prefers that cache over the vendored snapshot, but fetches and writes it
when it is missing or invalid. A running proxy refreshes the cache when it is older
than `models_dev_cache_ttl` (`24h` by default; `0` disables periodic refresh), and
`refresh-models` fetches and caches that projected catalog before rewriting
configured provider allowlists. The vendored snapshot is used only when there is
no parseable cache and a live fetch fails.

The synthetic `openai-codex` provider is the exception: its model list comes from
the OpenAI Codex model catalog (`codex-rs/models-manager/models.json`). Setup uses
a vendored copy or the last cached refresh; `refresh-models` fetches the latest
catalog from `openai/codex` on GitHub and caches only the fields consumed by the
adapter as `openai-codex.models.json`. Only list-visible Codex models are exposed.

### Managed vs manual provider configs

Provider config files are either **managed** or **manual**:

- **Managed** configs are written by `setup`/`refresh-models` and carry
  `"managed": true`. They store **no per-model `price`**; instead the proxy
  resolves each managed model's price and input modalities from the in-memory
  models.dev cache at request time. Because the background refresher (above)
  reloads that cache and the serving handler swaps in the new metadata live,
  refreshed prices and modality support reach the running server **without** a
  `setup` + restart. If a refreshed cache no longer lists a managed provider or
  model, the handler warns and removes that stale target from the live catalog;
  manual configs are not pruned by cache refreshes. Re-running `setup` never
  clobbers hand-edited prices because managed configs hold none.
- **Manual** configs are any provider file lacking `"managed": true` — typically
  hand-written. The proxy never touches them and serves their own `price` and
  `input_modalities` entries verbatim. A pre-existing price-bearing config
  without the flag is treated as manual and keeps its metadata (there is no
  migration). Running `refresh-models` against such a provider rewrites it as
  a managed, price-less config.

A managed config may also carry `"price_source"` — a models.dev provider id to
resolve its prices from when that differs from the config's own `name`. The
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
catalog fields, normalizing the user-facing `fast` suffix to the OpenAI wire
value `priority`. No provider-wide tier inference is applied. For every
non-default mode, the served catalog expands the base model into a separate
target such as `provider:model:fast`, carrying `base_target_id`, `variant`, and
its own price. Target resolution—not a caller-supplied request field—selects the
bounded provider mapping. Anthropic encodes `speed` and merges required feature
identifiers into `anthropic-beta`; OpenAI dialects encode `service_tier`.
`/fast` switches between sibling targets while retaining the base model's proxy
continuation/cache identity.

The base `llm.Price` schedule represents standard processing; each service tier
may carry its own schedule. Request pricing prefers provider-reported served
`service_tier`/`speed` metadata, which accounts for graceful downgrades, then
falls back to the requested mapping. A mode without an accurate price is
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
these wire contracts rather than being generic. The numeric schedules still
come from models.dev (or a manual provider config); the filtered models.dev
snapshot already preserves explicit `cost.reasoning` values when raw catalog
entries provide them.

Google Search per-query charges are not part of `llm.Price` and are not added to
`CostUSD`. Although the Interactions usage schema exposes
`grounding_tool_count`, the public pricing source is a human-facing,
model-dependent schedule rather than a stable model-ID keyed catalog. Harness
therefore reports and budgets the token portion only instead of presenting a
query fee as exact. Anthropic hosted web search follows the same policy: its
reported token usage is priced, but the per-search fee is excluded.

The serving handler holds its registry, pricer, and served catalog behind an
atomic snapshot. The initial snapshot is built at startup from the loaded
provider configs plus the cached models.dev catalog; after each successful cache
refresh the refresher rebuilds the snapshot (managed pricing schedules and
modalities from the new catalog, with managed entries absent from that catalog
pruned and manual metadata unchanged) and atomically swaps it in, so `/v1/models`
responses and
per-request `cost_usd` accounting always reflect the freshest managed metadata.
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
output limits, and reasoning support are loaded from the model proxy catalog.
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

Precedence: **flags > environment > config file > built-in defaults** — for settings
that *have* a flag. A few knobs have no flag and resolve **env > config file > default**:
MCP/LSP enable, `mcp.proxy`, `mcp.local.enable`, the global tool-result caps, and
the per-tool `rg`/`grep`/`read_file` caps. Others
(agent definitions, compaction knobs, `agents_md_warn_bytes`,
`delegate_max_turns`, `delegate_max_depth`) are config-file-only (listed below).
`delegate_output` has the normal flag/env/file/default precedence and accepts
`status`, `off`, and `lines`. `status` enables only the compact TTY row,
`off` constructs no delegate display registry/feed, and `lines` adds curated
scrolling child activity on stderr (including on non-TTY output). Quiet
constructs neither registry nor feed.

- Environment: `HARNESS_MODEL_PROXY_URL`, `HARNESS_MODEL`, plus
  most `HARNESS_*` equivalents for user-facing flags. `trace_proxy` /
  `HARNESS_TRACE_PROXY` / `-trace-proxy` opt in to W3C Trace Context headers for
  harness-to-proxy HTTP requests. `--log-level` uses
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
- Config file (optional): `~/.config/harness/config.json` — model,
  model_proxy_url, agent definitions, hooks, flag defaults, and
  context-efficiency knobs.
  `agents_md_warn_bytes` (applied to each AGENTS.md file independently),
  `compact_keep_turns`, `compact_keep_tokens`, `compact_auto_enabled`,
  `compact_trigger_percent`, `compact_target_percent`,
  `compact_summary_max_tokens`, and `compact_tool_result_max_bytes` are
  config-only.
  Tool-result truncation uses config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. `rg`/`grep` result caps use
  `rg_result_max_bytes`, `rg_result_max_lines`, `grep_result_max_bytes`, and
  `grep_result_max_lines`, or matching `HARNESS_*` env vars. `read_file` uses
  `read_file_default_limit`, `read_file_result_max_bytes`, and
  `read_file_result_max_lines`, or matching `HARNESS_*` env vars.
  `run_command_timeout_seconds` and `run_command_background_timeout_seconds`
  (with `HARNESS_RUN_COMMAND_TIMEOUT_SECONDS` and
  `HARNESS_RUN_COMMAND_BACKGROUND_TIMEOUT_SECONDS` env vars) override the
  built-in defaults of 120 s and 1200 s respectively; set to 0 to use the
  built-in values.
  `delegate_max_turns` (default `20`) and `delegate_max_depth` (default `3`,
  root depth `0`) are config-only for the delegate tool. Both must be positive.
  `delegate_output` defaults to `status`; its environment and CLI forms are
  `HARNESS_DELEGATE_OUTPUT` and `-delegate-output`.
- Hooks use inline `hooks` plus config-relative `hook_configs` files. They are
  additive in order: inline first, then each listed file. `--hooks <file>`
  replaces the configured hook set for one launch.
- `harness-model-proxy setup` creates a proxy config in the default proxy directory,
  appends a new provider config to an existing proxy config, or updates an existing
  configured provider. It reads cached models.dev provider metadata, fetching and
  caching all providers and models with unused upstream fields pruned when needed,
  falls back to a vendored models.dev
  snapshot only when no parseable cache is available and live fetch fails, lists
  harness-supported providers, marks existing providers with bold text and `*`,
  derives missing first-party API URLs from exact `@ai-sdk/openai`,
  `@ai-sdk/anthropic`, and plain `@ai-sdk/google` package metadata, prompts for
  the API key when the provider needs one, pages the selected provider's
  models newest-first, and asks which models should be locally available. The
  synthetic `openai-codex` provider is listed when OpenAI provider metadata is
  available, with its models from the vendored or cached Codex model catalog; it
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
  models.dev catalog and refreshes each configured provider file's current model
  allowlist, preserving stored API keys and `auth` blocks. When `openai-codex` is
  configured, it also refreshes the cached OpenAI Codex catalog from GitHub and
  rewrites Codex models from that source. Sakana configs refresh from the
  models.dev cache like any other managed provider.
  Refreshed files are rewritten as managed, price-less configs. If live fetch fails, it
  uses a parseable local cache before using the vendored fallback snapshot. When a
  configured model is missing from the selected catalog it prints a warning and
  drops that model; when a configured provider is missing, is no longer supported
  by harness, or has no models left after refresh it prints a warning and removes
  the provider (deleting its now-empty provider file and dropping the stale
  `provider_configs` reference), so a provider that no longer exists never fails
  the whole refresh. A referenced provider file that has gone missing is likewise
  warned about and dropped from `provider_configs`. Unreadable or malformed files
  still error.
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
  64 connections, 10-minute idle TTL, 50-minute absolute age, and a 30-second
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
  Every unsuccessful upstream attempt is additionally logged at WARN when it
  occurs, even when a later retry succeeds. These records include the proxy and
  upstream request ids, upstream attempt, status/code, parsed provider message,
  retryability, retry-after/delay, attempt duration, and request elapsed time.
  Model-request events and API error diagnostics carry the same instance ID;
  `(proxy_instance_id, proxy_request_id)` is the cross-replica correlation key.
  Retry scheduling is logged separately at INFO; cancellation is distinguished
  from an upstream failure.
- `POST /v1/input_tokens` accepts `{provider, request}` and returns
  `{input_tokens, source}` when the configured provider implements
  `InputTokenCounter`. `codex_oauth` Responses targets return a local
  `source:"o200k_base"` estimate instead of forwarding to a non-Codex
  `/responses/input_tokens` endpoint. Unsupported providers return `501` with
  `code:"input_token_count_unsupported"`. Count requests are best-effort
  preflight diagnostics and are not added to usage or cost aggregation.
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
  `model_proxy_cache_read_tokens_total`, `model_proxy_cache_write_tokens_total`,
  `model_proxy_cache_write_1h_tokens_total`,
  `model_proxy_reasoning_tokens_total`, `model_proxy_cost_usd_total`, and
  `model_proxy_request_duration_seconds_total` — are labeled by `provider`,
  `model`, bounded request `purpose`, and `key`, while the
  `model_proxy_build_info` gauge is labeled by `version` only.
  `purpose` is `turn`, `compaction`, `prewarm`, `handoff_summary`,
  `branch_summary`, or `unknown`;
  missing and unrecognized client values normalize to `unknown` to prevent label
  cardinality growth. The `key` label is the authorizing API key's stored `Name`
  (stashed in the request context by the auth middleware) or the sentinel
  `"anonymous"` when auth is disabled. Token counters
  are recorded for every `/v1/stream` that produced usage, priced or not — a
  deliberate superset of `/v1/usage`'s priced-only cost rollup — while
  `cost_usd_total` is recorded only when the model's price is known.
  `requests_total`/`errors_total` cover every `/v1/stream` attempt, including ones
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
  `pricing` object — `{source_date, max_age_seconds}` — and `max_age_seconds` is the
  configured models.dev refresh interval. `source_date` dates the served prices:
  when any provider is managed and a models.dev cache is loaded, it is the cache's
  source date (the cache file's mtime, kept fresh by the background refresher);
  for a manual-only catalog it is the newest modification time among the
  configured provider config files (the date those prices were last written). A
  client can compare them to detect stale prices. Static context-tier schedules
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
response. An **attempt** is one provider request for a turn; retries do not create
additional turns. Turn numbers restart at 1 for every prompt. Model-backed
maintenance such as compaction and prewarming is neither a prompt nor a turn.

```
append user prompt message (origin=prompt)
for turn := 1; maxTurns <= 0 || turn <= maxTurns; turn++ { // default 250; <=0 unlimited
    stream := provider.Stream(ctx, request) // attempt 1; retryable failures increment attempt
    accumulate: print text deltas live; collect assembled tool calls;
                capture usage + stop reason
    append assistant message (text blocks + tool_use blocks, emission order)
    if stopReason == tool_use {
        for each tool call, in order:              // read-only islands may run concurrently
            result := registry.Dispatch(ctx, call) // always returns a result
            print one-line tool summary
        append ONE user message carrying all tool_result blocks, in call order
    }
    emit turn_complete(prompt, turn)
    if stopReason != tool_use { break }
}
run post-prompt maintenance if needed
emit prompt_usage(prompt, completedTurns)
```

- **Mostly-sequential tool execution.** Coding tools mutate a shared filesystem; deterministic
  ordering matching the model's emission order is worth far more than parallelism. Consecutive
  read-only islands with 2+ calls dispatch concurrently, bounded at 8, unless
  `PreToolUse` or `PostToolUse` hooks are configured; mutating calls remain ordering
  barriers. Results, sink events, and transcript blocks stay in emission order. The
  tool-result message records each actually selected concurrent island as one
  `parallel_tool_batches` entry containing the complete ordered tool-use ID list; this
  distinguishes separate islands in the same model-emitted call batch.
- **One result per call, always.** Required by both APIs (§4 invariant). `Dispatch`
  produces a result even on panic.
- **Metered tools:** tools may optionally report token usage (currently synchronous
  `delegate`). The agent adds that usage to the prompt/session total, while the normal
  tool result remains the only child output added to the parent transcript.
- **File diffs:** unless `show_diffs` is disabled, the agent asks built-in file
  mutation tools for their affected paths, snapshots those files immediately
  before and after each sequential tool call, and emits a user-facing unified diff
  event after the normal tool summary. The diff is generated by a stdlib-only line
  renderer, not by repository `git diff`, so it works in non-git projects and shows
  incremental per-call changes when the same file is edited repeatedly. Displayed
  diffs are colored by `internal/term/highlight` with subdued red/green line
  backgrounds, colored `+`/`-` sigils, and content syntax-highlighted in the
  mutated file's language (plain content for unknown languages). The tint
  spans the full terminal row via erase-to-EOL under the active background
  (BCE) rather than padding spaces, so window-shrink reflow has nothing to
  wrap and copy-paste keeps the original bytes; terminals without BCE show a
  text-width tint.
- **Background jobs:** tools with `background:true` start process-local jobs and
  return a job id immediately. `delegate` uses the same flag for background child
  agents. Local jobs carry canonical resource/access leases: read-only leases may
  coexist, while exclusive access conflicts with every unfinished lease on the
  same resource. Completed job summaries are delivered once as request-only
  context on a later parent model request; they are not appended to the parent
  transcript.
- **Max-turns guard:** when `max_turns` is positive, on hit print
  `[stopped: reached max turns (250)]`, keep the transcript (it is valid — the
  last turn's results are appended), and return to the prompt. A non-positive
  `max_turns` disables this guard. One turn before the limit the loop injects a
  one-shot RoleUser wrap-up steer
  ("stop calling tools now and reply with a final message").
- **Structured termination:** every `PromptUsage` carries exactly one loop
  termination reason: `model_completed`, `turn_limit`, `token_limit`,
  `cost_limit`, `repeat_guard`, `error_guard`, `cancelled`, or `error`.
  Prompt replay events and delegate metadata persist it. The reason records
  loop control only; acceptance criteria and repository state remain the task
  completion oracle.
- **Caller-defined turn milestones:** `agent.Options.TurnMilestones` contains
  concrete closed-tool-turn boundaries and internal steering text. The agent
  normalizes them by turn, coalesces messages due on the same boundary, and
  injects each exactly once before the next model request. Core agent code owns
  deterministic delivery but no delegate-specific percentage policy.
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
- **Non-normal model stops:** `max_tokens` and stop-sequence finishes end the prompt
  but emit a visible notice, so a truncated or externally stopped assistant answer
  does not look like an ordinary completion.
- **Mid-stream retries:** each turn is wrapped in `streamWithRetry`, which
  re-requests the step from scratch on a retryable terminal stream error up to
  `streamRetries` (2) times. These attempts do **not** count against `max-turns`;
  failed-attempt usage is still billed and tracked (xref §5.5).
- **Stateful-Responses fallback:** when a turn that reused a `previous_response_id`
  fails because that response is no longer available (and nothing streamed), the agent
  resets the stored Responses state, notes it, and retries once with the full context.
- **Stop hook:** a configured `Stop` hook fires when the model would end the prompt;
  it may block the break and force one more turn. `stop_hook_active` guards it so it
  fires at most once per prompt (`agent.go`).
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
apply their wire-specific representation (§4.1).

| Failure | Internal result text |
|---|---|
| unknown tool name | `unknown tool "<name>"` |
| JSON type mismatch | `invalid arguments: invalid value for "<field>": expected <JSON type>; got <JSON type>` |
| invalid JSON syntax | `invalid arguments: invalid JSON at byte <offset>: <detail>` |
| malformed streamed tool-call args | `invalid tool call arguments for <name>: <detail>` |
| tool returned error | `<message>` |
| tool panicked | `tool panicked: <recovered>` (also logged to stderr) |
| tool exceeded the dispatch timeout | `tool timed out after <dur>` |

`json.UnmarshalTypeError` and `json.SyntaxError` are recognized centrally, including
when wrapped. Type errors are translated into JSON terminology (for example,
`"args"` expected an array of strings but got a string) rather than exposing Go
struct/type details. Tool-specific semantic validation continues to use concise
`badArgs` messages under the same `invalid arguments: ` prefix.

**Per-tool dispatch timeout backstop (`-tool-timeout`, default 600s, `<=0`
disables).** `Dispatch` runs each tool under a derived `context.WithTimeout` so a
hung tool that ignores cancellation cannot stall a turn; on expiry it returns the
`tool timed out after <dur>` error result above. It applies to both the
sequential path and the concurrent read-only batch. A tool that reports its own
deadline via `SelfTimeouter` only **raises** the ceiling, never lowers it, so
`run_command`'s `timeout_seconds` stays authoritative. An outer cancellation
(`^C`) is reported as cancellation, not a dispatch timeout.

### 8.3 Output truncation

A central cap in `Dispatch` (backstop for every tool): **64 KB or 1000 lines per
result** by default, configurable with `tool_result_max_bytes` and
`tool_result_max_lines`, or env `HARNESS_TOOL_RESULT_MAX_BYTES` and
`HARNESS_TOOL_RESULT_MAX_LINES`. Noisy file-inspection tools install smaller
defaults when no global cap is configured: `rg`/`grep` use 32 KB or 500 lines,
and `read_file` uses a 500-line default window plus a 32 KB result cap. Per-tool
caps are configurable with `rg_result_max_bytes` / `rg_result_max_lines`,
`grep_result_max_bytes` / `grep_result_max_lines`, and
`read_file_result_max_bytes` / `read_file_result_max_lines`. The first cap hit
adds a teaching marker:

```
[truncated: showing first 1000 of 4213 lines; use read_file offset/limit or grep to narrow]
```

Individual tools may also apply their own natural limits, but the central cap is the
backstop for every result. Truncated results carry metadata so the UI can warn and write
the full output to the session's `artifacts/tool-results/` directory. When an artifact
is written, the model-visible result includes the absolute artifact path and advises
using `read_file` with `offset`/`limit` or `rg` for targeted inspection. Foreground tool
results and completed background-job context share the registry's result preparation
and the same archive-hint formatter, including per-tool limits, so this recovery
behavior stays consistent between execution modes.

A tool that implements `ResultTool` may supply separate concise `Text` and full
`OriginalText`. `Dispatch` caps the concise text normally and marks the supplied
original for this same artifact pipeline. This is used by `run_command.steps`:
successful verification output remains recoverable without entering live model
context.

### 8.4 Interrupts

A single SIGINT handler plus a per-prompt `context.CancelFunc`:

- **^C during a prompt** → cancel the prompt context (aborts the HTTP stream; kills
  `run_command` process groups). Apply the cancel repair rule (§4): keep streamed
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
  editing, prefer `edit` with unique context, use tools rather than guessing file
  contents, use available search tools or `list_dir`, run builds/tests via
  `run_command`, stop when done.
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
  The skills catalog includes its activation/read instruction once and caps each
  description at 160 runes. Process-specific LSP/Serena hints follow it. The
  active agent prompt is always the final section.
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

type RichTool interface {
    RunRich(ctx context.Context, input json.RawMessage) (RichResult, error)
}

type RichResult struct {
    Text    string
    Content []llm.ContentBlock
    Usage   llm.Usage
}

type RequiredInputModality interface {
    RequiredInputModality() string
}

type MeteredTool interface {
    RunMetered(ctx context.Context, input json.RawMessage) (MeteredResult, error)
}

type MeteredResult struct {
    Text  string
    Usage llm.Usage
}

type ResultTool interface {
    RunResult(ctx context.Context, input json.RawMessage) (RunResult, error)
}

type RunResult struct {
    Text         string
    OriginalText string
    Usage        llm.Usage
}

type Registry struct{ /* ordered map */ }
func (r *Registry) Register(t Tool)
func (r *Registry) Specs() []llm.ToolSchema
func (r *Registry) Dispatch(ctx context.Context, call llm.ToolCall) llm.ToolResult
```

- **Schemas are hand-written JSON Schema constants.** `Tool.Schema` remains the
  full implementation contract. `Registry.Specs` removes annotation keywords
  (`description`, `title`, `$comment`, `example`, `examples`) from actual schema
  nodes before sending them to the model, while preserving validation keywords
  and property names that happen to equal an annotation keyword. `delegate`
  retains `description` because its dynamic compatible-agent catalog is essential.
  Enums and required-ness still deserve hand tuning, and reflection fights you on
  exactly those fields.
- **Tools self-validate args** after `json.Unmarshal` into a private struct (no stdlib
  JSON Schema validator; unknown extra keys are tolerated — models hallucinate them).
- **Optional execution seams have one strict preference order:** rich, result,
  metered, then legacy `Run`. Dispatch invokes exactly one path. Rich children are
  validated before they enter a transcript; errors and centrally rejected results
  have content cleared. `RequiredInputModality` is a separate static capability signal,
  so a tool can require image input regardless of whether all successful calls are rich.
- **Read-only classification is per call.** Static read-only tools ignore the input;
  argv-style tools can parse their arguments and return true only for safe subcommands
  (for example `git status`, `git diff`, `git log`).
- Relative paths resolve against the process cwd. No path restrictions — the harness is
  honest about its no-sandbox assumption.

### 9.1 `read_file`

> Read one file with optional offset/limit, or batch paths[]; returns line-numbered content.

| param | type | notes |
|---|---|---|
| `path` | string | single file; required unless `paths` is given |
| `paths` | array of strings | multi-file mode; each file rendered under a `==> path <==` header with its own per-file line budget; `offset` is ignored |
| `offset` | int | 1-based starting line (single-file mode only) |
| `limit` | int | max lines, default 500 or `read_file_default_limit` |

- **Parameter aliases (accepted silently; intentionally *not* in the schema):**
  `path` also accepts `file`, `file_path`, `filePath`, `filename`, `filepath`,
  `absolute_path`, and `target_file`; `paths` also accepts `files`. These match the
  names other harnesses give the parameter (Claude Code and Gemini CLI use
  `file_path`, opencode `filePath`, Cursor `target_file`), so a model that emits the
  other spelling still succeeds on the first call instead of wasting a round trip.
  They are left out of the advertised schema to keep the model-facing surface
  minimal and avoid nudging models off `path`; the canonical `path`/`paths` win
  when both a canonical name and an alias are set.
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

### 9.1a `view_image`

> Attach a local image file to the next model request for visual inspection.

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

### 9.2 `list_dir`

> List one directory with an optional base-name glob; non-recursive.

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

> Recursively list sorted files and directories matching pattern under optional root; ** crosses directories.

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
- Available to the default `auto`/`independent` agents and the shared
  inspection set used by `explore` and `plan`.

### 9.3 `grep`, optional `rg`, and `search_context`

> `grep`: Run grep without a shell. Input is an object; args must be an array of strings, not a string. Skips binary files unless overridden; background returns a job id. (Under `-search-tools both`, the description also says to prefer `rg`.)

> `rg`: Run raw `rg` for broad repository discovery, combined patterns, filenames, counts, native flags, or background searches. Once a target is known, use `search_context` for surrounding source.

> `search_context`: Targeted code lookup after broad discovery: return bounded, merged line-numbered source around a known symbol, call site, or text match. Use this instead of `rg` followed by `read_file`; use one raw `rg` with a combined pattern for broad multi-concept orientation.

| param | type | notes |
|---|---|---|
| `args` | array of strings, required | arguments passed after the program name; must be a JSON array, not a string or JSON-encoded array |
| `stdin` | string | written to the program's standard input |
| `cwd` | string | default process cwd |
| `timeout_seconds` | int | default 120, no maximum |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |

- Background `grep` and `rg` jobs automatically acquire a `read_only` lease on
  their canonical cwd.
- Search exposure is configurable with `search_tools` / `HARNESS_SEARCH_TOOLS` /
  `-search-tools`: `auto` (default), `grep`, `rg`, or `both`.
- Provider-hosted web search is not a local tool in this registry. It is exposed
  through `Request.ServerTools` when `web_search` is `auto` and the selected
  model-proxy target advertises `server_tools:["web_search"]`.
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
- Whenever the selected mode exposes `rg`, it also exposes `search_context`.
  The structured input requires `pattern`; `path` defaults to `.`, optional
  `globs` and `fixed_strings` retain ripgrep matching semantics, and bounded
  `context_lines` (default 20), `max_matches` (40), and `max_files` (8) control
  collection. The tool streams `rg --json --sort=path`, groups matches by file,
  merges touching source windows, and renders at most 400 numbered source lines.
  No match is a successful `(no matches)` result; collection and output bounds
  are called out explicitly. This is the deterministic search→read flow, while
  raw `rg` remains available for native flags, lists, and counts.
- Same process conventions as `run_command` (§9.7): own process group, timeout or ^C
  kills the group, combined stdout+stderr, `[exit code: N]` trailer, and non-zero exit
  is NOT an error result. For search this matters because no matches is commonly exit
  code 1.

### 9.4 `edit`

> Atomically apply exact-text replacements across files[]; oldText must be unique unless replaceAll is true.

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

> Create or overwrite path with content, creating parent directories.

| param | type | notes |
|---|---|---|
| `path` | string, required | |
| `content` | string, required | empty allowed |

- `os.MkdirAll` parents (0755), write 0644, overwrite without ceremony (no permission
  system by design). Reports `created`/`overwrote`, bytes, lines.
- Existing directory at path, or trailing `/` → error.

### 9.6 `apply_patch`

> Apply a Codex-format add/delete/update/move patch; prefer edit or write_file for ordinary changes.

| param | type | notes |
|---|---|---|
| `patch` | string | full `*** Begin Patch` / `*** End Patch` text |

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
- Tool input also accepts a bare JSON string containing the patch text for
  compatibility with callers that model `apply_patch` as a freeform argument.
  At least one non-empty patch value is required.
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

> Run one command or ordered steps using a shell command or argv as an array of strings. Auto output returns compact archived receipts for large success and bounded failure diagnostics. Background supports one command.

| param | type | notes |
|---|---|---|
| `command` | string | shell command line; mutually exclusive with `argv` |
| `argv` | array of strings | program + literal arguments; mutually exclusive with `command`; must not be a shell string or JSON-encoded array |
| `steps` | array | 1–16 ordered entries, mutually exclusive with top-level `command`/`argv`/`stdin` |
| `steps[].name` | string | receipt label; omitted means `step N` |
| `steps[].command` / `steps[].argv` | string / array | exactly one per step |
| `steps[].stdin` | string | step-specific stdin |
| `steps[].cwd` | string | overrides the inherited top-level cwd |
| `steps[].timeout_seconds` | int | overrides the inherited top-level timeout |
| `stop_on_failure` | bool | default true |
| `name` | string | optional top-level receipt label; unavailable with `steps` |
| `output_mode` | string | top-level `auto` (default), `receipt`, or `full`; unavailable with `steps` |
| `stdin` | string | written to the command's standard input |
| `cwd` | string | default process cwd |
| `timeout_seconds` | int | foreground default 120, background default 1200, no maximum |
| `background` | bool | when true, start as a process-local background job and return a job id immediately |
| `resource_key` | string | background only; defaults to the canonical command cwd |
| `access` | string | background only; `read_only` or `exclusive` (default) |

- Exactly one of top-level `command`, top-level `argv`, or `steps` is required.
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
- Top-level `output_mode:"auto"` keeps successful output through 8 KiB. Larger
  success becomes a `PASS` receipt containing the normalized, 160-byte-capped
  `name` or command identity, duration, status, output byte count, a tail capped
  at 512 bytes/eight lines, and the exit-code trailer. When that tail clips the
  output (or the wait did not finish, so the receipt drops the partial-reap
  status line), the complete formatted result is carried as
  `RunResult.OriginalText`, so dispatch archives it and appends the standard
  targeted recovery hint. A fully represented small success needs no duplicate
  artifact. `receipt` always selects this success path; `full` preserves the
  prior bounded full output.
- Non-success under `auto`/`receipt` becomes a `FAIL` receipt with at most 4 KiB
  and 40 lines from the output tail. A clipped original is archived (likewise an
  unclipped original whose wait did not finish); a fully represented small
  failure needs no duplicate artifact. `full` preserves the previous full
  failure result. Infrastructure failures remain tool errors.
- Runs in its own process group/session with no controlling TTY under the turn
  context; timeout or ^C kills the group (children included) and reports output
  captured so far.
- If the timeout/^C path cannot finish reaping promptly, the tool still returns a
  snapshot of captured output and the status line notes that the wait did not finish.
- Foreground calls finish when the direct shell/program exits, not when every
  descendant closes inherited stdout/stderr; any remaining same-group descendants
  are killed after that direct exit. Long-lived commands should use
  `background:true`.
- With `background:true`, the command uses the same process-group and output
  formatting rules, but runs under the background job manager instead of blocking
  the current tool call. Background jobs default to a 1200-second timeout
  (20 minutes) unless `timeout_seconds` is set explicitly. It also defaults to an
  exclusive lease on the canonical command cwd. Set `access:"read_only"` only
  for a command that cannot mutate that resource; `resource_key` may identify a
  different coordination unit. When later work depends
  on completion, use one `background_jobs` `wait` call rather than polling
  `get`/`list`; otherwise completed output is delivered once as request-only
  context. Use `/background` for interactive inspection or cancellation.
  The background result carries both compact and original text. Automatic
  completion uses the originating `run_command` limits/artifact path; explicit
  `background_jobs` get/wait results carry the full aggregate as their own
  `OriginalText`, so choosing an explicit wait never discards recovery.
- Environment inherited unmodified.
- `stdin`, when provided, is written verbatim to the command's standard input; absent
  means `/dev/null` (programs see immediate EOF, never hang on input). Prefer it over
  `echo`/heredocs when feeding content to a command (`git commit -F -`, `python -`,
  `tee file`) — content travels with zero shell escaping.
- `steps` runs related format/build/lint/test commands serially. Top-level `cwd`
  and `timeout_seconds` are defaults; each step may override them. Background
  mode and top-level stdin are rejected for steps. By default the first
  non-zero, timed-out, cancelled, or unstartable step stops execution and reports
  the remaining skip count; `stop_on_failure:false` continues.
- Each successful step returns only `PASS <name> (<duration>)`. Failure receipts
  include status and at most 4096 bytes of command output. Suppressed success
  output and clipped failure output are combined under named command headers and
  supplied as `ResultTool.OriginalText`, so the ordinary tool-result archiver
  persists it and appends targeted recovery guidance.
- A steps call reports the sum of its resolved per-step timeouts through
  `SelfTimeouter`, preserving every step's no-maximum timeout contract under the
  dispatch backstop.

### 9.8 Shared process execution (`runProcess`)

`run_command` (§9.7), `grep`/`rg` (§9.3), and `git`/`git_readonly` (§9.9, §9.11) all
run their subprocess through one shared `runProcess` helper, so they share identical
process semantics. The §9.7 schema/description above describe `run_command`'s surface;
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

> Run git without a shell or pager. Input is an object: use workflow workspace_summary for branch, status, diff sizes, and whitespace checks; otherwise args must be an array of strings, not a string.

| param | type | notes |
|---|---|---|
| `args` | array of strings | argv after `git`; mutually exclusive with `workflow`; must not be a string or JSON-encoded array |
| `workflow` | string | `workspace_summary`; mutually exclusive with `args` |
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
- `workspace_summary` is a read-only deterministic survey. It runs porcelain
  branch/status, the latest oneline commit, staged and unstaged diff stats, and
  staged and unstaged `diff --check`. The compact labeled result omits empty
  diffstat/whitespace sections, reports `whitespace: clean` when applicable, and
  handles an unborn repository explicitly. It does not include the full patch;
  the model uses a subsequent raw `git diff` only when patch inspection is needed.

### 9.10 `web_fetch`

> Fetch HTTP(S) text, reducing HTML to readable text; supports optional limits and background jobs.

| param | type | notes |
|---|---|---|
| `url` | string, required | http/https only |
| `max_bytes` | int | default 1 MB, cap 5 MB |
| `timeout_seconds` | int | default 30, no maximum |
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

> Run restricted git status/log/diff/show/grep/blame/bisect without shell or pager; bisect may check out commits. Input is an object; args must be an array of strings, not a string.

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

> Write a retained scratch file in this run's private temp directory; returns its absolute path.

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

> Replace the complete todo list for nontrivial work; allow at most one in_progress item.

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
  When all todos are completed, the tool result also includes a one-time
  `All todos are complete.` cue.
- Implemented in `internal/todo`, not `internal/tools`, so `internal/session` can persist
  `todo.Item` without importing the tools package. A single `todo.Store` is constructed
  per process (like `write_tmp_file`); the list is saved in `state.json` (`Session.Todos`),
  reseeded on resume, and cleared by `/clear`.
- When `update_todos` is available, the REPL/one-shot drivers add a short request-only
  reminder showing the current open list. The reminder is re-injected only when the
  list changes since it was last injected (`Store.ChangedRequestContext` /
  `MarkContextInjected`): the list already lives in the transcript via the tool
  result, so re-sending an unchanged reminder every turn is pure overhead. The
  injected marker resets when the transcript is rewritten (compaction, branch,
  fork/clone) and the raw `update_todos` result may be gone, so the model sees the
  list again. Completed lists are not added as request context, and request context
  is not saved into the transcript.
- In the interactive REPL, the visible session's non-empty todo list is also printed
  before the idle prompt when the current visible agent has `update_todos`, and
  the visible todo status is printed after each successful `update_todos` call.
  One-shot runs and child-agent private todo stores do not print there.

### 9.14 `delegate`

> Delegate broad exploration or separable work; keep small or tightly coupled tasks local. Launch independent calls together, then synthesize without polling.

| param | type | notes |
|---|---|---|
| `task` | string, required | complete child prompt: objective, scope, constraints, expected report, and verification |
| `agent` | string | optional configured agent name; omitted uses the current active agent; remains a simple enum for provider compatibility |
| `mode` | string | optional; `implementation` enables deterministic mutating-work milestones; omit for exploration/review |
| `max_turns` | int | optional tool-enabled loop budget; defaults to `delegate_max_turns`; the schema publishes that numeric maximum and over-cap values are rejected |
| `continue_child_id` | string | optional terminal sibling child ID; continues compatible retained state in a fresh child record |
| `background` | bool | only for independent non-overlapping work; after one useful parent model round, harness joins outstanding delegates and requires synthesis; do not poll or duplicate |
| `resource_key` | string | background only; defaults to the canonical process cwd |
| `access` | string | background only; `read_only` or `exclusive` (default) |

- Implemented in `internal/delegate`, not `internal/tools`, to avoid an import cycle:
  the delegate tool starts a child `agent.Agent`, while `internal/agent` already
  depends on `internal/tools` for dispatch.
- The `agent` schema description appends a deterministic catalog with exact shape
  `Available:\n- <name>: <one-line description>`. The enum and catalog
  contain only candidates whose configured tools are a subset of the current
  parent's live tools. Candidate descriptions are whitespace-normalized to one
  line and individually capped at 160 bytes. Every enum value has one catalog
  entry; incompatible names and descriptions are absent. `delegate` opts into
  preserving schema descriptions in `tools.Registry.Specs`; other tools retain the
  normal schema-description stripping behavior.
- Child agents normally start with an empty transcript and use the requested
  agent definition's prompt, configured tools, and optional model target. Delegate
  calls cannot narrow or expand that tool set; callers select or define a different
  agent when they need a different capability bundle. If no `agent` is provided,
  the child uses exactly the current parent agent's active tools.
  `prompts/delegate-child.txt` is appended after that resolved system
  prompt only in `Runner.Run`; root prompts, including a configured custom static
  prompt, never receive it. The suffix says the child reports to the parent, owns
  only its delegated scope, does not ask the user questions, returns an
  evidence-backed report, and avoids recursive delegation by default. A second
  child-only block states the exact effective tool-enabled turn budget and
  discloses the possible additional tools-disabled wind-down request.
- `max_turns` has JSON Schema bound `minimum: 1`; its numeric `maximum` equals
  the active `delegate_max_turns`. The runner validates the same bound even if a provider
  emits invalid arguments despite the schema; it never silently clamps. The
  foreground result receipt and background launch receipt state the effective
  budget.
- `mode` currently accepts only `implementation` and is validated by both the
  decoder and runner. The delegate layer converts the effective turn budget to
  concrete ceiling-rounded 25%/50%/75% boundaries, adds the full schedule to
  child system context, and passes generic `agent.TurnMilestone` values to the
  loop. Milestones are delivered only when another request can consume them;
  several boundaries on one short-budget turn are coalesced. Child metadata,
  result receipts, and session stats preserve the mode.
- `continue_child_id` names an already-terminal child of the same immediate
  parent. Continuation never appends to or overwrites that source directory:
  the Runner creates a fresh child ID and seeds it with the source transcript,
  private todos, proxy session ID, cache-affinity ID, and provider response
  anchor. The continuation user message identifies the source and warns the
  child to re-check current repository state. Source usage is not charged again;
  the new child's usage contains only its new physical model calls. Its turn
  counter also starts at zero with a fresh allowance equal to the source's
  effective budget.
- A continuation inherits omitted `agent`, `mode`, and `max_turns` values and
  rejects explicitly conflicting values. It also requires exact source metadata
  identity, terminal status, the same immediate parent, a valid resumable
  `state.json`, and non-empty persisted runtime identifiers. New child metadata
  records `continued_from` and a SHA-256 `runtime_fingerprint`. The fingerprint
  covers the effective provider implementation/name, model, resolved and
  requested agent selection, mode and turn budget, depth policy, resolved
  context/output settings, prompt/cost ceilings, reasoning, server tools,
  stateful-Responses setting, final child system prompt, model-facing tool
  schemas, implementation milestones, and compaction policy. Preserving the
  requested selection keeps an omitted agent on the parent's live tool subset
  while an explicitly selected agent remains explicit. Current runtime and
  source fingerprints must match.
  Children created before fingerprints were introduced are deliberately not
  resumable through this path.
- Before the first continued request, the Runner estimates the retained
  transcript, typed todo request context, and continuation prompt against the
  current context window. At or below 60%, it restores the complete transcript
  and compatible provider continuation anchor. Above 60%, it performs one
  tool-less maintenance summary over the complete source transcript and
  replaces the new child's active history with a single typed compaction
  checkpoint. This all-history boundary is distinct from normal compaction's
  recent-turn suffix: no work after an earlier checkpoint can be omitted. The
  checkpoint preserves active prompt/steering instructions verbatim, stores
  summary and file activity in `CompactionMetadata`, archives every removed
  source message under the new child, and resets remote continuation state.
- The Runner re-estimates the compact checkpoint plus the pending prompt and
  continues only at or below 60%. A failed summary, blocked/no-op rewrite, or
  still-oversized checkpoint is rejected; exact instructions are never
  truncated to force acceptance. Maintenance usage is included in the new
  child's prompt/session totals and replay log even when the post-checkpoint
  pressure check rejects. Source usage and files remain unchanged.
  `ChildMeta` records `continuation_mode` (`retained` or
  `compact_checkpoint`) and the before/after/window estimates; the delegate
  receipt and `session stats` expose the selected path. Foreground and
  background paths apply the same contract. The latter resolves inherited
  agent/mode/budget fields before scheduling so its launch receipt is truthful.
- Background launches resolve their resource before scheduling and default to an
  exclusive lease on the canonical process cwd. A caller may declare
  `access:"read_only"` only when the child cannot mutate the resource. Lease
  conflicts fail before child/session creation and identify the active job.
  Resource and access are persisted in the new child's metadata.
- Review groups deliberately compose ordinary background `delegate` calls with
  one stable `background_jobs` `ids`/`until:"all"` join. The parent owns
  synthesis; a coordinator child whose only role is launching and waiting adds
  model turns and processed context without adding an independent verdict.
  A dedicated `delegate_group` remains deferred until session telemetry shows
  that launch-call overhead or a typed verdict/quorum contract—not coordinator
  misuse—is the remaining bottleneck. Avoiding a second launch path also keeps
  resource conflicts and exactly-once result/usage delivery centralized in the
  background manager.
- A named child agent may only run when its configured tools are a subset of the
  current parent agent's active tools. Non-subset calls return a tool error before
  any child model request is made. This exact subset check remains the
  capability-escalation guard.
- Root depth is `0`. A launch is rejected before resolution/model I/O when the
  current depth reaches `delegate_max_depth`; child runtimes increment depth, and
  the deepest allowed child has `delegate` removed before its registry/specs are
  built. If a child receives `delegate` at shallower depth, it is rebound to the
  child's full runtime snapshot so recursive validation uses the immediate parent.
- Root `max_prompt_tokens` and `max_prompt_cost_usd` are copied into every child
  `agent.Options`. They remain per-prompt ceilings for each child, not a shared
  hierarchy-wide budget. Provider/model output/context settings and recursive
  runtime snapshots are preserved as before.
- Child agents receive a private `update_todos` store when that tool is available;
  child todo updates do not affect the parent session's todo list. Foreground
  delegates remain serialized because children share the checkout and may write.
- The parent transcript records only the normal `delegate` tool call and compact result.
  Child transcripts are saved under `children/<child-id>/` in the parent session
  directory for forensics. Child token usage is reported through `MeteredTool` and
  folded into the parent prompt/session usage totals.
- A shared process-local `delegate.ActivityRegistry` tracks every running child,
  including concurrent background work and recursively rebound nested delegates.
  Registration starts only after child identity and running metadata setup, and
  the Runner's exactly-once terminalization closure flushes pending display text,
  persists final child metadata, publishes the terminal lifecycle event, and
  removes the registry entry. Stable display labels (`d1`, `d2`, …) are
  independent of durable child IDs; the registry is their sole allocator. The
  greatest activity sequence selects the latest active child, with durable ID as
  a deterministic tie-break.
- Registry activity is bounded and ANSI/control-sanitized before retention. It
  may contain turn/attempt, context and usage totals, retry state, a semantic
  assistant reply state, and allowlisted path fields for local file tools. It
  never retains model-authored reply text, reasoning, raw tool results, command
  text, URLs/search patterns, unknown arguments, or generic serialized JSON.
- In `delegate_output=lines`, the registry owns one optional `ActivityFeed`.
  Publishers append structured, sanitized events only after the corresponding
  replay event; they never call UI code or wait for a consumer. The feed assigns
  one process-local sequence, retains at most 512 events / 256 KiB, returns
  64-event / 64-KiB batches, and rotates a close-and-replace notification
  channel. Start/terminal lifecycle records evict ordinary records first;
  sequence reads synthesize a gap for each missing interval. The active registry,
  not this lossy feed, remains authoritative for current status.
- The child display coalescer preserves ordinary spaces, strips split CSI/OSC,
  normalizes CRLF, drops invalid UTF-8, expands tabs, replaces controls, and
  emits at most 2,048 UTF-8 bytes per assistant/reasoning chunk. Feed events are
  capped at 4,096 bytes. Tool completion reuses the safe summary captured at
  start; notice and model-request publication uses exact/numeric allowlists and
  structured fields, never result/provider/error text. Reasoning requires an
  already-resolved child summary mode of `auto`, `concise`, or `detailed`.

### 9.15 background jobs

Tools that opt into the reusable background job contract hand the manager a job
kind, description, optional canonical resource/access lease, and cancellable
runner. The manager owns ids, status, list/get/wait/cancel, lease enforcement,
one-shot notices, and request-only context delivery.
`run_command`, `grep`, `rg`,
`web_fetch`, and `delegate` support this path via `background:true`; background
delegate jobs still use the same launch validation, child transcript, private todo,
and token-accounting behavior as synchronous delegate.

`background_jobs` accepts:

| param | type | notes |
|---|---|---|
| `action` | string | `list`, `get`, `wait`, or `cancel`; omitted means `list` |
| `id` | string | required for `get` and `cancel`; optional for `wait` |
| `ids` | array of strings | optional multi-job selection for `wait`; mutually exclusive with `id` |
| `until` | string | `first` (default) or `all` for `wait` |
| `timeout_seconds` | integer | `wait` timeout; omit for ordinary dependency waits (default 120 seconds), rather than using a short timeout as a status probe |

- Jobs live only in the current harness process. Running jobs are abandoned on process
  exit and cleared on `/clear`.
- Resource keys are absolute paths with symlinks resolved through the longest
  existing prefix. Multiple `read_only` jobs may share an exact resource key.
  `exclusive` conflicts with every unfinished lease for that key; the
  deterministic error identifies the first active job in launch order. Different
  resource keys do not conflict. Completion and failure release immediately.
  Cancellation retains the lease until runner cleanup finishes. Abandonment
  releases it immediately and late runner return cannot overwrite the abandoned
  status. Snapshots, `get` output, completion context, launch receipts, and
  delegate child metadata expose the normalized lease.
- `wait` is event-driven rather than polling. `id` selects one job; `ids`
  selects an explicit group; omitting both snapshots the jobs currently running.
  `until:"first"` returns on the first selected completion, while
  `until:"all"` joins every selected job. Selection is stable, so later launches
  never extend an in-flight wait. If an untargeted wait finds no running jobs, it
  returns the current list immediately. A timeout is a normal result containing
  the latest selected status. Jobs returned as completed are marked as delivered
  so the same result is not automatically injected again. Completion notices and
  nested usage accounting remain one-shot and independent.
- The system prompt and background-capable tool schemas route a strict completion
  dependency to one `wait` call. `get` and `list` are for nonblocking inspection,
  not repeated status polling.
- Completed job summaries are delivered once as request-only context to the parent
  agent, including the transcript path when one exists. They are not inserted into
  the parent transcript. Background delegates are marked join-required: the parent
  may perform one subsequent useful turn, but cannot complete the prompt until
  all such delegates finish and a model request has received their reports for
  synthesis. Ordinary background commands remain detached and may outlive the prompt.
  Output uses the same per-tool truncation limits as foreground
  dispatch; when truncated, the full result is archived under
  `artifacts/tool-results/` and the request context includes the same absolute path and
  targeted `read_file`/`rg` guidance as a foreground result.
- A job may carry compact `Text` plus complete `OriginalText`. Automatic
  completion uses `Registry.PrepareResultWithOriginal`; `background_jobs`
  implements `ResultTool` so explicit get/wait output preserves the same
  archive opportunity instead of consuming completion context and losing the
  suppressed original.
- Background delegate results carry child model usage through the manager exactly
  once; the agent folds it into the parent prompt and session totals before completion,
  including failed child runs that returned partial usage.
- Background jobs run in the same cwd/tool policy as ordinary tools. Resource
  leases coordinate opted-in local background work; they do not sandbox paths or
  serialize foreground filesystem edits.

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
  applies the annotation stripping described at the start of §9.
- **`ReadOnly(input)` is policy-controlled.** Harness trusts
  `annotations.readOnlyHint:true` for enabled MCP registrations, so advertised
  read-only tools can join read-only parallel islands (§8.1) and can be exposed
  to agents whose `mcp_tools` mode is `read_only` (§14).
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

### 9.17 `record_plan` (`internal/plan`)

- Persists an implementation plan as markdown under the live session directory:
  `<session>/plans/NNNN-<slug>.plan.md`, written temp-then-rename. Input is
  `{title, plan}`; `plan` is the self-contained markdown body. Returns the
  absolute path.
- The store is one `*plan.Store` per process (like `update_todos`); the list is
  saved in `state.json` (`Session.Plans`), reseeded on resume, and reset on
  `/clear`. `internal/plan` is a leaf package so `internal/session` can persist the
  `Plan` type without importing tools.
- Available to every default agent (in `defaultTools`), so plans are a first-class
  artifact even outside plan mode. The session directory is read at call time, so
  it errors clearly when none exists (one-shot mode).
- The REPL surfaces the latest recorded plan's path to the user, mirroring the
  `update_todos` status (§9.13). Display state is relative to the current model
  prompt: the first plan added by that prompt is `Plan recorded: <path>`, a newer
  artifact added when a plan already existed (or after multiple records in one
  prompt) is `Plan updated: <path>`, and an unchanged current plan — including a
  pre-existing plan when resuming — is `Plan: <path>`. The sink prints the line to
  `Errw` immediately after each successful `record_plan` result and again before
  the per-prompt usage line; the unchanged fallback is also shown at the idle
  prompt, with the existing prompt-boundary marker deduplicating a line already
  printed before usage. All variants are display-only — re-rendered from the
  shared store, never part of the model's tool result or context — so the user
  always learns where the plan was written even if the model does not mention it.

### 9.18 `request_implementation` (`internal/plan` + `internal/tools`)

- The plan agent's request to hand the recorded plan to an implementation agent.
  Input is `{brief, agent?}`. It requires and always selects the most recently
  recorded plan; the implementation agent reads the plan as its task spec rather
  than being handed only the brief. Model selection belongs to the target agent's
  configuration rather than this model-callable tool.
- The `agent` field's schema is built from the configured agent names: its
  `enum` constrains the value to a real agent, so the model cannot invent one.
  The tool also rejects an explicit unknown `agent` before recording a pending
  handoff. Omitting it leaves target selection to the REPL and configured handoff
  default (`auto` unless overridden).
- Tools cannot prompt, so it only records a `plan.HandoffRequest` in a shared
  `*plan.Pending` holder and returns. At the prompt boundary, the REPL displays
  the brief, asks for approval, performs the switch, and immediately starts the
  implementation agent; `/handoff` remains available as a manual fallback (§10).
  It errors in one-shot mode (no interactive approval).

## 10. CLI / REPL (`internal/ui`)

### Rendering

- Assistant text, reasoning summaries, and handoff briefs displayed for approval
  use a small stdlib-only Markdown renderer on terminal output. Emphasis becomes
  ANSI bold/italic when color is enabled, inline code and links use cyan,
  headings keep their `#` markers and render bold (with H1 headings also
  underlined), thematic breaks render as `────────────────────`, lists normalize
  and indent continuations, and tables are padded. Inline spans close only the
  bold, italic, underline, or foreground attribute they opened instead of issuing
  a blanket SGR reset, so nested code, links, and emphasis preserve their outer
  heading or emphasis style.
- Paragraphs and list bodies are rendered once and then wrapped by visible
  rendered width (80 columns when terminal width is unavailable), so Markdown
  delimiters do not consume columns and spans may cross a break. The wrapper
  tracks active SGR attributes, closes them before each newline, and reopens them
  after the continuation prefix so list indentation is never styled. Displayed
  handoff briefs follow the same Markdown, color, and width policy, while their
  archived and implementation-context copies retain the original Markdown
  source. Redirected one-shot stdout remains raw model text.
- Recognized language tags on fenced code blocks use the stateful, stdlib-only,
  additive highlighter in `internal/term/highlight` when ANSI is enabled. Aliases
  and decorated info strings are normalized; untagged, unknown, and `text`
  fences fall back to plain code. Opening and closing delimiters remain unstyled,
  code lines are never wrapped, and stripping ANSI reproduces the renderer's
  normalized plain output. Lexer state follows multiline comments and raw strings
  across completed streamed lines and arbitrary input deltas, then resets at the
  closing delimiter so it cannot leak into a later fence.
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
  model-backed compaction or handoff summary, or a join-required background
  delegate is outstanding, the static waiting line is replaced by a single in-place
  line painted with `\r\x1b[2K` and repainted ~once a second by a `time.Ticker`
  goroutine (with a stop-and-drain handshake): `[turn: 1 · 12s · ctx 30% 60.0k/200.0k │ prompt 18s]`,
  `[tool: grep args=["x"] · 3s]`, `[context: compacting · 3s]`,
  `[handoff: generating brief · 3s]`, or
  `[background: waiting for delegates · 12s │ prompt 30s]`, with the same compact key
  arguments as the completed tool summary and the running context-window percentage
  and compact used/window token counts appended for model waits
  (`· ctx 30% 60.0k/200.0k`). Active delegate registry state is appended to the
  same row as `· delegate d1 explore: turn 2 · thinking`; concurrent children use
  `· 3 delegates · latest d2 plan: tool read_file path="…"`. Join-required
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
- Tool results render as one-liners by default:
  `[grep] args=["-R","-n","func main","."] → 14 lines, 2.1KB`
  built from the tool name, key args, and a result summary. `-v` adds the first ~5 lines
  of each result, dimmed, and also enables progress details.
- Large estimated contexts, payloads, or tool schemas print one warning per
  prompt because they can materially slow first response latency.
- Per-prompt usage line:
  `[prompt: 3 turns · 12.4k (18.0k) in / 1.8k (2.6k) out · $0.071 ($0.101) · 4.3s]`
  (cost omitted for usage without known cost). When non-zero it
  also appends cache-read tokens with the cache-hit ratio (`· cache 3.0k read (75%)`)
  and reasoning tokens (`· 450 reasoning`). A model with no known cost prints a
  one-time-per-model `[note: no price configured for "<model>"; ...]` notice instead of
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
  `info`, `warn`, or `error`. Normal sessions also tee JSON diagnostics to
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
complete range is classified once its explicit end marker arrives. When a terminal
does not honor bracketed paste, a timing-based heuristic on the interactive TTY
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
and `!!...`. In raw prompt-editor buffers that start with `!`, the first word still
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
`{git_branch}`, `{model}`, `{reasoning}`, `{vimode}`, `{vimode:short}`, and
`{vimode:long}`.
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
During an active REPL turn, harness restores the prompt terminal mode and temporarily
configures Escape as the second canonical-mode line delimiter so Esc-Esc can cancel the
turn; typeahead lines are queued for the next prompt. Bracketed paste is disabled while
Escape is armed, then restored when the prompt returns. Before launching the editor,
harness restores the original termios and disables bracketed paste so the editor owns a
normal TTY; after it exits, the REPL reapplies its prompt settings. `!command`
shell escapes use the same terminal handoff.

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
available skill anywhere in the text; the next turn gets request-only
context telling it to read that skill's `SKILL.md` before acting. `$$` escapes a
literal `$`. Literal `@path` / `@"path with spaces"` references remain prompt text
and never expand file contents; when they point at supported image extensions,
typed REPL prompts, initial `-i` prompts, and one-shot prompts auto-attach the
image if the model supports image input. Pasted and external-editor prompts keep
literal-safety semantics and do not auto-attach from `@` references.

The canonical command inventory and operator-facing behavior live in the
[usage reference](usage.md#repl-commands). `internal/ui.App.command` dispatches
those commands; state-changing commands such as `/clear`, `/compact`, `/model`,
`/agent`, and `/handoff` also invalidate or rotate the relevant continuation and
prewarm state described in their owning sections. `/clear` starts a new cache
affinity; context/model/tool changes preserve the current conversation affinity.

Anthropic extended thinking and 1-hour prompt-cache writes appear in the
disjoint reasoning and cache-write buckets defined in §6.

`/model <name>` resolves exactly first, then falls back to a case-insensitive
unique prefix and then unique substring match over the catalog; an ambiguous match
lists the candidates rather than switching. An unknown `/command` prints
`unknown command "<cmd>"; did you mean <suggestion>? (type /help)`, where the
suggestion is the nearest known command by a stdlib Levenshtein distance (shared
prefix wins, threshold `1 + len(cmd)/3`).

### Flags

The exhaustive flag inventory is maintained in the
[usage reference](usage.md#flags). `internal/config.newFlagSet` is the parsing
source of truth and also backs `config.Usage`, so runtime `-h` output cannot drift
from accepted flags. Configuration resolution remains flags > environment > file
> defaults, except for the documented config-only structured settings.

`-show-config` includes the effective merged agent definitions and static
`system_prompt`; it exits before contacting the model proxy. Dynamic runtime
prompt sections such as env context, user/project `AGENTS.md`, skills, and the
active agent prompt are not included in the `system_prompt` field.

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

## 11. Session persistence (`internal/session`)

```go
type Session struct {
    Version       int                `json:"version"` // 5: build/runtime attribution and efficiency telemetry
    ID            string             `json:"id"`
    CWD           string             `json:"cwd,omitempty"`
    ParentSession string             `json:"parent_session,omitempty"`
    ParentEntryID string             `json:"parent_entry_id,omitempty"`
    ActiveLeaf    string             `json:"active_leaf,omitempty"`
    Provider      string             `json:"provider"`
    Model         string             `json:"model"`
    Created       time.Time          `json:"created"`
    Updated       time.Time          `json:"updated"`
    Build         BuildMetadata      `json:"build"`
    Runtime       RuntimeProfile     `json:"runtime"`
    System        string             `json:"system"`
    Agent         string             `json:"agent,omitempty"`
    Prompt        int                `json:"prompt,omitempty"`
    ResponseState *llm.ResponseState `json:"response_state,omitempty"` // Responses stateful continuation anchor
    ProxySessionID string `json:"proxy_session_id,omitempty"` // sticky-routing/WebSocket isolation key
    CacheAffinityID string `json:"cache_affinity_id,omitempty"` // stable prompt-cache routing identity
    Todos         []todo.Item        `json:"todos,omitempty"`          // update_todos list, reseeded on resume
    Plans         []plan.Plan        `json:"plans,omitempty"`          // record_plan list, reseeded on resume
    Usage         UsageTotals        `json:"usage"`                    // session aggregate
    UsageByModel  map[string]UsageTotals `json:"usage_by_model,omitempty"` // per model target cost
}

type Entry struct {
    Type     EntryType // segment | compaction | branch | context_reset
    ID       string
    ParentID string
    Time     time.Time
    Messages []llm.Message // segment/context_reset
    // Compaction checkpoint, kept-entry boundary, archive reference, and size.
    // Branch source/common ancestor/optional summary and workspace warning.
}

type UsageTotals struct {
    llm.Usage         // cumulative token counts
    CostUSD   float64 `json:"cost_usd"` // 0 when the model has no price entry
}
```

- Schema v5 is intentionally breaking: loading or replaying an older
  `state.json` returns a clear unsupported-version error; there are no aliases,
  migrations, or legacy linear-session fallback.
- A session path is a directory. `tree.ndjson` is canonical append-only
  conversation data; its first record is a session header and later records are
  immutable typed entries. `state.json` stores mutable runtime state and the
  active leaf. `active-turn.json` is a transient atomic recovery record for the
  current provider boundary. `raw.ndjson` remains chronological replay data,
  `diagnostics.ndjson` stores JSON slog diagnostics, `compactions/` stores raw
  messages removed from active context, and `artifacts/tool-results/` stores full
  truncated tool output.
- Segment entries are safe navigation boundaries. An assistant tool-use message
  and its immediately following tool-result message share one segment so a
  branch cannot split the provider transcript invariant.
- Before each provider request and before dispatching emitted tool calls,
  Harness atomically replaces `active-turn.json` with the complete resumable
  runtime state: transcript, todos, plans, usage, cache/proxy IDs, and safe
  Responses continuation anchor. An open tool-use is stored with synthetic
  `interrupted` results. Recovery therefore never automatically re-executes a
  tool whose process-local completion is unknown.
- After every validated closed conversational turn, root and child sinks first
  write the recovery record, then append/sync the tree and atomically replace
  `state.json`. Only after that canonical save succeeds is `active-turn.json`
  removed. Prompt-end/manual saves use the same consolidation order.
- Saves append and `fsync` new tree entries before atomically replacing
  `state.json` via temp-file plus rename. A malformed final tree record is
  treated as an interrupted append; malformed non-final records, missing
  parents, duplicate IDs, and invalid segments are hard errors.
- Active provider context is reconstructed by walking parents from
  `ActiveLeaf`. The newest compaction or context-reset entry defines the active
  prefix; later segment and branch entries are then applied in path order.
- Every saved message and append-only replay event carries a timestamp. Replay
  events identify `prompt`, `turn`, and (for provider requests) `attempt` separately.
  `turn_attempt_start`, `turn_attempt_abandoned`, and `turn_attempt_usage` describe
  provider calls; `turn_complete` closes a conversational turn; `prompt_usage`
  closes the top-level prompt; `maintenance_usage` accounts for compaction,
  prewarming, handoff-summary, and branch-summary calls without creating turns.
  `model_request` records proxy/request lifecycle and every API issue with timing,
  parsed error, and correlation metadata; these events are replay/analysis data
  only and never become conversation-tree entries or model context. `checkpoint`
  records the boundary kind, save duration, and message count; stats use
  closed-turn records to expose save overhead and lag. `branch` records
  navigation source/target IDs in chronological replay.
- Sessions store the CLI-owned previous response/interaction ID, anchored message
  count, and transcript fingerprint in `state.json`. Resume restores it only
  when continuation is enabled for the exact current target, saved/current
  provider and model match, the index is in range, and the fingerprint matches
  the materialized active prefix. `active-turn.json` enforces the same invariant;
  invalid recovery state is discarded. `-responses-stateful=false` installs no
  anchor and sends complete history on every request.
- Gemini Interactions uses the same CLI-owned continuation contract.
  `interactions_stateful` controls the target's catalog capability; sessions
  retain signed thought and Google Search steps so a missing/rejected stored
  interaction can be replayed from complete history through any proxy replica.
- Image bytes are embedded in `tree.ndjson` as provider-neutral base64 blocks so
  resume is self-contained; `raw.ndjson` records only image metadata for replay.
- Auto-save to `~/.local/state/harness/sessions/<timestamp>`; the path is printed at
  startup. `-session` chooses a directory; `-resume` loads `state.json` plus its
  active tree path, or applies a newer `active-turn.json` recovery record before
  continuing. Resume reports the recovered phase. It also marks child metadata
  left `running` by the prior process as `abandoned`; such children are terminal
  and may be continued by child ID when their saved runtime contract still
  matches. Distinct `-resume <source>` and `-session <destination>` clone the
  active path with parent lineage and fresh usage. `/clear` rotates to a fresh
  directory.
- `/tree` renders a harness-native searchable/paged line picker over tree nodes.
  The renderer keeps unary paths in one graph lane, adds lanes only for sibling
  branches, labels checkpoint kinds, condenses repeated tools, and clips rows to
  the live terminal width. Selecting a human prompt targets its parent and
  returns its text/images as editable prompt prefill. Other entries are selected
  directly. Before moving, the user chooses no summary (default), a model-written
  summary, or a summary with custom focus; summary failure leaves the active leaf
  untouched.
- `/fork` uses the same compact graph projected onto human prompts, so hidden
  assistant and tool entries do not affect indentation, then extracts the selected
  pre-prompt path into a new session. `/clone` extracts the current path. Extracted
  sessions receive a new session ID, `ParentSession`/`ParentEntryID`, prompt number
  zero, fresh lifetime usage, and cleared Responses/proxy continuation anchors.
  Model, provider, agent,
  reasoning, todos, plans, hooks, and working directory stay global/current.
- Conversation navigation does not alter filesystem or Git state. Every branch
  adds a model-visible internal warning to inspect current files before assuming
  their state. Optional branch summaries describe only the divergent old suffix.
- Child-agent runs are stored below `children/<child-id>/` with their own
  `state.json`, `raw.ndjson`, `meta.json`, and artifacts. Parent resume ignores these
  child transcripts; they are forensic sidecars. `meta.json` is a `ChildMeta` index —
  id, parent id, kind, agent, provider/model, status, task preview, transcript/replay
  paths, error, usage, message count, mode, resolved/requested agent,
  background resource/access lease,
  continuation source/runtime fingerprint, requested/effective turn budgets,
  physical turns used, and termination reason. The delegate Runner creates `running`
  metadata, then owns one terminal transition to `completed`, `failed`, or
  `canceled`; a later parent resume changes stale `running` metadata to
  `abandoned`. State-save failure does not skip the terminal metadata attempt.
  Terminal reasons are `model_completed`, `turn_limit`, `token_limit`,
  `cost_limit`, `repeat_guard`, `error_guard`, `cancelled`, or `error`;
  `turn_limit` does not imply semantic incompleteness, and `model_completed`
  does not prove acceptance. `prompt_usage` remains the final normal child event
  and carries the same reason. Inline delegate lines are
  process-local display events only: they are not appended to either parent or
  child persistence. Child `raw.ndjson` retains the exact replay callbacks and
  remains the full-fidelity source for `session replay --follow`.
- `harness session replay <session-dir>` prints `raw.ndjson` as the familiar
  user-facing terminal view, filtering assistant/reasoning deltas from retry
  attempts that were explicitly discarded before a later successful attempt.
  Raw assistant deltas remain unchanged on disk; replay renders Markdown at
  display time. `session replay --follow` keeps the same stateful renderer and
  consumes only newline-complete append-only records with the ordinary 16 MiB
  record limit. It filters discarded attempts in the initial batch; a later live
  discard marker is printed rather than retracting visible output. Terminal child
  metadata triggers one final drain and a child `prompt_usage` record is a fallback
  completion marker. Root sessions have no terminal marker and follow until their
  context is canceled. Log rotation is not supported.
- `harness session timings <session-dir>` reads `raw.ndjson` timestamps and
  prints prompt totals, turn-attempt durations, tool durations, largest event gaps,
  context/payload estimates, and model API issue counts/provider time/scheduled
  retry wait. A prompt without `prompt_usage` is labeled `in progress`, and its
  elapsed time ends at the latest recorded event rather than being reported as
  zero.
- `harness session stats <session-dir>` reads the existing root and child
  `state.json` and `raw.ndjson` files, `compactions/*.meta.json` plus their input
  transcripts, and `children/*/meta.json`. It reports turns, direct tool and
  command activity, lifetime parallel batches, compactions, tree
  entries/branches/leaves/depth, navigation events, authoritative token/cost
  totals, calls per tool-bearing turn, standalone todo/single-inspection turns,
  result truncation/byte/timing totals, and a hierarchical delegate breakdown.
  The session header includes build identity and the non-secret runtime profile
  used for efficiency comparisons. A child without a completed
  `state.json` checkpoint is reconstructed from its metadata and replay for
  analysis and marked `checkpoint: unavailable`. Conversation statistics
  distinguish prompts, turns, model calls, retries, and maintenance calls.
  Sessions with closed-turn checkpoint events also report checkpoint count,
  average/maximum save time, and lag in completed turns and seconds. Root usage already includes delegate and
  maintenance spend and is never
  summed with child usage; each child usage total likewise includes its nested
  delegates. Direct tool activity is instead summed once from every replay log.
  The non-overlapping direct model-activity total likewise sums physical
  `turn_attempt_usage` and `maintenance_usage` records once from every root and
  child replay. Compaction metadata uses the writer's canonical field shape;
  the stats reader accepts unknown additive fields while still rejecting
  malformed JSON and trailing values.
- Transcripts are provider-neutral; resuming under a different provider/model works.
  When flags disagree with the state, flags win with a warning. Tool-result messages
  may include local-only `parallel_tool_batches` metadata; provider adapters ignore it.

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
  reaches `compact_trigger_percent` (default **78%**) of the model's effective/learned
  context window. Successful compaction targets `compact_target_percent` (default
  **65%**) after fixed system/tool-schema overhead, providing hysteresis instead of
  re-compacting after each small result. Reported input counts cache-read/cache-write tokens because cached context
  still occupies the window. The trigger runs after a prompt and proactively between
  turns, before the next request when tool results balloon the estimate. Also manual
  `/compact`. The estimate side is the last **measured** input
  tokens (`lastInput`) plus a bytes/4 estimate of only the messages appended since
  that measurement (the append boundary), so the trigger tracks real usage instead of
  re-estimating the whole transcript; the raw bytes/4 estimate is reserved for the
  degradation ladder. `compact_auto_enabled: false` gates the proactive,
  exact-count, and post-prompt checks only; explicit compaction and the single
  provider-overflow recovery retry remain enabled. The terminal warning follows
  the configured trigger and is suppressed with automatic compaction.
- **Live-transcript retention pass.** Before each model request the agent runs a
  pure-local retention pass (no model round-trip). The default `auto` policy
  uses pressure-triggered epochs for both stateful and stateless providers
  instead of editing on ordinary age progression. An armed epoch runs when the
  estimated full context reaches 60% of the effective window, trims every
  currently eligible read-only tool-result block and aged image in one pass,
  and disarms until context falls to 50% or below. Compaction remains the safety
  net if an epoch cannot reclaim enough.
- A pressure edit clears the CLI-owned `previous_response_id`
  exactly once, while a below-pressure stateful request preserves it and sends
  only the appended delta. Stateless providers get the same batched transcript
  bounding without continuation reset semantics.
- The `retention_policy` experiment control can force legacy `age`, `pressure`,
  or `disabled`. Age mode trims eligible read-only results older than
  `compact_keep_turns` completed turns and images two or more turns old before
  each request. Disabling the local pass does not disable compaction or
  provider-overflow recovery. Delegate continuation fingerprints include the
  selected policy.
- Both retention policies preserve the §4 transcript invariant and are
  idempotent. `raw.ndjson` `retention` events record policy/trigger, blocks and
  bytes trimmed, estimated context before/after, whether Responses state was
  reset, and whether the next request used stateful continuation or full
  context. `session stats` summarizes those epochs and request shapes.
- **Turn boundary:** a completed turn begins at an assistant response and includes
  its immediately following tool-result message when that response requested tools.
  User prompts, steering messages, and synthetic context are inputs to a turn, not
  boundaries that merge all round trips since one prompt.
- **Mechanism:** keep the system prompt and select a raw whole-turn suffix newest
  first until it reaches `compact_keep_tokens` (default 20,000) or the
  `compact_keep_turns` cap (default 8). Always retain at least the newest completed
  turn. Under low-water pressure, move the oldest retained round behind the boundary,
  regenerate the summary over the enlarged removed set, and repeat until the target
  is met or only the newest turn remains. Send
  everything older to the model with the summarization instruction in
  `prompts/compaction-summary.txt`: preserve the task/goal and acceptance
  criteria, still-constraining decisions, semantic state for meaningful file
  changes, active worktree/blocker/gate/verification state, key facts, and
  unresolved intent; do not invent. The model does not enumerate read-only
  inspected files or reproduce the typed active todo list. Summary output is
  capped by `compact_summary_max_tokens` (default 2048).
  A first compaction uses `prompts/compaction-summary.txt`. A repeated compaction
  removes the structured prior checkpoint from ordinary summary history, passes its
  exact generated summary once as data, and uses `prompts/compaction-update.txt` to
  produce a complete replacement. Map/reduce phases summarize only newly aged chunks;
  the prior summary appears only in the final update call. Replace the old messages
  with one synthetic **user** checkpoint headed
  `=== Compaction checkpoint ===`. It carries the active prompt and any steering
  instructions verbatim, then the progress summary and raw archive reference. This
  preserves current instructions explicitly without pretending the summary was a
  conversational assistant turn. `/compact [focus]` adds one-shot emphasis to all
  summary phases and records that focus only on the resulting checkpoint/archive.
- **Typed compacted-history state:** the active todo store is persisted in
  session state independently of the transcript. A successful compaction resets
  its injection marker, so the first subsequent model request receives the
  current typed list even if the raw `update_todos` result aged out. The
  model-authored summary carries only unresolved intent, blockers, and gates
  that are not represented by that list.
- **Deterministic compacted-history files:** correlate each validated assistant
  tool-use message with its immediate result message. Successful supported
  `read_file`, `write_file`, `edit`, and `apply_patch` calls contribute normalized,
  sorted cumulative read/modified paths; modified wins over read. Batched reads are
  call-level and therefore retain every requested path even when one result is an
  inline per-file error. Commands, Git, MCP, malformed inputs, failed calls, and
  unsupported/custom tools are skipped. The JSON index is the authoritative
  recognized file-activity inventory in the active checkpoint and summary
  request. The model records semantic state only for meaningful changes and
  unfinished mutation intent; it does not duplicate read-only inspected paths.
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
  `trigger` field (`auto` or `manual`); a focused manual compaction also supplies
  `focus` to both hooks.
- **Speculative idle compaction:** the interactive REPL can opt in with
  `compact_idle_after_seconds` (zero disables) and a lower
  `compact_idle_trigger_percent`. At the timer boundary the owning goroutine
  captures a deep transcript copy plus a SHA-256 fingerprint covering the
  transcript and compaction-relevant runtime. A private Agent performs the
  model work against that snapshot; it shares only the provider and cannot call
  the live archiver or mutate session state. Submitted input cancels the worker
  and marks any late result for discard, so prompt execution never waits for
  speculative summarization. The owning goroutine applies a completed candidate
  only when that fingerprint still matches, then runs the live archive callback,
  installs the checkpoint, saves, and prewarms. Configured `PreCompact` or
  `PostCompact` hooks make the idle path ineligible because their external side
  effects cannot be rolled back.
- Each started idle attempt emits an `idle_compaction` replay event with
  outcome, wall time, threshold, message counts, and context before/after when
  applied. Summary tokens returned by the worker are `maintenance_usage` under
  purpose `idle_compaction`, including canceled or stale work. `session stats`
  reports attempt outcomes, average/maximum wall time, and average applied
  context reduction.
- Before replacing active history, raw removed messages are archived under
  `compactions/`; the active summary includes the archive reference.
- **Summary call hardening (`summarizeOne`).** The summarization request runs with
  reasoning disabled (`Reasoning: llm.ReasoningConfig{}`) regardless of the session's
  effort, so compaction never spends a thinking budget. It retries transient
  mid-stream errors with the shared `retry.Next` backoff (up to `streamRetries`) so a
  429 at 78% does not abort compaction. If the summary itself is truncated
  (`StopMaxTokens`) it doubles the token budget and retries once, then accepts the
  result. Chunking, retries, and archive writing remain covered by one transient
  `context: compacting` progress phase.
- **Image-aware estimation.** `estimateTokens`/`estimateRequest` weight each
  `BlockImage` at a flat `imageTokenEstimate` (1600 tokens) rather than counting its
  base64 bytes at bytes/4, which wildly overstated images. Correspondingly,
  `truncateLargestBlock` ranks an image by that token weight, so a large text result
  is truncated before an image.
- The summary call's tokens and cost are added to session totals as maintenance,
  never as a turn. Replay records a `maintenance_usage` event. The visible notice
  reports the full request estimate before and after:
  `[compacted: 4 turns → checkpoint · ctx ~18.2k → ~12.7k]`.
- **Tree/archive persistence:** archive the final enlarged removed set unchanged and
  persist the exact summary, current focus, and cumulative file lists in additive
  checkpoint/archive/tree metadata. `FirstKeptEntryID` remains an atomic original-tree
  boundary when retained history can be linked directly. If degradation rewrites retained
  content, or a valid boundary falls inside a wholesale context-reset entry, mark the
  compaction entry and materialize the retained suffix as new atomic segment entries so
  save/resume cannot resurrect or reject it. Old entries with omitted fields reconstruct
  metadata from their canonical tree summary without a schema-version bump.
- **Degradation:** once only the last turn remains, hard-truncate the largest tool
  result/input/image blocks in place with markers.
  When there is no older turn to summarize but the transcript is still over budget,
  the same ladder degrades the **oversized single turn** in place. Each degrade pass
  deep-copies before mutating (so a post-degrade `ValidateTranscript` failure rolls
  back to the live transcript) and skips a rewrite that would not actually shrink
  (`[compact: transcript over budget but nothing left to shrink]`). Automatic
  current-turn fallback notices are throttled within a prompt so repeated no-op or
  tiny-shrink attempts do not flood the UI. Never wedge.
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
| providers | `httptest.Server` replaying `.sse` golden fixtures per dialect → assert ordered events; golden request-JSON tests (Responses input items, Chat role:tool hoisting, args-string vs object, system placement, `stream_options`, cache_control); tool-call reassembly tables (fragment splits, empty args → `{}`, interleaved parallel calls, invalid tail → invalid `Done` diagnostic); truncated stream; mid-stream cancellation; retry loop via injected sleeper (429-then-200, 400 immediate failure, budget exhaustion) |
| `internal/retry` | `Next`: jitter bounds, 30s cap, Retry-After floor |
| tools | table-driven against `t.TempDir()`; `grep` wrapper against the host CLI; optional `rg` registration with a fake executable on PATH; `git` against a scratch `git init` repo (skipped if git absent); `run_command` timeout via `sleep`; `apply_patch` at the tool level covers the Codex Add envelope, canonical `patch`, compatibility decoding paths, bare-string input, and conflicting-alias / parse-error format-hint paths, while `internal/tools/patch` covers parse + apply for create/update/delete/rename and first-rejection-leaves-file-untouched |
| agent loop | `FakeProvider` scripts: multi-tool batches, error-result feedback (next request carries the error), max-turns stop, cancellation → transcript still re-sendable |
| delegate | child-agent request shape and child-only prompt suffix, model-visible compatible-agent enum/catalog (ordering, normalization, caps), parent-tool subset rejection, depth transitions/deepest-child removal, recursive runtime rebinding, inherited token/cost budgets, private child todo stores, child transcript persistence, metered usage folded into parent prompt totals |
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
Opt-in upstream model checks, including separate ChatGPT-subscription and
first-party OpenAI Responses legs, use the `livemodel` tag via
`make test-live-models`; see `docs/smoke.md` for setup and expectations.

## 14. Agent definitions (`internal/agentdef`)

An **agent definition** is a named bundle of an allowed-tool set, optional
model target, description, and extra system-prompt instructions. It lets one
harness behave as a collaborative planner, autonomous worker, specialized
reviewer, or the wide-open default without separate binaries.

- **Selection** follows the standard precedence (§7): `-agent` flag >
  `HARNESS_AGENT` > `agent` in the config file > the built-in default `auto`. An
  empty value means "unspecified", so a resumed session's saved agent (§11) can
  supply it before the `auto` fallback. `/agent <name>` switches at runtime;
  `/agent` lists inside the REPL; `harness --agents` lists from the CLI. `/mode`
  is a REPL alias only. Shift-Tab cycling invokes this same full runtime
  selection path—not a display-name or model-only swap—so it recomposes the agent
  prompt, tools, model/reasoning selection, and response continuation state.
- **Built-ins:** `auto` (all available built-in tools plus discovered MCP tools,
  including `record_plan`, `delegate` and background job tools; its
  `prompts/agents/auto.txt` is a one-byte file — a single newline — that trims to
  empty, so it contributes no prompt body), `explore` (the shared inspection
  tools — `read_file`, `list_dir`, `glob`, configured search tool(s),
  `run_command`, `web_fetch`, optional `git_readonly` — and read-only MCP tools;
  no file mutation, implementation handoff, todos, background jobs, or
  delegation; prompt in `prompts/agents/explore.txt`), `plan` (the shared
  inspection tools, read-only MCP tools, `write_tmp_file`, `record_plan`,
  `request_implementation`, `update_todos`, `delegate`, and background job
  tools, plus `prompts/agents/plan.txt`; both `explore` and `plan` gain
  `run_command` from the shared inspection set so they can explore via external
  tools (`gh`, builds, screenshots, live apps) but have no first-class
  file-mutation tools, keeping "don't modify the project" a prompt-level
  contract), and `independent` (all available built-in tools
  plus discovered MCP tools, including `record_plan`, `delegate` and background
  job tools, a complete-without-asking prompt from
  `prompts/agents/independent.txt`). `auto`/`independent` advertise `git` but not
  `git_readonly` — `git` covers every read-only operation, so listing both would
  duplicate functionality and waste context. The delegation subset guard treats an
  available `git` as satisfying a required `git_readonly`, so these parents can
  still delegate to `explore`/`plan`. `record_plan` (§9.17) is in every default
  agent's set;
  `request_implementation` (§9.18) is plan-only.
- **Descriptions are required selection metadata:** after resolution, every agent
  must have a nonblank trimmed `description` stating when a parent should use it.
  A new custom name without one is a fail-fast startup/`--agents`/`--show-config`
  error; there is no warning, generated fallback, or compatibility shim.
- **Config `agents`** entries **field-level merge** onto a built-in of the same name:
  a non-empty `description`, `allowed_tools`, `mcp_tools`, `prompt`, `model`, or
  `reasoning` replaces, and an omitted field inherits. Thus an override
  of `auto`, `explore`, `plan`, or `independent` may inherit its built-in
  description. A new name defines a new agent (no `allowed_tools` ⇒ the full
  default set). Agent prompts
  accept `@file` and are expanded once at startup (fail-fast); relative config-file
  references resolve from the config file directory.
- **MCP exposure:** `mcp_tools` is one of `disabled`, `read_only`, or `all` (with
  `read-only`/`readonly` accepted as aliases for `read_only`) and controls automatic
  exposure of discovered MCP tools. An invalid value is a fail-fast validation error
  (surfaced by `main`/`--show-config` after field-level merging). Built-ins default to
  `all` for `auto`/`independent` and `read_only` for `explore`/`plan`; a new agent with no
  explicit `allowed_tools` defaults to `all`, while an explicit `allowed_tools`
  whitelist defaults to `disabled` unless `mcp_tools` opts it back in. Explicit
  `mcp__...` names in `allowed_tools` remain strict whitelist entries.
- **Model:** an agent without `model` uses the current session target. An agent
  with `model` replaces it with that complete `<provider>:<model>` catalog target
  ID. `/agent <name>` prints the model target and warns when a switch changes it
  because prompt cache may start cold and increase token usage or cost.
- **Per-agent reasoning:** an agent's optional `reasoning` field pins its thinking
  effort. It overrides the session base effort whenever that agent is selected
  (startup, `/agent`, delegate, or a handoff target) and is then made
  model-compatible and validated like any effort. This lets a cheap implementation
  agent pair a smaller `model` with a lower `reasoning`.
- **Plan → implementation handoff:** the `plan` agent records plans with
  `record_plan` (§9.17) and requests a handoff with `request_implementation` (§9.18).
  At the next prompt boundary, or on manual `/handoff` (§10), the REPL prompts for
  approval, archives the planning transcript via `SaveCompaction`, switches to
  the target agent — default `auto`, overridable by
  `--handoff-agent`/`HARNESS_HANDOFF_AGENT`/`handoff_agent` or `/handoff -a
  <agent>` — optionally swaps the model for a manual `/handoff -m <model>`, then
  reseeds a clean transcript with a pointer to the recorded plan, the displayed
  brief, and any trailing `/handoff` user message as a separate section, clears
  the planning todos, and submits a fixed implementation-start prompt. Reusing
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
  {listen,logFile,logLevel,logFormat,metrics:{enabled,listen}}}`, at
  `$XDG_CONFIG_HOME/harness-mcp-proxy/config.json`
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
- **Prometheus metrics.** HTTP mode enables a separate, unauthenticated
  `GET /metrics` listener at `127.0.0.1:9091` by default. `proxy.metrics` stores
  `enabled`/`listen`; `serve -no-metrics` and `serve -metrics-listen` override it.
  Explicit config/flag addresses make bind failure fatal, while an implicit-default
  collision warns and lets the MCP listener continue. `-no-metrics` passes a nil
  registry into routing, disabling collection as well as exposition. The counters
  `mcp_proxy_requests_total`, `mcp_proxy_errors_total`,
  `mcp_proxy_request_bytes_total`, `mcp_proxy_response_bytes_total`, and
  `mcp_proxy_request_duration_seconds_total` use only `mcp` (downstream server),
  `tool` (bare name), and `key` (stored authorizing key name or `anonymous`).
  Unknown qualified tools omit `mcp`/`tool` rather than parsing the name.
  `mcp_proxy_build_info` is a gauge labeled only by `version`. Request bytes are
  raw arguments and response bytes are marshaled `mcp.CallToolResult`; the outer
  HTTP/JSON-RPC envelope is excluded. Unknown/downstream errors and `IsError`
  results increment errors, except caller cancellation, which still contributes
  requests/bytes/duration. Pre-routing malformed/session/method/auth failures are
  not tool-call metrics. `serve -stdio` neither creates an endpoint nor collects,
  even if metrics config or flags are present.
- **Request logging.** The MCP proxy logs one structured record per routed
  `tools/call` with requester/clientInfo, downstream MCP server name, bare and
  qualified tool name, request/response bytes, duration, `is_error`, and any
  protocol error. Unknown tools are warning records. When a valid W3C
  `traceparent` is present, the log appends `trace_id`, `span_id`,
  `parent_span_id`, and `trace_sampled` (plus `tracestate` when present). The
  proxy carries inbound trace metadata in `context.Context`; downstream HTTP MCP
  servers receive a child `traceparent` with the same trace id, while stdio
  downstream servers have no header channel and are correlated only by proxy logs.
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
- **Auth and security.** The proxy's HTTP listener supports optional API-key
  auth: keys are generated with
  `harness-mcp-proxy generate-api-key [-api-keys-file path] [-ttl 720h] <name>`,
  stored in the dedicated accepted-key file as `{name, hash, added, expires_at?}`
  entries under `api_keys`, and required on every request once any key exists.
  The config may set `proxy.api_keys_file` (relative to the config dir); otherwise
  HTTP mode defaults to `api_keys.json` next to the config/default config dir, and
  `serve -api-keys-file` overrides both. Running HTTP proxies poll only this key
  file and keep the previous good snapshot on reload errors. Inline
  `proxy.api_keys` is rejected with a migration error. Stdio mode remains
  unauthenticated and does not require/read accepted-key files. Clients send
  `Authorization: Bearer <key>`; the proxy verifies it with SHA-256 and
  constant-time comparison. Harness supplies the key via `-mcp-proxy-api-key`,
  `HARNESS_MCP_PROXY_API_KEY`, or the config-file `mcp.api_key` field, with flag >
  env > file precedence; this outgoing client key is loaded at process start and
  is not hot-reloaded. The `harness-mcp-proxy tools` debug command supplies the
  key via `tools -api-key` or `HARNESS_MCP_PROXY_API_KEY`. HTTP downstream servers
  may also set static `headers` and/or `auth`. Static headers are applied first,
  dynamic auth headers next, then MCP
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
`-config`/`-listen`/`-stdio`/`-no-metrics`/`-metrics-listen`/`-log`/
`-log-level`/`-log-format`.

## 15a. LSP code intelligence (optional)

The **LSP manager** (`internal/lspproxy`) launches already-installed language
servers on demand and exposes native LSP tools
(`lsp_definition`, `lsp_references`, `lsp_hover`, `lsp_document_symbols`,
`lsp_workspace_symbols`, `lsp_diagnostics`, and the read-only
`lsp_rename_plan`, plus the mutating `lsp_rename`). The normal path is first-class, not generic MCP:
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
without the `lsp_` prefix) registers only the listed subset of native tools; an
empty or unset list registers all, and unknown entries warn and are ignored. Tool
annotations drive scheduling and agent exposure: read-only LSP tools join the
read-only gate, while `lsp_rename` is mutating.

Serena support is independent of native LSP. `lsp.serena.enable=true` starts a
local stdio MCP child (default `serena start-mcp-server --context=ide
--project-from-cwd --open-web-dashboard False`) and registers its bare downstream tools as
`mcp__serena__<tool>` via `mcptools.RegisterWithOptions(..., Namespace:"serena")`.
Harness trusts Serena `readOnlyHint` annotations; unannotated Serena tools are
treated as mutating. Serena does not require `lsp.enable=true`, `mcp.enable`, or
`mcp.local`, and successful registration adds only a short system-prompt hint,
not Serena hooks.

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
`lsp_rename_plan`/`lsp_rename` additionally **require** `new_name`. `lsp_rename`
applies text edits from `textDocument/rename` only after validating all files and
ranges; unsupported WorkspaceEdit file operations are rejected before any write.
Built-in config uses top-level
`{"lsp":{"servers":{...}}}`; the compatibility `harness lsp serve -config` path
still accepts the legacy `{"version":1,"servers":{...}}` file shape. Both paths
replace embedded defaults (Go/Rust/Python/TS-JS/C-C++) by server name rather than
field-merging them. Per-tool descriptions stay capability-specific and are capped
at 512 bytes; one system-prompt runtime hint lists languages whose configured
server binary is on `PATH`, or reports that none are available.

**v1 non-goals:** completion, formatting, code actions, and WorkspaceEdit file
operations; full-text sync (no incremental); one root per language-server
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
