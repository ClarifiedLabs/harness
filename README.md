# harness

A minimal agentic coding harness in Go: a plain-text, line-oriented CLI that
supports basic tool use, delegate sub-agents, skills, and MCP.

## What it is

- **Zero third-party Go dependencies.** Go standard library only.
- **No sandbox, no permission prompts.** `harness` assumes it is launched in an
  environment that is already sandboxed enough; tools run with the process's
  privileges.
- **Isolated provider and MCP access.** `harness` uses separate services for
  model providers and MCP. Those services can run outside the agent's sandbox, so
  API keys, OAuth tokens, and MCP credentials do not need to be available to the
  agent process.

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
the model proxy listens on `127.0.0.1:8765`.

Use `harness --models` to list the providers, models, and cataloged reasoning
controls exposed by the configured proxy. Use `harness --agents` to list the
configured agents.

MCP is optional. After configuring downstream servers for `harness-mcp-proxy`,
start it separately and enable MCP for `harness`:

```sh
harness-mcp-proxy serve
HARNESS_MCP_ENABLE=true harness -provider <provider> -model <model>
```

## Process model

```text
+----------------------- sandboxed project environment ------------------------+
|                                                                              |
|  harness CLI                                                                 |
|  - reads and writes local files                                               |
|  - runs shell, git, and tool commands with this process's privileges          |
|                                                                              |
+----------------------+-------------------------------+-----------------------+
                       | Model Provider                | MCP (Optional)
                       |                               |
                       v                               v
+---------------------- outside sandbox / host environment ---------------------+
|                                                                              |
|  +------------------------+        +------------------------+                 |
|  | harness-model-proxy    |        | harness-mcp-proxy      |                 |
|  | provider config/auth   |        | MCP server config/auth |                 |
|  | model catalog/dialects |        | stdio/HTTP MCP clients |                 |
|  +-----------+------------+        +-----------+------------+                 |
|              |                                 |                              |
|              v                                 v                              |
|       model providers                   MCP servers/services                  |
|                                                                              |
+------------------------------------------------------------------------------+
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
