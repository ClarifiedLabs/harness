// Package config resolves harness settings from flags, environment variables,
// an optional project config file, an optional global config file, and defaults.
// Resolution is provider-neutral and follows flag > environment > project file > global file > default precedence.
// When --config or HARNESS_CONFIG is set explicitly, project discovery is disabled.
package config

import (
	"encoding/json"
	"fmt"

	"harness/internal/configmeta"
	"harness/internal/hooks"
)

const (
	DefaultSerenaCommand          = "serena"
	TimestampShort                = "short"
	TimestampFull                 = "full"
	TimestampNone                 = "none"
	ColorThemeDark                = "dark"
	ColorThemeLight               = "light"
	DefaultHistFileSize           = 1000
	DefaultHistSize               = 1000
	DefaultReplEditMode           = "emacs"
	DelegateOutputStatus          = "status"
	DelegateOutputOff             = "off"
	DelegateOutputLines           = "lines"
	DelegateTmuxLayoutPane        = "pane"
	DelegateTmuxLayoutWindow      = "window"
	defaultMaxTurns               = 0
	defaultContextWindow          = 256_000
	defaultGoalMaxContinuations   = 25
	defaultDelegateMaxTurns       = 20
	defaultDelegateMaxDepth       = 3
	defaultDelegateMaxActive      = 4
	defaultDelegateMaxDescendants = 16
	defaultDelegateTmuxMaxWindows = 4
	defaultToolTimeoutSeconds     = 600
	defaultCompactTriggerPercent  = 78
	defaultCompactTargetPercent   = 65
	defaultCompactIdleTrigger     = 35
	defaultCompactTimeoutSeconds  = 300
)

// RuntimeDefaults are contextual values owned by cmd/harness rather than this
// package. They are resolved like defaults and receive derived provenance.
type RuntimeDefaults struct {
	ModelProxyURL string
	MCPProxyURL   string
	HistoryPath   string
	Agent         string
	TmuxActive    bool
}

// LoadOptions contains all external inputs to one deterministic load.
type LoadOptions struct {
	Args              []string
	LookupEnv         func(string) (string, bool)
	DefaultConfigPath string
	Defaults          RuntimeDefaults
	WorkingDir        string // optional override for project-config discovery (tests); empty uses os.Getwd
}

// Result separates persistent/source-resolved settings from invocation-only
// controls. Config is embedded so callers can deliberately select result.Config
// while field promotion keeps inspection convenient.
type Result struct {
	Config            Config
	Run               RunOptions
	Sources           map[string]configmeta.Source
	ConfigPath        string // global or explicit file (never project)
	ProjectConfigPath string // discovered .harness/config.json, if any
	fileReferences    []configFileReference
}

type configFileReference struct {
	setting string
	value   string
}

// ValidateFileReferences validates every prompt reference supplied by the
// selected config file, including candidates hidden by environment or flags.
func (result Result) ValidateFileReferences(validate func(string) error) error {
	for _, reference := range result.fileReferences {
		if err := validate(reference.value); err != nil {
			return fmt.Errorf("%s: %w", reference.setting, err)
		}
	}
	return nil
}

// Config contains source-resolved user settings only.
type Config struct {
	Provider         string `json:"-"` // derived from a provider-qualified Model
	Model            string `json:"model"`
	ModelProxyURL    string `json:"model_proxy_url"`
	ModelProxyAPIKey string `json:"model_proxy_api_key"`
	TraceProxy       bool   `json:"trace_proxy"`
	SystemPrompt     string `json:"system_prompt"`
	NoEnv            bool   `json:"no_env"`

	HistFile     string `json:"histfile"`
	HistFileSize int    `json:"histfilesize"`
	HistSize     int    `json:"histsize"`

	MaxTurns                      int     `json:"max_turns"`
	MaxPromptTokens               int     `json:"max_prompt_tokens"`
	MaxOutputTokens               int     `json:"max_output_tokens"`
	MaxPromptCostUSD              float64 `json:"max_prompt_cost_usd"`
	GoalMaxContinuations          int     `json:"goal_max_continuations"`
	ToolTimeoutSeconds            int     `json:"tool_timeout_seconds"`
	ShellTimeoutSeconds           int     `json:"shell_timeout_seconds"`
	ShellBackgroundTimeoutSeconds int     `json:"shell_background_timeout_seconds"`
	DefaultContextWindow          int     `json:"default_context_window"`
	ContextWindow                 int     `json:"context_window"`
	Reasoning                     string  `json:"reasoning"`
	ReasoningSummary              string  `json:"reasoning_summary"`
	ImageDetail                   string  `json:"image_detail"`
	WebSearch                     string  `json:"web_search"`
	AgentsMDWarnBytes             int     `json:"agents_md_warn_bytes"`
	ToolResultMaxBytes            int     `json:"tool_result_max_bytes"`
	ToolResultMaxLines            int     `json:"tool_result_max_lines"`
	RGResultMaxBytes              int     `json:"rg_result_max_bytes"`
	RGResultMaxLines              int     `json:"rg_result_max_lines"`
	GrepResultMaxBytes            int     `json:"grep_result_max_bytes"`
	GrepResultMaxLines            int     `json:"grep_result_max_lines"`
	ReadFileDefaultLimit          int     `json:"read_file_default_limit"`
	ReadFileResultMaxBytes        int     `json:"read_file_result_max_bytes"`
	ReadFileResultMaxLines        int     `json:"read_file_result_max_lines"`
	CompactKeepTurns              int     `json:"compact_keep_turns"`
	CompactKeepTokens             int     `json:"compact_keep_tokens"`
	CompactAutoEnabled            bool    `json:"compact_auto_enabled"`
	CompactTriggerPercent         int     `json:"compact_trigger_percent"`
	CompactTargetPercent          int     `json:"compact_target_percent"`
	CompactIdleAfterSeconds       int     `json:"compact_idle_after_seconds"`
	CompactIdleTriggerPercent     int     `json:"compact_idle_trigger_percent"`
	CompactTimeoutSeconds         int     `json:"compact_timeout_seconds"`
	CompactSummaryMaxTokens       int     `json:"compact_summary_max_tokens"`
	CompactToolResultMaxBytes     int     `json:"compact_tool_result_max_bytes"`
	DelegateMaxTurns              int     `json:"delegate_max_turns"`
	DelegateMaxDepth              int     `json:"delegate_max_depth"`
	DelegateMaxActive             int     `json:"delegate_max_active"`
	DelegateMaxDescendants        int     `json:"delegate_max_descendants"`
	DelegateOutput                string  `json:"delegate_output"`
	DelegateTmux                  bool    `json:"delegate_tmux"`
	DelegateTmuxMaxWindows        int     `json:"delegate_tmux_max_windows"`
	DelegateTmuxLayout            string  `json:"delegate_tmux_layout"`
	ResponsesStateful             bool    `json:"responses_stateful"`
	RetentionPolicy               string  `json:"retention_policy"`
	RetentionFloorTokens          int     `json:"retention_floor_tokens"`
	NoSteer                       bool    `json:"no_steer"`

	Agent        string                     `json:"agent"`
	Agents       map[string]FileAgentConfig `json:"agents,omitempty"`
	HandoffAgent string                     `json:"handoff_agent"`

	Verbose       bool   `json:"verbose"`
	ToolStream    bool   `json:"tool_stream"`
	ShowDiffs     bool   `json:"show_diffs"`
	LogLevel      string `json:"log_level"`
	NoColor       bool   `json:"no_color"`
	ColorTheme    string `json:"color_theme"`
	TimestampMode string `json:"timestamps"`
	ReplPrompt    string `json:"repl_prompt"`
	ReplEditMode  string `json:"repl_edit_mode"`

	Hooks       hooks.Config `json:"hooks,omitempty"`
	HookConfigs []string     `json:"hook_configs,omitempty"`
	MCP         MCPConfig    `json:"mcp"`
	LSP         LSPConfig    `json:"lsp"`
	OTel        OTelConfig   `json:"otel"`
}

// RunOptions contains controls that apply only to this invocation.
type RunOptions struct {
	Help             bool
	Version          bool
	Resume           string
	Session          string
	Images           []ImageAttachment
	Prompt           string
	PromptSet        bool
	InitialPrompt    string
	InitialPromptSet bool
	Quiet            bool
	OutputFormat     string
	DebugRequest     bool
	ShowAgents       bool
	ShowModels       bool
	CheckModelProxy  bool
}

type MCPConfig struct {
	Enable          bool              `json:"enable"`
	Proxy           string            `json:"proxy"`
	APIKey          string            `json:"api_key"`
	Headers         map[string]string `json:"headers,omitempty"`
	MaxTools        int               `json:"max_tools"`
	DisabledServers []string          `json:"disabled_servers,omitempty"`
	Local           LocalMCPConfig    `json:"local"`
}

type LocalMCPConfig struct {
	Enable    bool              `json:"enable"`
	EnableSet bool              `json:"-"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type LSPConfig struct {
	Enable  bool                       `json:"enable"`
	Prewarm bool                       `json:"prewarm"`
	Tools   []string                   `json:"tools,omitempty"`
	Servers map[string]LSPServerConfig `json:"servers,omitempty"`
	Serena  SerenaConfig               `json:"serena"`
}

type SerenaConfig struct {
	Enable    bool              `json:"enable"`
	EnableSet bool              `json:"-"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type LSPServerConfig struct {
	Languages   []string          `json:"languages"`
	RootMarkers []string          `json:"root_markers"`
	Command     []string          `json:"command"`
	Extensions  []string          `json:"extensions"`
	Env         map[string]string `json:"env"`
	InitOptions json.RawMessage   `json:"initialization_options"`
}

type FileAgentConfig struct {
	Description     string   `json:"description"`
	AllowedTools    []string `json:"allowed_tools"`
	MCPTools        string   `json:"mcp_tools"`
	WorkspaceAccess string   `json:"workspace_access"`
	Prompt          string   `json:"prompt"`
	Model           string   `json:"model"`
	Reasoning       string   `json:"reasoning"`
}

type ImageAttachment struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

type OTelConfig struct {
	Enabled            bool              `json:"enabled"`
	Endpoint           string            `json:"endpoint"`
	Protocol           string            `json:"protocol"`
	TimeoutSeconds     int               `json:"timeout_seconds"`
	ServiceName        string            `json:"service_name"`
	Hostname           string            `json:"hostname"`
	HostnameSet        bool              `json:"-"` // false means the built-in default; true includes an explicitly empty value
	Headers            map[string]string `json:"headers,omitempty"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
}

func DefaultSerenaArgs() []string {
	return []string{"start-mcp-server", "--context=ide", "--project-from-cwd", "--open-web-dashboard", "False"}
}
