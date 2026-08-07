package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// findProjectConfig walks from startDir upward and returns the first
// <dir>/.harness/config.json regular file between startDir and the git root
// (inclusive). The git root is the nearest ancestor (including startDir)
// containing a .git entry (file or directory, covering worktrees/submodules).
// If no git root is found above startDir, only startDir itself is eligible
// so $HOME/.harness/config.json never leaks when outside a repository.
// It is pure path/stat walking; it never invokes the git binary.
func findProjectConfig(startDir string) (string, error) {
	if startDir == "" {
		return "", nil
	}
	clean := filepath.Clean(startDir)
	// Find git root.
	gitRoot := ""
	current := clean
	for {
		gitMarker := filepath.Join(current, ".git")
		if info, err := os.Stat(gitMarker); err == nil {
			_ = info // file or directory both count
			gitRoot = current
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			// Permission or other error: treat as not a git root and continue
			// upward rather than failing discovery entirely. This keeps
			// discovery best-effort for unreadable parents.
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	// Determine search limit.
	limit := clean
	if gitRoot != "" {
		limit = gitRoot
	}

	current = clean
	for {
		candidate := filepath.Join(current, ".harness", "config.json")
		if info, err := os.Stat(candidate); err == nil {
			if !info.IsDir() {
				return candidate, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			// For other errors (permission etc.), ignore and continue upward.
		}
		if current == limit {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", nil
}

// findProjectConfigFromWD is the production entry point that uses the
// process working directory. It is separated for testability.
func findProjectConfigFromWD() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findProjectConfig(wd)
}

func isExplicitConfig(flags *flagState, lookup func(string) (string, bool)) bool {
	if flags != nil {
		if vals := flags.invocation["config"]; len(vals) > 0 {
			return true
		}
	}
	if lookup != nil {
		if _, present := lookup("HARNESS_CONFIG"); present {
			return true
		}
	}
	return false
}

func mergeOpt[T any](global, project optional[T], globalPath, projectPath, key string, sourceMap map[string]string) optional[T] {
	if project.Set {
		sourceMap[key] = projectPath
		return project
	}
	if global.Set {
		sourceMap[key] = globalPath
		return global
	}
	return optional[T]{}
}

func mergeFileConfig(global, project fileConfig, globalPath, projectPath string) (fileConfig, map[string]string) {
	sourceMap := make(map[string]string)
	var out fileConfig

	out.Model = mergeOpt(global.Model, project.Model, globalPath, projectPath, "model", sourceMap)
	out.ModelProxyURL = mergeOpt(global.ModelProxyURL, project.ModelProxyURL, globalPath, projectPath, "model_proxy_url", sourceMap)
	out.ModelProxyAPIKey = mergeOpt(global.ModelProxyAPIKey, project.ModelProxyAPIKey, globalPath, projectPath, "model_proxy_api_key", sourceMap)
	out.TraceProxy = mergeOpt(global.TraceProxy, project.TraceProxy, globalPath, projectPath, "trace_proxy", sourceMap)
	out.SystemPrompt = mergeOpt(global.SystemPrompt, project.SystemPrompt, globalPath, projectPath, "system_prompt", sourceMap)
	out.NoEnv = mergeOpt(global.NoEnv, project.NoEnv, globalPath, projectPath, "no_env", sourceMap)
	out.MaxTurns = mergeOpt(global.MaxTurns, project.MaxTurns, globalPath, projectPath, "max_turns", sourceMap)
	out.MaxPromptTokens = mergeOpt(global.MaxPromptTokens, project.MaxPromptTokens, globalPath, projectPath, "max_prompt_tokens", sourceMap)
	out.MaxOutputTokens = mergeOpt(global.MaxOutputTokens, project.MaxOutputTokens, globalPath, projectPath, "max_output_tokens", sourceMap)
	out.MaxPromptCostUSD = mergeOpt(global.MaxPromptCostUSD, project.MaxPromptCostUSD, globalPath, projectPath, "max_prompt_cost_usd", sourceMap)
	out.GoalMaxContinuations = mergeOpt(global.GoalMaxContinuations, project.GoalMaxContinuations, globalPath, projectPath, "goal_max_continuations", sourceMap)
	out.ToolTimeoutSeconds = mergeOpt(global.ToolTimeoutSeconds, project.ToolTimeoutSeconds, globalPath, projectPath, "tool_timeout_seconds", sourceMap)
	out.RunCommandTimeoutSeconds = mergeOpt(global.RunCommandTimeoutSeconds, project.RunCommandTimeoutSeconds, globalPath, projectPath, "run_command_timeout_seconds", sourceMap)
	out.RunCommandBackgroundTimeoutSeconds = mergeOpt(global.RunCommandBackgroundTimeoutSeconds, project.RunCommandBackgroundTimeoutSeconds, globalPath, projectPath, "run_command_background_timeout_seconds", sourceMap)
	out.DefaultContextWindow = mergeOpt(global.DefaultContextWindow, project.DefaultContextWindow, globalPath, projectPath, "default_context_window", sourceMap)
	out.ContextWindow = mergeOpt(global.ContextWindow, project.ContextWindow, globalPath, projectPath, "context_window", sourceMap)
	out.Reasoning = mergeOpt(global.Reasoning, project.Reasoning, globalPath, projectPath, "reasoning", sourceMap)
	out.ReasoningSummary = mergeOpt(global.ReasoningSummary, project.ReasoningSummary, globalPath, projectPath, "reasoning_summary", sourceMap)
	out.ImageDetail = mergeOpt(global.ImageDetail, project.ImageDetail, globalPath, projectPath, "image_detail", sourceMap)
	out.WebSearch = mergeOpt(global.WebSearch, project.WebSearch, globalPath, projectPath, "web_search", sourceMap)
	out.AgentsMDWarnBytes = mergeOpt(global.AgentsMDWarnBytes, project.AgentsMDWarnBytes, globalPath, projectPath, "agents_md_warn_bytes", sourceMap)
	out.ToolResultMaxBytes = mergeOpt(global.ToolResultMaxBytes, project.ToolResultMaxBytes, globalPath, projectPath, "tool_result_max_bytes", sourceMap)
	out.ToolResultMaxLines = mergeOpt(global.ToolResultMaxLines, project.ToolResultMaxLines, globalPath, projectPath, "tool_result_max_lines", sourceMap)
	out.RGResultMaxBytes = mergeOpt(global.RGResultMaxBytes, project.RGResultMaxBytes, globalPath, projectPath, "rg_result_max_bytes", sourceMap)
	out.RGResultMaxLines = mergeOpt(global.RGResultMaxLines, project.RGResultMaxLines, globalPath, projectPath, "rg_result_max_lines", sourceMap)
	out.GrepResultMaxBytes = mergeOpt(global.GrepResultMaxBytes, project.GrepResultMaxBytes, globalPath, projectPath, "grep_result_max_bytes", sourceMap)
	out.GrepResultMaxLines = mergeOpt(global.GrepResultMaxLines, project.GrepResultMaxLines, globalPath, projectPath, "grep_result_max_lines", sourceMap)
	out.ReadFileDefaultLimit = mergeOpt(global.ReadFileDefaultLimit, project.ReadFileDefaultLimit, globalPath, projectPath, "read_file_default_limit", sourceMap)
	out.ReadFileResultMaxBytes = mergeOpt(global.ReadFileResultMaxBytes, project.ReadFileResultMaxBytes, globalPath, projectPath, "read_file_result_max_bytes", sourceMap)
	out.ReadFileResultMaxLines = mergeOpt(global.ReadFileResultMaxLines, project.ReadFileResultMaxLines, globalPath, projectPath, "read_file_result_max_lines", sourceMap)
	out.CompactKeepTurns = mergeOpt(global.CompactKeepTurns, project.CompactKeepTurns, globalPath, projectPath, "compact_keep_turns", sourceMap)
	out.CompactKeepTokens = mergeOpt(global.CompactKeepTokens, project.CompactKeepTokens, globalPath, projectPath, "compact_keep_tokens", sourceMap)
	out.CompactAutoEnabled = mergeOpt(global.CompactAutoEnabled, project.CompactAutoEnabled, globalPath, projectPath, "compact_auto_enabled", sourceMap)
	out.CompactTriggerPercent = mergeOpt(global.CompactTriggerPercent, project.CompactTriggerPercent, globalPath, projectPath, "compact_trigger_percent", sourceMap)
	out.CompactTargetPercent = mergeOpt(global.CompactTargetPercent, project.CompactTargetPercent, globalPath, projectPath, "compact_target_percent", sourceMap)
	out.CompactIdleAfterSeconds = mergeOpt(global.CompactIdleAfterSeconds, project.CompactIdleAfterSeconds, globalPath, projectPath, "compact_idle_after_seconds", sourceMap)
	out.CompactIdleTriggerPercent = mergeOpt(global.CompactIdleTriggerPercent, project.CompactIdleTriggerPercent, globalPath, projectPath, "compact_idle_trigger_percent", sourceMap)
	out.CompactTimeoutSeconds = mergeOpt(global.CompactTimeoutSeconds, project.CompactTimeoutSeconds, globalPath, projectPath, "compact_timeout_seconds", sourceMap)
	out.CompactSummaryMaxTokens = mergeOpt(global.CompactSummaryMaxTokens, project.CompactSummaryMaxTokens, globalPath, projectPath, "compact_summary_max_tokens", sourceMap)
	out.CompactToolResultMaxBytes = mergeOpt(global.CompactToolResultMaxBytes, project.CompactToolResultMaxBytes, globalPath, projectPath, "compact_tool_result_max_bytes", sourceMap)
	out.DelegateMaxTurns = mergeOpt(global.DelegateMaxTurns, project.DelegateMaxTurns, globalPath, projectPath, "delegate_max_turns", sourceMap)
	out.DelegateMaxDepth = mergeOpt(global.DelegateMaxDepth, project.DelegateMaxDepth, globalPath, projectPath, "delegate_max_depth", sourceMap)
	out.DelegateMaxActive = mergeOpt(global.DelegateMaxActive, project.DelegateMaxActive, globalPath, projectPath, "delegate_max_active", sourceMap)
	out.DelegateMaxDescendants = mergeOpt(global.DelegateMaxDescendants, project.DelegateMaxDescendants, globalPath, projectPath, "delegate_max_descendants", sourceMap)
	out.DelegateOutput = mergeOpt(global.DelegateOutput, project.DelegateOutput, globalPath, projectPath, "delegate_output", sourceMap)
	out.DelegateTmux = mergeOpt(global.DelegateTmux, project.DelegateTmux, globalPath, projectPath, "delegate_tmux", sourceMap)
	out.DelegateTmuxMaxWindows = mergeOpt(global.DelegateTmuxMaxWindows, project.DelegateTmuxMaxWindows, globalPath, projectPath, "delegate_tmux_max_windows", sourceMap)
	out.DelegateTmuxLayout = mergeOpt(global.DelegateTmuxLayout, project.DelegateTmuxLayout, globalPath, projectPath, "delegate_tmux_layout", sourceMap)
	out.ResponsesStateful = mergeOpt(global.ResponsesStateful, project.ResponsesStateful, globalPath, projectPath, "responses_stateful", sourceMap)
	out.RetentionPolicy = mergeOpt(global.RetentionPolicy, project.RetentionPolicy, globalPath, projectPath, "retention_policy", sourceMap)
	out.RetentionFloorTokens = mergeOpt(global.RetentionFloorTokens, project.RetentionFloorTokens, globalPath, projectPath, "retention_floor_tokens", sourceMap)
	out.NoSteer = mergeOpt(global.NoSteer, project.NoSteer, globalPath, projectPath, "no_steer", sourceMap)
	out.Verbose = mergeOpt(global.Verbose, project.Verbose, globalPath, projectPath, "verbose", sourceMap)
	out.ToolStream = mergeOpt(global.ToolStream, project.ToolStream, globalPath, projectPath, "tool_stream", sourceMap)
	out.ShowDiffs = mergeOpt(global.ShowDiffs, project.ShowDiffs, globalPath, projectPath, "show_diffs", sourceMap)
	out.LogLevel = mergeOpt(global.LogLevel, project.LogLevel, globalPath, projectPath, "log_level", sourceMap)
	out.NoColor = mergeOpt(global.NoColor, project.NoColor, globalPath, projectPath, "no_color", sourceMap)
	out.ColorTheme = mergeOpt(global.ColorTheme, project.ColorTheme, globalPath, projectPath, "color_theme", sourceMap)
	out.Timestamps = mergeOpt(global.Timestamps, project.Timestamps, globalPath, projectPath, "timestamps", sourceMap)
	out.ReplPrompt = mergeOpt(global.ReplPrompt, project.ReplPrompt, globalPath, projectPath, "repl_prompt", sourceMap)
	out.ReplEditMode = mergeOpt(global.ReplEditMode, project.ReplEditMode, globalPath, projectPath, "repl_edit_mode", sourceMap)
	out.Agent = mergeOpt(global.Agent, project.Agent, globalPath, projectPath, "agent", sourceMap)
	out.HandoffAgent = mergeOpt(global.HandoffAgent, project.HandoffAgent, globalPath, projectPath, "handoff_agent", sourceMap)
	// Agents, Hooks, HookConfigs, Hist* are handled separately to keep leaf keys consistent
	out.Agents = mergeOpt(global.Agents, project.Agents, globalPath, projectPath, "agents", sourceMap)
	// For hooks we intentionally keep override semantics per setting (hook_configs slice)
	// The merge helpers for hook_configs ensure relative paths were absolutized per file at decode time.
	out.Hooks = mergeOpt(global.Hooks, project.Hooks, globalPath, projectPath, "hooks", sourceMap)
	out.HookConfigs = mergeOpt(global.HookConfigs, project.HookConfigs, globalPath, projectPath, "hook_configs", sourceMap)
	_ = json.RawMessage{} // ensure json import used
	out.HistFile = mergeOpt(global.HistFile, project.HistFile, globalPath, projectPath, "histfile", sourceMap)
	out.HistFileSize = mergeOpt(global.HistFileSize, project.HistFileSize, globalPath, projectPath, "histfilesize", sourceMap)
	out.HistSize = mergeOpt(global.HistSize, project.HistSize, globalPath, projectPath, "histsize", sourceMap)
	out.MCP = mergeMCPOpt(global.MCP, project.MCP, globalPath, projectPath, sourceMap)
	out.LSP = mergeLSPOpt(global.LSP, project.LSP, globalPath, projectPath, sourceMap)
	return out, sourceMap
}

func mergeMCPOpt(global, project optional[fileMCPConfig], globalPath, projectPath string, sourceMap map[string]string) optional[fileMCPConfig] {
	if !global.Set && !project.Set {
		return optional[fileMCPConfig]{}
	}
	var gVal, pVal fileMCPConfig
	if global.Set {
		gVal = global.Value
	}
	if project.Set {
		pVal = project.Value
	}
	var merged fileMCPConfig
	merged.Enable = mergeOpt(gVal.Enable, pVal.Enable, globalPath, projectPath, "mcp.enable", sourceMap)
	merged.Proxy = mergeOpt(gVal.Proxy, pVal.Proxy, globalPath, projectPath, "mcp.proxy", sourceMap)
	merged.APIKey = mergeOpt(gVal.APIKey, pVal.APIKey, globalPath, projectPath, "mcp.api_key", sourceMap)
	merged.Headers = mergeOpt(gVal.Headers, pVal.Headers, globalPath, projectPath, "mcp.headers", sourceMap)
	merged.MaxTools = mergeOpt(gVal.MaxTools, pVal.MaxTools, globalPath, projectPath, "mcp.max_tools", sourceMap)
	merged.DisabledServers = mergeOpt(gVal.DisabledServers, pVal.DisabledServers, globalPath, projectPath, "mcp.disabled_servers", sourceMap)
	// Local is optional nested; merge with dedicated helper
	merged.Local = mergeLocalMCPOpt(gVal.Local, pVal.Local, globalPath, projectPath, sourceMap)
	// Consider MCP set if either original had it or any leaf is set
	hasLeaf := merged.Enable.Set || merged.Proxy.Set || merged.APIKey.Set || merged.Headers.Set || merged.MaxTools.Set || merged.DisabledServers.Set || merged.Local.Set
	if hasLeaf || global.Set || project.Set {
		return optional[fileMCPConfig]{Set: true, Value: merged}
	}
	return optional[fileMCPConfig]{}
}

func mergeLocalMCPOpt(global, project optional[fileLocalMCPConfig], globalPath, projectPath string, sourceMap map[string]string) optional[fileLocalMCPConfig] {
	if !global.Set && !project.Set {
		return optional[fileLocalMCPConfig]{}
	}
	var gVal, pVal fileLocalMCPConfig
	if global.Set {
		gVal = global.Value
	}
	if project.Set {
		pVal = project.Value
	}
	var merged fileLocalMCPConfig
	merged.Enable = mergeOpt(gVal.Enable, pVal.Enable, globalPath, projectPath, "mcp.local.enable", sourceMap)
	merged.Command = mergeOpt(gVal.Command, pVal.Command, globalPath, projectPath, "mcp.local.command", sourceMap)
	merged.Args = mergeOpt(gVal.Args, pVal.Args, globalPath, projectPath, "mcp.local.args", sourceMap)
	merged.Env = mergeOpt(gVal.Env, pVal.Env, globalPath, projectPath, "mcp.local.env", sourceMap)
	hasLeaf := merged.Enable.Set || merged.Command.Set || merged.Args.Set || merged.Env.Set
	if hasLeaf || global.Set || project.Set {
		return optional[fileLocalMCPConfig]{Set: true, Value: merged}
	}
	return optional[fileLocalMCPConfig]{}
}

func mergeLSPOpt(global, project optional[fileLSPConfig], globalPath, projectPath string, sourceMap map[string]string) optional[fileLSPConfig] {
	if !global.Set && !project.Set {
		return optional[fileLSPConfig]{}
	}
	var gVal, pVal fileLSPConfig
	if global.Set {
		gVal = global.Value
	}
	if project.Set {
		pVal = project.Value
	}
	var merged fileLSPConfig
	merged.Enable = mergeOpt(gVal.Enable, pVal.Enable, globalPath, projectPath, "lsp.enable", sourceMap)
	merged.Tools = mergeOpt(gVal.Tools, pVal.Tools, globalPath, projectPath, "lsp.tools", sourceMap)
	merged.Servers = mergeOpt(gVal.Servers, pVal.Servers, globalPath, projectPath, "lsp.servers", sourceMap)
	merged.Serena = mergeSerenaOpt(gVal.Serena, pVal.Serena, globalPath, projectPath, sourceMap)
	hasLeaf := merged.Enable.Set || merged.Tools.Set || merged.Servers.Set || merged.Serena.Set
	if hasLeaf || global.Set || project.Set {
		return optional[fileLSPConfig]{Set: true, Value: merged}
	}
	return optional[fileLSPConfig]{}
}

func mergeSerenaOpt(global, project optional[fileSerenaConfig], globalPath, projectPath string, sourceMap map[string]string) optional[fileSerenaConfig] {
	if !global.Set && !project.Set {
		return optional[fileSerenaConfig]{}
	}
	var gVal, pVal fileSerenaConfig
	if global.Set {
		gVal = global.Value
	}
	if project.Set {
		pVal = project.Value
	}
	var merged fileSerenaConfig
	merged.Enable = mergeOpt(gVal.Enable, pVal.Enable, globalPath, projectPath, "lsp.serena.enable", sourceMap)
	merged.Command = mergeOpt(gVal.Command, pVal.Command, globalPath, projectPath, "lsp.serena.command", sourceMap)
	merged.Args = mergeOpt(gVal.Args, pVal.Args, globalPath, projectPath, "lsp.serena.args", sourceMap)
	merged.Env = mergeOpt(gVal.Env, pVal.Env, globalPath, projectPath, "lsp.serena.env", sourceMap)
	hasLeaf := merged.Enable.Set || merged.Command.Set || merged.Args.Set || merged.Env.Set
	if hasLeaf || global.Set || project.Set {
		return optional[fileSerenaConfig]{Set: true, Value: merged}
	}
	return optional[fileSerenaConfig]{}
}
