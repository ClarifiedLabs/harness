# MCP Servers

Harness exposes tools from [Model Context Protocol](https://modelcontextprotocol.io)
servers. Configure any number of named services under `mcp.servers`; local stdio
commands and remote Streamable HTTP endpoints can run simultaneously. No enable
switch is needed: an empty map (the default) starts no services. Each entry may
set `"enabled": false` to keep its connection details without starting or connecting.
Omitted `enabled` (or `true`) enables the service. Disabled entries do not expand
environment references, so unused credentials need not be present. This works in
both Harness's `mcp.servers` and the standalone proxy's `mcpServers`.

Harness reuses `internal/mcpproxy` in-process over in-memory stdio. The aggregator
needs no `harness-mcp-proxy` subprocess, TCP listener, metrics listener, or separate
proxy config. Model backend selection (`local` or `remote`) is independent.

## Configuring Harness services

In `~/.config/harness/config.json` (or a project `.harness/config.json`):

```json
{
  "mcp": {
    "servers": {
      "maestro": {
        "command": "maestro",
        "args": ["mcp"],
        "env": { "LOG_LEVEL": "info" },
        "excludedTools": ["run_on_cloud"]
      },
      "search": {
        "url": "https://mcp.example.com/mcp",
        "headers": { "X-Workspace": "prod" },
        "auth": {
          "type": "token_command",
          "command": "provider-cli",
          "args": ["mcp", "token"]
        }
      }
    },
    "max_tools": 0,
    "disabled_servers": []
  }
}
```

Each value uses the proxy's `ServerConfig` shape: `command`, `args`, and `env`
for stdio; `url`, `headers`, and `auth` for Streamable HTTP. `type` is optional:
a nonempty URL infers HTTP, otherwise stdio. Explicit `stdio`, `http`, and
`streamable-http` types are accepted; stdio and HTTP fields cannot be mixed in
one entry. Legacy HTTP+SSE is not supported. Existing `token_command` and
`oauth2` authentication work; this adds no OAuth discovery or dynamic client
registration. `excludedTools` uses the exact pre-namespace downstream tool names,
with the same semantics as [proxy exclusions](#excluding-individual-tools).

Command, argument, environment, URL, header, and auth strings expand `${NAME}`
and `${NAME:-default}` once during config loading, through `LoadOptions.LookupEnv`.
An unset reference without a default is an error; the runtime does not expand
resolved strings again. `excludedTools` is literal. Project entries merge by
server name: a same-name project entry replaces the whole global entry, not
individual fields. Each server retains its owning file's provenance and config
directory (including relative auth paths). Child processes run in the session
workspace and inherit the environment plus their configured `env` overrides.
Treat project configuration and commands as trusted code. `config show` redacts
MCP commands, arguments, URLs, headers, auth, and environment values, since any of
these can contain interpolated credentials.

Tools use `mcp__<server>__<tool>` names. Invalid or overlong qualified names are
skipped, never truncated. Dispatch routes to the original downstream name. An
external proxy is an ordinary service: its already-qualified names receive the
configured service prefix too (for example, `mcp__shared__mcp__fs__read`). Keep
service names short enough for the 64-character qualified-name limit. Service
names must use ASCII letters, digits, underscores, or hyphens; they cannot contain
`__` or end with `_`, which would make namespace boundaries ambiguous.

### Migration from older configuration

Legacy `mcp.enable`, `mcp.proxy`, `mcp.api_key`, `mcp.headers`, and `mcp.local`
are automatically converted in memory on load; the original file is not changed.
The proxy becomes `mcp.servers.shared`, and the local command becomes
`mcp.servers.local`. Credentials, arguments, and environment values are retained;
a proxy API key becomes `headers.Authorization` with a `Bearer ` prefix. Legacy
connections stay disabled unless their old `enable` was true. Existing named
entries are never overwritten: migration rejects name collisions.

To persist the conversion explicitly:

```sh
harness config migrate -config ~/.config/harness/config.json
```

The command validates the file without expanding environment references, writes
an exact-byte private backup at `<path>.pre-mcp-migration.bak`, and atomically
replaces the config with private permissions. It refuses an existing backup;
already-current configs are unchanged. It prints paths, never credentials.
Without `-config`, it selects `HARNESS_CONFIG` or the conventional global config;
legacy project `.harness/config.json` files are refused because translating them
alone would lose inherited global fields. Consolidate those settings into named
entries manually. Automatic loading still merges legacy global/project fields
before translation. Symlink paths are refused to avoid replacing a managed
symlink; select its real target instead. `--mcp-proxy-api-key` is removed.

All `lsp` config and native LSP/Serena integrations, the `harness lsp` command,
and `/lsp` are removed. External language-tool MCP services can be configured as
ordinary named entries; Harness no longer supplies language servers, a shim,
prewarming, mutation diagnostics, or special runtime hints.

The old environment variables are rejected with migration errors; remove them:
`HARNESS_MCP_ENABLE`, `HARNESS_MCP_PROXY`, `HARNESS_MCP_PROXY_API_KEY`,
`HARNESS_MCP_LOCAL_ENABLE`, `HARNESS_LSP_ENABLE`, `HARNESS_LSP_PREWARM`, and
`HARNESS_LSP_SERENA_ENABLE`. This rejection applies even to empty values.
`mcp.max_tools` and `mcp.disabled_servers` remain supported across the unified
stdio/HTTP surface.

### Discovery and readiness

Startup waits for all services' first discovery attempts within one shared
five-second readiness budget, then registers currently available tools. Slow or
failed services do not block the session indefinitely. Remote initial discovery
retries are bounded to five attempts, with a 30-second timeout per attempt;
these background attempts may outlive the startup budget. Late tools refresh at
prompt boundaries, not in the middle of a model request. An empty catalog is a
valid ready result. Invalid service configuration is an error, not a silently
ignored server.

### ACP attachments

ACP `session/new` accepts up to 32 stdio or HTTP MCP attachments and advertises
HTTP support. SSE attachments are rejected. Attachment names must be unique and
must not conflict with configured service names; conflicts are fatal before any
service starts. A same-name attachment for a disabled configured service is
suppressed rather than re-enabling it. HTTP headers are literal and are not environment-expanded.
Attachments join the same in-process aggregator, and stdio children use the ACP
session's workspace directory.

### Owned-process cleanup

The Harness root owns the aggregator, its connection, and local stdio children;
ordinary delegates share them. Children shut down when the root exits, not when
each delegate finishes. Connection closure is terminal at owner shutdown: late
calls cannot reconnect. Remote services are not owned and survive client exit.

Cleanup closes stdio, then escalates to TERM/KILL for owned process groups and
joins reaping, including earlier reconnect cleanup. The immediate child exiting
does not leave group members running. A local service gets up to 15 seconds of
EOF grace so a nested proxy can finish concurrent downstream cleanup; an
unresponsive service can delay exit. SIGKILL/crashes cannot run cleanup, and
third-party descendants that detach need their own external supervisor.

## Optional standalone proxy

`harness-mcp-proxy` remains independently useful as a shared daemon or stdio
service. Harness does not start a shared daemon automatically. Configure it as
an ordinary named service, either over HTTP:

```json
{"mcp":{"servers":{"shared":{"url":"http://127.0.0.1:8766"}}}}
```

or as an owned command:

```json
{"mcp":{"servers":{"shared":{"command":"harness-mcp-proxy","args":["serve","-stdio","-config","/path/to/proxy.json"]}}}}
```

The external proxy's own downstream config and operations follow. HTTP uses MCP
JSON-RPC 2.0 revision `2025-06-18`. The HTTP client keeps reverse-proxy cookies
in memory for affinity and does not persist them across restarts.

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

A server with `"stdio"` is a child process; omitted `type` infers HTTP when
`url` is set and stdio otherwise. `"http"` or
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

### Excluding individual tools

Add `excludedTools` to a downstream server entry to remove selected tools from
both the proxy's `tools/list` and `tools/call` routing. For example, to keep
Maestro's local tools without its cloud tools:

```json
{
  "mcpServers": {
    "maestro": {
      "command": "maestro",
      "args": ["mcp"],
      "excludedTools": [
        "describe_cloud_run",
        "get_cloud_run_status",
        "list_cloud_devices",
        "run_on_cloud"
      ]
    }
  }
}
```

Entries match exact, case-sensitive names advertised by that server, **before**
the proxy adds `mcp__<server>__`. There is no glob matching or environment-variable
expansion in this list. Omitted, `null`, or empty lists exclude nothing; duplicate
entries and names the server does not advertise are harmless. Exclusions apply
only to that server and work with both stdio and HTTP downstream connections,
regardless of whether the proxy itself serves HTTP or stdio. They remain in force
when tools refresh or a downstream server reconnects. If an advertised excluded
tool's qualified name collides with another server's tool, the proxy omits that
qualified name entirely rather than redirecting calls to the other tool.

Restart the proxy after editing its config, then start a new Harness session to
rediscover the filtered surface. Excluded tools cannot be restored through an
agent's `allowed_tools`: the proxy rejects calls to them as unknown tools. This
setting works in both the proxy's `mcpServers` and Harness's `mcp.servers` entries.

### MCP Apps passthrough

Both `serve` and `serve -stdio` preserve tool and tool-result `_meta`, including
MCP Apps UI metadata, and preserve raw `structuredContent`. Proxy supervisor
clients advertise the `io.modelcontextprotocol/ui` extension to downstream
servers. Ordinary Harness MCP clients do not claim UI hosting.

The proxy retains the full catalog, including tools whose
`_meta.ui.visibility` omits `"model"`, so app hosts can authorize widget calls.
Harness's model-facing registration omits those app-only tools. Missing
visibility defaults to model plus app. Configured `excludedTools` and their UI
resource routes are still omitted by the proxy.

The proxy advertises resource support but intentionally returns one empty
`resources/list` page: MCP Apps permits UI resources to be discovered only
through tool metadata. `resources/read` routes URIs from `_meta.ui.resourceUri`
and the deprecated `_meta["ui/resourceUri"]`. A unique URI needs no hint. When
multiple servers advertise the same URI, `_meta.sourceTool` must contain the
full namespaced tool name (for example, `mcp__one__show`) so the read reaches
that exact server. Unknown and ambiguous reads fail with invalid-params errors.
Resource HTML, MIME types, and result/content `_meta` pass through unchanged.
The proxy does not otherwise aggregate downstream resource catalogs.

Widget `tools/call` uses the same `_meta.sourceTool` hint. The proxy requires the
source and target routes to belong to the same downstream server and requires
`"app"` visibility on the target. Bare companion-tool names resolve only through
that source server's exact route records, so names containing `__` cannot cause
cross-server routing. Unknown, model-only, and cross-server app calls fail with
invalid-params errors.

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

Add the daemon's URL to a named Harness service as shown above. Harness's
[discovery budget](#discovery-and-readiness) applies to that service too; a
shared daemon and its downstream children survive a Harness client exiting.

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
the process group, bounded by per-stage timeouts). HTTP mode also closes the
metrics listener before returning, including when the main listener fails to
start, allowing up to five seconds for active scrapes to finish.

## Proxy API-key authentication

API-key authentication is disabled by default and becomes required as soon as
the first key is stored in the proxy's dedicated API-key file. The default is
`api_keys.json` next to the proxy config. Set `proxy.api_keys_file` to select
another path or use `serve -api-keys-file path` to override it. Inline
`proxy.api_keys` entries in the normal config are rejected.

Generate and store a key, then provide it to harness:

```sh
harness-mcp-proxy generate-api-key [-api-keys-file path] [-ttl 720h] laptop
# Configure the generated key in the named service header shown below.
harness -model <provider>:<model>
```

Configure Harness with `mcp.servers.shared.headers.Authorization` set to
`"Bearer ${MCP_PROXY_KEY}"` and export `MCP_PROXY_KEY` before starting it. Proxy
keys have the `hmcpp_` prefix. Only SHA-256 hashes are stored, and the plaintext
key is printed once. Omit `-ttl` (or use `0`) for a non-expiring key. A running
HTTP proxy polls its key file for additions and removals; Harness expands its
configured outgoing header at process start. The `harness-mcp-proxy tools`
diagnostic command
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

On the Harness side, configure a named HTTP service. Headers are sent on every
request, including when a reverse proxy provides auth:

```json
{
  "mcp": {
    "servers": {
      "shared": {
        "url": "https://mcp.internal.example/mcp",
        "headers": { "Authorization": "Bearer ${MCP_PROXY_KEY}" }
      }
    }
  }
}
```

Header values follow the once-only config expansion rules above. The standalone
`harness-mcp-proxy tools` command still accepts its own `-api-key` flag and
`HARNESS_MCP_PROXY_API_KEY` environment variable; that variable is no longer a
Harness CLI setting.

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
allowed as a strict whitelist. Temporarily unavailable explicitly allowed MCP
tools are omitted from the live list without losing the configured whitelist;
they return when discovery succeeds. One-shot runs use the tool list discovered before
the model request; REPL runs may gain tools from any service after background
discovery succeeds.

When one agent's combined optional MCP declarations exceed 32 KiB, Harness
keeps the tools registered but initially publishes only `tool_catalog`. Its
`list` action filters names and descriptions, `describe` returns exact schemas,
and sequential `activate` publishes up to 16 selected direct schemas on the next
model turn. This reduces repeated prompt overhead; it does not bypass the agent's
MCP policy or `allowed_tools` checks.

For configured services, Harness trusts `readOnlyHint` annotations from the
configured MCP server path so advertised read-only tools can be exposed to
`read_only` agents. All MCP calls inherit default-parallel scheduling regardless
of that policy annotation; Harness does not infer remote side effects.

Two config-file-only keys (under the harness `mcp` block, no flag or env var)
restrict the auto-exposed unified MCP surface:

```json
{
  "mcp": {
    "max_tools": 64,
    "disabled_servers": ["noisy-server"]
  }
}
```

- `mcp.max_tools` caps how many discovered MCP tools are auto-exposed to
  `read_only`/`all` agents. `0` (the default) means unlimited; a negative value is
  rejected. On overflow the surface is truncated in discovery order and a warning is
  logged. Both stdio and HTTP service tools are counted.
- `mcp.disabled_servers` lists MCP server names (the segment between `mcp__`
  and the next `__` in a tool name) whose tools are dropped from auto-exposure.

Unlike per-entry `enabled: false`, neither limit prevents a service from starting
or connecting. Both limits affect only automatic exposure; an explicit `mcp__…` name in an agent's
`allowed_tools` whitelist still resolves even if the cap or disable list excluded it.

Leave `mcp.servers` empty, the default, for latency-sensitive one-shot
invocations that do not need it.

## V1 Non-Goals

- Narrow MCP Apps UI-resource passthrough only; no general resource aggregation or prompts.
- Streamable HTTP only for remote servers; no legacy HTTP+SSE transport.
- OAuth discovery and dynamic client registration.
- Plain HTTP proxy listener; use a reverse proxy for TLS.
