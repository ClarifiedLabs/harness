# AGENTS.md - harness

Harness is a small Go CLI for running a tool-using LLM loop over local files,
shell commands, web fetches, and git. Keep it simple, stdlib-only, terminal
first, and provider-neutral.

## Hard Rules

- No third-party Go module dependencies unless explicitly approved.
- Bug fixes need regression tests.
- Use conventional commit messages. Do not open draft PRs.
- Do not revert or overwrite user changes unless explicitly asked.
- Do not add sandboxing or permission prompts unless explicitly requested.

## Verify

- Quick build: `go build ./cmd/harness`, or `make` (the default `build` target builds all three binaries: `harness`, `harness-model-proxy`, `harness-mcp-proxy`).
- Tests: `make test` (`go test ./...`).
- Before submitting: `go build ./... && go vet ./... && go test ./...`.

## Package Map

- `cmd/harness/main.go`: flags, config/setup, provider wiring, signals, REPL vs one-shot.
- `internal/llm`: provider-neutral types, validation, model/reasoning/pricing metadata. Must not import dialects or factory.
- `internal/llm/openai`, `internal/llm/anthropic`, `internal/llm/responses`: HTTP dialects (OpenAI Chat Completions, Anthropic Messages, OpenAI Responses), request builders, stream decoders, tool-call assembly.
- `internal/llm/factory`: selects dialects (`openai`/`anthropic`/`responses`); keep separate to avoid import cycles.
- `internal/agent`: turn loop, tool orchestration, interrupts, compaction.
- `internal/tools`: `Tool` interface, registry/subsets, dispatch recovery/truncation, built-ins. Inputs are `json.RawMessage`.
- `internal/delegate`: configured child-agent execution (the `[delegatable]` tools); lives outside `internal/tools` to avoid a `tools` -> `agent` import cycle.
- `internal/background`: process-local background job runner backing the background-command tool.
- `internal/todo`: the `update_todos` model-callable task list (whole-list replace); lives outside `internal/tools` so `internal/session` can persist items without importing tools.
- `internal/hooks`: loads and runs command hooks around lifecycle events (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PreCompact`, `PostCompact`, `Stop`); provider- and agent-neutral.
- `internal/config`, `internal/agentdef`, `internal/modelsdev`: config precedence, agent definitions, models.dev setup/catalog metadata.
- `internal/session`: transcripts, replay logs, compaction archives, tool artifacts. New persistence should write temp-file then rename.
- `internal/ui`, `internal/term`, `internal/logging`: REPL/one-shot rendering, terminal behavior, plaintext slog. ANSI belongs here only.
- `internal/sysprompt`, `internal/skills`: built-in prompt/env context and skill discovery/disclosure.
- `internal/sse`, `internal/retry`: shared SSE reader and provider HTTP retry/backoff.
- Smaller support packages: `internal/auth` (OAuth/token-command credential acquisition: `oauth2`/`codex_oauth`/`token_command`), `internal/replprompt` (interactive REPL prompt parse/render), `internal/inputimage` (validate local image attachments into provider-neutral content blocks), `internal/markdown` (stdlib terminal-friendly Markdown subset), `internal/httpserve` (shared HTTP server lifecycle for the auxiliary binaries), `internal/httpx` (small HTTP helpers, e.g. Content-Type media-type parsing).
- `cmd/harness-model-proxy`: provider/model proxy binary (subcommands `serve` (default), `setup`, `refresh-models`, `auth` `<login|logout|status>`, `generate-api-key`, `version`); thin CLI over `internal/modelproxy`. Owns provider config, API keys, model-catalog metadata, and concrete provider calls so `harness` only needs a proxy URL.
- `internal/modelproxy` (`client`/`protocol`/`server`): the model-proxy implementation — provider config loading, the HTTP handler that dispatches to dialects, OAuth credential storage, and the client harness uses to reach it.
- `cmd/harness-mcp-proxy`: optional MCP proxy binary (`serve`/`tools`/`auth`/`version`); thin CLI over `internal/mcpproxy`. `serve -stdio` serves the aggregated tools over stdin/stdout (for a harness-spawned local proxy) instead of HTTP.
- `harness lsp serve`: generic LSP-to-MCP shim hosted by the main binary, serving MCP over stdio for proxy-hosting scenarios.
- `internal/mcp` (+`jsonrpc`): tools-only MCP slice — schema, client/server, stdio + streamable-HTTP transports, JSON-RPC framing (newline; `jsonrpc.NewPeerWithCodec` accepts an alternate codec). No `internal/llm`/`internal/tools` imports.
- `internal/mcpproxy`: proxy daemon — Claude Code-compatible config, downstream supervisors, namespaced tool registry; `Daemon.RunStdio` serves over stdio.
- `internal/mcptools`: harness-side adapter exposing MCP tools as `tools.Tool` over a reconnecting `Conn`. HTTP proxy is off unless `mcp.enable`; the local stdio service (`mcp.local`) is off unless `mcp.local.enable=true` and `mcp.local.command` is configured.
- `internal/mcpchild`: spawns/reaps a local stdio MCP service as a child process (harness-side, for `mcp.local`).
- `internal/lspproxy`: the LSP manager/shim — inline top-level `lsp.servers` or compatibility `{version,servers}` config plus embedded defaults, Content-Length JSON-RPC codec, hand-rolled LSP client, per-(server,root) supervisor, and the Manager (MCP `ToolProvider`) with the 7 read-only tools. Built-in harness registration exposes short `lsp_*` names; `harness lsp serve` self-namespaces `mcp__lsp__*` via `-namespace`.
- `internal/lsptools`: adapts the built-in LSP provider to harness `tools.Tool` with short `lsp_*` names and trusted read-only behavior.

## Code Patterns

- Keep packages cohesive and functions small. Return errors from library code; only UI/logging should print.
- Use `errors.Is`/`errors.As` and `fmt.Errorf("%w")`; avoid string matching.
- Keep the system prompt on `llm.Request.System`, never in message history.
- Preserve provider neutrality: agent code depends on `internal/llm` contracts, not dialect packages.
- Hand-write tool JSON schemas; decode inputs into typed private structs; tolerate unknown JSON keys.
- Prefer argv-style tools (`exec`, `git`, `grep`, `rg`) when shell quoting is risky; use shell commands only for shell features.

## Tests

- Unit tests live next to code; `//go:build integration` integration tests live in `cmd/harness/*_test.go` (run via `make test-integration`).
- Avoid network in tests except `httptest.Server`; use fake providers, fixtures, and temp dirs.
- Avoid sleeps for goroutine coordination; use channels or `sync.WaitGroup`.
- Preserve `ValidateTranscript` invariants after transcript mutations.
- Behavioral tool changes need focused tests under `internal/tools`.

## Keep Docs In Sync

- Public flags/usage: `README.md` and `cmd/harness/main.go` usage text.
- Tool behavior/schemas: `docs/design.md` section 9.
- System prompt behavior: `internal/sysprompt` tests/docs; consider compaction impact.
- Agent definitions: `README.md` and `docs/design.md` section 14.
- MCP proxy: `README.md` ("MCP servers"), `docs/design.md` sections 9.15 and 15.
- Smoke workflow changes: `docs/smoke.md`.

## Adding Things

- Tool: add one file in `internal/tools`, implement `Tool`, register it, test it, document its model-facing contract.
- Provider dialect: add `internal/llm/<dialect>`, implement `llm.Provider`, register in `internal/llm/factory`, keep dialect details out of `internal/llm`.
- Config field or flag: follow `flags > env > config > defaults`; update examples when useful.
