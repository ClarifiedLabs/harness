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

## Serena

- Commit `.serena/project.yml` and `.serena/.gitignore`, never `.serena/cache/` or `.serena/project.local.yml`.
- Commit concise `.serena/memories/` entries only when curated, non-sensitive, and useful; remove stale entries.

## Keep Docs In Sync

- Public flags/usage: `README.md` and `cmd/harness/main.go` usage.
- Tool behavior/schemas: `docs/design.md` section 9.
- System prompts and compaction: `internal/sysprompt` tests/docs.
- Agents: `README.md` and design section 14; MCP: README and design sections 9.15/15; smoke workflows: `docs/smoke.md`.

## Adding Things

- Tool: implement `Tool`, register, test, and document its model-facing contract.
- Dialect: add `internal/llm/<dialect>`, implement `llm.Provider`, and register it in factory.
- Config/flag: follow `flags > env > config > defaults`; update examples when useful.
