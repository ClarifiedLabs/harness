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

Once the first release is published, install one of the release packages. They
include `harness`, `harness-model-proxy`, and `harness-mcp-proxy`.

```sh
brew tap ClarifiedLabs/tap
brew install harness
```

Or download a package from
[`github.com/ClarifiedLabs/harness/releases`](https://github.com/ClarifiedLabs/harness/releases):

- macOS arm64: signed `.pkg` or `harness_<version>_darwin_arm64.tar.gz`
- Linux amd64/arm64: `.deb`, `.rpm`, or `harness_<version>_linux_<arch>.tar.gz`

Until the first release exists, install from a checkout with Go 1.26:

```sh
git clone https://github.com/ClarifiedLabs/harness.git
cd harness
go install ./cmd/harness ./cmd/harness-model-proxy ./cmd/harness-mcp-proxy
```

Configure provider access and start the model proxy:

```sh
harness-model-proxy --setup
harness-model-proxy
```

Then run the CLI in another shell:

```sh
harness -provider <provider> -model <model>
harness -provider <provider> -model <model> -p "summarize README.md"
```

`harness-model-proxy --setup` writes proxy/provider config, prompts for auth
when needed, and lets you choose which models are available locally. By default
the model proxy listens on `127.0.0.1:8765`. It caches the full models.dev
catalog locally and refreshes that cache every 24 hours while serving; configure
`models_dev_cache_ttl` or pass `-models-dev-cache-ttl 0` to disable the periodic
refresh.

Provider config files written by `--setup`/`--refresh-models` are **managed**
(`"managed": true`) and store **no per-model prices**: the proxy resolves managed
prices live from the models.dev cache, so the 24h refresh updates served prices
without re-running `--setup` or restarting. A hand-written config without
`"managed": true` is **manual** — the proxy never edits it and keeps its own
`price` entries. A managed config may set `"price_source"` to resolve prices from
a different models.dev provider id; `--setup` sets it to `openai` for
`openai-codex` so codex models are priced at the normal OpenAI per-token rates.

While serving, the proxy also answers a read-only `GET /v1/usage` that aggregates
token and cost totals per provider/model (including delegate child-agent spend),
and its `GET /v1/models` response carries a pricing `source_date` plus
`max_age_seconds` so clients can detect stale catalog prices. For managed
providers `source_date` tracks the models.dev cache (kept fresh by the
refresher); for manual-only setups it is the provider config file's mtime.

Use `harness --models` to list the providers, models, and cataloged reasoning
controls exposed by the configured proxy. Use `harness --agents` to list the
configured agents. Add `--format json` to `--models`, `--agents`, or
`--check-model-proxy` when another program needs structured output.

MCP is optional. After configuring downstream servers for `harness-mcp-proxy`,
start it separately and enable MCP for `harness`:

```sh
harness-mcp-proxy serve
HARNESS_MCP_ENABLE=true harness -provider <provider> -model <model>
```

## Basic usage

Interactive mode starts when no prompt is provided:

```sh
harness -provider anthropic -model claude-opus-4-8
```

One-shot mode sends a single prompt and exits:

```sh
harness -model openrouter:openai/gpt-5.5 -p "summarize README.md"
```

`provider:model` is shorthand for selecting a proxy provider and sending the
provider-local model id. You can also configure defaults in
`~/.config/harness/config.json` or with `HARNESS_PROVIDER` and `HARNESS_MODEL`.

Saved sessions can be replayed or inspected:

```sh
harness session replay ~/.local/state/harness/sessions/20260611T123456Z
harness session timings ~/.local/state/harness/sessions/20260611T123456Z
```

## Runaway protection

The agent loop has guardrails against runaway token burn, beyond the blunt
`-max-turns` count (default 250):

- `-max-turn-tokens <n>` stops a user turn once accumulated tokens (input +
  cache + output + reasoning, across every model call in the turn) reach the
  budget. `0`, the default, means unlimited.
- `-max-prompt-cost <usd>` stops a user turn once its accumulated model cost (in
  USD, using catalog pricing) reaches the budget. `0`, the default, means
  unlimited. Only fires for models with catalog pricing — an uncatalogued model
  has no known cost, so the budget cannot apply (you'll see the unknown-price
  warning instead).
- Tool calls that repeat with identical results are first steered, then hard
  stopped; consecutive turns where every tool call fails are steered then broken.
- `-tool-timeout <s>` (default 600) is a per-tool-call backstop so a hung tool
  cannot stall a turn; `run_command`'s own `timeout_seconds` stays authoritative.

When a turn hits its model-turn limit, harness issues one final tools-disabled
request so the turn ends on an assistant summary rather than a dangling tool call.

## Plan and implementation handoff

The `plan` agent investigates and designs without modifying the project. It can
`record_plan` to persist a plan as markdown under the session (a durable,
human-diffable artifact), and `request_implementation` to ask to hand the plan
off to an implementation agent. The handoff is interactive and user-approved:

```text
/handoff [agent]   review the recorded plan and, on approval, switch this session
                   to an implementation agent with a clean context seeded by the
                   plan plus a short handoff brief (provenance + environment facts)
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
- [LSP](docs/lsp.md): optional read-only code intelligence tools.
- [Release](docs/release.md): release artifacts, tagging, and required secrets.
- [Design](docs/design.md): architecture and implementation details.
- [Smoke tests](docs/smoke.md): end-to-end verification matrix.
