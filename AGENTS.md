# AGENTS.md - harness

Harness is a small, terminal-first Go CLI for a provider-neutral, tool-using LLM loop. Keep it simple and standard-library-only.

## Hard Rules

- Add no third-party Go modules without approval.
- Bug fixes require regression tests.
- Use conventional commit messages; never open draft PRs.
- Do not revert or overwrite user changes unless asked.
- Add no sandboxing or permission prompts unless requested.

## Verify

- Quick build: `go build ./cmd/harness`, or `make` to build all three binaries.
- Tests: `make test` (`go test ./...`).
- Before submitting: `go build ./... && go vet ./... && go test ./...`.

## Architecture Map

- `cmd/harness` wires flags/config, providers, signals, agents, and REPL/one-shot execution. UI, terminal, and logging code lives under `internal/ui`, `internal/term`, and `internal/logging`; ANSI belongs only there.
- `internal/llm` owns provider-neutral contracts, validation, reasoning, pricing, and model metadata. Dialects live in `openai`, `anthropic`, and `responses`; `factory` selects them. Core `llm` must not import dialects or factory.
- `internal/agent` owns the turn loop, tool orchestration, interrupts, retention, and compaction. Keep the system prompt on `llm.Request.System`, never in history.
- `internal/tools` owns built-ins, schemas, registry/subsets, dispatch recovery, and truncation. `internal/delegate`, `background`, `todo`, and `plan` hold agent-starting or persisted tool state outside `tools` to avoid import cycles.
- `internal/config`, `agentdef`, and `modelcatalog` handle precedence, agent definitions, and normalized models.dev/OpenAI Codex catalog metadata. `internal/hooks`, `session`, `sysprompt`, and `skills` handle lifecycle hooks, durable transcripts/artifacts, prompt composition, and skill disclosure. Session writes should use temp-file then rename.
- `cmd/harness-model-proxy` is a thin CLI over `internal/modelproxy`; the proxy owns provider config, credentials, catalogs, concrete provider calls, budgets, and API keys.
- `cmd/harness-mcp-proxy`, `internal/mcpproxy`, and `internal/mcp`/`jsonrpc` implement MCP aggregation and transports. `internal/mcptools` adapts namespaced MCP tools; `internal/mcpchild` supervises local stdio services. Core `internal/mcp` and `jsonrpc` must not import `llm` or `tools`.
- `internal/lspproxy` is the hand-written LSP manager/shim; `internal/lsptools` exposes trusted short `lsp_*` tools. `harness lsp serve` provides the stdio MCP compatibility path.
- Support packages include `auth`, `apikey`, `inputimage`, `markdown`, `replprompt`, `retry`, `sse`, `httpserve`, and `httpx`.

## Code Patterns

- Keep packages cohesive and functions small. Libraries return errors; only UI/logging prints.
- Use `errors.Is`/`errors.As` and `fmt.Errorf("%w")`; avoid error-string matching.
- Preserve provider neutrality: agent code depends on `internal/llm`, not dialects.
- Hand-write tool schemas, decode into private typed structs, and tolerate unknown JSON keys.
- Prefer argv-style tools when quoting is risky; use a shell only for shell features.

## Tests

- Unit tests live beside code. `//go:build integration` tests live in `cmd/harness/*_test.go` and run with `make test-integration`.
- Avoid network except `httptest.Server`; use fakes, fixtures, and temp directories.
- Coordinate goroutines with channels or `sync.WaitGroup`, not sleeps.
- Preserve `ValidateTranscript` invariants after transcript mutation. Behavioral tool changes need focused `internal/tools` tests.

## Documentation

- `README.md` is the onboarding and value-proposition surface: project summary,
  design invariants, basic architecture, installation, first model-proxy setup,
  first prompt, compact feature highlights, and links. Preserve the Design
  Invariants and Basic Architecture sections as intentional README content.
- Keep detailed flags, configuration, provider behavior, REPL commands, agents,
  sessions, diagnostics, compaction, interrupts, and hooks in `docs/usage.md`.
  Keep its flag and command references synchronized with
  `cmd/harness/main.go`.
- Keep operational tool behavior in `docs/tools.md`; keep model-facing schemas,
  implementation contracts, architecture, system prompts, and compaction
  internals in `docs/design.md` and the relevant tests.
- Keep MCP, LSP, smoke, release, and benchmark details in their dedicated
  documents under `docs/`.
- Update the canonical document for every user-visible feature change. Change
  the README only when the value proposition or new-user journey materially
  changes; do not append changelog-style implementation details or duplicate
  reference prose there.
- Preserve the `release-artifacts` marker block in `README.md`; release
  automation owns its versioned links.

## Adding Things

- Tool: implement `Tool`, register, test, document operational behavior in
  `docs/tools.md`, and document its model-facing contract in design section 9.
- Dialect: add `internal/llm/<dialect>`, implement `llm.Provider`, register it in
  factory, and update the provider design and any user-facing setup in
  `docs/usage.md`.
- Config/flag: follow `flags > env > config > defaults`; update
  `cmd/harness/main.go`, `docs/usage.md`, and examples when useful. Update the
  README only when the quickstart changes.
