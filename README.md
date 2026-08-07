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

- Apple silicon (arm64): [`.pkg` (v0.5.3)](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness_v0.5.3_darwin_arm64.pkg)

### Linux

The latest release, v0.5.3, is available for amd64/x86_64 and arm64/aarch64:

| Binary | amd64 / x86_64 | arm64 / aarch64 |
|---|---|---|
| `harness` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness_0.5.3_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-0.5.3-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness_0.5.3_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-0.5.3-1.aarch64.rpm) |
| `harness-model-proxy` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-model-proxy_0.5.3_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-model-proxy-0.5.3-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-model-proxy_0.5.3_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-model-proxy-0.5.3-1.aarch64.rpm) |
| `harness-mcp-proxy` | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-mcp-proxy_0.5.3_amd64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-mcp-proxy-0.5.3-1.x86_64.rpm) | [`.deb`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-mcp-proxy_0.5.3_arm64.deb) · [`.rpm`](https://github.com/ClarifiedLabs/harness/releases/download/v0.5.3/harness-mcp-proxy-0.5.3-1.aarch64.rpm) |

### Docker

Multi-architecture images are available for Linux amd64 and arm64:

- [`ghcr.io/clarifiedlabs/harness:0.5.3`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness)
- [`ghcr.io/clarifiedlabs/harness-model-proxy:0.5.3`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-model-proxy)
- [`ghcr.io/clarifiedlabs/harness-mcp-proxy:0.5.3`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-mcp-proxy)
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

## Highlights

- Set prompt budgets, turn limits, and tool timeouts to
  [control runaway work](docs/usage.md#prompt-limits-and-lifecycle).
- Redirect work without canceling by
  [steering a running prompt](docs/usage.md#steering-while-a-prompt-runs).
- Resume, inspect, fork, and branch
  [saved conversation trees](docs/usage.md#sessions).
- Plan with a read-only agent, then use an approved
  [implementation handoff](docs/usage.md#planning-and-implementation-handoff).
- Inspect local images and use the built-in, delegated, and background
  [tool set](docs/tools.md), with optional [MCP](docs/mcp.md) and
  [LSP](docs/lsp.md) integrations.

## Documentation

User guides:

- [Usage reference](docs/usage.md): flags, config, provider selection, one-shot
  mode, REPL commands, agents, sessions, compaction, interrupts, and hooks.
- [Tools](docs/tools.md): built-in tools, rich image results, delegation,
  background jobs, truncation, and tool artifacts.
- [MCP](docs/mcp.md): configuring and running `harness-mcp-proxy`.
- [LSP](docs/lsp.md): optional code intelligence tools, including independent Serena support.

Engineering references:

- [Design](docs/design.md): architecture and implementation details.
- [Release](docs/release.md): release artifacts, tagging, and required secrets.
- [Smoke tests](docs/smoke.md): end-to-end verification matrix.
- [Deterministic-flow benchmark](docs/flowbench.md): historic session patterns,
  paired live-model protocol, results, and promotion decisions.
