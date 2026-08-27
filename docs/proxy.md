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

Harness retries transient connection failures and retryable provider responses
such as 429, 500, 502, 503, and 529. A `Retry-After` value or equivalent
streaming error hint is honored when it is at most 60 seconds. Longer 429/529
waits fail immediately with the original provider message so an interactive
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

Harness sends standard W3C `traceparent` headers. Proxy logs that receive a
valid trace include `trace_id`, `span_id`, `parent_span_id`, and
`trace_sampled` fields. Tracing does not log prompts, request bodies, API
keys, or authentication headers.

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
