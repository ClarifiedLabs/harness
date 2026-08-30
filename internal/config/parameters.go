package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"harness/internal/configmeta"
	"harness/internal/hooks"
	"harness/internal/logging"
	"harness/internal/replprompt"
)

type defaultSpec[T any] struct {
	value func(RuntimeDefaults) T
	meta  configmeta.Default
	name  string
}

func literal[T any](value T, display, note string) defaultSpec[T] {
	return defaultSpec[T]{value: func(RuntimeDefaults) T { return value }, meta: configmeta.Default{Kind: configmeta.DefaultLiteral, Value: value, Display: display, Note: note}, name: "built-in"}
}

func derived[T any](value func(RuntimeDefaults) T, display, name string) defaultSpec[T] {
	return defaultSpec[T]{value: value, meta: configmeta.Default{Kind: configmeta.DefaultDerived, Display: display}, name: name}
}

type parameterDefinition interface {
	parameter() configmeta.Parameter
	register(*flagState)
	resolve(*resolveContext) error
	validateFile(fileConfig, string) error
	project(Config) any
}

type scalarDefinition[T any] struct {
	meta         configmeta.Parameter
	defaultValue defaultSpec[T]
	fileValue    func(fileConfig) optional[T]
	set          func(*Config, T)
	get          func(Config) T
	parse        func(string) (T, error)
	parseEnv     func(string, string) (T, error)
	skipEnv      func(string, string) bool
	normalize    func(T) (T, error)
	boolFlag     bool
	redact       func(T) any
}

func (definition scalarDefinition[T]) parameter() configmeta.Parameter { return definition.meta }
func (definition scalarDefinition[T]) register(state *flagState) {
	for _, name := range definition.meta.Flags {
		state.addSettingFlag(name, definition.meta.Key, definition.meta.Description, definition.boolFlag)
	}
}
func (definition scalarDefinition[T]) resolve(context *resolveContext) error {
	value := definition.defaultValue.value(context.options.Defaults)
	sourceKind := configmeta.SourceDefault
	if definition.defaultValue.meta.Kind == configmeta.DefaultDerived {
		sourceKind = configmeta.SourceDerived
	}
	source := configmeta.Source{Kind: sourceKind, Name: definition.defaultValue.name}
	if candidate := definition.fileValue(context.file); candidate.Set {
		resolved, err := definition.normalize(candidate.Value)
		if err != nil {
			return context.fileError(definition.meta.JSONPath, err)
		}
		value, source = resolved, context.fileSourceFor(definition.meta.Key)
	}
	for _, name := range definition.meta.Environment {
		if raw, present := context.lookup(name); present {
			if definition.skipEnv != nil && definition.skipEnv(name, raw) {
				continue
			}
			var parsed T
			var err error
			if definition.parseEnv != nil {
				parsed, err = definition.parseEnv(name, raw)
			} else {
				parsed, err = definition.parse(raw)
			}
			if err != nil {
				return fmt.Errorf("environment %s for %s: %w", name, definition.meta.Key, err)
			}
			resolved, err := definition.normalize(parsed)
			if err != nil {
				return fmt.Errorf("environment %s for %s: %w", name, definition.meta.Key, err)
			}
			value, source = resolved, configmeta.Source{Kind: configmeta.SourceEnvironment, Name: name}
		}
	}
	for _, occurrence := range context.flags.settings[definition.meta.Key] {
		parsed, err := definition.parse(occurrence.value)
		if err != nil {
			return fmt.Errorf("flag --%s for %s: %w", occurrence.name, definition.meta.Key, err)
		}
		resolved, err := definition.normalize(parsed)
		if err != nil {
			return fmt.Errorf("flag --%s for %s: %w", occurrence.name, definition.meta.Key, err)
		}
		value, source = resolved, configmeta.Source{Kind: configmeta.SourceFlag, Name: "--" + occurrence.name}
	}
	definition.set(&context.result.Config, value)
	context.result.Sources[definition.meta.Key] = source
	return nil
}
func (definition scalarDefinition[T]) validateFile(file fileConfig, path string) error {
	candidate := definition.fileValue(file)
	if !candidate.Set {
		return nil
	}
	if _, err := definition.normalize(candidate.Value); err != nil {
		return fmt.Errorf("config %q setting %s: %w", path, definition.meta.JSONPath, err)
	}
	return nil
}
func (definition scalarDefinition[T]) project(config Config) any {
	value := definition.get(config)
	if definition.redact != nil {
		return definition.redact(value)
	}
	if definition.meta.Sensitive {
		if text, ok := any(value).(string); ok && text == "" {
			return ""
		}
		return redactedValue
	}
	return value
}

func metadata(key, typ, jsonPath string, flags, env []string, def configmeta.Default, description string, accepted []string, sensitive bool) configmeta.Parameter {
	return configmeta.Parameter{Key: key, Type: typ, JSONPath: jsonPath, Flags: flags, Environment: env, Default: def, Description: description, Accepted: accepted, Sensitive: sensitive}
}

func strDef(key, jsonPath string, flags, env []string, def defaultSpec[string], file func(fileConfig) optional[string], set func(*Config, string), get func(Config) string, normalize func(string) (string, error), accepted []string, sensitive bool) scalarDefinition[string] {
	if normalize == nil {
		normalize = identity[string]
	}
	return scalarDefinition[string]{meta: metadata(key, "string", jsonPath, flags, env, def.meta, "Harness "+strings.ReplaceAll(key, "_", " ")+" setting.", accepted, sensitive), defaultValue: def, fileValue: file, set: set, get: get, parse: parseString, normalize: normalize}
}
func intDef(key, jsonPath string, flags, env []string, def defaultSpec[int], file func(fileConfig) optional[int], set func(*Config, int), get func(Config) int, normalize func(int) (int, error)) scalarDefinition[int] {
	if normalize == nil {
		normalize = identity[int]
	}
	return scalarDefinition[int]{meta: metadata(key, "integer", jsonPath, flags, env, def.meta, "Harness "+strings.ReplaceAll(key, "_", " ")+" setting.", nil, false), defaultValue: def, fileValue: file, set: set, get: get, parse: parseInt, normalize: normalize}
}
func floatDef(key, jsonPath string, flags, env []string, def defaultSpec[float64], file func(fileConfig) optional[float64], set func(*Config, float64), get func(Config) float64, normalize func(float64) (float64, error)) scalarDefinition[float64] {
	if normalize == nil {
		normalize = identity[float64]
	}
	return scalarDefinition[float64]{meta: metadata(key, "number", jsonPath, flags, env, def.meta, "Harness "+strings.ReplaceAll(key, "_", " ")+" setting.", nil, false), defaultValue: def, fileValue: file, set: set, get: get, parse: parseFloat, normalize: normalize}
}
func boolDef(key, jsonPath string, flags, env []string, def defaultSpec[bool], file func(fileConfig) optional[bool], set func(*Config, bool), get func(Config) bool) scalarDefinition[bool] {
	return scalarDefinition[bool]{meta: metadata(key, "boolean", jsonPath, flags, env, def.meta, "Harness "+strings.ReplaceAll(key, "_", " ")+" setting.", []string{"true", "false"}, false), defaultValue: def, fileValue: file, set: set, get: get, parse: parseBool, normalize: identity[bool], boolFlag: true}
}

var definitions = []parameterDefinition{
	strDef("model", "model", []string{"model"}, []string{"HARNESS_MODEL"}, literal("", "unset", "provider/model selected elsewhere"), func(f fileConfig) optional[string] { return f.Model }, func(c *Config, v string) { c.Model = v }, func(c Config) string {
		if c.Provider != "" {
			return c.Provider + ":" + c.Model
		}
		return c.Model
	}, nil, nil, false),
	strDef("model_proxy_url", "model_proxy_url", []string{"model-proxy-url"}, []string{"HARNESS_MODEL_PROXY_URL"}, derived(func(d RuntimeDefaults) string { return d.ModelProxyURL }, "runtime model proxy URL", "runtime:model-proxy-url"), func(f fileConfig) optional[string] { return f.ModelProxyURL }, func(c *Config, v string) { c.ModelProxyURL = v }, func(c Config) string { return c.ModelProxyURL }, canonicalNonEmptyFor("model_proxy_url"), nil, false),
	strDef("model_proxy_api_key", "model_proxy_api_key", []string{"model-proxy-api-key"}, []string{"HARNESS_MODEL_PROXY_API_KEY"}, literal("", "unset", ""), func(f fileConfig) optional[string] { return f.ModelProxyAPIKey }, func(c *Config, v string) { c.ModelProxyAPIKey = v }, func(c Config) string { return c.ModelProxyAPIKey }, nil, nil, true),
	boolDef("trace_proxy", "trace_proxy", []string{"trace-proxy"}, []string{"HARNESS_TRACE_PROXY"}, literal(false, "", ""), func(f fileConfig) optional[bool] { return f.TraceProxy }, func(c *Config, v bool) { c.TraceProxy = v }, func(c Config) bool { return c.TraceProxy }),
	strDef("system_prompt", "system_prompt", []string{"system-prompt"}, []string{"HARNESS_SYSTEM_PROMPT"}, literal("", "unset", ""), func(f fileConfig) optional[string] { return f.SystemPrompt }, func(c *Config, v string) { c.SystemPrompt = v }, func(c Config) string { return c.SystemPrompt }, nil, nil, false),
	boolDef("no_env", "no_env", []string{"no-env"}, []string{"HARNESS_NO_ENV"}, literal(false, "", ""), func(f fileConfig) optional[bool] { return f.NoEnv }, func(c *Config, v bool) { c.NoEnv = v }, func(c Config) bool { return c.NoEnv }),
	strDef("histfile", "histfile", []string{"histfile"}, []string{"HARNESS_HISTFILE"}, derived(func(d RuntimeDefaults) string { return d.HistoryPath }, "runtime history path", "runtime:history-path"), func(f fileConfig) optional[string] { return f.HistFile }, func(c *Config, v string) { c.HistFile = v }, func(c Config) string { return c.HistFile }, canonicalNonEmptyFor("histfile"), nil, false),
	intDef("histfilesize", "histfilesize", []string{"histfilesize"}, []string{"HARNESS_HISTFILESIZE"}, literal(DefaultHistFileSize, "", "disk entry cap"), func(f fileConfig) optional[int] { return f.HistFileSize }, func(c *Config, v int) { c.HistFileSize = v }, func(c Config) int { return c.HistFileSize }, nonNegative("histfilesize")),
	intDef("histsize", "histsize", []string{"histsize"}, []string{"HARNESS_HISTSIZE"}, literal(DefaultHistSize, "", "memory entry cap"), func(f fileConfig) optional[int] { return f.HistSize }, func(c *Config, v int) { c.HistSize = v }, func(c Config) int { return c.HistSize }, nonNegative("histsize")),
	intDef("max_turns", "max_turns", []string{"max-turns"}, []string{"HARNESS_MAX_TURNS"}, literal(defaultMaxTurns, "", "non-positive means unlimited"), func(f fileConfig) optional[int] { return f.MaxTurns }, func(c *Config, v int) { c.MaxTurns = v }, func(c Config) int { return c.MaxTurns }, nil),
	intDef("max_prompt_tokens", "max_prompt_tokens", []string{"max-prompt-tokens"}, []string{"HARNESS_MAX_PROMPT_TOKENS"}, literal(0, "", "unlimited"), func(f fileConfig) optional[int] { return f.MaxPromptTokens }, func(c *Config, v int) { c.MaxPromptTokens = v }, func(c Config) int { return c.MaxPromptTokens }, nonNegative("max_prompt_tokens")),
	intDef("max_output_tokens", "max_output_tokens", []string{"max-output-tokens"}, []string{"HARNESS_MAX_OUTPUT_TOKENS"}, literal(0, "", "automatic"), func(f fileConfig) optional[int] { return f.MaxOutputTokens }, func(c *Config, v int) { c.MaxOutputTokens = v }, func(c Config) int { return c.MaxOutputTokens }, nonNegative("max_output_tokens")),
	floatDef("max_prompt_cost_usd", "max_prompt_cost_usd", []string{"max-prompt-cost"}, []string{"HARNESS_MAX_PROMPT_COST"}, literal(0.0, "", "unlimited"), func(f fileConfig) optional[float64] { return f.MaxPromptCostUSD }, func(c *Config, v float64) { c.MaxPromptCostUSD = v }, func(c Config) float64 { return c.MaxPromptCostUSD }, func(v float64) (float64, error) {
		if v < 0 {
			return 0, fmt.Errorf("max_prompt_cost_usd must be non-negative")
		}
		return v, nil
	}),
	intDef("goal_max_continuations", "goal_max_continuations", []string{"goal-max-continuations"}, []string{"HARNESS_GOAL_MAX_CONTINUATIONS"}, literal(defaultGoalMaxContinuations, "", "zero means unlimited"), func(f fileConfig) optional[int] { return f.GoalMaxContinuations }, func(c *Config, v int) { c.GoalMaxContinuations = v }, func(c Config) int { return c.GoalMaxContinuations }, nonNegative("goal_max_continuations")),
	intDef("tool_timeout_seconds", "tool_timeout_seconds", []string{"tool-timeout"}, []string{"HARNESS_TOOL_TIMEOUT"}, literal(defaultToolTimeoutSeconds, "", "non-positive disables"), func(f fileConfig) optional[int] { return f.ToolTimeoutSeconds }, func(c *Config, v int) { c.ToolTimeoutSeconds = v }, func(c Config) int { return c.ToolTimeoutSeconds }, nil),
	intDef("shell_timeout_seconds", "shell_timeout_seconds", nil, []string{"HARNESS_SHELL_TIMEOUT_SECONDS"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ShellTimeoutSeconds }, func(c *Config, v int) { c.ShellTimeoutSeconds = v }, func(c Config) int { return c.ShellTimeoutSeconds }, nonNegative("shell_timeout_seconds")),
	intDef("shell_background_timeout_seconds", "shell_background_timeout_seconds", nil, []string{"HARNESS_SHELL_BACKGROUND_TIMEOUT_SECONDS"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ShellBackgroundTimeoutSeconds }, func(c *Config, v int) { c.ShellBackgroundTimeoutSeconds = v }, func(c Config) int { return c.ShellBackgroundTimeoutSeconds }, nonNegative("shell_background_timeout_seconds")),
	intDef("default_context_window", "default_context_window", []string{"default-context-window"}, []string{"HARNESS_DEFAULT_CONTEXT_WINDOW"}, literal(defaultContextWindow, "", "tokens"), func(f fileConfig) optional[int] { return f.DefaultContextWindow }, func(c *Config, v int) { c.DefaultContextWindow = v }, func(c Config) int { return c.DefaultContextWindow }, positive("default_context_window")),
	intDef("context_window", "context_window", []string{"context-window"}, []string{"HARNESS_CONTEXT_WINDOW"}, literal(0, "", "no override"), func(f fileConfig) optional[int] { return f.ContextWindow }, func(c *Config, v int) { c.ContextWindow = v }, func(c Config) int { return c.ContextWindow }, nonNegative("context_window")),
	strDef("reasoning", "reasoning", []string{"reasoning"}, []string{"HARNESS_REASONING"}, literal("", "provider default", ""), func(f fileConfig) optional[string] { return f.Reasoning }, func(c *Config, v string) { c.Reasoning = v }, func(c Config) string { return c.Reasoning }, canonicalReasoning, []string{"default", "none", "minimal", "low", "medium", "high", "xhigh", "max"}, false),
	strDef("reasoning_summary", "reasoning_summary", []string{"reasoning-summary"}, []string{"HARNESS_REASONING_SUMMARY"}, literal("", "provider default", ""), func(f fileConfig) optional[string] { return f.ReasoningSummary }, func(c *Config, v string) { c.ReasoningSummary = v }, func(c Config) string { return c.ReasoningSummary }, canonicalReasoningSummary, []string{"auto", "concise", "detailed", "none"}, false),
	strDef("image_detail", "image_detail", []string{"image-detail"}, []string{"HARNESS_IMAGE_DETAIL"}, literal("auto", "", ""), func(f fileConfig) optional[string] { return f.ImageDetail }, func(c *Config, v string) { c.ImageDetail = v }, func(c Config) string { return c.ImageDetail }, canonicalImageDetail, []string{"auto", "low", "high", "original"}, false),
	strDef("web_search", "web_search", []string{"web-search"}, []string{"HARNESS_WEB_SEARCH"}, literal("off", "", ""), func(f fileConfig) optional[string] { return f.WebSearch }, func(c *Config, v string) { c.WebSearch = v }, func(c Config) string { return c.WebSearch }, canonicalWebSearch, []string{"off", "auto"}, false),
	intDef("agents_md_warn_bytes", "agents_md_warn_bytes", nil, nil, literal(8192, "", ""), func(f fileConfig) optional[int] { return f.AgentsMDWarnBytes }, func(c *Config, v int) { c.AgentsMDWarnBytes = v }, func(c Config) int { return c.AgentsMDWarnBytes }, nonNegative("agents_md_warn_bytes")),
	intDef("tool_result_max_bytes", "tool_result_max_bytes", nil, []string{"HARNESS_TOOL_RESULT_MAX_BYTES"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ToolResultMaxBytes }, func(c *Config, v int) { c.ToolResultMaxBytes = v }, func(c Config) int { return c.ToolResultMaxBytes }, nonNegative("tool_result_max_bytes")),
	intDef("tool_result_max_lines", "tool_result_max_lines", nil, []string{"HARNESS_TOOL_RESULT_MAX_LINES"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ToolResultMaxLines }, func(c *Config, v int) { c.ToolResultMaxLines = v }, func(c Config) int { return c.ToolResultMaxLines }, nonNegative("tool_result_max_lines")),
	intDef("read_default_limit", "read_default_limit", nil, []string{"HARNESS_READ_DEFAULT_LIMIT"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ReadDefaultLimit }, func(c *Config, v int) { c.ReadDefaultLimit = v }, func(c Config) int { return c.ReadDefaultLimit }, nonNegative("read_default_limit")),
	intDef("read_total_lines_max_bytes", "read_total_lines_max_bytes", nil, []string{"HARNESS_READ_TOTAL_LINES_MAX_BYTES"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ReadTotalLinesMaxBytes }, func(c *Config, v int) { c.ReadTotalLinesMaxBytes = v }, func(c Config) int { return c.ReadTotalLinesMaxBytes }, nonNegative("read_total_lines_max_bytes")),
	intDef("read_result_max_bytes", "read_result_max_bytes", nil, []string{"HARNESS_READ_RESULT_MAX_BYTES"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ReadResultMaxBytes }, func(c *Config, v int) { c.ReadResultMaxBytes = v }, func(c Config) int { return c.ReadResultMaxBytes }, nonNegative("read_result_max_bytes")),
	intDef("read_result_max_lines", "read_result_max_lines", nil, []string{"HARNESS_READ_RESULT_MAX_LINES"}, literal(0, "", "tool default"), func(f fileConfig) optional[int] { return f.ReadResultMaxLines }, func(c *Config, v int) { c.ReadResultMaxLines = v }, func(c Config) int { return c.ReadResultMaxLines }, nonNegative("read_result_max_lines")),
	intDef("compact_keep_turns", "compact_keep_turns", nil, nil, literal(0, "", "all retained"), func(f fileConfig) optional[int] { return f.CompactKeepTurns }, func(c *Config, v int) { c.CompactKeepTurns = v }, func(c Config) int { return c.CompactKeepTurns }, nonNegative("compact_keep_turns")),
	intDef("compact_keep_tokens", "compact_keep_tokens", nil, nil, literal(20000, "", ""), func(f fileConfig) optional[int] { return f.CompactKeepTokens }, func(c *Config, v int) { c.CompactKeepTokens = v }, func(c Config) int { return c.CompactKeepTokens }, positive("compact_keep_tokens")),
	boolDef("compact_auto_enabled", "compact_auto_enabled", nil, nil, literal(true, "", ""), func(f fileConfig) optional[bool] { return f.CompactAutoEnabled }, func(c *Config, v bool) { c.CompactAutoEnabled = v }, func(c Config) bool { return c.CompactAutoEnabled }),
	intDef("compact_trigger_percent", "compact_trigger_percent", nil, nil, literal(defaultCompactTriggerPercent, "", ""), func(f fileConfig) optional[int] { return f.CompactTriggerPercent }, func(c *Config, v int) { c.CompactTriggerPercent = v }, func(c Config) int { return c.CompactTriggerPercent }, percent("compact_trigger_percent")),
	intDef("compact_target_percent", "compact_target_percent", nil, nil, literal(defaultCompactTargetPercent, "", ""), func(f fileConfig) optional[int] { return f.CompactTargetPercent }, func(c *Config, v int) { c.CompactTargetPercent = v }, func(c Config) int { return c.CompactTargetPercent }, percent("compact_target_percent")),
	intDef("compact_idle_after_seconds", "compact_idle_after_seconds", nil, nil, literal(0, "", "disabled"), func(f fileConfig) optional[int] { return f.CompactIdleAfterSeconds }, func(c *Config, v int) { c.CompactIdleAfterSeconds = v }, func(c Config) int { return c.CompactIdleAfterSeconds }, nonNegative("compact_idle_after_seconds")),
	intDef("compact_idle_trigger_percent", "compact_idle_trigger_percent", nil, nil, literal(defaultCompactIdleTrigger, "", ""), func(f fileConfig) optional[int] { return f.CompactIdleTriggerPercent }, func(c *Config, v int) { c.CompactIdleTriggerPercent = v }, func(c Config) int { return c.CompactIdleTriggerPercent }, percent("compact_idle_trigger_percent")),
	intDef("compact_timeout_seconds", "compact_timeout_seconds", nil, nil, literal(defaultCompactTimeoutSeconds, "", ""), func(f fileConfig) optional[int] { return f.CompactTimeoutSeconds }, func(c *Config, v int) { c.CompactTimeoutSeconds = v }, func(c Config) int { return c.CompactTimeoutSeconds }, positive("compact_timeout_seconds")),
	intDef("compact_summary_max_tokens", "compact_summary_max_tokens", nil, nil, literal(0, "", "automatic"), func(f fileConfig) optional[int] { return f.CompactSummaryMaxTokens }, func(c *Config, v int) { c.CompactSummaryMaxTokens = v }, func(c Config) int { return c.CompactSummaryMaxTokens }, nonNegative("compact_summary_max_tokens")),
	intDef("compact_tool_result_max_bytes", "compact_tool_result_max_bytes", nil, nil, literal(0, "", "automatic; negative disables truncation"), func(f fileConfig) optional[int] { return f.CompactToolResultMaxBytes }, func(c *Config, v int) { c.CompactToolResultMaxBytes = v }, func(c Config) int { return c.CompactToolResultMaxBytes }, nil),
	intDef("delegate_max_turns", "delegate_max_turns", nil, nil, literal(defaultDelegateMaxTurns, "", ""), func(f fileConfig) optional[int] { return f.DelegateMaxTurns }, func(c *Config, v int) { c.DelegateMaxTurns = v }, func(c Config) int { return c.DelegateMaxTurns }, positive("delegate_max_turns")),
	intDef("delegate_max_depth", "delegate_max_depth", nil, nil, literal(defaultDelegateMaxDepth, "", ""), func(f fileConfig) optional[int] { return f.DelegateMaxDepth }, func(c *Config, v int) { c.DelegateMaxDepth = v }, func(c Config) int { return c.DelegateMaxDepth }, positive("delegate_max_depth")),
	intDef("delegate_max_active", "delegate_max_active", nil, nil, literal(defaultDelegateMaxActive, "", ""), func(f fileConfig) optional[int] { return f.DelegateMaxActive }, func(c *Config, v int) { c.DelegateMaxActive = v }, func(c Config) int { return c.DelegateMaxActive }, positive("delegate_max_active")),
	strDef("delegate_output", "delegate_output", []string{"delegate-output"}, []string{"HARNESS_DELEGATE_OUTPUT"}, literal(DelegateOutputStatus, "", ""), func(f fileConfig) optional[string] { return f.DelegateOutput }, func(c *Config, v string) { c.DelegateOutput = v }, func(c Config) string { return c.DelegateOutput }, func(v string) (string, error) {
		return canonicalChoice("delegate_output", v, DelegateOutputStatus, DelegateOutputOff, DelegateOutputLines)
	}, []string{DelegateOutputStatus, DelegateOutputOff, DelegateOutputLines}, false),
	boolDef("delegate_tmux", "delegate_tmux", []string{"delegate-tmux"}, []string{"HARNESS_DELEGATE_TMUX"}, derived(func(d RuntimeDefaults) bool { return d.TmuxActive }, "enabled inside tmux", "runtime:tmux"), func(f fileConfig) optional[bool] { return f.DelegateTmux }, func(c *Config, v bool) { c.DelegateTmux = v }, func(c Config) bool { return c.DelegateTmux }),
	intDef("delegate_tmux_max_windows", "delegate_tmux_max_windows", nil, nil, literal(defaultDelegateTmuxMaxWindows, "", ""), func(f fileConfig) optional[int] { return f.DelegateTmuxMaxWindows }, func(c *Config, v int) { c.DelegateTmuxMaxWindows = v }, func(c Config) int { return c.DelegateTmuxMaxWindows }, positive("delegate_tmux_max_windows")),
	strDef("delegate_tmux_layout", "delegate_tmux_layout", []string{"delegate-tmux-layout"}, []string{"HARNESS_DELEGATE_TMUX_LAYOUT"}, literal(DelegateTmuxLayoutPane, "", ""), func(f fileConfig) optional[string] { return f.DelegateTmuxLayout }, func(c *Config, v string) { c.DelegateTmuxLayout = v }, func(c Config) string { return c.DelegateTmuxLayout }, func(v string) (string, error) {
		return canonicalChoice("delegate_tmux_layout", v, DelegateTmuxLayoutPane, DelegateTmuxLayoutWindow)
	}, []string{DelegateTmuxLayoutPane, DelegateTmuxLayoutWindow}, false),
	boolDef("responses_stateful", "responses_stateful", []string{"responses-stateful"}, []string{"HARNESS_RESPONSES_STATEFUL"}, literal(true, "", ""), func(f fileConfig) optional[bool] { return f.ResponsesStateful }, func(c *Config, v bool) { c.ResponsesStateful = v }, func(c Config) bool { return c.ResponsesStateful }),
	strDef("retention_policy", "retention_policy", []string{"retention-policy"}, []string{"HARNESS_RETENTION_POLICY"}, literal("auto", "", ""), func(f fileConfig) optional[string] { return f.RetentionPolicy }, func(c *Config, v string) { c.RetentionPolicy = v }, func(c Config) string { return c.RetentionPolicy }, canonicalRetentionPolicy, []string{"auto", "age", "pressure", "disabled"}, false),
	intDef("retention_floor_tokens", "retention_floor_tokens", nil, nil, literal(0, "", ""), func(f fileConfig) optional[int] { return f.RetentionFloorTokens }, func(c *Config, v int) { c.RetentionFloorTokens = v }, func(c Config) int { return c.RetentionFloorTokens }, nonNegative("retention_floor_tokens")),
	intDef("retention_keep_turns", "retention_keep_turns", nil, []string{"HARNESS_RETENTION_KEEP_TURNS"}, literal(4, "", ""), func(f fileConfig) optional[int] { return f.RetentionKeepTurns }, func(c *Config, v int) { c.RetentionKeepTurns = v }, func(c Config) int { return c.RetentionKeepTurns }, nonNegative("retention_keep_turns")),
	intDef("retention_result_head_bytes", "retention_result_head_bytes", nil, []string{"HARNESS_RETENTION_RESULT_HEAD_BYTES"}, literal(800, "", "clamped to the 4096-byte retention threshold"), func(f fileConfig) optional[int] { return f.RetentionResultHeadBytes }, func(c *Config, v int) { c.RetentionResultHeadBytes = v }, func(c Config) int { return c.RetentionResultHeadBytes }, nonNegative("retention_result_head_bytes")),
	boolDef("no_steer", "no_steer", []string{"no-steer"}, []string{"HARNESS_NO_STEER"}, literal(false, "", ""), func(f fileConfig) optional[bool] { return f.NoSteer }, func(c *Config, v bool) { c.NoSteer = v }, func(c Config) bool { return c.NoSteer }),
	strDef("agent", "agent", []string{"agent"}, []string{"HARNESS_AGENT"}, derived(func(d RuntimeDefaults) string { return d.Agent }, "runtime default agent", "runtime:agent"), func(f fileConfig) optional[string] { return f.Agent }, func(c *Config, v string) { c.Agent = v }, func(c Config) string { return c.Agent }, func(v string) (string, error) { return canonicalLowerNonEmpty("agent", v) }, nil, false),
	strDef("handoff_agent", "handoff_agent", []string{"handoff-agent"}, []string{"HARNESS_HANDOFF_AGENT"}, derived(func(d RuntimeDefaults) string {
		if d.HandoffAgent != "" {
			return d.HandoffAgent
		}
		return d.Agent
	}, "runtime default implementation agent", "runtime:handoff-agent"), func(f fileConfig) optional[string] { return f.HandoffAgent }, func(c *Config, v string) { c.HandoffAgent = v }, func(c Config) string { return c.HandoffAgent }, func(v string) (string, error) { return canonicalLowerNonEmpty("handoff_agent", v) }, nil, false),
	boolDef("verbose", "verbose", []string{"v"}, []string{"HARNESS_VERBOSE"}, literal(false, "", ""), func(f fileConfig) optional[bool] { return f.Verbose }, func(c *Config, v bool) { c.Verbose = v }, func(c Config) bool { return c.Verbose }),
	boolDef("tool_stream", "tool_stream", []string{"tool-stream"}, []string{"HARNESS_TOOL_STREAM"}, literal(false, "", ""), func(f fileConfig) optional[bool] { return f.ToolStream }, func(c *Config, v bool) { c.ToolStream = v }, func(c Config) bool { return c.ToolStream }),
	boolDef("show_diffs", "show_diffs", []string{"show-diffs"}, []string{"HARNESS_SHOW_DIFFS"}, literal(true, "", ""), func(f fileConfig) optional[bool] { return f.ShowDiffs }, func(c *Config, v bool) { c.ShowDiffs = v }, func(c Config) bool { return c.ShowDiffs }),
	boolDef("stagnation_nudge", "stagnation_nudge", []string{"stagnation-nudge"}, []string{"HARNESS_STAGNATION_NUDGE"}, literal(true, "", ""), func(f fileConfig) optional[bool] { return f.StagnationNudge }, func(c *Config, v bool) { c.StagnationNudge = v }, func(c Config) bool { return c.StagnationNudge }),
	strDef("log_level", "log_level", []string{"log-level"}, []string{"HARNESS_LOG_LEVEL"}, literal(logging.LevelInfo, "", ""), func(f fileConfig) optional[string] { return f.LogLevel }, func(c *Config, v string) { c.LogLevel = v }, func(c Config) string { return c.LogLevel }, canonicalLogLevel, []string{"debug", "info", "warn", "error"}, false),
	noColorDefinition(),
	strDef("color_theme", "color_theme", []string{"color-theme"}, []string{"HARNESS_COLOR_THEME"}, literal(ColorThemeDark, "", ""), func(f fileConfig) optional[string] { return f.ColorTheme }, func(c *Config, v string) { c.ColorTheme = v }, func(c Config) string { return c.ColorTheme }, canonicalColorTheme, []string{ColorThemeDark, ColorThemeLight}, false),
	strDef("timestamps", "timestamps", []string{"timestamps"}, []string{"HARNESS_TIMESTAMPS"}, literal(TimestampShort, "", ""), func(f fileConfig) optional[string] { return f.Timestamps }, func(c *Config, v string) { c.TimestampMode = v }, func(c Config) string { return c.TimestampMode }, NormalizeTimestampMode, []string{TimestampShort, TimestampFull, TimestampNone}, false),
	strDef("repl_prompt", "repl_prompt", []string{"repl-prompt"}, []string{"HARNESS_REPL_PROMPT"}, literal(replprompt.DefaultFormat, "", ""), func(f fileConfig) optional[string] { return f.ReplPrompt }, func(c *Config, v string) { c.ReplPrompt = v }, func(c Config) string { return c.ReplPrompt }, canonicalReplPrompt, nil, false),
	strDef("repl_edit_mode", "repl_edit_mode", []string{"repl-edit-mode"}, []string{"HARNESS_REPL_EDIT_MODE"}, literal(DefaultReplEditMode, "", ""), func(f fileConfig) optional[string] { return f.ReplEditMode }, func(c *Config, v string) { c.ReplEditMode = v }, func(c Config) string { return c.ReplEditMode }, canonicalReplEditMode, []string{"emacs", "vi"}, false),
	boolDef("mcp.enable", "mcp.enable", nil, []string{"HARNESS_MCP_ENABLE"}, literal(false, "", ""), func(f fileConfig) optional[bool] {
		if f.MCP.Set {
			return f.MCP.Value.Enable
		}
		return optional[bool]{}
	}, func(c *Config, v bool) { c.MCP.Enable = v }, func(c Config) bool { return c.MCP.Enable }),
	strDef("mcp.proxy", "mcp.proxy", nil, []string{"HARNESS_MCP_PROXY"}, derived(func(d RuntimeDefaults) string { return d.MCPProxyURL }, "runtime MCP proxy URL", "runtime:mcp-proxy-url"), func(f fileConfig) optional[string] {
		if f.MCP.Set {
			return f.MCP.Value.Proxy
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.MCP.Proxy = v }, func(c Config) string { return c.MCP.Proxy }, canonicalNonEmptyFor("mcp.proxy"), nil, false),
	strDef("mcp.api_key", "mcp.api_key", []string{"mcp-proxy-api-key"}, []string{"HARNESS_MCP_PROXY_API_KEY"}, literal("", "unset", ""), func(f fileConfig) optional[string] {
		if f.MCP.Set {
			return f.MCP.Value.APIKey
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.MCP.APIKey = v }, func(c Config) string { return c.MCP.APIKey }, nil, nil, true),
	intDef("mcp.max_tools", "mcp.max_tools", nil, nil, literal(0, "", "unlimited"), func(f fileConfig) optional[int] {
		if f.MCP.Set {
			return f.MCP.Value.MaxTools
		}
		return optional[int]{}
	}, func(c *Config, v int) { c.MCP.MaxTools = v }, func(c Config) int { return c.MCP.MaxTools }, nonNegative("mcp.max_tools")),
	boolDef("mcp.local.enable", "mcp.local.enable", nil, []string{"HARNESS_MCP_LOCAL_ENABLE"}, literal(false, "", ""), func(f fileConfig) optional[bool] {
		if f.MCP.Set && f.MCP.Value.Local.Set {
			return f.MCP.Value.Local.Value.Enable
		}
		return optional[bool]{}
	}, func(c *Config, v bool) { c.MCP.Local.Enable = v }, func(c Config) bool { return c.MCP.Local.Enable }),
	strDef("mcp.local.command", "mcp.local.command", nil, nil, literal("", "unset", ""), func(f fileConfig) optional[string] {
		if f.MCP.Set && f.MCP.Value.Local.Set {
			return f.MCP.Value.Local.Value.Command
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.MCP.Local.Command = v }, func(c Config) string { return c.MCP.Local.Command }, nil, nil, false),
	boolDef("lsp.enable", "lsp.enable", nil, []string{"HARNESS_LSP_ENABLE"}, literal(false, "", ""), func(f fileConfig) optional[bool] {
		if f.LSP.Set {
			return f.LSP.Value.Enable
		}
		return optional[bool]{}
	}, func(c *Config, v bool) { c.LSP.Enable = v }, func(c Config) bool { return c.LSP.Enable }),
	boolDef("lsp.prewarm", "lsp.prewarm", nil, []string{"HARNESS_LSP_PREWARM"}, literal(true, "", ""), func(f fileConfig) optional[bool] {
		if f.LSP.Set {
			return f.LSP.Value.Prewarm
		}
		return optional[bool]{}
	}, func(c *Config, v bool) { c.LSP.Prewarm = v }, func(c Config) bool { return c.LSP.Prewarm }),
	boolDef("lsp.serena.enable", "lsp.serena.enable", nil, []string{"HARNESS_LSP_SERENA_ENABLE"}, literal(false, "", ""), func(f fileConfig) optional[bool] {
		if f.LSP.Set && f.LSP.Value.Serena.Set {
			return f.LSP.Value.Serena.Value.Enable
		}
		return optional[bool]{}
	}, func(c *Config, v bool) { c.LSP.Serena.Enable = v }, func(c Config) bool { return c.LSP.Serena.Enable }),
	strDef("lsp.serena.command", "lsp.serena.command", nil, nil, literal(DefaultSerenaCommand, "", ""), func(f fileConfig) optional[string] {
		if f.LSP.Set && f.LSP.Value.Serena.Set {
			return f.LSP.Value.Serena.Value.Command
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.LSP.Serena.Command = v }, func(c Config) string { return c.LSP.Serena.Command }, canonicalNonEmptyFor("lsp.serena.command"), nil, false),
	boolDef("otel.enabled", "otel.enabled", []string{"otel-enabled"}, []string{"HARNESS_OTEL_ENABLED"}, literal(false, "", ""), func(f fileConfig) optional[bool] {
		if f.OTel.Set {
			return f.OTel.Value.Enabled
		}
		return optional[bool]{}
	}, func(c *Config, v bool) { c.OTel.Enabled = v }, func(c Config) bool { return c.OTel.Enabled }),
	oTelEndpointDefinition(),
	strDef("otel.protocol", "otel.protocol", []string{"otel-protocol"}, []string{"HARNESS_OTEL_PROTOCOL"}, literal("http/json", "", ""), func(f fileConfig) optional[string] {
		if f.OTel.Set {
			return f.OTel.Value.Protocol
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.OTel.Protocol = v }, func(c Config) string { return c.OTel.Protocol }, canonicalOTelProtocol, []string{"http/json"}, false),
	intDef("otel.timeout_seconds", "otel.timeout_seconds", []string{"otel-timeout"}, []string{"HARNESS_OTEL_TIMEOUT"}, literal(5, "", "seconds"), func(f fileConfig) optional[int] {
		if f.OTel.Set {
			return f.OTel.Value.TimeoutSeconds
		}
		return optional[int]{}
	}, func(c *Config, v int) { c.OTel.TimeoutSeconds = v }, func(c Config) int { return c.OTel.TimeoutSeconds }, oTelTimeoutSeconds),
	strDef("otel.service_name", "otel.service_name", []string{"otel-service-name"}, []string{"OTEL_SERVICE_NAME", "HARNESS_OTEL_SERVICE_NAME"}, literal("harness", "", ""), func(f fileConfig) optional[string] {
		if f.OTel.Set {
			return f.OTel.Value.ServiceName
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.OTel.ServiceName = v }, func(c Config) string { return c.OTel.ServiceName }, canonicalOTelServiceName, nil, false),
	strDef("otel.hostname", "otel.hostname", []string{"otel-hostname"}, []string{"HARNESS_OTEL_HOSTNAME", "OTEL_HOSTNAME"}, literal("", "short hostname", "empty disables host.name"), func(f fileConfig) optional[string] {
		if f.OTel.Set {
			return f.OTel.Value.Hostname
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.OTel.Hostname = v }, func(c Config) string { return c.OTel.Hostname }, canonicalOTelHostname, nil, false),
}

func oTelEndpointDefinition() scalarDefinition[string] {
	definition := strDef("otel.endpoint", "otel.endpoint", []string{"otel-endpoint"}, []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "HARNESS_OTEL_ENDPOINT"}, literal("", "unset", ""), func(f fileConfig) optional[string] {
		if f.OTel.Set {
			return f.OTel.Value.Endpoint
		}
		return optional[string]{}
	}, func(c *Config, v string) { c.OTel.Endpoint = v }, func(c Config) string { return c.OTel.Endpoint }, canonicalOTelEndpoint, nil, false)
	definition.skipEnv = func(name, value string) bool {
		return name == "OTEL_EXPORTER_OTLP_ENDPOINT" && strings.TrimSpace(value) == ""
	}
	return definition
}

func canonicalNonEmptyFor(setting string) func(string) (string, error) {
	return func(value string) (string, error) { return canonicalNonEmpty(setting, value) }
}

func noColorDefinition() scalarDefinition[bool] {
	definition := boolDef("no_color", "no_color", []string{"no-color"}, []string{"HARNESS_NO_COLOR", "NO_COLOR"}, literal(false, "", "NO_COLOR is a presence-based override"), func(f fileConfig) optional[bool] { return f.NoColor }, func(c *Config, v bool) { c.NoColor = v }, func(c Config) bool { return c.NoColor })
	definition.parseEnv = func(name, value string) (bool, error) {
		if name == "NO_COLOR" {
			return true, nil
		}
		return parseBool(value)
	}
	return definition
}

// customDefinition owns a structured subtree whose dynamic map boundaries
// cannot be represented by scalar flag values.
type customDefinition struct {
	meta       configmeta.Parameter
	resolveFn  func(*resolveContext) error
	validateFn func(fileConfig, string) error
	projectFn  func(Config) any
}

func (d customDefinition) parameter() configmeta.Parameter { return d.meta }
func (d customDefinition) register(*flagState)             {}
func (d customDefinition) resolve(c *resolveContext) error { return d.resolveFn(c) }
func (d customDefinition) validateFile(f fileConfig, p string) error {
	if d.validateFn == nil {
		return nil
	}
	return d.validateFn(f, p)
}
func (d customDefinition) project(c Config) any { return d.projectFn(c) }

var customDefinitions = []parameterDefinition{
	custom("agents", "object", "agents", false, func(c *resolveContext) error {
		if c.file.Agents.Set {
			c.result.Config.Agents = maps.Clone(c.file.Agents.Value)
			c.fileSource("agents")
		} else {
			c.defaultSource("agents")
		}
		return nil
	}, func(c Config) any { return c.Agents }),
	custom("mcp.headers", "object", "mcp.headers", true, resolveMCPHeaders, func(c Config) any { return redactStringMap(c.MCP.Headers) }),
	custom("mcp.disabled_servers", "string[]", "mcp.disabled_servers", false, func(c *resolveContext) error {
		if c.file.MCP.Set && c.file.MCP.Value.DisabledServers.Set {
			c.result.Config.MCP.DisabledServers = append([]string(nil), c.file.MCP.Value.DisabledServers.Value...)
			c.fileSource("mcp.disabled_servers")
		} else {
			c.defaultSource("mcp.disabled_servers")
		}
		return nil
	}, func(c Config) any { return c.MCP.DisabledServers }),
	custom("mcp.local.args", "string[]", "mcp.local.args", false, resolveMCPLocalArgs, func(c Config) any { return c.MCP.Local.Args }),
	custom("mcp.local.env", "object", "mcp.local.env", true, resolveMCPLocalEnv, func(c Config) any { return redactStringMap(c.MCP.Local.Env) }),
	custom("lsp.tools", "string[]", "lsp.tools", false, resolveLSPTools, func(c Config) any { return c.LSP.Tools }),
	custom("lsp.servers", "object", "lsp.servers", true, resolveLSPServers, func(c Config) any { return redactLSPServers(c.LSP.Servers) }),
	custom("lsp.serena.args", "string[]", "lsp.serena.args", false, resolveSerenaArgs, func(c Config) any { return c.LSP.Serena.Args }),
	custom("lsp.serena.env", "object", "lsp.serena.env", true, resolveSerenaEnv, func(c Config) any { return redactStringMap(c.LSP.Serena.Env) }),
	hooksCustomDefinition(),
	custom("hook_configs", "string[]", "hook_configs", false, func(*resolveContext) error { return nil }, func(c Config) any { return c.HookConfigs }),
	oTelHeadersDefinition(),
	custom("otel.resource_attributes", "object", "otel.resource_attributes", false, resolveOTelResourceAttributes, func(c Config) any { return c.OTel.ResourceAttributes }),
}

func oTelHeadersDefinition() customDefinition {
	definition := custom("otel.headers", "object", "otel.headers", true, resolveOTelHeaders, func(c Config) any { return redactStringMap(c.OTel.Headers) })
	definition.meta.Environment = []string{"OTEL_EXPORTER_OTLP_HEADERS", "HARNESS_OTEL_HEADERS"}
	return definition
}

func hooksCustomDefinition() customDefinition {
	definition := custom("hooks", "object", "hooks", false, resolveHooks, func(c Config) any { return c.Hooks })
	definition.meta.Flags = []string{"hooks"}
	definition.validateFn = func(file fileConfig, path string) error {
		if file.Hooks.Set {
			if _, err := hooks.DecodeEventMap(file.Hooks.Value); err != nil {
				return fmt.Errorf("config %q setting hooks: %w", path, err)
			}
		}
		if file.HookConfigs.Set {
			if _, err := hooks.LoadFiles(filepath.Dir(path), file.HookConfigs.Value); err != nil {
				return fmt.Errorf("config %q setting hook_configs: %w", path, err)
			}
		}
		return nil
	}
	return definition
}

func custom(key, typ, path string, sensitive bool, resolve func(*resolveContext) error, project func(Config) any) customDefinition {
	return customDefinition{meta: metadata(key, typ, path, nil, nil, configmeta.Default{Kind: configmeta.DefaultLiteral, Value: nil, Display: "unset"}, "Structured "+key+" settings.", nil, sensitive), resolveFn: resolve, projectFn: project}
}

var allDefinitions = append(append([]parameterDefinition(nil), definitions...), customDefinitions...)
var parameterCatalog = func() configmeta.Catalog {
	parameters := make([]configmeta.Parameter, 0, len(allDefinitions))
	for _, definition := range allDefinitions {
		parameters = append(parameters, definition.parameter())
	}
	return configmeta.MustCatalog(parameters...)
}()

// Catalog returns the immutable ordered harness parameter catalog.
func Catalog() configmeta.Catalog { return parameterCatalog }

func redactStringMap(values map[string]string) any {
	if values == nil {
		return map[string]string(nil)
	}
	out := make(map[string]string, len(values))
	for key := range values {
		out[key] = redactedValue
	}
	return out
}
func redactLSPServers(values map[string]LSPServerConfig) any {
	out := cloneLSPServers(values)
	for name, server := range out {
		if server.Env != nil {
			for key := range server.Env {
				server.Env[key] = redactedValue
			}
		}
		if value := strings.TrimSpace(string(server.InitOptions)); value != "" && value != "null" {
			server.InitOptions = json.RawMessage(`"<redacted>"`)
		}
		out[name] = server
	}
	return out
}
