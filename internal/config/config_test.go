package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"harness/internal/hooks"
	"harness/internal/replprompt"
)

// noEnv is a getenv that returns "" for everything: the default environment for
// tests that exercise flag/file/default precedence without env interference.
func noEnv(string) string { return "" }

// envFrom builds a getenv closure backed by a map.
func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// writeConfig writes a config file in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func loadOK(t *testing.T, args []string, getenv func(string) string, configPath string) Config {
	t.Helper()
	c, err := Load(args, getenv, configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

type precedenceCase[T comparable] struct {
	file     string
	env      map[string]string
	baseArgs []string
	flagArgs []string
	got      func(Config) T
	wantFlag T
	wantEnv  T
	wantFile T
}

func checkPrecedence[T comparable](t *testing.T, tc precedenceCase[T]) {
	t.Helper()
	cfgPath := writeConfig(t, tc.file)
	env := envFrom(tc.env)

	for _, step := range []struct {
		name string
		args []string
		env  func(string) string
		want T
	}{
		{name: "flag", args: append(append([]string{}, tc.baseArgs...), tc.flagArgs...), env: env, want: tc.wantFlag},
		{name: "env", args: tc.baseArgs, env: env, want: tc.wantEnv},
		{name: "file", args: tc.baseArgs, env: noEnv, want: tc.wantFile},
	} {
		t.Run(step.name, func(t *testing.T) {
			if got := tc.got(loadOK(t, step.args, step.env, cfgPath)); got != step.want {
				t.Fatalf("%s precedence: got %v, want %v", step.name, got, step.want)
			}
		})
	}
}

func TestModelPrecedenceFlagBeatsEnvBeatsFileBeatsDefault(t *testing.T) {
	// Flag wins over env and file.
	// Env wins over file when no flag.
	// File wins over default when no flag and no env.
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"model":"file-model"}`,
		env:      map[string]string{"HARNESS_MODEL": "env-model"},
		flagArgs: []string{"-model", "flag-model"},
		got:      func(c Config) string { return c.Model },
		wantFlag: "flag-model",
		wantEnv:  "env-model",
		wantFile: "file-model",
	})
}

func TestCompactionConfigDefaultsAndFileOverrides(t *testing.T) {
	defaults := loadOK(t, nil, noEnv, "")
	if defaults.CompactKeepTokens != 20_000 || !defaults.CompactAutoEnabled || defaults.CompactTriggerPercent != 78 || defaults.CompactTargetPercent != 65 || defaults.CompactIdleAfterSeconds != 0 || defaults.CompactIdleTriggerPercent != 35 {
		t.Fatalf("compaction defaults = tokens=%d auto=%t trigger=%d target=%d idle_after=%d idle_trigger=%d",
			defaults.CompactKeepTokens, defaults.CompactAutoEnabled, defaults.CompactTriggerPercent, defaults.CompactTargetPercent, defaults.CompactIdleAfterSeconds, defaults.CompactIdleTriggerPercent)
	}

	path := writeConfig(t, `{
		"compact_keep_tokens": 12345,
		"compact_auto_enabled": false,
		"compact_trigger_percent": 90,
		"compact_target_percent": 70,
		"compact_idle_after_seconds": 600,
		"compact_idle_trigger_percent": 35
	}`)
	got := loadOK(t, nil, noEnv, path)
	if got.CompactKeepTokens != 12_345 || got.CompactAutoEnabled || got.CompactTriggerPercent != 90 || got.CompactTargetPercent != 70 || got.CompactIdleAfterSeconds != 600 || got.CompactIdleTriggerPercent != 35 {
		t.Fatalf("compaction overrides = tokens=%d auto=%t trigger=%d target=%d idle_after=%d idle_trigger=%d",
			got.CompactKeepTokens, got.CompactAutoEnabled, got.CompactTriggerPercent, got.CompactTargetPercent, got.CompactIdleAfterSeconds, got.CompactIdleTriggerPercent)
	}
}

func TestCompactionConfigRejectsInvalidValues(t *testing.T) {
	for _, body := range []string{
		`{"compact_keep_tokens":0}`,
		`{"compact_target_percent":0}`,
		`{"compact_target_percent":78,"compact_trigger_percent":78}`,
		`{"compact_target_percent":90,"compact_trigger_percent":80}`,
		`{"compact_trigger_percent":100}`,
		`{"compact_idle_after_seconds":-1}`,
		`{"compact_idle_trigger_percent":0}`,
		`{"compact_idle_trigger_percent":100}`,
		`{"compact_idle_after_seconds":60,"compact_idle_trigger_percent":78}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := Load(nil, noEnv, writeConfig(t, body)); err == nil {
				t.Fatal("expected invalid compaction config to fail")
			}
		})
	}
}

// TestLoadSplitsProviderModel pins SplitProviderModel's contract at the Load
// call site, including the whitespace trimming the consolidated helper adopted
// from the REPL-side copy (regression: the two pre-merge copies had drifted).
func TestLoadSplitsProviderModel(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		wantProvider string
		wantModel    string
	}{
		{name: "plain split", model: "anthropic:claude-opus-4-8", wantProvider: "anthropic", wantModel: "claude-opus-4-8"},
		{name: "padded value is trimmed before split", model: " anthropic:claude-opus-4-8 ", wantProvider: "anthropic", wantModel: "claude-opus-4-8"},
		{name: "colon inside model id is not a provider prefix", model: "org/model:fp16", wantProvider: "", wantModel: "org/model:fp16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load([]string{"-model", tc.model}, noEnv, "")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.Provider != tc.wantProvider || c.Model != tc.wantModel {
				t.Fatalf("got provider=%q model=%q, want provider=%q model=%q", c.Provider, c.Model, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

func TestModelProxyURLPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"model_proxy_url":"http://file.example"}`,
		env:      map[string]string{"HARNESS_MODEL_PROXY_URL": "http://env.example"},
		flagArgs: []string{"-model-proxy-url", "http://flag.example"},
		got:      func(c Config) string { return c.ModelProxyURL },
		wantFlag: "http://flag.example",
		wantEnv:  "http://env.example",
		wantFile: "http://file.example",
	})
}

func TestModelProxyAPIKeyPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"model_proxy_api_key":"file-key"}`,
		env:      map[string]string{"HARNESS_MODEL_PROXY_API_KEY": "env-key"},
		flagArgs: []string{"-model-proxy-api-key", "flag-key"},
		got:      func(c Config) string { return c.ModelProxyAPIKey },
		wantFlag: "flag-key",
		wantEnv:  "env-key",
		wantFile: "file-key",
	})
}

func TestMCPProxyAPIKeyPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"mcp":{"api_key":"file-key"}}`,
		env:      map[string]string{"HARNESS_MCP_PROXY_API_KEY": "env-key"},
		flagArgs: []string{"-mcp-proxy-api-key", "flag-key"},
		got:      func(c Config) string { return c.MCP.APIKey },
		wantFlag: "flag-key",
		wantEnv:  "env-key",
		wantFile: "file-key",
	})
}

func TestTraceProxyPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"trace_proxy":true}`,
		env:      map[string]string{"HARNESS_TRACE_PROXY": "false"},
		flagArgs: []string{"-trace-proxy=true"},
		got:      func(c Config) bool { return c.TraceProxy },
		wantFlag: true,
		wantEnv:  false,
		wantFile: true,
	})
}

// HARNESS_* env mapping covers the user-facing flags.
func TestHarnessEnvMapping(t *testing.T) {
	env := envFrom(map[string]string{
		"HARNESS_MODEL":                      "env-model",
		"HARNESS_MODEL_PROXY_URL":            "http://proxy.example",
		"HARNESS_MAX_TURNS":                  "12",
		"HARNESS_DEFAULT_CONTEXT_WINDOW":     "512000",
		"HARNESS_CONTEXT_WINDOW":             "256000",
		"HARNESS_REASONING":                  "HIGH",
		"HARNESS_REASONING_SUMMARY":          "AUTO",
		"HARNESS_RESPONSES_STATEFUL":         "true",
		"HARNESS_RETENTION_POLICY":           "pressure",
		"HARNESS_TRACE_PROXY":                "true",
		"HARNESS_TOOL_RESULT_MAX_BYTES":      "32768",
		"HARNESS_TOOL_RESULT_MAX_LINES":      "500",
		"HARNESS_RG_RESULT_MAX_BYTES":        "24576",
		"HARNESS_RG_RESULT_MAX_LINES":        "300",
		"HARNESS_GREP_RESULT_MAX_BYTES":      "20480",
		"HARNESS_GREP_RESULT_MAX_LINES":      "250",
		"HARNESS_READ_FILE_DEFAULT_LIMIT":    "400",
		"HARNESS_READ_FILE_RESULT_MAX_BYTES": "28672",
		"HARNESS_READ_FILE_RESULT_MAX_LINES": "450",
		"HARNESS_SYSTEM_PROMPT":              "env system prompt",
		"HARNESS_NO_ENV":                     "true",
		"HARNESS_NO_COLOR":                   "true",
		"HARNESS_TIMESTAMPS":                 "full",
		"HARNESS_VERBOSE":                    "true",
		"HARNESS_TOOL_STREAM":                "false",
		"HARNESS_SHOW_DIFFS":                 "true",
		"HARNESS_REPL_PROMPT":                "env> ",
		"HARNESS_REPL_EDIT_MODE":             "vi",
		"HARNESS_DELEGATE_TMUX_LAYOUT":       "window",
		"LOG_LEVEL":                          "WARN",
	})
	c, err := Load(nil, env, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Model != "env-model" {
		t.Fatalf("model %q", c.Model)
	}
	if c.ModelProxyURL != "http://proxy.example" {
		t.Fatalf("model proxy URL %q", c.ModelProxyURL)
	}
	if c.MaxTurns != 12 {
		t.Fatalf("max-turns %d, want 12", c.MaxTurns)
	}
	if c.DefaultContextWindow != 512000 {
		t.Fatalf("default-context-window %d, want 512000", c.DefaultContextWindow)
	}
	if c.ContextWindow != 256000 {
		t.Fatalf("context-window %d, want 256000", c.ContextWindow)
	}
	if c.Reasoning != "high" {
		t.Fatalf("reasoning %q, want high", c.Reasoning)
	}
	if c.ReasoningSummary != "auto" {
		t.Fatalf("reasoning summary = %q, want auto", c.ReasoningSummary)
	}
	if !c.ResponsesStateful {
		t.Fatalf("responses_stateful false, want true")
	}
	if c.RetentionPolicy != "pressure" {
		t.Fatalf("retention_policy = %q, want pressure", c.RetentionPolicy)
	}
	if !c.TraceProxy {
		t.Fatalf("trace_proxy false, want true")
	}
	if c.ToolResultMaxBytes != 32768 {
		t.Fatalf("tool result max bytes = %d, want 32768", c.ToolResultMaxBytes)
	}
	if c.ToolResultMaxLines != 500 {
		t.Fatalf("tool result max lines = %d, want 500", c.ToolResultMaxLines)
	}
	if c.RGResultMaxBytes != 24576 || c.RGResultMaxLines != 300 {
		t.Fatalf("rg result limits = %d/%d, want 24576/300", c.RGResultMaxBytes, c.RGResultMaxLines)
	}
	if c.GrepResultMaxBytes != 20480 || c.GrepResultMaxLines != 250 {
		t.Fatalf("grep result limits = %d/%d, want 20480/250", c.GrepResultMaxBytes, c.GrepResultMaxLines)
	}
	if c.ReadFileDefaultLimit != 400 {
		t.Fatalf("read file default limit = %d, want 400", c.ReadFileDefaultLimit)
	}
	if c.ReadFileResultMaxBytes != 28672 || c.ReadFileResultMaxLines != 450 {
		t.Fatalf("read file result limits = %d/%d, want 28672/450", c.ReadFileResultMaxBytes, c.ReadFileResultMaxLines)
	}
	if c.SystemPrompt != "env system prompt" {
		t.Fatalf("system prompt %q", c.SystemPrompt)
	}
	if !c.NoEnv {
		t.Fatalf("no-env false, want true")
	}
	if !c.NoColor {
		t.Fatalf("no-color false, want true")
	}
	if c.TimestampMode != TimestampFull {
		t.Fatalf("timestamp mode %q, want full", c.TimestampMode)
	}
	if !c.Verbose {
		t.Fatalf("verbose false, want true")
	}
	if c.ToolStream {
		t.Fatalf("tool-stream true, want false")
	}
	if !c.ShowDiffs {
		t.Fatalf("show-diffs false, want true")
	}
	if c.LogLevel != "warn" {
		t.Fatalf("log level %q, want warn", c.LogLevel)
	}
	if c.ReplPrompt != "env> " {
		t.Fatalf("repl prompt %q, want env> ", c.ReplPrompt)
	}
	if c.ReplEditMode != "vi" {
		t.Fatalf("repl edit mode %q, want vi", c.ReplEditMode)
	}
	if c.DelegateTmuxLayout != "window" {
		t.Fatalf("delegate_tmux_layout %q, want window", c.DelegateTmuxLayout)
	}
}

func TestTimestampsPrecedenceAndAliases(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"timestamps":"none"}`,
		env:      map[string]string{"HARNESS_TIMESTAMPS": "long"},
		flagArgs: []string{"-timestamps", "short"},
		got:      func(c Config) string { return c.TimestampMode },
		wantFlag: TimestampShort,
		wantEnv:  TimestampFull,
		wantFile: TimestampNone,
	})

	c := loadOK(t, []string{"-no-timestamps"}, noEnv, "")
	if c.TimestampMode != TimestampNone {
		t.Fatalf("-no-timestamps mode %q, want none", c.TimestampMode)
	}

	c = loadOK(t, nil, envFrom(map[string]string{"HARNESS_NO_TIMESTAMPS": "true"}), "")
	if c.TimestampMode != TimestampNone {
		t.Fatalf("HARNESS_NO_TIMESTAMPS mode %q, want none", c.TimestampMode)
	}
}

func TestTimestampsRejectsInvalidMode(t *testing.T) {
	if _, err := Load([]string{"-timestamps", "verbose"}, noEnv, ""); err == nil {
		t.Fatalf("expected invalid timestamp mode to fail")
	}
}

func TestReplPromptPrecedence(t *testing.T) {
	// Default names the active agent.
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.ReplPrompt != replprompt.DefaultFormat {
		t.Fatalf("default repl prompt %q, want %q", c.ReplPrompt, replprompt.DefaultFormat)
	}

	// File overrides default.
	// Env overrides file.
	// Flag overrides all.
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"repl_prompt":"$ "}`,
		env:      map[string]string{"HARNESS_REPL_PROMPT": "# "},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-repl-prompt", ">>> "},
		got:      func(c Config) string { return c.ReplPrompt },
		wantFlag: ">>> ",
		wantEnv:  "# ",
		wantFile: "$ ",
	})
}

func TestReplPromptValidation(t *testing.T) {
	if _, err := Load([]string{"-repl-prompt", "{missing}"}, noEnv, ""); err == nil {
		t.Fatalf("expected unknown repl_prompt placeholder to fail")
	}
	if _, err := Load([]string{"-repl-prompt", `line\n{agent}@{hostname}> `}, noEnv, ""); err != nil {
		t.Fatalf("escaped newline and hostname prompt should load: %v", err)
	}
	if _, err := Load([]string{"-repl-prompt", `{reasoning}> `}, noEnv, ""); err != nil {
		t.Fatalf("reasoning prompt should load: %v", err)
	}
	// The vimode placeholder variants are valid config and should load.
	for _, p := range []string{"{vimode}> ", "{vimode:long}> ", "{vimode:short}> "} {
		if _, err := Load([]string{"-repl-prompt", p}, noEnv, ""); err != nil {
			t.Fatalf("vimode prompt %q should load: %v", p, err)
		}
	}
	// The hostname placeholder variants are valid config and should load.
	for _, p := range []string{"{hostname}> ", "{hostname:long}> ", "{hostname:short}> "} {
		if _, err := Load([]string{"-repl-prompt", p}, noEnv, ""); err != nil {
			t.Fatalf("hostname prompt %q should load: %v", p, err)
		}
	}
	// An unknown hostname style is still an unregistered field and must fail.
	if _, err := Load([]string{"-repl-prompt", "{hostname:bogus}> "}, noEnv, ""); err == nil {
		t.Fatalf("expected {hostname:bogus} repl_prompt placeholder to fail")
	}
	// An unknown vimode style is still an unregistered field and must fail.
	if _, err := Load([]string{"-repl-prompt", "{vimode:bogus}> "}, noEnv, ""); err == nil {
		t.Fatalf("expected {vimode:bogus} repl_prompt placeholder to fail")
	}
}

func TestReplEditModePrecedenceAndValidation(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.ReplEditMode != DefaultReplEditMode {
		t.Fatalf("default repl edit mode %q, want %q", c.ReplEditMode, DefaultReplEditMode)
	}

	checkPrecedence(t, precedenceCase[string]{
		file:     `{"repl_edit_mode":"vi"}`,
		env:      map[string]string{"HARNESS_REPL_EDIT_MODE": "emacs"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-repl-edit-mode", "vim"},
		got:      func(c Config) string { return c.ReplEditMode },
		wantFlag: "vi",
		wantEnv:  DefaultReplEditMode,
		wantFile: "vi",
	})

	if _, err := Load([]string{"-repl-edit-mode", "not-a-mode"}, noEnv, ""); err == nil {
		t.Fatalf("expected invalid repl edit mode to fail")
	}
}

func TestToolStreamPrecedence(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.ToolStream {
		t.Fatalf("default tool-stream true, want false")
	}

	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"tool_stream":true}`,
		env:      map[string]string{"HARNESS_TOOL_STREAM": "false"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-tool-stream"},
		got:      func(c Config) bool { return c.ToolStream },
		wantFlag: true,
		wantEnv:  false,
		wantFile: true,
	})
}

func TestShowDiffsPrecedence(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if !c.ShowDiffs {
		t.Fatalf("default show-diffs false, want true")
	}

	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"show_diffs":false}`,
		env:      map[string]string{"HARNESS_SHOW_DIFFS": "true"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-show-diffs=false"},
		got:      func(c Config) bool { return c.ShowDiffs },
		wantFlag: false,
		wantEnv:  true,
		wantFile: false,
	})
}

func TestResponsesStatefulDefaultAndPrecedence(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if !c.ResponsesStateful {
		t.Fatalf("default responses_stateful false, want true")
	}

	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"responses_stateful":false}`,
		env:      map[string]string{"HARNESS_RESPONSES_STATEFUL": "true"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-responses-stateful=false"},
		got:      func(c Config) bool { return c.ResponsesStateful },
		wantFlag: false,
		wantEnv:  true,
		wantFile: false,
	})
}

func TestRetentionPolicyDefaultPrecedenceAndValidation(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.RetentionPolicy != "auto" {
		t.Fatalf("default retention_policy = %q, want auto", c.RetentionPolicy)
	}

	checkPrecedence(t, precedenceCase[string]{
		file:     `{"retention_policy":"age"}`,
		env:      map[string]string{"HARNESS_RETENTION_POLICY": "disabled"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-retention-policy", "pressure"},
		got:      func(c Config) string { return c.RetentionPolicy },
		wantFlag: "pressure",
		wantEnv:  "disabled",
		wantFile: "age",
	})

	if _, err := Load([]string{"-retention-policy", "unknown"}, noEnv, ""); err == nil ||
		!strings.Contains(err.Error(), "invalid retention_policy") {
		t.Fatalf("invalid retention policy error = %v", err)
	}
}

func TestNoSteerDefaultAndPrecedence(t *testing.T) {
	// Default is false (steering on); -no-steer / HARNESS_NO_STEER / no_steer enable it.
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.NoSteer {
		t.Fatalf("default no_steer true, want false (steering on)")
	}

	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"no_steer":true}`,
		env:      map[string]string{"HARNESS_NO_STEER": "false"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-no-steer"},
		got:      func(c Config) bool { return c.NoSteer },
		wantFlag: true,
		wantEnv:  false,
		wantFile: true,
	})
}

func TestWebSearchPrecedenceAndValidation(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"web_search":"off"}`,
		env:      map[string]string{"HARNESS_WEB_SEARCH": "true"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-web-search", "off"},
		got:      func(c Config) string { return c.WebSearch },
		wantFlag: "off",
		wantEnv:  "auto",
		wantFile: "off",
	})

	if _, err := Load([]string{"-web-search", "always"}, noEnv, ""); err == nil {
		t.Fatalf("expected invalid web_search to fail")
	}
}

func TestToolResultLimitPrecedenceEnvBeatsFile(t *testing.T) {
	cfgPath := writeConfig(t, `{
		"tool_result_max_bytes":111,
		"tool_result_max_lines":222,
		"rg_result_max_bytes":11,
		"rg_result_max_lines":12,
		"grep_result_max_bytes":21,
		"grep_result_max_lines":22,
		"read_file_default_limit":31,
		"read_file_result_max_bytes":32,
		"read_file_result_max_lines":33
	}`)

	fileCfg := loadOK(t, nil, noEnv, cfgPath)
	if fileCfg.ToolResultMaxBytes != 111 || fileCfg.ToolResultMaxLines != 222 {
		t.Fatalf("file tool result limits = %d/%d, want 111/222", fileCfg.ToolResultMaxBytes, fileCfg.ToolResultMaxLines)
	}
	if fileCfg.RGResultMaxBytes != 11 || fileCfg.RGResultMaxLines != 12 {
		t.Fatalf("file rg limits = %d/%d, want 11/12", fileCfg.RGResultMaxBytes, fileCfg.RGResultMaxLines)
	}
	if fileCfg.GrepResultMaxBytes != 21 || fileCfg.GrepResultMaxLines != 22 {
		t.Fatalf("file grep limits = %d/%d, want 21/22", fileCfg.GrepResultMaxBytes, fileCfg.GrepResultMaxLines)
	}
	if fileCfg.ReadFileDefaultLimit != 31 || fileCfg.ReadFileResultMaxBytes != 32 || fileCfg.ReadFileResultMaxLines != 33 {
		t.Fatalf("file read_file limits = default %d result %d/%d, want 31 and 32/33",
			fileCfg.ReadFileDefaultLimit, fileCfg.ReadFileResultMaxBytes, fileCfg.ReadFileResultMaxLines)
	}

	envCfg := loadOK(t, nil, envFrom(map[string]string{
		"HARNESS_TOOL_RESULT_MAX_BYTES":      "333",
		"HARNESS_TOOL_RESULT_MAX_LINES":      "444",
		"HARNESS_RG_RESULT_MAX_BYTES":        "55",
		"HARNESS_RG_RESULT_MAX_LINES":        "56",
		"HARNESS_GREP_RESULT_MAX_BYTES":      "65",
		"HARNESS_GREP_RESULT_MAX_LINES":      "66",
		"HARNESS_READ_FILE_DEFAULT_LIMIT":    "75",
		"HARNESS_READ_FILE_RESULT_MAX_BYTES": "76",
		"HARNESS_READ_FILE_RESULT_MAX_LINES": "77",
	}), cfgPath)
	if envCfg.ToolResultMaxBytes != 333 || envCfg.ToolResultMaxLines != 444 {
		t.Fatalf("env tool result limits = %d/%d, want 333/444", envCfg.ToolResultMaxBytes, envCfg.ToolResultMaxLines)
	}
	if envCfg.RGResultMaxBytes != 55 || envCfg.RGResultMaxLines != 56 {
		t.Fatalf("env rg limits = %d/%d, want 55/56", envCfg.RGResultMaxBytes, envCfg.RGResultMaxLines)
	}
	if envCfg.GrepResultMaxBytes != 65 || envCfg.GrepResultMaxLines != 66 {
		t.Fatalf("env grep limits = %d/%d, want 65/66", envCfg.GrepResultMaxBytes, envCfg.GrepResultMaxLines)
	}
	if envCfg.ReadFileDefaultLimit != 75 || envCfg.ReadFileResultMaxBytes != 76 || envCfg.ReadFileResultMaxLines != 77 {
		t.Fatalf("env read_file limits = default %d result %d/%d, want 75 and 76/77",
			envCfg.ReadFileDefaultLimit, envCfg.ReadFileResultMaxBytes, envCfg.ReadFileResultMaxLines)
	}
}

// NO_COLOR (the de-facto standard env var) disables color independent of HARNESS_*.
func TestNoColorStandardEnv(t *testing.T) {
	env := envFrom(map[string]string{"NO_COLOR": "1"})
	c, err := Load([]string{"-model", "gpt-5.5"}, env, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.NoColor {
		t.Fatalf("NO_COLOR did not disable color")
	}
}

func TestMaxTurnsDefault(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxTurns != 250 {
		t.Fatalf("default max-turns %d, want 250", c.MaxTurns)
	}
}

func TestDefaultContextWindowDefault(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DefaultContextWindow != 256_000 {
		t.Fatalf("default context window %d, want 256000", c.DefaultContextWindow)
	}
}

func TestDefaultContextWindowPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[int]{
		file:     `{"default_context_window":300000}`,
		env:      map[string]string{"HARNESS_DEFAULT_CONTEXT_WINDOW": "400000"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-default-context-window", "500000"},
		got:      func(c Config) int { return c.DefaultContextWindow },
		wantFlag: 500000,
		wantEnv:  400000,
		wantFile: 300000,
	})
}

func TestHandoffAgentPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"handoff_agent":"filey"}`,
		env:      map[string]string{"HARNESS_HANDOFF_AGENT": "envy"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-handoff-agent", "Flaggy"},
		got:      func(c Config) string { return c.HandoffAgent },
		wantFlag: "flaggy",
		wantEnv:  "envy",
		wantFile: "filey",
	})
}

func TestHandoffAgentDefaultsToAuto(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.HandoffAgent != "auto" {
		t.Errorf("HandoffAgent = %q, want auto by default", c.HandoffAgent)
	}
}

func TestAgentReasoningCarriedFromConfig(t *testing.T) {
	cfg := writeConfig(t, `{"agents":{"fast":{"model":"cheap","reasoning":"low"}}}`)
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, cfg)
	if c.Agents["fast"].Reasoning != "low" {
		t.Errorf("agent reasoning = %q, want low", c.Agents["fast"].Reasoning)
	}
}

func TestReasoningPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"reasoning":"low"}`,
		env:      map[string]string{"HARNESS_REASONING": "medium"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-reasoning", "HIGH"},
		got:      func(c Config) string { return c.Reasoning },
		wantFlag: "high",
		wantEnv:  "medium",
		wantFile: "low",
	})
}

func TestReasoningSummaryPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"reasoning_summary":"concise"}`,
		env:      map[string]string{"HARNESS_REASONING_SUMMARY": "detailed"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-reasoning-summary", "AUTO"},
		got:      func(c Config) string { return c.ReasoningSummary },
		wantFlag: "auto",
		wantEnv:  "detailed",
		wantFile: "concise",
	})
}

func TestReasoningSummaryAliases(t *testing.T) {
	c, err := Load([]string{"-reasoning-summary", "on"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load on: %v", err)
	}
	if c.ReasoningSummary != "auto" {
		t.Fatalf("on summary = %q, want auto", c.ReasoningSummary)
	}
	c, err = Load([]string{"-reasoning-summary", "off"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load off: %v", err)
	}
	if c.ReasoningSummary != "none" {
		t.Fatalf("off summary = %q, want none", c.ReasoningSummary)
	}
}

func TestBadReasoningSummaryValueIsUsageError(t *testing.T) {
	_, err := Load([]string{"-reasoning-summary", "verbose"}, noEnv, "")
	if err == nil {
		t.Fatalf("expected usage error for invalid -reasoning-summary")
	}
}

func TestBadReasoningProfileValueIsUsageError(t *testing.T) {
	_, err := Load([]string{"-reasoning", "ultra"}, noEnv, "")
	if err == nil {
		t.Fatalf("expected usage error for invalid -reasoning")
	}
}

func TestImageDetailPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"image_detail":"low"}`,
		env:      map[string]string{"HARNESS_IMAGE_DETAIL": "high"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-image-detail", "ORIGINAL"},
		got:      func(c Config) string { return c.ImageDetail },
		wantFlag: "original",
		wantEnv:  "high",
		wantFile: "low",
	})
}

func TestImageFlagsRepeatAndDetailPrefix(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5", "-image-detail", "low", "-image", "screen.png", "-image", "high:detail.png"}, noEnv, "")
	if len(c.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(c.Images))
	}
	if c.Images[0].Path != "screen.png" || c.Images[0].Detail != "low" {
		t.Fatalf("first image = %+v", c.Images[0])
	}
	if c.Images[1].Path != "detail.png" || c.Images[1].Detail != "high" {
		t.Fatalf("second image = %+v", c.Images[1])
	}
}

func TestImageDetailRejectsUnknown(t *testing.T) {
	if _, err := Load([]string{"-model", "gpt-5.5", "-image-detail", "zoom"}, noEnv, ""); err == nil {
		t.Fatal("Load accepted invalid image detail")
	}
}

func TestToolTimeoutResolution(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ToolTimeoutSeconds != defaultToolTimeoutSeconds {
		t.Fatalf("default tool-timeout %d, want %d", c.ToolTimeoutSeconds, defaultToolTimeoutSeconds)
	}

	cfgPath := writeConfig(t, `{"tool_timeout_seconds":900}`)
	c, err = Load([]string{"-tool-timeout", "45"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ToolTimeoutSeconds != 45 {
		t.Fatalf("tool-timeout %d, want 45 (flag beats file)", c.ToolTimeoutSeconds)
	}

	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ToolTimeoutSeconds != 900 {
		t.Fatalf("tool-timeout %d, want 900 (file beats default)", c.ToolTimeoutSeconds)
	}

	c, err = Load(nil, envFrom(map[string]string{"HARNESS_TOOL_TIMEOUT": "120"}), "")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if c.ToolTimeoutSeconds != 120 {
		t.Fatalf("env tool-timeout %d, want 120", c.ToolTimeoutSeconds)
	}
}

func TestMaxPromptTokensResolution(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxPromptTokens != 0 {
		t.Fatalf("default max-prompt-tokens %d, want 0 (unlimited)", c.MaxPromptTokens)
	}

	c, err = Load([]string{"-max-prompt-tokens", "50000"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxPromptTokens != 50000 {
		t.Fatalf("max-prompt-tokens %d, want 50000", c.MaxPromptTokens)
	}
}

func TestMaxOutputTokensPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[int]{
		file:     `{"max_output_tokens":12000}`,
		env:      map[string]string{"HARNESS_MAX_OUTPUT_TOKENS": "16000"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-max-output-tokens", "24000"},
		got:      func(c Config) int { return c.MaxOutputTokens },
		wantFlag: 24000,
		wantEnv:  16000,
		wantFile: 12000,
	})
}

func TestMaxPromptCostResolution(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxPromptCostUSD != 0 {
		t.Fatalf("default max-prompt-cost %v, want 0 (unlimited)", c.MaxPromptCostUSD)
	}

	c, err = Load([]string{"-max-prompt-cost", "2.50"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxPromptCostUSD != 2.50 {
		t.Fatalf("max-prompt-cost %v, want 2.50 (flag)", c.MaxPromptCostUSD)
	}

	cfgPath := writeConfig(t, `{"max_prompt_cost_usd":1.0}`)
	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxPromptCostUSD != 1.0 {
		t.Fatalf("max-prompt-cost %v, want 1.0 (file)", c.MaxPromptCostUSD)
	}

	c, err = Load(nil, envFrom(map[string]string{"HARNESS_MAX_PROMPT_COST": "3.25"}), "")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if c.MaxPromptCostUSD != 3.25 {
		t.Fatalf("env max-prompt-cost %v, want 3.25", c.MaxPromptCostUSD)
	}
}

func TestMaxTurnsFlagBeatsFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"max_turns":7}`)
	c, err := Load([]string{"-max-turns", "9"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxTurns != 9 {
		t.Fatalf("max-turns %d, want 9 (flag beats file)", c.MaxTurns)
	}

	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxTurns != 7 {
		t.Fatalf("max-turns %d, want 7 (file beats default)", c.MaxTurns)
	}
}

func TestMaxTurnsAllowsNonPositiveUnlimited(t *testing.T) {
	c, err := Load([]string{"-max-turns", "0"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load flag: %v", err)
	}
	if c.MaxTurns != 0 {
		t.Fatalf("flag max-turns %d, want 0", c.MaxTurns)
	}

	c, err = Load(nil, envFrom(map[string]string{"HARNESS_MAX_TURNS": "-1"}), "")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if c.MaxTurns != -1 {
		t.Fatalf("env max-turns %d, want -1", c.MaxTurns)
	}

	cfgPath := writeConfig(t, `{"max_turns":0}`)
	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load file: %v", err)
	}
	if c.MaxTurns != 0 {
		t.Fatalf("file max-turns %d, want 0", c.MaxTurns)
	}
}

func TestDelegateMaxTurnsConfigOnly(t *testing.T) {
	c, err := Load(nil, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DelegateMaxTurns != 20 {
		t.Fatalf("default delegate max turns = %d, want 20", c.DelegateMaxTurns)
	}

	cfgPath := writeConfig(t, `{"delegate_max_turns":5}`)
	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DelegateMaxTurns != 5 {
		t.Fatalf("file delegate max turns = %d, want 5", c.DelegateMaxTurns)
	}
}

func TestDelegateMaxTurnsMustBePositive(t *testing.T) {
	cfgPath := writeConfig(t, `{"delegate_max_turns":0}`)
	if _, err := Load(nil, noEnv, cfgPath); err == nil {
		t.Fatal("delegate_max_turns=0 should be invalid")
	}
}

func TestDelegateMaxDepthConfigOnly(t *testing.T) {
	c, err := Load(nil, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DelegateMaxDepth != 3 {
		t.Fatalf("default delegate max depth = %d, want 3", c.DelegateMaxDepth)
	}

	cfgPath := writeConfig(t, `{"delegate_max_depth":5}`)
	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DelegateMaxDepth != 5 {
		t.Fatalf("file delegate max depth = %d, want 5", c.DelegateMaxDepth)
	}
}

func TestDelegateMaxDepthMustBePositive(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		cfgPath := writeConfig(t, `{"delegate_max_depth":`+value+`}`)
		if _, err := Load(nil, noEnv, cfgPath); err == nil {
			t.Fatalf("delegate_max_depth=%s should be invalid", value)
		}
	}
}

func TestDelegateOutputPrecedence(t *testing.T) {
	c, err := Load(nil, noEnv, "")
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if c.DelegateOutput != DelegateOutputStatus {
		t.Fatalf("default delegate output = %q, want status", c.DelegateOutput)
	}

	cfgPath := writeConfig(t, `{"delegate_output":"lines"}`)
	c, err = Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load file: %v", err)
	}
	if c.DelegateOutput != DelegateOutputLines {
		t.Fatalf("file delegate output = %q, want lines", c.DelegateOutput)
	}

	env := envFrom(map[string]string{"HARNESS_DELEGATE_OUTPUT": " OFF "})
	c, err = Load(nil, env, cfgPath)
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if c.DelegateOutput != DelegateOutputOff {
		t.Fatalf("env delegate output = %q, want off", c.DelegateOutput)
	}

	c, err = Load([]string{"-delegate-output", "status"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load flag: %v", err)
	}
	if c.DelegateOutput != DelegateOutputStatus {
		t.Fatalf("flag delegate output = %q, want status", c.DelegateOutput)
	}
}

func TestDelegateTmuxPrecedenceFlagBeatsEnvBeatsFileBeatsDefault(t *testing.T) {
	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"delegate_tmux":true}`,
		env:      map[string]string{"HARNESS_DELEGATE_TMUX": "true"},
		flagArgs: []string{"-delegate-tmux"},
		got:      func(c Config) bool { return c.DelegateTmux },
		wantFlag: true,
		wantEnv:  true,
		wantFile: true,
	})

	c := loadOK(t, nil, noEnv, "")
	if c.DelegateTmux {
		t.Fatal("delegate_tmux should default to off")
	}
	if c.DelegateTmuxCLI != "default" {
		t.Fatalf("delegate_tmux_cli default = %q, want default", c.DelegateTmuxCLI)
	}
	if c.DelegateTmuxMaxWindows != 4 {
		t.Fatalf("delegate_tmux_max_windows default = %d, want 4", c.DelegateTmuxMaxWindows)
	}
}

func TestDelegateTmuxCLITracksWinningSource(t *testing.T) {
	cfgPath := writeConfig(t, `{"delegate_tmux":true}`)

	// Flag wins over env and file, even when env disagrees.
	c := loadOK(t, []string{"-delegate-tmux"}, envFrom(map[string]string{"HARNESS_DELEGATE_TMUX": "false"}), cfgPath)
	if !c.DelegateTmux || c.DelegateTmuxCLI != "flag" {
		t.Fatalf("flag source: tmux=%t cli=%q", c.DelegateTmux, c.DelegateTmuxCLI)
	}
	// Env wins over file.
	c = loadOK(t, nil, envFrom(map[string]string{"HARNESS_DELEGATE_TMUX": "true"}), cfgPath)
	if !c.DelegateTmux || c.DelegateTmuxCLI != "env" {
		t.Fatalf("env source: tmux=%t cli=%q", c.DelegateTmux, c.DelegateTmuxCLI)
	}
	// File alone.
	c = loadOK(t, nil, noEnv, cfgPath)
	if !c.DelegateTmux || c.DelegateTmuxCLI != "file" {
		t.Fatalf("file source: tmux=%t cli=%q", c.DelegateTmux, c.DelegateTmuxCLI)
	}
	// Nothing set.
	c = loadOK(t, nil, noEnv, "")
	if c.DelegateTmux || c.DelegateTmuxCLI != "default" {
		t.Fatalf("default source: tmux=%t cli=%q", c.DelegateTmux, c.DelegateTmuxCLI)
	}
}

func TestDelegateTmuxMaxWindowsConfig(t *testing.T) {
	cfgPath := writeConfig(t, `{"delegate_tmux_max_windows":7}`)
	if c := loadOK(t, nil, noEnv, cfgPath); c.DelegateTmuxMaxWindows != 7 {
		t.Fatalf("file delegate_tmux_max_windows = %d, want 7", c.DelegateTmuxMaxWindows)
	}
	for _, value := range []string{"0", "-1"} {
		cfgPath := writeConfig(t, `{"delegate_tmux_max_windows":`+value+`}`)
		if _, err := Load(nil, noEnv, cfgPath); err == nil {
			t.Fatalf("delegate_tmux_max_windows=%s should be invalid", value)
		}
	}
}

func TestDelegateTmuxLayoutPrecedenceFlagBeatsEnvBeatsFileBeatsDefault(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"delegate_tmux_layout":"window"}`,
		env:      map[string]string{"HARNESS_DELEGATE_TMUX_LAYOUT": "pane"},
		flagArgs: []string{"-delegate-tmux-layout", "window"},
		got:      func(c Config) string { return c.DelegateTmuxLayout },
		wantFlag: "window",
		wantEnv:  "pane",
		wantFile: "window",
	})

	c := loadOK(t, nil, noEnv, "")
	if c.DelegateTmuxLayout != DelegateTmuxLayoutPane {
		t.Fatalf("delegate_tmux_layout default = %q, want pane", c.DelegateTmuxLayout)
	}
}

func TestDelegateTmuxLayoutRejectsUnknownValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		getenv func(string) string
		file   string
	}{
		{name: "file", getenv: noEnv, file: `{"delegate_tmux_layout":"vertical"}`},
		{name: "environment", getenv: envFrom(map[string]string{"HARNESS_DELEGATE_TMUX_LAYOUT": "vertical"})},
		{name: "flag", args: []string{"-delegate-tmux-layout", "vertical"}, getenv: noEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.file != "" {
				path = writeConfig(t, tc.file)
			}
			_, err := Load(tc.args, tc.getenv, path)
			if err == nil || !strings.Contains(err.Error(), "delegate_tmux_layout") {
				t.Fatalf("Load error = %v, want delegate_tmux_layout validation error", err)
			}
		})
	}
}

func TestDelegateOutputRejectsUnknownMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		getenv func(string) string
		file   string
	}{
		{name: "file", getenv: noEnv, file: `{"delegate_output":"stream"}`},
		{name: "environment", getenv: envFrom(map[string]string{"HARNESS_DELEGATE_OUTPUT": "stream"})},
		{name: "flag", args: []string{"-delegate-output", "stream"}, getenv: noEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.file != "" {
				path = writeConfig(t, tc.file)
			}
			_, err := Load(tc.args, tc.getenv, path)
			if err == nil || !strings.Contains(err.Error(), "delegate_output") {
				t.Fatalf("Load error = %v, want delegate_output validation error", err)
			}
		})
	}
}

func TestBoolFlagsParsed(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5", "-no-env", "-no-color", "-v", "-q"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.NoEnv || !c.NoColor || !c.Verbose || !c.Quiet {
		t.Fatalf("bool flags not all set: %+v", c)
	}
}

func TestQuietLongFlagParsed(t *testing.T) {
	c, err := Load([]string{"--quiet"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Quiet {
		t.Fatalf("Quiet = false, want true")
	}
}

func TestShowConfigFlagParsed(t *testing.T) {
	c, err := Load([]string{"--show-config"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ShowConfig {
		t.Fatalf("ShowConfig = false, want true")
	}
}

func TestListFlagsParsed(t *testing.T) {
	c, err := Load([]string{"--agents", "--models", "--format", "json"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ShowAgents {
		t.Fatalf("ShowAgents = false, want true")
	}
	if !c.ShowModels {
		t.Fatalf("ShowModels = false, want true")
	}
	if c.OutputFormat != "json" {
		t.Fatalf("OutputFormat = %q, want json", c.OutputFormat)
	}
}

func TestCheckModelProxyFlagParsed(t *testing.T) {
	c, err := Load([]string{"--check-model-proxy"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.CheckModelProxy {
		t.Fatalf("CheckModelProxy = false, want true")
	}
}

func TestLogLevelPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"log_level":"debug"}`,
		env:      map[string]string{"LOG_LEVEL": "error"},
		flagArgs: []string{"--log-level", "warn"},
		got:      func(c Config) string { return c.LogLevel },
		wantFlag: "warn",
		wantEnv:  "error",
		wantFile: "debug",
	})
}

func TestInvalidLogLevelIsUsageError(t *testing.T) {
	if _, err := Load([]string{"--log-level", "verbose"}, noEnv, ""); err == nil {
		t.Fatal("expected invalid log level to fail")
	}
}

func TestOneShotAndSessionFlags(t *testing.T) {
	c, err := Load([]string{
		"-model", "gpt-5.5",
		"-p", "do the thing",
		"-resume", "/tmp/in.json",
		"-session", "/tmp/out.json",
		"-system-prompt", "be terse",
	}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Prompt != "do the thing" || !c.PromptSet {
		t.Fatalf("prompt %q set=%v", c.Prompt, c.PromptSet)
	}
	if c.Resume != "/tmp/in.json" {
		t.Fatalf("resume %q", c.Resume)
	}
	if c.Session != "/tmp/out.json" {
		t.Fatalf("session %q", c.Session)
	}
	if c.SystemPrompt != "be terse" {
		t.Fatalf("system-prompt %q", c.SystemPrompt)
	}
}

func TestInitialPromptFlags(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5", "-i", "start here"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load -i: %v", err)
	}
	if c.InitialPrompt != "start here" || !c.InitialPromptSet {
		t.Fatalf("initial prompt %q set=%v", c.InitialPrompt, c.InitialPromptSet)
	}
	if c.PromptSet {
		t.Fatal("-i should not set one-shot prompt")
	}

	c, err = Load([]string{"-model", "gpt-5.5", "-initial-prompt", "start here"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load -initial-prompt: %v", err)
	}
	if c.InitialPrompt != "start here" || !c.InitialPromptSet {
		t.Fatalf("initial prompt alias %q set=%v", c.InitialPrompt, c.InitialPromptSet)
	}
}

func TestInitialPromptRejectsConflictingModes(t *testing.T) {
	if _, err := Load([]string{"-p", "one shot", "-i", "interactive"}, noEnv, ""); err == nil {
		t.Fatal("expected -p with -i to fail")
	}
}

func TestInitialPromptRejectsDashStdin(t *testing.T) {
	if _, err := Load([]string{"-i", "-"}, noEnv, ""); err == nil {
		t.Fatal("expected -i - to fail")
	}
}

func TestBadFlagIsUsageError(t *testing.T) {
	_, err := Load([]string{"-nonexistent-flag"}, noEnv, "")
	if err == nil {
		t.Fatalf("expected usage error for unknown flag")
	}
}

func TestBadMaxTurnsValueIsUsageError(t *testing.T) {
	_, err := Load([]string{"-max-turns", "notanumber"}, noEnv, "")
	if err == nil {
		t.Fatalf("expected usage error for non-integer -max-turns")
	}
}

func TestBadFormatValueIsUsageError(t *testing.T) {
	_, err := Load([]string{"--format", "yaml"}, noEnv, "")
	if err == nil {
		t.Fatalf("expected usage error for invalid -format")
	}
}

// -h and --help are help requests, not usage errors: Load reports ErrHelp so the
// caller can print a proper usage screen and exit 0 (design §10).
func TestHelpFlagReturnsErrHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "-help"} {
		_, err := Load([]string{arg}, noEnv, "")
		if !errors.Is(err, ErrHelp) {
			t.Fatalf("Load(%q) err = %v, want ErrHelp", arg, err)
		}
	}
}

func TestProviderQualifiedModelSetsProviderAndStripsModel(t *testing.T) {
	c, err := Load([]string{"-model", "openrouter:openai/gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load provider-qualified model: %v", err)
	}
	if c.Provider != "openrouter" || c.Model != "openai/gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want openrouter/openai/gpt-5.5", c.Provider, c.Model)
	}
}

func TestModelColonWithoutProviderQualifierStaysModel(t *testing.T) {
	c, err := Load([]string{"-model", "qwen/qwen3-coder:free"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load colon model: %v", err)
	}
	if c.Provider != "" || c.Model != "qwen/qwen3-coder:free" {
		t.Fatalf("provider/model = %q/%q, want no provider and unchanged model", c.Provider, c.Model)
	}
}

func TestSaveSelectedModelCreatesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := SaveSelectedModel(path, "openai:gpt-5.5", "HIGH"); err != nil {
		t.Fatalf("SaveSelectedModel: %v", err)
	}
	c, err := Load(nil, noEnv, path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if c.Provider != "openai" || c.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want openai/gpt-5.5", c.Provider, c.Model)
	}
	if c.Reasoning != "high" {
		t.Fatalf("reasoning = %q, want high", c.Reasoning)
	}
}

func TestSaveSelectedModelPreservesOtherConfigKeys(t *testing.T) {
	path := writeConfig(t, `{"agent":"plan","max_turns":7,"provider":"old","model":"old-model","reasoning_effort":"max","reasoning_enabled":true,"reasoning_budget_tokens":2048}`)
	if err := SaveSelectedModel(path, "anthropic:claude-opus-4-8", ""); err != nil {
		t.Fatalf("SaveSelectedModel: %v", err)
	}
	c, err := Load(nil, noEnv, path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if c.Provider != "anthropic" || c.Model != "claude-opus-4-8" {
		t.Fatalf("provider/model = %q/%q, want anthropic/claude-opus-4-8", c.Provider, c.Model)
	}
	if c.Agent != "plan" || c.MaxTurns != 7 {
		t.Fatalf("preserved agent/max_turns = %q/%d, want plan/7", c.Agent, c.MaxTurns)
	}
	if c.Reasoning != "" {
		t.Fatalf("reasoning = %q, want empty default", c.Reasoning)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), `"provider"`) || strings.Contains(string(data), "reasoning_effort") || strings.Contains(string(data), "reasoning_enabled") || strings.Contains(string(data), "reasoning_budget_tokens") || strings.Contains(string(data), `"reasoning"`) {
		t.Fatalf("saved config should remove standalone provider and reasoning keys:\n%s", data)
	}
}

func TestSaveSelectedModelDoesNotHTMLEscapeConfigStrings(t *testing.T) {
	path := writeConfig(t, `{"repl_prompt":"({git_branch}) {cwd} [{model} | {agent}]> ","htmlish":"<&>"}`)
	if err := SaveSelectedModel(path, "openai:gpt-5.5", ""); err != nil {
		t.Fatalf("SaveSelectedModel: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	out := string(data)
	if strings.Contains(out, `\u003e`) || strings.Contains(out, `\u003c`) || strings.Contains(out, `\u0026`) {
		t.Fatalf("saved config should not HTML-escape string values:\n%s", out)
	}
	if !strings.Contains(out, `"repl_prompt": "({git_branch}) {cwd} [{model} | {agent}]> "`) {
		t.Fatalf("saved config should preserve literal repl_prompt >:\n%s", out)
	}
	if !strings.Contains(out, `"htmlish": "<&>"`) {
		t.Fatalf("saved config should preserve literal <&>:\n%s", out)
	}
}

func TestSaveSelectedModelStillEscapesRequiredJSONStringCharacters(t *testing.T) {
	path := writeConfig(t, `{"custom":"quote \" backslash \\ newline\n"}`)
	if err := SaveSelectedModel(path, "openai:gpt-5.5", ""); err != nil {
		t.Fatalf("SaveSelectedModel: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	out := string(data)
	if !json.Valid(data) {
		t.Fatalf("saved config is not valid JSON:\n%s", out)
	}
	if !strings.Contains(out, `"custom": "quote \" backslash \\ newline\n"`) {
		t.Fatalf("saved config should still escape JSON-required string characters:\n%s", out)
	}
}

func TestSaveReplEditModeCreatesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := SaveReplEditMode(path, "vi"); err != nil {
		t.Fatalf("SaveReplEditMode: %v", err)
	}
	c, err := Load(nil, noEnv, path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if c.ReplEditMode != "vi" {
		t.Fatalf("repl_edit_mode = %q, want vi", c.ReplEditMode)
	}
}

func TestSaveReplEditModeCanonicalizes(t *testing.T) {
	for in, want := range map[string]string{"VIM": "vi", "emacs": "emacs", "": "emacs"} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := SaveReplEditMode(path, in); err != nil {
			t.Fatalf("SaveReplEditMode(%q): %v", in, err)
		}
		c, err := Load(nil, noEnv, path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.ReplEditMode != want {
			t.Fatalf("SaveReplEditMode(%q): repl_edit_mode = %q, want %q", in, c.ReplEditMode, want)
		}
	}
}

func TestSaveReplEditModeRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveReplEditMode(path, "bogus"); err == nil {
		t.Fatalf("SaveReplEditMode(%q): want error, got nil", "bogus")
	}
}

func TestSaveReplEditModePreservesOtherConfigKeys(t *testing.T) {
	path := writeConfig(t, `{"agent":"plan","model":"openai:gpt-5.5","repl_edit_mode":"emacs"}`)
	if err := SaveReplEditMode(path, "vi"); err != nil {
		t.Fatalf("SaveReplEditMode: %v", err)
	}
	c, err := Load(nil, noEnv, path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if c.ReplEditMode != "vi" {
		t.Fatalf("repl_edit_mode = %q, want vi", c.ReplEditMode)
	}
	if c.Agent != "plan" || c.Provider != "openai" || c.Model != "gpt-5.5" {
		t.Fatalf("preserved keys = agent=%q provider=%q model=%q, want plan/openai/gpt-5.5", c.Agent, c.Provider, c.Model)
	}
}

// Usage writes a screen that names every registered flag with its default, so the
// help output and canonical usage guide remain complete references.
func TestUsageListsEveryFlag(t *testing.T) {
	var b bytes.Buffer
	Usage(&b)
	out := b.String()

	usageDoc, err := os.ReadFile(filepath.Join("..", "..", "docs", "usage.md"))
	if err != nil {
		t.Fatalf("read usage documentation: %v", err)
	}
	flagSection := markdownSection(t, string(usageDoc), "## Flags", "## Configuration And Environment")

	fs, _ := newFlagSet()
	fs.VisitAll(func(f *flag.Flag) {
		flagToken := "-" + f.Name
		if !documentedFlag(out, f.Name) {
			t.Errorf("usage text missing registered flag %q:\n%s", flagToken, out)
		}
		if !documentedFlag(flagSection, f.Name) {
			t.Errorf("docs/usage.md flag section missing registered flag %q", flagToken)
		}
	})
	for _, name := range []string{"h", "help", "version"} {
		if !documentedFlag(flagSection, name) {
			t.Errorf("docs/usage.md flag section missing manual flag alias %q", "-"+name)
		}
	}
	// -max-turns default (250) must be visible so the reference is accurate.
	if !strings.Contains(out, "250") {
		t.Errorf("usage text should show the -max-turns default 250:\n%s", out)
	}
	if !strings.Contains(out, "256000") {
		t.Errorf("usage text should show the -default-context-window default 256000:\n%s", out)
	}
}

func TestExampleConfigUsesOnlySupportedKeys(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "harness", "config.json"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	checkJSONKeysMatchType(t, data, reflect.TypeOf(fileConfig{}), "config")
}

func markdownSection(t *testing.T, text, start, end string) string {
	t.Helper()
	startAt := strings.Index(text, start)
	if startAt < 0 {
		t.Fatalf("documentation missing section %q", start)
	}
	text = text[startAt+len(start):]
	endAt := strings.Index(text, end)
	if endAt < 0 {
		t.Fatalf("documentation section %q missing end marker %q", start, end)
	}
	return text[:endAt]
}

func documentedFlag(section, name string) bool {
	pattern := `(?m)(?:^|[\s,])--?` + regexp.QuoteMeta(name) + `(?:$|[\s,=<])`
	return regexp.MustCompile(pattern).MatchString(section)
}

func checkJSONKeysMatchType(t *testing.T, raw []byte, typ reflect.Type, path string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		fields := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = field.Type
			}
		}
		for name, value := range object {
			fieldType, ok := fields[name]
			if !ok {
				t.Errorf("%s contains unsupported key %q", path, name)
				continue
			}
			checkJSONKeysMatchType(t, value, fieldType, path+"."+name)
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for name, value := range object {
			checkJSONKeysMatchType(t, value, typ.Elem(), path+"."+name)
		}
	}
}

func TestWriteResolvedIncludesDefaults(t *testing.T) {
	c := loadOK(t, []string{
		"--show-config",
		"-model", "openrouter:openai/gpt-5.5",
		"-p", "hi",
	}, noEnv, "")
	var b bytes.Buffer
	if err := WriteResolved(&b, c); err != nil {
		t.Fatalf("WriteResolved: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("resolved config is not JSON: %v\n%s", err, b.String())
	}
	if got["provider"] != "openrouter" || got["model"] != "openai/gpt-5.5" {
		t.Fatalf("provider/model = %v/%v, want openrouter/openai/gpt-5.5\n%s", got["provider"], got["model"], b.String())
	}
	if got["max_turns"] != float64(250) {
		t.Fatalf("max_turns = %v, want default 250\n%s", got["max_turns"], b.String())
	}
	if got["default_context_window"] != float64(256000) {
		t.Fatalf("default_context_window = %v, want default 256000\n%s", got["default_context_window"], b.String())
	}
	if got["tool_stream"] != false {
		t.Fatalf("tool_stream = %v, want default false\n%s", got["tool_stream"], b.String())
	}
	if got["repl_prompt"] != replprompt.DefaultFormat {
		t.Fatalf("repl_prompt = %v, want default REPL prompt\n%s", got["repl_prompt"], b.String())
	}
	if got["repl_edit_mode"] != DefaultReplEditMode {
		t.Fatalf("repl_edit_mode = %v, want %s\n%s", got["repl_edit_mode"], DefaultReplEditMode, b.String())
	}
	if got["one_shot_prompt"] != "hi" || got["one_shot_prompt_set"] != true {
		t.Fatalf("one-shot prompt fields = %v/%v, want hi/true\n%s", got["one_shot_prompt"], got["one_shot_prompt_set"], b.String())
	}
	if got["show_config"] != true {
		t.Fatalf("show_config = %v, want true\n%s", got["show_config"], b.String())
	}
	if got["check_model_proxy"] != false {
		t.Fatalf("check_model_proxy = %v, want false\n%s", got["check_model_proxy"], b.String())
	}
	if _, ok := got["mcp_proxy_api_key"]; ok {
		t.Fatalf("resolved config should not expose unused top-level mcp_proxy_api_key:\n%s", b.String())
	}
}

// A malformed config file is a usage/config error, not a silent ignore.
func TestMalformedConfigFileIsError(t *testing.T) {
	cfgPath := writeConfig(t, `{not valid json`)
	_, err := Load(nil, noEnv, cfgPath)
	if err == nil {
		t.Fatalf("expected error for malformed config file")
	}
}

func TestConfigRelativeAtFileRefsAreNormalized(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("create prompt dir: %v", err)
	}
	cfgPath := filepath.Join(configDir, "config.json")
	body := `{
  "system_prompt": "@../prompts/system.txt",
  "agents": {
    "review": {"prompt": "@../prompts/review.txt"},
    "literal": {"prompt": "@@not-a-file"},
    "home": {"prompt": "@~/prompt.txt"}
  }
}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load(nil, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if want := "@" + filepath.Join(promptDir, "system.txt"); c.SystemPrompt != want {
		t.Fatalf("system_prompt = %q, want %q", c.SystemPrompt, want)
	}
	if want := "@" + filepath.Join(promptDir, "review.txt"); c.Agents["review"].Prompt != want {
		t.Fatalf("review prompt = %q, want %q", c.Agents["review"].Prompt, want)
	}
	if got := c.Agents["literal"].Prompt; got != "@@not-a-file" {
		t.Fatalf("literal prompt = %q, want escaped @ preserved", got)
	}
	if got := c.Agents["home"].Prompt; got != "@~/prompt.txt" {
		t.Fatalf("home prompt = %q, want home reference preserved", got)
	}
}

// A missing config file at the explicit path is an error (the user asked for it);
// a missing file at the implicit default path is silently tolerated.
func TestMissingExplicitConfigFileIsError(t *testing.T) {
	_, err := Load(nil, noEnv, filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatalf("expected error for missing explicit config file")
	}
}

func TestAgentPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"agent":"plan"}`,
		env:      map[string]string{"HARNESS_AGENT": "independent"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-agent", "AUTO"},
		got:      func(c Config) string { return c.Agent },
		wantFlag: "auto",
		wantEnv:  "independent",
		wantFile: "plan",
	})
}

// An unspecified agent stays empty so main can distinguish "not specified"
// (session resume may supply the agent) from an explicit choice.
func TestAgentUnspecifiedIsEmpty(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Agent != "" {
		t.Fatalf("agent %q, want empty when unspecified", c.Agent)
	}
}

func TestAgentsObjectDecodes(t *testing.T) {
	cfgPath := writeConfig(t, `{
		"agents": {
			"review": {"description":"Review changes", "allowed_tools": ["read_file", "grep"], "mcp_tools":"read_only", "prompt": "review the diff", "model":"openai:gpt-5.5"},
			"plan": {"prompt": "custom plan prompt"}
		}
	}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	review, ok := c.Agents["review"]
	if !ok {
		t.Fatal("agents.review not decoded")
	}
	if review.Description != "Review changes" {
		t.Errorf("review.Description = %q", review.Description)
	}
	if len(review.AllowedTools) != 2 || review.AllowedTools[0] != "read_file" || review.AllowedTools[1] != "grep" {
		t.Errorf("review.AllowedTools = %v", review.AllowedTools)
	}
	if review.Prompt != "review the diff" {
		t.Errorf("review.Prompt = %q", review.Prompt)
	}
	if review.Model != "openai:gpt-5.5" {
		t.Errorf("review model = %q", review.Model)
	}
	if review.MCPTools != "read_only" {
		t.Errorf("review.MCPTools = %q", review.MCPTools)
	}
	if c.Agents["plan"].Prompt != "custom plan prompt" {
		t.Errorf("plan.Prompt = %q", c.Agents["plan"].Prompt)
	}
	if len(c.Agents["plan"].AllowedTools) != 0 {
		t.Errorf("plan.AllowedTools should be empty (inherit), got %v", c.Agents["plan"].AllowedTools)
	}
}

func TestHooksInlineAndHookConfigsAppendInOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"printf file"}]}]}}`), 0o644); err != nil {
		t.Fatalf("write hook config: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	body := `{
		"hooks": {
			"PreToolUse": [
				{"hooks":[{"type":"command","command":"printf inline"}]}
			]
		},
		"hook_configs": ["hooks.json"]
	}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	groups := c.Hooks.Groups(hooks.PreToolUse)
	if len(groups) != 2 {
		t.Fatalf("PreToolUse groups = %d, want 2", len(groups))
	}
	if groups[0].Hooks[0].Command != "printf inline" || groups[1].Hooks[0].Command != "printf file" {
		t.Fatalf("hook order = %+v", groups)
	}
	if len(c.HookConfigs) != 1 || c.HookConfigs[0] != "hooks.json" {
		t.Fatalf("HookConfigs = %v, want [hooks.json]", c.HookConfigs)
	}
}

func TestHooksFlagOverridesInlineAndHookConfigs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file-hooks.json"), []byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"printf file"}]}]}`), 0o644); err != nil {
		t.Fatalf("write file hook: %v", err)
	}
	overrideHook := filepath.Join(dir, "override-hooks.json")
	if err := os.WriteFile(overrideHook, []byte(`{"PostToolUse":[{"hooks":[{"type":"command","command":"printf override"}]}]}`), 0o644); err != nil {
		t.Fatalf("write override hook: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	body := `{
		"hooks": {"PreToolUse":[{"hooks":[{"type":"command","command":"printf inline"}]}]},
		"hook_configs": ["file-hooks.json"]
	}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load([]string{"-model", "gpt-5.5", "--hooks", overrideHook}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(c.Hooks.Groups(hooks.PreToolUse)); got != 0 {
		t.Fatalf("PreToolUse groups = %d, want 0 after override", got)
	}
	groups := c.Hooks.Groups(hooks.PostToolUse)
	if len(groups) != 1 || groups[0].Hooks[0].Command != "printf override" {
		t.Fatalf("PostToolUse override groups = %+v", groups)
	}
}

func TestMCPDefaults(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.Enable {
		t.Errorf("MCP.Enable default = true, want false")
	}
	if c.MCP.Proxy != "" {
		t.Errorf("MCP.Proxy default = %q, want empty (resolved at use)", c.MCP.Proxy)
	}
	if c.MCP.MaxTools != 0 {
		t.Errorf("MCP.MaxTools default = %d, want 0 (unlimited)", c.MCP.MaxTools)
	}
	if c.MCP.DisabledServers != nil {
		t.Errorf("MCP.DisabledServers default = %v, want nil", c.MCP.DisabledServers)
	}
}

func TestMCPToolLimitsFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"enable":true,"max_tools":12,"disabled_servers":["playwright","browser"]}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.MaxTools != 12 {
		t.Errorf("MCP.MaxTools = %d, want 12", c.MCP.MaxTools)
	}
	if len(c.MCP.DisabledServers) != 2 || c.MCP.DisabledServers[0] != "playwright" || c.MCP.DisabledServers[1] != "browser" {
		t.Errorf("MCP.DisabledServers = %v, want [playwright browser]", c.MCP.DisabledServers)
	}
}

func TestMCPNegativeMaxToolsErrors(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"max_tools":-1}}`)
	if _, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath); err == nil {
		t.Fatal("negative mcp.max_tools should be a load error")
	}
}

func TestMCPFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"enable":true,"proxy":"http://127.0.0.1:8766"}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MCP.Enable {
		t.Errorf("MCP.Enable = false, want true")
	}
	if c.MCP.Proxy != "http://127.0.0.1:8766" {
		t.Errorf("MCP.Proxy = %q, want http://127.0.0.1:8766", c.MCP.Proxy)
	}
}

func TestMCPEnvOverridesFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"enable":false,"proxy":"http://file.example/mcp"}}`)
	env := envFrom(map[string]string{
		"HARNESS_MCP_ENABLE": "true",
		"HARNESS_MCP_PROXY":  "http://env.example/mcp",
	})
	c, err := Load([]string{"-model", "gpt-5.5"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MCP.Enable {
		t.Errorf("MCP.Enable = false, want true (env overrides file)")
	}
	if c.MCP.Proxy != "http://env.example/mcp" {
		t.Errorf("MCP.Proxy = %q, want http://env.example/mcp (env overrides file)", c.MCP.Proxy)
	}
}

func TestMCPLocalDefaults(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.Local.Enable || c.MCP.Local.EnableSet {
		t.Errorf("Local default = {Enable:%v EnableSet:%v}, want both false", c.MCP.Local.Enable, c.MCP.Local.EnableSet)
	}
}

func TestMCPLocalFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"local":{"enable":true,"command":"custom-mcp","args":["serve"],"env":{"K":"v"}}}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MCP.Local.Enable || !c.MCP.Local.EnableSet {
		t.Errorf("Local = {Enable:%v EnableSet:%v}, want both true", c.MCP.Local.Enable, c.MCP.Local.EnableSet)
	}
	if c.MCP.Local.Command != "custom-mcp" || len(c.MCP.Local.Args) != 1 || c.MCP.Local.Args[0] != "serve" {
		t.Errorf("Local command/args = %q %v", c.MCP.Local.Command, c.MCP.Local.Args)
	}
	if c.MCP.Local.Env["K"] != "v" {
		t.Errorf("Local env = %v", c.MCP.Local.Env)
	}
}

func TestMCPLocalEnableSetTracksEnv(t *testing.T) {
	// An env value marks EnableSet even when it disables the feature.
	c, err := Load([]string{"-model", "gpt-5.5"}, envFrom(map[string]string{"HARNESS_MCP_LOCAL_ENABLE": "false"}), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.Local.Enable || !c.MCP.Local.EnableSet {
		t.Errorf("Local = {Enable:%v EnableSet:%v}, want {false true}", c.MCP.Local.Enable, c.MCP.Local.EnableSet)
	}
}

func TestLSPDefaults(t *testing.T) {
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LSP.Enable {
		t.Errorf("LSP.Enable default = true, want false")
	}
	if c.LSP.Servers != nil {
		t.Errorf("LSP.Servers default = %v, want nil", c.LSP.Servers)
	}
	if c.LSP.Tools != nil {
		t.Errorf("LSP.Tools default = %v, want nil", c.LSP.Tools)
	}
	if c.LSP.Serena.Enable || c.LSP.Serena.EnableSet {
		t.Errorf("LSP.Serena default = {Enable:%v EnableSet:%v}, want both false", c.LSP.Serena.Enable, c.LSP.Serena.EnableSet)
	}
	if c.LSP.Serena.Command != DefaultSerenaCommand {
		t.Errorf("LSP.Serena.Command = %q, want %q", c.LSP.Serena.Command, DefaultSerenaCommand)
	}
	if !slices.Equal(c.LSP.Serena.Args, DefaultSerenaArgs()) {
		t.Errorf("LSP.Serena.Args = %v, want %v", c.LSP.Serena.Args, DefaultSerenaArgs())
	}
}

func TestLSPToolsAllowlistFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"lsp":{"enable":true,"tools":["definition","references"]}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.LSP.Tools) != 2 || c.LSP.Tools[0] != "definition" || c.LSP.Tools[1] != "references" {
		t.Fatalf("LSP.Tools = %v, want [definition references]", c.LSP.Tools)
	}
}

func TestLSPFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{
		"lsp": {
			"enable": true,
			"servers": {
				"ruby-lsp": {
					"languages": ["ruby"],
					"extensions": [".rb"],
					"root_markers": ["Gemfile", ".git"],
					"command": ["ruby-lsp"],
					"env": {"K":"v"},
					"initialization_options": {"enabledFeatures": ["codeActions"]}
				}
			}
		}
	}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.LSP.Enable {
		t.Errorf("LSP.Enable = false, want true")
	}
	ruby := c.LSP.Servers["ruby-lsp"]
	if len(ruby.Languages) != 1 || ruby.Languages[0] != "ruby" {
		t.Fatalf("ruby languages = %v", ruby.Languages)
	}
	if len(ruby.Extensions) != 1 || ruby.Extensions[0] != ".rb" {
		t.Fatalf("ruby extensions = %v", ruby.Extensions)
	}
	if len(ruby.RootMarkers) != 2 || ruby.RootMarkers[0] != "Gemfile" {
		t.Fatalf("ruby root markers = %v", ruby.RootMarkers)
	}
	if len(ruby.Command) != 1 || ruby.Command[0] != "ruby-lsp" {
		t.Fatalf("ruby command = %v", ruby.Command)
	}
	if ruby.Env["K"] != "v" {
		t.Fatalf("ruby env = %v", ruby.Env)
	}
	if got := string(ruby.InitOptions); !strings.Contains(got, "enabledFeatures") {
		t.Fatalf("ruby initialization_options = %s", got)
	}
}

func TestLSPSerenaFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{
		"lsp": {
			"enable": false,
			"serena": {
				"enable": true,
				"command": "uvx",
				"args": ["--from", "git+https://example.invalid/serena", "serena", "start-mcp-server", "--context=ide", "--project-from-cwd"],
				"env": {"SERENA_TOKEN": "${TOKEN}"}
			}
		}
	}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, envFrom(map[string]string{"TOKEN": "secret"}), cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LSP.Enable {
		t.Errorf("LSP.Enable = true, want false")
	}
	if !c.LSP.Serena.Enable || !c.LSP.Serena.EnableSet {
		t.Errorf("LSP.Serena = {Enable:%v EnableSet:%v}, want both true", c.LSP.Serena.Enable, c.LSP.Serena.EnableSet)
	}
	if c.LSP.Serena.Command != "uvx" {
		t.Fatalf("Serena.Command = %q, want uvx", c.LSP.Serena.Command)
	}
	wantArgs := []string{"--from", "git+https://example.invalid/serena", "serena", "start-mcp-server", "--context=ide", "--project-from-cwd"}
	if !slices.Equal(c.LSP.Serena.Args, wantArgs) {
		t.Fatalf("Serena.Args = %v, want %v", c.LSP.Serena.Args, wantArgs)
	}
	if c.LSP.Serena.Env["SERENA_TOKEN"] != "secret" {
		t.Fatalf("Serena.Env = %v", c.LSP.Serena.Env)
	}
}

func TestLSPEnableEnvOverridesFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"lsp":{"enable":false}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, envFrom(map[string]string{"HARNESS_LSP_ENABLE": "true"}), cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.LSP.Enable {
		t.Errorf("LSP.Enable = false, want true (env overrides file)")
	}
}

func TestLSPSerenaEnableEnvOverridesFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"lsp":{"serena":{"enable":false}}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, envFrom(map[string]string{"HARNESS_LSP_SERENA_ENABLE": "true"}), cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.LSP.Serena.Enable || !c.LSP.Serena.EnableSet {
		t.Errorf("LSP.Serena = {Enable:%v EnableSet:%v}, want {true true}", c.LSP.Serena.Enable, c.LSP.Serena.EnableSet)
	}
	if c.LSP.Enable {
		t.Errorf("LSP.Enable = true, want false; Serena must not imply native LSP")
	}
}

func TestMCPEnableBoolParsing(t *testing.T) {
	// A bogus env value falls through to the file value (resolveBool ignores
	// unparseable env), and an empty/unset env leaves the file/default in place.
	cfgPath := writeConfig(t, `{"mcp":{"enable":true}}`)
	env := envFrom(map[string]string{"HARNESS_MCP_ENABLE": "not-a-bool"})
	c, err := Load([]string{"-model", "gpt-5.5"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MCP.Enable {
		t.Errorf("MCP.Enable = false, want true (unparseable env falls back to file)")
	}

	// "0" parses as false and overrides the file's true.
	env = envFrom(map[string]string{"HARNESS_MCP_ENABLE": "0"})
	c, err = Load([]string{"-model", "gpt-5.5"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.Enable {
		t.Errorf("MCP.Enable = true, want false (HARNESS_MCP_ENABLE=0)")
	}
}

// TestMCPHeadersFromFile decodes the "headers" map under the "mcp" object.
func TestMCPHeadersFromFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"enable":true,"proxy":"https://proxy.example/mcp","headers":{"Authorization":"Bearer tok","X-Env":"prod"}}}`)
	c, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.MCP.Headers["Authorization"]; got != "Bearer tok" {
		t.Errorf("Headers[Authorization] = %q, want %q", got, "Bearer tok")
	}
	if got := c.MCP.Headers["X-Env"]; got != "prod" {
		t.Errorf("Headers[X-Env] = %q, want %q", got, "prod")
	}
	if c.MCP.Proxy != "https://proxy.example/mcp" {
		t.Errorf("Proxy = %q, want the http URL", c.MCP.Proxy)
	}
}

func TestMCPHeadersExpandEnvRefs(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"headers":{"Authorization":"Bearer ${TOKEN}","X-Default":"${MISSING:-fallback}","X-Literal":"price$5 $$ ${1BAD}"}}}`)
	env := envFrom(map[string]string{"TOKEN": "secret"})
	c, err := Load([]string{"-model", "gpt-5.5"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.MCP.Headers["Authorization"]; got != "Bearer secret" {
		t.Fatalf("Authorization = %q, want Bearer secret", got)
	}
	if got := c.MCP.Headers["X-Default"]; got != "fallback" {
		t.Fatalf("X-Default = %q, want fallback", got)
	}
	if got := c.MCP.Headers["X-Literal"]; got != "price$5 $$ ${1BAD}" {
		t.Fatalf("X-Literal = %q, want literal dollar forms preserved", got)
	}
}

func TestMCPHeadersUnsetEnvRefErrors(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"headers":{"Authorization":"Bearer ${TOKEN}"}}}`)
	if _, err := Load([]string{"-model", "gpt-5.5"}, noEnv, cfgPath); err == nil {
		t.Fatal("unset mcp header variable should error")
	} else if !strings.Contains(err.Error(), "mcp.headers.Authorization") || !strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("error should name header and variable, got %v", err)
	}
}

// TestMCPHeadersAbsentIsNil confirms an mcp block without "headers" leaves
// Headers nil (not an empty map), and that there is NO env var for headers: an
// env that looks header-ish cannot leak into the resolved map.
func TestMCPHeadersAbsentIsNil(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"enable":true,"proxy":"https://proxy.example/mcp"}}`)
	// Throw a plausible-but-irrelevant env at Load; headers are config-file-only.
	env := envFrom(map[string]string{
		"HARNESS_MCP_HEADERS":       `{"Authorization":"leak"}`,
		"HARNESS_MCP_AUTHORIZATION": "leak",
	})
	c, err := Load([]string{"-model", "gpt-5.5"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP.Headers != nil {
		t.Errorf("Headers = %v, want nil (absent in file, no env layer)", c.MCP.Headers)
	}
}

// TestMCPHeadersNoEnvLeakageWithFileHeaders confirms env cannot override or
// augment file headers: the file is the only source.
func TestMCPHeadersNoEnvLeakageWithFileHeaders(t *testing.T) {
	cfgPath := writeConfig(t, `{"mcp":{"headers":{"Authorization":"Bearer file"}}}`)
	env := envFrom(map[string]string{
		"HARNESS_MCP_HEADERS":       `{"Authorization":"Bearer env","X-Extra":"env"}`,
		"HARNESS_MCP_AUTHORIZATION": "Bearer env",
	})
	c, err := Load([]string{"-model", "gpt-5.5"}, env, cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.MCP.Headers["Authorization"]; got != "Bearer file" {
		t.Errorf("Headers[Authorization] = %q, want %q (env must not leak)", got, "Bearer file")
	}
	if _, ok := c.MCP.Headers["X-Extra"]; ok {
		t.Errorf("Headers gained X-Extra from env; headers are config-file-only")
	}
	if n := len(c.MCP.Headers); n != 1 {
		t.Errorf("Headers has %d entries, want 1 (file only)", n)
	}
}

func TestRetentionFloorTokensDefaultAndValidation(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.RetentionFloorTokens != 0 {
		t.Fatalf("default retention_floor_tokens = %d, want 0 (disabled)", c.RetentionFloorTokens)
	}

	c = loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, writeConfig(t, `{"retention_floor_tokens":200000}`))
	if c.RetentionFloorTokens != 200000 {
		t.Fatalf("retention_floor_tokens = %d, want 200000", c.RetentionFloorTokens)
	}

	if _, err := Load([]string{"-model", "gpt-5.5"}, noEnv, writeConfig(t, `{"retention_floor_tokens":-1}`)); err == nil ||
		!strings.Contains(err.Error(), "retention_floor_tokens") {
		t.Fatalf("negative retention_floor_tokens error = %v", err)
	}
}
