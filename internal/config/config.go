// Package config resolves user-facing settings from flags, environment
// variables, an optional config file, and built-in defaults, with precedence
// flags > env > file > defaults. It deliberately depends on neither internal/llm
// nor the model proxy client: it is the flag/env/file machinery layer, and main
// translates a resolved Config into agent/ui options.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"harness/internal/hooks"
	"harness/internal/logging"
	"harness/internal/replprompt"
)

// ErrHelp is returned by Load when -h/--help is requested. It is not a usage
// error: the caller prints the usage screen (via Usage) and exits 0, because
// help is a request, not a misuse (design §10).
var ErrHelp = flag.ErrHelp

// Config is the fully resolved, provider-neutral configuration.
type Config struct {
	// Provider selection.
	Provider      string `json:"provider"` // model proxy provider id
	Model         string `json:"model"`
	ModelProxyURL string `json:"model_proxy_url"`

	// Proxy API keys. These become Authorization: Bearer headers sent to the
	// respective proxies. Empty means the proxy is contacted without auth.
	ModelProxyAPIKey string `json:"model_proxy_api_key"`

	// System prompt composition (design §8.5).
	SystemPrompt string `json:"system_prompt"` // -system-prompt: replace the static system prompt
	NoEnv        bool   `json:"no_env"`        // -no-env: drop the env-context block

	// Session.
	Resume  string `json:"resume"`  // -resume: load this transcript and continue
	Session string `json:"session"` // -session: explicit save path

	// REPL history (bash-style). HistFile overrides the default location;
	// HistFileSize caps on-disk entries (0 disables persistence); HistSize
	// caps in-memory recall (0 disables recall).
	HistFile     string `json:"histfile"`
	HistFileSize int    `json:"histfilesize"`
	HistSize     int    `json:"histsize"`

	// Loop / model limits.
	MaxTurns                  int               `json:"max_turns"`              // -max-turns, default 250
	MaxTurnTokens             int               `json:"max_turn_tokens"`        // -max-turn-tokens, accumulated-token ceiling per user turn; 0 = unlimited
	MaxOutputTokens           int               `json:"max_output_tokens"`      // -max-output-tokens, per model turn output cap; 0 = automatic
	MaxPromptCostUSD          float64           `json:"max_prompt_cost_usd"`    // -max-prompt-cost, accumulated USD ceiling per user turn; 0 = unlimited (needs known model cost)
	ToolTimeoutSeconds        int               `json:"tool_timeout_seconds"`   // -tool-timeout, per-tool-call dispatch ceiling (s); default 600, <=0 disables
	DefaultContextWindow      int               `json:"default_context_window"` // -default-context-window, fallback when metadata lacks a window
	ContextWindow             int               `json:"context_window"`         // -context-window, 0 = registry/default
	ReasoningEffort           string            `json:"reasoning_effort"`
	ReasoningEnabled          *bool             `json:"reasoning_enabled"`
	ReasoningBudgetTokens     *int              `json:"reasoning_budget_tokens"`
	ReasoningSummary          string            `json:"reasoning_summary"`
	ImageDetail               string            `json:"image_detail"`
	Images                    []ImageAttachment `json:"images"`
	SearchTools               string            `json:"search_tools"`
	AgentsMDWarnBytes         int               `json:"agents_md_warn_bytes"`          // config-only warning threshold in bytes; default 8192, explicit 0 disables
	ToolResultMaxBytes        int               `json:"tool_result_max_bytes"`         // 0 = tool default
	ToolResultMaxLines        int               `json:"tool_result_max_lines"`         // 0 = tool default
	ReadFileDefaultLimit      int               `json:"read_file_default_limit"`       // config-only; 0 = tool default
	CompactKeepTurns          int               `json:"compact_keep_turns"`            // config-only; 0 = agent default
	CompactSummaryMaxTokens   int               `json:"compact_summary_max_tokens"`    // config-only; 0 = agent default
	CompactToolResultMaxBytes int               `json:"compact_tool_result_max_bytes"` // config-only; 0 = agent default, negative disables
	DelegateMaxTurns          int               `json:"delegate_max_turns"`            // config-only; default 20, per delegate call cap
	ResponsesStateful         bool              `json:"responses_stateful"`

	// Agent definition. Empty means "not specified" so main can let a resumed
	// session supply the agent before falling back to the default.
	Agent  string                     `json:"agent"`
	Agents map[string]FileAgentConfig `json:"agents"` // raw "agents" config entries; main converts to agentdef.FileDefinition

	// HandoffAgent is the agent a plan->implementation handoff switches to when
	// no agent is given on the request. Defaults to "auto".
	HandoffAgent string `json:"handoff_agent"`

	// One-shot mode (design §10).
	Prompt    string `json:"one_shot_prompt"`     // -p value
	PromptSet bool   `json:"one_shot_prompt_set"` // -p was supplied (distinguishes "" from absent)

	// Interactive initial prompt mode.
	InitialPrompt    string `json:"initial_prompt"`     // -i/-initial-prompt value
	InitialPromptSet bool   `json:"initial_prompt_set"` // initial prompt flag was supplied

	// UI.
	Verbose       bool   `json:"verbose"`     // -v
	ToolStream    bool   `json:"tool_stream"` // -tool-stream: show live tool-call progress
	ShowDiffs     bool   `json:"show_diffs"`  // -show-diffs: show per-tool file diffs
	Quiet         bool   `json:"quiet"`       // -q / --quiet: suppress status messages and implicit reasoning output
	LogLevel      string `json:"log_level"`   // --log-level / LOG_LEVEL: debug, info, warn, error
	NoColor       bool   `json:"no_color"`    // -no-color or NO_COLOR
	TimestampMode string `json:"timestamps"`  // -timestamps: short, full, or none
	ReplPrompt    string `json:"repl_prompt"` // -repl-prompt: REPL input prompt format
	ReplEditMode  string `json:"repl_edit_mode"`
	OutputFormat  string `json:"-"` // -format: display format for informational commands

	// Meta.
	ShowConfig      bool `json:"show_config"`       // --show-config: print this resolved config and exit
	DebugRequest    bool `json:"debug_request"`     // --debug-request: print the first model request and exit
	ShowAgents      bool `json:"show_agents"`       // --agents: print resolved agents and exit
	ShowModels      bool `json:"show_models"`       // --models: print configured proxy models and exit
	CheckModelProxy bool `json:"check_model_proxy"` // --check-model-proxy: verify the proxy catalog endpoint and exit

	// Hooks are command-only lifecycle handlers. Inline hooks and hook_configs
	// are config-file-only; --hooks replaces both for orchestrated launches.
	Hooks       hooks.Config `json:"hooks"`
	HookConfigs []string     `json:"hook_configs,omitempty"`

	// MCP proxy integration (opt-in). Proxy is the HTTP proxy URL; an empty
	// Proxy means "use the shared default", which main resolves at connect
	// time so internal/config stays free of proxy packages.
	MCP MCPConfig `json:"mcp"`

	// LSP code intelligence (opt-in). When enabled, harness registers built-in
	// read-only LSP tools directly with short lsp_* names. Server definitions are
	// config-file-only and overlay the embedded defaults by server name.
	LSP LSPConfig `json:"lsp"`
}

// MCPConfig is the resolved harness-side MCP block. All downstream server
// configuration lives with the proxy; the harness only needs to know whether
// MCP is enabled and which proxy to dial.
type MCPConfig struct {
	Enable bool   `json:"enable"`
	Proxy  string `json:"proxy"`   // http(s) proxy URL; "" means resolve the shared default at use
	APIKey string `json:"api_key"` // API key for harness-mcp-proxy

	// Headers are static request headers (e.g. Authorization) sent on every
	// request to the proxy. It is config-file-only (file key "headers" under
	// "mcp"), with NO env var: this matches the config-file-only precedent for
	// structured settings (a map cannot be expressed cleanly through a single env
	// var), so a header set belongs in the config file alongside the proxy URL
	// it authenticates to.
	Headers map[string]string `json:"headers"`

	// MaxTools optionally caps how many discovered remote (HTTP-proxy) MCP tool
	// names are auto-exposed to agents whose mcp_tools mode is read_only/all. 0
	// means unlimited. A single chatty server can otherwise add thousands of
	// schema tokens per turn. Local MCP and LSP tools are not counted. On overflow
	// the surface is truncated in discovery order and a warning is logged; the
	// catalog still holds every tool so an explicit allowed_tools whitelist can
	// still name one the cap excludes. Config-file-only (structured scoping data,
	// matching the Headers precedent).
	MaxTools int `json:"max_tools"`

	// DisabledServers lists remote MCP server labels (the segment between mcp__ and
	// the next __) whose tools are dropped from auto-exposure. Config-file-only.
	DisabledServers []string `json:"disabled_servers,omitempty"`

	// Local configures an explicitly enabled local stdio MCP service that harness
	// spawns (e.g. a local harness-mcp-proxy hosting local tools). It is
	// independent of Enable: a user can run a local stdio MCP service with no
	// remote HTTP proxy.
	Local LocalMCPConfig `json:"local"`
}

// LocalMCPConfig is the resolved harness-side local MCP service block. When
// enabled, harness spawns Command as a stdio MCP child and registers its tools
// alongside the HTTP proxy's. Local MCP is disabled by default; EnableSet
// distinguishes an explicit choice from the default.
type LocalMCPConfig struct {
	Enable    bool              `json:"enable"`
	EnableSet bool              `json:"enable_set"` // whether enable was explicitly set (env or file)
	Command   string            `json:"command"`    // required when local MCP is enabled
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"` // config-file-only, ${VAR}-expanded
}

// LSPConfig is the resolved first-class LSP feature block. Enable follows
// env > file > default, while Servers and Tools are config-file-only because
// they are structured data.
type LSPConfig struct {
	Enable bool `json:"enable"`

	// Tools is an optional allowlist of bare LSP tool names (e.g. "definition",
	// "references", "diagnostics") to register. Empty/unset registers the full
	// built-in set. It lets an operator expose a subset without an allowed_tools
	// whitelist, which would also disable MCP auto-exposure. The "lsp_" prefix is
	// optional in each entry.
	Tools []string `json:"tools,omitempty"`

	Servers map[string]LSPServerConfig `json:"servers,omitempty"`
}

// LSPServerConfig mirrors one inline "lsp.servers" entry. It intentionally
// matches lspproxy.ServerConfig without importing the runtime package into the
// config layer.
type LSPServerConfig struct {
	Languages   []string          `json:"languages"`
	RootMarkers []string          `json:"root_markers"`
	Command     []string          `json:"command"`
	Extensions  []string          `json:"extensions"`
	Env         map[string]string `json:"env"`
	InitOptions json.RawMessage   `json:"initialization_options"`
}

const (
	defaultMaxTurns           = 250
	defaultContextWindow      = 256_000
	defaultDelegateMaxTurns   = 20
	defaultToolTimeoutSeconds = 600
	TimestampShort            = "short"
	TimestampFull             = "full"
	TimestampNone             = "none"

	// DefaultHistFile controls the default REPL history file location. The
	// actual default is resolved by the caller (cmd/harness/main.go) against
	// stateDir, so this constant is used only as documentation and test seed.
	// DefaultHistFileSize and DefaultHistSize mirror bash-style HISTFILESIZE
	// and HISTSIZE caps.
	DefaultHistFileSize = 1000
	DefaultHistSize     = 1000
	DefaultReplEditMode = "emacs"
)

// FileAgentConfig is one entry of the config file's "agents" object. It mirrors
// agentdef.FileDefinition; config deliberately does not import internal/agentdef (which
// would pull in the tools/llm layers), so main performs the conversion.
type FileAgentConfig struct {
	Description  string   `json:"description"`
	AllowedTools []string `json:"allowed_tools"`
	MCPTools     string   `json:"mcp_tools"`
	Prompt       string   `json:"prompt"`
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Reasoning    string   `json:"reasoning"`
}

// ImageAttachment is one -image flag value after parsing and detail resolution.
type ImageAttachment struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// fileConfig mirrors the subset of Config that the main config file may set.
// Provider connection settings and secrets belong to harness-model-proxy, not
// the harness process.
type fileConfig struct {
	Provider                  string                     `json:"provider"`
	Model                     string                     `json:"model"`
	ModelProxyURL             string                     `json:"model_proxy_url"`
	ModelProxyAPIKey          string                     `json:"model_proxy_api_key"`
	SystemPrompt              string                     `json:"system_prompt"`
	NoEnv                     *bool                      `json:"no_env"`
	MaxTurns                  *int                       `json:"max_turns"`
	MaxTurnTokens             *int                       `json:"max_turn_tokens"`
	MaxOutputTokens           *int                       `json:"max_output_tokens"`
	MaxPromptCostUSD          *float64                   `json:"max_prompt_cost_usd"`
	ToolTimeoutSeconds        *int                       `json:"tool_timeout_seconds"`
	DefaultContextWindow      *int                       `json:"default_context_window"`
	ContextWindow             *int                       `json:"context_window"`
	ReasoningEffort           string                     `json:"reasoning_effort"`
	ReasoningEnabled          *bool                      `json:"reasoning_enabled"`
	ReasoningBudgetTokens     *int                       `json:"reasoning_budget_tokens"`
	ReasoningSummary          string                     `json:"reasoning_summary"`
	ImageDetail               string                     `json:"image_detail"`
	SearchTools               string                     `json:"search_tools"`
	AgentsMDWarnBytes         *int                       `json:"agents_md_warn_bytes"`
	ToolResultMaxBytes        *int                       `json:"tool_result_max_bytes"`
	ToolResultMaxLines        *int                       `json:"tool_result_max_lines"`
	ReadFileDefaultLimit      *int                       `json:"read_file_default_limit"`
	CompactKeepTurns          *int                       `json:"compact_keep_turns"`
	CompactSummaryMaxTokens   *int                       `json:"compact_summary_max_tokens"`
	CompactToolResultMaxBytes *int                       `json:"compact_tool_result_max_bytes"`
	DelegateMaxTurns          *int                       `json:"delegate_max_turns"`
	ResponsesStateful         *bool                      `json:"responses_stateful"`
	Verbose                   *bool                      `json:"verbose"`
	ToolStream                *bool                      `json:"tool_stream"`
	ShowDiffs                 *bool                      `json:"show_diffs"`
	LogLevel                  string                     `json:"log_level"`
	NoColor                   *bool                      `json:"no_color"`
	Timestamps                string                     `json:"timestamps"`
	NoTimestamps              *bool                      `json:"no_timestamps"`
	ReplPrompt                string                     `json:"repl_prompt"`
	ReplEditMode              string                     `json:"repl_edit_mode"`
	Agent                     string                     `json:"agent"`
	Agents                    map[string]FileAgentConfig `json:"agents"`
	HandoffAgent              string                     `json:"handoff_agent"`
	Hooks                     json.RawMessage            `json:"hooks"`
	HookConfigs               []string                   `json:"hook_configs"`

	// REPL history (bash-style). HistFile is a string pointer so an empty
	// config file does not override the default path derived in main.
	HistFile     string `json:"histfile"`
	HistFileSize *int   `json:"histfilesize"`
	HistSize     *int   `json:"histsize"`

	MCP *fileMCPConfig `json:"mcp"`
	LSP *fileLSPConfig `json:"lsp"`
}

// fileMCPConfig mirrors the config file's "mcp" object. Pointer/string fields
// follow the existing unset-detection convention: a nil block means "no mcp
// config", letting env and defaults supply every field.
type fileMCPConfig struct {
	Enable          *bool               `json:"enable"`
	Proxy           string              `json:"proxy"`
	APIKey          string              `json:"api_key"`
	Headers         map[string]string   `json:"headers"`
	MaxTools        *int                `json:"max_tools"`
	DisabledServers []string            `json:"disabled_servers"`
	Local           *fileLocalMCPConfig `json:"local"`
}

// fileLocalMCPConfig mirrors the config file's "mcp.local" object.
type fileLocalMCPConfig struct {
	Enable  *bool             `json:"enable"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// fileLSPConfig mirrors the config file's top-level "lsp" object.
type fileLSPConfig struct {
	Enable  *bool                      `json:"enable"`
	Tools   []string                   `json:"tools"`
	Servers map[string]LSPServerConfig `json:"servers"`
}

// Load resolves a Config from the given args (argv after the program name), a
// getenv accessor, and a config-file path. getenv and configPath are injected so
// the loader has no hidden dependency on os.Args/os.Getenv and is testable. An
// empty configPath means "no config file": the caller (main) is responsible for
// existence-checking the implicit default ~/.config/harness/config.json and
// passing "" when it is absent, so that a non-empty path is always required to
// exist (a typo'd -config must not be silently ignored).
func Load(args []string, getenv func(string) string, configPath string) (Config, error) {
	fs, f := newFlagSet()
	fs.SetOutput(io.Discard) // errors are returned, not printed by the loader

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	fc, err := readConfigFile(configPath)
	if err != nil {
		return Config{}, err
	}

	fProvider, fModel, fModelProxyURL := f.provider, f.model, f.modelProxyURL
	fModelProxyAPIKey, fMCPProxyAPIKey := f.modelProxyAPIKey, f.mcpProxyAPIKey
	fSystemPrompt, fNoEnv := f.systemPrompt, f.noEnv
	fResume, fSession := f.resume, f.session
	fMaxTurns, fDefaultContextWindow, fContextWindow := f.maxTurns, f.defaultContextWindow, f.contextWindow
	fReasoningEffort, fReasoningEnabled, fReasoningBudgetTokens, fReasoningSummary := f.reasoningEffort, f.reasoningEnabled, f.reasoningBudgetTokens, f.reasoningSummary
	fImageDetail, fSearchTools := f.imageDetail, f.searchTools
	fPrompt, fInitialPrompt, fReplPrompt, fReplEditMode, fOutputFormat := f.prompt, f.initialPrompt, f.replPrompt, f.replEditMode, f.outputFormat
	fVerbose, fToolStream, fShowDiffs, fNoColor := f.verbose, f.toolStream, f.showDiffs, f.noColor
	fTimestamps, fNoTimestamps := f.timestamps, f.noTimestamps

	// set records which flags were explicitly provided, so a flag only overrides
	// env/file when actually present (flag defaults must not beat lower sources).
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var c Config
	// Each resolution goes default -> file -> env -> flag, last writer wins.

	c.Model = resolveString(set["model"], *fModel,
		getenv("HARNESS_MODEL"), fc.Model, "")
	c.Provider = resolveString(set["provider"], *fProvider,
		getenv("HARNESS_PROVIDER"), fc.Provider, "")
	if provider, model, ok := SplitProviderModel(c.Model); ok {
		if c.Provider == "" {
			c.Provider = provider
		}
		c.Model = model
	}
	c.ModelProxyURL = resolveString(set["model-proxy-url"], *fModelProxyURL,
		getenv("HARNESS_MODEL_PROXY_URL"), fc.ModelProxyURL, "")
	c.ModelProxyAPIKey = resolveString(set["model-proxy-api-key"], *fModelProxyAPIKey,
		getenv("HARNESS_MODEL_PROXY_API_KEY"), fc.ModelProxyAPIKey, "")
	c.SystemPrompt = resolveString(set["system-prompt"], *fSystemPrompt,
		getenv("HARNESS_SYSTEM_PROMPT"), fc.SystemPrompt, "")
	if set["resume"] {
		c.Resume = *fResume
	} else {
		c.Resume = getenv("HARNESS_RESUME")
	}
	if set["session"] {
		c.Session = *fSession
	} else {
		c.Session = getenv("HARNESS_SESSION")
	}

	// REPL history (bash-style). HistFile defaults to empty so main can derive
	// the path from stateDir; the size caps follow flags > env > file > defaults.
	if set["histfile"] {
		c.HistFile = *f.histFile
	} else if env := getenv("HARNESS_HISTFILE"); env != "" {
		c.HistFile = env
	} else {
		c.HistFile = fc.HistFile
	}
	c.HistFileSize = resolveInt(set["histfilesize"], *f.histFileSize,
		getenv("HARNESS_HISTFILESIZE"), fc.HistFileSize, DefaultHistFileSize)
	c.HistSize = resolveInt(set["histsize"], *f.histSize,
		getenv("HARNESS_HISTSIZE"), fc.HistSize, DefaultHistSize)

	c.MaxTurns = resolveInt(set["max-turns"], *fMaxTurns,
		getenv("HARNESS_MAX_TURNS"), fc.MaxTurns, defaultMaxTurns)
	c.MaxTurnTokens = resolveInt(set["max-turn-tokens"], *f.maxTurnTokens,
		getenv("HARNESS_MAX_TURN_TOKENS"), fc.MaxTurnTokens, 0)
	c.MaxOutputTokens = resolveInt(set["max-output-tokens"], *f.maxOutputTokens,
		getenv("HARNESS_MAX_OUTPUT_TOKENS"), fc.MaxOutputTokens, 0)
	c.MaxPromptCostUSD = resolveFloat(set["max-prompt-cost"], *f.maxPromptCost,
		getenv("HARNESS_MAX_PROMPT_COST"), fc.MaxPromptCostUSD, 0)
	c.ToolTimeoutSeconds = resolveInt(set["tool-timeout"], *f.toolTimeout,
		getenv("HARNESS_TOOL_TIMEOUT"), fc.ToolTimeoutSeconds, defaultToolTimeoutSeconds)
	c.DefaultContextWindow = resolveInt(set["default-context-window"], *fDefaultContextWindow,
		getenv("HARNESS_DEFAULT_CONTEXT_WINDOW"), fc.DefaultContextWindow, defaultContextWindow)
	c.ContextWindow = resolveInt(set["context-window"], *fContextWindow,
		getenv("HARNESS_CONTEXT_WINDOW"), fc.ContextWindow, 0)
	c.ReasoningEffort = strings.ToLower(strings.TrimSpace(resolveString(set["reasoning-effort"], *fReasoningEffort,
		getenv("HARNESS_REASONING_EFFORT"), fc.ReasoningEffort, "")))
	c.ReasoningEnabled = resolveBoolPtr(set["reasoning-enabled"], *fReasoningEnabled,
		getenv("HARNESS_REASONING_ENABLED"), fc.ReasoningEnabled)
	c.ReasoningBudgetTokens = resolveIntPtr(set["reasoning-budget-tokens"], *fReasoningBudgetTokens,
		getenv("HARNESS_REASONING_BUDGET_TOKENS"), fc.ReasoningBudgetTokens)
	c.ReasoningSummary, err = canonicalReasoningSummary(resolveString(set["reasoning-summary"], *fReasoningSummary,
		getenv("HARNESS_REASONING_SUMMARY"), fc.ReasoningSummary, ""))
	if err != nil {
		return Config{}, err
	}
	c.ImageDetail, err = canonicalImageDetail(resolveString(set["image-detail"], *fImageDetail,
		getenv("HARNESS_IMAGE_DETAIL"), fc.ImageDetail, "auto"))
	if err != nil {
		return Config{}, err
	}
	c.SearchTools, err = canonicalSearchTools(resolveString(set["search-tools"], *fSearchTools,
		getenv("HARNESS_SEARCH_TOOLS"), fc.SearchTools, "auto"))
	if err != nil {
		return Config{}, err
	}
	if set["image"] {
		for _, spec := range *f.images {
			att, err := parseImageAttachment(spec, c.ImageDetail)
			if err != nil {
				return Config{}, err
			}
			c.Images = append(c.Images, att)
		}
	}
	c.AgentsMDWarnBytes = intValue(fc.AgentsMDWarnBytes, 8192)
	c.ToolResultMaxBytes = resolveInt(false, 0,
		getenv("HARNESS_TOOL_RESULT_MAX_BYTES"), fc.ToolResultMaxBytes, 0)
	c.ToolResultMaxLines = resolveInt(false, 0,
		getenv("HARNESS_TOOL_RESULT_MAX_LINES"), fc.ToolResultMaxLines, 0)
	c.ReadFileDefaultLimit = intValue(fc.ReadFileDefaultLimit, 0)
	c.CompactKeepTurns = intValue(fc.CompactKeepTurns, 0)
	c.CompactSummaryMaxTokens = intValue(fc.CompactSummaryMaxTokens, 0)
	c.CompactToolResultMaxBytes = intValue(fc.CompactToolResultMaxBytes, 0)
	c.DelegateMaxTurns = intValue(fc.DelegateMaxTurns, defaultDelegateMaxTurns)
	if c.DelegateMaxTurns <= 0 {
		return Config{}, fmt.Errorf("delegate_max_turns must be positive")
	}
	c.ResponsesStateful = resolveBool(set["responses-stateful"], *f.responsesStateful,
		getenv("HARNESS_RESPONSES_STATEFUL"), fc.ResponsesStateful, true)
	c.Agent = strings.ToLower(strings.TrimSpace(resolveString(set["agent"], *f.agent,
		getenv("HARNESS_AGENT"), fc.Agent, "")))
	c.HandoffAgent = strings.ToLower(strings.TrimSpace(resolveString(set["handoff-agent"], *f.handoffAgent,
		getenv("HARNESS_HANDOFF_AGENT"), fc.HandoffAgent, "auto")))
	c.Agents = fc.Agents

	c.NoEnv = resolveBool(set["no-env"], *fNoEnv,
		getenv("HARNESS_NO_ENV"), fc.NoEnv, false)
	c.Verbose = resolveBool(set["v"], *fVerbose,
		getenv("HARNESS_VERBOSE"), fc.Verbose, false)
	c.ToolStream = resolveBool(set["tool-stream"], *fToolStream,
		getenv("HARNESS_TOOL_STREAM"), fc.ToolStream, true)
	c.ShowDiffs = resolveBool(set["show-diffs"], *fShowDiffs,
		getenv("HARNESS_SHOW_DIFFS"), fc.ShowDiffs, true)
	c.Quiet = *f.quietShort || *f.quiet
	logLevel := resolveString(set["log-level"], *f.logLevel,
		getenv("LOG_LEVEL"), fc.LogLevel, logging.LevelInfo)
	canonicalLogLevel, err := logging.CanonicalLevel(logLevel)
	if err != nil {
		return Config{}, err
	}
	c.LogLevel = canonicalLogLevel
	c.NoColor = resolveBool(set["no-color"], *fNoColor,
		getenv("HARNESS_NO_COLOR"), fc.NoColor, false)
	c.TimestampMode, err = resolveTimestampMode(timestampInputs{
		flagSet:      set["timestamps"],
		flagValue:    *fTimestamps,
		noFlagSet:    set["no-timestamps"],
		noFlagValue:  *fNoTimestamps,
		envValue:     getenv("HARNESS_TIMESTAMPS"),
		noEnvValue:   getenv("HARNESS_NO_TIMESTAMPS"),
		fileValue:    fc.Timestamps,
		noFileValue:  fc.NoTimestamps,
		defaultValue: TimestampShort,
	})
	if err != nil {
		return Config{}, err
	}
	c.ReplPrompt = resolveString(set["repl-prompt"], *fReplPrompt,
		getenv("HARNESS_REPL_PROMPT"), fc.ReplPrompt, replprompt.DefaultFormat)
	if c.ReplPrompt == "" {
		c.ReplPrompt = replprompt.DefaultFormat
	}
	if err := replprompt.Validate(c.ReplPrompt); err != nil {
		return Config{}, fmt.Errorf("repl_prompt: %w", err)
	}
	c.ReplEditMode, err = canonicalReplEditMode(resolveString(set["repl-edit-mode"], *fReplEditMode,
		getenv("HARNESS_REPL_EDIT_MODE"), fc.ReplEditMode, DefaultReplEditMode))
	if err != nil {
		return Config{}, err
	}
	c.OutputFormat = strings.ToLower(strings.TrimSpace(*fOutputFormat))
	switch c.OutputFormat {
	case "", "text":
		c.OutputFormat = "text"
	case "json":
	default:
		return Config{}, fmt.Errorf("-format must be text or json")
	}

	// MCP block (env > file > default; no flags). Proxy is left empty when
	// unset so main can resolve the shared default HTTP URL at connect time.
	var mcpEnableFile *bool
	var mcpProxyFile string
	var mcpAPIKeyFile string
	var mcpLocalEnableFile *bool
	if fc.MCP != nil {
		mcpEnableFile = fc.MCP.Enable
		mcpProxyFile = fc.MCP.Proxy
		mcpAPIKeyFile = fc.MCP.APIKey
		// Headers are config-file-only (no env layer); copy so a later mutation
		// of fc cannot reach the resolved Config. Values support ${VAR} and
		// ${VAR:-default} interpolation. Absent → nil.
		if len(fc.MCP.Headers) > 0 {
			headers, err := expandMCPHeaders(fc.MCP.Headers, getenv)
			if err != nil {
				return Config{}, err
			}
			c.MCP.Headers = headers
		}
		// MaxTools and DisabledServers are config-file-only structured scoping
		// data (no env layer), matching the Headers precedent.
		if fc.MCP.MaxTools != nil {
			if *fc.MCP.MaxTools < 0 {
				return Config{}, fmt.Errorf("mcp.max_tools must not be negative")
			}
			c.MCP.MaxTools = *fc.MCP.MaxTools
		}
		if len(fc.MCP.DisabledServers) > 0 {
			c.MCP.DisabledServers = append([]string(nil), fc.MCP.DisabledServers...)
		}
		// Local block: Command/Args are config-file-only; Env supports ${VAR}
		// interpolation (reusing the headers expander).
		if fc.MCP.Local != nil {
			mcpLocalEnableFile = fc.MCP.Local.Enable
			c.MCP.Local.Command = fc.MCP.Local.Command
			c.MCP.Local.Args = fc.MCP.Local.Args
			if len(fc.MCP.Local.Env) > 0 {
				env, err := expandMCPHeaders(fc.MCP.Local.Env, getenv)
				if err != nil {
					return Config{}, err
				}
				c.MCP.Local.Env = env
			}
		}
	}
	c.MCP.Enable = resolveBool(false, false,
		getenv("HARNESS_MCP_ENABLE"), mcpEnableFile, false)
	c.MCP.Proxy = resolveString(false, "",
		getenv("HARNESS_MCP_PROXY"), mcpProxyFile, "")
	c.MCP.APIKey = resolveString(set["mcp-proxy-api-key"], *fMCPProxyAPIKey,
		getenv("HARNESS_MCP_PROXY_API_KEY"), mcpAPIKeyFile, "")
	localEnableEnv := getenv("HARNESS_MCP_LOCAL_ENABLE")
	c.MCP.Local.EnableSet = localEnableEnv != "" || mcpLocalEnableFile != nil
	c.MCP.Local.Enable = resolveBool(false, false,
		localEnableEnv, mcpLocalEnableFile, false)

	// LSP block (env > file > default for enable; servers are config-file-only).
	var lspEnableFile *bool
	if fc.LSP != nil {
		lspEnableFile = fc.LSP.Enable
		if len(fc.LSP.Tools) > 0 {
			c.LSP.Tools = append([]string(nil), fc.LSP.Tools...)
		}
		if len(fc.LSP.Servers) > 0 {
			c.LSP.Servers = cloneLSPServers(fc.LSP.Servers)
		}
	}
	c.LSP.Enable = resolveBool(false, false,
		getenv("HARNESS_LSP_ENABLE"), lspEnableFile, false)

	if set["hooks"] {
		if strings.TrimSpace(*f.hooks) == "" {
			return Config{}, fmt.Errorf("--hooks requires a path")
		}
		hooksCfg, err := hooks.LoadFile("", *f.hooks)
		if err != nil {
			return Config{}, fmt.Errorf("--hooks: %w", err)
		}
		c.Hooks = hooksCfg
	} else {
		baseDir := ""
		if configPath != "" {
			baseDir = filepath.Dir(configPath)
		}
		if len(fc.Hooks) > 0 {
			inline, err := hooks.DecodeEventMap(fc.Hooks)
			if err != nil {
				return Config{}, fmt.Errorf("hooks: %w", err)
			}
			c.Hooks.Append(inline)
		}
		if len(fc.HookConfigs) > 0 {
			external, err := hooks.LoadFiles(baseDir, fc.HookConfigs)
			if err != nil {
				return Config{}, fmt.Errorf("hook_configs: %w", err)
			}
			c.Hooks.Append(external)
			c.HookConfigs = append([]string(nil), fc.HookConfigs...)
		}
	}

	// NO_COLOR (the de-facto standard) disables color regardless of HARNESS_*.
	if getenv("NO_COLOR") != "" {
		c.NoColor = true
	}

	if set["p"] {
		c.Prompt = *fPrompt
		c.PromptSet = true
	}
	initialPromptSet := set["i"] || set["initial-prompt"]
	if c.PromptSet && initialPromptSet {
		return Config{}, fmt.Errorf("-p cannot be combined with -i or -initial-prompt")
	}
	if initialPromptSet {
		if *fInitialPrompt == "-" {
			return Config{}, fmt.Errorf("-i does not read from stdin; pass prompt text directly")
		}
		c.InitialPrompt = *fInitialPrompt
		c.InitialPromptSet = true
	}
	c.ShowConfig = set["show-config"]
	c.DebugRequest = set["debug-request"]
	c.ShowAgents = set["agents"]
	c.ShowModels = set["models"]
	c.CheckModelProxy = set["check-model-proxy"]

	return c, nil
}

// WriteResolved writes v as indented JSON. It intentionally includes zero values
// so `--show-config` shows defaults and unset fields alike.
func WriteResolved(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func expandMCPHeaders(headers map[string]string, getenv func(string) string) (map[string]string, error) {
	out := maps.Clone(headers)
	for k, v := range out {
		expanded, err := expandMCPHeaderValue(v, getenv)
		if err != nil {
			return nil, fmt.Errorf("mcp.headers.%s: %w", k, err)
		}
		out[k] = expanded
	}
	return out, nil
}

func cloneLSPServers(in map[string]LSPServerConfig) map[string]LSPServerConfig {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]LSPServerConfig, len(in))
	for name, s := range in {
		out[name] = LSPServerConfig{
			Languages:   append([]string(nil), s.Languages...),
			RootMarkers: append([]string(nil), s.RootMarkers...),
			Command:     append([]string(nil), s.Command...),
			Extensions:  append([]string(nil), s.Extensions...),
			Env:         maps.Clone(s.Env),
			InitOptions: append([]byte(nil), s.InitOptions...),
		}
	}
	return out
}

func expandMCPHeaderValue(s string, getenv func(string) string) (string, error) {
	if !strings.ContainsRune(s, '$') {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		ref, ok := parseMCPHeaderVarRef(s, i)
		if !ok {
			b.WriteByte('$')
			i++
			continue
		}
		if val := getenv(ref.name); val != "" {
			b.WriteString(val)
		} else if ref.hasDefault {
			b.WriteString(ref.def)
		} else {
			return "", fmt.Errorf("references unset variable ${%s}", ref.name)
		}
		i = ref.end
	}
	return b.String(), nil
}

type mcpHeaderVarRef struct {
	name       string
	def        string
	hasDefault bool
	end        int
}

func parseMCPHeaderVarRef(s string, i int) (mcpHeaderVarRef, bool) {
	if i+1 >= len(s) || s[i+1] != '{' {
		return mcpHeaderVarRef{}, false
	}
	j := i + 2
	start := j
	for j < len(s) && s[j] != '}' {
		j++
	}
	if j >= len(s) {
		return mcpHeaderVarRef{}, false
	}
	body := s[start:j]
	name, def, hasDefault := strings.Cut(body, ":-")
	if !isMCPHeaderVarName(name) {
		return mcpHeaderVarRef{}, false
	}
	return mcpHeaderVarRef{name: name, def: def, hasDefault: hasDefault, end: j + 1}, true
}

func isMCPHeaderVarName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type imageFlags []string

func (f *imageFlags) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *imageFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func canonicalImageDetail(detail string) (string, error) {
	detail = strings.ToLower(strings.TrimSpace(detail))
	if detail == "" {
		return "auto", nil
	}
	switch detail {
	case "auto", "low", "high", "original":
		return detail, nil
	default:
		return "", fmt.Errorf("invalid image_detail %q (want auto, low, high, or original)", detail)
	}
}

func canonicalReasoningSummary(summary string) (string, error) {
	summary = strings.ToLower(strings.TrimSpace(summary))
	switch summary {
	case "", "default", "provider-default":
		return "", nil
	case "auto", "concise", "detailed":
		return summary, nil
	case "none", "off", "false", "disabled", "disable":
		return "none", nil
	case "on", "true", "enabled", "enable":
		return "auto", nil
	default:
		return "", fmt.Errorf("invalid reasoning_summary %q (want auto, concise, detailed, or none)", summary)
	}
}

func canonicalSearchTools(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", "auto":
		return "auto", nil
	case "grep":
		return "grep", nil
	case "rg", "ripgrep":
		return "rg", nil
	case "both":
		return "both", nil
	default:
		return "", fmt.Errorf("invalid search_tools %q (want auto, grep, rg, or both)", mode)
	}
}

func canonicalReplEditMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", DefaultReplEditMode:
		return DefaultReplEditMode, nil
	case "vi", "vim":
		return "vi", nil
	default:
		return "", fmt.Errorf("invalid repl_edit_mode %q (want emacs or vi)", mode)
	}
}

func parseImageAttachment(spec, defaultDetail string) (ImageAttachment, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ImageAttachment{}, fmt.Errorf("-image requires a path")
	}
	detail, err := canonicalImageDetail(defaultDetail)
	if err != nil {
		return ImageAttachment{}, err
	}
	if before, after, ok := strings.Cut(spec, ":"); ok && after != "" {
		if parsed, err := canonicalImageDetail(before); err == nil {
			detail = parsed
			spec = after
		}
	}
	return ImageAttachment{Path: spec, Detail: detail}, nil
}

// flags holds the pointers returned by the FlagSet so the same flag definitions
// back both Load (parsing) and Usage (the -h screen) — one source of truth, so
// the help can never drift from what is actually parsed (design §10).
type flags struct {
	provider, model, modelProxyURL   *string
	modelProxyAPIKey, mcpProxyAPIKey *string
	systemPrompt                     *string
	noEnv                            *bool
	resume, session                  *string
	histFile                         *string
	histFileSize, histSize           *int
	maxTurns                         *int
	maxTurnTokens                    *int
	maxOutputTokens                  *int
	maxPromptCost                    *float64
	toolTimeout                      *int
	defaultContextWindow             *int
	contextWindow                    *int
	reasoningEffort                  *string
	reasoningEnabled                 *bool
	reasoningBudgetTokens            *int
	reasoningSummary                 *string
	imageDetail                      *string
	searchTools                      *string
	images                           *imageFlags
	agent                            *string
	handoffAgent                     *string
	prompt                           *string
	initialPrompt                    *string
	replPrompt                       *string
	replEditMode                     *string
	logLevel                         *string
	timestamps                       *string
	verbose, toolStream              *bool
	showDiffs                        *bool
	responsesStateful                *bool
	noColor                          *bool
	noTimestamps                     *bool
	quietShort, quiet                *bool
	outputFormat                     *string
	config                           *string
	hooks                            *string
	showConfig, showAgents           *bool
	debugRequest                     *bool
	showModels                       *bool
	checkModelProxy                  *bool
}

// newFlagSet defines every design §10 flag on a fresh FlagSet, used by both Load
// and Usage so the help screen lists exactly the flags that are parsed.
func newFlagSet() (*flag.FlagSet, flags) {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	var f flags
	imageVals := imageFlags{}
	initialPrompt := ""
	f.prompt = fs.String("p", "", "one-shot prompt; \"-\" or piped stdin reads the prompt from stdin")
	f.initialPrompt = &initialPrompt
	fs.StringVar(f.initialPrompt, "i", "", "initial interactive prompt; run it first, then continue in the REPL")
	fs.StringVar(f.initialPrompt, "initial-prompt", "", "initial interactive prompt; run it first, then continue in the REPL")
	f.provider = fs.String("provider", "", "model proxy provider id")
	f.model = fs.String("model", "", "model id")
	f.modelProxyURL = fs.String("model-proxy-url", "", "harness-model-proxy URL")
	f.modelProxyAPIKey = fs.String("model-proxy-api-key", "", "API key for harness-model-proxy (also HARNESS_MODEL_PROXY_API_KEY)")
	f.mcpProxyAPIKey = fs.String("mcp-proxy-api-key", "", "API key for harness-mcp-proxy (also HARNESS_MCP_PROXY_API_KEY)")
	f.systemPrompt = fs.String("system-prompt", "", "replace the static system prompt (text or @file)")
	f.noEnv = fs.Bool("no-env", false, "omit the environment context block")
	f.resume = fs.String("resume", "", "load a session transcript and continue")
	f.session = fs.String("session", "", "explicit session save path")
	f.histFile = fs.String("histfile", "", "REPL history file path")
	f.histFileSize = fs.Int("histfilesize", DefaultHistFileSize, "max REPL history entries stored on disk (0 disables)")
	f.histSize = fs.Int("histsize", DefaultHistSize, "max REPL history entries loaded into memory (0 disables)")
	f.maxTurns = fs.Int("max-turns", defaultMaxTurns, "model turns per user prompt; <=0 means unlimited")
	f.maxTurnTokens = fs.Int("max-turn-tokens", 0, "stop a user turn after this many accumulated tokens; 0 means unlimited")
	f.maxOutputTokens = fs.Int("max-output-tokens", 0, "per-model-turn output token cap; 0 uses the automatic cap")
	f.maxPromptCost = fs.Float64("max-prompt-cost", 0, "stop a user turn once its accumulated model cost reaches this many USD; 0 means unlimited (requires known model cost)")
	f.toolTimeout = fs.Int("tool-timeout", defaultToolTimeoutSeconds, "per-tool-call timeout backstop in seconds; <=0 disables (run_command's own timeout_seconds still applies)")
	f.defaultContextWindow = fs.Int("default-context-window", defaultContextWindow, "default context window for configured models without context metadata (tokens)")
	f.contextWindow = fs.Int("context-window", 0, "context window override (tokens)")
	f.reasoningEffort = fs.String("reasoning-effort", "", "reasoning/thinking effort (provider/model dependent)")
	f.reasoningEnabled = fs.Bool("reasoning-enabled", false, "explicitly enable or disable reasoning when supported")
	f.reasoningBudgetTokens = fs.Int("reasoning-budget-tokens", 0, "reasoning/thinking budget tokens when supported")
	f.reasoningSummary = fs.String("reasoning-summary", "", "Responses API reasoning summary: auto, concise, detailed, or none")
	f.imageDetail = fs.String("image-detail", "auto", "default image detail: auto, low, high, or original")
	f.images = &imageVals
	fs.Var(&imageVals, "image", "attach an image in one-shot mode; repeatable; optionally detail:path")
	f.agent = fs.String("agent", "", "agent: auto, plan, independent, or a config-defined agent (default auto)")
	f.handoffAgent = fs.String("handoff-agent", "", "agent a plan->implementation handoff switches to by default (default auto)")
	f.searchTools = fs.String("search-tools", "auto", "search tools to expose: auto, grep, rg, or both")
	f.responsesStateful = fs.Bool("responses-stateful", true, "enable OpenAI Responses previous_response_id continuation when supported")
	f.verbose = fs.Bool("v", false, "show tool result snippets")
	f.toolStream = fs.Bool("tool-stream", true, "show live tool-call progress")
	f.showDiffs = fs.Bool("show-diffs", true, "show per-tool-call file diffs for built-in file edits")
	f.quietShort = fs.Bool("q", false, "suppress status messages and reasoning output unless -reasoning-summary is set")
	f.quiet = fs.Bool("quiet", false, "suppress status messages and reasoning output unless -reasoning-summary is set")
	f.logLevel = fs.String("log-level", logging.LevelInfo, "diagnostic log level: debug, info, warn, error (also LOG_LEVEL)")
	f.noColor = fs.Bool("no-color", false, "disable color output")
	f.timestamps = fs.String("timestamps", TimestampShort, "bracketed status timestamps: short, full, long, or none")
	f.noTimestamps = fs.Bool("no-timestamps", false, "disable bracketed status timestamps")
	f.replPrompt = fs.String("repl-prompt", replprompt.DefaultFormat, "REPL input prompt format")
	f.replEditMode = fs.String("repl-edit-mode", DefaultReplEditMode, "REPL prompt edit mode: emacs or vi")
	f.outputFormat = fs.String("format", "text", "output format for informational commands: text or json")
	f.showConfig = fs.Bool("show-config", false, "dump resolved config including defaults and exit")
	f.debugRequest = fs.Bool("debug-request", false, "dump the first provider-neutral model request as JSON and exit without calling the model")
	f.showAgents = fs.Bool("agents", false, "list configured agents and exit")
	f.showModels = fs.Bool("models", false, "list configured providers and models and exit")
	f.checkModelProxy = fs.Bool("check-model-proxy", false, "check harness-model-proxy reachability and exit")
	f.hooks = fs.String("hooks", "", "override hook config file for this run")
	// -config is consumed by the caller before Load (it picks the file Load
	// reads); accepted here so it is not rejected as an unknown flag.
	f.config = fs.String("config", "", "alternate config path")
	return fs, f
}

// Usage writes the -h/--help screen: a one-line summary followed by every design
// §10 flag with its description and default. Provider secrets are configured on
// harness-model-proxy, not exposed through harness flags.
func Usage(w io.Writer) {
	fmt.Fprintln(w, "harness — a minimal agentic coding harness.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  harness [flags]            interactive REPL")
	fmt.Fprintln(w, "  harness -i \"prompt\" [flags]  run a first prompt, then continue in the REPL")
	fmt.Fprintln(w, "  harness -p \"prompt\" [flags]  one-shot: prints the assistant's answer to stdout")
	fmt.Fprintln(w, "  harness session replay <session-dir>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Model provider access goes through harness-model-proxy.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fs, _ := newFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// readConfigFile reads and decodes the config file at path. An empty path means
// "no config"; a missing or malformed file at a non-empty path is an error (the
// path was requested, so silently ignoring it would hide a typo).
func readConfigFile(path string) (fileConfig, error) {
	if path == "" {
		return fileConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, err
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return fileConfig{}, err
	}
	normalizeConfigAtFileRefs(&fc, filepath.Dir(path))
	return fc, nil
}

func normalizeConfigAtFileRefs(fc *fileConfig, baseDir string) {
	fc.SystemPrompt = normalizeConfigAtFileRef(fc.SystemPrompt, baseDir)
	for name, a := range fc.Agents {
		a.Prompt = normalizeConfigAtFileRef(a.Prompt, baseDir)
		fc.Agents[name] = a
	}
}

func normalizeConfigAtFileRef(v, baseDir string) string {
	if baseDir == "" || !strings.HasPrefix(v, "@") || strings.HasPrefix(v, "@@") {
		return v
	}
	path := v[1:]
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
		return v
	}
	return "@" + filepath.Join(baseDir, path)
}

// SaveSelectedModel writes provider/model/reasoning settings into the harness
// config file, preserving any existing top-level keys. Missing files are
// created atomically.
func SaveSelectedModel(path, provider, model, reasoningEffort string, reasoningEnabled *bool, reasoningBudgetTokens *int) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config path is required")
	}
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
	}
	raw["provider"], err = json.Marshal(provider)
	if err != nil {
		return err
	}
	raw["model"], err = json.Marshal(model)
	if err != nil {
		return err
	}
	raw["reasoning_effort"], err = json.Marshal(strings.ToLower(strings.TrimSpace(reasoningEffort)))
	if err != nil {
		return err
	}
	raw["reasoning_enabled"], err = json.Marshal(reasoningEnabled)
	if err != nil {
		return err
	}
	raw["reasoning_budget_tokens"], err = json.Marshal(reasoningBudgetTokens)
	if err != nil {
		return err
	}
	return writeConfigFile(path, raw)
}

// SaveReplEditMode writes the repl_edit_mode setting into the harness config
// file, preserving any existing top-level keys. Missing files are created
// atomically. The mode is canonicalized to "emacs" or "vi".
func SaveReplEditMode(path, mode string) error {
	mode, err := canonicalReplEditMode(mode)
	if err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
	}
	modeJSON, err := json.Marshal(mode)
	if err != nil {
		return err
	}
	raw["repl_edit_mode"] = modeJSON
	return writeConfigFile(path, raw)
}

// writeConfigFile marshals raw to indented JSON and atomically replaces path.
func writeConfigFile(path string, raw map[string]json.RawMessage) error {
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// resolveString returns the highest-precedence non-empty value among an
// explicitly-set flag, an env value, a file value, and a default.
func resolveString(flagSet bool, flagVal, envVal, fileVal, def string) string {
	if flagSet && flagVal != "" {
		return flagVal
	}
	if envVal != "" {
		return envVal
	}
	if fileVal != "" {
		return fileVal
	}
	return def
}

// resolveInt mirrors resolveString for integers. fileVal of nil means unset.
func resolveInt(flagSet bool, flagVal int, envVal string, fileVal *int, def int) int {
	if flagSet {
		return flagVal
	}
	if envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil {
			return n
		}
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

// resolveFloat mirrors resolveInt for float64 values. fileVal of nil means unset.
func resolveFloat(flagSet bool, flagVal float64, envVal string, fileVal *float64, def float64) float64 {
	if flagSet {
		return flagVal
	}
	if envVal != "" {
		if n, err := strconv.ParseFloat(envVal, 64); err == nil {
			return n
		}
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

func resolveIntPtr(flagSet bool, flagVal int, envVal string, fileVal *int) *int {
	if flagSet {
		v := flagVal
		return &v
	}
	if envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil {
			return &n
		}
	}
	if fileVal != nil {
		v := *fileVal
		return &v
	}
	return nil
}

func intValue(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

type timestampInputs struct {
	flagSet      bool
	flagValue    string
	noFlagSet    bool
	noFlagValue  bool
	envValue     string
	noEnvValue   string
	fileValue    string
	noFileValue  *bool
	defaultValue string
}

func resolveTimestampMode(in timestampInputs) (string, error) {
	value := in.defaultValue
	if in.fileValue != "" {
		value = in.fileValue
	}
	if in.noFileValue != nil && *in.noFileValue {
		value = TimestampNone
	}
	if in.envValue != "" {
		value = in.envValue
	}
	if disabled, err := parseOptionalBool(in.noEnvValue); err != nil {
		return "", fmt.Errorf("HARNESS_NO_TIMESTAMPS: %w", err)
	} else if disabled {
		value = TimestampNone
	}
	if in.flagSet {
		value = in.flagValue
	}
	if in.noFlagSet && in.noFlagValue {
		value = TimestampNone
	}
	mode, err := NormalizeTimestampMode(value)
	if err != nil {
		return "", err
	}
	return mode, nil
}

func NormalizeTimestampMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", TimestampShort:
		return TimestampShort, nil
	case TimestampFull, "long":
		return TimestampFull, nil
	case TimestampNone, "off", "false", "disabled":
		return TimestampNone, nil
	default:
		return "", fmt.Errorf("timestamps must be short, full, long, or none")
	}
}

func parseOptionalBool(s string) (bool, error) {
	if strings.TrimSpace(s) == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, err
	}
	return b, nil
}

// resolveBool mirrors resolveString for booleans. fileVal of nil means unset.
func resolveBool(flagSet bool, flagVal bool, envVal string, fileVal *bool, def bool) bool {
	if flagSet {
		return flagVal
	}
	if envVal != "" {
		if b, err := strconv.ParseBool(envVal); err == nil {
			return b
		}
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

func resolveBoolPtr(flagSet bool, flagVal bool, envVal string, fileVal *bool) *bool {
	if flagSet {
		v := flagVal
		return &v
	}
	if envVal != "" {
		if b, err := strconv.ParseBool(envVal); err == nil {
			return &b
		}
	}
	if fileVal != nil {
		v := *fileVal
		return &v
	}
	return nil
}

// SplitProviderModel splits a "provider:model" string into its parts. The
// provider half must look like a provider name ([a-zA-Z0-9._-]); anything else
// (e.g. a model id with a colon in it) is returned as not-ok. Shared with the
// REPL /model switch in cmd/harness.
func SplitProviderModel(model string) (provider, bareModel string, ok bool) {
	provider, bareModel, ok = strings.Cut(strings.TrimSpace(model), ":")
	if !ok || provider == "" || bareModel == "" {
		return "", "", false
	}
	for _, r := range provider {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return "", "", false
		}
	}
	return strings.ToLower(provider), bareModel, true
}
