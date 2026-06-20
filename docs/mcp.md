# MCP Servers

Harness can expose tools from [Model Context Protocol](https://modelcontextprotocol.io)
servers. The usual remote path uses `harness-mcp-proxy`, which owns downstream
MCP servers and aggregates their tools into one namespaced surface. Harness
connects to that proxy over HTTP and registers each tool as an ordinary harness
tool.

Harness and the proxy speak MCP streamable HTTP (JSON-RPC 2.0, revision
`2025-06-18`). The proxy is a shared daemon that many harness sessions can reuse.
Harness never starts that daemon for you. A separate `mcp.local` path can
explicitly spawn one configured local stdio MCP service from the harness process.

## Enabling MCP

MCP is opt-in and off by default. Turn it on in
`~/.config/harness/config.json`:

```json
{
  "mcp": {
    "enable": true,
    "proxy": ""
  }
}
```

Or use environment variables:

```sh
HARNESS_MCP_ENABLE=true
HARNESS_MCP_PROXY=http://127.0.0.1:8766
```

There are no flags. An empty `proxy` resolves to `http://127.0.0.1:8766`.
Precedence is **env > config file > default**. `proxy` must be an `http(s)://`
URL. The separate `mcp.local.enable` setting has its own env override,
`HARNESS_MCP_LOCAL_ENABLE`.

## Configuring Downstream Servers

The proxy has its own config file, separate from harness:

- `$XDG_CONFIG_HOME/harness-mcp-proxy/config.json`
- `~/.config/harness-mcp-proxy/config.json`

The shape is Claude Code-compatible:

```json
{
  "mcpServers": {
    "fs": {
      "command": "mcp-server-filesystem",
      "args": ["--root", "/srv/data"],
      "env": { "LOG_LEVEL": "info" }
    },
    "search": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": { "X-Workspace": "prod" },
      "auth": {
        "type": "token_command",
        "command": "provider-cli",
        "args": ["mcp", "token"]
      }
    },
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "X-MCP-Toolsets": "context,repos,issues,pull_requests,actions",
        "X-MCP-Readonly": "true"
      },
      "auth": {
        "type": "token_command",
        "command": "gh",
        "args": ["auth", "token"],
        "cache_ttl_seconds": 300
      }
    }
  },
  "proxy": {
    "listen": "127.0.0.1:8766",
    "logFile": "",
    "logLevel": "info",
    "logFormat": "json"
  }
}
```

`proxy.listen` defaults to `127.0.0.1:8766`. Set it to another address such as
`127.0.0.1:8420` when you need a different port or host. Proxy logs use JSON by
default (`proxy.logFormat: "json"`) or built-in slog text format (`"text"`).

A server with no `type` or `"stdio"` is a child process. `"http"` or
`"streamable-http"` is a streamable-HTTP endpoint. HTTP `auth` uses the same
`token_command` and `oauth2` shapes as model providers. Run
`harness-mcp-proxy auth login <server>`, `auth status <server>`, or
`auth logout <server>` for servers configured with built-in OAuth.

`${NAME}` references in any string are expanded from the proxy's environment.
Invalid server entries are skipped with a warning; the proxy still serves the
valid ones. See `examples/harness-mcp-proxy/config.json` for a copyable starting
point.

For the GitHub remote MCP example, run `gh auth login` first; the proxy uses
`gh auth token` to fetch the bearer token.

Stdio servers inherit the proxy's full environment plus the per-server `env`
overrides. Do not configure untrusted stdio servers when secrets live in the
environment, since the child process can read them.

## Running The Proxy

For the HTTP proxy path, run the proxy once and leave it up:

```sh
harness-mcp-proxy serve
```

For a persistent setup, run it from your shell profile, a launchd agent on macOS,
or a systemd user unit on Linux.

When MCP is enabled for a one-shot run, harness connects to the proxy and
registers the proxy's tools under a 5 second timeout before the model request.
If the connection or registration fails it emits one warning and continues with
no MCP tools:

```text
mcp: cannot connect to proxy at http://127.0.0.1:8766: <err>; MCP tools unavailable
```

MCP never fails harness startup. In the interactive REPL, remote HTTP MCP
discovery starts in the background; failures log a retrying warning, startup
continues immediately, and discovered tools are applied at a later prompt
boundary.

Default paths:

- Proxy URL: `http://127.0.0.1:8766`
- Config: `$XDG_CONFIG_HOME/harness-mcp-proxy/config.json`, else
  `~/.config/harness-mcp-proxy/config.json`
- Log: stderr unless `proxy.logFile` or `serve -log` is set

Inspect the live surface without harness with:

```sh
harness-mcp-proxy tools
harness-mcp-proxy tools -proxy http://127.0.0.1:8420
```

`harness-mcp-proxy --version` prints the release version;
`harness-mcp-proxy version` prints the release version plus MCP protocol version.

## Proxy HTTP Details

The proxy serves its merged MCP surface over streamable HTTP. Set `proxy.listen`
in the proxy config, or pass `serve -listen`, to change the default listener:

```json
{ "proxy": { "listen": "127.0.0.1:8420" } }
```

```sh
harness-mcp-proxy serve -listen 127.0.0.1:8420
```

The listener speaks plain HTTP only. Put a reverse proxy such as nginx or Caddy
in front for TLS and stronger auth. Each session is carried by an
`Mcp-Session-Id` header with a 30-minute idle TTL. Responses are
`application/json` only, and there is no server-push channel.

On the harness side, point `mcp.proxy` at the URL. For an MCP proxy behind a
reverse proxy that wants auth, add a config-file-only `mcp.headers` map sent on
every request:

```json
{
  "mcp": {
    "enable": true,
    "proxy": "https://mcp.internal.example/mcp",
    "headers": { "Authorization": "Bearer ${TOKEN}" }
  }
}
```

`headers` has no environment variable. Header values expand `${VAR}` and
`${VAR:-default}`. An unset `${VAR}` is a config error.

## Tools, Agents, And Limits

Aggregated tools are named `mcp__<server>__<tool>`. Names must fit
`[a-zA-Z0-9_-]{1,64}`; names that do not are dropped with a warning. They are
plain harness tools, so they flow through normal truncation, artifact, and
session paths.

Each agent's `mcp_tools` setting controls automatic exposure: `disabled`,
`read_only`, or `all`. Explicit `mcp__` names in `allowed_tools` are still
allowed as a strict whitelist. One-shot runs use the tool list discovered before
the model request; REPL runs may gain remote MCP tools after background discovery
succeeds.

When MCP is enabled, harness trusts `readOnlyHint` annotations from the
configured MCP server path, so tools advertised as read-only can be exposed to
`read_only` agents and join read-only parallel dispatch.

Two config-file-only keys (under the harness `mcp` block, no flag or env var)
restrict the auto-exposed remote MCP surface:

```json
{
  "mcp": {
    "enable": true,
    "max_tools": 64,
    "disabled_servers": ["noisy-server"]
  }
}
```

- `mcp.max_tools` caps how many discovered remote MCP tools are auto-exposed to
  `read_only`/`all` agents. `0` (the default) means unlimited; a negative value is
  rejected. On overflow the surface is truncated in discovery order and a warning is
  logged. Local (`mcp.local`) and LSP tools are not counted.
- `mcp.disabled_servers` lists remote MCP server names (the segment between `mcp__`
  and the next `__` in a tool name) whose tools are dropped from auto-exposure.

Both limits affect only automatic exposure; an explicit `mcp__…` name in an agent's
`allowed_tools` whitelist still resolves even if the cap or disable list excluded it.

Leave MCP off, the default, for latency-sensitive one-shot invocations that do
not need it.

## V1 Non-Goals

- Tools only; no MCP resources or prompts.
- Streamable HTTP only for remote servers; no legacy HTTP+SSE transport.
- OAuth discovery and dynamic client registration.
- Plain HTTP proxy listener; use a reverse proxy for TLS.
