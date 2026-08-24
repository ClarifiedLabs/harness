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
The shared HTTP client stores reverse-proxy cookies in memory and returns them on
later requests to the matching origin, allowing cookie-based affinity for an MCP
proxy behind a load balancer. Cookies last for the harness process and are not
persisted across restarts.

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
    "logFormat": "json",
    "metrics": {
      "enabled": true,
      "listen": "127.0.0.1:9091"
    }
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

`${NAME}` references in downstream server command, URL, argument, environment,
header, and authentication string values are expanded from the proxy's
environment. Invalid server entries are skipped with a warning; the proxy still
serves the valid ones. See `examples/harness-mcp-proxy/config.json` for a
copyable starting point.

For the GitHub remote MCP example, run `gh auth login` first; the proxy uses
`gh auth token` to fetch the bearer token.

Stdio servers inherit the proxy's full environment plus the per-server `env`
overrides. Do not configure untrusted stdio servers when secrets live in the
environment, since the child process can read them.

### Proxy commands and configuration inspection

`harness-mcp-proxy --help` renders the generated root command catalog; use
`harness-mcp-proxy <command> --help` for command-scoped flags. The available
commands are `serve`, `tools`, `auth`, `generate-api-key`, `config`, and
`version`.

The `config` group is offline and non-mutating:

```text
harness-mcp-proxy config list  [-format text|json|markdown]
harness-mcp-proxy config show  [-config path] [-format text|json] [-sources] [setting flags]
harness-mcp-proxy config check [-config path]
```

An explicit `-config` path wins over conventional-path discovery. Setting flags
then override file values; source-specific empty-value behavior is preserved,
including inverse `-no-metrics[=true|false]` handling and explicit empty
listeners retaining their effective defaults. Relative API-key and OAuth token
paths from the file are based on the config directory, while explicit
command-line paths (and the legacy `proxy.logFile` path) are based on the
working directory. Invalid child servers remain warnings
and are skipped in deterministic order rather than invalidating otherwise usable
configuration.

`config list` does not load a file. `config show` performs normal `${NAME}` and
`${NAME:-default}` expansion in downstream server command, URL, argument,
environment, header, and authentication string values; it may emit the same
unset-variable or skipped-server warnings as serving. Its versioned JSON
projection exposes only server names, counts, and transport kinds: command
arguments, environment/header
values, URLs (including credentials and query strings), auth configuration,
API-key contents, and OAuth tokens are never shown. `config check` validates the
same local configuration without opening listeners, starting stdio children, or
mutating files.

#### MCP-proxy configuration parameters

<!-- mcp-proxy-config-parameters:start -->
| Key | Type | Accepted | Flags | Environment | JSON path | Default | Sensitive | Description |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `mcp_servers` | `object` | - | - | - | `mcpServers` | {} | yes | Downstream MCP server definitions. |
| `listen` | `string` | - | `-listen` | - | `proxy.listen` | "127.0.0.1:8766" | no | HTTP listen address. |
| `log_file` | `path` | - | `-log` | - | `proxy.logFile` | stderr | no | Log file path; empty writes to stderr. |
| `log_level` | `string` | `debug`, `info`, `warn`, `warning`, `error` | `-log-level` | - | `proxy.logLevel` | "info" | no | Minimum log level. |
| `log_format` | `string` | `json`, `text` | `-log-format` | - | `proxy.logFormat` | "json" | no | Log output format. |
| `api_keys_file` | `path` | - | `-api-keys-file` | - | `proxy.api_keys_file` | derived: api_keys.json beside the selected config | no | File containing accepted proxy API-key hashes. |
| `metrics_enabled` | `boolean` | - | `-no-metrics` | - | `proxy.metrics.enabled` | true (The no-metrics flag inversely controls this setting) | no | Whether the Prometheus metrics endpoint is enabled. |
| `metrics_listen` | `string` | - | `-metrics-listen` | - | `proxy.metrics.listen` | "127.0.0.1:9091" | no | Prometheus metrics listen address. |
<!-- mcp-proxy-config-parameters:end -->

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
- Prometheus metrics: `http://127.0.0.1:9091/metrics` in HTTP mode

### Prometheus metrics

HTTP mode enables a separate Prometheus text-exposition endpoint by default at
`127.0.0.1:9091`. It serves only unauthenticated `GET /metrics`; it is not part of
the MCP listener or its API-key middleware. Keep it on a trusted interface, or
apply network-level controls around it. Configure the endpoint under
`proxy.metrics` with `enabled` and `listen`, and override config with
`serve -no-metrics[=true|false]` or `serve -metrics-listen addr`. Flags take
precedence over config, then the defaults apply.

The endpoint exports these tool-call counters:

- `mcp_proxy_requests_total`
- `mcp_proxy_errors_total`
- `mcp_proxy_request_bytes_total`
- `mcp_proxy_response_bytes_total`
- `mcp_proxy_request_duration_seconds_total`

Routed series are labeled only by `mcp` (downstream server), `tool` (bare tool
name), and `key` (the authorizing API key's stored name, or `anonymous`). An
unknown qualified tool is not parsed, so its series omits `mcp` and `tool`.
`mcp_proxy_build_info{version="..."} 1` carries only the `version` label.
Request bytes are the raw tool argument bytes; response bytes are the
JSON-marshaled MCP `CallToolResult`, not the outer HTTP/JSON-RPC envelope.
Errors include unknown/downstream failures and results with `isError: true`.
Caller cancellation still records the request, bytes, and duration, but does not
increment errors. Requests rejected before routing—such as malformed JSON-RPC,
missing sessions, unsupported methods, or failed API-key auth—are outside these
tool-call metrics.

A bind failure for an address selected by config or flag is fatal. Failure to
bind the implicit default logs a warning and leaves the primary MCP proxy
running. `-no-metrics` disables both the endpoint and collection. In
`serve -stdio` mode all metrics flags and config are inert: no listener opens,
collector families are not registered, and tool calls are not recorded.

Inspect the live surface without harness with:

```sh
harness-mcp-proxy tools
harness-mcp-proxy tools -proxy http://127.0.0.1:8420
```

`harness-mcp-proxy --version` and `harness-mcp-proxy version` both print the
release version plus the MCP protocol version.

### Request logging and tracing

The MCP proxy logs one structured record per routed `tools/call` with
requester/clientInfo, downstream MCP server name, bare and qualified tool
name, request/response bytes, duration, `is_error`, and any protocol error.
Unknown tools are warning records. When a valid W3C `traceparent` is present,
the log appends `trace_id`, `span_id`, `parent_span_id`, and `trace_sampled`
(plus `tracestate` when present). The proxy carries inbound trace metadata in
`context.Context`; downstream HTTP MCP servers receive a child `traceparent`
with the same trace id, while stdio downstream servers have no header channel
and are correlated only by proxy logs.

On SIGINT/SIGTERM the daemon shuts down gracefully: HTTP sessions close with
the server, and each stdio child is reaped (close stdin → SIGTERM → SIGKILL on
the process group, bounded by per-stage timeouts).

## Proxy API-key authentication

API-key authentication is disabled by default and becomes required as soon as
the first key is stored in the proxy's dedicated API-key file. The default is
`api_keys.json` next to the proxy config. Set `proxy.api_keys_file` to select
another path or use `serve -api-keys-file path` to override it. Inline
`proxy.api_keys` entries in the normal config are rejected.

Generate and store a key, then provide it to harness:

```sh
harness-mcp-proxy generate-api-key [-api-keys-file path] [-ttl 720h] laptop
HARNESS_MCP_PROXY_API_KEY=<key> HARNESS_MCP_ENABLE=true harness -model <provider>:<model>
```

Harness also reads `mcp.api_key` from `~/.config/harness/config.json`. MCP proxy
keys have the `hmcpp_` prefix. Only SHA-256 hashes are stored, and the plaintext
key is printed once. Omit `-ttl` (or use `0`) for a non-expiring key. A running
HTTP proxy polls its key file for additions and removals; harness loads its
outgoing key at process start. The `harness-mcp-proxy tools` diagnostic command
uses `HARNESS_MCP_PROXY_API_KEY` or `tools -api-key <key>`.

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

A direct MCP image content item is preserved as an image-bearing rich tool
result when it is valid PNG, JPEG, WebP, or non-animated GIF data. Harness calls
the remote tool exactly once, validates its declared MIME type and data with the
same 10 MiB per-image and 32 MiB aggregate encoded-request limits used for local
images, and sends valid images through the provider-neutral tool-result path.
Invalid image data, unsupported MCP content kinds, and over-limit batches become
bounded textual placeholders instead of raw payloads; MCP error results remain
text-only. Terminal summaries, hooks, artifacts, replay previews, and logs expose
only text or safe image metadata, never image base64.

Each agent's `mcp_tools` setting controls automatic exposure: `disabled`,
`read_only`, or `all`. Explicit `mcp__` names in `allowed_tools` are still
allowed as a strict whitelist. One-shot runs use the tool list discovered before
the model request; REPL runs may gain remote MCP tools after background discovery
succeeds.

When one agent's combined optional MCP/LSP declarations exceed 32 KiB, Harness
keeps the tools registered but initially publishes only `tool_catalog`. Its
`list` action filters names and descriptions, `describe` returns exact schemas,
and sequential `activate` publishes up to 16 selected direct schemas on the next
model turn. This reduces repeated prompt overhead; it does not bypass the agent's
MCP policy or `allowed_tools` checks.

When MCP is enabled, Harness trusts `readOnlyHint` annotations from the
configured MCP server path so advertised read-only tools can be exposed to
`read_only` agents. All MCP calls inherit default-parallel scheduling regardless
of that policy annotation; Harness does not infer remote side effects.

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
