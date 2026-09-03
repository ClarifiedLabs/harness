# Usage Reference

This page covers everyday behavior that is too detailed for the README:
provider/model selection, interactive initial prompts, one-shot mode, flags, configuration, REPL commands,
agents, sessions, compaction, interrupts, and hooks.

For tool behavior, see [tools.md](tools.md). For MCP and LSP, see
[mcp.md](mcp.md) and [lsp.md](lsp.md).

## Model Selection

`harness` fetches configured model targets from `harness-model-proxy`. A model
value like `openrouter:openai/gpt-5.5` selects that exact proxy target. Model
selection belongs to `harness`: use `-model`, `HARNESS_MODEL`, config `model`,
or `/model` in the REPL. Model values use the proxy's `<provider>:<model>`
target IDs.

Use `harness -model-proxy-url http://host:port` when the proxy runs somewhere
other than `127.0.0.1:8765`. When `-model-proxy-url` /
`HARNESS_MODEL_PROXY_URL` is left unset, harness applies the effective default
`http://127.0.0.1:8765`.

Use `harness --check-model-proxy` to verify that the configured proxy is
reachable. It sends `GET /v1/models`, prints a short success line on stdout, and
exits before creating a session or starting the REPL.

Use `harness --models` to print the targets and whether they support portable
reasoning profiles. Use `harness --agents` to print the resolved built-in and
config-defined agents. Both commands exit before creating a session.

`--models --format json` also shows each target's `api_type`,
`continuation_stateful`, provider-native `native_compaction`, zero-generation
`prewarm` support, `server_tools`, price, and variant relationship
(`base_target_id` / `variant`). When a target advertises `web_search`,
`-web-search auto` lets harness declare the provider-hosted web search tool for
model calls. The default is `off`.

## Interactive Initial Prompt

Use `-i` or `-initial-prompt` to run one prompt immediately and then continue in
the normal REPL:

```sh
harness -model openai:gpt-5.5 -i "review the current git diff"
```

The initial prompt is literal text: leading `/` is not a REPL command and leading
`!` is not a shell escape. Unlike `-p`, `-i` does not read from stdin or append
piped stdin; any stdin lines remain available as scripted REPL input after the
initial prompt. `-image` attachments used with `-i` attach only to the initial
prompt.

## One-Shot Mode

In one-shot mode (`-p`) the assistant's text goes to stdout while model progress,
model cost checkpoints, tool-call progress, tool summaries, the usage line,
notices, explicitly enabled reasoning summaries, and errors go to stderr. Tool
preamble/commentary messages are assistant text, so they stream with the answer
rather than as bracketed status lines.

When stdout is a terminal, basic Markdown is rendered for readability. Pipe
tables remain padded grids when they fit, wrap cells within the terminal width
when a minimum grid fits, and become labeled stacked records only on very narrow
terminals. With color enabled, recognized language tags on fenced code blocks
also enable syntax highlighting; untagged, unknown-language, and `text` fences
remain plain. Choose
`--color-theme dark` (the default) or `--color-theme light` to match the terminal
profile. The `-no-color` flag or `NO_COLOR` disables highlighting and all other
ANSI styling while structural Markdown rendering remains readable. Redirected or piped
one-shot stdout stays raw model text. Bracketed status lines are timestamped by
default; disable them when you want untimestamped diagnostics:

```sh
harness -model openai:gpt-5.5 -timestamps=none -p "explain this repo in one paragraph" > answer.txt
```

`-p -`, or piping into stdin, reads the prompt from stdin. With both a flag value
and piped stdin they are concatenated, so this works:

```sh
harness -p "summarize:" < notes.txt
```

At the end of a one-shot run harness prints a `[session summary: …]` cost line to
stderr (cumulative input/cached/output/reasoning tokens, the always-present
successful-compaction count, and total cost, in that order). This summary
bypasses `-q`/`--quiet`, so a quiet one-shot run still reports what it spent.

Exit codes: `0` completed, `1` runtime error, `2` usage error, `130`
interrupted. When an admitted prompt fails with an error carrying a concrete
nonzero subprocess status, harness preserves that status instead of collapsing
it to `1`.

### JSON run stream (`-format json`)

`harness -p "<task>" -format json` runs one-shot exactly as above, except
**stdout carries only NDJSON** — one JSON object per line, no human text.
There are three outcomes:

1. Before a valid JSON run mode is selected, invalid CLI syntax and invalid
   JSON-mode combinations (such as TTY stdin without `-p`, or `-i`) are ordinary
   usage errors: stdout is empty and guidance is printed on stderr.
2. After valid one-shot or piped-interactive JSON mode selection, a startup
   failure before `run_start` emits exactly one stdout line:
   `{"type":"startup_error","v":4,"mode":"oneshot"|"interactive","exit_code":…,"error":…,"time":…}`.
   Consumers must accept it as the complete stream. Stderr is silent.
3. Once startup succeeds, stdout begins with `run_start` and continues through
   best-effort `run_end`; physical stderr stays silent. `-q`, `-v`, and
   `-tool-stream` cannot re-enable human output.

- For a successfully started run, line 1 is a `run_start` envelope
  (`{"type":"run_start","v":4,"mode":"oneshot","session_id":…,"agent":…,
  "provider":…,"model":…,"images":N}`); the last line is a best-effort
  `run_end` envelope (`exit_code` mirroring the process exit code, plus
  `termination_reason`/`error` on failures).
- Each submitted one-shot prompt is bracketed by `prompt_start`/`prompt_end`
  envelopes, including a prompt rejected by skill or hook preflight.
  `prompt_end` carries `exit_code`, `termination_reason`, a
  `usage` summary
  (`input_tokens`/`output_tokens`/`cost_usd`/`turns`/`compactions`), and
  `final_text` containing only assistant text emitted for that prompt (empty
  when it emitted none; never copied from an earlier session turn).
- Between the envelopes: the same durable `session.Event` objects the session
  records to `raw.ndjson` (`user`, `assistant_delta` post-coalescing,
  `assistant_phase`, `reasoning_summary`, `tool_start`, `tool_result`,
  `tool_diff`, `notice`, `turn_attempt_start`, `turn_attempt_usage`,
  `turn_complete`, `prompt_usage`, `model_request`, `checkpoint`, …). Streamed
  notices are therefore both visible to the client and replayable from the
  session log.

The stream protocol is versioned (`run_start.v` and `startup_error.v`,
currently `5`). Events use ordered bounded backpressure rather than silent
dropping. Consumers must ignore unknown event types and must handle EOF without
`run_end` (process crash, forced exit, or stdout write failure); a broken stdout
may prevent any usable stream output and never causes a plaintext stderr
fallback. A stdout write failure makes an otherwise successful process exit as a
runtime error.

#### Interactive JSON session (piped stdin, no `-p`)

`harness -format json` with no `-p` and **piped stdin** runs a full
interactive session driven by NDJSON messages on stdin, with the same event
stream on stdout (`run_start` reports `"mode":"interactive"`). This is the
embedding surface for apps driving harness as a subprocess: stdin is the
control channel, stdout the machine event channel, and stderr is silent after
valid JSON-mode selection.
The simplest client is one line:

```sh
printf '%s\n' '{"type":"prompt","text":"explain this repo"}' | harness -model openai:gpt-5.5 -format json
```

With TTY stdin, `-format json` without `-p` is a pre-selection usage error
(exit 2) — the TTY REPL has no JSON mode; `-i` is likewise rejected. These
errors emit no JSON and print guidance on stderr. A resolvable model is required
(`-model` or a configured default): the startup model/reasoning pickers do not
run without a TTY.

Input messages — one JSON object per line, max 16 MiB per line, unknown keys
ignored:

| `type` | Fields | Semantics |
|---|---|---|
| `prompt` | `text` (required, or `images`), `id?`, `agent?`, `model?`, `images?` `[{path, detail?}]` | Run a prompt. Sent while a prompt runs, a bare prompt **steers** into the running prompt (injected before the next model request, like Enter-during-prompt in the TTY REPL). Steering is attempted only when no earlier input is still queued, so queued and recovered input always runs in submission order. If immediate steering admission is busy, the input queues for the next prompt rather than being dropped. Steers that race prompt completion are recovered separately in submission order, retaining each input's `id`. With `agent`/`model`/`images` the message queues instead of steering, then the switch happens before that prompt starts (unknown agent/model → `input_error`, nothing runs). `id` is echoed in `prompt_start`/`prompt_end` for correlation. |
| `interrupt` | — | Cancel the active prompt (the in-band ^C): `prompt_end` with `termination_reason:"cancelled"`, and the session keeps accepting input. An interrupt that races prompt completion exits 130 rather than cancelling the next prompt. No-op when truly idle. |
| `shutdown` | — | Cancel any active prompt, save the session, emit `run_end`, exit 0. |

Stdin EOF is a graceful **drain**, not a cancel: the active prompt and any
queued prompts finish, then harness saves the session, emits `run_end`, and
exits 0.

Malformed JSON, an unknown `type`, missing required fields, or prompt-preparation
failure produce an
`{"type":"input_error","id?":…,"message":…}` event and the session keeps
running — bad input never kills it. For a syntactically valid envelope, harness
retains an available `id` even when the type or another field is invalid;
malformed JSON may have no recoverable ID.

Additional output events beyond the one-shot vocabulary:

| `type` | Fields | When |
|---|---|---|
| `prompt_start` | `prompt` (server-assigned number), `id?`, `cause?`, `text`, `agent`, `model`, `has_images` | Before each prompt |
| `prompt_end` | `prompt`, `id?`, `cause?`, `exit_code`, `termination_reason`, `usage`, `final_text` | After each prompt; the same optional `cause` as its start is repeated here |
| `input_error` | `id?`, `message` | Rejected input line |

Before reading the first prompt and between completed prompts, interactive JSON
runs the same idle boundary as the TTY REPL: it refreshes the MCP registry and
records/emits any resulting notice before the next `prompt_start`.

An accepted steer can release a currently blocked `background_jobs` wait without
cancelling its selected jobs. When its original selection later reaches completion
or timeout and no client work, EOF, shutdown, or interrupt is pending,
the driver emits a normal host-created continuation. Its `prompt_start` and
`prompt_end` carry `"cause":"detached_background_wait"` and omit `id`; the
aggregate wait result is delivered once as request-only context, not transcript
result text. Ordinary client prompts omit `cause`, preserving their event shape.

Session events between `prompt_start`/`prompt_end` carry the server-assigned
`prompt` number. Slash commands, session goals, idle compaction, history, and
interactive pickers are not part of the JSON mode —
agent and model switching are `prompt` fields, and a client that needs a
picker's data can use `--models --format json` / `--agents --format json`
and send an explicit switch.

## Flags

`harness --help` is the authoritative flag list: it is generated from the same
configuration catalog used for parsing and includes each setting's defaults and
corresponding environment variables. The reference below adds the behavioral
notes.

```text
-p <prompt|->     one-shot mode; "-" or piped stdin reads the prompt from stdin
-i, -initial-prompt <prompt>   run an initial prompt, then continue in the REPL
-model <provider>:<model>   model proxy target id
-model-proxy-url <url>   harness-model-proxy URL (default http://127.0.0.1:8765)
-model-proxy-api-key <key>   API key for harness-model-proxy (also HARNESS_MODEL_PROXY_API_KEY)
-mcp-proxy-api-key <key>   API key for harness-mcp-proxy (also HARNESS_MCP_PROXY_API_KEY)
-system-prompt <text|@file>    replace the static system prompt
-no-env           omit the environment context block (cwd/os/date/git)
-resume <session-dir>    load a saved session and continue
-session <session-dir>   explicit session directory
-histfile <path>      REPL history file path
-histfilesize <n>     max REPL history entries stored on disk (0 disables, default 1000)
-histsize <n>         max REPL history entries loaded into memory (0 disables, default 1000)
-max-turns <n>    turns per prompt; <=0 means unlimited (default 0)
-tool-timeout <s>   per-tool-call timeout backstop in seconds; <=0 disables (default 1800). A
                    hung tool that ignores cancellation is force-failed after this many
                    seconds so it cannot stall a turn; shell's own timeout_seconds stays
                    authoritative and is never cut below.
-max-prompt-tokens <n>   stop a prompt after this many accumulated tokens; 0 means unlimited
                    (default 0). Counts input + cache-read + cache-write + output + reasoning
                    tokens across every model call in the prompt, and breaks before the next paid
                    request with a `[stopped: prompt token budget N exceeded]` notice.
-max-output-tokens <n> per-turn output cap; 0 uses the automatic cap (default 0)
-max-prompt-cost <usd>   stop a prompt once its accumulated model cost reaches this many USD;
                    0 means unlimited (default 0). Applies only when provider usage reports
                    known cost; breaks before the next paid request with a
                    `[stopped: prompt cost budget $X reached]` notice. Complements -max-prompt-tokens.
-goal-max-continuations <n>   autonomous continuations allowed per goal before pausing
                    (default 25); 0 means unlimited (also HARNESS_GOAL_MAX_CONTINUATIONS)
-default-context-window <n>   fallback window for configured models without context metadata (default 256000)
-context-window <n>   override the model's context window (tokens)
-reasoning <profile> reasoning profile: default, none, minimal, low, medium, high, xhigh, or max
-reasoning-summary <mode> reasoning summary for Responses API: auto, concise, detailed, or none
-responses-stateful   use CLI-owned provider continuation when the selected target supports it (default true)
-retention-policy <mode>   live transcript retention: auto, age, pressure, or disabled (default auto)
-no-steer         disable in-prompt steering: queue input for the next prompt instead of injecting it before the next turn (default off; see "Steering")
-image-detail <level>   default image detail: auto, low, high, or original
-image <path|detail:path>   attach an image in one-shot mode or to the initial -i prompt; repeatable
-agent <name>     agent: auto (default), explore, plan, review, independent, or a config-defined agent
-handoff-agent <name>   default implementation agent for plan handoffs (default auto)
-delegate-output <mode> delegate UI: status (default), off, or curated scrolling lines on stderr
-delegate-tmux    follow each delegate child session in its own live tmux view (default: on inside tmux)
-delegate-tmux-layout <mode> delegate tmux layout: pane (default right-hand stack) or window (requires -delegate-tmux)
-web-search <mode>     server-side web search: off or auto (default off)
-trace-proxy      send W3C trace headers to the model and MCP proxies
-v                restore detailed per-call read summaries, show tool result snippets (first ~5 lines, dimmed), and show tool-call progress details
-tool-stream      show tool-call progress details (default false)
-show-diffs       show per-tool-call file diffs for built-in file edits (default true; use -show-diffs=false to disable);
                  diffs are syntax-highlighted with full-width tinted added/removed line backgrounds when color is on
                  (via background color erase; the tint covers only the text on terminals without BCE)
-stagnation-nudge   issue one strategy-reset instruction after two same-lane evaluator non-improvements (default true; use -stagnation-nudge=false to disable)
-q, --quiet       suppress status diagnostics and reasoning output unless -reasoning-summary is set;
                  still prints one per-prompt usage/cost line at an interactive terminal (suppressed only
                  when output is also non-TTY/piped), and one-shot runs always print the session summary
-version, --version  print release version and exit 0
--log-level <level>  diagnostic log level: debug, info, warn, error (also HARNESS_LOG_LEVEL)
-no-color         disable ANSI color (also HARNESS_NO_COLOR or presence-style NO_COLOR; color is TTY-only anyway)
-color-theme <dark|light>  syntax and displayed-diff palette (default dark; also HARNESS_COLOR_THEME)
-timestamps <mode>  bracketed status timestamps: short (default), full, or none
-repl-prompt <text>    REPL input prompt format (default "[{agent}] > "; supports placeholders such as {agent}, {model}, and {reasoning})
-repl-edit-mode <mode> REPL prompt edit mode: emacs (default) or vi
--format <text|json>  output format: text or json (informational commands; with -p: NDJSON run events; without -p and piped stdin: interactive NDJSON session) (default text)
--debug-request  dump the first provider-neutral model request as JSON and exit without calling the model
--agents         list configured agents and exit
--models         list configured providers and models and exit
--check-model-proxy    check harness-model-proxy reachability and exit
-hooks <file>    replace configured hooks with this hook config file
-otel-enabled, -otel-endpoint, -otel-protocol, -otel-timeout, -otel-service-name, -otel-hostname
                  OTLP/HTTP metrics export controls (see the configuration parameters matrix)
-config <file>    alternate config path
-h, --help        print this usage screen and exit 0
```

`color_theme` changes the truecolor syntax roles and displayed added/removed diff
rows used by live fenced Markdown, reasoning-summary fences, tool diffs, session
replay, and follow. It does not recolor inline code, links, headings, status lines,
or prose, and it does not set a terminal background. The only values are `dark`
and `light`: there is intentionally no `auto`, because Harness does not probe or
guess terminal backgrounds. Select the value matching the active terminal
profile. Theme selection is independent of ANSI enablement; `-no-color`, `NO_COLOR`,
and non-TTY output still suppress escapes without changing Markdown structure
or source text. `NO_COLOR` is presence-style: any non-empty value, including
`false`, disables ANSI. `HARNESS_NO_COLOR` is instead parsed as a boolean, so a
valid false value does not disable ANSI; malformed values fail configuration
loading. An explicitly empty or whitespace-only `--color-theme=` is invalid rather than an
instruction to fall through to environment, file, or default values.

`-system-prompt` accepts a `@file` reference. A literal leading `@` is escaped as
`@@`; `@~/path` expands through the current user's home directory. Relative
`@file` references in the config file resolve from the config file directory. It
replaces the built-in static instructions only; runtime sections such as
environment context, user/project `AGENTS.md`, skills, and agent prompts are
still composed around it.

Image attachments accept local PNG, JPEG, WebP, and non-animated GIF files.
Paths must be regular files; directories, devices, and FIFOs are rejected.
Images are embedded in `tree.ndjson` as provider-neutral base64 blocks so resumed
sessions remain self-contained; replay logs show only image metadata. Harness only sends queued
images when the current model explicitly advertises image input support. Manual
provider configs should set `input_modalities`, for example
`["text", "image"]`; models without `image` are treated as text-only and image
attachments are skipped with a warning.

Each image is limited to 10 MiB decoded and to the corresponding base64-encoded
ceiling. A 32 MiB encoded aggregate applies to the complete retained request,
including current and older user images plus images nested in tool results—not
just one prompt or dispatch batch. Persisted and direct-proxy rich blocks are
structurally validated, and malformed or over-budget requests are rejected
before token counting, provider lowering, or network activity.

Typed REPL prompts, initial `-i` prompts, and one-shot `-p` prompts also treat
literal `@path` or `@"path with spaces"` references to supported image files as
image attachments when the model supports images. The reference remains literal
prompt text; harness does not expand file contents or strip the `@...` text.
Pasted and externally edited REPL prompts stay fully literal and do not auto-attach
images from `@` references.

Provider configs can advertise hosted model tools with `server_tools`.
Currently `web_search` is recognized. It can be set at the provider level or on
individual model entries; the proxy also infers it for known web-search-capable
providers such as OpenAI Responses, Anthropic, Sakana, OpenRouter, MiMo, Kimi,
Z.AI, and native Google Interactions. Harness only declares it when
`-web-search auto` (or
`web_search:"auto"`) is enabled and the selected target advertises support.

Provider and model entries can advertise request-level scheduling tiers with
typed `service_tiers` objects. Each object has a target-suffix `id`, optional
`name` and `description`, a bounded `request` mapping, and an optional
tier-specific `price`. For example, OpenAI fast mode is represented as
`{"id":"fast","name":"Fast","request":{"service_tier":"fast"}}`.
OpenAI renamed priority processing to Fast mode; its older responses can still
report `priority`, which Harness accounts as Fast. Anthropic fast mode instead
maps to `request.speed:"fast"` plus its required
`request.betas` feature identifier. A model-level list overrides a provider
list.

Managed models receive these options from models.dev mode metadata; Codex
models receive them from the OpenAI Codex catalog. Harness canonicalizes
first-party OpenAI and Codex Fast metadata to `service_tier:"fast"`, but does
not infer or rewrite tiers for manual compatible endpoints; those must declare
their options explicitly. The proxy publishes each non-default mode as a
separate model target: `provider:model:fast`, `provider:model:flex`, and so on. Select a
mode with `-model`, `/model`, or an agent's `model` setting. `/fast
[on|off|status]` is a shortcut that switches between the current base target
and its `:fast` sibling; it reports unavailable when no such sibling exists.

The proxy uses a mode-specific catalog price when present. Provider-reported
served tier/speed metadata takes precedence, so a graceful downgrade is charged
at the standard model rate. A selected mode with no accurate catalog price
remains unpriced; token accounting continues normally and reject-unpriced
dollar budgets fail closed.

## Prompt Limits And Lifecycle

The agent loop has several controls against runaway work:

- `-max-turns` limits model turns within one prompt. It defaults to `0`
  (unlimited); `/max-turns` can change it for subsequent prompts in the current
  REPL session.
- `-max-prompt-tokens` stops before the next paid request once cumulative input,
  cache, output, and reasoning tokens reach the configured budget.
- `-max-prompt-cost` applies the equivalent cumulative USD ceiling when provider
  usage reports a known cost. Unpriced models cannot enforce this limit.
- When a normal model response reaches its output-token ceiling, Harness requests
  one continuation from the exact stopping point if the turn, token, and cost
  budgets permit it. A second ceiling stop ends the prompt with a visible notice.
- `-tool-timeout` is a per-tool-call backstop; `shell`'s own
  `timeout_seconds` remains authoritative.
- `-goal-max-continuations` caps autonomous `/goal` continuation prompts before
  pausing the goal; `0` disables that count cap.
- Repeated identical tool results and consecutive all-error tool turns are
  steered first and eventually stopped if the model does not change course.
  Consecutive single `shell` turns that keep the same underlying shell
  command while changing only downstream pipeline filters are likewise steered
  after four turns and stopped after twelve ignored repeats.
- Three consecutive turns that each perform one repository lookup get a steer
  to coissue independent top-level lookups. Twelve inspection-only turns without mutation, verification,
  wait, or coordination progress get one phase-transition steer. These semantic
  guards are advisory and never hard-stop a run; explicit user steering resets
  their streaks.
- One exact tool call (same tool, same input) that keeps failing with the same
  error is steered on the 2nd identical failure and blocked before it runs on
  the 3rd. A successful edit or write resets that per-prompt counter.

Turn-limit and loop-guard stops make one best-effort tools-disabled request so
the model can finish with a summary. Token and cost budgets stop without another
paid request.

Harness uses these terms consistently:

- A **prompt** is one top-level interaction started by user input.
- A **turn** is one model response plus the complete tool-result batch it
  requested. Turn numbers restart at 1 for each prompt.
- An **attempt** is one provider request for a turn; retries increase the
  attempt number without creating another turn.
- **Maintenance** calls such as compaction, cache prewarming, handoff summaries,
  and optional branch summaries are accounted separately and never increment
  the turn count.

While a prompt runs, status uses `[turn: N … │ prompt …]`; completion places an
optional compaction segment immediately after `ctx …` and before elapsed time.
The prompt count appears only when it is at least 1, the cumulative total appears
only when it is at least 2, and the whole segment is omitted when neither value
qualifies. For example: `ctx … · compactions 1 (3 total) · 4.3s`. On an
interactive TTY, active delegates are included in
that same transient row as `delegate d1 <agent>: <activity>`. Concurrent runs
show the count and most recently active child, for example
`3 delegates · latest d2 plan: tool read path="docs/usage.md"`. Background
and nested delegates use the same row while a model, tool, or prompt-work join
wait is active. These rows are process-local display state, not durable output,
and are absent from non-TTY output. The [Flags](#flags)
section lists defaults and config forms; [design section 8.1](design.md#81-prompt-and-turn-loop)
records the exact loop mechanics.

## Configuration And Environment

For every applicable setting, precedence is exactly **flag > environment >
project config file > global config file > default**. A setting without a flag starts at environment; a
config-file-only setting starts at the files. Every supplied candidate is parsed
and validated even when a higher-precedence value wins, so a valid flag does not
hide a malformed environment or file value. Repeated scalar flags use the last
occurrence.

The global config path itself resolves as **`-config` > `HARNESS_CONFIG` > an existing
`~/.config/harness/config.json`**. A missing conventional file is allowed. An
explicit flag or environment path must be non-empty, name an existing regular
file, and decode successfully. Config JSON must be one object with no unknown
fields, trailing values, or `null` scalar/structured settings. Omit a setting to
inherit it.

### Project configuration

When neither `-config` nor `HARNESS_CONFIG` is set, harness also looks for a
project config at `.harness/config.json` by walking from the current working
directory up to (and including) the nearest ancestor containing a `.git` entry
(directories and files both count, so worktrees and submodules are handled). If
no `.git` ancestor is found, only the working directory itself is checked. The
first regular file found in that walk is the project config; it is decoded
strictly and validated like the global file and its `@file` prompt references
are resolved relative to the project file's own directory.

The project file overlays the global file on a per-setting basis: for each key it
sets, it replaces the global file value while leaving any unset keys inherited.
`@file` prompt references and `hook_configs` entries are absolutized at decode
time against their owning file before the overlay, and provenance for file-backed
values points at the actual owning file. The final ordering is therefore
`flag > environment > project config file > global config file > default`.

Setting `-config` or `HARNESS_CONFIG` disables project discovery entirely for
that invocation. This is the documented escape hatch to run with only the
selected file: both `-config data/config.json` and
`HARNESS_CONFIG=/path/to/config.json` suppress `.harness/config.json` discovery,
and the selected file remains authoritative. `harness config check` and
`harness config show` honor the same rule and, when a project file is present,
`check` prints both files:

```text
config ok: ~/.config/harness/config.json (global)
project config: /path/to/repo/.harness/config.json
```

When only a project config is present, `check` reports `config ok: <path>
(project)`; with no config files, it reports the defaults-only form.
`/model` and REPL edit-mode saves always write to `Result.ConfigPath`—the
global or explicitly selected file—never to the project config.

Project configs can define hooks, `hook_configs`, MCP local commands, LSP
servers, and other command-bearing settings that harness will load
automatically when invoked inside that project tree. Treat the project config
as trusted code: only run harness in projects you trust and review
`.harness/config.json` before executing untrusted clones.

Source presence is distinct from content. An explicitly empty flag, environment
value, or JSON string is therefore a real candidate: plain strings that support
clearing (such as API keys) accept it, while required strings and enums reject
it. Boolean, integer, and number text must use the documented syntax; invalid
known environment values are errors rather than fallbacks. `HARNESS_NO_COLOR`
is an ordinary strict boolean, while standard `NO_COLOR` is presence-based and
disables ANSI when non-empty. `HARNESS_LOG_LEVEL` controls harness diagnostics.
Provider API-key environment variables and model-proxy configuration remain
owned by `harness-model-proxy`.

Defaults marked *derived* below are injected from the runtime context: the model
and MCP proxy URLs, history path, default agent, and whether Harness is running
inside tmux. They participate in provenance like other defaults, so any explicit
flag, environment, or file value—including `false` for tmux—wins.

### Config commands

```text
harness config list  [-format text|json|markdown]
harness config show  [-config path] [-format text|json] [-sources] [config-setting flags]
harness config check [-config path]
```

All three commands are offline: they never contact the model proxy or another
network service. `config list` renders the complete parameter catalog, including
source names, types, accepted values, and default semantics. `config show`
resolves settings and contextual defaults; `-sources` adds each winning source.
Its stable JSON form is a versioned envelope. Output always redacts non-empty
model/MCP proxy API keys, MCP header values, configured environment-map
values, and opaque LSP initialization options; there is no show-secrets option.
It contains user settings rather than a runtime dump and does not embed the
built-in prompt or materialize built-in agents. `config check` strictly decodes and validates the selected file and local
semantic dependencies such as agents, hooks, and `@file` references, then names
the checked path on success. Explicitly selected missing files are errors.

OTLP/HTTP JSON metrics are opt-in through `otel.enabled`; enabling them requires
an absolute HTTP(S) `otel.endpoint`. A base endpoint has `/v1/metrics` appended,
while an endpoint already ending in `/v1/metrics` is used as-is. Harness exports
cumulative metrics every 30 seconds in every run mode and once more at shutdown;
collector failures are retried on bounded transient errors and never fail a
prompt. `otel.headers` may come from JSON, `OTEL_EXPORTER_OTLP_HEADERS`, or
`HARNESS_OTEL_HEADERS` (in increasing precedence), and `${NAME}` references are
expanded before use. Header values are always redacted from config output. The
`host.name` resource defaults to the short OS hostname; explicitly setting
`otel.hostname` to an empty string disables it. Prompt-cache reporting exports
`harness.tokens.cache_read` and the write-inclusive denominator
`harness.tokens.prompt_input`; divide their sums to obtain the token-weighted
cache-read ratio.

`HARNESS_RESUME` and `HARNESS_SESSION` are invocation-only counterparts to
`-resume` and `-session`, not persistent settings. `HARNESS_REPL_INPUT_TRACE` is
a diagnostic knob that appends timestamped terminal-input events to its path
(`-` means stderr). `HARNESS_REPL_PASTE_HEURISTIC=off` disables the non-bracketed
paste fallback. These process controls are intentionally outside the parameter
catalog.

### Harness configuration parameters

This generated table is the canonical reference for harness setting flags,
environment variables, JSON paths, types, and defaults. The concise
`examples/harness/config.json` is an example, not a complete schema.

<!-- harness-config-parameters:start -->
| Key | Type | Accepted | Flags | Environment | JSON path | Default | Sensitive | Description |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `model` | `string` | - | `-model` | `HARNESS_MODEL` | `model` | unset (provider/model selected elsewhere) | no | Harness model setting. |
| `model_proxy_url` | `string` | - | `-model-proxy-url` | `HARNESS_MODEL_PROXY_URL` | `model_proxy_url` | derived: runtime model proxy URL | no | Harness model proxy url setting. |
| `model_proxy_api_key` | `string` | - | `-model-proxy-api-key` | `HARNESS_MODEL_PROXY_API_KEY` | `model_proxy_api_key` | unset | yes | Harness model proxy api key setting. |
| `trace_proxy` | `boolean` | `true`, `false` | `-trace-proxy` | `HARNESS_TRACE_PROXY` | `trace_proxy` | false | no | Harness trace proxy setting. |
| `system_prompt` | `string` | - | `-system-prompt` | `HARNESS_SYSTEM_PROMPT` | `system_prompt` | unset | no | Harness system prompt setting. |
| `no_env` | `boolean` | `true`, `false` | `-no-env` | `HARNESS_NO_ENV` | `no_env` | false | no | Harness no env setting. |
| `histfile` | `string` | - | `-histfile` | `HARNESS_HISTFILE` | `histfile` | derived: runtime history path | no | Harness histfile setting. |
| `histfilesize` | `integer` | - | `-histfilesize` | `HARNESS_HISTFILESIZE` | `histfilesize` | 1000 (disk entry cap) | no | Harness histfilesize setting. |
| `histsize` | `integer` | - | `-histsize` | `HARNESS_HISTSIZE` | `histsize` | 1000 (memory entry cap) | no | Harness histsize setting. |
| `max_turns` | `integer` | - | `-max-turns` | `HARNESS_MAX_TURNS` | `max_turns` | 0 (non-positive means unlimited) | no | Harness max turns setting. |
| `max_prompt_tokens` | `integer` | - | `-max-prompt-tokens` | `HARNESS_MAX_PROMPT_TOKENS` | `max_prompt_tokens` | 0 (unlimited) | no | Harness max prompt tokens setting. |
| `max_output_tokens` | `integer` | - | `-max-output-tokens` | `HARNESS_MAX_OUTPUT_TOKENS` | `max_output_tokens` | 0 (automatic) | no | Harness max output tokens setting. |
| `max_prompt_cost_usd` | `number` | - | `-max-prompt-cost` | `HARNESS_MAX_PROMPT_COST` | `max_prompt_cost_usd` | 0 (unlimited) | no | Harness max prompt cost usd setting. |
| `goal_max_continuations` | `integer` | - | `-goal-max-continuations` | `HARNESS_GOAL_MAX_CONTINUATIONS` | `goal_max_continuations` | 25 (zero means unlimited) | no | Harness goal max continuations setting. |
| `tool_timeout_seconds` | `integer` | - | `-tool-timeout` | `HARNESS_TOOL_TIMEOUT` | `tool_timeout_seconds` | 1800 (non-positive disables) | no | Harness tool timeout seconds setting. |
| `shell_timeout_seconds` | `integer` | - | - | `HARNESS_SHELL_TIMEOUT_SECONDS` | `shell_timeout_seconds` | 0 (tool default) | no | Harness shell timeout seconds setting. |
| `shell_background_timeout_seconds` | `integer` | - | - | `HARNESS_SHELL_BACKGROUND_TIMEOUT_SECONDS` | `shell_background_timeout_seconds` | 0 (tool default) | no | Harness shell background timeout seconds setting. |
| `default_context_window` | `integer` | - | `-default-context-window` | `HARNESS_DEFAULT_CONTEXT_WINDOW` | `default_context_window` | 256000 (tokens) | no | Harness default context window setting. |
| `context_window` | `integer` | - | `-context-window` | `HARNESS_CONTEXT_WINDOW` | `context_window` | 0 (no override) | no | Harness context window setting. |
| `reasoning` | `string` | `default`, `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` | `-reasoning` | `HARNESS_REASONING` | `reasoning` | provider default | no | Harness reasoning setting. |
| `reasoning_summary` | `string` | `auto`, `concise`, `detailed`, `none` | `-reasoning-summary` | `HARNESS_REASONING_SUMMARY` | `reasoning_summary` | provider default | no | Harness reasoning summary setting. |
| `image_detail` | `string` | `auto`, `low`, `high`, `original` | `-image-detail` | `HARNESS_IMAGE_DETAIL` | `image_detail` | "auto" | no | Harness image detail setting. |
| `web_search` | `string` | `off`, `auto` | `-web-search` | `HARNESS_WEB_SEARCH` | `web_search` | "off" | no | Harness web search setting. |
| `agents_md_warn_bytes` | `integer` | - | - | - | `agents_md_warn_bytes` | 8192 | no | Harness agents md warn bytes setting. |
| `tool_result_max_bytes` | `integer` | - | - | `HARNESS_TOOL_RESULT_MAX_BYTES` | `tool_result_max_bytes` | 0 (tool default) | no | Harness tool result max bytes setting. |
| `tool_result_max_lines` | `integer` | - | - | `HARNESS_TOOL_RESULT_MAX_LINES` | `tool_result_max_lines` | 0 (tool default) | no | Harness tool result max lines setting. |
| `read_default_limit` | `integer` | - | - | `HARNESS_READ_DEFAULT_LIMIT` | `read_default_limit` | 0 (tool default) | no | Harness read default limit setting. |
| `read_total_lines_max_bytes` | `integer` | - | - | `HARNESS_READ_TOTAL_LINES_MAX_BYTES` | `read_total_lines_max_bytes` | 0 (tool default) | no | Harness read total lines max bytes setting. |
| `read_result_max_bytes` | `integer` | - | - | `HARNESS_READ_RESULT_MAX_BYTES` | `read_result_max_bytes` | 0 (tool default) | no | Harness read result max bytes setting. |
| `read_result_max_lines` | `integer` | - | - | `HARNESS_READ_RESULT_MAX_LINES` | `read_result_max_lines` | 0 (tool default) | no | Harness read result max lines setting. |
| `compact_keep_turns` | `integer` | - | - | - | `compact_keep_turns` | 0 (all retained) | no | Harness compact keep turns setting. |
| `compact_keep_tokens` | `integer` | - | - | - | `compact_keep_tokens` | 20000 | no | Harness compact keep tokens setting. |
| `compact_auto_enabled` | `boolean` | `true`, `false` | - | - | `compact_auto_enabled` | true | no | Harness compact auto enabled setting. |
| `compact_trigger_percent` | `integer` | - | - | - | `compact_trigger_percent` | 78 | no | Harness compact trigger percent setting. |
| `compact_target_percent` | `integer` | - | - | - | `compact_target_percent` | 65 | no | Harness compact target percent setting. |
| `compact_idle_after_seconds` | `integer` | - | - | - | `compact_idle_after_seconds` | 0 (disabled) | no | Harness compact idle after seconds setting. |
| `compact_idle_trigger_percent` | `integer` | - | - | - | `compact_idle_trigger_percent` | 35 | no | Harness compact idle trigger percent setting. |
| `compact_timeout_seconds` | `integer` | - | - | - | `compact_timeout_seconds` | 300 | no | Harness compact timeout seconds setting. |
| `compact_summary_max_tokens` | `integer` | - | - | - | `compact_summary_max_tokens` | 0 (automatic) | no | Harness compact summary max tokens setting. |
| `compact_tool_result_max_bytes` | `integer` | - | - | - | `compact_tool_result_max_bytes` | 0 (automatic; negative disables truncation) | no | Harness compact tool result max bytes setting. |
| `delegate_max_turns` | `integer` | - | - | - | `delegate_max_turns` | 20 | no | Harness delegate max turns setting. |
| `delegate_max_depth` | `integer` | - | - | - | `delegate_max_depth` | 3 | no | Harness delegate max depth setting. |
| `delegate_max_active` | `integer` | - | - | - | `delegate_max_active` | 8 | no | Harness delegate max active setting. |
| `delegate_output` | `string` | `status`, `off`, `lines` | `-delegate-output` | `HARNESS_DELEGATE_OUTPUT` | `delegate_output` | "status" | no | Harness delegate output setting. |
| `delegate_tmux` | `boolean` | `true`, `false` | `-delegate-tmux` | `HARNESS_DELEGATE_TMUX` | `delegate_tmux` | derived: enabled inside tmux | no | Harness delegate tmux setting. |
| `delegate_tmux_max_windows` | `integer` | - | - | - | `delegate_tmux_max_windows` | 4 | no | Harness delegate tmux max windows setting. |
| `delegate_tmux_layout` | `string` | `pane`, `window` | `-delegate-tmux-layout` | `HARNESS_DELEGATE_TMUX_LAYOUT` | `delegate_tmux_layout` | "pane" | no | Harness delegate tmux layout setting. |
| `responses_stateful` | `boolean` | `true`, `false` | `-responses-stateful` | `HARNESS_RESPONSES_STATEFUL` | `responses_stateful` | true | no | Harness responses stateful setting. |
| `retention_policy` | `string` | `auto`, `age`, `pressure`, `disabled` | `-retention-policy` | `HARNESS_RETENTION_POLICY` | `retention_policy` | "auto" | no | Harness retention policy setting. |
| `retention_floor_tokens` | `integer` | - | - | - | `retention_floor_tokens` | 0 | no | Harness retention floor tokens setting. |
| `retention_keep_turns` | `integer` | - | - | `HARNESS_RETENTION_KEEP_TURNS` | `retention_keep_turns` | 4 | no | Harness retention keep turns setting. |
| `retention_result_head_bytes` | `integer` | - | - | `HARNESS_RETENTION_RESULT_HEAD_BYTES` | `retention_result_head_bytes` | 800 (clamped to the 4096-byte retention threshold) | no | Harness retention result head bytes setting. |
| `no_steer` | `boolean` | `true`, `false` | `-no-steer` | `HARNESS_NO_STEER` | `no_steer` | false | no | Harness no steer setting. |
| `agent` | `string` | - | `-agent` | `HARNESS_AGENT` | `agent` | derived: runtime default agent | no | Harness agent setting. |
| `handoff_agent` | `string` | - | `-handoff-agent` | `HARNESS_HANDOFF_AGENT` | `handoff_agent` | derived: runtime default implementation agent | no | Harness handoff agent setting. |
| `verbose` | `boolean` | `true`, `false` | `-v` | `HARNESS_VERBOSE` | `verbose` | false | no | Harness verbose setting. |
| `tool_stream` | `boolean` | `true`, `false` | `-tool-stream` | `HARNESS_TOOL_STREAM` | `tool_stream` | false | no | Harness tool stream setting. |
| `show_diffs` | `boolean` | `true`, `false` | `-show-diffs` | `HARNESS_SHOW_DIFFS` | `show_diffs` | true | no | Harness show diffs setting. |
| `stagnation_nudge` | `boolean` | `true`, `false` | `-stagnation-nudge` | `HARNESS_STAGNATION_NUDGE` | `stagnation_nudge` | true | no | Harness stagnation nudge setting. |
| `log_level` | `string` | `debug`, `info`, `warn`, `error` | `-log-level` | `HARNESS_LOG_LEVEL` | `log_level` | "info" | no | Harness log level setting. |
| `no_color` | `boolean` | `true`, `false` | `-no-color` | `HARNESS_NO_COLOR`, `NO_COLOR` | `no_color` | false (NO_COLOR is a presence-based override) | no | Harness no color setting. |
| `color_theme` | `string` | `dark`, `light` | `-color-theme` | `HARNESS_COLOR_THEME` | `color_theme` | "dark" | no | Harness color theme setting. |
| `timestamps` | `string` | `short`, `full`, `none` | `-timestamps` | `HARNESS_TIMESTAMPS` | `timestamps` | "short" | no | Harness timestamps setting. |
| `repl_prompt` | `string` | - | `-repl-prompt` | `HARNESS_REPL_PROMPT` | `repl_prompt` | "[{agent}] \\u003e " | no | Harness repl prompt setting. |
| `repl_edit_mode` | `string` | `emacs`, `vi` | `-repl-edit-mode` | `HARNESS_REPL_EDIT_MODE` | `repl_edit_mode` | "emacs" | no | Harness repl edit mode setting. |
| `mcp.enable` | `boolean` | `true`, `false` | - | `HARNESS_MCP_ENABLE` | `mcp.enable` | false | no | Harness mcp.enable setting. |
| `mcp.proxy` | `string` | - | - | `HARNESS_MCP_PROXY` | `mcp.proxy` | derived: runtime MCP proxy URL | no | Harness mcp.proxy setting. |
| `mcp.api_key` | `string` | - | `-mcp-proxy-api-key` | `HARNESS_MCP_PROXY_API_KEY` | `mcp.api_key` | unset | yes | Harness mcp.api key setting. |
| `mcp.max_tools` | `integer` | - | - | - | `mcp.max_tools` | 0 (unlimited) | no | Harness mcp.max tools setting. |
| `mcp.local.enable` | `boolean` | `true`, `false` | - | `HARNESS_MCP_LOCAL_ENABLE` | `mcp.local.enable` | false | no | Harness mcp.local.enable setting. |
| `mcp.local.command` | `string` | - | - | - | `mcp.local.command` | unset | no | Harness mcp.local.command setting. |
| `lsp.enable` | `boolean` | `true`, `false` | - | `HARNESS_LSP_ENABLE` | `lsp.enable` | false | no | Harness lsp.enable setting. |
| `lsp.prewarm` | `boolean` | `true`, `false` | - | `HARNESS_LSP_PREWARM` | `lsp.prewarm` | true | no | Harness lsp.prewarm setting. |
| `lsp.serena.enable` | `boolean` | `true`, `false` | - | `HARNESS_LSP_SERENA_ENABLE` | `lsp.serena.enable` | false | no | Harness lsp.serena.enable setting. |
| `lsp.serena.command` | `string` | - | - | - | `lsp.serena.command` | "serena" | no | Harness lsp.serena.command setting. |
| `otel.enabled` | `boolean` | `true`, `false` | `-otel-enabled` | `HARNESS_OTEL_ENABLED` | `otel.enabled` | false | no | Harness otel.enabled setting. |
| `otel.endpoint` | `string` | - | `-otel-endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT`, `HARNESS_OTEL_ENDPOINT` | `otel.endpoint` | unset | no | Harness otel.endpoint setting. |
| `otel.protocol` | `string` | `http/json` | `-otel-protocol` | `HARNESS_OTEL_PROTOCOL` | `otel.protocol` | "http/json" | no | Harness otel.protocol setting. |
| `otel.timeout_seconds` | `integer` | - | `-otel-timeout` | `HARNESS_OTEL_TIMEOUT` | `otel.timeout_seconds` | 5 (seconds) | no | Harness otel.timeout seconds setting. |
| `otel.service_name` | `string` | - | `-otel-service-name` | `OTEL_SERVICE_NAME`, `HARNESS_OTEL_SERVICE_NAME` | `otel.service_name` | "harness" | no | Harness otel.service name setting. |
| `otel.hostname` | `string` | - | `-otel-hostname` | `HARNESS_OTEL_HOSTNAME`, `OTEL_HOSTNAME` | `otel.hostname` | short hostname (empty disables host.name) | no | Harness otel.hostname setting. |
| `agents` | `object` | - | - | - | `agents` | unset | no | Structured agents settings. |
| `mcp.headers` | `object` | - | - | - | `mcp.headers` | unset | yes | Structured mcp.headers settings. |
| `mcp.disabled_servers` | `string[]` | - | - | - | `mcp.disabled_servers` | unset | no | Structured mcp.disabled_servers settings. |
| `mcp.local.args` | `string[]` | - | - | - | `mcp.local.args` | unset | no | Structured mcp.local.args settings. |
| `mcp.local.env` | `object` | - | - | - | `mcp.local.env` | unset | yes | Structured mcp.local.env settings. |
| `lsp.tools` | `string[]` | - | - | - | `lsp.tools` | unset | no | Structured lsp.tools settings. |
| `lsp.servers` | `object` | - | - | - | `lsp.servers` | unset | yes | Structured lsp.servers settings. |
| `lsp.serena.args` | `string[]` | - | - | - | `lsp.serena.args` | unset | no | Structured lsp.serena.args settings. |
| `lsp.serena.env` | `object` | - | - | - | `lsp.serena.env` | unset | yes | Structured lsp.serena.env settings. |
| `hooks` | `object` | - | `-hooks` | - | `hooks` | unset | no | Structured hooks settings. |
| `hook_configs` | `string[]` | - | - | - | `hook_configs` | unset | no | Structured hook_configs settings. |
| `otel.headers` | `object` | - | - | `OTEL_EXPORTER_OTLP_HEADERS`, `HARNESS_OTEL_HEADERS` | `otel.headers` | unset | yes | Structured otel.headers settings. |
| `otel.resource_attributes` | `object` | - | - | - | `otel.resource_attributes` | unset | no | Structured otel.resource_attributes settings. |
<!-- harness-config-parameters:end -->

`HARNESS_WEB_SEARCH=auto` is equivalent to `-web-search auto`; `off` disables it.
`auto` only declares web search when the selected model-proxy target advertises
`server_tools:["web_search"]`.

- `--debug-request` prints the first provider-neutral `llm.Request`, context
  estimate, active tools, reasoning settings, and request byte counts. It
  resolves the model proxy catalog, then exits before prewarm, session hooks,
  session writes, or any model stream.
- `--agents` prints the resolved agent list without contacting the model proxy.
  Text output marks interactive-selectable agents with `[interactive]`; JSON
  output includes `interactive_selectable` for every agent. `--models` prints
  the configured proxy model catalog. Use `--format json` with
  `--agents`, `--models`, or `--check-model-proxy` for structured output.
- Context-efficiency knobs are config-file-only except where noted:
  `agents_md_warn_bytes`, `compact_keep_turns`, `compact_keep_tokens`,
  `compact_auto_enabled`, `compact_trigger_percent`,
  `compact_target_percent`, `compact_idle_after_seconds`,
  `compact_idle_trigger_percent`, `compact_timeout_seconds`, `compact_summary_max_tokens`,
  `compact_tool_result_max_bytes`, and `retention_floor_tokens`.
  Tool-result truncation is controlled by config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. `read` defaults to a 1000-line source window
  and a byte allowance of `max(64 KiB, limit × 512 bytes)`, bounded by a
  configurable 256 KiB hard ceiling. It has no independent default result-line
  cap: `limit` is the line bound unless a global or read-specific result-line cap
  is configured. Each read cap inherits independently when its corresponding
  global cap is configured. Configure `read_default_limit`,
  `read_result_max_bytes` (the hard byte ceiling), and `read_result_max_lines`,
  or `HARNESS_READ_DEFAULT_LIMIT`, `HARNESS_READ_RESULT_MAX_BYTES`, and
  `HARNESS_READ_RESULT_MAX_LINES`. In an agent turn, all reads additionally share
  `min(48,000 estimated tokens, 20% of context remaining after reserving the next
  output allowance)`; Harness uses its coarse four-bytes-per-token context
  estimate and divides the allowance across parallel or staged read calls. Each
  share includes bounded hook/artifact suffixes; an actionable continuation
  receipt has a 64-byte minimum when the calculated share is smaller.
  Result-limited reads stream bounded physical lines, keep complete source lines,
  and report the exact next offset, including reads that reached EOF before
  result limiting. If one physical line cannot fit, its buffered fragments are
  discarded and the receipt gives targeted-shell guidance instead of retaining
  the whole line. An explicit byte ceiling too small for the compact
  `[read offset=N]` fallback necessarily clips that receipt. Natural pagination
  notices report exact file bytes. Regular files no larger than
  1 MiB also report exact total lines;
  configure that threshold with `read_total_lines_max_bytes` or
  `HARNESS_READ_TOTAL_LINES_MAX_BYTES`. The former
  `read_file_*`, `rg_result_*`, and `grep_result_*` settings are removed; strict
  config decoding rejects them rather than silently ignoring a stale limit. The delegate
  tool also has config-file-only `delegate_max_turns` (maximum per-child
  tool-enabled loop budget), `delegate_max_depth` (recursive depth cap, root
  depth `0`), and `delegate_max_active` (root-wide simultaneous descendant cap,
  default `8`). Completed descendants have no lifetime cap. The former
  `delegate_max_descendants` setting is removed; strict config decoding rejects
  that obsolete key.
  `delegate_output` / `HARNESS_DELEGATE_OUTPUT` / `-delegate-output` accepts
  `status` (the default one-row TTY display), `off` (no delegate-specific UI),
  and `lines` (the status row on a TTY plus curated scrolling child activity on
  stderr). The TTY row updates once per second; when tool arguments and delegate
  details do not all fit, it keeps a compact wait label, elapsed counter, and
  delegate identity. On non-TTY output, `status` is silent while `lines` still
  writes stderr. Quiet mode is authoritative and suppresses both forms, including
  child reasoning summaries when `-reasoning-summary` was explicitly enabled.
  `-v` and `-tool-stream` do not enable or expand child lines.

  | mode | stdout TTY | stdout non-TTY |
  |---|---|---|
  | `status` | compact delegate status row | no delegate output |
  | `off` | no delegate-specific output | no delegate output |
  | `lines` | compact status row plus scrolling stderr lines | scrolling stderr lines |

  Inline lines use stable, ANSI-free prefixes:

  ```text
  [delegate d1 explore] assistant: Checking the call path.
  [delegate d2 plan depth=2] tool read path="docs/design.md" started
  [delegate d1 explore] completed · 3 turns
  ```

  They never write stdout or enter the parent transcript/model context. Harness
  emits only bounded, sanitized lifecycle, assistant, allowlisted tool/notice,
  structured retry, and permitted reasoning-summary fields; it omits tasks,
  raw tool results, error/provider text, commands, URLs, request IDs, and opaque
  reasoning. An incomplete parent plain/Markdown line delays delivery until a
  natural line boundary rather than changing parent stdout. The process-local
  feed retains at most 512 events / 256 KiB and reports loss as
  `[delegate output] omitted N events`. Use
  `harness session replay --follow <child-session>` for complete durable child
  output. `delegate_tmux` / `HARNESS_DELEGATE_TMUX` / `-delegate-tmux`
  automates that full-fidelity view: inside a tmux session each
  delegate child opens its own live tmux view. The default is on inside tmux
  (`$TMUX` set) and off otherwise; an explicit `false` from the flag, env, or
  config file keeps it off inside tmux. `delegate_tmux_layout` /
  `HARNESS_DELEGATE_TMUX_LAYOUT` / `-delegate-tmux-layout` selects `pane`
  (default right-hand pane stack) or `window` (one detached window per child).
  Every child closes its view when it ends, including failed or canceled
  children; any still-tracked views close when harness exits. Config-only
  `delegate_tmux_max_windows` (default 4) caps simultaneous views. The feature
  is display-only and independent of `delegate_output`; outside tmux it
  degrades to one startup warning when explicitly enabled (suppressed by
  quiet, or logged at debug level when auto-enabled), and pane layout
  degrades to windows if `TMUX_PANE` is missing.
- Tool-surface limits for MCP and LSP are config-file-only: `mcp.max_tools` caps
  how many discovered remote MCP tools are auto-exposed (`0` = unlimited),
  `mcp.disabled_servers` is a list of remote MCP server names dropped from
  auto-exposure, and `lsp.tools` registers only the listed subset of LSP tools
  (empty = core set, `["all"]` = full surface). See [mcp.md](mcp.md) and [lsp.md](lsp.md). An explicit
  `allowed_tools` whitelist can still name a tool that auto-exposure excluded.
  If one agent's included MCP/LSP declarations exceed 32 KiB, Harness exposes
  them lazily from one deferred inventory. Supported native OpenAI Responses
  requests search complete MCP-server/LSP namespaces; supported Anthropic
  Messages requests search top-level deferred declarations containing every
  tool in those groups. Other request paths use `tool_catalog`, where selected
  names can be activated for the next model turn.
- Serena can be launched independently with `lsp.serena.enable=true` or
  `HARNESS_LSP_SERENA_ENABLE=true`; this does not imply `lsp.enable=true`.
- A single turn's output is capped at the configured
  `max_output_tokens` value when set, otherwise at one quarter of the effective
  `context_window` (with a very high 1,000,000-token runaway ceiling). A model's
  configured `output_limit`, when known, is a ceiling rather than the default.
  The chosen cap is then clamped to the counted or estimated remaining context
  window. This client-side runaway brake is distinct from `-max-prompt-tokens`,
  which is the cumulative per-prompt token *budget* across all model calls. If a
  provider reports a smaller real context window in an overflow error, harness
  learns that window for the session and retries once. When the cap is reached,
  harness surfaces `[stopped: model reached max tokens]`. Provider configs may
  set `min_output_tokens` for endpoints that reject very small output caps. If
  an OpenAI-compatible endpoint rejects a request with a parseable minimum
  `max_tokens` error, the model proxy retries once at the inferred floor and logs
  the inferred/configured values. First-party Chat Completions requests to
  `api.openai.com` encode the cap as `max_completion_tokens`; custom and
  compatible endpoints continue to use `max_tokens`.
- Before normal model requests, harness resolves input tokens in tiers:
  provider-specific count APIs for OpenAI Responses and Anthropic Messages when
  available through `harness-model-proxy`; a local `o200k_base` BPE estimate for
  OpenAI/OpenRouter Chat Completions; then the coarse byte-based heuristic.
  Once a turn reports real usage, the *reported* context (`turn_attempt_start`
  and `/context`) anchors to that actual input (uncached plus cache read/write)
  plus a local estimate of only the messages appended since — count APIs can
  systematically miss provider-billed opaque replay state such as thinking
  signatures. Compaction or retention resets the anchor until the next measured
  turn. In the byte heuristic, opaque payloads (thinking signatures,
  encrypted/redacted reasoning, interaction steps) are weighted separately from
  prose (8 vs 4 bytes/token) because base64-style blobs tokenize much coarser.

Harness automatically adds static AGENTS instructions from
`~/.agents/AGENTS.md`, then from `AGENTS.md` in the current working directory.
Missing files are ignored; unreadable existing files fail startup.

Reasoning selection is opt-in via `reasoning` / `HARNESS_REASONING` /
`-reasoning`. Accepted profiles are `default`, `none`, `minimal`, `low`,
`medium`, `high`, `xhigh`, and `max`. The model proxy maps those profiles to the
closest provider/model-specific effort value or, for budget-token models, to a
percentage of the supported maximum budget. Responses API reasoning summaries
are controlled separately by `reasoning_summary` /
`HARNESS_REASONING_SUMMARY` / `-reasoning-summary`; they default off and are
displayed only when explicitly enabled. `-q` disables reasoning summary output
unless `-reasoning-summary` is explicitly set on the CLI.

Responses continuation is on by default for proxy providers that report both
provider continuation support and `continuation_stateful:true` in the proxy
catalog. This includes supported Responses and Gemini Interactions targets.
Disable it with
`responses_stateful:false`, `HARNESS_RESPONSES_STATEFUL=false`, or
`-responses-stateful=false`. Harness owns the response/interaction ID, the
anchored message count, and a SHA-256 fingerprint of the exact provider-neutral
transcript prefix represented by that ID. It persists this state with the
session and sends only the appended suffix on the next request. A missing,
malformed, out-of-range, or fingerprint-mismatched anchor is discarded before
the request and the complete transcript is sent safely.

Responses provider configs can opt into native compaction with
`responses_compaction:true`. The model proxy advertises this capability per
target; Harness does not infer it from `api_type:"responses"` alone. Managed
providers using the canonical official OpenAI API or ChatGPT Codex identity and
base URL default on, while `responses_compaction:false` explicitly disables the
capability. The two canonical first-party backends use Responses compaction v2
through `POST /responses` with a trailing `compaction_trigger`. Compatible and
manual providers remain opt-in and use the standalone v1 contract, so enable it
only when their base URL implements `POST /responses/compact` with the OpenAI
response-compaction contract.

Live transcript retention defaults to `auto`, which batches eligible trimming
into pressure epochs when the larger of the local and provider-derived estimates
reaches 60% of the context window. `auto` and `pressure` do not rewrite history
below pressure; select explicit `age` to bound replay by the retention keep
window. Experiments can override this with `retention_policy`,
`HARNESS_RETENTION_POLICY`, or `-retention-policy`; accepted values are `auto`,
`age`, `pressure`, and `disabled`. Disabling live retention does not disable compaction or
provider-overflow recovery. The config-only `retention_floor_tokens` adds an
absolute-context fallback to the pressure epoch: when the estimated context
reaches the floor, the same hysteretic trim of aged tool results runs
even below the 60% window high-water mark — which a very large (e.g. 1M-token)
window may never reach. It defaults to 0 (disabled) and stays opt-in because
trimming rewrites history and invalidates the cache prefix from the first
trimmed block, the same tradeoff pressure retention already makes at the
high-water mark.

Retention ages results by `retention_keep_turns` completed turns (default 4,
`HARNESS_RETENTION_KEEP_TURNS`), decoupled from the eight-turn compaction
suffix. A result is trimmable when it is read-only, or when its exact bytes are
durably archived and it is twice that age. A superseded `write`/`edit` input is
replaced with a compact path receipt once a newer successful mutation to the
same path exists. A trimmed result keeps `retention_result_head_bytes` live
bytes (default 800, `HARNESS_RETENTION_RESULT_HEAD_BYTES`, clamped to the
4096-byte eligibility threshold) plus a typed recovery hint pointing at the
archived original.

The model proxy is a stateless continuation pass-through: it never stores,
reconstructs, trims, or resets a continuation, and it never retries a
continuation suffix. It ordinarily forwards the CLI-supplied messages, `store`,
and previous-response/interaction ID unchanged. An unsupported target rejects
continuation with `continuation_unsupported`. A connection-affine WebSocket
provider can reject a known-unavailable ID before streaming with
`previous_response_unavailable`; an upstream previous-ID rejection is also
passed through unchanged. The exception is a Responses WebSocket
`websocket_connection_limit_reached` error: the proxy normalizes it to
`previous_response_unavailable` for a socket-scoped continuation, or retries it
once when the request has no previous ID and already contains full context. In
the continuation case harness clears its local anchor and performs one
full-context resend. A miss that recovers is retained in proxy logs, metrics,
and session diagnostics but is not rendered as a terminal model error; the
normal state-reset notice remains visible unless quiet mode suppresses it. If
the resend fails, harness still renders the final error normally.

Retention, compaction, branch navigation, agent changes, and true base-model
changes reset the CLI-owned anchor. Switching a target between its base and
`/fast` variant preserves a valid anchor because both variants share the same
provider/model continuation chain. `-responses-stateful=false` sends complete
messages without `store` or a previous ID on every request.

Responses provider configs may also set `responses_websocket:true` to have the
model proxy use the Responses WebSocket transport instead of HTTP SSE. The proxy
defaults this on for `codex_oauth` Responses providers and preserves an explicit
`responses_websocket:false` override. WebSocket connections live in a bounded
per-pod lease pool. Harness sends its opaque session ID as
`X-Harness-Session`, allowing a load balancer to keep that connection-affine
traffic on one pod as a performance optimization. Correctness does not depend
on stickiness: a pod/socket miss produces the reset-and-resend path above.

Gemini Interactions targets are continuation-capable by default; set
`interactions_stateful:false` in the provider config to make the catalog reject
continuation for that target. The CLI owns and persists
`previous_interaction_id` under the same fingerprint contract. Signed `thought`,
Google Search call, and Google Search result steps remain in the invisible
provider-neutral transcript so a full-history fallback is valid after a proxy
restart. Stored interactions remain subject to the provider account's
applicable retention policy.

## Model Proxy

### Commands and configuration inspection

`harness-model-proxy --help` renders the generated root command catalog; use
`harness-model-proxy <command> --help` for command-scoped flags. With no
arguments the binary runs `serve`, preserving its historical default.

The model proxy also provides offline configuration inspection:

```text
harness-model-proxy config list  [-format text|json|markdown]
harness-model-proxy config show  [-config path] [-format text|json] [-sources] [setting flags]
harness-model-proxy config check [-config path]
```

An explicit `-config` path wins. Without one, the proxy discovers an existing
`~/.config/harness-model-proxy/config.json` (or its documented temporary
fallback when `HOME` is unavailable). Setting flags override the file; lifecycle
environment variables override their corresponding file values and are in turn
overridden by flags. Empty-value handling remains setting-specific: for example,
an empty log or metrics-listener override preserves the lower-precedence value,
an empty instance ID requests derivation, and an empty duration is invalid.

`config list` reads no configuration. `config show` resolves only the top-level
proxy settings and never opens referenced provider, API-key, or OAuth token
files. Its versioned JSON projection may show safe file paths but never provider
credentials, key contents, or token contents. `config check` additionally reads,
decodes, and locally validates referenced provider files, including auth and
model-catalog normalization, using the normal warning-and-skip rules. It performs
no network requests and does not mutate configuration or managed state.

#### Model-proxy configuration parameters

<!-- model-proxy-config-parameters:start -->
| Key | Type | Accepted | Flags | Environment | JSON path | Default | Sensitive | Description |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `provider_configs` | `array` | - | - | - | `provider_configs` | [] | no | Ordered provider configuration file paths, resolved relative to the main configuration file. |
| `default_context_window` | `integer` | - | - | - | `default_context_window` | 256000 | no | Fallback context window for models without an explicit value. |
| `log_level` | `string` | `debug`, `info`, `warn`, `error` | `-log-level` | - | `log_level` | info | no | Proxy log level. |
| `log_format` | `string` | `json`, `text` | `-log-format` | - | `log_format` | json | no | Proxy log format. |
| `models_dev_cache_ttl` | `duration` | - | `-models-dev-cache-ttl` | - | `models_dev_cache_ttl` | 24h | no | models.dev cache refresh interval; zero disables periodic refresh. |
| `provider_models_cache_ttl` | `duration` | - | `-provider-models-cache-ttl` | - | `provider_models_cache_ttl` | 1h | no | Authenticated provider model catalog refresh interval; zero disables background refresh. |
| `drain_delay` | `duration` | - | `-drain-delay` | `HARNESS_MODEL_PROXY_DRAIN_DELAY` | `drain_delay` | 5s | no | Readiness propagation delay before API shutdown. |
| `shutdown_timeout` | `duration` | - | `-shutdown-timeout` | `HARNESS_MODEL_PROXY_SHUTDOWN_TIMEOUT` | `shutdown_timeout` | 5m | no | Maximum graceful stream drain time. |
| `instance_id` | `string` | - | `-instance-id` | `HARNESS_MODEL_PROXY_INSTANCE_ID` | `instance_id` | derived: generated at startup | no | Proxy instance identifier. |
| `api_keys_file` | `string` | - | `-api-keys-file` | - | `api_keys_file` | derived: api_keys.json beside the selected config | no | Accepted API keys file path; this setting is a path and never exposes file contents. |
| `metrics_enabled` | `boolean` | - | `-no-metrics` | - | `metrics.enabled` | true | no | Whether the Prometheus metrics endpoint is enabled; the command-line flag is inverse. |
| `metrics_listen` | `string` | - | `-metrics-listen` | - | `metrics.listen` | 127.0.0.1:9090 | no | Prometheus metrics listen address. |
<!-- model-proxy-config-parameters:end -->

### Setup

Run `harness-model-proxy setup` to create a proxy config and a provider config
from provider and models.dev metadata, append a new provider config to an existing proxy config, or
update an existing configured provider without configuring a proxy default model.
Setup lists harness-supported providers, prompts for the API key when the
provider needs one, queries the provider's authenticated model-list endpoint
when supported, then lets you choose which provider models are available
locally. Newly discovered models are choices, not automatically enabled. If
direct discovery is unavailable, setup uses models.dev plus the Codex fallback
catalog. If models.dev omits a provider API URL, setup can still derive
first-party OpenAI and Anthropic defaults from exact `@ai-sdk/openai` and
`@ai-sdk/anthropic` package metadata, and maps plain `@ai-sdk/google` to
the native Gemini Interactions endpoint. Managed Google configs use
`api_type:"interactions"`, explicitly default `interactions_stateful:true`, and
advertise `web_search`; Vertex Google package variants are not auto-configured.
Managed Meta configs default to `api_type:"responses"`, following Meta's
recommended protocol. Setup and refresh query Meta's authenticated
`GET /v1/models` endpoint directly; its ID-only catalog spans multiple API
families, so direct-only `muse-spark-*` models are accepted as Responses models
while image-generation and transcription IDs are not advertised as text targets
under the default policy. An explicit `include_unknown_models:true` discovery
override bypasses that conservative family filter.
Anthropic `base_url` values are versioned API prefixes (normally ending in
`/v1`); the dialect appends `/messages` or `/messages/count_tokens`.

The special `openai-codex` provider uses ChatGPT subscription auth instead of an
API key, exposes models from the OpenAI Codex catalog, and reports token usage
without dollar pricing. Authenticated model discovery uses the numeric
compatibility version of the official stable Codex CLI bundled with Harness; it
does not send the Harness application version. It omits Responses
`max_output_tokens` because the Codex backend rejects that parameter.
Input-token preflight counts use a local
`o200k_base` estimate because the Codex CLI protocol does not expose a separate
count-token endpoint. The proxy also uses the Responses WebSocket transport by
default for this provider, matching Codex's stateful continuation path while
sending `store:false` to the ChatGPT backend. Managed setup explicitly emits the
hashed conversation cache key as `prompt_cache_key`. Its startup warm-up uses
WebSocket `generate:false`; only that transport returns an explicit transcript
anchor at message zero, which the owner goroutine installs if the model,
session, transcript, and continuation generation are still unchanged. HTTP
warm-up usage is retained but its disposable response ID is never installed.
ChatGPT Codex advertises native Responses compaction. Harness uses the normal
streamed `POST /responses` route with a trailing `compaction_trigger`, rather
than the unavailable legacy `POST /responses/compact` route; malformed or
rejected maintenance responses fall back to textual compaction.
After setup, run:

```sh
harness-model-proxy auth login openai-codex
```

A successful OAuth login immediately refreshes that provider's configured
allowlist and caches the authenticated catalog. Models absent from a complete
response are removed; newly discovered models remain disabled until setup is
run again. Failure to refresh the catalog is only a warning and does not undo a
successful login.

The `kimi-for-coding` provider is deliberately configured with
`api_type:"openai"` even though models.dev lists the Anthropic SDK package.
Kimi for Coding serves both protocols off one base URL (OpenAI at
`/coding/v1/chat/completions`, Anthropic at `/coding/v1/messages`; see Kimi's
service-endpoint docs), and the OpenAI chat-completions shape replays preserved
thinking as compact `reasoning_content` — the sanctioned mechanism, matching
Kimi's own CLI (`packages/kosong/src/providers/kimi.ts`) and Kimi's Preserved
Thinking guide — instead of the Anthropic shape's much larger signed thinking
blobs. Managed setup and `refresh-models` therefore also write
`reasoning_replay:true` for this provider so historical assistant reasoning
round-trips (see the `reasoning_replay` provider quirk above). Do not disable
reasoning replay for kimi-k3: Kimi's docs make preserved thinking mandatory in
tool-call loops.

Provider configs accept an optional `auth` block in place of `api_key` /
`api_key_env`; when `auth` is present, API-key fields are ignored and there is no
fallback if auth fails. Supported auth shapes include `token_command`, `oauth2`,
and `codex_oauth`. Codex terminal refresh failures are cached only in the
failing process and keyed by a digest of the rejected refresh token; they are
never written into the shared token file. A replica rereads the file before
returning that cached failure and immediately after a terminal refresh response,
so it can adopt a valid token rotated by a peer without overwriting it.

Provider configs also accept an optional model-discovery override:

```json
{
  "model_discovery": {
    "enabled": true,
    "url": "https://provider.example/v1/models",
    "format": "openai",
    "include_unknown_models": false
  }
}
```

With no block, managed providers are detected from their name, dialect, and
base URL. Supported formats are `openai`, `openrouter`, `codex`, `anthropic`,
`gemini`, and `meta`; the override URL must be an absolute HTTP(S) URL without
userinfo or a fragment. Set `enabled:false` to use catalog-only availability.
Generic OpenAI-compatible ID-only responses validate configured/models.dev models
but do not introduce unknown models. Rich capability fields can establish that a
new model is generative; Sakana and the provider-specific adapters trust their
generative-only results. `include_unknown_models` explicitly overrides that
policy. For Meta, `include_unknown_models:true` also opts image-generation and
transcription IDs into the local target list; only use it when another layer can
serve those IDs through the configured dialect.

Provider configs may also set `prompt_cache` to control how the stable harness
conversation cache key is sent to OpenAI-compatible backends. This cache
affinity survives continuation resets such as compaction and model/agent/tool
changes, is shared with delegates, and rotates for a new logical session.
`key_field` accepts
`auto` (default), `none`, `prompt_cache_key`, or `session_id`; `auto` sends
`prompt_cache_key` to first-party OpenAI and ChatGPT Codex endpoints,
`session_id` to OpenRouter, and omits cache key fields for other custom base
URLs. `affinity_headers` can
copy the same key into non-auth routing headers such as `x-session-id`. The
proxy derives the provider-facing value as a SHA-256 hash of harness's local
cache-affinity key, so providers do not receive the raw identifier.
`explicit_breakpoints` is a tri-state Responses override: omitted enables one
conservative stable-message breakpoint only for supported first-party OpenAI
models, `false` disables it, and `true` opts a compatible Responses backend in.
Harness leaves OpenAI's default implicit breakpoint enabled, keeps top-level
`instructions` unchanged, and omits explicit markers from token-count and
compaction requests.

Responses provider configs may set `responses_tool_search`. When omitted,
Harness enables native hosted tool search on the canonical OpenAI API for
GPT-5.4-or-newer models except GPT-5.4 Nano, and explicitly for GPT-5.3 Codex
Spark. It is also enabled for the canonical ChatGPT Codex backend used by
`openai-codex`, with the same Nano exclusion. Harness sends each complete
deferred MCP-server or LSP group as
one namespace tool, adds `{"type":"tool_search"}`, and omits the local
`tool_catalog` plus any already activated duplicate declarations from that
request. Set `false` to disable the native path, or `true` to opt a compatible
Responses endpoint/model in when it implements the same contract. Explicit
settings override the model defaults, including for Nano.
Hosted `tool_search_call` and `tool_search_output` items and the selected
function namespace are persisted for exact stateless replay. If a native
request is rejected before output with a structured status-400
`invalid_request_error` for the `tools` parameter, whether in an HTTP response or
WebSocket error frame, Harness retries once with native search disabled so the
same neutral inventory is exposed through `tool_catalog`. That downgrade is
remembered for the model on that provider instance. Other errors and post-output
failures are not downgraded.

Anthropic provider configs may set `anthropic_tool_search` to `auto`, `bm25`,
`regex`, or `off`. Omitted and `auto` select the versioned BM25 search tool only
on the canonical Anthropic API for supported Claude Opus, Sonnet, and Haiku 4.5+
or Claude 5-family models. Explicit `bm25` or `regex` also opts a compatible
Anthropic Messages endpoint into that exact wire contract; `off` disables the
native path. For compatibility with boolean provider files, `true` means `bm25`
and `false` means `off`.

When enabled, Harness flattens every complete deferred MCP/LSP group into the
top-level Anthropic `tools` array with `defer_loading:true`, appends the selected
non-deferred `tool_search_tool_*_20251119` declaration, and omits the local
`tool_catalog` plus activated duplicates. Anthropic's `server_tool_use` and
`tool_search_tool_result` blocks are persisted in their original block order for
same-domain replay; only the ordinary `tool_use` selected from a search result is
dispatched locally. Anthropic capability selection happens before dispatch; an
upstream rejection is returned without retry or catalog downgrade.

Provider configs may set `reasoning_replay` to control how much historical
reasoning state a dialect replays on the wire. The default leaves each
dialect's behavior unchanged: the OpenAI chat-completions dialect replays
nothing (strict OpenAI-compatible servers reject unknown fields), and the
Anthropic dialect replays all signed thinking blocks verbatim. Set
`reasoning_replay:true` (or `"full"`) on an OpenAI-dialect provider whose
endpoint requires preserved thinking in multi-turn tool loops — such as Kimi
for Coding — to replay persisted assistant reasoning as `reasoning_content` on
later requests. Replay is gated on reasoning being enabled for the request, so
compaction summaries and prewarm requests stay clean. With replay enabled, the
dialect tags streamed `reasoning_content` for persistence (as thinking blocks
with no signature); without it the text remains display-only.

On the official `api.anthropic.com` endpoint an unset `reasoning_replay`
defaults to `"current_turn"`: only the in-flight tool chain's thinking is
replayed. Anthropic strips non-final thinking server-side and does not bill old
turns' thinking again, so this saves upload bytes and keeps harness's context
estimates accurate rather than changing billed context. Anthropic-compatible
gateways keep the verbatim default unless they opt in explicitly, and any
explicit `reasoning_replay` value wins on every endpoint.

Opaque reasoning compatibility is configured separately, per model, with
`reasoning_replay_domain`. Without that field, a model and its service-tier
variants get one exact-target domain; switching to any other base model omits
the old provider-owned reasoning from requests while retaining it in the saved
transcript. Models from the same configured provider can opt into replay across
model IDs by declaring the same provider-local label:

```json
{
  "models": [
    {"name": "k3-256k", "reasoning_replay_domain": "k3-family"},
    {"name": "k3", "reasoning_replay_domain": "k3-family"}
  ]
}
```

The model proxy namespaces that label by provider, so matching text in two
different provider configs never authorizes cross-provider replay. Prompt-cache
affinity is independent: documentation that two models share a prompt cache
does not by itself establish that their signed or encrypted reasoning payloads
are interchangeable. If a provider nevertheless returns
`invalid_encrypted_content`, harness disables opaque replay for that domain for
the rest of the current agent session and retries the request once without those
blocks.
`setup` and `refresh-models` preserve hand-configured replay-domain labels.

For the Anthropic dialect, `reasoning_replay:"current_turn"` drops thinking
and redacted-thinking blocks from every assistant message older than the
in-flight tool chain (the last real user turn forward keeps its thinking, as
the protocol requires). The reduction is wire-only: the persisted transcript
keeps every block, so session replay and later full-replay requests are
unaffected. Enable it ONLY where the provider documents Anthropic-style
history dropping — api.anthropic.com ignores and does not bill old-turn
thinking. **Do not enable `current_turn` for kimi-k3 or kimi-k2.7-code**:
Kimi's Preserved Thinking guide makes historical thinking mandatory in
multi-turn tool loops, and Kimi bills it ("`reasoning_content` counts toward
token consumption… historical thinking content keeps occupying the context
window and is billed accordingly"), so there it is a required cost, not
waste. `harness session stats` shows a "reasoning replay" line — block count,
payload bytes, and an estimated token share of the active branch — so the
replay cost is visible before deciding; the cumulative reasoning-token count
stays in the regular usage lines.

Anthropic-dialect provider configs may set `usage_input_includes_cache:true`
when the endpoint reports `input_tokens` as TOTAL input (cached tokens
included) instead of real Anthropic's uncached-only figure — observed on
Anthropic-compatible third-party routes. The dialect then subtracts cache
read/write tokens during usage normalization so `harness session stats`
"uncached input" and cost accounting keep the uncached-input contract instead
of double-counting cache reads. The quirk is explicit config only; harness
does not sniff hosts. Leave it off for api.anthropic.com.

For Anthropic, harness also declares cache semantics directly on each neutral
request. Interactive turns request a one-hour TTL only for stable system/tool
anchors; one-shot, delegate, prewarm, compaction, handoff, and branch-summary
requests use the default five-minute TTL. Message breakpoints always use the
provider's default five-minute TTL. Harness computes a leading message prefix
that future retention cannot rewrite, and the Anthropic dialect places one
message breakpoint there plus a rolling tail breakpoint within the four-anchor
limit. Volatile request-only context (the unresolved TODO reminder, hook output, background
notices) rides a trailing user-role message appended after the breakpoints are
placed — never the system head — so its appearance or change does not
invalidate the cached prefix. Maintenance requests derive deterministic, purpose-separated proxy and
cache IDs from the owning session, so they do not reuse or compete with the main
conversation's connection/continuation chain.

For hand-written model-proxy config shape references, see
`examples/harness-model-proxy/config.json` and
`examples/harness-model-proxy/providers.json`. Setup remains the recommended way
to create real provider allowlists. Manual model entries must declare supported
input modalities with `input_modalities`; use `["text"]` for text-only models
and `["text", "image"]` for models that accept image attachments.

Provider config files written by `setup` and `refresh-models` are managed
(`"managed": true`) and store no per-model prices. The proxy resolves their
prices and missing metadata from authenticated provider catalogs and models.dev,
so cache refreshes update served metadata without re-running setup or restarting.
A fresh, complete provider response controls availability. A failed provider
request preserves the last successful snapshot and configured allowlist. An
auto-detected 404/405 falls back to models.dev availability only until that
endpoint has returned a successful snapshot; later 404/405 responses preserve
the cached snapshot like other transient failures. Background refresh only hides
confirmed-absent targets in memory and never rewrites provider files.
A hand-written config without `"managed": true` is manual: the proxy does not
edit it and uses its own `price` and `input_modalities` entries. A managed config
may set `price_source` to resolve prices from a different models.dev provider
ID; it does not change availability or capability metadata.

`harness-model-proxy` stores every models.dev provider and model, projected to
the metadata harness consumes, at
`~/.config/harness-model-proxy/models.dev.api.json`. `setup` uses the cache
when present; if there is no cache, or the cache cannot be parsed, it fetches and
rewrites the cache before using the vendored fallback snapshot. While serving,
the proxy refreshes this cache when it is older than `24h` by default. Set
`models_dev_cache_ttl` in the proxy config, or pass
`-models-dev-cache-ttl <duration>`, to override the interval; use `0` to disable
periodic serving-time refreshes. Cache updates are parsed and sanity-checked
before replacing the old file; a candidate catalog with no providers/models, or
one whose provider/model counts swing by more than 4x with a meaningful absolute
delta, is rejected and the old cache is preserved. Successful refreshes prune
unused upstream fields before saving the previous cache to
`models.dev.api.json.bak` and replacing the active cache; that single backup is
overwritten each time.

Authenticated provider snapshots are stored with mode `0600` under
`~/.config/harness-model-proxy/provider-models/`. The serving process loads
fresh snapshots as availability authorities and stale snapshots as metadata
only, then refreshes supported authenticated managed providers immediately and
every `provider_models_cache_ttl` (`1h` by default). Set the value to `0` to
disable background provider polling; setup, `refresh-models`, and post-login
refreshes still query providers explicitly. OpenAI-compatible providers use
`/models`; Meta uses its bearer-authenticated `/v1/models` catalog and filters
for Responses-capable model families; OpenRouter, Anthropic, Gemini, and ChatGPT
Codex use their provider-specific authentication, response, and pagination
contracts.

Run `harness-model-proxy refresh-models` to fetch models.dev and query every
supported provider with working credentials before rewriting provider files.
The command reports each catalog stage on stderr, and each provider query has a
30-second deadline so an unresponsive endpoint cannot stall the refresh
indefinitely. A complete provider response is authoritative for the existing
allowlist: it removes configured models that disappeared but never adds newly
discovered models. Authentication, transport, decoding, empty-result, or
incomplete-pagination failures preserve that provider's entire configured
allowlist and emit a warning. Providers without a supported endpoint, explicit
`model_discovery.enabled:false`, and auto-detected 404/405 responses use
models.dev availability. A provider that loses every model is deleted along
with its `provider_configs` reference. Stored API keys, auth blocks, discovery
overrides, and provider quirks are preserved.

### Operations

Serving lifecycle, probes, rolling-update and stickiness guidance,
harness-to-proxy API-key authentication, Prometheus metrics, provider retry
behavior, request tracing, and multimodal compatibility diagnostics are
documented in [proxy.md](proxy.md).

### Usage, pricing, and budgets

The read-only `GET /v1/usage` endpoint aggregates token and cost totals per model
target for the serving process, including delegate child-agent spend. Its
top-level `instance` and `since` fields identify that per-pod lifetime.
`GET /v1/models` includes complete
static pricing schedules, including context-length tiers, plus `source_date`,
`max_age_seconds`, and `expires_at` fields for detecting stale catalog prices.
For mixed sources, `expires_at` is the earliest provider/models.dev expiry;
manual-only setups use the provider config file modification time. Setup and runtime model pickers display
each price band as input/output USD per million tokens.

Cost budgets are attached to model-proxy API keys:

```sh
harness-model-proxy generate-api-key -budget-usd 25 -budget-period 24h laptop
```

New authenticated streams and native compaction requests are rejected with HTTP
429 after the key's recorded known-cost spend reaches its fixed-window limit.
Spend persists under the proxy config directory across restarts. Unpriced targets are allowed by default and do
not count toward the budget; add `-budget-reject-unpriced` to reject them.
`/v1/usage` includes the authenticated key's current budget state when it has a
budget. Both usage and budget enforcement are per pod; use one replica when a
strict global limit is required.

For first-party Google Interactions targets, thought tokens are billed at the
model's output-token rate when the catalog does not provide a separate reasoning
rate. Google Search per-query grounding fees are not included in `cost_usd` or
cost-budget spend; those figures include the request's token charges only.

Responses and Chat Completions report reasoning as a breakdown of their aggregate
output/completion tokens. Harness reports the non-reasoning remainder as output
and the breakdown as reasoning so displayed totals and budgets do not double
count it. When a Responses or Chat catalog price omits a reasoning rate, harness
uses that schedule's output rate; explicit base, context-tier, and service-tier
reasoning rates remain authoritative.

Anthropic Messages reports thinking tokens inside its aggregate output count.
Harness similarly exposes a non-reasoning output remainder plus a disjoint
reasoning bucket and prices reasoning at the output rate when the catalog has no
explicit rate. Anthropic cache writes are split into default 5-minute and
1-hour buckets; the latter uses the configured rate or the documented
`2 × input` fallback. A long-TTL response without the required breakdown is
reported as unpriced instead of using a cheaper estimate. Anthropic hosted
web-search request fees are excluded from `cost_usd` and cost-budget spend; its
token charges remain included.

## REPL Commands

Lines starting with `/` are commands; `//` sends a literal leading slash. At an
interactive TTY prompt, lines starting with `!` run a local shell command and
return to the prompt without contacting the model; `!!` sends a literal leading
`!`. In one-shot mode, initial `-i` prompts, non-TTY/scripted input, pasted text, and edited prompts,
`!text` is literal prompt text. In a normal typed prompt, `$name`, an exact
word-bounded plain skill name, `skill://name`, or `…/SKILL.md` mentions the
named skill anywhere in the text. Harness reads that skill's complete
`SKILL.md`, wraps it in a Codex-style `<skill>` envelope, and prepends it to the
user prompt. That enriched prompt is persisted as one ordinary user-role
transcript message; the body is not request-only context and is not reinjected
on later requests. A failed read aborts before model work. Re-mention a skill to
inject its current contents again on a later turn. If the model selects a skill
implicitly, its `read` of `SKILL.md` remains an ordinary persisted tool result;
Harness does not replace it with a receipt or pin the body. `$$` escapes a
literal `$`.

In terminals that support bracketed paste, pasted text fills the normal prompt
or active-turn steering input for review and is submitted as one literal message
when you press Enter, preserving embedded newlines. Each paste event is
classified independently after newline
normalization: a range of at most 1,000 normalized UTF-8 bytes renders inline,
including multiline content, while a range over 1,000 bytes shows a one-line
`[N bytes of pasted content]` placeholder wherever it occurs in the prompt. The
full content is retained and submitted in either case. A collapsed placeholder
persists while you edit surrounding text and acts as one unit for cursor movement
and deletion. Press Ctrl-G / `/edit` to open the external editor with the full
expanded content; text returned from the editor remains expanded in the prompt.
A paste that fills an empty prompt is submitted literally — pasted `/commands`
are not executed, `!command` is not a shell escape, and `$skill` is not resolved.
This holds on the Enter path in every edit mode, including the vi normal-mode
Enter after Esc. Typing anything after a paste (in emacs mode, or after entering
vi normal mode with Esc) makes the whole line typed (so `!`/`/`/`$` apply).
Bracketed paste is classified once its explicit end marker arrives. In terminals
that do not support bracketed paste, harness falls back to detecting a fast paste
burst so newlines in a paste do not submit prematurely; that incremental fallback
may transition a range from inline to collapsed when it crosses 1,000 bytes. Set
`HARNESS_REPL_PASTE_HEURISTIC=off` to disable the fallback. For non-interactive
large input, prefer `-p -` or piped stdin.

At an interactive terminal, the prompt supports basic line editing. Shift-Enter
inserts a newline without submitting. Press Ctrl-G at the prompt, or run
`/edit [draft]`, to open an external editor for a multi-line prompt. Harness
uses `$VISUAL`, then `$EDITOR`, then `vi`. In normal prompt text, Tab completes
literal `@path` file references; paths containing whitespace, quotes, or
backslashes are inserted as `@"..."`. These references remain prompt text and do
not expand file contents. Supported image references auto-attach as images using
the default image detail when the model supports images. Tab also completes
`$skill` mentions against the discovered skill set: a unique match completes
inline with a trailing space, multiple matches extend to their common prefix or
list candidates below the prompt, and no match leaves the buffer unchanged.
Skill completion respects the `$$` escape and, like `@` completion, is skipped
on `!` shell and `/` command lines but works on escaped `!!` and `//` prompt
lines. On `!` command lines,
Tab completes the first word from `PATH` and completes path words with `/`, `~/`,
`./`, `../`, and nested relative path prefixes.

At the idle main interactive TTY prompt only, Shift-Tab cycles the configured
agents by canonical sorted name, wrapping at the end, without changing the
editable draft. It works in emacs mode and in both vi insert and normal modes;
ordinary Tab keeps the completion behavior above.

The editor preamble contains only the latest completed conversational turn from
the replay log (assistant output, that turn's tool activity/notices, and its
`[turn: …]` completion line). It excludes the user prompt, earlier turns, attempt
accounting, maintenance calls, and the aggregate `[prompt: …]` usage line.

| command | effect |
|---|---|
| `/help` | list commands |
| `/exit`, `/quit` | save, print a session token summary, and exit |
| `/clear` | echo the discarded session token/cost totals, then reset the conversation and rotate to a fresh session directory |
| `/continue` | after the latest prompt in this live REPL ends with an API error, retry model work from the current transcript boundary under a fresh prompt/accounting ID without adding a user message |
| `/compact` | force compaction now |
| `/tree [entry]` | browse the conversation tree and branch in place; selecting a user prompt rewinds to its parent and pre-fills that prompt for editing |
| `/fork [entry]` | select a prior user prompt and branch before it into a new session with fresh usage accounting |
| `/clone` | copy the current branch into a new session with fresh usage accounting |
| `/context` | dump the current provider-neutral model context as JSON |
| `/context <file>` | save the current provider-neutral model context as JSON |
| `/prompt` | show the full system prompt currently sent to the model, including environment, AGENTS.md, skills, runtime capability hints, and active-agent instructions |
| `/usage` | cumulative input, cached input and token-weighted cache-read ratio, output, reasoning tokens, cost, and successful compactions |
| `/evidence` | list the newest bounded session evidence metadata without contacting the model or reading artifact bodies |
| `/evidence list [--kind evaluator\|tool] [--status STATUS] [--prompt N] [--limit N]` | filter evaluator results, archived/truncated tool outputs, and tool errors by metadata |
| `/evidence show <id>` | inspect one catalog record, including its current local artifact status and resolved path when safe |
| `/max-turns` | show the current per-prompt turn limit |
| `/max-turns <n>` | change the turn limit for subsequent prompts in this REPL session; `n <= 0` means unlimited |
| `/tools` | list enabled built-in and MCP tools with descriptions, plus disabled optional tools |
| `/tools --raw` | dump the active model-facing tool definitions as provider-neutral JSON, including normalized input schemas and any provider-hosted `server_tools`; `/tools raw` is also accepted |
| `/lsp [status\|enable\|disable]` | inspect missing commands and idle, initializing, ready, or failed language-server roots (including backoff errors), or toggle native LSP tools for this REPL session |
| `/image` | list images queued for the next prompt |
| `/image <path>` | attach an image to the next prompt |
| `/image --detail <level> <path>` | attach an image with per-image detail |
| `/image --clear` | clear queued images |
| `/edit [draft]` | open an external editor for the next prompt |
| `/save [file]` | force save, optionally elsewhere |
| `/model` | choose a configured provider/model; the picker shows input/output USD per 1M tokens for every static context-price band; interactive runs can optionally save it as the default |
| `/model <id>` | switch subsequent turns to model `<id>`; a near-miss falls back to a unique prefix/substring match |
| `/model <provider>:<id>` | switch to `<id>` on a specific configured provider |
| `/reasoning` | list reasoning controls for the current model |
| `/reasoning <profile>` | switch reasoning profile for subsequent turns |
| `/reasoning summary <auto\|concise\|detailed\|none>` | switch Responses API reasoning summaries for subsequent turns |
| `/effort [profile]` | alias for `/reasoning [profile]` |
| `/fast [on\|off\|status]` | toggle or inspect the current model's `:fast` sibling; reports unavailable when the model has none |
| `/agent` | list agents and descriptions, marking the current one |
| `/agent <name>` | switch the active agent |
| `/mode`, `/mode <name>` | alias for `/agent` |
| `/plan` | alias for `/agent plan` |
| `/auto` | alias for `/agent auto` |
| `/handoff [-a agent] [-m model] [message]` | review the latest recorded plan and supplementary context, then after approval switch to an implementation agent, apply optional agent/model overrides and user guidance, and start the implementation turn |
| `/background` | list background jobs |
| `/background <id>` | show a background job's status, result, and transcript path |
| `/background cancel <id>` | cancel a running background job |
| `/goal` | show the current goal and status |
| `/goal <text>` | set or replace the goal and immediately start working on it; only exact `clear`, `pause`, and `resume` arguments are subcommands |
| `/goal clear` | remove the current goal |
| `/goal pause` | pause autonomous continuation for the active goal |
| `/goal resume` | reactivate a paused goal with a fresh continuation count, then immediately submit a continuation prompt |
| `/skills` | list available skills |
| `/vi on\|off` | enable or disable vi-style prompt editing (persisted as the default) |
| `!command` | run a local shell command at an interactive TTY prompt |

`/continue` is process-local recovery for the most recent non-cancelled model API
failure. It reuses the valid current transcript and the failed prompt's
request-only hook context, while dynamic TODO, goal, and background context is
regenerated normally for each request. The command does not submit its literal
text, add a user-role message, invoke `UserPromptSubmit`, resolve skills, or
consume pending images. A repeated API failure leaves it available again. A
successful run, cancellation, non-API failure, newer ordinary or host-created
model prompt, `/clear`, or conversation branch invalidates it. Model, agent,
and reasoning switches may be made first and apply to the retry. Eligibility is
not restored after process restart or `-resume`.

Anthropic extended thinking and 1-hour prompt-cache writes appear as separate
reasoning and `cache write (1h)` usage buckets. They remain disjoint from output
and default-rate cache writes in session totals and token budgets. Cache-read
percentages are token-weighted after aggregation: cache reads divided by
uncached input plus cache reads plus both cache-write buckets. Cache writes are
therefore misses for this metric.

An unknown `/command` prints a `did you mean <command>?` suggestion (nearest known
command by edit distance) instead of failing silently. The per-prompt usage line
appends cache-read and reasoning token counts (with the cache-read ratio) when they
are non-zero, conditionally places compaction counters after context usage, and a
model with no configured price prints a one-time
`[note: no price configured …]` notice instead of silently dropping cost.

### Waiting and typing during a prompt

At an interactive terminal harness shows a live wait indicator while a model
request or a tool call is outstanding: a single in-place line such as
`[turn: 1 · 12s · ctx 30% 60.0k/200.0k │ prompt 18s]` (or
`[tool: grep · 3s]`), updated about once a second. The context field includes
both the percentage and compact used/window token counts. Turn numbers restart
at 1 for each prompt. When a turn completes, its closing line adds the turn's
dollar cost (e.g. `[turn: 1 · 12s · $0.032 · ctx 30% 60.0k/200.0k │ prompt 18s]`)
once the model stream has closed and the final token totals are known; the
cost is omitted for models with no configured price.
It is erased the instant real output or a tool line appears, and is shown only at a
TTY when not quiet.

By default, successful built-in reads in an interactive session appear as one
`[read] path=…` line per unique effective path in each conversational turn;
repeated offset/limit windows of the same file are deduplicated. Read errors and
centrally truncated results stay detailed, while `-v` restores detailed summaries
for every read call. `-q` suppresses these tool/status lines entirely. Session
recording and ordinary replay remain fully detailed.

Text typed while a prompt is running is captured with echo off and shown on that
wait line after a `>` marker. Unsubmitted text is deposited into the next prompt
as editable, pre-filled text when the active prompt completes or is interrupted.
Ctrl-C and double-Esc cancel the active prompt.

### Steering while a prompt runs

By default, model-bound input submitted with Enter while a prompt is running is
injected as a user message before the next model request. This lets you redirect
the current work without canceling and retyping the prompt. The active-turn input
uses the normal prompt editor's editing and paste behavior, so a multiline paste
fills one buffer and one Enter submits one steer; Ctrl-C and double-Esc remain
active-turn cancellation gestures.

- `!shell`, `/commands`, and `/edit` retain their queued behavior and run at the
  next idle prompt.
- A steer does not consume a `max-turns` slot and resets the loop-guard streaks.
- Steering is attempted only while no earlier submitted input is still queued,
  so queued and recovered input always runs in submission order.
- If the prompt finishes before another model request, the submitted steer is
  recovered and run as the next prompt.
- At a TTY, submission prints a dim `[steer queued: …]` or
  `[queued for next prompt: …]` line. The first means steering was accepted for
  the active prompt, not that it has reached the model: a prompt that ends before
  its next model request still recovers that input as the next prompt. When the
  agent actually injects an accepted steer, it prints `[steer sent: …]`; queued
  input is instead echoed in the normal prompt line when its own prompt starts.
- An accepted steer also releases any blocked `background_jobs` wait from that
  prompt. The selected jobs keep running; once its completion or original timeout
  is available, an interactive mode starts a host-created continuation (after
  already-delivered input, drafts, approvals, EOF, shutdown, and interrupts) with
  the aggregate result in request-only context.
- Ctrl-C or double-Esc still cancels the active prompt.

`-no-steer`, `HARNESS_NO_STEER`, or config `no_steer` disables steering and
queues submitted input as the next prompt. Steering is available in the TTY REPL
and interactive JSON mode, not one-shot mode.

### Session goals

`/goal <text>` turns an interactive session into an autonomous continuation
loop. Harness starts a visible continuation prompt containing the objective,
then, after each completed prompt, submits another whenever the goal is still
active and no user input is already queued. The continuation keeps the complete
objective, requires evidence-based progress and a requirement-by-requirement
completion audit, and asks the model to explain concrete blockers and report
clear evidence when the objective is achieved.

Goals are created and controlled exclusively through the `/goal` command; they
are not exposed as model-callable tools. Goal state is independent of plans and
TODOs. While active, the controller is regenerated as request-only context on
every model round, so it remains salient after compaction without duplicating
the reminder in the transcript. The loop remains active until the user pauses
or clears it, a prompt returns `context.Canceled` from user interruption
(including cancellation during pre-prompt tool refresh or a submission hook), a
continuation prompt is rejected by a submission hook, or the continuation cap is
reached. Errors and deadline expiry do not pause a goal. Rejected continuation
prompts pause without consuming the cap. The cap defaults to 25 and is configured
with `-goal-max-continuations`, `HARNESS_GOAL_MAX_CONTINUATIONS`, or
`goal_max_continuations`; `0` means unlimited. `/goal resume` reactivates a
paused goal with a fresh continuation count. It rejects an already-active goal rather
than resetting the safety count. If a goal changes or becomes inactive while a
rendered prompt is waiting on tool refresh or submission hooks, Harness skips
that stale prompt without consuming the count or overwriting the newer state.

Goal state is saved in `state.json`, with idle command and safety-limit
transitions checkpointed immediately. It is restored by `-resume`, copied by
`/clone`, and removed by `/clear`. A restored active goal continues at the first
idle boundary regardless of the selected agent's tool set. The autonomous driver
and `/goal` command are interactive-REPL-only; one-shot and piped runs expose no
goal controls and run no continuation loop. No model-facing goal tools, token
budget, or interactive goal menu are provided.

## Agents

An agent definition bundles a set of allowed tools with extra system-prompt
instructions and an optional model target override. Root interactive sessions
can start with or switch to agents whose `interactive_selectable` setting is
true, using `-agent <name>`, `HARNESS_AGENT`, config `agent`, or `/agent <name>`.
The setting defaults to true when omitted. It does not restrict one-shot runs,
delegation or child continuation, and plan handoffs may still target hidden
implementation agents.

Shift-Tab switches use the same full agent runtime selection as `/agent` and emit
the existing `[agent switched: <name>]` notice and provider/model line.
For targets that advertise zero-generation prewarm support, all switch-driven
prewarms — Shift-Tab cycling, explicit `/agent`, handoff, `/model`, and startup
agent resolution — use a 500ms idle debounce, so rapid cycles or resolution
bursts warm only the final settled selection. Prewarming is not suppressed
merely because the model ID is unchanged: system/tool prefixes and response
continuation differ by agent. A warm-up requested while a prompt is running is
deferred and fired once when the prompt completes (including interrupt/error
exits): a running prompt refreshes the cache prefix every turn, so a mid-prompt
prewarm would be wasted. Standalone `/compact` keeps immediate prewarming;
submitting a real prompt cancels any pending delayed warmup. Harness does not
issue speculative generated completions to prewarm other providers.

Five agents are built in. `auto` is the default. `auto` and `plan`
are interactive-selectable by default; the other built-ins remain available
for one-shot runs and delegation.

| agent | interactive | tools | behavior |
|---|---|---|---|
| `auto` | yes | `read`, `view_image`, `edit`, `write`, `shell`, `web_fetch`, discovered MCP tools, `update_todos`, `record_plan`, `delegate`, and background job tools | the default general-purpose behavior; the model decides what to do |
| `explore` | no | `read`, `view_image`, `shell`, `web_fetch`, `update_todos`, and read-only MCP tools; no mutation, background, handoff, or delegate tools | broad search, architecture/dependency tracing, root-cause investigation, and questions spanning many files; not a known-file lookup |
| `plan` | yes | `read`, `view_image`, `shell`, `web_fetch`, read-only MCP tools, `update_todos`, `record_plan`, `delegate`, and `background_jobs` | collaborate on a self-contained implementation plan without modifying the project |
| `review` | no | the same read-only local and MCP surface as `explore` | findings-first review of a concrete change; if no range is supplied, inspect the working-tree diff and untracked files |
| `independent` | no | the same local tools as `auto`, plus discovered MCP tools | complete the task end-to-end without pausing for input |

Define new agents or override built-ins in the config file under `agents`.
**Breaking configuration rule:** every new custom agent must have a nonblank
`description` that tells the parent *when to use it*. Startup, `--agents`, `harness config show`, and `harness config check` fail
when this selection metadata is missing or whitespace-only;
there is no generated fallback. An override of `auto`, `explore`, `plan`,
`review`, or `independent` may omit `description` and inherit the built-in
value. Other fields continue to merge onto a built-in of the same name:

```json
{
  "agent": "plan",
  "agents": {
    "plan": { "prompt": "@~/.config/harness/plan-prompt.md" },
    "security_review": {
      "description": "Use after implementation for an independent review of concrete security issues.",
      "allowed_tools": ["read", "shell"],
      "mcp_tools": "read_only",
      "workspace_access": "read_only",
      "interactive_selectable": false,
      "model": "anthropic:claude-opus-4-8",
      "prompt": "Review the diff and surrounding code for security issues. Report only concrete findings."
    }
  }
}
```

An agent without `model` inherits the current session target. When set, `model`
is a complete `<provider>:<model>` model-proxy target ID. New custom agents
default to `interactive_selectable: true`; set it to false for agents intended
only for one-shot execution or delegation. Built-in overrides inherit each
built-in's explicit value unless the field is set.

Each agent can set `mcp_tools` to control automatic exposure of discovered MCP
tools: `disabled`, `read_only`, or `all`. Explicit `mcp__...` names in
`allowed_tools` still work as a strict whitelist. Tool gating is the one place
the harness restricts tools; the underlying tools still assume an external
sandbox for real isolation.

`workspace_access` controls the default background-delegate lease:
`read_only` permits concurrent children on one scope, while `exclusive`
conflicts with every active lease for that scope. Built-in `explore`, `plan`,
and `review` use `read_only`; `auto`, `independent`, and new custom agents
default to `exclusive`. Implementation-mode delegates are always exclusive.

### Planning and implementation handoff

The `plan` agent investigates and designs without modifying the project. It uses
`record_plan` to write a self-contained immutable Markdown artifact under the
session. Implementation is a user command: `/handoff` reviews the latest
recorded plan and, after approval, switches agents and starts implementation
with a clean context seeded by that complete plan. Delegated plan agents have
private plan stores; `/handoff` is interactive-root only.

`/handoff [-a agent] [-m model] [message]` reviews the latest recorded plan.
`-a` overrides the configured target agent, `-m` applies a one-off model
override, and trailing text is added as separate user guidance. Options must
precede the message; use `--` when the message itself begins with a dash. The
target otherwise comes from `--handoff-agent`, `HARNESS_HANDOFF_AGENT`, config
`handoff_agent`, or the `auto` default. Handoffs require an interactive TTY
session and are unavailable in one-shot and JSON modes.

All built-in agents use `update_todos` for an advisory checklist. Each call
replaces the complete `{step,status}` list, and the statuses never complete,
block, or otherwise control the agent loop. The unresolved list is restored on
resume and injected once after compaction or another transcript rewrite so the
model can reconcile it with current progress. Without another successful
update, Harness also reminds the model after 12 conversational rounds, then
backs off to intervals of 24, 48, and at most 96 rounds. Updates and delivered
recovery reminders reset the cadence; request retries do not advance it.

## Sessions

- A session path is a directory: `tree.ndjson` is the canonical append-only
  conversation tree, `state.json` the compact mutable state, `active-turn.json`
  a transient atomic recovery record, and `raw.ndjson` the chronological replay
  log; `compactions/`, `children/`, and `artifacts/tool-results/` hold archived
  messages, child-agent transcripts, and full truncated tool outputs. The
  storage invariants and save/recovery ordering are documented in
  [session.md](session.md).
- Auto-save uses `~/.local/state/harness/sessions/<timestamp>`, honoring
  `$XDG_STATE_HOME`.
- Harness checkpoints root and child runs before provider requests, before tool
  dispatch, and after each validated closed turn. A crash during tool execution
  recovers that open call as an explicit `interrupted` error instead of
  automatically executing it again.
- `-session <dir>` chooses an explicit session directory. Active sessions hold a
  non-blocking OS lock on `session.lock`; starting another harness with
  `-resume <dir>` while that session is active fails without loading or
  modifying it. `-resume <dir>` otherwise loads its active tree path, latest
  plan, and TODO list, then continues, applying a newer active-turn recovery
  record when present. Resume prints a bounded recap of the last exchange to
  stderr before the first prompt, with an explicit trailer when the prior
  session ended mid-turn. Child runs still marked `running` from the prior
  process become `abandoned`; their durable checkpoint remains eligible for
  compatible child-ID continuation. Combining distinct `-resume <source>` and
  `-session <destination>` clones the active branch into the destination with
  fresh usage accounting. `/clear` rotates to a fresh directory.
- `/tree` opens a searchable, paged line picker over safe tree nodes. Selecting
  a human prompt branches from its parent and returns the prompt (including
  images) to the editor; selecting another node makes that node the branch
  point. Before moving, harness asks whether to attach no summary, a default
  summary, or a summary with custom focus; a failed summary leaves the branch
  unchanged.
- `/fork` presents the same compact fork graph filtered to prior human prompts,
  then performs the selected move in a new session. `/clone` copies the current
  branch into a new session. Both record a parent-session link, reset
  prompt/usage accounting and remote continuation anchors, and preserve the
  current model, agent, reasoning settings, latest plan, and advisory TODO list.
- Tree navigation changes only model-visible conversation context. It never
  rewinds the working directory or Git; every new branch carries an internal
  warning telling the model to inspect current files before assuming their state.
- Transcripts are provider-neutral, so a session started against Anthropic can
  resume against an OpenAI-compatible server and vice versa.

Inspect saved sessions with:

```sh
harness session ls [-a|--all] [-l|--long]
harness session resume [root-options] [--] [session-dir]
harness session replay [-f|--follow] [-q|--quiet] [--color-theme dark|light] [--config path] ~/.local/state/harness/sessions/20260611T123456Z
harness session timings ~/.local/state/harness/sessions/20260611T123456Z
harness session stats [--format text|json] ~/.local/state/harness/sessions/20260611T123456Z
harness session evidence [--kind evaluator|tool] [--status STATUS] [--prompt N] [--limit N] [--format text|json] <session-dir> [record-id]
harness session analyze [--since D|--all] [--before RFC3339] [--format text|json] [--] [dir]
harness session errors [--tool T] [--kind K] [--model M] [--agent A] [--since D|--all] [--before RFC3339] [--format text|json] [dir]
```

`session timings`, `session stats`, `session errors`, and `session analyze`
accept a bare session ID such as `20260813T115922Z` in place of the session
directory; it resolves under the default sessions root, so the commands work
from any working directory. An existing directory path keeps working unchanged,
and an unknown ID fails with `unknown session <id> (expected a session
directory or timestamp ID)`.

`session ls` lists recorded root sessions newest first. By default it includes
only sessions whose persisted startup working directory matches the current
working directory. `-a`/`--all` removes that working-directory filter, but still
indexes only immediate sessions under the default
`$XDG_STATE_HOME/harness/sessions` root; arbitrary directories previously chosen
with root `-session` are not indexed. The short form prints one full session path
per line. `-l`/`--long` also prints the point-in-time `active`, `inactive`, or
`unknown` kernel-lock status, RFC3339 start and end columns, and a bounded initial
prompt preview. Prompt previews collapse whitespace to one line and clip
rune-safely. `START` is the persisted creation time. For an inactive or unknown
session, `END` is the latest durable state save and is only an approximation, not
proof of a clean exit; active sessions show `-`. The activity check does not
create or rewrite `session.lock`, and the lock acquired by resume remains the
authoritative race-safe decision.

`session resume <session-dir>` forwards the source path, including paths outside
the default root, through the normal root resume flow. With no path, it opens a
searchable, paged picker over the same-working-directory sessions. Each entry
shows its prompt first, followed by an aligned start/end/session detail row on
stderr, leaving stdout available for resumed text or JSON output. Sessions saved
with an unsupported schema version are skipped silently; malformed or unreadable
sessions still produce warnings. Active sessions remain selectable, but may then
fail the normal final resume lock. Piped stdin requires an explicit source path so
the picker never consumes prompt or NDJSON input. Existing root options are
accepted before the optional source path; Go flag parsing stops at the first
positional argument, so use `--` for a source path beginning with `-`. A distinct
forwarded `-session <destination>` retains the existing clone behavior: resume the
selected source branch into the destination with fresh usage accounting.

`session evidence` derives a read-only metadata catalog from one session's
canonical `raw.ndjson` and current local artifact metadata. Artifact status is
a point-in-time report about current filesystem metadata, not proof of the
historical byte identity of a file. The equivalent interactive commands are
`/evidence`, `/evidence list ...`, and `/evidence show <id>`; the catalog never
contacts the model or reads artifact bodies. Catalog version 1 includes every
typed evaluator result, every tool error, and every truncated tool result that
declares a full-output artifact. Stable chronological IDs use `eval-NNNNNN` and
`tool-NNNNNN`; lists are newest first, return 20 records by default, and cap
`--limit` at 100. Supplying a record ID prints that record in text mode; JSON
always emits the versioned page envelope.

Record status vocabulary: `available`, `stale` (modified after its event),
`missing`, `unreadable`,
`unsafe`, `external`, `unreferenced`, and `recorded`. Relative evaluator
references resolve only beneath the session's persisted startup working
directory; tool artifacts must stay beneath the session directory. Run the
command against a child session path to inspect that child's stream; the first
catalog version does not merge root and delegate streams. The catalog internals
are documented in
[trajectory.md](trajectory.md#human-only-evidence-catalog-internalsession).

`session replay --follow` first renders the existing complete `raw.ndjson`
records, then renders complete records as they are appended, using the same
user-facing view as ordinary replay; `-q`/`--quiet` suppresses status lines but
keeps prompts and assistant text. Replay resolves `--color-theme`,
`HARNESS_COLOR_THEME`, and `color_theme` with the normal flag > environment >
file > dark-default precedence and requires no model/provider configuration; no
theme or ANSI metadata is persisted in the event log. A followed child exits
after its terminal metadata is observed; a followed root session has no
terminal marker and continues until interrupted. A missing directory is an
immediate error. Replay rendering, coalescing, and terminal-marker internals
are documented in [session.md](session.md).

`session timings` labels a prompt without a terminal `prompt_usage` event as
`in progress` and measures its elapsed time through the latest recorded event.

`session stats` prints a deterministic, human-readable report for one session:
build/runtime attribution, turns, tree shape, direct and delegate tool/command
activity, result size/truncation totals, normalized repeated-call aggregates
with arguments redacted, active-context composition and the latest request
estimate, compactions, retention epochs, termination reasons, an `Errors`
section, and a hierarchical delegate breakdown. Root token/cost totals already
include delegate and compaction usage. `--format json` emits a versioned,
transcript-free machine report. The full field inventory and accounting rules
are documented in [session.md](session.md).

`session analyze` emits a deterministic, versioned, transcript-free report for
one session or a directory containing session roots. When `dir` is omitted,
`--since D` controls recent session-root discovery (default `24h`) and `--all`
removes that lookback; those discovery flags are mutually exclusive and do not
apply to an explicit directory. Discovery recursively
includes `children/<id>/` streams and never follows symlinks. A stream is
capped at a 256 MiB snapshot prefix, 16 MiB per record, and 500,000 records;
hitting a cap sets `limit_exceeded` and excludes that partial hierarchy from
promotion distributions. `--before` applies an inclusive event-time cutoff.
`--format json` is the stable analyzer input for corpus comparisons (used by
`scripts/reliabilitybench`).

Reliability fields carry explicit availability: a supported signal with no
occurrences is an observed zero; a legacy or missing stream is unavailable.
The `trajectory` telemetry section reports the host-only evaluator-lane
streaks and classifications behind the default-on `stagnation_nudge` policy
(see [trajectory.md](trajectory.md)); candidate IDs, evidence references, and
filesystem paths are not included in analyzer output. Mutation-path counts are
possible paths attributed to successful mutation-reporting tool calls, not
proven filesystem deltas. The full analyzer vocabulary — usage/storage
accounting, hierarchy/cohort summaries, completion
counters, and availability semantics — is documented in
[session.md](session.md).

`session errors` lists classified failures — every failed tool result and
failed model request in one session (root plus delegate children) — one row per
failure with agent, model, prompt/turn, context percentage, tool, error kind,
and a bounded excerpt. Without a directory it scans sessions under the default
sessions root created within `--since` (default `24h`; `--all` disables the
window). `--tool`, `--kind`, `--model`, and `--agent` filter rows, `--before`
applies an event-time cutoff, and `--format json` emits the scope, summary, and
rows as JSON. Error kinds come from the structured `error_kind` field on new
logs and from text classification on legacy logs; see the design document's
tool failure handling section (§8.2) for the kind vocabulary and
[session.md](session.md) for the report internals.

### Session diagnostics

`diagnostics.ndjson` contains JSON-line diagnostics, including MCP and LSP child
process stderr that is hidden from the terminal by default. `raw.ndjson` is the
chronological replay and analysis log. In addition to visible conversation
events, it records model-request acceptance and completion, every failed
upstream attempt, scheduled retries, terminal failures, cancellation, and
retention epochs. Retention records include the trigger, reclaimed blocks/bytes,
context estimates, continuation reset, and next-request shape; they never enter
model context. Tool-result records may carry aggregate integer `result_metrics`,
and failed results carry a structured `error_kind` plus a bounded, rune-safe
`error_excerpt` (2 lines / 240 runes) for error analysis. Truncated tool results
additionally carry their deterministic session-relative `artifact_ref`; the
reference is expected metadata, while `session evidence` verifies whether the
file is currently available. New skill activation records carry only the
`explicit` source and `injected` summary. The activation record itself includes
no skill body and adds no model-visible content; the
Codex-style `<skill>` block is already part of the persisted user event.
New tool start/result records also snapshot `model_target`, `provider`,
`api_type`, and `model`, making attribution stable if a resumed session later
switches models.

Model-request lifecycle records carry parsed provider messages, timing, and
request correlation used by `harness session timings`. They never become
conversation-tree entries or model context. The model proxy logs the same
failed upstream attempts individually. Failed provider JSON is also stored as
`response_payload` on the lifecycle record in `raw.ndjson` and as
`api_response_payload` in both the session and model-proxy diagnostic logs.
These payloads are capped at 16 KiB and recursively redact common prompt,
generated-content, reasoning, tool-argument, credential, and binary fields.
Image-bearing requests omit them; malformed non-JSON responses retain only
their byte length and SHA-256. OpenRouter `X-Generation-Id` values appear as
`upstream_request_id` when available. Multimodal endpoint rejections add the
sanitized records described in
[Multimodal tool-result compatibility diagnostics](proxy.md#multimodal-tool-result-compatibility-diagnostics);
prompts, tool arguments, local paths, result text, and image base64 remain
excluded.

## Compaction

Compaction fires when `max(reported input tokens, estimated full-request
footprint)` reaches `compact_trigger_percent` (default 78) of the effective
model context window, or on `/compact`. `compact_auto_enabled: false` disables
only threshold-based compaction; `/compact` and provider-overflow recovery
still work. Harness compacts toward `compact_target_percent` (default 65)
after fixed system/tool overhead.

When the active target advertises native Responses compaction, automatic
compaction and an unfocused `/compact` use that provider operation; the semantic
transcript is retained, so switching provider or model simply replays normal
history. Otherwise — and always for focused, delegate-continuation, and idle
compaction — Harness replaces removed history with a summary checkpoint while
keeping the newest completed whole turns (`compact_keep_tokens` /
`compact_keep_turns`, default 8 turns; an unset token budget adapts to the
window as `clamp(window/5, 4000, 20000)`). If the summary fails or times out,
a sparse deterministic checkpoint takes over once the exact removed transcript
has been archived; if archiving fails, the full transcript is kept. Archive
and checkpoint metadata identify model-generated versus deterministic
summaries. The full algorithm, live retention passes, and fallback rules are
documented in [compaction.md](compaction.md).

Use `/compact optional focus text` to emphasize one manual summary. Focus is
trimmed, recorded in hook/archive/tree metadata, and applies only to that
successful compaction.

Interactive idle compaction is an opt-in experiment. Set
`compact_idle_after_seconds` above zero (default `0`, disabled) to prepare a
summary after that much REPL idle time when the estimated full context has
reached `compact_idle_trigger_percent` (default `35`; it must be lower than
the normal trigger). Submitted input cancels the work and starts immediately;
a late result is discarded. Sessions with `PreCompact` or `PostCompact` hooks
are ineligible because hook side effects cannot safely run on a candidate.
`compact_auto_enabled: false` also disables idle preparation. `session stats`
reports attempt outcomes, duration, and applied context reduction.

`compact_timeout_seconds` bounds the complete model-backed phase across all
chunks and retries (default `300`). `compact_tool_result_max_bytes` (default
`4096`) bounds old tool-result previews. If the active context, request
payload, or tool schemas are large enough to likely slow response startup,
harness prints one warning per prompt to stderr.

## Interrupts

- Ctrl-C during a prompt, or Esc twice in short succession during a REPL prompt,
  cancels the prompt. It aborts the HTTP stream, kills any `shell` process
  group, keeps streamed partial text, strips unexecuted tool calls, prints
  `[cancelled]`, and returns to the prompt. Any text typed during the prompt is
  preserved and deposited into the next prompt as editable pre-filled text.
- A second Ctrl-C within about one second, or Ctrl-C at the idle prompt, saves,
  prints the session token summary, and exits 130.
- Ctrl-D at the prompt saves, prints the summary, and exits 0.
- Ctrl-C during startup (including `SessionStart` hooks) or helper-command
  network work cancels the in-flight operation and exits 130 instead of waiting
  for its timeout. This includes `session replay --follow`, where it ends an
  otherwise open-ended root follow.

## Hooks

Harness supports command hooks for `SessionStart`, `UserPromptSubmit`,
`PreToolUse`, `PostToolUse`, `PreCompact`, `PostCompact`, and `Stop`.
`PreCompact` and `PostCompact` receive `trigger`; focused manual compactions also
receive the one-shot `focus` field.

```json
{
  "hook_configs": ["hooks_config.json"],
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "shell|edit",
        "hooks": [
          {
            "type": "command",
            "command": "./hooks/pre-tool.sh",
            "timeout_seconds": 30,
            "max_consecutive_timeouts": 3,
            "timeout_cooldown_seconds": 60,
            "status_message": "Checking tool call"
          }
        ]
      }
    ]
  }
}
```

Each command runs in the harness cwd with a JSON event payload on stdin. Every
payload carries common fields such as `session_id`, `transcript_path`, `cwd`,
`hook_event_name`, `model`, and `permission_mode`, plus per-event fields. `Stop`
fires once before control returns from every admitted prompt, including canceled
prompts. At a normal model stop its `can_block` field is `true` only when the
turn, token, and cost budgets permit another request; a blocking result then
requests one more model turn. Provider failures, cancellation, exhausted hard
budgets, and other terminal paths that cannot continue emit `Stop` with
`can_block:false`, and blocking output is ignored. A canceled prompt runs this
terminal notification in a detached context bounded to ten seconds. This lets
integrations observe abnormal prompt exits as well as normal model completion;
`can_block` distinguishes a completion guard that may continue from a terminal
notification that cannot.

`matcher` is a Go regexp over the tool name for tool hooks, `manual|auto` for
compaction hooks, and `startup|resume|clear|fork|clone` for `SessionStart`. Omitted, empty,
or `*` matches all. Hook commands may block with exit code `2` or JSON stdout
such as `{"decision":"block","reason":"..."}` / `{"continue":false}`. Plain
stdout is added as hook context only when the command exits `0`.

A `Stop` hook may additionally emit a typed semantic evaluator result:

```json
{
  "accepted": false,
  "score": 0.873,
  "score_direction": "maximize",
  "candidate": "sha256:abc123",
  "remaining_requirements": 2,
  "evidence_ref": "artifacts/verify.log",
  "reason": "Two acceptance checks still fail."
}
```

`accepted` is required whenever any evaluator field is present. `false` is a
blocking result, subject to the same `can_block` budget check and one-turn Stop
recursion guard as `decision:block`; its bounded fields and optional `reason`
are included in corrective context. `true` records acceptance without forcing
another turn and, when supplied, requires `remaining_requirements:0`. The
optional scalar `score` is observational by default: Harness preserves it
(including a numeric zero) but does not assume that higher or lower is better.
Optional `score_direction` may be `maximize` or `minimize`, requires `score`,
and gives only the host-owned stagnation projection an ordering rule. It is not
added to corrective model context. `candidate`
and `evidence_ref` are optional, non-empty single-line identifiers capped at
256 and 1024 bytes respectively; `remaining_requirements` is optional and must
be between 0 and 1,000,000.

Semantic evaluator fields are accepted only from `Stop` hooks that exit 0 or 2.
Invalid semantic fields are ignored; an otherwise successful exit-0 command is
recorded with a `parse_failed` hook diagnostic, while exit 2 retains its normal
`exit_nonzero` process outcome. Legacy `decision`, `continue`, exit-code, and
textual-reason behavior from the same output is still honored. Each valid result is recorded as a displayless
`evaluator_result` event in `raw.ndjson`, containing only the bounded semantic
fields and handler identity—not free-form reason, stdout, or stderr. In the
absence of an explicit orchestrator workflow status, all accepted evaluator
results project to `complete` and any rejection projects to `in_progress`.
`remaining_requirements` is projected when exactly one evaluator supplied it;
independent handlers have no generic count-aggregation rule. A separate legacy
hook block that has no rejecting semantic result leaves workflow status
unavailable rather than reporting a false completion.

These events also feed Harness's runtime-only host-owned stagnation state.
`raw.ndjson` is authoritative; the projection is not serialized into
`state.json` and is reconstructed only from current evaluator, nudge, and
branch events. It remains model-invisible, while `session stats` and `session
analyze` report aggregate evaluator, stagnation, and mutation counters without
emitting candidate IDs, evidence references, or filesystem paths.

By default, a rejecting Stop continuation receives one generic strategy-reset
instruction when the active evaluator lane reaches two consecutive
non-improvements. The reset is one-shot for that lane and is not re-armed by a
later improvement. Disable it with `-stagnation-nudge=false`, config
`"stagnation_nudge": false`, or `HARNESS_STAGNATION_NUDGE=false`.

Every handler has an independent deadline: `timeout_seconds` defaults to 120
seconds and is capped at 600. A timeout records a bounded diagnostic, terminates
that command, and continues to later matching handlers instead of hanging the
prompt. After `max_consecutive_timeouts` (default 3), that handler's circuit is
opened for `timeout_cooldown_seconds` (default 60, maximum 3600); skipped calls
are also recorded. Set `max_consecutive_timeouts` to `0` to disable the circuit.
A successful run or non-timeout failure clears the consecutive-timeout streak.
Prompt cancellation remains distinct from timeout and is propagated to the hook
process. Diagnostics include only event/handler identity, target/tool id,
deadline, elapsed time, bounded outcome, streak, and circuit state—never command
text, stdin payload, stdout, or stderr.

Tool calls are parallel-eligible by default. Hooks are evaluated per target:
`PreToolUse`/`PostToolUse` only affect parallelism for calls whose tool name
matches the hook's `matcher`. A hook that matches `edit` does not prevent
parallel dispatch of co-issued `shell` calls; only calls whose own name matches
a configured hook matcher are forced to run sequentially. `Run` is safe for
concurrent use; `Config` must not be mutated after `Runner` creation.

`hook_configs` files may contain either a `{"hooks": {...}}` wrapper or a bare
event map, and relative `hook_configs` paths resolve against the config-file
directory. Static preferences belong in `~/.agents/AGENTS.md`; command-derived
facts belong in hook output, which the model receives as `[hook context]`.

### Stop-hook evaluator recipe

`auto` is the built-in default agent. Harness does not invent a universal
verifier command: build, test, lint, and evaluation contracts are
repository-specific, and the default hook set remains empty. A rejecting Stop
hook still supplies bounded evidence to the existing one-turn corrective path.

`examples/harness/stop-evaluator/` enables a concrete Stop-hook evaluator for
this Go repository. From the Harness repository root, run it explicitly with:

```sh
harness -config examples/harness/stop-evaluator/config.json \
  -p 'Implement the requested change and verify it.'
```

The recipe combines the built-in `auto` agent with a `Stop` hook whose
`verify.sh` runs `go build ./...`, `go vet ./...`, and `go test ./...`. Hook
invocations emit a typed evaluator result: success reports `accepted:true`,
while failure reports `accepted:false` with a deterministic score equal to the
number of checks that passed, `score_direction:"maximize"`, and the remaining
failed-check count before exiting 2. Run the script directly for human-readable
verification output and bounded failure details. The one-turn bound is the
existing Stop-hook recursion guard, not an unbounded self-improvement loop. The
script also reads `can_block` from hook stdin and skips expensive work without
claiming an evaluation result for cancellation, provider failure, and
exhausted-budget notifications. Hard turn, token, and cost limits still take
precedence.

To add the external gate in another project: copy the recipe, update the hook
command path, and replace `verify.sh` with that project's objective checks.
Keep the hook contract deterministic and typed, and keep direct invocation
useful to a person diagnosing failures. The recipe never rewrites its own
verifier, creates commits, or changes git history; candidate promotion remains
an explicit user action.
