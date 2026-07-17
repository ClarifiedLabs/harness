# harness

A minimal agentic coding harness in Go: a plain-text, line-oriented CLI that
supports basic tool use, delegate sub-agents, skills, and MCP.

## Design Invariants

- **Zero third-party Go dependencies.** Go standard library only.
- **No sandbox.** `harness` assumes it is launched in an environment that is
  already sandboxed enough; tools run with the process's privileges.
- **Isolated provider and MCP access.** `harness` uses separate services for
  model providers and MCP. Those services can run outside the agent's sandbox, so
  API keys, OAuth tokens, and MCP credentials do not need to be available to the
  agent process.

**Important Note:** There are no built in protections in harness unlike other
popular agentic coding tools. You are in full control.

## Basic Architecture

Harness uses an unrestricted agent CLI tool combined with separate service
that enable access to AI models and MCP services. This architecture enables
running the coding agent in a sandboxed environment that doesn't have access
to any credentials.

```text
                               +-----------------------+
                               | Env with Credentials  |
+----------------------+       |                       |
| Sandboxed Workspace  |       |  +-------------+      |
|                      |   +---+--> Model Proxy |      |
|   +-------------+    |   |   |  +-------------+      |
|   |             |    |   |   |  +-----------+        |
|   | Harness CLI +----+---+---+--> MCP Proxy |        |
|   |             |    |   |   |  +-----------+        |
|   +-------------+    |   |   |  +------------------+ |
+----------------------+   +---+--> Local Git Remote | |
                               |  +------------------+ |
                               +-----------------------+
```

## Quickstart

Once the first release is published, install one of the release packages.
Homebrew formulae are split by binary: `harness` installs only the CLI,
`harness-model-proxy` installs the model proxy, `harness-mcp-proxy` installs
the MCP proxy, and `harness-full` installs all three.

```sh
brew tap ClarifiedLabs/tap
brew install harness-full
```

Or download a package from
[`github.com/ClarifiedLabs/harness/releases`](https://github.com/ClarifiedLabs/harness/releases):

- macOS arm64: signed `.pkg` or `harness_<version>_darwin_arm64.tar.gz`
- Linux amd64/arm64: per-binary `.deb`/`.rpm` packages or
  `harness_<version>_linux_<arch>.tar.gz`

Tarballs include all three binaries. Homebrew, `.deb`, and `.rpm` packages are
split by binary.

Container images are published to GHCR with version tags like `1.2.3` and
`latest`:

```sh
docker run --rm -it -v "$PWD:/workspace" -w /workspace ghcr.io/clarifiedlabs/harness:latest --version
docker run --rm -p 8765:8765 ghcr.io/clarifiedlabs/harness-model-proxy:latest serve -listen 0.0.0.0:8765
docker run --rm -p 8766:8766 ghcr.io/clarifiedlabs/harness-mcp-proxy:latest serve -listen 0.0.0.0:8766
```

Until the first release exists, install from a checkout with Go 1.26:

```sh
git clone https://github.com/ClarifiedLabs/harness.git
cd harness
go install ./cmd/harness ./cmd/harness-model-proxy ./cmd/harness-mcp-proxy
```

Configure provider access and start the model proxy:

```sh
harness-model-proxy setup
harness-model-proxy
```

Then run the CLI in another shell:

```sh
harness -provider <provider> -model <model>
harness -provider <provider> -model <model> -p "summarize README.md"
```

`harness-model-proxy setup` writes proxy/provider config, prompts for auth
when needed, and lets you choose which models are available locally. By default
the model proxy listens on `127.0.0.1:8765`. It caches the full models.dev
catalog locally and refreshes that cache every 24 hours while serving; configure
`models_dev_cache_ttl` or pass `-models-dev-cache-ttl 0` to disable the periodic
refresh.

Provider config files written by `setup`/`refresh-models` are **managed**
(`"managed": true`) and store **no per-model prices**: the proxy resolves managed
prices and input modalities live from the models.dev cache, so the 24h refresh
updates served metadata without re-running `setup` or restarting. If that cache
no longer lists a managed provider/model, the proxy warns and removes the stale
target from the live catalog. A hand-written config without `"managed": true` is
**manual** — the proxy never edits it and keeps its own `price` and
`input_modalities` entries. Manual models
must set `input_modalities` explicitly; use `["text", "image"]` for models that
accept image attachments. A managed config may set `"price_source"` to resolve
metadata from a different models.dev provider id. `openai-codex` is subscription
backed, so setup writes no prices for it; its model list comes from the OpenAI
Codex bundled catalog and `refresh-models` can update that catalog from
`openai/codex`. Codex configs also set `"omit_max_output_tokens": true` because
that backend rejects the standard Responses parameter. The proxy uses the
Responses WebSocket transport by default for `codex_oauth` Responses providers
unless `responses_websocket:false` is set. Responses providers default to
stateful continuation; if a backend rejects stored responses, harness disables
stateful continuation for that agent and retries the request stateless.

Provider configs may set `prompt_cache.key_field` to `auto`, `none`,
`prompt_cache_key`, or `session_id`; `auto` sends `prompt_cache_key` to
first-party OpenAI endpoints, `session_id` to OpenRouter, and omits cache key
fields for other custom OpenAI-compatible base URLs. The model proxy derives the
provider-facing cache key as `sha256` of harness's local session key, so providers
do not receive the raw harness session identifier. `prompt_cache.affinity_headers`
can copy the same provider-facing key into non-auth routing headers such as
`x-session-id`.

While serving, the proxy also answers a read-only `GET /v1/usage` that aggregates
token and cost totals per model target (including delegate child-agent spend),
and its `GET /v1/models` response carries complete static pricing schedules
(including context-length tiers) plus a pricing `source_date` and
`max_age_seconds` so clients can detect stale catalog prices. Setup and runtime
model pickers show each band as input/output USD per 1M tokens. For managed
providers `source_date` tracks the models.dev cache (kept fresh by the
refresher); for manual-only setups it is the provider config file's mtime.
Cost budgets are per model-proxy API key. Generate a key with
`-budget-usd 25 -budget-period 24h` to attach a fixed-window quota to that key:
new authenticated `/v1/stream` requests are rejected with 429 once the key's
recorded known-cost spend reaches the limit, and spend state persists under the
model-proxy config directory across restarts. Unpriced targets are allowed by
default and do not count toward the budget; add `-budget-reject-unpriced` to the
key if those requests should be rejected instead. `/v1/usage` includes the
current authenticated key's budget state when that key has a budget.

The proxy also exposes a Prometheus `/metrics` endpoint on a **separate port**
(default `127.0.0.1:9090`), unauthenticated, so a scraper can reach it without
the harness CLI's API-key path. Metrics break usage down by `provider`, `model`,
and `key` (the authorizing API key's stored name, or `anonymous` when auth is
disabled); the separate `model_proxy_build_info` gauge carries only the build
`version`. Token counters (`model_proxy_*_tokens_total`) are recorded for every
stream that produced usage — priced or not — while `model_proxy_cost_usd_total`
is recorded only for priced models, a deliberate superset of `/v1/usage`'s
priced-only cost rollup. Use `-no-metrics` to disable the endpoint and
`-metrics-listen` to move it; both also have config-file counterparts under a
`metrics` object (`enabled`, `listen`).

Use `harness --models` to list the model targets and whether they support
portable reasoning profiles. Use `harness --agents` to list the
configured agents. Add `--format json` to `--models`, `--agents`, or
`--check-model-proxy` when another program needs structured output. Use
`harness --debug-request -p "..."` to dump the first provider-neutral model
request, tool set, context estimate, and byte counts without calling the model.
When a model target advertises `server_tools:["web_search"]`, opt in with
`-web-search auto` to let the provider run its hosted web search tool.

MCP is optional. After configuring downstream servers for `harness-mcp-proxy`,
start it separately and enable MCP for `harness`:

```sh
harness-mcp-proxy serve
HARNESS_MCP_ENABLE=true harness -provider <provider> -model <model>
```

In HTTP mode, the MCP proxy also exposes an unauthenticated Prometheus endpoint
on a separate listener, defaulting to `127.0.0.1:9091`. Configure it with
`proxy.metrics.enabled` / `proxy.metrics.listen`, or override those settings with
`serve -no-metrics` / `serve -metrics-listen`. The tool-call counter families are
`mcp_proxy_requests_total`, `mcp_proxy_errors_total`,
`mcp_proxy_request_bytes_total`, `mcp_proxy_response_bytes_total`, and
`mcp_proxy_request_duration_seconds_total`; routed series use only `mcp`, `tool`,
and `key` (the authorizing key's stored name, or `anonymous`). The
`mcp_proxy_build_info` gauge uses only `version`. `serve -stdio` opens no metrics
listener and performs no metrics collection, regardless of flags or config.

## Authenticating harness to its proxies

Both proxies support optional API-key authentication. It is disabled by default
and becomes required as soon as the first key is stored in the proxy's dedicated
API-key file (`api_keys.json` next to the proxy config by default). Normal proxy
configs may point elsewhere with `api_keys_file` (model proxy) or
`proxy.api_keys_file` (MCP proxy), and `serve -api-keys-file path` overrides both.
Inline `api_keys` / `proxy.api_keys` in normal configs are rejected with a
migration error.

Generate and store a key for the model proxy (the full plaintext key is printed
once):

```sh
harness-model-proxy generate-api-key [-api-keys-file path] [-ttl 720h] [-budget-usd 25 -budget-period 24h] laptop
harness --model-proxy-api-key <key> -provider <provider> -model <model>
```

Or use the `HARNESS_MODEL_PROXY_API_KEY` environment variable, or set
`model_proxy_api_key` in `~/.config/harness/config.json`.

For the MCP proxy:

```sh
harness-mcp-proxy generate-api-key [-api-keys-file path] [-ttl 720h] laptop
HARNESS_MCP_PROXY_API_KEY=<key> HARNESS_MCP_ENABLE=true harness -provider <provider> -model <model>
```

Or set `mcp.api_key` in `~/.config/harness/config.json`. The model proxy key has
prefix `hmp_`; the MCP proxy key has prefix `hmcpp_`. Only SHA-256 hashes of
keys are stored; the plaintext is shown exactly once at generation. Omit `-ttl`
(or use `0`) for a non-expiring key; running HTTP proxies poll their key file and
pick up valid additions/removals without restart. Existing `harness` clients load
the outgoing key only at process start. For an authenticated MCP proxy, the debug
`harness-mcp-proxy tools` command uses `HARNESS_MCP_PROXY_API_KEY` or
`tools -api-key <key>`.

To correlate a harness run across proxy logs, enable opt-in proxy request tracing:

```sh
harness -trace-proxy -provider <provider> -model <model>
```

This sends standard W3C `traceparent` headers to `harness-model-proxy` and
`harness-mcp-proxy`. Proxy logs that receive a valid trace include `trace_id`,
`span_id`, `parent_span_id`, and `trace_sampled` fields; prompts, request bodies,
API keys, and auth headers are not logged by tracing.

## Basic usage

Interactive mode starts when no prompt is provided:

```sh
harness -provider anthropic -model claude-opus-4-8
```

Use `-i` to send the first prompt from the command line and then continue in the
interactive session:

```sh
harness -model openrouter:openai/gpt-5.5 -i "inspect the current diff"
```

One-shot mode sends a single prompt and exits:

```sh
harness -model openrouter:openai/gpt-5.5 -p "summarize README.md"
```

`provider:model` selects a configured model-proxy target. If `-provider` is
also set, harness resolves that provider's target first and reports an error
when the model name only matches another provider. You can also configure
defaults in `~/.config/harness/config.json` or with `HARNESS_PROVIDER` and
`HARNESS_MODEL`.

Saved sessions can be replayed or inspected:

```sh
harness session replay ~/.local/state/harness/sessions/20260611T123456Z
harness session timings ~/.local/state/harness/sessions/20260611T123456Z
```

Diagnostic logs, including MCP/LSP child-process stderr that is hidden from the
terminal by default, are kept as JSON lines in the session's `diagnostics.ndjson`.

## Runaway protection

The agent loop has guardrails against runaway token burn, beyond the blunt
`-max-turns` count (default 250):

- `-max-turn-tokens <n>` stops a user turn once accumulated tokens (input +
  cache + output + reasoning, across every model call in the turn) reach the
  budget. `0`, the default, means unlimited.
- `-max-prompt-cost <usd>` stops a user turn once its accumulated model cost (in
  USD, when provider usage reports known cost) reaches the budget. `0`, the
  default, means unlimited. Models without known cost have no cost ceiling.
- Tool calls that repeat with identical results are first steered, then hard
  stopped; consecutive turns where every tool call fails are steered then broken.
- `-tool-timeout <s>` (default 600) is a per-tool-call backstop so a hung tool
  cannot stall a turn; `run_command`'s own `timeout_seconds` stays authoritative.

When a turn hits its model-turn limit, harness issues one final tools-disabled
request so the turn ends on an assistant summary rather than a dangling tool call.

## Mid-turn steering

By default, a prompt you submit with Enter while a model turn is running is
**steered**: it is injected as a user message before the *next* model request
(that is, between tool rounds — the next time harness sends data back to the
model) rather than waiting for the turn to end. This lets you redirect a
running turn ("no, use the other file", "stop, you're overcomplicating it")
without canceling and re-typing.

- Only model-bound input is steered. `!shell`, `/commands`, and `/edit`
  submitted during a turn keep the legacy behavior (queued, run at the idle
  prompt after the turn).
- A steer does not consume a `max-turns` slot and resets the loop-guard
  streaks, so redirecting is never penalized.
- If the turn ends without a tool round to inject into (a plain answer, or a
  budget/cancel break), the steer is recovered and run as the next turn, so the
  input is never lost.
- `^C` / double-Esc still cancel the in-flight turn exactly as before.

`--no-steer` (or `no_steer` / `HARNESS_NO_STEER`) restores the original
behavior: during-turn input is queued and runs as the next turn after the
current one completes. Steering is interactive-REPL only (not one-shot mode).

## Plan and implementation handoff

The `plan` agent investigates and designs without modifying the project. It can
`record_plan` to persist a plan as markdown under the session (a durable,
human-diffable artifact), and `request_implementation` to ask to hand the plan
off to an implementation agent. The handoff is interactive and user-approved;
when approved, harness switches agents and immediately starts implementation
from the recorded plan:

```text
/handoff [agent]   review the recorded plan and, on approval, switch this session
                   to an implementation agent with a clean context seeded by the
                   plan plus a short handoff brief, then start implementation
```

`record_plan` is available to every default agent; the handoff is plan-only and
unavailable in one-shot mode. The target defaults to `auto`; override it with
`--handoff-agent <name>`, `HARNESS_HANDOFF_AGENT`, the `handoff_agent` config
key, or the `/handoff <agent>` argument. Because the implementation starts from a
clean context, the target may use a different, cheaper model. Any agent can pin
its own thinking effort with a per-agent `reasoning` field in config, so a
fast/cheap implementation agent can pair a smaller model with lower effort.

## Build from source

```sh
make build
```

`make build` builds the three core binaries. To build only the CLI:

```sh
go build -o harness ./cmd/harness
```

Verify a checkout with:

```sh
go build ./... && go vet ./... && go test ./...
```

## Documentation

- [Usage reference](docs/usage.md): flags, config, provider selection, one-shot
  mode, REPL commands, agents, sessions, compaction, interrupts, and hooks.
- [Tools](docs/tools.md): built-in tools, delegation, background jobs,
  truncation, and tool artifacts.
- [MCP](docs/mcp.md): configuring and running `harness-mcp-proxy`.
- [LSP](docs/lsp.md): optional code intelligence tools, including independent Serena support.
- [Release](docs/release.md): release artifacts, tagging, and required secrets.
- [Design](docs/design.md): architecture and implementation details.
- [Smoke tests](docs/smoke.md): end-to-end verification matrix.
