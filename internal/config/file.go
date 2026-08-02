package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// optional preserves omission separately from explicit zero, false, and empty.
// JSON null is never a valid way to inherit a setting.
type optional[T any] struct {
	Set   bool
	Value T
}

func (o *optional[T]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("must not be null; omit the setting to inherit it")
	}
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	o.Set, o.Value = true, value
	return nil
}

type fileConfig struct {
	Model                              optional[string]                     `json:"model"`
	ModelProxyURL                      optional[string]                     `json:"model_proxy_url"`
	ModelProxyAPIKey                   optional[string]                     `json:"model_proxy_api_key"`
	TraceProxy                         optional[bool]                       `json:"trace_proxy"`
	SystemPrompt                       optional[string]                     `json:"system_prompt"`
	NoEnv                              optional[bool]                       `json:"no_env"`
	MaxTurns                           optional[int]                        `json:"max_turns"`
	MaxPromptTokens                    optional[int]                        `json:"max_prompt_tokens"`
	MaxOutputTokens                    optional[int]                        `json:"max_output_tokens"`
	MaxPromptCostUSD                   optional[float64]                    `json:"max_prompt_cost_usd"`
	GoalMaxContinuations               optional[int]                        `json:"goal_max_continuations"`
	ToolTimeoutSeconds                 optional[int]                        `json:"tool_timeout_seconds"`
	RunCommandTimeoutSeconds           optional[int]                        `json:"run_command_timeout_seconds"`
	RunCommandBackgroundTimeoutSeconds optional[int]                        `json:"run_command_background_timeout_seconds"`
	DefaultContextWindow               optional[int]                        `json:"default_context_window"`
	ContextWindow                      optional[int]                        `json:"context_window"`
	Reasoning                          optional[string]                     `json:"reasoning"`
	ReasoningSummary                   optional[string]                     `json:"reasoning_summary"`
	ImageDetail                        optional[string]                     `json:"image_detail"`
	WebSearch                          optional[string]                     `json:"web_search"`
	AgentsMDWarnBytes                  optional[int]                        `json:"agents_md_warn_bytes"`
	ToolResultMaxBytes                 optional[int]                        `json:"tool_result_max_bytes"`
	ToolResultMaxLines                 optional[int]                        `json:"tool_result_max_lines"`
	RGResultMaxBytes                   optional[int]                        `json:"rg_result_max_bytes"`
	RGResultMaxLines                   optional[int]                        `json:"rg_result_max_lines"`
	GrepResultMaxBytes                 optional[int]                        `json:"grep_result_max_bytes"`
	GrepResultMaxLines                 optional[int]                        `json:"grep_result_max_lines"`
	ReadFileDefaultLimit               optional[int]                        `json:"read_file_default_limit"`
	ReadFileResultMaxBytes             optional[int]                        `json:"read_file_result_max_bytes"`
	ReadFileResultMaxLines             optional[int]                        `json:"read_file_result_max_lines"`
	CompactKeepTurns                   optional[int]                        `json:"compact_keep_turns"`
	CompactKeepTokens                  optional[int]                        `json:"compact_keep_tokens"`
	CompactAutoEnabled                 optional[bool]                       `json:"compact_auto_enabled"`
	CompactTriggerPercent              optional[int]                        `json:"compact_trigger_percent"`
	CompactTargetPercent               optional[int]                        `json:"compact_target_percent"`
	CompactIdleAfterSeconds            optional[int]                        `json:"compact_idle_after_seconds"`
	CompactIdleTriggerPercent          optional[int]                        `json:"compact_idle_trigger_percent"`
	CompactSummaryMaxTokens            optional[int]                        `json:"compact_summary_max_tokens"`
	CompactToolResultMaxBytes          optional[int]                        `json:"compact_tool_result_max_bytes"`
	DelegateMaxTurns                   optional[int]                        `json:"delegate_max_turns"`
	DelegateMaxDepth                   optional[int]                        `json:"delegate_max_depth"`
	DelegateOutput                     optional[string]                     `json:"delegate_output"`
	DelegateTmux                       optional[bool]                       `json:"delegate_tmux"`
	DelegateTmuxMaxWindows             optional[int]                        `json:"delegate_tmux_max_windows"`
	DelegateTmuxLayout                 optional[string]                     `json:"delegate_tmux_layout"`
	ResponsesStateful                  optional[bool]                       `json:"responses_stateful"`
	RetentionPolicy                    optional[string]                     `json:"retention_policy"`
	RetentionFloorTokens               optional[int]                        `json:"retention_floor_tokens"`
	NoSteer                            optional[bool]                       `json:"no_steer"`
	Verbose                            optional[bool]                       `json:"verbose"`
	ToolStream                         optional[bool]                       `json:"tool_stream"`
	ShowDiffs                          optional[bool]                       `json:"show_diffs"`
	LogLevel                           optional[string]                     `json:"log_level"`
	NoColor                            optional[bool]                       `json:"no_color"`
	ColorTheme                         optional[string]                     `json:"color_theme"`
	Timestamps                         optional[string]                     `json:"timestamps"`
	ReplPrompt                         optional[string]                     `json:"repl_prompt"`
	ReplEditMode                       optional[string]                     `json:"repl_edit_mode"`
	Agent                              optional[string]                     `json:"agent"`
	Agents                             optional[map[string]FileAgentConfig] `json:"agents"`
	HandoffAgent                       optional[string]                     `json:"handoff_agent"`
	Hooks                              optional[json.RawMessage]            `json:"hooks"`
	HookConfigs                        optional[[]string]                   `json:"hook_configs"`
	HistFile                           optional[string]                     `json:"histfile"`
	HistFileSize                       optional[int]                        `json:"histfilesize"`
	HistSize                           optional[int]                        `json:"histsize"`
	MCP                                optional[fileMCPConfig]              `json:"mcp"`
	LSP                                optional[fileLSPConfig]              `json:"lsp"`
}

type fileMCPConfig struct {
	Enable          optional[bool]               `json:"enable"`
	Proxy           optional[string]             `json:"proxy"`
	APIKey          optional[string]             `json:"api_key"`
	Headers         optional[map[string]string]  `json:"headers"`
	MaxTools        optional[int]                `json:"max_tools"`
	DisabledServers optional[[]string]           `json:"disabled_servers"`
	Local           optional[fileLocalMCPConfig] `json:"local"`
}

type fileLocalMCPConfig struct {
	Enable  optional[bool]              `json:"enable"`
	Command optional[string]            `json:"command"`
	Args    optional[[]string]          `json:"args"`
	Env     optional[map[string]string] `json:"env"`
}

type fileLSPConfig struct {
	Enable  optional[bool]                       `json:"enable"`
	Tools   optional[[]string]                   `json:"tools"`
	Servers optional[map[string]LSPServerConfig] `json:"servers"`
	Serena  optional[fileSerenaConfig]           `json:"serena"`
}

type fileSerenaConfig struct {
	Enable  optional[bool]              `json:"enable"`
	Command optional[string]            `json:"command"`
	Args    optional[[]string]          `json:"args"`
	Env     optional[map[string]string] `json:"env"`
}

func decodeConfigFile(path string) (fileConfig, error) {
	if path == "" {
		return fileConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, fmt.Errorf("read config %q: %w", path, err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fileConfig{}, fmt.Errorf("decode config %q: top level must be a JSON object", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var fc fileConfig
	if err := decoder.Decode(&fc); err != nil {
		return fileConfig{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fileConfig{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	normalizeConfigAtFileRefs(&fc, filepath.Dir(path))
	return fc, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func normalizeConfigAtFileRefs(fc *fileConfig, baseDir string) {
	if fc.SystemPrompt.Set {
		fc.SystemPrompt.Value = normalizeConfigAtFileRef(fc.SystemPrompt.Value, baseDir)
	}
	if fc.Agents.Set {
		for name, agent := range fc.Agents.Value {
			agent.Prompt = normalizeConfigAtFileRef(agent.Prompt, baseDir)
			fc.Agents.Value[name] = agent
		}
	}
}

func normalizeConfigAtFileRef(value, baseDir string) string {
	if baseDir == "" || !strings.HasPrefix(value, "@") || strings.HasPrefix(value, "@@") {
		return value
	}
	path := value[1:]
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
		return value
	}
	return "@" + filepath.Join(baseDir, path)
}

func expandStringMap(values map[string]string, lookup func(string) (string, bool), setting string) (map[string]string, error) {
	out := maps.Clone(values)
	for key, value := range out {
		expanded, err := expandEnvRefs(value, lookup)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", setting, key, err)
		}
		out[key] = expanded
	}
	return out, nil
}

func expandEnvRefs(value string, lookup func(string) (string, bool)) (string, error) {
	if !strings.ContainsRune(value, '$') {
		return value, nil
	}
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 >= len(value) || value[i+1] != '{' {
			out.WriteByte(value[i])
			i++
			continue
		}
		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			out.WriteByte(value[i])
			i++
			continue
		}
		end += i + 2
		body := value[i+2 : end]
		name, fallback, hasFallback := strings.Cut(body, ":-")
		if !validEnvName(name) {
			out.WriteString(value[i : end+1])
			i = end + 1
			continue
		}
		resolved, present := lookup(name)
		if hasFallback && (!present || resolved == "") {
			resolved, present = fallback, true
		}
		if !present {
			return "", fmt.Errorf("references unset variable ${%s}", name)
		}
		out.WriteString(resolved)
		i = end + 1
	}
	return out.String(), nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, char := range []byte(name) {
		if char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || i > 0 && char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}

func cloneLSPServers(in map[string]LSPServerConfig) map[string]LSPServerConfig {
	if in == nil {
		return nil
	}
	out := make(map[string]LSPServerConfig, len(in))
	for name, server := range in {
		server.Languages = append([]string(nil), server.Languages...)
		server.RootMarkers = append([]string(nil), server.RootMarkers...)
		server.Command = append([]string(nil), server.Command...)
		server.Extensions = append([]string(nil), server.Extensions...)
		server.Env = maps.Clone(server.Env)
		server.InitOptions = append(json.RawMessage(nil), server.InitOptions...)
		out[name] = server
	}
	return out
}

func SaveSelectedModel(path, model, reasoning string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config path is required")
	}
	canonical, err := canonicalReasoning(reasoning)
	if err != nil {
		return err
	}
	return updateConfigFile(path, func(raw map[string]json.RawMessage) error {
		raw["model"], _ = json.Marshal(model)
		if canonical == "" {
			delete(raw, "reasoning")
		} else {
			raw["reasoning"], _ = json.Marshal(canonical)
		}
		return nil
	})
}

func SaveReplEditMode(path, mode string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config path is required")
	}
	canonical, err := canonicalReplEditMode(mode)
	if err != nil {
		return err
	}
	return updateConfigFile(path, func(raw map[string]json.RawMessage) error {
		raw["repl_edit_mode"], _ = json.Marshal(canonical)
		return nil
	})
}

func updateConfigFile(path string, update func(map[string]json.RawMessage) error) error {
	raw := make(map[string]json.RawMessage)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	if err == nil {
		fc, decodeErr := decodeConfigFile(path)
		if decodeErr != nil {
			return decodeErr
		}
		if validateErr := validateFileConfig(fc, path); validateErr != nil {
			return validateErr
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		if decodeErr := decoder.Decode(&raw); decodeErr != nil {
			return fmt.Errorf("decode config %q: %w", path, decodeErr)
		}
		if decodeErr := requireJSONEOF(decoder); decodeErr != nil {
			return fmt.Errorf("decode config %q: %w", path, decodeErr)
		}
	}
	if err := update(raw); err != nil {
		return err
	}
	return writeConfigFile(path, raw)
}

func writeConfigFile(path string, raw map[string]json.RawMessage) error {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(raw); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(output.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
