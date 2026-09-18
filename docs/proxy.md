# Model-proxy operations

This document covers operating `harness-model-proxy` beyond initial setup:
serving lifecycle, probes, rolling updates, harness-to-proxy authentication,
metrics, provider failure behavior, request tracing, and diagnostics.
Installation, `setup`, provider walkthroughs, and configuration inspection
live in [usage.md](usage.md#model-proxy); usage aggregation, pricing, and
cost-budget semantics live in
[usage.md](usage.md#usage-pricing-and-budgets).

## Serving, probes, and rolling updates

The model proxy exposes unauthenticated process probes on its API listener,
outside API-key middleware:

- `GET /readyz` returns `200` normally and `503` as soon as SIGTERM or SIGINT
  starts a drain.
- `GET /healthz` remains `200` until final teardown begins.
- Other methods on either probe path return `405`.

The first termination signal removes readiness, stops background catalog/key
refresh work, waits for load-balancer propagation, and then gracefully closes
the API listener without cancelling in-flight handler contexts. Once the
stream drain reaches its bound, the server force-closes remaining requests. It
then closes the bounded WebSocket pool and shuts down the metrics listener
last.

Lifecycle settings use flag > environment > config > default precedence:

| Purpose | Serve flag | Environment | Config | Default |
|---|---|---|---|---|
| readiness propagation delay | `-drain-delay` | `HARNESS_MODEL_PROXY_DRAIN_DELAY` | `drain_delay` | `5s` |
| maximum stream drain | `-shutdown-timeout` | `HARNESS_MODEL_PROXY_SHUTDOWN_TIMEOUT` | `shutdown_timeout` | `5m` |
| process identity | `-instance-id` | `HARNESS_MODEL_PROXY_INSTANCE_ID` | `instance_id` | random 16-byte hex |

Instance IDs must match `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`. A Kubernetes pod
name or UID is a useful value. It appears in request events, error
diagnostics, `/v1/usage`, and structured logs; correlate a request by
`(proxy_instance_id, proxy_request_id)`.

A minimal Kubernetes fragment is:

```yaml
spec:
  terminationGracePeriodSeconds: 330
  containers:
    - name: model-proxy
      args:
        - serve
        - -listen=0.0.0.0:8765
        - -metrics-listen=0.0.0.0:9090
      env:
        - name: HARNESS_MODEL_PROXY_INSTANCE_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
      readinessProbe:
        httpGet: {path: /readyz, port: 8765}
      livenessProbe:
        httpGet: {path: /healthz, port: 8765}
```

Set `terminationGracePeriodSeconds` greater than the drain delay plus shutdown
timeout; use at least `330` seconds with the defaults. The metrics default
(`127.0.0.1:9090`) is not pod-scrapable, so bind `-metrics-listen 0.0.0.0:9090`
in a pod.

Harness sends `X-Harness-Session` only on stream requests with a session ID.
Use it for consistent hashing when Codex Responses WebSockets are enabled:

```nginx
upstream harness_model_proxy {
    hash $http_x_harness_session consistent;
    server model-proxy-0:8765;
    server model-proxy-1:8765;
}
```

```haproxy
backend harness_model_proxy
  balance hdr(X-Harness-Session)
  hash-type consistent
  server proxy0 model-proxy-0:8765 check
  server proxy1 model-proxy-1:8765 check
```

For Envoy, use a `RING_HASH` cluster and a route header hash policy:

```yaml
route:
  cluster: harness_model_proxy
  hash_policy:
    - header:
        header_name: X-Harness-Session
clusters:
  - name: harness_model_proxy
    lb_policy: RING_HASH
```

See the official [NGINX upstream hash](https://nginx.org/en/docs/http/ngx_http_upstream_module.html#hash),
[HAProxy balancing](https://www.haproxy.com/documentation/haproxy-configuration-manual/latest/#4-balance),
and [Envoy route hash-policy](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/route/v3/route_components.proto#config-route-v3-routeaction-hashpolicy-header)
references for the complete surrounding configuration.

Harness also accepts reverse-proxy affinity cookies. The model-proxy HTTP
client stores them in memory and returns them on later requests to the
matching origin. Cookies are scoped to one harness process and are not
persisted across restarts; cookie affinity therefore pins all model-proxy
traffic from that process, while `X-Harness-Session` supports finer
logical-session routing.

Stickiness improves WebSocket continuation hit rate but is never required for
correctness. HTTP stored continuations work on any replica; a Codex
`store:false` socket miss returns 409 and the CLI resends complete history
once.

For a replicated production deployment, bake `models.dev.api.json` into the
image or mount an identical read-only copy in every pod. Set
`models_dev_cache_ttl: 0` and treat a catalog change as a deployment. Cost
budgets and `/v1/usage` remain deliberately per pod: strict budget enforcement
requires one replica, and sharing one budget-state directory between
independent replicas is unsupported because their read-modify-write cycles are
not coordinated. `/v1/usage` includes `instance` and `since` so that per-pod
reports are explicit.

The continuation ownership change is a coordinated cutover: old and new
CLI/proxy combinations are not wire-compatible. Finish active CLI turns, stop
the CLIs, deploy the complete new proxy fleet without sending traffic through
a mixed-version Service, wait for every pod to become ready, and only then
start/update the CLI. Validate one HTTP stateful session and one Codex
WebSocket session through a forced pod replacement. Future proxy-only rollouts
can use the normal readiness-driven rolling strategy.

## Harness-to-proxy authentication

Model-proxy API-key authentication is disabled by default and becomes required
as soon as the first key is stored in the proxy's dedicated API-key file. The
default is `api_keys.json` next to the proxy config; `api_keys_file` selects
another path and `serve -api-keys-file path` overrides it. Inline `api_keys`
in the normal proxy config are rejected.

Generate and store a key, then provide it to harness:

```sh
harness-model-proxy generate-api-key [-api-keys-file path] [-ttl 720h] [-budget-usd 25 -budget-period 24h] laptop
harness --model-proxy-api-key <key> -model <provider>:<model>
```

Harness also reads `HARNESS_MODEL_PROXY_API_KEY` and the `model_proxy_api_key`
field in `~/.config/harness/config.json`. Model-proxy keys have the `hmp_`
prefix. Only SHA-256 hashes are stored, and the plaintext key is printed once.
Omit `-ttl` (or use `0`) for a non-expiring key. A running proxy polls its key
file for additions and removals; harness loads its outgoing key at process
start.

See [mcp.md](mcp.md#proxy-api-key-authentication) for the equivalent MCP proxy
configuration.

## Subscription quota operations

The [limits commands](usage.md#subscription-limits) use these routes on the
ordinary API listener, **inside the existing API-key middleware**, never the
unauthenticated probe surface:

| Method and route | Request / response |
|---|---|
| `GET /v1/limits?provider=<optional-name>` | One configured subscription or all supported configured subscriptions; returns a `providers` array with independent results/errors in provider-name order. |
| `GET /v1/limits/reset-credits?provider=openai-codex` | Codex reset-credit details, separate from the status query. |
| `POST /v1/limits/reset` | JSON `{ "provider": "openai-codex", "credit_id": "<selected-id>", "request_id": "<stable-id>" }`; all three fields are required. Returns the selected operation's identity and outcome. |

Existing trusted proxy callers can inspect **account-wide** quotas and explicitly
redeem the selected Codex reset credit. There is no new authorization system,
per-key quota partition, reset-specific permission, or confirmation prompt.
If proxy API-key authentication is disabled, the same routes are accessible
without a key: retain the trusted-network deployment boundary. These operations
neither charge nor reset model-proxy cost budgets and do not change `/v1/usage`,
model-generation metrics, or session accounting.

Responses use `Cache-Control: no-store`. CLI/REPL status is fetched on demand,
with at most three concurrent provider queries; it never serves cached metrics.
The separate [quota metrics poller](#subscription-quota-metrics) keeps an in-memory
snapshot but does not persist quota data. A valid query returns HTTP 200 with
per-provider errors, so one upstream outage does not hide the other results.
Credit and reset operation failures likewise use their structured result's
`error`; clients must inspect it, not only the HTTP status. Invalid request or
provider selection returns 400; a wrong method returns 405. Reset request bodies
are bounded to 4 KiB. The normalized response contract is in
[design §7.1](design.md#71-subscription-quota-contract).

Provider credentials are resolved only in the proxy, using the existing shared
sources: dynamic `auth.Source.Headers` > configured `api_key_env` values >
existing dialect environment fallback > inline API key. Each operation captures
one provider/auth snapshot, including its best-effort refreshes; it does not
create a new OAuth source or change inference authentication. Initial support
requires the exact configured provider name and an official HTTPS origin/known
base path. Custom gateways fail with `unsupported_endpoint` rather than sending
their credentials to an assumed official service; no quota endpoint override is
provided.

Each upstream call is bounded to 10 seconds and 1 MiB of response data, with
bounded arrays/strings, no credential-bearing redirects, and no automatic
retries. Only normalized safe fields cross the proxy: no raw upstream bodies,
tokens, account/user IDs, email, or unrelated profile/upsell metadata.
The proxy logs quota request completion at info level even without tracing,
plus per-provider results. Failures and refresh warnings log safe error codes at
warn level; request/response bodies, credentials, credit/request IDs, and raw
upstream errors are not logged.
Provider-controlled labels are sanitized to bounded plain text. Errors use safe
codes such as `missing_auth`, `unauthorized`, `unsupported_endpoint`, `throttled`,
`timeout`, and `invalid_payload`, without echoing upstream bodies or headers.

### Reset identity and outcomes

The proxy forwards `request_id` unchanged as upstream `redeem_request_id`, together
with exactly the selected `credit_id`. The backend owns idempotency; there is no
local persistent redemption ledger, credit auto-selection, or generic model-retry
path. Operators/reverse proxies must not automatically retry these operations.
An explicit retry must keep the same provider/account configuration, credit ID,
and request ID, even if the credit has disappeared from a later listing.

Outcomes are `reset`, `nothing_to_reset`, `no_credit`, and `already_redeemed`;
`windows_reset`, when reported, is an **integer count**, not a list of windows.
A timeout/transport failure or unrecognized response can be `indeterminate` and
must not be described as “no credit consumed.” After `reset` or
`already_redeemed`, status and credit-detail refreshes are best-effort: a refresh
failure adds a warning without replacing the successful outcome. See the
[command reference](usage.md#explicit-codex-resets) for retry commands, REPL
pending-state behavior, and exit codes.

### Upstream compatibility and evidence

These subscription endpoints are unofficial/undocumented integration contracts,
not stable public API guarantees. Implementations follow the pinned sources
below; fixture tests are not proof of current live account compatibility.
Unknown JSON fields are tolerated, but empty/malformed known data is an error,
not a healthy-looking zero-usage report.

- **Kimi:** `GET https://api.kimi.com/coding/v1/usages` (China deployment,
  `kimi-code-plan-cn`) or `GET https://api.kimi.ai/coding/v1/usages` (global
  deployment, `kimi-code-plan-global`), with
  `Authorization: Bearer <coding-key>`. The
  [official Kimi CLI usage implementation](https://github.com/MoonshotAI/kimi-cli/blob/86f136422a0aae6b217ea49e7ea1d2e8a1defcd2/src/kimi_cli/ui/shell/usage.py)
  supplies the weekly `usage` summary and additional `limits` windows. Numeric
  strings, nested/flat details, remaining-derived usage, relative resets, and
  RFC3339 nanosecond timestamps are handled without interpreting quota units as
  literal model tokens.
- **Z.ai:** `GET https://api.z.ai/api/monitor/usage/quota/limit`. Static keys are
  sent directly in `Authorization` **without `Bearer`**, following the
  [official query script](https://github.com/zai-org/zai-coding-plugins/blob/0446d0bb0bc537d97d3ab3664c4b8b9c4a0e1254/plugins/glm-plan-usage/skills/usage-query-skill/scripts/query-usage.mjs);
  Harness does not probe alternative auth formats. Coding Plan token/credit
  quotas and MCP call quotas remain separate. Newer `CREDIT_LIMIT`, multi-period
  windows, `remaining`, envelope checks, and `nextResetTime` in epoch
  **milliseconds** also rely on
  [CodexBar secondary schema evidence](https://github.com/steipete/CodexBar/blob/7d7c7301850827f4469b147639995e02137e9667/Sources/CodexBarCore/Resources/Plugins/zai.js)
  and its [reset tests](https://github.com/steipete/CodexBar/blob/7d7c7301850827f4469b147639995e02137e9667/TestsPlugin/ZaiPluginResetTests.swift).
  **Read-only live account verification is still required for this secondary
  schema.** Not every Coding Plan window is five hours. Duration units are
  day=1, hour=3, minute=5, week=6; MCP's `TIME_LIMIT` unit=5/number=1 marker means
  monthly, not one minute or an exact 30-day reset. Implausible reset dates remain
  provider-reported and may warn; no timezone offset is guessed. China-region,
  team, historical usage, and balance queries are outside this feature.
- **Codex:** the
  [official reset client](https://github.com/openai/codex/blob/105fe8761cd56bb9cf143106435f487f7e902b62/codex-rs/backend-client/src/client/rate_limit_resets.rs)
  and [types](https://github.com/openai/codex/blob/105fe8761cd56bb9cf143106435f487f7e902b62/codex-rs/backend-client/src/types.rs)
  use `GET https://chatgpt.com/backend-api/wham/usage`,
  `GET .../wham/rate-limit-reset-credits`, and
  `POST .../wham/rate-limit-reset-credits/consume`. The quota path is a sibling
  of inference's `/backend-api/codex`, **not** `/backend-api/codex/wham`.
  Existing auth headers include `ChatGPT-Account-ID` and optional FedRAMP routing;
  passive readers do not opt into Luna Reserve. Window `reset_at` is epoch
  **seconds**. Admission flags and independent additional pools are preserved;
  availability of credit details is not required for ordinary quota status.

### Read-only account smoke check

After starting the updated proxy, the user can run these without creating a
model session or consuming a reset credit:

```sh
harness limits
harness limits kimi-code-plan-cn -format json
harness limits zai-coding-plan -format json
harness limits openai-codex -format json
harness limits resets openai-codex -format json
```

Compare each fetch with the same account's native client or dashboard: plan,
separate pools/window periods, provider-defined units, used/remaining values,
reset timestamps/timezones, Codex admission status, and reset-credit IDs/statuses/
expiry. Allow for activity in other clients between observations. Record missing
fields and discrepancies rather than filling them in or correcting timestamps.
Public-source research has not verified these accounts live; in particular,
confirm Z.ai's secondary-evidence fields before declaring live compatibility.
**Do not run the singular `reset` command as a test.** Real redemption is a
separate deliberate user action, not part of the smoke check.

### Subscription quota metrics

With metrics enabled, the proxy fetches all supported configured subscriptions
asynchronously at startup and every `subscription_poll_interval` (default `5m`).
`serve -subscription-poll-interval <duration>` overrides
`HARNESS_MODEL_PROXY_SUBSCRIPTION_POLL_INTERVAL`, then the config field, then the
default. Positive intervals must be at least `1m`; the delay starts after each
refresh completes, including failures, so slow calls cannot queue rapid polls.
Use `0` to disable polling. `-no-metrics` / `metrics.enabled:false` also
disables it. Poll cycles do not overlap, run at most three provider queries in
parallel, and are canceled on shutdown. They reuse the same credentials, bounds,
and normalization as `/limits`; failures never block model generation.

The existing `/metrics` endpoint exports these **gauges**, prefixed with
`model_proxy_subscription_`:

| Suffix | Labels | Value |
|---|---|---|
| `used_percent`, `remaining_percent` | `provider`, `pool`, `window` | Last normalized quota percentages, not model token usage. |
| `used`, `remaining`, `limit` | `provider`, `pool`, `window`, `unit` | Provider-defined quota counts; independent pools are not added together. |
| `window_duration_seconds` | `provider`, `pool`, `window` | Reported window duration. |
| `reset_timestamp_seconds` | `provider`, `pool`, `window` | Automatic reset Unix timestamp; relative resets use fetch time plus reported seconds. |
| `allowed`, `limit_reached` | `provider`, `pool` | Authoritative admission flags as 1/0, never inferred from percentages. |
| `reset_credits_available` | `provider` | Optional reset-credit count from status; not purchased credits. |
| `refresh_success` | `provider` | Whether the last status refresh succeeded (1) or failed (0). |
| `last_refresh_timestamp_seconds` | `provider` | Completion time of the latest refresh attempt. |
| `last_success_timestamp_seconds` | `provider` | Fetch time of the latest successful status. |

Labels use normalized provider/pool/window IDs and units, not plan/display names,
account IDs, credit IDs, or errors. Unknown values are absent; explicit zero/false
is exported. A successful refresh replaces all series for that provider, removing
fields/windows no longer reported. A failed refresh retains the last good quota
values, sets `refresh_success` to 0, and leaves the success timestamp unchanged.
Before the first success there are no quota values or success timestamp. Warnings
such as inconsistent provider counts do not turn a successful query into a
failed refresh; the reported numbers remain uncorrected.

Scrapes only read the in-memory gauges and **never contact providers**. Manual
`/limits` queries and best-effort status refreshes after an explicit reset also
update the gauges, including when polling is disabled. Polling does not list
credit details, redeem credits, retry failed requests, or send model prompts.
For example, graph only fresh, successfully refreshed usage with:

```promql
model_proxy_subscription_used_percent
  and on (provider) (model_proxy_subscription_refresh_success == 1)
  and on (provider) (time() - model_proxy_subscription_last_success_timestamp_seconds < 600)
```

Adjust the freshness threshold to your polling interval. Each proxy instance
polls independently, so replicas add upstream requests; disable polling on
replicas that do not need these metrics. Account-wide quota information is
visible on the existing **unauthenticated metrics listener**. Restrict that
listener to trusted monitoring clients; API-key authorization on `/v1/limits`
does not protect `/metrics`.

## Prometheus metrics

The proxy exposes unauthenticated Prometheus metrics on a separate listener,
`127.0.0.1:9090` by default. Metrics break usage down by `provider`, `model`,
bounded `purpose` (`turn`, `compaction`, `prewarm`, `branch_summary`, or
`unknown`), and `key` (the API key's stored name, or `anonymous` when
authentication is disabled). `model_proxy_build_info` carries the build
version. Token counters are recorded for every stream or native compaction
request that produced usage, priced or not, while
`model_proxy_cost_usd_total` is recorded only when a price is known.
`model_proxy_cache_write_tokens_total` records default-rate writes and
`model_proxy_cache_write_1h_tokens_total` records Anthropic's 1-hour writes.
`model_proxy_prompt_input_tokens_total` is the write-inclusive sum of uncached
input, cache reads, and both cache-write buckets. Compute the token-weighted
cache-read ratio without averaging request percentages:

```promql
sum(rate(model_proxy_cache_read_tokens_total[5m]))
/
sum(rate(model_proxy_prompt_input_tokens_total[5m]))
```

Continuation and transport health use bounded, proxy-observable families:

- `model_proxy_continuation_total{result=...}` records exactly one of
  `not_offered`, `served`, `unavailable`, `rejected_upstream`, or `failed` per
  stream request.
- `model_proxy_ws_pool_events_total{event=...}` records `hit`, `miss`,
  `create`, `evict_lru`, `evict_idle`, `evict_age`, or `overflow`.
- `model_proxy_ws_pool_connections` and `model_proxy_ws_pool_capacity` expose
  current pooled connections and the configured bound.

These families have no API-key or instance label. Prometheus scrape-target
labels identify replicas, and all existing request/usage plus new operational
counters can be summed across targets without double counting client-side
retries. CLI-only resets remain in session diagnostics rather than being
reported back to the proxy.

Use `-no-metrics` to disable the endpoint or `-metrics-listen` to move it. The
equivalent proxy-config `metrics` object accepts `enabled` and `listen`. The
listener has an explicit lifetime and remains available until API draining,
handler teardown, and connection-pool closure have completed.

## Provider failures and retries

For model generation, Harness retries transient connection failures and retryable
provider responses such as 429, 500, 502, 503, and 529. Subscription quota queries
and reset operations are excluded from this retry path. A `Retry-After` value or
equivalent streaming error hint is honored when it is at most 60 seconds. Longer
429/529 waits fail immediately with the original provider message so an interactive
prompt is not silently parked for minutes or hours.

Every unsuccessful upstream attempt is logged by the model proxy, including
attempts followed by a successful retry. Session-side lifecycle records are
described under
[Session diagnostics](usage.md#session-diagnostics); the exact backoff,
stream-retry, and cancellation rules are in
[design section 5.5](design.md#55-errors-and-retries-internalretry).

## Proxy request tracing

Enable opt-in tracing to correlate a harness run across model and MCP proxy
logs:

```sh
harness -trace-proxy -model <provider>:<model>
```

Harness sends standard W3C `traceparent` headers, including on model catalog,
streaming, token-count, compaction, and native-steering requests. Each request gets
its own span. Proxy logs that receive a valid trace include `trace_id`, `span_id`,
`parent_span_id`, and `trace_sampled` fields. Tracing does not log prompts, request
bodies, API keys, or authentication headers.

## Multimodal tool-result compatibility diagnostics

Image-bearing tool results have three separate compatibility layers:

1. **Catalog modality:** the selected target must advertise `image` input.
   Harness rejects a statically image-requiring tool before it reads the file
   when this capability is absent.
2. **Configured dialect:** the provider config's `api_type` selects the wire
   lowering. Anthropic nests images in `tool_result.content`; OpenAI Chat
   emits tool messages followed by one adjacent multimodal user message;
   Responses emits function outputs followed by one adjacent user image item;
   Gemini Interactions emits `function_result.result` text/image content.
3. **Concrete endpoint conformance:** an OpenAI-compatible endpoint can
   reject a valid dialect shape despite catalog metadata. On the final
   non-retryable, targeted rejection, after normal
   continuation/server-tool/output-floor fallbacks, the proxy attaches the
   structured category `multimodal_tool_result_rejected`.

Harness shows one concise compatibility notice with the target, remediation,
proxy request ID, and trace ID when available. It also writes a structured
warning to the session's `diagnostics.ndjson` with prompt/turn/attempt,
sanitized upstream status/code/message, correlation fields, lowering strategy,
and bounded shape metadata. The ordinary error remains available. For
streaming requests the proxy's outer HTTP response can be `200` while the
diagnostic's `api_status_code` records the upstream provider failure.
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
metadata, or learn a persistent endpoint quirk. Select a conforming image
target or inspect the image outside that model call.

## Native steering transport

`POST /v1/steer` routes a submission to a currently active stream using the same
API-key identity, target ID, and `proxy_session_id`. The body contains
`target_id` and `submission` (`id`, `session_id`, optional `correlation_id`, and
user `messages` containing text or images). The route shares ordinary proxy
authentication. Identity is the authenticated credential hash, not its display
name. Pooled transports use the same isolation, so different keys cannot share
a live connection even when their names and session IDs match.
A mismatched or ended binding returns `409`; an unknown target
returns `404`. Both mean the client can retain ordinary queued delivery.

A `202` confirms a local transport write, not provider acceptance or application.
`EventLiveSteer` on `/v1/stream` reports accepted, applied, failed, or lost input.
The capability is advertised only for public Astra Responses targets using
WebSockets. These targets default to WebSockets when `responses_websocket` is
unspecified; an explicit `false` keeps HTTP streaming and disables the capability.
Harness enables `native_steering:true` on eligible streams by default, unless
`astra_native_steering:false` disables it in Harness config.
Native steering is connection-local: multi-instance deployments must route the
steering request to the same instance as the active stream. A transport failure
with uncertain delivery is retained in history rather than blindly retried;
recovery rotates transport affinity before resending that history.
Automatic successor responses are priced individually before usage aggregation.
