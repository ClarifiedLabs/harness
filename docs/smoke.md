# Smoke / verification matrix

This document records the smoke matrix for `harness` (design §13) and how to
re-run each leg. It complements — it does not replace — the default unit and
golden suites (`go test ./...`).

The legs split into three groups:

- **Automated integration legs** drive the real, freshly-built `harness` binary as a
  subprocess through an in-process `harness-model-proxy` whose provider config
  points at a throwaway OpenAI-compatible mock server bound to `127.0.0.1` (no
  external network, no API keys). They are automated behind the `integration`
  build tag. The proxy and mock live only in `_test.go`, so they are never
  compiled into the shipped binaries. Real-tmux and real-`gopls` legs use those
  optional local programs and skip when they are unavailable.
- **Opt-in automated live-model legs** drive a freshly-built `harness` binary
  through a separately running `harness-model-proxy` and make real upstream
  calls. They live behind the `livemodel` build tag and are not part of the
  default test suite.
- **Manual legs** cover a real downstream MCP server and broader live-provider
  workflows. They require the corresponding local service or provider
  credentials and are not part of the default test suite.

## Prerequisites

- Go 1.26 or newer, as declared by `go.mod`.
- `tmux` on `PATH` to run the optional real-tmux delegate legs; otherwise those
  legs skip. Each test uses its own temporary server socket and unique session.
- `gopls` on `PATH` to run the optional real-LSP integration leg; otherwise that
  leg skips.
- A separately running, configured `harness-model-proxy` is needed for the
  opt-in live-model legs.
- Provider credentials, Ollama, and a real downstream MCP server are needed only
  for their respective live or manual legs.

## Automated integration legs

Run them all:

```sh
go test -tags=integration ./cmd/harness/ -run TestSmoke -v
# or under the race detector:
go test -race -tags=integration ./cmd/harness/ -run TestSmoke
```

`make test-integration` (`go test -tags=integration ./cmd/harness`) runs the
entire integration suite. The `-run TestSmoke` command above is a scoped subset
that selects only the harness smoke legs in this table; the LSP legs below are
part of the same `make test-integration` run.

| Leg | Test | What it asserts |
|---|---|---|
| Local OpenAI-compatible server, tool round-trip | `TestSmokeToolRoundTrip` | The mock streams a `read_file` tool call, the harness executes it, and a **second** request to the mock carries the `role:"tool"` result with the file's content. The assistant's final text lands on **stdout**; the session directory's `state.json` and `tree.ndjson` are written, and the loaded active path passes `ValidateTranscript`. |
| `^C` during a stream | `TestSmokeInterruptMidStream` | The mock streams `partial answer` then stalls briefly. After the partial text reaches stdout, the test sends `SIGINT` to the subprocess. The process exits **130**; the saved session keeps the partial assistant text and passes `ValidateTranscript` (the §4 cancel-repair: keep streamed text, strip un-executed tool calls). |
| Resume of an interrupted session | `TestSmokeResumeInterrupted` | A crafted save requested with a **dangling `tool_use`** synthesizes a `tool_result` (`is_error`, text `interrupted`) before immutable tree storage, then resumes with `-resume`. The mock's single request is verified to contain that `role:"tool"` / `tool_call_id` message, and the run completes against the mock's text turn. |
| Delegate tmux window | `TestSmokeDelegateTmuxWindow` | A scripted foreground delegate runs with `-delegate-tmux -delegate-tmux-layout window` against a real tmux server on a private temporary socket. While the child request is gated, the server contains one detached, correctly named delegate window with `remain-on-exit=on` and `automatic-rename=off`; after completion only the base window remains. The delegate flow completes (3 mock requests) with no degradation warning. |
| Delegate tmux pane | `TestSmokeDelegateTmuxPane` | A scripted foreground delegate runs **without** `-delegate-tmux` (the tmux auto default enables the view) against a real tmux server on a private temporary socket. While the child request is gated, the server contains a titled, right-hand split from the recorded parent pane with `remain-on-exit=on`; after completion only the base pane remains. The delegate flow completes (3 mock requests) with no degradation warning. |

### MCP proxy legs (automated)

These exercise the optional MCP proxy end to end without a network or any real
downstream server: a fake in-process proxy (or the real `harness-mcp-proxy`
serve loop driven against a fake downstream) stands in. They live in
`cmd/harness/mcp_test.go`, `cmd/harness-mcp-proxy/main_test.go`, and
`internal/mcpproxy/daemon_test.go`, and run under `go test ./...`.

```sh
go test ./cmd/harness/ -run TestSetupMCP -v
go test ./cmd/harness-mcp-proxy/ -run 'TestServe|TestTools' -v
go test ./internal/mcpproxy/ -run TestDaemonServesHTTP -v
```

| Leg | Test | What it asserts |
|---|---|---|
| Proxy `serve` + `tools` listing | `TestToolsListsAggregatedTools` | `runServe` binds an HTTP listener, supervises a fake downstream, and aggregates its tools; the `tools` subcommand connects and prints `2 tools` with the namespaced names `mcp__fake__echo` / `mcp__fake__ping`, descriptions collapsed to their first line. A `SIGINT` shuts the daemon down cleanly. |
| `tools` interrupted while connecting | `TestToolsCommandSIGINTCancelsHangingProxy` | The `tools` subcommand starts an HTTP request to a hanging proxy; injected `SIGINT` cancels it immediately and exits **130**, without waiting for the command timeout. |
| One-shot calling an `mcp__` tool | `TestSetupMCPRegistersToolsAndOneShotCalls` | With `HARNESS_MCP_ENABLE=true` and an HTTP proxy URL in env, `harness -p` discovers the proxy's tool, the model calls `mcp__test__echo`, the harness dispatches it over HTTP, and the **second** model request carries the `echo:` tool result. The assistant's text lands on **stdout**; stderr shows `mcp: connected`. |
| MCP startup interrupted | `TestRunSigintDuringMCPRegistration` | Harness starts MCP registration against a hanging proxy; injected `SIGINT` cancels registration and exits **130** without emitting the ordinary MCP-unavailable warning. |
| Non-HTTP proxy → warn and continue | `TestSetupMCPRejectsNonHTTPProxyAndContinues` | MCP is enabled but the proxy value is not an `http(s)` URL. Startup **proceeds** (exit 0), emits one `[warn] [mcp]` `cannot connect to proxy … MCP tools unavailable` line, registers **zero** `mcp__` tools, and returns a no-op cleanup — MCP never fails startup. |
| HTTP proxy down → warn and continue | `TestSetupMCPHTTPUnreachableWarnsAndContinues` | With `mcp.proxy` set to a closed `http://` URL, harness attempts registration, emits one warning, and continues without MCP tools. |
| Daemon serves HTTP | `TestDaemonServesHTTP` | With `proxy.listen` set, one daemon binds the TCP listener; an MCP client lists the aggregated tools and calls one. The HTTP side uses an `Mcp-Session-Id` session and JSON-only responses. |
| `tools -proxy <url>` against the HTTP listener | `TestServeListenFlagAndToolsProxy` | `runServe -listen <addr>` brings up the HTTP listener; the `tools` subcommand with `-proxy http://<addr>` connects over HTTP and prints the same aggregated table. |

### LSP shim integration legs (automated)

These exercise the `harness lsp serve` shim (the generic LSP-to-MCP bridge) end
to end. They live in `cmd/harness/lsp_integration_test.go` and
`cmd/harness/lsp_chain_integration_test.go` behind the `integration` build tag,
so they run under `make test-integration` (which also covers the harness legs
above):

```sh
make test-integration
# or just the LSP shim legs:
go test -tags=integration ./cmd/harness/ -run TestIntegration -v
```

| Leg | Test | What it asserts |
|---|---|---|
| Real `gopls` over the shim | `TestIntegrationGopls` | Drives `harness lsp serve` against a real `gopls` over a tiny temp Go module — server selection, root detection, launch + handshake, `didOpen` — then definition (`Foo` resolves to `main.go:3`), signature-help (`Foo() int`), and diagnostics (`undefinedThing`) calls. **Skipped** when `gopls` is not on `PATH`. |
| Production proxy chain | `TestIntegrationProxyChain` | Builds `harness` and `harness-mcp-proxy` and runs the real chain: a local `harness-mcp-proxy serve -stdio` hosts `harness lsp serve` as a downstream; the test confirms the shim's tools surface under the `mcp__lsp__` namespace (e.g. `mcp__lsp__definition`) — which is what lets harness register them. No language server is launched (`tools/list` is static), and this production stdio chain opens no metrics listener or collectors. |

## Live-model legs (opt-in automated)

The `livemodel` suite builds the real `harness` CLI once, checks a separately
running model proxy, reads its catalog, and then exercises Gemini Interactions,
OpenAI Chat Completions through OpenRouter, Alibaba Token Plan, DeepSeek, and
Xiaomi, OpenAI Responses through Sakana, the ChatGPT subscription, and the
first-party API route, and Anthropic Messages sequentially. It verifies clean
text turns, dialect-specific reasoning usage and summaries, an Anthropic tool
round-trip, rendered session summaries, and valid persisted transcripts.

Set the proxy URL and run the suite:

```sh
export HARNESS_LIVE_PROXY_URL=http://localhost:8765
make test-live-models
```

If the model proxy requires its own API key, supply it separately:

```sh
export HARNESS_LIVE_PROXY_API_KEY=...
make test-live-models
```

An unset `HARNESS_LIVE_PROXY_URL` skips the top-level suite before another
binary is built or any network connection is attempted. Once the URL is set,
an unreachable proxy, malformed catalog, timeout, build error, or model-call
error fails the suite. If the proxy catalog has no candidate for one dialect,
only that dialect's subtest skips.

The default `go test ./...` and `make test` commands never compile or run these
tests. Live calls may incur provider charges; the candidate order favors free
or subscription-backed targets and the tests run sequentially to limit cost
and rate-limit pressure.

### Real downstream MCP server (manual)

To smoke a real downstream MCP server, write a proxy config at
`~/.config/harness-mcp-proxy/config.json` (one `mcpServers` entry, stdio or
http; see the README), then:

```sh
make

# Start the proxy yourself — harness never spawns it. Leave it running:
./harness-mcp-proxy serve &
./harness-mcp-proxy tools          # prints the mcp__<server>__<tool> table
curl -fsS http://127.0.0.1:9091/metrics

# Drive a model through an MCP tool:
HARNESS_MCP_ENABLE=true ./harness -model anthropic:claude-opus-4-8 \
  -p "use an MCP tool to <task>"
```

Expect: `mcp: connected (N servers, M tools): ...` on stderr, the daemon outliving
harness (a second harness reuses it), and downstream stderr/crashes recorded in the
proxy log. If the proxy is **not** running, harness emits one
`mcp: cannot connect to proxy at http://127.0.0.1:8766: …; MCP tools unavailable`
warning and continues toolless.

To smoke a non-default proxy address, add
`"proxy": {"listen": "127.0.0.1:8420"}` to the proxy config (or pass
`serve -listen 127.0.0.1:8420`), then:

```sh
./harness-mcp-proxy serve -listen 127.0.0.1:8420 &
./harness-mcp-proxy tools -proxy http://127.0.0.1:8420

# Point harness at the URL (config mcp.proxy = "http://127.0.0.1:8420", or env):
HARNESS_MCP_ENABLE=true HARNESS_MCP_PROXY=http://127.0.0.1:8420 \
  ./harness -model anthropic:claude-opus-4-8 -p "use an MCP tool to <task>"
```

Expect: the same `mcp: connected` line for the one-shot command; one-shot uses
the tool list discovered before the model request. In an interactive REPL,
remote HTTP MCP discovery runs in the background and can print
`[mcp: tool list updated; N tools]` when a successful registration is applied at
the next prompt boundary.

### How the mock works

`startModelProxy` creates a temp proxy config with one `openai` provider whose
base URL points at `recordingMock`. The subprocess is invoked with
`-model-proxy-url`, so the tested path is `harness -> harness-model-proxy ->
provider dialect -> mock endpoint`.

`recordingMock.ServeHTTP` decodes each `/v1/chat/completions` request body,
records it, and replies with a scripted SSE stream (OpenAI chunk shape:
`choices[].delta` for text, `choices[].delta.tool_calls[]` fragments for a tool
call, `finish_reason`, a trailing usage chunk, then `data: [DONE]`).

## Real-API legs (manual; requires credentials)

These exercise the live provider dialects end to end through
`harness-model-proxy`. Start the proxy in a separate shell first; `harness`
should receive only the proxy URL, provider id, and model id.

The reproducible paired live-model protocol for deterministic tool-flow changes
is recorded separately in [flowbench.md](flowbench.md), including its high
run limits, acceptance gates, provider-cost treatment, and latest results.

```sh
go build -o harness ./cmd/harness
go build -o harness-model-proxy ./cmd/harness-model-proxy
./harness-model-proxy setup
./harness-model-proxy
```

### Live delegation steering matrix (repeat on Anthropic and OpenAI)

Run this matrix in a clean disposable checkout after starting the model proxy.
Use at least one Anthropic Messages model and one first-party OpenAI Responses
model, and repeat every row at least three times per model; delegation propensity
is probabilistic and a single successful run is not sufficient evidence.

For each run, choose an explicit session directory and retain stderr:

```sh
MODEL=anthropic:claude-opus-4-8 # repeat with an OpenAI target such as openai:gpt-5.5
CASE=architecture              # narrow | architecture | multi-angle | coupled
SESSION=/tmp/harness-steering-${CASE}-$(date +%s)
./harness -model "$MODEL" -session "$SESSION" 2>"$SESSION.stderr"
```

Submit one matrix prompt in the REPL, wait for all background completion notices
and the final synthesis, then `/exit`:

| Case | Prompt shape | Expected steering |
|---|---|---|
| Narrow known lookup | “In `internal/agentdef/agentdef.go`, find `Definition` and tell me what `Reasoning` means. Do not edit.” | Parent uses direct search/read tools; no `delegate` event and no child record. |
| Broad architecture | “Trace delegation end to end from model-facing schema through child launch, recursive rebinding, persistence, and usage accounting. Cite paths/symbols; do not edit.” | Parent delegates to `explore`, then synthesizes and verifies its evidence. |
| Independent multi-angle research | “Assess delegation from three independent angles: capability safety, context/cost behavior, and operator observability. Investigate in parallel where safe, then synthesize; do not edit.” | Multiple independent background children are reasonable; parent continues useful work, does not poll or duplicate them, and waits for automatic completion context before synthesis. |
| Tightly coupled edit/test | “Make one small change to the delegate schema wording and update its focused test, then run that test.” | Parent owns the coupled edit/test loop rather than delegating it merely because `delegate` exists. Revert this smoke-only edit manually afterward. |

For every repetition, inspect actual evidence rather than only the final prose:

```sh
rg -n 'delegate|background job' "$SESSION.stderr"
find "$SESSION/children" -maxdepth 2 -type f -print 2>/dev/null
for f in "$SESSION"/children/*/meta.json; do test -e "$f" && cat "$f"; done
```

Record the selected agent, foreground/background mode, child count, task prompt,
report quality, whether the parent synthesized and independently verified the
report, and whether the final parent usage/session summary includes child usage
(the parent total must exceed the sum of parent-only model checkpoints by the child
usage recorded in child metadata). For child runs, inspect
`children/<id>/state.json` and `raw.ndjson` to
confirm the fresh child transcript and tool events. The narrow and coupled rows
must not be scored as failures merely for making zero child calls—that is their
intended steering behavior. Compare repeated Anthropic and OpenAI results before
changing heuristics.

### Anthropic Messages API

```sh
export ANTHROPIC_API_KEY=sk-ant-...
# Start or restart harness-model-proxy in this environment after setup.

# One-shot, assistant text captured to a file (tool summaries/usage go to stderr):
./harness -model anthropic:claude-opus-4-8 \
  -p "list the Go files in this directory using your tools" > answer.txt

# Interactive REPL (try /help, a prompt that needs a tool, then /usage, /exit):
./harness -model anthropic:claude-opus-4-8
```

Expect: `[turn: N … │ prompt …]` progress/completion lines and one aggregate
`[prompt: N turns …]` usage line on stderr with dollar costs (from configured
pricing or models.dev), tool one-liners on stderr, the
final answer on stdout, and a session auto-saved under
`~/.local/state/harness/sessions/`.

### OpenAI Responses API

```sh
export OPENAI_API_KEY=sk-...
# Start or restart harness-model-proxy in this environment after setup.

./harness -model openai:gpt-5.5 \
  -p "read README.md and summarize it in two sentences" > answer.txt
./harness -model openai:gpt-5.5            # interactive
```

Expect: same behavior as above. First-party OpenAI models use the Responses
dialect when models.dev identifies them. Cost appears when the model has
configured pricing or pricing can be found through models.dev; unknown model
names show token counts without a dollar figure.

### Stateful continuation and proxy-rollout matrix

Run `make test-live-models`, then exercise every catalog target that reports
`continuation_stateful:true` (at minimum one durable HTTP Responses target,
OpenAI Codex Responses WebSocket, and Gemini Interactions):

1. Start an explicit session, complete two turns including a tool call, and
   confirm `state.json` contains a response ID, anchor count, and lowercase
   fingerprint.
2. Interrupt a later streamed turn, resume the same session, and confirm the
   recovered transcript either continues from a matching anchor or safely sends
   full history.
3. Replace the serving proxy pod between completed turns. Durable HTTP/
   Interactions state should work through either replica. Codex `store:false`
   may print exactly one `previous response unavailable` reset notice and resend
   complete valid history once.
4. With sticky routing enabled, repeat Codex turns and confirm they stay on one
   proxy instance and avoid the 409/reset path. Then deliberately break
   stickiness or close the socket and confirm recovery remains correct.
5. During a long stream, terminate its pod. Verify `/readyz` becomes 503 while
   `/healthz` remains 200, the stream reaches its terminal event before the
   drain timeout, new work reaches another pod, and the metrics endpoint remains
   scrapeable until teardown finishes.

Across replicas, verify pinned catalogs are identical, correlate logs by
`(proxy_instance_id, proxy_request_id)`, sum request/usage/continuation counters
in Prometheus, and confirm `/v1/usage` visibly reports each pod's `instance` and
`since`.

### Google Gemini Interactions

```sh
export GEMINI_API_KEY=...
# Select Google and at least one Gemini model in setup, then restart the proxy.
harness-model-proxy setup
harness-model-proxy

./harness -model google:gemini-3.6-flash \
  -p "read README.md and summarize it in two sentences"
./harness -model google:gemini-3.6-flash -web-search auto \
  -p "What is the latest stable Go release? Verify it with Google Search."
```

Expect: Google uses `api_type:"interactions"`, function calls dispatch normally,
and the second command uses server-side Google Search before returning model
text. Repeat a tool-using prompt after restarting the model proxy to exercise
signed full-history replay; it must not fail with a missing thought/search
signature.

### Local Ollama (OpenAI-compatible, no key)

```sh
ollama serve &                 # if not already running
ollama pull llama3.2

mkdir -p ~/.config/harness-model-proxy
cat > ~/.config/harness-model-proxy/config.json <<'JSON'
{
  "provider_configs": ["ollama.json"]
}
JSON
cat > ~/.config/harness-model-proxy/ollama.json <<'JSON'
{
  "name": "ollama",
  "api_type": "openai",
  "base_url": "http://localhost:11434/v1",
  "models": [{"name": "llama3.2", "context_window": 131072}]
}
JSON

./harness-model-proxy
./harness -model ollama:llama3.2 -p "what files are in this directory?"
```

Expect: the proxy uses the OpenAI-compatible dialect with an empty API key,
token counts with no dollar figure, and tool reliability depending on the local
model's tool-calling support.

### Interrupt and resume against a real provider

To reproduce the interrupt/resume legs against a live API rather than the mock:

```sh
# Start a turn that will take a while, then press Ctrl-C once mid-stream:
./harness -model anthropic:claude-opus-4-8 -session /tmp/harness-smoke-session
> write a very long essay about distributed systems
# ^C  -> [cancelled], partial text kept; ^C again (or at the idle prompt) -> exit 130

# Resume the saved session and continue:
./harness -model anthropic:claude-opus-4-8 -resume /tmp/harness-smoke-session -p "continue"
```

Expect: the resumed transcript is re-sent intact; if the prior run was saved
mid-tool-call, the dangling `tool_use` is repaired with an `interrupted`
`tool_result` before the next request (design §4, §11).
