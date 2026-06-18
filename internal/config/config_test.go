package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func boolPtrLabel(v *bool) string {
	if v == nil {
		return "nil"
	}
	if *v {
		return "true"
	}
	return "false"
}

func intPtrLabel(v *int) string {
	if v == nil {
		return "nil"
	}
	return strconv.Itoa(*v)
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

func TestProviderPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"provider":"openai"}`,
		env:      map[string]string{"HARNESS_PROVIDER": "anthropic"},
		flagArgs: []string{"-provider", "openai"},
		got:      func(c Config) string { return c.Provider },
		wantFlag: "openai",
		wantEnv:  "anthropic",
		wantFile: "openai",
	})
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

func TestExplicitProviderIsPreserved(t *testing.T) {
	c, err := Load([]string{"-model", "claude-opus-4-8", "-provider", "openai"}, noEnv, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Provider != "openai" {
		t.Fatalf("provider %q, want openai (explicit overrides inference)", c.Provider)
	}
}

// HARNESS_* env mapping covers the user-facing flags.
func TestHarnessEnvMapping(t *testing.T) {
	env := envFrom(map[string]string{
		"HARNESS_MODEL":                   "env-model",
		"HARNESS_MODEL_PROXY_URL":         "http://proxy.example",
		"HARNESS_MAX_TURNS":               "12",
		"HARNESS_DEFAULT_CONTEXT_WINDOW":  "512000",
		"HARNESS_CONTEXT_WINDOW":          "256000",
		"HARNESS_REASONING_EFFORT":        "HIGH",
		"HARNESS_REASONING_ENABLED":       "true",
		"HARNESS_REASONING_BUDGET_TOKENS": "2048",
		"HARNESS_REASONING_SUMMARY":       "AUTO",
		"HARNESS_RESPONSES_STATEFUL":      "true",
		"HARNESS_SEARCH_TOOLS":            "both",
		"HARNESS_TOOL_RESULT_MAX_BYTES":   "32768",
		"HARNESS_TOOL_RESULT_MAX_LINES":   "500",
		"HARNESS_SYSTEM_PROMPT":           "env system prompt",
		"HARNESS_NO_ENV":                  "true",
		"HARNESS_NO_COLOR":                "true",
		"HARNESS_TIMESTAMPS":              "full",
		"HARNESS_VERBOSE":                 "true",
		"HARNESS_TOOL_STREAM":             "false",
		"HARNESS_SHOW_DIFFS":              "true",
		"HARNESS_REPL_PROMPT":             "env> ",
		"HARNESS_REPL_EDIT_MODE":          "vi",
		"LOG_LEVEL":                       "WARN",
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
	if c.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort %q, want high", c.ReasoningEffort)
	}
	if c.ReasoningEnabled == nil || !*c.ReasoningEnabled {
		t.Fatalf("reasoning enabled = %v, want true", c.ReasoningEnabled)
	}
	if c.ReasoningBudgetTokens == nil || *c.ReasoningBudgetTokens != 2048 {
		t.Fatalf("reasoning budget tokens = %v, want 2048", c.ReasoningBudgetTokens)
	}
	if c.ReasoningSummary != "auto" {
		t.Fatalf("reasoning summary = %q, want auto", c.ReasoningSummary)
	}
	if !c.ResponsesStateful {
		t.Fatalf("responses_stateful false, want true")
	}
	if c.SearchTools != "both" {
		t.Fatalf("search_tools = %q, want both", c.SearchTools)
	}
	if c.ToolResultMaxBytes != 32768 {
		t.Fatalf("tool result max bytes = %d, want 32768", c.ToolResultMaxBytes)
	}
	if c.ToolResultMaxLines != 500 {
		t.Fatalf("tool result max lines = %d, want 500", c.ToolResultMaxLines)
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
	if _, err := Load([]string{"-repl-prompt", `line\n{agent}> `}, noEnv, ""); err != nil {
		t.Fatalf("escaped newline prompt should load: %v", err)
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
	if !c.ToolStream {
		t.Fatalf("default tool-stream false, want true")
	}

	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"tool_stream":false}`,
		env:      map[string]string{"HARNESS_TOOL_STREAM": "true"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-tool-stream=false"},
		got:      func(c Config) bool { return c.ToolStream },
		wantFlag: false,
		wantEnv:  true,
		wantFile: false,
	})
}

func TestShowDiffsPrecedence(t *testing.T) {
	c := loadOK(t, []string{"-model", "gpt-5.5"}, noEnv, "")
	if c.ShowDiffs {
		t.Fatalf("default show-diffs true, want false")
	}

	checkPrecedence(t, precedenceCase[bool]{
		file:     `{"show_diffs":true}`,
		env:      map[string]string{"HARNESS_SHOW_DIFFS": "false"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-show-diffs"},
		got:      func(c Config) bool { return c.ShowDiffs },
		wantFlag: true,
		wantEnv:  false,
		wantFile: true,
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

func TestSearchToolsPrecedenceAndValidation(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"search_tools":"grep"}`,
		env:      map[string]string{"HARNESS_SEARCH_TOOLS": "ripgrep"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-search-tools", "both"},
		got:      func(c Config) string { return c.SearchTools },
		wantFlag: "both",
		wantEnv:  "rg",
		wantFile: "grep",
	})

	if _, err := Load([]string{"-search-tools", "ack"}, noEnv, ""); err == nil {
		t.Fatalf("expected invalid search_tools to fail")
	}
}

func TestToolResultLimitPrecedenceEnvBeatsFile(t *testing.T) {
	cfgPath := writeConfig(t, `{"tool_result_max_bytes":111,"tool_result_max_lines":222}`)

	fileCfg := loadOK(t, nil, noEnv, cfgPath)
	if fileCfg.ToolResultMaxBytes != 111 || fileCfg.ToolResultMaxLines != 222 {
		t.Fatalf("file tool result limits = %d/%d, want 111/222", fileCfg.ToolResultMaxBytes, fileCfg.ToolResultMaxLines)
	}

	envCfg := loadOK(t, nil, envFrom(map[string]string{
		"HARNESS_TOOL_RESULT_MAX_BYTES": "333",
		"HARNESS_TOOL_RESULT_MAX_LINES": "444",
	}), cfgPath)
	if envCfg.ToolResultMaxBytes != 333 || envCfg.ToolResultMaxLines != 444 {
		t.Fatalf("env tool result limits = %d/%d, want 333/444", envCfg.ToolResultMaxBytes, envCfg.ToolResultMaxLines)
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

func TestReasoningEffortPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"reasoning_effort":"low"}`,
		env:      map[string]string{"HARNESS_REASONING_EFFORT": "medium"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-reasoning-effort", "HIGH"},
		got:      func(c Config) string { return c.ReasoningEffort },
		wantFlag: "high",
		wantEnv:  "medium",
		wantFile: "low",
	})
}

func TestReasoningEnabledPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"reasoning_enabled":false}`,
		env:      map[string]string{"HARNESS_REASONING_ENABLED": "true"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-reasoning-enabled=false"},
		got:      func(c Config) string { return boolPtrLabel(c.ReasoningEnabled) },
		wantFlag: "false",
		wantEnv:  "true",
		wantFile: "false",
	})
}

func TestReasoningBudgetTokensPrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	checkPrecedence(t, precedenceCase[string]{
		file:     `{"reasoning_budget_tokens":1024}`,
		env:      map[string]string{"HARNESS_REASONING_BUDGET_TOKENS": "2048"},
		baseArgs: []string{"-model", "gpt-5.5"},
		flagArgs: []string{"-reasoning-budget-tokens", "4096"},
		got:      func(c Config) string { return intPtrLabel(c.ReasoningBudgetTokens) },
		wantFlag: "4096",
		wantEnv:  "2048",
		wantFile: "1024",
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

// helpFlags are every flag the design §10 table lists. The -h usage screen must
// name every one of them so the help is an accurate reference.
var helpFlags = []string{
	"-p", "-provider", "-model", "-model-proxy-url", "-system-prompt",
	"-no-env", "-resume", "-session", "-max-turns", "-default-context-window", "-context-window",
	"-reasoning-effort", "-reasoning-enabled", "-reasoning-budget-tokens", "-reasoning-summary", "-responses-stateful", "-image-detail", "-image", "-agent", "-search-tools", "-v", "-tool-stream", "-q", "-quiet", "-log-level", "-no-color", "-config", "-repl-prompt", "-show-config",
	"-check-model-proxy", "-repl-edit-mode", "-hooks",
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
	if err := SaveSelectedModel(path, "openai", "gpt-5.5", "HIGH", nil, nil); err != nil {
		t.Fatalf("SaveSelectedModel: %v", err)
	}
	c, err := Load(nil, noEnv, path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if c.Provider != "openai" || c.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want openai/gpt-5.5", c.Provider, c.Model)
	}
	if c.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q, want high", c.ReasoningEffort)
	}
	if c.ReasoningEnabled != nil {
		t.Fatalf("reasoning enabled = %v, want nil", c.ReasoningEnabled)
	}
	if c.ReasoningBudgetTokens != nil {
		t.Fatalf("reasoning budget tokens = %v, want nil", c.ReasoningBudgetTokens)
	}
}

func TestSaveSelectedModelWritesReasoningControls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	enabled := true
	budget := 2048
	if err := SaveSelectedModel(path, "anthropic", "claude-opus-4-8", "", &enabled, &budget); err != nil {
		t.Fatalf("SaveSelectedModel: %v", err)
	}
	c, err := Load(nil, noEnv, path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if c.ReasoningEnabled == nil || !*c.ReasoningEnabled {
		t.Fatalf("reasoning enabled = %v, want true", c.ReasoningEnabled)
	}
	if c.ReasoningBudgetTokens == nil || *c.ReasoningBudgetTokens != 2048 {
		t.Fatalf("reasoning budget tokens = %v, want 2048", c.ReasoningBudgetTokens)
	}
}

func TestSaveSelectedModelPreservesOtherConfigKeys(t *testing.T) {
	path := writeConfig(t, `{"agent":"plan","max_turns":7,"provider":"old","model":"old-model","reasoning_effort":"max","reasoning_enabled":true,"reasoning_budget_tokens":2048}`)
	if err := SaveSelectedModel(path, "anthropic", "claude-opus-4-8", "", nil, nil); err != nil {
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
	if c.ReasoningEffort != "" {
		t.Fatalf("reasoning effort = %q, want empty default", c.ReasoningEffort)
	}
	if c.ReasoningEnabled != nil {
		t.Fatalf("reasoning enabled = %v, want nil", c.ReasoningEnabled)
	}
	if c.ReasoningBudgetTokens != nil {
		t.Fatalf("reasoning budget tokens = %v, want nil", c.ReasoningBudgetTokens)
	}
}

// Usage writes a screen that names every design §10 flag with its default, so the
// help output is a complete and accurate reference.
func TestUsageListsEveryFlag(t *testing.T) {
	var b bytes.Buffer
	Usage(&b)
	out := b.String()
	for _, f := range helpFlags {
		if !strings.Contains(out, f) {
			t.Errorf("usage text missing flag %q:\n%s", f, out)
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
	if got["tool_stream"] != true {
		t.Fatalf("tool_stream = %v, want default true\n%s", got["tool_stream"], b.String())
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
			"review": {"description":"Review changes", "allowed_tools": ["read_file", "grep"], "mcp_tools":"read_only", "prompt": "review the diff", "provider":"openai", "model":"gpt-5.5"},
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
	if review.Provider != "openai" || review.Model != "gpt-5.5" {
		t.Errorf("review provider/model = %q/%q", review.Provider, review.Model)
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
