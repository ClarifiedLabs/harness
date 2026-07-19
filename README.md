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
to credentials that are usable outside of your local machine/network[^1].

[^1]: The Model & MCP proxies both support optional API keys for Harness CLI -> Proxy access. These services are intended to be exposed only on localhost or within a trusted local network.

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

## Quickstart

### macOS

Install the CLI and both proxies with Homebrew:

```sh
brew tap ClarifiedLabs/tap
brew install harness-full
```

<!-- release-artifacts:start -->
Or download the latest signed package containing all three binaries:

- Apple silicon (arm64): [`.pkg` (v0.0.37)](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness_v0.0.37_darwin_arm64.pkg)

### Linux

The latest release, v0.0.37, is available for amd64/x86_64 and arm64/aarch64:

| Binary | amd64 / x86_64 | arm64 / aarch64 |
|---|---|---|
| `harness` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness_0.0.37_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-0.0.37-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness_0.0.37_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-0.0.37-1.aarch64.rpm) |
| `harness-model-proxy` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-model-proxy_0.0.37_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-model-proxy-0.0.37-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-model-proxy_0.0.37_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-model-proxy-0.0.37-1.aarch64.rpm) |
| `harness-mcp-proxy` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-mcp-proxy_0.0.37_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-mcp-proxy-0.0.37-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-mcp-proxy_0.0.37_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.0.37/harness-mcp-proxy-0.0.37-1.aarch64.rpm) |

### Docker

Multi-architecture images are available for Linux amd64 and arm64:

- [`ghcr.io/clarifiedlabs/harness:0.0.37`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness)
- [`ghcr.io/clarifiedlabs/harness-model-proxy:0.0.37`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-model-proxy)
- [`ghcr.io/clarifiedlabs/harness-mcp-proxy:0.0.37`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-mcp-proxy)
<!-- release-artifacts:end -->

### Configure the model proxy

Choose a provider, authenticate, select the models to expose, and start the
proxy:

```sh
harness-model-proxy setup
harness-model-proxy
```

Then run the CLI in another shell:

```sh
harness -model <provider>:<model>
harness -model <provider>:<model> -p "summarize README.md"
```

The proxy listens on `127.0.0.1:8765` by default. See the
[model proxy documentation](docs/usage.md#model-proxy) for advanced provider,
authentication, pricing, budget, metrics, and tracing configuration. MCP is
optional; see [MCP servers](docs/mcp.md).

## Basic usage

Interactive mode starts when no prompt is provided:

```sh
harness -model anthropic:claude-opus-4-8
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

`provider:model` selects a configured model-proxy target. You can also configure
the default target as `model` in `~/.config/harness/config.json` or with
`HARNESS_MODEL`.

Saved sessions can be replayed or inspected:

```sh
harness session replay ~/.local/state/harness/sessions/20260611T123456Z
harness session timings ~/.local/state/harness/sessions/20260611T123456Z
harness session stats ~/.local/state/harness/sessions/20260611T123456Z
```

The stats report covers turns, tools, commands, parallel batches, compactions,
and per-delegate details; its session token and cost totals are authoritative
and include delegate usage.

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
