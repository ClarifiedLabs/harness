# Usage Reference

This page covers everyday behavior that is too detailed for the README:
provider/model selection, interactive initial prompts, one-shot mode, flags, configuration, REPL commands,
agents, sessions, compaction, interrupts, and hooks.

For tool behavior, see [tools.md](tools.md). For MCP and LSP, see
[mcp.md](mcp.md) and [lsp.md](lsp.md).

## Model Selection

`harness` fetches configured model targets from `harness-model-proxy`. A model
value like `openrouter:openai/gpt-5.5` selects that exact proxy target. Model
selection belongs to `harness`: use `-model`, `HARNESS_MODEL`, config `model`,
or `/model` in the REPL. Model values use the proxy's `<provider>:<model>`
target IDs.

Use `harness -model-proxy-url http://host:port` when the proxy runs somewhere
other than `127.0.0.1:8765`. When `-model-proxy-url` /
`HARNESS_MODEL_PROXY_URL` is left unset, harness applies the effective default
`http://127.0.0.1:8765`.

Use `harness --check-model-proxy` to verify that the configured proxy is
reachable. It sends `GET /v1/models`, prints a short success line on stdout, and
exits before creating a session or starting the REPL.

Use `harness --models` to print the targets and whether they support portable
reasoning profiles. Use `harness --agents` to print the resolved built-in and
config-defined agents. Both commands exit before creating a session.

`--models --format json` also shows each target's `api_type`,
`continuation_stateful`, `server_tools`, price, and variant relationship
(`base_target_id` / `variant`). When a target advertises `web_search`,
`-web-search auto` lets harness declare the provider-hosted web search tool for
model calls. The default is `off`.

## Interactive Initial Prompt

Use `-i` or `-initial-prompt` to run one prompt immediately and then continue in
the normal REPL:

```sh
harness -model openai:gpt-5.5 -i "review the current git diff"
```

The initial prompt is literal text: leading `/` is not a REPL command and leading
`!` is not a shell escape. Unlike `-p`, `-i` does not read from stdin or append
piped stdin; any stdin lines remain available as scripted REPL input after the
initial prompt. `-image` attachments used with `-i` attach only to the initial
prompt.

## One-Shot Mode

In one-shot mode (`-p`) the assistant's text goes to stdout while model progress,
model cost checkpoints, tool-call progress, tool summaries, the usage line,
notices, explicitly enabled reasoning summaries, and errors go to stderr. Tool
preamble/commentary messages are assistant text, so they stream with the answer
rather than as bracketed status lines.

When stdout is a terminal, basic Markdown is rendered for readability. With
color enabled, recognized language tags on fenced code blocks also enable syntax
highlighting; untagged, unknown-language, and `text` fences remain plain. The
`-no-color` flag or `NO_COLOR` disables highlighting and all other ANSI styling
while structural Markdown rendering remains readable. Redirected or piped
one-shot stdout stays raw model text. Bracketed status lines are timestamped by
default; disable them when you want untimestamped diagnostics:

```sh
harness -model openai:gpt-5.5 -timestamps=none -p "explain this repo in one paragraph" > answer.txt
```

`-p -`, or piping into stdin, reads the prompt from stdin. With both a flag value
and piped stdin they are concatenated, so this works:

```sh
harness -p "summarize:" < notes.txt
```

At the end of a one-shot run harness prints a `[session summary: …]` cost line to
stderr (cumulative input/cached/output/reasoning tokens and total cost). This
summary bypasses `-q`/`--quiet`, so a quiet one-shot run still reports what it
spent.

Exit codes: `0` completed, `1` runtime error, `2` usage error, `130`
interrupted.

## Flags

```text
-p <prompt|->     one-shot mode; "-" or piped stdin reads the prompt from stdin
-i, -initial-prompt <prompt>   run an initial prompt, then continue in the REPL
-model <provider>:<model>   model proxy target id
-model-proxy-url <url>   harness-model-proxy URL (default http://127.0.0.1:8765)
-model-proxy-api-key <key>   API key for harness-model-proxy (also HARNESS_MODEL_PROXY_API_KEY)
-mcp-proxy-api-key <key>   API key for harness-mcp-proxy (also HARNESS_MCP_PROXY_API_KEY)
-system-prompt <text|@file>    replace the static system prompt
-no-env           omit the environment context block (cwd/os/date/git)
-resume <session-dir>    load a saved session and continue
-session <session-dir>   explicit session directory
-histfile <path>      REPL history file path
-histfilesize <n>     max REPL history entries stored on disk (0 disables, default 1000)
-histsize <n>         max REPL history entries loaded into memory (0 disables, default 1000)
-max-turns <n>    turns per prompt; <=0 means unlimited (default 250)
-tool-timeout <s>   per-tool-call timeout backstop in seconds; <=0 disables (default 600). A
                    hung tool that ignores cancellation is force-failed after this many
                    seconds so it cannot stall a turn; run_command's own timeout_seconds stays
                    authoritative and is never cut below.
-max-prompt-tokens <n>   stop a prompt after this many accumulated tokens; 0 means unlimited
                    (default 0). Counts input + cache-read + cache-write + output + reasoning
                    tokens across every model call in the prompt, and breaks before the next paid
                    request with a `[stopped: prompt token budget N exceeded]` notice.
-max-output-tokens <n> per-turn output cap; 0 uses the automatic cap (default 0)
-max-prompt-cost <usd>   stop a prompt once its accumulated model cost reaches this many USD;
                    0 means unlimited (default 0). Applies only when provider usage reports
                    known cost; breaks before the next paid request with a
                    `[stopped: prompt cost budget $X reached]` notice. Complements -max-prompt-tokens.
-default-context-window <n>   fallback window for configured models without context metadata (default 256000)
-context-window <n>   override the model's context window (tokens)
-reasoning <profile> reasoning profile: default, none, minimal, low, medium, high, xhigh, or max
-reasoning-summary <mode> reasoning summary for Responses API: auto, concise, detailed, or none
-responses-stateful   use CLI-owned provider continuation when the selected target supports it (default true)
-retention-policy <mode>   live transcript retention: auto, age, pressure, or disabled (default auto)
-no-steer         disable in-prompt steering: queue input for the next prompt instead of injecting it before the next turn (default off; see "Steering")
-image-detail <level>   default image detail: auto, low, high, or original
-image <path|detail:path>   attach an image in one-shot mode or to the initial -i prompt; repeatable
-agent <name>     agent: auto (default), explore, plan, independent, or a config-defined agent
-handoff-agent <name>   default implementation agent for plan handoffs (default auto)
-delegate-output <mode> delegate UI: status (default), off, or curated scrolling lines on stderr
-search-tools <mode>   search tools to expose: auto, grep, rg, or both
-web-search <mode>     server-side web search: off or auto (default off)
-trace-proxy      send W3C trace headers to the model and MCP proxies
-v                show tool result snippets (first ~5 lines, dimmed) and tool-call progress details
-tool-stream      show tool-call progress details (default false)
-show-diffs       show per-tool-call file diffs for built-in file edits (default true; use -show-diffs=false to disable);
                  diffs are syntax-highlighted with full-width tinted added/removed line backgrounds when color is on
                  (via background color erase; the tint covers only the text on terminals without BCE)
-q, --quiet       suppress status diagnostics and reasoning output unless -reasoning-summary is set;
                  still prints one per-prompt usage/cost line at an interactive terminal (suppressed only
                  when output is also non-TTY/piped), and one-shot runs always print the session summary
--version        print release version and exit 0
--log-level <level>  diagnostic log level: debug, info, warn, error (also LOG_LEVEL)
-no-color         disable ANSI color (also: NO_COLOR env var; color is TTY-only anyway)
-timestamps <mode>  bracketed status timestamps: short (default), full/long, or none
-no-timestamps   alias for -timestamps=none
-repl-prompt <text>    REPL input prompt format (default "[{agent}] > "; supports placeholders such as {agent}, {model}, and {reasoning})
-repl-edit-mode <mode> REPL prompt edit mode: emacs (default) or vi
--format <text|json>  output format for informational commands (default text)
--show-config    dump the resolved config, including defaults, as JSON and exit
--debug-request  dump the first provider-neutral model request as JSON and exit without calling the model
--agents         list configured agents and exit
--models         list configured providers and models and exit
--check-model-proxy    check harness-model-proxy reachability and exit
-hooks <file>    replace configured hooks with this hook config file
-config <file>    alternate config path
-h, --help        print this usage screen and exit 0
```

`-system-prompt` accepts a `@file` reference. A literal leading `@` is escaped as
`@@`; `@~/path` expands through the current user's home directory. Relative
`@file` references in the config file resolve from the config file directory. It
replaces the built-in static instructions only; runtime sections such as
environment context, user/project `AGENTS.md`, skills, and agent prompts are
still composed around it.

Image attachments accept local PNG, JPEG, WebP, and non-animated GIF files.
Paths must be regular files; directories, devices, and FIFOs are rejected.
Images are embedded in `tree.ndjson` as provider-neutral base64 blocks so resumed
sessions remain self-contained; replay logs show only image metadata. Harness only sends queued
images when the current model explicitly advertises image input support. Manual
provider configs should set `input_modalities`, for example
`["text", "image"]`; models without `image` are treated as text-only and image
attachments are skipped with a warning.

Each image is limited to 10 MiB decoded and to the corresponding base64-encoded
ceiling. A 32 MiB encoded aggregate applies to the complete retained request,
including current and older user images plus images nested in tool results—not
just one prompt or dispatch batch. Persisted and direct-proxy rich blocks are
structurally validated, and malformed or over-budget requests are rejected
before token counting, provider lowering, or network activity.

Typed REPL prompts, initial `-i` prompts, and one-shot `-p` prompts also treat
literal `@path` or `@"path with spaces"` references to supported image files as
image attachments when the model supports images. The reference remains literal
prompt text; harness does not expand file contents or strip the `@...` text.
Pasted and externally edited REPL prompts stay fully literal and do not auto-attach
images from `@` references.

Provider configs can advertise hosted model tools with `server_tools`.
Currently `web_search` is recognized. It can be set at the provider level or on
individual model entries; the proxy also infers it for known web-search-capable
providers such as OpenAI Responses, Anthropic, Sakana, OpenRouter, MiMo, Kimi,
Z.AI, and native Google Interactions. Harness only declares it when
`-web-search auto` (or
`web_search:"auto"`) is enabled and the selected target advertises support.

Provider and model entries can advertise request-level scheduling tiers with
typed `service_tiers` objects. Each object has a target-suffix `id`, optional
`name` and `description`, a bounded `request` mapping, and an optional
tier-specific `price`. For example, OpenAI fast mode is represented as
`{"id":"fast","name":"Fast","request":{"service_tier":"priority"}}`.
Anthropic fast mode instead maps to `request.speed:"fast"` plus its required
`request.betas` feature identifier. A model-level list overrides a provider
list.

Managed models receive these options from models.dev mode metadata; Codex
models receive them from the OpenAI Codex catalog. Harness does not infer tiers
for every model on a provider. Manual compatible endpoints must declare their
options explicitly. The proxy publishes each non-default mode as a separate
model target: `provider:model:fast`, `provider:model:flex`, and so on. Select a
mode with `-model`, `/model`, or an agent's `model` setting. `/fast
[on|off|status]` is a shortcut that switches between the current base target
and its `:fast` sibling; it reports unavailable when no such sibling exists.

The proxy uses a mode-specific catalog price when present. Provider-reported
served tier/speed metadata takes precedence, so a graceful downgrade is charged
at the standard model rate. A selected mode with no accurate catalog price
remains unpriced; token accounting continues normally and reject-unpriced
dollar budgets fail closed.

## Prompt Limits And Lifecycle

The agent loop has several controls against runaway work:

- `-max-turns` limits model turns within one prompt.
- `-max-prompt-tokens` stops before the next paid request once cumulative input,
  cache, output, and reasoning tokens reach the configured budget.
- `-max-prompt-cost` applies the equivalent cumulative USD ceiling when provider
  usage reports a known cost. Unpriced models cannot enforce this limit.
- `-tool-timeout` is a per-tool-call backstop; `run_command`'s own
  `timeout_seconds` remains authoritative.
- Repeated identical tool results and consecutive all-error tool turns are
  steered first and eventually stopped if the model does not change course.

Turn-limit and loop-guard stops make one best-effort tools-disabled request so
the model can finish with a summary. Token and cost budgets stop without another
paid request.

Harness uses these terms consistently:

- A **prompt** is one top-level interaction started by user input.
- A **turn** is one model response plus the complete tool-result batch it
  requested. Turn numbers restart at 1 for each prompt.
- An **attempt** is one provider request for a turn; retries increase the
  attempt number without creating another turn.
- **Maintenance** calls such as compaction, cache prewarming, handoff summaries,
  and optional branch summaries are accounted separately and never increment
  the turn count.

While a prompt runs, status uses `[turn: N … │ prompt …]`; completion uses
`[prompt: N turns …]`. On an interactive TTY, active delegates are included in
that same transient row as `delegate d1 <agent>: <activity>`. Concurrent runs
show the count and most recently active child, for example
`3 delegates · latest d2 plan: tool read_file path="docs/usage.md"`. Background
and nested delegates use the same row while a model, tool, or prompt-work join
wait is active. These rows are process-local display state, not durable output,
and are absent from non-TTY output. The [Flags](#flags)
section lists defaults and config forms; [design section 8.1](design.md#81-prompt-and-turn-loop)
records the exact loop mechanics.

## Configuration And Environment

Precedence is **flags > environment > config file > built-in defaults** for any
setting that has a flag. Settings with no flag use **environment > config file >
default**. This covers the MCP/LSP `enable` and `proxy` keys, global
tool-result caps (`HARNESS_TOOL_RESULT_MAX_BYTES` /
`HARNESS_TOOL_RESULT_MAX_LINES`), and the per-tool caps for `rg`, `grep`, and
`read_file`. A few context-efficiency knobs are config-file-only.

- Environment: `HARNESS_MODEL_PROXY_URL`, `HARNESS_MODEL`,
  `HARNESS_MAX_TURNS`, `HARNESS_MAX_PROMPT_TOKENS`,
  `HARNESS_MAX_OUTPUT_TOKENS`, `HARNESS_TOOL_TIMEOUT`,
  `HARNESS_DEFAULT_CONTEXT_WINDOW`, `HARNESS_TIMESTAMPS`,
  `HARNESS_IMAGE_DETAIL`, and most other `HARNESS_*` equivalents for
  user-facing flags. The convention is `HARNESS_` plus the flag name uppercased
  with dashes turned into underscores. For example, `-context-window`, `-no-env`,
  `-no-color`, `-resume`, and `-session` map to `HARNESS_CONTEXT_WINDOW`,
  `HARNESS_NO_ENV`, `HARNESS_NO_COLOR`, `HARNESS_RESUME`, and
  `HARNESS_SESSION`.
- The `-v` verbose flag uses `HARNESS_VERBOSE`. `--log-level` uses `LOG_LEVEL`.
  `HARNESS_NO_TIMESTAMPS` is an alias for `HARNESS_TIMESTAMPS=none`.
  `HARNESS_REPL_INPUT_TRACE` is a debug knob that appends timestamped
  terminal-input events to the given file path (`-` for stderr).
- `HARNESS_WEB_SEARCH=auto` is equivalent to `-web-search auto`; `off` disables
  it. `auto` only declares web search when the selected model-proxy target
  advertises `server_tools:["web_search"]`.
- Provider API-key environment variables are read only by
  `harness-model-proxy`.
- The optional config file is `~/.config/harness/config.json`, overrideable with
  `-config`. It may set `model_proxy_url`, `model`, `agent`,
  `agents`, `hooks`, `hook_configs`, and flag defaults. See
  `examples/harness/config.json` for a representative schema.
- `--show-config` prints the resolved config as JSON after applying file, env,
  flag, and built-in defaults. It exits without contacting the model proxy.
- `--debug-request` prints the first provider-neutral `llm.Request`, context
  estimate, active tools, reasoning settings, and request byte counts. It
  resolves the model proxy catalog, then exits before prewarm, session hooks,
  session writes, or any model stream.
- `--agents` prints the resolved agent list without contacting the model proxy.
  `--models` prints the configured proxy model catalog. Use `--format json` with
  `--agents`, `--models`, or `--check-model-proxy` for structured output.
- Context-efficiency knobs are config-file-only except where noted:
  `agents_md_warn_bytes`, `compact_keep_turns`, `compact_keep_tokens`,
  `compact_auto_enabled`, `compact_trigger_percent`,
  `compact_target_percent`, `compact_idle_after_seconds`,
  `compact_idle_trigger_percent`, `compact_summary_max_tokens`, and
  `compact_tool_result_max_bytes`.
  Tool-result truncation is controlled by config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. `rg` and `grep` default to 32 KB / 500 lines
  and can be overridden with `rg_result_max_bytes`, `rg_result_max_lines`,
  `grep_result_max_bytes`, and `grep_result_max_lines`, or the matching
  `HARNESS_*` env vars. `read_file` defaults to 500 lines and a 32 KB result cap;
  configure `read_file_default_limit`, `read_file_result_max_bytes`, and
  `read_file_result_max_lines`, or matching `HARNESS_*` env vars. The delegate
  tool also has config-file-only `delegate_max_turns` (maximum per-child
  tool-enabled loop budget)
  and `delegate_max_depth` (recursive depth cap, root depth `0`).
  `delegate_output` / `HARNESS_DELEGATE_OUTPUT` / `-delegate-output` accepts
  `status` (the default one-row TTY display), `off` (no delegate-specific UI),
  and `lines` (the status row on a TTY plus curated scrolling child activity on
  stderr). On non-TTY output, `status` is silent while `lines` still writes
  stderr. Quiet mode is authoritative and suppresses both forms, including
  child reasoning summaries when `-reasoning-summary` was explicitly enabled.
  `-v` and `-tool-stream` do not enable or expand child lines.

  | mode | stdout TTY | stdout non-TTY |
  |---|---|---|
  | `status` | compact delegate status row | no delegate output |
  | `off` | no delegate-specific output | no delegate output |
  | `lines` | compact status row plus scrolling stderr lines | scrolling stderr lines |

  Inline lines use stable, ANSI-free prefixes:

  ```text
  [delegate d1 explore] assistant: Checking the call path.
  [delegate d2 plan depth=2] tool read_file path="docs/design.md" started
  [delegate d1 explore] completed · 3 turns
  ```

  They never write stdout or enter the parent transcript/model context. Harness
  emits only bounded, sanitized lifecycle, assistant, allowlisted tool/notice,
  structured retry, and permitted reasoning-summary fields; it omits tasks,
  raw tool results, error/provider text, commands, URLs, request IDs, and opaque
  reasoning. An incomplete parent plain/Markdown line delays delivery until a
  natural line boundary rather than changing parent stdout. The process-local
  feed retains at most 512 events / 256 KiB and reports loss as
  `[delegate output] omitted N events`. Use
  `harness session replay --follow <child-session>` for complete durable child
  output.
- Tool-surface limits for MCP and LSP are config-file-only: `mcp.max_tools` caps
  how many discovered remote MCP tools are auto-exposed (`0` = unlimited),
  `mcp.disabled_servers` is a list of remote MCP server names dropped from
  auto-exposure, and `lsp.tools` registers only the listed subset of LSP tools
  (empty = all). See [mcp.md](mcp.md) and [lsp.md](lsp.md). An explicit
  `allowed_tools` whitelist can still name a tool that auto-exposure excluded.
- Serena can be launched independently with `lsp.serena.enable=true` or
  `HARNESS_LSP_SERENA_ENABLE=true`; this does not imply `lsp.enable=true`.
- A single turn's output is capped at the configured
  `max_output_tokens` value when set, otherwise at one quarter of the effective
  `context_window` (with a very high 1,000,000-token runaway ceiling). A model's
  configured `output_limit`, when known, is a ceiling rather than the default.
  The chosen cap is then clamped to the counted or estimated remaining context
  window. This client-side runaway brake is distinct from `-max-prompt-tokens`,
  which is the cumulative per-prompt token *budget* across all model calls. If a
  provider reports a smaller real context window in an overflow error, harness
  learns that window for the session and retries once. When the cap is reached,
  harness surfaces `[stopped: model reached max tokens]`. Provider configs may
  set `min_output_tokens` for endpoints that reject very small output caps. If
  an OpenAI-compatible endpoint rejects a request with a parseable minimum
  `max_tokens` error, the model proxy retries once at the inferred floor and logs
  the inferred/configured values. First-party Chat Completions requests to
  `api.openai.com` encode the cap as `max_completion_tokens`; custom and
  compatible endpoints continue to use `max_tokens`.
- Before normal model requests, harness resolves input tokens in tiers:
  provider-specific count APIs for OpenAI Responses and Anthropic Messages when
  available through `harness-model-proxy`; a local `o200k_base` BPE estimate for
  OpenAI/OpenRouter Chat Completions; then the coarse byte-based heuristic.
  Once a turn reports real usage, the *reported* context (`turn_attempt_start`
  and `/context`) anchors to that actual input (uncached plus cache read/write)
  plus a local estimate of only the messages appended since — count APIs can
  systematically miss provider-billed opaque replay state such as thinking
  signatures. Compaction or retention resets the anchor until the next measured
  turn. In the byte heuristic, opaque payloads (thinking signatures,
  encrypted/redacted reasoning, interaction steps) are weighted separately from
  prose (8 vs 4 bytes/token) because base64-style blobs tokenize much coarser.

Harness automatically adds static AGENTS instructions from
`~/.agents/AGENTS.md`, then from `AGENTS.md` in the current working directory.
Missing files are ignored; unreadable existing files fail startup.

Reasoning selection is opt-in via `reasoning` / `HARNESS_REASONING` /
`-reasoning`. Accepted profiles are `default`, `none`, `minimal`, `low`,
`medium`, `high`, `xhigh`, and `max`. The model proxy maps those profiles to the
closest provider/model-specific effort value or, for budget-token models, to a
percentage of the supported maximum budget. Responses API reasoning summaries
are controlled separately by `reasoning_summary` /
`HARNESS_REASONING_SUMMARY` / `-reasoning-summary`; they default off and are
displayed only when explicitly enabled. `-q` disables reasoning summary output
unless `-reasoning-summary` is explicitly set on the CLI.

Responses continuation is on by default for proxy providers that report both
provider continuation support and `continuation_stateful:true` in the proxy
catalog. This includes supported Responses and Gemini Interactions targets.
Disable it with
`responses_stateful:false`, `HARNESS_RESPONSES_STATEFUL=false`, or
`-responses-stateful=false`. Harness owns the response/interaction ID, the
anchored message count, and a SHA-256 fingerprint of the exact provider-neutral
transcript prefix represented by that ID. It persists this state with the
session and sends only the appended suffix on the next request. A missing,
malformed, out-of-range, or fingerprint-mismatched anchor is discarded before
the request and the complete transcript is sent safely.

Live transcript retention defaults to `auto`, which uses pressure-triggered
epochs for both stateful and stateless providers. Experiments can override this
with `retention_policy`, `HARNESS_RETENTION_POLICY`, or `-retention-policy`;
accepted values are `auto`, `age`, `pressure`, and `disabled`. Disabling live
retention does not disable compaction or provider-overflow recovery.

The model proxy is a stateless continuation pass-through: it never stores,
reconstructs, trims, resets, or retries a continuation. It forwards the
CLI-supplied messages, `store`, and previous-response/interaction ID unchanged.
An unsupported target rejects continuation with
`continuation_unsupported`. A connection-affine WebSocket provider can reject a
known-unavailable ID before streaming with
`previous_response_unavailable`; an upstream previous-ID rejection is also
passed through unchanged. In either case harness clears its local anchor and
performs one full-context resend. A miss that recovers is retained in proxy
logs, metrics, and session diagnostics but is not rendered as a terminal model
error; the normal state-reset notice remains visible unless quiet mode suppresses
it. If the resend fails, harness still renders the final error normally.

Retention, compaction, branch navigation, agent changes, and true base-model
changes reset the CLI-owned anchor. Switching a target between its base and
`/fast` variant preserves a valid anchor because both variants share the same
provider/model continuation chain. `-responses-stateful=false` sends complete
messages without `store` or a previous ID on every request.

Responses provider configs may also set `responses_websocket:true` to have the
model proxy use the Responses WebSocket transport instead of HTTP SSE. The proxy
defaults this on for `codex_oauth` Responses providers and preserves an explicit
`responses_websocket:false` override. WebSocket connections live in a bounded
per-pod lease pool. Harness sends its opaque session ID as
`X-Harness-Session`, allowing a load balancer to keep that connection-affine
traffic on one pod as a performance optimization. Correctness does not depend
on stickiness: a pod/socket miss produces the reset-and-resend path above.

Gemini Interactions targets are continuation-capable by default; set
`interactions_stateful:false` in the provider config to make the catalog reject
continuation for that target. The CLI owns and persists
`previous_interaction_id` under the same fingerprint contract. Signed `thought`,
Google Search call, and Google Search result steps remain in the invisible
provider-neutral transcript so a full-history fallback is valid after a proxy
restart. Stored interactions remain subject to the provider account's
applicable retention policy.

## Model Proxy

### Setup

Run `harness-model-proxy setup` to create a proxy config and a provider config
from models.dev, append a new provider config to an existing proxy config, or
update an existing configured provider without configuring a proxy default model.
Setup lists harness-supported providers, prompts for the API key when the
provider needs one, then lets you choose which provider models are available
locally. If models.dev omits a provider API URL, setup can still derive
first-party OpenAI and Anthropic defaults from exact `@ai-sdk/openai` and
`@ai-sdk/anthropic` package metadata, and maps plain `@ai-sdk/google` to
the native Gemini Interactions endpoint. Managed Google configs use
`api_type:"interactions"`, explicitly default `interactions_stateful:true`, and
advertise `web_search`; Vertex Google package variants are not auto-configured.
Anthropic `base_url` values are versioned API prefixes (normally ending in
`/v1`); the dialect appends `/messages` or `/messages/count_tokens`.

The special `openai-codex` provider uses ChatGPT subscription auth instead of an
API key, exposes models from the OpenAI Codex catalog, and reports token usage
without dollar pricing. It omits Responses `max_output_tokens` because the Codex
backend rejects that parameter. Input-token preflight counts use a local
`o200k_base` estimate because the Codex CLI protocol does not expose a separate
count-token endpoint. The proxy also uses the Responses WebSocket transport by
default for this provider, matching Codex's stateful continuation path while
sending `store:false` to the ChatGPT backend. Managed setup explicitly emits the
hashed conversation cache key as `prompt_cache_key`. Its startup warm-up uses
WebSocket `generate:false`; only that transport returns an explicit transcript
anchor at message zero, which the owner goroutine installs if the model,
session, transcript, and continuation generation are still unchanged. HTTP
warm-up usage is retained but its disposable response ID is never installed.
After setup, run:

```sh
harness-model-proxy auth login openai-codex
```

The `kimi-for-coding` provider is deliberately configured with
`api_type:"openai"` even though models.dev lists the Anthropic SDK package.
Kimi for Coding serves both protocols off one base URL (OpenAI at
`/coding/v1/chat/completions`, Anthropic at `/coding/v1/messages`; see Kimi's
service-endpoint docs), and the OpenAI chat-completions shape replays preserved
thinking as compact `reasoning_content` — the sanctioned mechanism, matching
Kimi's own CLI (`packages/kosong/src/providers/kimi.ts`) and Kimi's Preserved
Thinking guide — instead of the Anthropic shape's much larger signed thinking
blobs. Managed setup and `refresh-models` therefore also write
`reasoning_replay:true` for this provider so historical assistant reasoning
round-trips (see the `reasoning_replay` provider quirk above). Do not disable
reasoning replay for kimi-k3: Kimi's docs make preserved thinking mandatory in
tool-call loops.

Provider configs accept an optional `auth` block in place of `api_key` /
`api_key_env`; when `auth` is present, API-key fields are ignored and there is no
fallback if auth fails. Supported auth shapes include `token_command`, `oauth2`,
and `codex_oauth`. Codex terminal refresh failures are cached only in the
failing process and keyed by a digest of the rejected refresh token; they are
never written into the shared token file. A replica rereads the file before
returning that cached failure and immediately after a terminal refresh response,
so it can adopt a valid token rotated by a peer without overwriting it.

Provider configs may also set `prompt_cache` to control how the stable harness
conversation cache key is sent to OpenAI-compatible backends. This cache
affinity survives continuation resets such as compaction and model/agent/tool
changes, is shared with delegates, and rotates for a new logical session.
`key_field` accepts
`auto` (default), `none`, `prompt_cache_key`, or `session_id`; `auto` sends
`prompt_cache_key` to first-party OpenAI and ChatGPT Codex endpoints,
`session_id` to OpenRouter, and omits cache key fields for other custom base
URLs. `affinity_headers` can
copy the same key into non-auth routing headers such as `x-session-id`. The
proxy derives the provider-facing value as a SHA-256 hash of harness's local
cache-affinity key, so providers do not receive the raw identifier.

Provider configs may set `reasoning_replay` to control how much historical
reasoning state a dialect replays on the wire. The default leaves each
dialect's behavior unchanged: the OpenAI chat-completions dialect replays
nothing (strict OpenAI-compatible servers reject unknown fields), and the
Anthropic dialect replays all signed thinking blocks verbatim. Set
`reasoning_replay:true` (or `"full"`) on an OpenAI-dialect provider whose
endpoint requires preserved thinking in multi-turn tool loops — such as Kimi
for Coding — to replay persisted assistant reasoning as `reasoning_content` on
later requests. Replay is gated on reasoning being enabled for the request, so
compaction summaries and prewarm requests stay clean. With replay enabled, the
dialect tags streamed `reasoning_content` for persistence (as thinking blocks
with no signature); without it the text remains display-only. `"current_turn"`
is an Anthropic-dialect-only reduction mode documented under reasoning replay
in the context-efficiency section.

Anthropic-dialect provider configs may set `usage_input_includes_cache:true`
when the endpoint reports `input_tokens` as TOTAL input (cached tokens
included) instead of real Anthropic's uncached-only figure — observed on
Anthropic-compatible third-party routes. The dialect then subtracts cache
read/write tokens during usage normalization so `harness session stats`
"uncached input" and cost accounting keep the uncached-input contract instead
of double-counting cache reads. The quirk is explicit config only; harness
does not sniff hosts. Leave it off for api.anthropic.com.

For Anthropic, harness also declares cache semantics directly on each neutral
request. Interactive turns request a one-hour TTL only for stable system/tool
anchors; one-shot, delegate, prewarm, compaction, handoff, and branch-summary
requests use the default five-minute TTL. Message breakpoints always use the
provider's default five-minute TTL. Harness computes a leading message prefix
that future retention cannot rewrite, and the Anthropic dialect places one
message breakpoint there plus a rolling tail breakpoint within the four-anchor
limit. Volatile request-only context (todo reminders, hook output, background
notices) rides a trailing user-role message appended after the breakpoints are
placed — never the system head — so its appearance or change does not
invalidate the cached prefix. Maintenance requests derive deterministic, purpose-separated proxy and
cache IDs from the owning session, so they do not reuse or compete with the main
conversation's connection/continuation chain.

For hand-written model-proxy config shape references, see
`examples/harness-model-proxy/config.json` and
`examples/harness-model-proxy/providers.json`. Setup remains the recommended way
to create real provider allowlists. Manual model entries must declare supported
input modalities with `input_modalities`; use `["text"]` for text-only models
and `["text", "image"]` for models that accept image attachments.

Provider config files written by `setup` and `refresh-models` are managed
(`"managed": true`) and store no per-model prices. The proxy resolves their
prices and input modalities from the models.dev cache, so a cache refresh updates
served metadata without re-running setup or restarting. Providers or models no
longer present in that cache are warned about and removed from the live catalog.
A hand-written config without `"managed": true` is manual: the proxy does not
edit it and uses its own `price` and `input_modalities` entries. A managed config
may set `price_source` to resolve metadata from a different models.dev provider
ID.

`harness-model-proxy` stores every models.dev provider and model, projected to
the metadata harness consumes, at
`~/.config/harness-model-proxy/models.dev.api.json`. `setup` uses the cache
when present; if there is no cache, or the cache cannot be parsed, it fetches and
rewrites the cache before using the vendored fallback snapshot. While serving,
the proxy refreshes this cache when it is older than `24h` by default. Set
`models_dev_cache_ttl` in the proxy config, or pass
`-models-dev-cache-ttl <duration>`, to override the interval; use `0` to disable
periodic serving-time refreshes. Cache updates are parsed and sanity-checked
before replacing the old file; a candidate catalog with no providers/models, or
one whose provider/model counts swing by more than 4x with a meaningful absolute
delta, is rejected and the old cache is preserved. Successful refreshes prune
unused upstream fields before saving the previous cache to
`models.dev.api.json.bak` and replacing the active cache; that single backup is
overwritten each time.

Run `harness-model-proxy refresh-models` to fetch and cache the latest live
`models.dev` catalog, then refresh metadata for the currently configured model
allowlists while preserving stored API keys. If live fetch fails, refresh uses a
parseable local cache before falling back to the vendored snapshot. Configured
providers or models that no longer exist in the catalog (or providers harness can
no longer support) are reported with a warning and removed rather than failing the
refresh: a missing model is dropped, and a provider that loses all its models is
deleted along with its `provider_configs` reference.

### Serving, probes, and rolling updates

The model proxy exposes unauthenticated process probes on its API listener,
outside API-key middleware:

- `GET /readyz` returns `200` normally and `503` as soon as SIGTERM or SIGINT
  starts a drain.
- `GET /healthz` remains `200` until final teardown begins.
- Other methods on either probe path return `405`.

The first termination signal removes readiness, stops background catalog/key
refresh work, waits for load-balancer propagation, and then gracefully closes
the API listener without cancelling in-flight handler contexts. Once the stream
drain reaches its bound, the server force-closes remaining requests. It then
closes the bounded WebSocket pool and shuts down the metrics listener last.

Lifecycle settings use flag > environment > config > default precedence:

| Purpose | Serve flag | Environment | Config | Default |
|---|---|---|---|---|
| readiness propagation delay | `-drain-delay` | `HARNESS_MODEL_PROXY_DRAIN_DELAY` | `drain_delay` | `5s` |
| maximum stream drain | `-shutdown-timeout` | `HARNESS_MODEL_PROXY_SHUTDOWN_TIMEOUT` | `shutdown_timeout` | `5m` |
| process identity | `-instance-id` | `HARNESS_MODEL_PROXY_INSTANCE_ID` | `instance_id` | random 16-byte hex |

Instance IDs must match `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`. A Kubernetes pod
name or UID is a useful value. It appears in request events, error diagnostics,
`/v1/usage`, and structured logs; correlate a request by
`(proxy_instance_id, proxy_request_id)`.

A minimal Kubernetes fragment is:

```yaml
spec:
  terminationGracePeriodSeconds: 330
  containers:
    - name: model-proxy
      args:
        - serve
        - -listen=0.0.0.0:8765
        - -metrics-listen=0.0.0.0:9090
      env:
        - name: HARNESS_MODEL_PROXY_INSTANCE_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
      readinessProbe:
        httpGet: {path: /readyz, port: 8765}
      livenessProbe:
        httpGet: {path: /healthz, port: 8765}
```

Set `terminationGracePeriodSeconds` greater than the drain delay plus shutdown
timeout; use at least `330` seconds with the defaults. The metrics default
(`127.0.0.1:9090`) is not pod-scrapable, so bind
`-metrics-listen 0.0.0.0:9090` in a pod.

Harness sends `X-Harness-Session` only on stream requests with a session ID.
Use it for consistent hashing when Codex Responses WebSockets are enabled:

```nginx
upstream harness_model_proxy {
    hash $http_x_harness_session consistent;
    server model-proxy-0:8765;
    server model-proxy-1:8765;
}
```

```haproxy
backend harness_model_proxy
  balance hdr(X-Harness-Session)
  hash-type consistent
  server proxy0 model-proxy-0:8765 check
  server proxy1 model-proxy-1:8765 check
```

For Envoy, use a `RING_HASH` cluster and a route header hash policy:

```yaml
route:
  cluster: harness_model_proxy
  hash_policy:
    - header:
        header_name: X-Harness-Session
clusters:
  - name: harness_model_proxy
    lb_policy: RING_HASH
```

See the official [NGINX upstream hash](https://nginx.org/en/docs/http/ngx_http_upstream_module.html#hash),
[HAProxy balancing](https://www.haproxy.com/documentation/haproxy-configuration-manual/latest/#4-balance),
and [Envoy route hash-policy](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/route/v3/route_components.proto#config-route-v3-routeaction-hashpolicy-header)
references for the complete surrounding configuration.

Stickiness improves WebSocket continuation hit rate but is never required for
correctness. HTTP stored continuations work on any replica; a Codex
`store:false` socket miss returns 409 and the CLI resends complete history once.

For a replicated production deployment, bake `models.dev.api.json` into the
image or mount an identical read-only copy in every pod. Set
`models_dev_cache_ttl: 0` and treat a catalog change as a deployment. Cost
budgets and `/v1/usage` remain deliberately per pod: strict budget enforcement
requires one replica, and sharing one budget-state directory between independent
replicas is unsupported because their read-modify-write cycles are not
coordinated. `/v1/usage` includes `instance` and `since` so that per-pod reports
are explicit.

The continuation ownership change is a coordinated cutover: old and new CLI/
proxy combinations are not wire-compatible. Finish active CLI turns, stop the
CLIs, deploy the complete new proxy fleet without sending traffic through a
mixed-version Service, wait for every pod to become ready, and only then
start/update the CLI. Validate one HTTP stateful session and one Codex
WebSocket session through a forced pod replacement. Future proxy-only rollouts
can use the normal readiness-driven rolling strategy.

### Harness-to-proxy authentication

Model-proxy API-key authentication is disabled by default and becomes required
as soon as the first key is stored in the proxy's dedicated API-key file. The
default is `api_keys.json` next to the proxy config; `api_keys_file` selects
another path and `serve -api-keys-file path` overrides it. Inline `api_keys` in
the normal proxy config are rejected.

Generate and store a key, then provide it to harness:

```sh
harness-model-proxy generate-api-key [-api-keys-file path] [-ttl 720h] [-budget-usd 25 -budget-period 24h] laptop
harness --model-proxy-api-key <key> -model <provider>:<model>
```

Harness also reads `HARNESS_MODEL_PROXY_API_KEY` and the `model_proxy_api_key`
field in `~/.config/harness/config.json`. Model-proxy keys have the `hmp_`
prefix. Only SHA-256 hashes are stored, and the plaintext key is printed once.
Omit `-ttl` (or use `0`) for a non-expiring key. A running proxy polls its key
file for additions and removals; harness loads its outgoing key at process start.

See [mcp.md](mcp.md#proxy-api-key-authentication) for the equivalent MCP proxy
configuration.

### Usage, pricing, and budgets

The read-only `GET /v1/usage` endpoint aggregates token and cost totals per model
target for the serving process, including delegate child-agent spend. Its
top-level `instance` and `since` fields identify that per-pod lifetime.
`GET /v1/models` includes complete
static pricing schedules, including context-length tiers, plus `source_date` and
`max_age_seconds` fields for detecting stale catalog prices. Managed-provider
`source_date` values track the models.dev cache; manual-only setups use the
provider config file modification time. Setup and runtime model pickers display
each price band as input/output USD per million tokens.

Cost budgets are attached to model-proxy API keys:

```sh
harness-model-proxy generate-api-key -budget-usd 25 -budget-period 24h laptop
```

New authenticated streams are rejected with HTTP 429 after the key's recorded
known-cost spend reaches its fixed-window limit. Spend persists under the proxy
config directory across restarts. Unpriced targets are allowed by default and do
not count toward the budget; add `-budget-reject-unpriced` to reject them.
`/v1/usage` includes the authenticated key's current budget state when it has a
budget. Both usage and budget enforcement are per pod; use one replica when a
strict global limit is required.

For first-party Google Interactions targets, thought tokens are billed at the
model's output-token rate when the catalog does not provide a separate reasoning
rate. Google Search per-query grounding fees are not included in `cost_usd` or
cost-budget spend; those figures include the request's token charges only.

Responses and Chat Completions report reasoning as a breakdown of their aggregate
output/completion tokens. Harness reports the non-reasoning remainder as output
and the breakdown as reasoning so displayed totals and budgets do not double
count it. When a Responses or Chat catalog price omits a reasoning rate, harness
uses that schedule's output rate; explicit base, context-tier, and service-tier
reasoning rates remain authoritative.

Anthropic Messages reports thinking tokens inside its aggregate output count.
Harness similarly exposes a non-reasoning output remainder plus a disjoint
reasoning bucket and prices reasoning at the output rate when the catalog has no
explicit rate. Anthropic cache writes are split into default 5-minute and
1-hour buckets; the latter uses the configured rate or the documented
`2 × input` fallback. A long-TTL response without the required breakdown is
reported as unpriced instead of using a cheaper estimate. Anthropic hosted
web-search request fees are excluded from `cost_usd` and cost-budget spend; its
token charges remain included.

### Prometheus metrics

The proxy exposes unauthenticated Prometheus metrics on a separate listener,
`127.0.0.1:9090` by default. Metrics break usage down by `provider`, `model`,
bounded `purpose` (`turn`, `compaction`, `prewarm`, `handoff_summary`,
`branch_summary`, or
`unknown`), and `key` (the API key's stored name, or `anonymous` when
authentication is disabled). `model_proxy_build_info` carries the build version.
Token counters are recorded for every stream that produced usage, priced or not, while
`model_proxy_cost_usd_total` is recorded only when a price is known.
`model_proxy_cache_write_tokens_total` records default-rate writes and
`model_proxy_cache_write_1h_tokens_total` records Anthropic's 1-hour writes.

Continuation and transport health use bounded, proxy-observable families:

- `model_proxy_continuation_total{result=...}` records exactly one of
  `not_offered`, `served`, `unavailable`, `rejected_upstream`, or `failed` per
  stream request.
- `model_proxy_ws_pool_events_total{event=...}` records `hit`, `miss`, `create`,
  `evict_lru`, `evict_idle`, `evict_age`, or `overflow`.
- `model_proxy_ws_pool_connections` and
  `model_proxy_ws_pool_capacity` expose current pooled connections and the
  configured bound.

These families have no API-key or instance label. Prometheus scrape-target
labels identify replicas, and all existing request/usage plus new operational
counters can be summed across targets without double counting client-side
retries. CLI-only resets remain in session diagnostics rather than being
reported back to the proxy.

Use `-no-metrics` to disable the endpoint or `-metrics-listen` to move it. The
equivalent proxy-config `metrics` object accepts `enabled` and `listen`. The
listener has an explicit lifetime and remains available until API draining,
handler teardown, and connection-pool closure have completed.

### Provider failures and retries

Harness retries transient connection failures and retryable provider responses
such as 429, 500, 502, 503, and 529. A `Retry-After` value or equivalent
streaming error hint is honored when it is at most 60 seconds. Longer 429/529
waits fail immediately with the original provider message so an interactive
prompt is not silently parked for minutes or hours.

Every unsuccessful upstream attempt is logged by the model proxy, including
attempts followed by a successful retry. Session-side lifecycle records are
described under [Session diagnostics](#session-diagnostics); the exact backoff,
stream-retry, and cancellation rules are in
[design section 5.5](design.md#55-errors-and-retries-internalretry).

### Proxy request tracing

Enable opt-in tracing to correlate a harness run across model and MCP proxy logs:

```sh
harness -trace-proxy -model <provider>:<model>
```

Harness sends standard W3C `traceparent` headers. Proxy logs that receive a valid
trace include `trace_id`, `span_id`, `parent_span_id`, and `trace_sampled` fields.
Tracing does not log prompts, request bodies, API keys, or authentication
headers.

### Multimodal tool-result compatibility diagnostics

Image-bearing tool results have three separate compatibility layers:

1. **Catalog modality:** the selected target must advertise `image` input.
   Harness rejects a statically image-requiring tool before it reads the file
   when this capability is absent.
2. **Configured dialect:** the provider config's `api_type` selects the wire
   lowering. Anthropic nests images in `tool_result.content`; OpenAI Chat emits
   tool messages followed by one adjacent multimodal user message; Responses
   emits function outputs followed by one adjacent user image item; Gemini
   Interactions emits `function_result.result` text/image content.
3. **Concrete endpoint conformance:** an OpenAI-compatible endpoint can reject a
   valid dialect shape despite catalog metadata. On the final non-retryable,
   targeted rejection, after normal continuation/server-tool/output-floor
   fallbacks, the proxy attaches the structured category
   `multimodal_tool_result_rejected`.

Harness shows one concise compatibility notice with the target, remediation,
proxy request ID, and trace ID when available. It also writes a structured
warning to the session's `diagnostics.ndjson` with prompt/turn/attempt,
sanitized upstream status/code/message, correlation fields, lowering strategy,
and bounded shape metadata. The ordinary error remains available. For streaming
requests the proxy's outer HTTP response can be `200` while the diagnostic's
`api_status_code` records the upstream provider failure.
`--quiet` suppresses the compatibility notice (and no verbose duplicate is
printed), while session diagnostics still receive exactly one structured
record when enabled.

Diagnostics include image counts, MIME types, dimensions, encoded/decoded byte
totals, and deterministic SHA-256 fingerprints. They never include prompts,
tool arguments, result text, local paths, data URLs, or image base64. The same
concise notice is stored as a normal `raw.ndjson` replay event. Use
`-trace-proxy` to correlate its `trace_id` with model-proxy logs.

This classification is observational only: Harness does not silently drop the
image, resend altered text-only content, switch serializers, mutate target
metadata, or learn a persistent endpoint quirk. Select a conforming image target
or inspect the image outside that model call.

## REPL Commands

Lines starting with `/` are commands; `//` sends a literal leading slash. At an
interactive TTY prompt, lines starting with `!` run a local shell command and
return to the prompt without contacting the model; `!!` sends a literal leading
`!`. In one-shot mode, initial `-i` prompts, non-TTY/scripted input, pasted text, and edited prompts,
`!text` is literal prompt text. In a normal typed prompt, `$name` mentions the
named skill anywhere in the text; the model receives request-only context
telling it to read that skill's `SKILL.md` before acting. `$$` escapes a
literal `$`.

In terminals that support bracketed paste, pasted text fills the prompt for
review and is submitted as one literal prompt when you press Enter, preserving
embedded newlines. Each paste event is classified independently after newline
normalization: a range of at most 1,000 normalized UTF-8 bytes renders inline,
including multiline content, while a range over 1,000 bytes shows a one-line
`[N bytes of pasted content]` placeholder wherever it occurs in the prompt. The
full content is retained and submitted in either case. A collapsed placeholder
persists while you edit surrounding text and acts as one unit for cursor movement
and deletion. Press Ctrl-G / `/edit` to open the external editor with the full
expanded content; text returned from the editor remains expanded in the prompt.
A paste that fills an empty prompt is submitted literally — pasted `/commands`
are not executed, `!command` is not a shell escape, and `$skill` is not resolved.
This holds on the Enter path in every edit mode, including the vi normal-mode
Enter after Esc. Typing anything after a paste (in emacs mode, or after entering
vi normal mode with Esc) makes the whole line typed (so `!`/`/`/`$` apply).
Bracketed paste is classified once its explicit end marker arrives. In terminals
that do not support bracketed paste, harness falls back to detecting a fast paste
burst so newlines in a paste do not submit prematurely; that incremental fallback
may transition a range from inline to collapsed when it crosses 1,000 bytes. Set
`HARNESS_REPL_PASTE_HEURISTIC=off` to disable the fallback. For non-interactive
large input, prefer `-p -` or piped stdin.

At an interactive terminal, the prompt supports basic line editing. Shift-Enter
inserts a newline without submitting. Press Ctrl-G at the prompt, or run
`/edit [draft]`, to open an external editor for a multi-line prompt. Harness
uses `$VISUAL`, then `$EDITOR`, then `vi`. In normal prompt text, Tab completes
literal `@path` file references; paths containing whitespace, quotes, or
backslashes are inserted as `@"..."`. These references remain prompt text and do
not expand file contents. Supported image references auto-attach as images using
the default image detail when the model supports images. On `!` command lines,
Tab completes the first word from `PATH` and completes path words with `/`, `~/`,
`./`, `../`, and nested relative path prefixes.

At the idle main interactive TTY prompt only, Shift-Tab cycles the configured
agents by canonical sorted name, wrapping at the end, without changing the
editable draft. It works in emacs mode and in both vi insert and normal modes;
ordinary Tab keeps the completion behavior above.

The editor preamble contains only the latest completed conversational turn from
the replay log (assistant output, that turn's tool activity/notices, and its
`[turn: …]` completion line). It excludes the user prompt, earlier turns, attempt
accounting, maintenance calls, and the aggregate `[prompt: …]` usage line.

| command | effect |
|---|---|
| `/help` | list commands |
| `/exit`, `/quit` | save, print a session token summary, and exit |
| `/clear` | echo the discarded session token/cost totals, then reset the conversation and rotate to a fresh session directory |
| `/compact` | force compaction now |
| `/tree [entry]` | browse the conversation tree and branch in place; selecting a user prompt rewinds to its parent and pre-fills that prompt for editing |
| `/fork [entry]` | select a prior user prompt and branch before it into a new session with fresh usage accounting |
| `/clone` | copy the current branch into a new session with fresh usage accounting |
| `/context` | dump the current provider-neutral model context as JSON |
| `/context <file>` | save the current provider-neutral model context as JSON |
| `/usage` | cumulative input, cached input, output, reasoning tokens, and cost |
| `/tools` | list enabled built-in and MCP tools with descriptions, plus disabled optional tools |
| `/image` | list images queued for the next prompt |
| `/image <path>` | attach an image to the next prompt |
| `/image --detail <level> <path>` | attach an image with per-image detail |
| `/image --clear` | clear queued images |
| `/edit [draft]` | open an external editor for the next prompt |
| `/save [file]` | force save, optionally elsewhere |
| `/model` | choose a configured provider/model; the picker shows input/output USD per 1M tokens for every static context-price band; interactive runs can optionally save it as the default |
| `/model <id>` | switch subsequent turns to model `<id>`; a near-miss falls back to a unique prefix/substring match |
| `/model <provider>:<id>` | switch to `<id>` on a specific configured provider |
| `/reasoning` | list reasoning controls for the current model |
| `/reasoning <profile>` | switch reasoning profile for subsequent turns |
| `/reasoning summary <auto\|concise\|detailed\|none>` | switch Responses API reasoning summaries for subsequent turns |
| `/effort [profile]` | alias for `/reasoning [profile]` |
| `/fast [on\|off\|status]` | toggle or inspect the current model's `:fast` sibling; reports unavailable when the model has none |
| `/agent` | list agents and descriptions, marking the current one |
| `/agent <name>` | switch the active agent |
| `/mode`, `/mode <name>` | alias for `/agent` |
| `/plan` | alias for `/agent plan` |
| `/auto` | alias for `/agent auto` |
| `/handoff [-a agent] [-m model] [message]` | review the recorded plan and displayed handoff brief, then after approval switch to an implementation agent, apply optional agent/model overrides and user guidance, and start the implementation turn |
| `/background` | list background jobs |
| `/background <id>` | show a background job's status, result, and transcript path |
| `/background cancel <id>` | cancel a running background job |
| `/skills` | list available skills |
| `/vi on\|off` | enable or disable vi-style prompt editing (persisted as the default) |
| `!command` | run a local shell command at an interactive TTY prompt |

Anthropic extended thinking and 1-hour prompt-cache writes appear as separate
reasoning and `cache write (1h)` usage buckets. They remain disjoint from output
and default-rate cache writes in session totals and token budgets.

An unknown `/command` prints a `did you mean <command>?` suggestion (nearest known
command by edit distance) instead of failing silently. The per-prompt usage line
appends cache-read and reasoning token counts (with the cache-hit ratio) when they
are non-zero, and a model with no configured price prints a one-time
`[note: no price configured …]` notice instead of silently dropping cost.

### Waiting and typing during a prompt

At an interactive terminal harness shows a live wait indicator while a model
request or a tool call is outstanding: a single in-place line such as
`[turn: 1 · 12s · ctx 30% 60.0k/200.0k │ prompt 18s]` (or
`[tool: grep · 3s]`), updated about once a second. The context field includes
both the percentage and compact used/window token counts. Turn numbers restart
at 1 for each prompt. When a turn completes, its closing line adds the turn's
dollar cost (e.g. `[turn: 1 · 12s · $0.032 · ctx 30% 60.0k/200.0k │ prompt 18s]`)
once the model stream has closed and the final token totals are known; the
cost is omitted for models with no configured price.
It is erased the instant real output or a tool line appears, and is shown only at a
TTY when not quiet.

Text typed while a prompt is running is captured with echo off and shown on that
wait line after a `>` marker. Unsubmitted text is deposited into the next prompt
as editable, pre-filled text when the active prompt completes or is interrupted.
Ctrl-C and double-Esc cancel the active prompt.

### Steering while a prompt runs

By default, model-bound input submitted with Enter while a prompt is running is
injected as a user message before the next model request. This lets you redirect
the current work without canceling and retyping the prompt.

- `!shell`, `/commands`, and `/edit` retain their queued behavior and run at the
  next idle prompt.
- A steer does not consume a `max-turns` slot and resets the loop-guard streaks.
- If the prompt finishes before another model request, the submitted steer is
  recovered and run as the next prompt.
- Ctrl-C or double-Esc still cancels the active prompt.

`-no-steer`, `HARNESS_NO_STEER`, or config `no_steer` disables steering and
queues submitted input as the next prompt. Steering is available only in the
interactive REPL, not one-shot mode.

## Agents

An agent definition bundles a set of allowed tools with extra system-prompt
instructions and an optional model target override. Select one with
`-agent <name>`, `HARNESS_AGENT`, or `agent` in the config file. Switch
mid-session with `/agent <name>`.

Shift-Tab switches use the same full agent runtime selection as `/agent` and emit
the existing `[agent switched: <name>]` notice and provider/model line.
All switch-driven prewarms — Shift-Tab cycling, explicit `/agent`, `/model`,
and startup agent resolution — use a 500ms idle debounce, so rapid cycles or
resolution bursts warm only the final settled selection. It is not suppressed
merely because the model ID is unchanged:
system/tool prefixes and response continuation differ by agent. A warm-up
requested while a prompt is running is deferred and fired once when the prompt
completes (including interrupt/error exits): a running prompt refreshes the
cache prefix every turn, so a mid-prompt prewarm would be wasted.
Standalone `/compact` keeps immediate
prewarming; submitting a real prompt cancels any pending delayed warmup.

Four agents are built in:

| agent | tools | behavior |
|---|---|---|
| `auto` | all available built-in tools plus discovered MCP tools, including `delegate` and background job tools | the default; the model decides what to do |
| `explore` | read-only inspection/search tools, `web_fetch`, optional `git_readonly`, and read-only MCP tools; no mutation, todo, background, handoff, or delegate tools | broad search, architecture/dependency tracing, root-cause investigation, and questions spanning many files; not a known-file lookup |
| `plan` | inspection tools, read-only MCP tools, `write_tmp_file`, `update_todos`, `delegate`, and `background_jobs` | collaborate on a plan without modifying the project |
| `independent` | all available built-in tools plus discovered MCP tools, including `delegate` and background job tools | complete the task end-to-end without pausing for input |

Define new agents or override built-ins in the config file under `agents`.
**Breaking configuration rule:** every new custom agent must have a nonblank
`description` that tells the parent *when to use it*. Startup, `--agents`, and
`--show-config` fail when this selection metadata is missing or whitespace-only;
there is no generated fallback. An override of `auto`, `explore`, `plan`, or
`independent` may omit `description` and inherit the built-in value. Other fields
continue to merge onto a built-in of the same name:

```json
{
  "agent": "plan",
  "agents": {
    "plan": { "prompt": "@~/.config/harness/plan-prompt.md" },
    "security_review": {
      "description": "Use after implementation for an independent review of concrete security issues.",
      "allowed_tools": ["read_file", "list_dir", "grep", "git_readonly"],
      "mcp_tools": "read_only",
      "model": "anthropic:claude-opus-4-8",
      "prompt": "Review the diff and surrounding code for security issues. Report only concrete findings."
    }
  }
}
```

An agent without `model` inherits the current session target. When set, `model`
is a complete `<provider>:<model>` model-proxy target ID.

Each agent can set `mcp_tools` to control automatic exposure of discovered MCP
tools: `disabled`, `read_only`, or `all`. Explicit `mcp__...` names in
`allowed_tools` still work as a strict whitelist. Tool gating is the one place
the harness restricts tools; the underlying tools still assume an external
sandbox for real isolation.

### Planning and implementation handoff

The `plan` agent investigates and designs without modifying the project. It can
use `record_plan` to persist a durable Markdown plan under the session, then
`request_implementation` to propose handing the latest plan to an implementation
agent. At the prompt boundary, Harness displays the handoff brief and asks for
approval before switching agents and starting implementation with a clean
context seeded by the plan.

`/handoff [-a agent] [-m model] [message]` performs the same review manually.
`-a` overrides the configured target agent, `-m` applies a one-off model
override, and trailing text is added as separate user guidance. Options must
precede the message; use `--` when the message itself begins with a dash. The
target otherwise comes from `--handoff-agent`, `HARNESS_HANDOFF_AGENT`, config
`handoff_agent`, or the `auto` default. Handoffs require an interactive session
and are unavailable in one-shot mode.

## Sessions

- A session path is a directory. `tree.ndjson` is the canonical append-only
  conversation tree; `state.json` is compact mutable state containing the active
  leaf and runtime settings; `active-turn.json` is a transient atomic recovery
  record for the current model/tool boundary; `raw.ndjson` is the chronological replay log.
  `compactions/` stores raw messages removed from active context, `children/`
  stores child-agent transcripts and metadata, and `artifacts/tool-results/`
  stores full outputs omitted from model context.
- New tree records are appended and synced before `state.json` atomically moves
  its active-leaf pointer. An interrupted final tree record is ignored on load;
  malformed earlier records are errors. Auto-save uses
  `~/.local/state/harness/sessions/<timestamp>`, honoring `$XDG_STATE_HOME`.
- Harness checkpoints root and child runs before provider requests, before tool
  dispatch, and after each validated closed turn. A crash during tool execution
  recovers that open call as an explicit `interrupted` error instead of
  automatically executing it again. Closed-turn checkpoints include current
  todos, plans, usage, cache/proxy IDs, and a safe provider continuation anchor.
- `-session <dir>` chooses an explicit session directory. `-resume <dir>` loads
  its active tree path and continues, applying a newer active-turn recovery
  record when present and printing the recovered boundary. Child runs still
  marked `running` from the prior process become `abandoned`; their durable
  checkpoint remains eligible for compatible child-ID continuation. Continued
  children record whether they restored retained history directly or first
  built a compact checkpoint, together with before/after/window context
  estimates; `session stats` prints those fields and counts checkpoint summary
  calls as maintenance. Combining distinct `-resume <source>` and
  `-session <destination>` clones the active
  branch into the destination with fresh usage accounting. `/clear` rotates to
  a fresh directory.
- `/tree` opens a searchable, paged line picker over safe tree nodes. Its compact
  graph stays flat along linear history and adds indentation only at real forks;
  semantic row labels and condensed tool batches keep checkpoints readable
  within the terminal width. Selecting a human prompt branches from its parent
  and returns the prompt (including images) to the editor; selecting another
  node makes that node the branch point. Before moving, harness asks whether to
  attach no summary, a default summary, or a summary with custom focus. A failed
  summary leaves the branch unchanged.
- `/fork` presents the same compact fork graph filtered to prior human prompts,
  then performs the selected move in a new session. Hidden tool and assistant
  checkpoints do not add indentation. `/clone` copies the current branch into a
  new session. Both record parent-session lineage, reset prompt/usage accounting
  and remote continuation anchors, and preserve the current model, agent,
  reasoning settings, todos, and plans.
- Tree navigation changes only model-visible conversation context. It never
  rewinds the working directory or Git; every new branch carries an internal
  warning telling the model to inspect current files before assuming their state.
- Transcripts are provider-neutral, so a session started against Anthropic can
  resume against an OpenAI-compatible server and vice versa.
- A save requested mid-turn synthesizes an `interrupted` tool result before the
  transcript becomes immutable tree data, so resumed paths are valid for both APIs.

Inspect saved sessions with:

```sh
harness session replay [-f|--follow] [-q|--quiet] ~/.local/state/harness/sessions/20260611T123456Z
harness session timings ~/.local/state/harness/sessions/20260611T123456Z
harness session stats ~/.local/state/harness/sessions/20260611T123456Z
```

`session replay --follow` first renders the existing complete `raw.ndjson`
records, then renders complete records as they are appended. It uses the same
user-facing view as ordinary replay; `-q`/`--quiet` suppresses status lines but
keeps prompts and assistant text. Terminal replay applies the same color-gated
syntax highlighting to recognized tagged assistant fences; untagged and unknown
fences remain plain, and replay without ANSI emits no highlighting. Rendering is
display-only: stored events and ANSI-free latest-turn output remain unchanged. A
followed child exits successfully after its terminal metadata is observed and
the log receives one final drain. If that
metadata update failed, a child `prompt_usage` record can establish completion.
A followed root session has no terminal marker and continues until interrupted.
An existing directory without `raw.ndjson` is valid while following; a missing
directory is an immediate error.

`session timings` labels a prompt without a terminal `prompt_usage` event as
`in progress` and measures its elapsed time through the latest recorded event.

`session stats` prints a deterministic, human-readable report for one session:
root conversation turns, navigation count, tree entries/branches/leaves/depth,
direct and delegate tool/command activity, parallel batches, compactions, and a
hierarchical delegate breakdown. A child that has metadata and replay events
but no `state.json` checkpoint is included with `checkpoint: unavailable`
instead of aborting the report. The root token and cost totals come from
`state.json` and already include delegate and compaction usage; delegate totals
similarly include any nested delegates. The separate `Direct model activity
(non-overlapping)` section sums `turn_attempt_usage` and `maintenance_usage`
from each physical root and child replay exactly once. New prompt replay events
and child metadata also expose structured termination reasons; the stats report
summarizes them without treating them as task-success labels. When checkpoint
events are present, conversation statistics also report closed-turn checkpoint
count, average/maximum save duration, and lag in completed turns and seconds.
Retention activity is reported as epoch count, pressure-versus-age passes,
blocks/bytes trimmed, Responses-state resets, and whether the following request
used stateful continuation or full context.

### Session diagnostics

`diagnostics.ndjson` contains JSON-line diagnostics, including MCP and LSP child
process stderr that is hidden from the terminal by default. `raw.ndjson` is the
chronological replay and analysis log. In addition to visible conversation
events, it records model-request acceptance and completion, every failed
upstream attempt, scheduled retries, terminal failures, cancellation, and
retention epochs. Retention records include the trigger, reclaimed blocks/bytes,
context estimates, continuation reset, and next-request shape; they never enter
model context.

Model-request lifecycle records carry parsed provider messages, timing, and
request correlation used by `harness session timings`. They never become
conversation-tree entries or model context. The model proxy logs the same
failed upstream attempts individually. Multimodal endpoint rejections add the
sanitized records described in
[Multimodal tool-result compatibility diagnostics](#multimodal-tool-result-compatibility-diagnostics);
prompts, tool arguments, local paths, result text, and image base64 remain
excluded.

## Compaction

Compaction fires when `max(reported input tokens, estimated full-request
footprint)` reaches `compact_trigger_percent` (default 78) of the effective
model context window, or on `/compact`. `compact_auto_enabled: false` disables
only threshold-based compaction; `/compact` and provider-overflow recovery still
work. Harness compacts toward `compact_target_percent` (default 65) after fixed
system/tool overhead.

Interactive idle compaction is an opt-in experiment. Set
`compact_idle_after_seconds` above zero (default `0`, disabled) to prepare a
summary after that much REPL idle time when the estimated full context has
reached `compact_idle_trigger_percent` (default `35`; it must be lower than the
normal trigger). The summary runs against an immutable snapshot. Submitted
input cancels the work and starts immediately; a late result is discarded. A
candidate is archived and applied only if both the transcript and relevant
compaction runtime are unchanged. Sessions with `PreCompact` or `PostCompact`
hooks skip speculative compaction because hook side effects cannot safely run
on a candidate. `compact_auto_enabled: false` also disables idle preparation.

Every started idle attempt records an `idle_compaction` replay event with its
applied/discarded/failed/no-change outcome, wall time, trigger, message counts,
and context before/after when applied. Metered summary usage returned by the
worker is recorded as `maintenance_usage` with purpose `idle_compaction`, even
when the candidate is discarded. `session stats` summarizes outcomes, duration,
and applied context reduction.

The recent raw suffix is selected in whole completed turns, newest first, until
it first reaches `compact_keep_tokens` (default `20000`) or
`compact_keep_turns` (default `8`) turns. At least the newest completed turn is
kept. A turn is one assistant response plus its immediate tool-result batch, not
all model calls made after one user prompt. Low-water pressure can retain fewer
turns than the preferred suffix.

Before summarization, large old tool results and large old tool inputs are
reduced to previews (`compact_tool_result_max_bytes`, default `4096`), old images
are replaced with placeholders, and the raw removed messages are archived under
`compactions/`. If the old history is too large for one summary call, harness
summarizes chunks and then summarizes the chunk summaries. Later compactions
explicitly update the exact prior generated summary with only newly aged
history; they do not re-summarize the rendered checkpoint as conversation.
Non-quiet TTY runs show a transient elapsed-time indicator while this summary
work is in progress.
Older raw messages are archived before replacement. The active transcript gets
a synthetic user checkpoint containing the active prompt and steering text
verbatim, the progress summary, the archive reference, and a deterministic
cumulative index of successful supported `read_file`, `write_file`, `edit`, and
`apply_patch` paths from compacted history. The index records requested paths at
tool-call success granularity—so a successful batched read includes paths that
reported inline per-file errors—and does not infer effects from commands, Git,
MCP, or custom tools. The model records semantic state only for meaningful
changes and unfinished mutation intent; it does not duplicate read-only
inspected paths. Active todos are persisted separately and re-injected after a
successful compaction.

Use `/compact optional focus text` to emphasize one manual summary. Focus is
trimmed, recorded in hook/archive/tree metadata, and applies only to that
successful compaction. If low-water pressure leaves only the newest complete
turn, Harness truncates oversized tool payloads as a last resort without
splitting a tool-use/result pair. If compaction fails validation or
archive/summary writing, the full transcript is kept.

Turn summaries include approximate context footprint and, when stateful Responses
sends a smaller request than the full active conversation, the payload estimate.
If the active context, request payload, or tool schemas are large enough to
likely slow response startup, harness prints one warning per prompt to stderr.

## Interrupts

- Ctrl-C during a prompt, or Esc twice in short succession during a REPL prompt,
  cancels the prompt. It aborts the HTTP stream, kills any `run_command` process
  group, keeps streamed partial text, strips unexecuted tool calls, prints
  `[cancelled]`, and returns to the prompt. Any text typed during the prompt is
  preserved and deposited into the next prompt as editable pre-filled text.
- A second Ctrl-C within about one second, or Ctrl-C at the idle prompt, saves,
  prints the session token summary, and exits 130.
- Ctrl-D at the prompt saves, prints the summary, and exits 0.
- Ctrl-C during startup or helper-command network work cancels the in-flight
  request and exits 130 instead of waiting for the request timeout. This includes
  `session replay --follow`, where it ends an otherwise open-ended root follow.

## Hooks

Harness supports command hooks for `SessionStart`, `UserPromptSubmit`,
`PreToolUse`, `PostToolUse`, `PreCompact`, `PostCompact`, and `Stop`.
`PreCompact` and `PostCompact` receive `trigger`; focused manual compactions also
receive the one-shot `focus` field.

```json
{
  "hook_configs": ["hooks_config.json"],
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "run_command|apply_patch",
        "hooks": [
          {
            "type": "command",
            "command": "./hooks/pre-tool.sh",
            "timeout_seconds": 30,
            "status_message": "Checking tool call"
          }
        ]
      }
    ]
  }
}
```

Each command runs in the harness cwd with a JSON event payload on stdin. Every
payload carries common fields such as `session_id`, `transcript_path`, `cwd`,
`hook_event_name`, `model`, and `permission_mode`, plus per-event fields.

`matcher` is a Go regexp over the tool name for tool hooks, `manual|auto` for
compaction hooks, and `startup|resume|clear|fork|clone` for `SessionStart`. Omitted, empty,
or `*` matches all. Hook commands may block with exit code `2` or JSON stdout
such as `{"decision":"block","reason":"..."}` / `{"continue":false}`. Plain
stdout is added as hook context only when the command exits `0`.

`hook_configs` files may contain either a `{"hooks": {...}}` wrapper or a bare
event map, and relative `hook_configs` paths resolve against the config-file
directory. Static preferences belong in `~/.agents/AGENTS.md`; command-derived
facts belong in hook output, which the model receives as `[hook context]`.
