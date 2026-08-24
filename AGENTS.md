# AGENTS.md - harness

Harness is a small, terminal-first Go CLI for a provider-neutral, tool-using LLM loop. Keep it simple and standard-library-only.

## Hard Rules

- Add no third-party Go modules without approval.
- Bug fixes require regression tests.
- Use conventional commit messages; never open draft PRs.
- Do not revert or overwrite user changes unless asked.
- Add no sandboxing or permission prompts unless requested.

## Verify

- Build all three binaries: `make`. Tests: `make test` (`go test ./...`).
- Before submitting: `go build ./... && go vet ./... && go test ./...`.

## Architecture Invariants

The package-by-package map lives in `docs/design.md` §3; update it when ownership moves. These invariants hold regardless of layout:

- Core `internal/llm` must not import dialects (`openai`, `anthropic`, `responses`) or `factory`; agent code depends on `internal/llm`, not dialects.
- Core `internal/mcp` and `internal/mcp/jsonrpc` must not import `llm` or `tools`.
- The system prompt travels on `llm.Request.System`, never in history.
- `internal/sessionrec` is the one canonical `raw.ndjson` recorder, shared by the parent UI sink and delegate child sessions.
- Session writes use temp-file then rename.
- ANSI escapes belong to terminal display paths only (`internal/ui`, `internal/term`, Markdown/replay rendering), gated on TTY/color config — never in transcripts, logs, tool results, or model-facing text.

## Code Patterns

- Libraries return errors; only UI/logging prints.
- Use `errors.Is`/`errors.As` and `fmt.Errorf("%w")`; avoid error-string matching.
- Hand-write tool schemas, decode into private typed structs, and tolerate unknown JSON keys.
- Prefer argv-style tools when quoting is risky; use a shell only for shell features.

## Tests

- `//go:build integration` tests live in `cmd/harness/*_test.go` and run with `make test-integration`.
- Avoid network except `httptest.Server`; use fakes, fixtures, and temp directories.
- Coordinate goroutines with channels or `sync.WaitGroup`, not sleeps.
- Preserve `ValidateTranscript` invariants after transcript mutation.

## Documentation

- `README.md` is the onboarding and value-proposition surface. Preserve its Design Invariants and Basic Architecture sections and the `release-artifacts` marker block (release automation owns its versioned links). Change the README only when the value proposition or new-user journey materially changes.
- Reference detail lives under `docs/`: usage/flags/config/REPL in `usage.md`, model-proxy operations in `proxy.md`, operational tool behavior in `tools.md`, model-facing contracts and architecture in `design.md`, session/compaction/trajectory internals in `session.md`/`compaction.md`/`trajectory.md`, and MCP/LSP/smoke/release/benchmarks in their own docs. Keep flag and command references synchronized with `cmd/harness/main.go`.
- Update the canonical document for every user-visible feature change; do not duplicate reference prose in the README.

## Adding Things

- Tool: implement `Tool`, register, test, document operational behavior in `docs/tools.md`, and the model-facing contract in design §9.
- Dialect: add `internal/llm/<dialect>`, implement `llm.Provider`, register it in factory, and update the provider design and user-facing setup in `docs/usage.md`.
- Config/flag: follow `flags > env > config > defaults`; update `cmd/harness/main.go` and `docs/usage.md`, plus examples when useful. README only when the quickstart changes.
