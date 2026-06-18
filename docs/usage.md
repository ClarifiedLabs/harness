# Usage Reference

This page covers everyday behavior that is too detailed for the README:
provider/model selection, one-shot mode, flags, configuration, REPL commands,
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

Exit codes: `0` completed, `1` runtime error, `2` usage error, `130`
interrupted.

## Flags

```text
-p <prompt|->     one-shot mode; "-" or piped stdin reads the prompt from stdin
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
-default-context-window <n>   fallback window for configured models without context metadata (default 256000)
-context-window <n>   override the model's context window (tokens)
-reasoning-effort <level>   reasoning/thinking effort when supported
-reasoning-enabled <bool>    explicitly enable or disable reasoning when supported
-reasoning-budget-tokens <n> reasoning/thinking budget tokens when supported
-reasoning-summary <mode> reasoning summary for Responses API: auto, concise, detailed, or none
-responses-stateful   use Responses API previous_response_id continuation when supported (default true)
-image-detail <level>   default image detail: auto, low, high, or original
-image <path|detail:path>   attach an image in one-shot mode; repeatable
-agent <name>     agent: auto (default), plan, independent, or a config-defined agent
-search-tools <mode>   search tools to expose: auto, grep, rg, or both
-v                show tool result snippets (first ~5 lines, dimmed)
-tool-stream      show live tool-call progress (default true; use -tool-stream=false to disable)
-show-diffs       show per-tool-call file diffs for built-in file edits
-q, --quiet       suppress status diagnostics and reasoning output unless -reasoning-summary is set
--version        print release version and exit 0
--log-level <level>  diagnostic log level: debug, info, warn, error (also LOG_LEVEL)
-no-color         disable ANSI color (also: NO_COLOR env var; color is TTY-only anyway)
-timestamps <mode>  bracketed status timestamps: short (default), full/long, or none
-no-timestamps   alias for -timestamps=none
-repl-prompt <text>    REPL input prompt format (default "[{agent}] > ")
-repl-edit-mode <mode> REPL prompt edit mode: emacs (default) or vi
--show-config    dump the resolved config, including defaults, as JSON and exit
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
self-contained; replay logs show only image metadata.

## Configuration And Environment

Precedence is **flags > environment > config file > built-in defaults** for any
setting that has a flag. Settings with no flag use **environment > config file >
default**. This covers the MCP/LSP `enable` and `proxy` keys and the tool-result
caps (`HARNESS_TOOL_RESULT_MAX_BYTES` / `HARNESS_TOOL_RESULT_MAX_LINES`). A few
context-efficiency knobs are config-file-only.

- Environment: `HARNESS_MODEL_PROXY_URL`, `HARNESS_PROVIDER`, `HARNESS_MODEL`,
  `HARNESS_MAX_TURNS`, `HARNESS_DEFAULT_CONTEXT_WINDOW`, `HARNESS_TIMESTAMPS`,
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
- `--agents` prints the resolved agent list without contacting the model proxy.
  `--models` prints the configured proxy model catalog.
- Context-efficiency knobs are config-file-only except where noted:
  `agents_md_warn_bytes`, `read_file_default_limit`, `compact_keep_turns`,
  `compact_summary_max_tokens`, and `compact_tool_result_max_bytes`.
  Tool-result truncation is controlled by config `tool_result_max_bytes` /
  `tool_result_max_lines` or env `HARNESS_TOOL_RESULT_MAX_BYTES` /
  `HARNESS_TOOL_RESULT_MAX_LINES`. The delegate tool also has
  `delegate_max_turns` as a config-file-only cap.

Harness automatically adds static AGENTS instructions from
`~/.agents/AGENTS.md`, then from `AGENTS.md` in the current working directory.
Missing files are ignored; unreadable existing files fail startup.

Reasoning controls are opt-in: `reasoning_effort` /
`HARNESS_REASONING_EFFORT` / `-reasoning-effort`, `reasoning_enabled` /
`HARNESS_REASONING_ENABLED` / `-reasoning-enabled`, and
`reasoning_budget_tokens` / `HARNESS_REASONING_BUDGET_TOKENS` /
`-reasoning-budget-tokens`. Responses API reasoning summaries are controlled by
`reasoning_summary` / `HARNESS_REASONING_SUMMARY` / `-reasoning-summary`; they
default to `auto` in interactive sessions and off in one-shot mode. `-q` disables
reasoning summary output unless `-reasoning-summary` is explicitly set on the CLI.

Responses continuation is on by default for proxy providers that report both
`api_type: "responses"` and `responses_stateful:true`. Disable it with
`responses_stateful:false`, `HARNESS_RESPONSES_STATEFUL=false`, or
`-responses-stateful=false`.

## Model Proxy Setup

Run `harness-model-proxy --setup` to create a proxy config and a provider config
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
API key. After setup, run:

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
to create real provider allowlists.

Run `harness-model-proxy --refresh-models` to fetch the latest live `models.dev`
catalog and refresh metadata for the currently configured model allowlists,
preserving stored API keys. Unlike setup, refresh fails if models.dev is
inaccessible.

## REPL Commands

Lines starting with `/` are commands; `//` sends a literal leading slash. A line
beginning with `$name` invokes the named skill. `$$` sends a literal leading
`$`.

In terminals that support bracketed paste, pasted text is submitted as one
literal prompt, preserving embedded newlines. Pasted `/commands` are not
executed as meta-commands. For non-interactive large input, prefer `-p -` or
piped stdin.

At an interactive terminal, the prompt supports basic line editing. Shift-Enter
inserts a newline without submitting. Press Ctrl-G at the prompt, or run
`/edit [draft]`, to open an external editor for a multi-line prompt. Harness
uses `$VISUAL`, then `$EDITOR`, then `vi`.

| command | effect |
|---|---|
| `/help` | list commands |
| `/exit`, `/quit` | save, print a session token summary, and exit |
| `/clear` | reset the conversation; rotate to a fresh session directory |
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
| `/model <id>` | switch subsequent turns to model `<id>` |
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

Anthropic usage does not currently expose a separate reasoning-token field;
extended thinking is counted in output tokens, so the reasoning total remains
zero for Anthropic sessions.

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
  `[cancelled]`, and returns to the prompt.
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
