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

`--models --format json` also shows each target's `server_tools`, price, and
variant relationship (`base_target_id` / `variant`). When a target advertises
`web_search`, `-web-search auto` lets harness declare the provider-hosted web
search tool for model calls. The default is `off`.

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

When stdout is a terminal, basic Markdown is rendered for readability; redirected
or piped one-shot stdout stays raw model text. Bracketed status lines are
timestamped by default; disable them when you want untimestamped diagnostics:

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
-responses-stateful   use Responses API previous_response_id continuation when supported (default true)
-no-steer         disable in-prompt steering: queue input for the next prompt instead of injecting it before the next turn (default off; see "Steering")
-image-detail <level>   default image detail: auto, low, high, or original
-image <path|detail:path>   attach an image in one-shot mode or to the initial -i prompt; repeatable
-agent <name>     agent: auto (default), explore, plan, independent, or a config-defined agent
-handoff-agent <name>   default implementation agent for plan handoffs (default auto)
-search-tools <mode>   search tools to expose: auto, grep, rg, or both
-web-search <mode>     server-side web search: off or auto (default off)
-trace-proxy      send W3C trace headers to the model and MCP proxies
-v                show tool result snippets (first ~5 lines, dimmed) and tool-call progress details
-tool-stream      show tool-call progress details (default false)
-show-diffs       show per-tool-call file diffs for built-in file edits (default true; use -show-diffs=false to disable)
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
`[prompt: N turns …]`. The [Flags](#flags) section lists defaults and config
forms; [design section 8.1](design.md#81-prompt-and-turn-loop) records the exact
loop mechanics.

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
  `compact_target_percent`, `compact_summary_max_tokens`, and
  `compact_tool_result_max_bytes`.
  Tool-result truncation is controlled by config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. `rg` and `grep` default to 32 KB / 500 lines
  and can be overridden with `rg_result_max_bytes`, `rg_result_max_lines`,
  `grep_result_max_bytes`, and `grep_result_max_lines`, or the matching
  `HARNESS_*` env vars. `read_file` defaults to 500 lines and a 32 KB result cap;
  configure `read_file_default_limit`, `read_file_result_max_bytes`, and
  `read_file_result_max_lines`, or matching `HARNESS_*` env vars. The delegate
  tool also has config-file-only `delegate_max_turns` (per-child turn cap)
  and `delegate_max_depth` (recursive depth cap, root depth `0`).
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
`api_type: "responses"` and `responses_stateful:true`. Disable it with
`responses_stateful:false`, `HARNESS_RESPONSES_STATEFUL=false`, or
`-responses-stateful=false`. If a provider rejects stored Responses requests,
harness disables stateful continuation for that agent and retries the request
stateless.
The model proxy also content-addresses the exact provider-neutral transcript
prefix represented by each stored response. It reuses `previous_response_id`
only when the incoming prefix matches; retention, compaction, branch changes,
or any other prefix rewrite cause a full resend that establishes a fresh anchor.
Responses provider configs may also set `responses_websocket:true` to have the
model proxy use the Responses WebSocket transport instead of HTTP SSE. The proxy
defaults this on for `codex_oauth` Responses providers and preserves an explicit
`responses_websocket:false` override.

Gemini Interactions continuation is managed entirely by the model proxy and is
on by default for `api_type:"interactions"`. It sends `store:true` and reuses
`previous_interaction_id` only while the content-addressed transcript prefix
matches. Set `interactions_stateful:false` in the provider config to send
`store:false` with complete history. Missing/rejected stored state also retries
with complete history; signed `thought`, Google Search call, and Google Search
result steps are persisted invisibly so this fallback remains valid after proxy
restart. Stored interactions are retained by Google under the account's
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

The special `openai-codex` provider uses ChatGPT subscription auth instead of an
API key, exposes models from the OpenAI Codex catalog, and reports token usage
without dollar pricing. It omits Responses `max_output_tokens` because the Codex
backend rejects that parameter. Input-token preflight counts use a local
`o200k_base` estimate because the Codex CLI protocol does not expose a separate
count-token endpoint. The proxy also uses the Responses WebSocket transport by
default for this provider, matching Codex's stateful continuation path while
sending `store:false` to the ChatGPT backend. Managed setup explicitly emits the
hashed conversation cache key as `prompt_cache_key`. Its startup warm-up uses
WebSocket `generate:false`, then reuses that response id for the first real
request. After setup, run:

```sh
harness-model-proxy auth login openai-codex
```

Provider configs accept an optional `auth` block in place of `api_key` /
`api_key_env`; when `auth` is present, API-key fields are ignored and there is no
fallback if auth fails. Supported auth shapes include `token_command`, `oauth2`,
and `codex_oauth`.

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
target, including delegate child-agent spend. `GET /v1/models` includes complete
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
budget.

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

Use `-no-metrics` to disable the endpoint or `-metrics-listen` to move it. The
equivalent proxy-config `metrics` object accepts `enabled` and `listen`.

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
Their prewarm uses a 500ms idle debounce, so rapid cycles warm only the final
settled agent. It is not suppressed merely because the model ID is unchanged:
system/tool prefixes and response continuation differ by agent. Startup, explicit
`/agent`, handoff and model changes, and standalone `/compact` keep immediate
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
  leaf and runtime settings; `raw.ndjson` is the chronological replay log.
  `compactions/` stores raw messages removed from active context, `children/`
  stores child-agent transcripts and metadata, and `artifacts/tool-results/`
  stores full outputs omitted from model context.
- New tree records are appended and synced before `state.json` atomically moves
  its active-leaf pointer. An interrupted final tree record is ignored on load;
  malformed earlier records are errors. Auto-save uses
  `~/.local/state/harness/sessions/<timestamp>`, honoring `$XDG_STATE_HOME`.
- `-session <dir>` chooses an explicit session directory. `-resume <dir>` loads
  its active tree path and continues. Combining distinct `-resume <source>` and
  `-session <destination>` clones the active branch into the destination with
  fresh usage accounting. `/clear` rotates to a fresh directory.
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
keeps prompts and assistant text. A followed child exits successfully after its
terminal metadata is observed and the log receives one final drain. If that
metadata update failed, a child `prompt_usage` record can establish completion.
A followed root session has no terminal marker and continues until interrupted.
An existing directory without `raw.ndjson` is valid while following; a missing
directory is an immediate error.

`session stats` prints a deterministic, human-readable report for one session:
root conversation turns, navigation count, tree entries/branches/leaves/depth,
direct and delegate tool/command activity, parallel batches, compactions, and a
hierarchical delegate breakdown. The root token and
cost totals come from `state.json` and already include delegate and compaction
usage; delegate totals similarly include any nested delegates.

### Session diagnostics

`diagnostics.ndjson` contains JSON-line diagnostics, including MCP and LSP child
process stderr that is hidden from the terminal by default. `raw.ndjson` is the
chronological replay and analysis log. In addition to visible conversation
events, it records model-request acceptance and completion, every failed
upstream attempt, scheduled retries, terminal failures, and cancellation.

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
MCP, or custom tools. The model-authored `Files touched` section remains the
semantic source for file state and unsupported operations.

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
