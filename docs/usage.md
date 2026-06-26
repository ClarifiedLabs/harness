# Usage Reference

This page covers everyday behavior that is too detailed for the README:
provider/model selection, interactive initial prompts, one-shot mode, flags, configuration, REPL commands,
agents, sessions, compaction, interrupts, and hooks.

For tool behavior, see [tools.md](tools.md). For MCP and LSP, see
[mcp.md](mcp.md) and [lsp.md](lsp.md).

## Provider Selection

`harness` fetches providers and models from `harness-model-proxy`. A model value
like `openrouter:openai/gpt-5.5` selects the proxy provider `openrouter` while
sending `openai/gpt-5.5` as the provider-local model id. Model selection belongs
to `harness`: use `-model`, `HARNESS_MODEL`, config `model`, or `/model` in the
REPL.

Use `harness -model-proxy-url http://host:port` when the proxy runs somewhere
other than `127.0.0.1:8765`. When `-model-proxy-url` /
`HARNESS_MODEL_PROXY_URL` is left unset, harness applies the effective default
`http://127.0.0.1:8765`.

Use `harness --check-model-proxy` to verify that the configured proxy is
reachable. It sends `GET /v1/models`, prints a short success line on stdout, and
exits before creating a session or starting the REPL.

Use `harness --models` to print the providers and models exposed by the
configured proxy. Use `harness --agents` to print the resolved built-in and
config-defined agents. Both commands exit before creating a session.

## Interactive Initial Prompt

Use `-i` or `-initial-prompt` to run one prompt immediately and then continue in
the normal REPL:

```sh
harness -model gpt-5.5 -i "review the current git diff"
```

The initial prompt is literal text: leading `/` is not a REPL command and leading
`!` is not a shell escape. Unlike `-p`, `-i` does not read from stdin or append
piped stdin; any stdin lines remain available as scripted REPL input after the
initial turn. `-image` attachments used with `-i` attach only to the initial
prompt.

## One-Shot Mode

In one-shot mode (`-p`) the assistant's text goes to stdout while model progress,
model cost checkpoints, tool-call progress, tool summaries, the usage line,
notices, explicitly enabled reasoning summaries, and errors go to stderr. Tool
preamble/commentary messages are assistant text, so they stream with the answer
rather than as bracketed status lines.

When stdout is a terminal, basic Markdown is rendered for readability; redirected
or piped one-shot stdout stays raw model text. Bracketed status lines are
timestamped by default; disable them when you want untimestamped diagnostics:

```sh
harness -model gpt-5.5 -timestamps=none -p "explain this repo in one paragraph" > answer.txt
```

`-p -`, or piping into stdin, reads the prompt from stdin. With both a flag value
and piped stdin they are concatenated, so this works:

```sh
harness -p "summarize:" < notes.txt
```

At the end of a one-shot run harness prints a `[session summary: …]` cost line to
stderr (cumulative input/cached/output/reasoning tokens and total cost). This
summary bypasses `-q`/`--quiet`, so a quiet one-shot run still reports what it
spent.

Exit codes: `0` completed, `1` runtime error, `2` usage error, `130`
interrupted.

## Flags

```text
-p <prompt|->     one-shot mode; "-" or piped stdin reads the prompt from stdin
-i, -initial-prompt <prompt>   run an initial prompt, then continue in the REPL
-provider <name>  model proxy provider id
-model <id>       model id
-model-proxy-url <url>   harness-model-proxy URL (default http://127.0.0.1:8765)
-system-prompt <text|@file>    replace the static system prompt
-no-env           omit the environment context block (cwd/os/date/git)
-resume <file>    load a session transcript and continue
-session <file>   explicit session save path
-histfile <path>      REPL history file path
-histfilesize <n>     max REPL history entries stored on disk (0 disables, default 1000)
-histsize <n>         max REPL history entries loaded into memory (0 disables, default 1000)
-max-turns <n>    model turns per user prompt; <=0 means unlimited (default 250)
-tool-timeout <s>   per-tool-call timeout backstop in seconds; <=0 disables (default 600). A
                    hung tool that ignores cancellation is force-failed after this many
                    seconds so it cannot stall a turn; run_command's own timeout_seconds stays
                    authoritative and is never cut below.
-max-turn-tokens <n>   stop a user turn after this many accumulated tokens; 0 means unlimited
                    (default 0). Counts input + cache-read + cache-write + output + reasoning
                    tokens across every model call in the turn, and breaks before the next paid
                    request with a `[stopped: turn token budget N exceeded]` notice.
-max-output-tokens <n> per-model-turn output cap; 0 uses the automatic cap (default 0)
-max-prompt-cost <usd>   stop a user turn once its accumulated model cost reaches this many USD;
                    0 means unlimited (default 0). Uses catalog pricing, so it applies only to
                    models with a known price; breaks before the next paid request with a
                    `[stopped: turn cost budget $X reached]` notice. Complements -max-turn-tokens.
-default-context-window <n>   fallback window for configured models without context metadata (default 256000)
-context-window <n>   override the model's context window (tokens)
-reasoning-effort <level>   reasoning/thinking effort when supported
-reasoning-enabled <bool>    explicitly enable or disable reasoning when supported
-reasoning-budget-tokens <n> reasoning/thinking budget tokens when supported
-reasoning-summary <mode> reasoning summary for Responses API: auto, concise, detailed, or none
-responses-stateful   use Responses API previous_response_id continuation when supported (default true)
-image-detail <level>   default image detail: auto, low, high, or original
-image <path|detail:path>   attach an image in one-shot mode or to the initial -i prompt; repeatable
-agent <name>     agent: auto (default), plan, independent, or a config-defined agent
-search-tools <mode>   search tools to expose: auto, grep, rg, or both
-v                show tool result snippets (first ~5 lines, dimmed)
-tool-stream      show live tool-call progress (default true; use -tool-stream=false to disable)
-show-diffs       show per-tool-call file diffs for built-in file edits (default true; use -show-diffs=false to disable)
-q, --quiet       suppress status diagnostics and reasoning output unless -reasoning-summary is set;
                  still prints one per-turn usage/cost line at an interactive terminal (suppressed only
                  when output is also non-TTY/piped), and one-shot runs always print the session summary
--version        print release version and exit 0
--log-level <level>  diagnostic log level: debug, info, warn, error (also LOG_LEVEL)
-no-color         disable ANSI color (also: NO_COLOR env var; color is TTY-only anyway)
-timestamps <mode>  bracketed status timestamps: short (default), full/long, or none
-no-timestamps   alias for -timestamps=none
-repl-prompt <text>    REPL input prompt format (default "[{agent}] > ")
-repl-edit-mode <mode> REPL prompt edit mode: emacs (default) or vi
--format <text|json>  output format for informational commands (default text)
--show-config    dump the resolved config, including defaults, as JSON and exit
--debug-request  dump the first provider-neutral model request as JSON and exit without calling the model
--agents         list configured agents and exit
--models         list configured providers and models and exit
--check-model-proxy    check harness-model-proxy reachability and exit
-hooks <file>    replace configured hooks with this hook config file
-config <file>    alternate config path
-h, --help        print this usage screen and exit 0
```

`-system-prompt` accepts a `@file` reference. A literal leading `@` is escaped as
`@@`; `@~/path` expands through the current user's home directory. Relative
`@file` references in the config file resolve from the config file directory. It
replaces the built-in static instructions only; runtime sections such as
environment context, user/project `AGENTS.md`, skills, and agent prompts are
still composed around it.

Image attachments accept local PNG, JPEG, WebP, and non-animated GIF files.
Images are embedded in the saved transcript as base64 so resumed sessions remain
self-contained; replay logs show only image metadata. Harness only sends queued
images when the current model explicitly advertises image input support. Manual
provider configs should set `input_modalities`, for example
`["text", "image"]`; models without `image` are treated as text-only and image
attachments are skipped with a warning.

## Configuration And Environment

Precedence is **flags > environment > config file > built-in defaults** for any
setting that has a flag. Settings with no flag use **environment > config file >
default**. This covers the MCP/LSP `enable` and `proxy` keys and the tool-result
caps (`HARNESS_TOOL_RESULT_MAX_BYTES` / `HARNESS_TOOL_RESULT_MAX_LINES`). A few
context-efficiency knobs are config-file-only.

- Environment: `HARNESS_MODEL_PROXY_URL`, `HARNESS_PROVIDER`, `HARNESS_MODEL`,
  `HARNESS_MAX_TURNS`, `HARNESS_MAX_TURN_TOKENS`,
  `HARNESS_MAX_OUTPUT_TOKENS`, `HARNESS_TOOL_TIMEOUT`,
  `HARNESS_DEFAULT_CONTEXT_WINDOW`, `HARNESS_TIMESTAMPS`,
  `HARNESS_IMAGE_DETAIL`, and most other `HARNESS_*` equivalents for
  user-facing flags. The convention is `HARNESS_` plus the flag name uppercased
  with dashes turned into underscores. For example, `-context-window`, `-no-env`,
  `-no-color`, `-resume`, and `-session` map to `HARNESS_CONTEXT_WINDOW`,
  `HARNESS_NO_ENV`, `HARNESS_NO_COLOR`, `HARNESS_RESUME`, and
  `HARNESS_SESSION`.
- The `-v` verbose flag uses `HARNESS_VERBOSE`. `--log-level` uses `LOG_LEVEL`.
  `HARNESS_NO_TIMESTAMPS` is an alias for `HARNESS_TIMESTAMPS=none`.
  `HARNESS_REPL_INPUT_TRACE` is a debug knob that appends timestamped
  terminal-input events to the given file path (`-` for stderr).
- Provider API-key environment variables are read only by
  `harness-model-proxy`.
- The optional config file is `~/.config/harness/config.json`, overrideable with
  `-config`. It may set `model_proxy_url`, `provider`, `model`, `agent`,
  `agents`, `hooks`, `hook_configs`, and flag defaults. See
  `examples/harness/config.json` for a representative schema.
- `--show-config` prints the resolved config as JSON after applying file, env,
  flag, and built-in defaults. It exits without contacting the model proxy.
- `--debug-request` prints the first provider-neutral `llm.Request`, context
  estimate, active tools, reasoning settings, and request byte counts. It
  resolves the model proxy catalog, then exits before prewarm, session hooks,
  session writes, or any model stream.
- `--agents` prints the resolved agent list without contacting the model proxy.
  `--models` prints the configured proxy model catalog. Use `--format json` with
  `--agents`, `--models`, or `--check-model-proxy` for structured output.
- Context-efficiency knobs are config-file-only except where noted:
  `agents_md_warn_bytes`, `read_file_default_limit`, `compact_keep_turns`,
  `compact_summary_max_tokens`, and `compact_tool_result_max_bytes`.
  Tool-result truncation is controlled by config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. The delegate tool also has
  `delegate_max_turns` as a config-file-only cap.
- Tool-surface limits for MCP and LSP are config-file-only: `mcp.max_tools` caps
  how many discovered remote MCP tools are auto-exposed (`0` = unlimited),
  `mcp.disabled_servers` is a list of remote MCP server names dropped from
  auto-exposure, and `lsp.tools` registers only the listed subset of LSP tools
  (empty = all). See [mcp.md](mcp.md) and [lsp.md](lsp.md). An explicit
  `allowed_tools` whitelist can still name a tool that auto-exposure excluded.
- A single model turn's output is capped at the configured
  `max_output_tokens` value when set, otherwise at one quarter of the effective
  `context_window` (with a very high 1,000,000-token runaway ceiling). A model's
  configured `output_limit`, when known, is a ceiling rather than the default.
  The chosen cap is then clamped to the counted or estimated remaining context
  window. This client-side runaway brake is distinct from `-max-turn-tokens`,
  which is the cumulative per-turn token *budget* across all model calls. If a
  provider reports a smaller real context window in an overflow error, harness
  learns that window for the session and retries once. When the cap is reached,
  harness surfaces `[stopped: model reached max tokens]`.
- Before normal model requests, harness resolves input tokens in tiers:
  provider-specific count APIs for OpenAI Responses and Anthropic Messages when
  available through `harness-model-proxy`; a local `o200k_base` BPE estimate for
  OpenAI/OpenRouter Chat Completions; then the coarse byte-based heuristic.

Harness automatically adds static AGENTS instructions from
`~/.agents/AGENTS.md`, then from `AGENTS.md` in the current working directory.
Missing files are ignored; unreadable existing files fail startup.

Reasoning controls are opt-in: `reasoning_effort` /
`HARNESS_REASONING_EFFORT` / `-reasoning-effort`, `reasoning_enabled` /
`HARNESS_REASONING_ENABLED` / `-reasoning-enabled`, and
`reasoning_budget_tokens` / `HARNESS_REASONING_BUDGET_TOKENS` /
`-reasoning-budget-tokens`. Responses API reasoning summaries are controlled by
`reasoning_summary` / `HARNESS_REASONING_SUMMARY` / `-reasoning-summary`; they
default off and are displayed only when explicitly enabled. `-q` disables
reasoning summary output unless `-reasoning-summary` is explicitly set on the CLI.

Responses continuation is on by default for proxy providers that report both
`api_type: "responses"` and `responses_stateful:true`. Disable it with
`responses_stateful:false`, `HARNESS_RESPONSES_STATEFUL=false`, or
`-responses-stateful=false`. If a provider rejects stored Responses requests,
harness disables stateful continuation for that agent and retries the request
stateless.
Responses provider configs may also set `responses_websocket:true` to have the
model proxy use the Responses WebSocket transport instead of HTTP SSE. The proxy
defaults this on for `codex_oauth` Responses providers and preserves an explicit
`responses_websocket:false` override.

## Model Proxy Setup

Run `harness-model-proxy setup` to create a proxy config and a provider config
from models.dev, append a new provider config to an existing proxy config, or
update an existing configured provider without configuring a proxy default model.
Setup lists harness-supported providers, prompts for the API key when the
provider needs one, then lets you choose which provider models are available
locally. If models.dev omits a provider API URL, setup can still derive
first-party OpenAI and Anthropic defaults from exact `@ai-sdk/openai` and
`@ai-sdk/anthropic` package metadata, and maps plain `@ai-sdk/google` to
Google's OpenAI-compatible Gemini endpoint. Vertex Google package variants are
not auto-configured.

The special `openai-codex` provider uses ChatGPT subscription auth instead of an
API key and omits Responses `max_output_tokens` because the Codex backend
rejects that parameter. The proxy also uses the Responses WebSocket transport by
default for this provider, matching Codex's stateful continuation path while
sending `store:false` to the ChatGPT backend. After setup, run:

```sh
harness-model-proxy auth login openai-codex
```

Provider configs accept an optional `auth` block in place of `api_key` /
`api_key_env`; when `auth` is present, API-key fields are ignored and there is no
fallback if auth fails. Supported auth shapes include `token_command`, `oauth2`,
and `codex_oauth`.

For hand-written model-proxy config shape references, see
`examples/harness-model-proxy/config.json` and
`examples/harness-model-proxy/providers.json`. Setup remains the recommended way
to create real provider allowlists. Manual model entries must declare supported
input modalities with `input_modalities`; use `["text"]` for text-only models
and `["text", "image"]` for models that accept image attachments.

`harness-model-proxy` stores the full models.dev catalog at
`~/.config/harness-model-proxy/models.dev.api.json`. `setup` uses the cache
when present; if there is no cache, or the cache cannot be parsed, it fetches and
rewrites the cache before using the vendored fallback snapshot. While serving,
the proxy refreshes this cache when it is older than `24h` by default. Set
`models_dev_cache_ttl` in the proxy config, or pass
`-models-dev-cache-ttl <duration>`, to override the interval; use `0` to disable
periodic serving-time refreshes. Cache updates are parsed and sanity-checked
before replacing the old file; a candidate catalog with no providers/models, or
one whose provider/model counts swing by more than 4x with a meaningful absolute
delta, is rejected and the old cache is preserved. Successful replacements first
copy the previous cache to `models.dev.api.json.bak`, overwriting that one backup
each time.

Run `harness-model-proxy refresh-models` to fetch and cache the latest live
`models.dev` catalog, then refresh metadata for the currently configured model
allowlists while preserving stored API keys. If live fetch fails, refresh uses a
parseable local cache before falling back to the vendored snapshot.

## REPL Commands

Lines starting with `/` are commands; `//` sends a literal leading slash. At an
interactive TTY prompt, lines starting with `!` run a local shell command and
return to the prompt without contacting the model; `!!` sends a literal leading
`!`. In one-shot mode, initial `-i` prompts, non-TTY/scripted input, pasted text, and edited prompts,
`!text` is literal prompt text. In a normal typed prompt, `$name` mentions the
named skill anywhere in the text; the model receives request-only context
telling it to read that skill's `SKILL.md` before acting. `$$` escapes a
literal `$`.

In terminals that support bracketed paste, pasted text fills the prompt for
review and is submitted as one literal prompt when you press Enter, preserving
embedded newlines. A large or multi-line paste shows a one-line
`[N bytes of pasted content]` placeholder instead of the full content; press
Ctrl-G / `/edit` to open the external editor with the full pasted content. A
paste that fills an empty prompt is submitted literally — pasted `/commands`
are not executed, `!command` is not a shell escape, and `$skill` is not
resolved. This holds on the Enter path in every edit mode, including the vi
normal-mode Enter after Esc. Typing anything after a paste (in emacs mode, or
after entering vi normal mode with Esc) makes the whole line typed (so
`!`/`/`/`$` apply). In terminals that do not support bracketed paste, harness
falls back to detecting a fast paste burst so newlines in a paste do not submit
prematurely; set `HARNESS_REPL_PASTE_HEURISTIC=off` to disable that. For
non-interactive large input, prefer `-p -` or piped stdin.

At an interactive terminal, the prompt supports basic line editing. Shift-Enter
inserts a newline without submitting. Press Ctrl-G at the prompt, or run
`/edit [draft]`, to open an external editor for a multi-line prompt. Harness
uses `$VISUAL`, then `$EDITOR`, then `vi`. On `!` command lines, Tab completes
the first word from `PATH` and completes path words with `/`, `~/`, `./`, `../`,
and nested relative path prefixes.

| command | effect |
|---|---|
| `/help` | list commands |
| `/exit`, `/quit` | save, print a session token summary, and exit |
| `/clear` | echo the discarded session token/cost totals, then reset the conversation and rotate to a fresh session directory |
| `/compact` | force compaction now |
| `/context` | dump the current provider-neutral model context as JSON |
| `/context <file>` | save the current provider-neutral model context as JSON |
| `/usage` | cumulative input, cached input, output, reasoning tokens, and cost |
| `/tools` | list enabled built-in and MCP tools with descriptions, plus disabled optional tools |
| `/image` | list images queued for the next prompt |
| `/image <path>` | attach an image to the next prompt |
| `/image --detail <level> <path>` | attach an image with per-image detail |
| `/image --clear` | clear queued images |
| `/edit [draft]` | open an external editor for the next prompt |
| `/save [file]` | force save, optionally elsewhere |
| `/model` | choose a configured provider/model; interactive runs can optionally save it as the default |
| `/model <id>` | switch subsequent turns to model `<id>`; a near-miss falls back to a unique prefix/substring match |
| `/model <provider>:<id>` | switch to `<id>` on a specific configured provider |
| `/reasoning` | list reasoning controls for the current model |
| `/reasoning on`, `/reasoning off`, `/reasoning default` | set explicit reasoning toggle or return to provider defaults |
| `/reasoning budget <n>` | set reasoning budget tokens for subsequent turns |
| `/reasoning effort <level>` | switch reasoning effort for subsequent turns |
| `/reasoning summary <auto\|concise\|detailed\|none>` | switch Responses API reasoning summaries for subsequent turns |
| `/effort` | list reasoning effort levels for the current model, marking the selected one |
| `/effort <level>` | switch reasoning effort for subsequent turns |
| `/agent` | list agents and descriptions, marking the current one |
| `/agent <name>` | switch the active agent |
| `/mode`, `/mode <name>` | alias for `/agent` |
| `/plan` | alias for `/agent plan` |
| `/auto` | alias for `/agent auto` |
| `/background` | list background jobs |
| `/background <id>` | show a background job's status, result, and transcript path |
| `/background cancel <id>` | cancel a running background job |
| `/skills` | list available skills |
| `/vi on\|off` | enable or disable vi-style prompt editing |
| `!command` | run a local shell command at an interactive TTY prompt |

Anthropic usage does not currently expose a separate reasoning-token field;
extended thinking is counted in output tokens, so the reasoning total remains
zero for Anthropic sessions.

An unknown `/command` prints a `did you mean <command>?` suggestion (nearest known
command by edit distance) instead of failing silently. The per-turn usage line
appends cache-read and reasoning token counts (with the cache-hit ratio) when they
are non-zero, and a model with no configured price prints a one-time
`[note: no price configured …]` notice instead of silently dropping cost.

### Waiting and typing during a turn

At an interactive terminal harness shows a live wait indicator while a model
request or a tool call is outstanding: a single in-place line such as
`[model: turn 1 · 12s]` (or `[tool: grep · 3s]`), updated about once a second and
appended with the running context-window percentage (`· ctx 30%`) for model waits.
It is erased the instant real output or a tool line appears, and is shown only at a
TTY when not quiet.

Text typed while a turn is running is captured with echo off and shown on that wait
line after a `>` marker. During-turn input is **never** auto-submitted: Enter inserts
a newline into the buffer rather than starting a turn. When the turn finishes — on
normal completion **or** on interrupt (Ctrl-C / double-Esc) — the accumulated text is
deposited into the next prompt as editable, pre-filled text for you to review, edit,
or submit manually. Ctrl-C and double-Esc still cancel the active turn.

## Agents

An agent definition bundles a set of allowed tools with extra system-prompt
instructions and optional provider/model overrides. Select one with
`-agent <name>`, `HARNESS_AGENT`, or `agent` in the config file. Switch
mid-session with `/agent <name>`.

Three agents are built in:

| agent | tools | behavior |
|---|---|---|
| `auto` | all available built-in tools plus discovered MCP tools, including `delegate` and background job tools | the default; the model decides what to do |
| `plan` | inspection tools, read-only MCP tools, `write_tmp_file`, `update_todos`, `delegate`, and `background_jobs` | collaborate on a plan without modifying the project |
| `independent` | all available built-in tools plus discovered MCP tools, including `delegate` and background job tools | complete the task end-to-end without pausing for input |

Define new agents or override built-ins in the config file under `agents`.
Entries field-level merge onto a built-in of the same name:

```json
{
  "agent": "plan",
  "agents": {
    "plan": { "prompt": "@~/.config/harness/plan-prompt.md" },
    "security_review": {
      "description": "Review for concrete security issues.",
      "allowed_tools": ["read_file", "list_dir", "grep", "git_readonly"],
      "mcp_tools": "read_only",
      "provider": "anthropic",
      "model": "claude-opus-4-8",
      "prompt": "Review the diff and surrounding code for security issues. Report only concrete findings."
    }
  }
}
```

Each agent can set `mcp_tools` to control automatic exposure of discovered MCP
tools: `disabled`, `read_only`, or `all`. Explicit `mcp__...` names in
`allowed_tools` still work as a strict whitelist. Tool gating is the one place
the harness restricts tools; the underlying tools still assume an external
sandbox for real isolation.

## Sessions

- A session path is a directory. `state.json` is the compact resumable state,
  `raw.ndjson` is an append-only replay log, `compactions/` stores raw messages
  removed from active context, `children/` stores child-agent transcripts and
  metadata, and `artifacts/tool-results/` stores full outputs omitted from model
  context.
- The compact state is saved after every turn, atomically. Auto-save uses
  `~/.local/state/harness/sessions/<timestamp>`, honoring `$XDG_STATE_HOME`.
- `-session <dir>` chooses an explicit session directory. `-resume <dir>` loads
  its `state.json` and continues. `/clear` rotates to a fresh directory.
- Transcripts are provider-neutral, so a session started against Anthropic can
  resume against an OpenAI-compatible server and vice versa.
- A session saved mid-turn is repaired on load by synthesizing an `interrupted`
  tool result, so the resumed transcript is valid for both APIs.

Inspect saved sessions with:

```sh
harness session replay ~/.local/state/harness/sessions/20260611T123456Z
harness session timings ~/.local/state/harness/sessions/20260611T123456Z
```

## Compaction

Compaction fires when `max(reported input tokens, estimated full-request
footprint)` reaches 78% of the model's context window, or on `/compact`.
Harness summarizes old conversation history to free context while keeping the
system prompt and the configured number of recent turns (`compact_keep_turns`,
default `4`) verbatim.

Before summarization, large old tool results and large old tool inputs are
reduced to previews (`compact_tool_result_max_bytes`, default `4096`), old images
are replaced with placeholders, and the raw removed messages are archived under
`compactions/`. If the old history is too large for one summary call, harness
summarizes chunks and then summarizes the chunk summaries. If compaction fails,
the full transcript is kept.

Turn summaries include approximate context footprint and, when stateful Responses
sends a smaller request than the full active conversation, the payload estimate.
If the active context, request payload, or tool schemas are large enough to
likely slow response startup, harness prints one warning per user turn to stderr.

## Interrupts

- Ctrl-C during a turn, or Esc twice in short succession during a REPL turn,
  cancels the turn. It aborts the HTTP stream, kills any `run_command` process
  group, keeps streamed partial text, strips unexecuted tool calls, prints
  `[cancelled]`, and returns to the prompt. Any text typed during the turn is
  preserved and deposited into the next prompt as editable pre-filled text.
- A second Ctrl-C within about one second, or Ctrl-C at the idle prompt, saves,
  prints the session token summary, and exits 130.
- Ctrl-D at the prompt saves, prints the summary, and exits 0.
- Ctrl-C during startup or helper-command network work cancels the in-flight
  request and exits 130 instead of waiting for the request timeout.

## Hooks

Harness supports command hooks for `SessionStart`, `UserPromptSubmit`,
`PreToolUse`, `PostToolUse`, `PreCompact`, `PostCompact`, and `Stop`.

```json
{
  "hook_configs": ["hooks_config.json"],
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "run_command|apply_patch",
        "hooks": [
          {
            "type": "command",
            "command": "./hooks/pre-tool.sh",
            "timeout_seconds": 30,
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
`hook_event_name`, `model`, and `permission_mode`, plus per-event fields.

`matcher` is a Go regexp over the tool name for tool hooks, `manual|auto` for
compaction hooks, and `startup|resume|clear` for `SessionStart`. Omitted, empty,
or `*` matches all. Hook commands may block with exit code `2` or JSON stdout
such as `{"decision":"block","reason":"..."}` / `{"continue":false}`. Plain
stdout is added as hook context only when the command exits `0`.

`hook_configs` files may contain either a `{"hooks": {...}}` wrapper or a bare
event map, and relative `hook_configs` paths resolve against the config-file
directory. Static preferences belong in `~/.agents/AGENTS.md`; command-derived
facts belong in hook output, which the model receives as `[hook context]`.
