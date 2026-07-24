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

- Apple silicon (arm64): [`.pkg` (v0.2.0)](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness_v0.2.0_darwin_arm64.pkg)

### Linux

The latest release, v0.2.0, is available for amd64/x86_64 and arm64/aarch64:

| Binary | amd64 / x86_64 | arm64 / aarch64 |
|---|---|---|
| `harness` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness_0.2.0_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-0.2.0-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness_0.2.0_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-0.2.0-1.aarch64.rpm) |
| `harness-model-proxy` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-model-proxy_0.2.0_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-model-proxy-0.2.0-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-model-proxy_0.2.0_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-model-proxy-0.2.0-1.aarch64.rpm) |
| `harness-mcp-proxy` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-mcp-proxy_0.2.0_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-mcp-proxy-0.2.0-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-mcp-proxy_0.2.0_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.2.0/harness-mcp-proxy-0.2.0-1.aarch64.rpm) |

### Docker

Multi-architecture images are available for Linux amd64 and arm64:

- [`ghcr.io/clarifiedlabs/harness:0.2.0`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness)
- [`ghcr.io/clarifiedlabs/harness-model-proxy:0.2.0`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-model-proxy)
- [`ghcr.io/clarifiedlabs/harness-mcp-proxy:0.2.0`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-mcp-proxy)
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

Managed Google targets use the native Gemini Interactions API. The model proxy
stores interactions by default for efficient `previous_interaction_id`
continuation; set `interactions_stateful:false` in the Google provider config to
send complete stateless history instead. Signed Gemini thought and Google Search
steps are retained in sessions so a proxy restart can safely fall back to that
stateless history. Enable provider-hosted Google Search with
`-web-search auto`.

Image-capable models can call the read-only `view_image` tool to inspect a local
PNG, JPEG, WebP, or non-animated GIF during a turn. Rich image tool results stay
provider-neutral inside harness and are lowered to each provider's multimodal
wire shape; text-only models are rejected before the file is read. Image paths
must be regular files—directories, devices, and FIFOs are rejected—and image
limits are enforced over the complete retained request before network activity.

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
conversation-tree shape, and per-delegate details; its session token and cost
totals are authoritative and include delegate usage.

Interactive sessions keep an append-only conversation tree. `/tree` moves to an
earlier safe point without changing files or Git state; selecting a prior user
prompt removes that prompt from active context and places its text back in the
editor. `/fork` does the same in a new session, while `/clone` copies the current
branch. Forked and cloned sessions start fresh usage accounting and retain the
current model, agent, reasoning controls, todos, plans, and working directory.

Diagnostic logs, including MCP/LSP child-process stderr that is hidden from the
terminal by default, are kept as JSON lines in the session's `diagnostics.ndjson`.
When a concrete endpoint rejects a valid image-bearing tool result, Harness emits
one actionable compatibility notice and records safe request-shape and
request/trace correlation metadata there. Prompts, tool arguments, local paths,
result text, and image base64 are excluded. Use `-trace-proxy` to correlate the
notice with model-proxy logs; Harness never retries by silently dropping the
image. `--quiet` suppresses the terminal compatibility notice while retaining
the single structured diagnostic record.

## Runaway protection

The agent loop has guardrails against runaway token burn, beyond the blunt
`-max-turns` count (default 250):

- `-max-prompt-tokens <n>` stops a prompt once accumulated tokens (input +
  cache + output + reasoning, across every model call in the prompt) reach the
  budget. `0`, the default, means unlimited.
- `-max-prompt-cost <usd>` stops a prompt once its accumulated model cost (in
  USD, when provider usage reports known cost) reaches the budget. `0`, the
  default, means unlimited. Models without known cost have no cost ceiling.
- Tool calls that repeat with identical results are first steered, then hard
  stopped; consecutive turns where every tool call fails are steered then broken.
- `-tool-timeout <s>` (default 600) is a per-tool-call backstop so a hung tool
  cannot stall a turn; `run_command`'s own `timeout_seconds` stays authoritative.

When a prompt hits its turn limit, harness issues one final tools-disabled turn
so the prompt ends on an assistant summary rather than a dangling tool call.

Harness uses these terms consistently:

- A **prompt** is one top-level interaction started by user input.
- A **turn** is one model response plus the complete tool-result batch it
  requested. Turn numbers restart at 1 for each prompt.
- An **attempt** is one provider request for a turn; stream retries increase the
  attempt number without creating another turn.
- **Maintenance** calls such as compaction, cache prewarming, handoff summaries,
  and optional branch summaries are accounted separately and never increment
  the turn count.

While a prompt runs, status uses `[turn: N … │ prompt …]`; completion uses
`[prompt: N turns …]`.

## Mid-prompt steering

By default, input you submit with Enter while a prompt is running is
**steered**: it is injected as a user message before the *next* model request
(that is, between turns — the next time harness sends data back to the model)
rather than waiting for the prompt to end. This lets you redirect a
running prompt ("no, use the other file", "stop, you're overcomplicating it")
without canceling and re-typing.

- Only model-bound input is steered. `!shell`, `/commands`, and `/edit`
  submitted during a prompt keep the queued behavior and run at the next idle
  prompt.
- A steer does not consume a `max-turns` slot and resets the loop-guard
  streaks, so redirecting is never penalized.
- If the prompt ends without another turn to inject into (a plain answer, or a
  budget/cancel break), the steer is recovered and run as the next prompt, so the
  input is never lost.
- `^C` / double-Esc cancel the in-flight prompt.

`--no-steer` (or `no_steer` / `HARNESS_NO_STEER`) restores the original
behavior: input submitted during a prompt is queued as the next prompt.
Steering is interactive-REPL only (not one-shot mode).

## Plan and implementation handoff

The `plan` agent investigates and designs without modifying the project. It can
`record_plan` to persist a plan as markdown under the session (a durable,
human-diffable artifact), and `request_implementation` to ask to hand the plan
off to an implementation agent. The handoff is interactive and user-approved;
when approved, harness switches agents and immediately starts implementation
from the recorded plan:

```text
/handoff [-a agent] [-m model] [message]
                   review the recorded plan and, on approval, switch this session
                   to an implementation agent with a clean context seeded by the
                   plan, the displayed handoff brief, and optional user guidance
```

`record_plan` is available to every default agent; the handoff is plan-only and
unavailable in one-shot mode. `request_implementation` accepts a required brief
and optional target agent; model selection comes from the target agent's
configuration. The target defaults to `auto`; override it with
`--handoff-agent <name>`, `HARNESS_HANDOFF_AGENT`, the `handoff_agent` config
key, or `/handoff -a <agent>`. A manual `/handoff -m <model>` applies a one-off
model override, and trailing `message` text is added to the implementation
context separately from the generated or tool-supplied brief. Harness displays
the brief before asking for approval. Any agent can pin its own thinking effort
with a per-agent `reasoning` field in config, so a fast/cheap implementation
agent can pair a smaller model with lower effort.

## Documentation

- [Usage reference](docs/usage.md): flags, config, provider selection, one-shot
  mode, REPL commands, agents, sessions, compaction, interrupts, and hooks.
- [Tools](docs/tools.md): built-in tools, rich image results, delegation,
  background jobs, truncation, and tool artifacts.
- [MCP](docs/mcp.md): configuring and running `harness-mcp-proxy`.
- [LSP](docs/lsp.md): optional code intelligence tools, including independent Serena support.
- [Release](docs/release.md): release artifacts, tagging, and required secrets.
- [Design](docs/design.md): architecture and implementation details.
- [Smoke tests](docs/smoke.md): end-to-end verification matrix.
- [Deterministic-flow benchmark](docs/flowbench.md): historic session patterns,
  paired live-model protocol, results, and promotion decisions.
