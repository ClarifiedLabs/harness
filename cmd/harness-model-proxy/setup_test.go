package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/modelsdev"
)

func TestRunSetupWritesOnlySelectedModelsAndNoProxyDefault(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("testai\n\nsave\n2\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalog(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(out.String(), "(0 enabled)") {
		t.Fatalf("model selector should start with no enabled models, output=%q", out.String())
	}
	if !strings.Contains(out.String(), "Select at least one model before continuing.") {
		t.Fatalf("saving without a selected model should explain the required selection, output=%q", out.String())
	}
	if !strings.Contains(out.String(), "*") || !strings.Contains(out.String(), "\x1b[1m") {
		t.Fatalf("model selector should mark enabled rows with star and bold, output=%q", out.String())
	}

	dir := filepath.Join(home, ".config", "harness-model-proxy")
	configData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read proxy config: %v", err)
	}
	var mainConfig map[string]json.RawMessage
	if err := json.Unmarshal(configData, &mainConfig); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	if _, ok := mainConfig["provider"]; ok {
		t.Fatalf("proxy config should not contain provider: %s", configData)
	}
	if _, ok := mainConfig["model"]; ok {
		t.Fatalf("proxy config should not contain model: %s", configData)
	}

	providerData, err := os.ReadFile(filepath.Join(dir, "testai.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "alpha" {
		t.Fatalf("provider models = %+v, want only alpha", provider.Models)
	}
	cacheData, err := os.ReadFile(modelsDevCachePath(dir))
	if err != nil {
		t.Fatalf("read models.dev cache: %v", err)
	}
	cache, err := modelsdev.Decode(bytes.NewReader(cacheData))
	if err != nil {
		t.Fatalf("decode models.dev cache: %v", err)
	}
	if _, ok := cache.Provider("testai"); !ok {
		t.Fatalf("models.dev cache missing testai provider")
	}
}

// TestRunSetupWritesManagedConfigWithoutPrices verifies that setup marks the
// written provider config managed and omits per-model prices, even when
// models.dev has prices for the selected model. The proxy resolves those prices
// live from the cache instead, so refreshes reach the server without a re-setup.
func TestRunSetupWritesManagedConfigWithoutPrices(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	priced := &modelsdev.Catalog{Providers: map[string]modelsdev.Provider{
		"testai": {
			ID:   "testai",
			Name: "TestAI",
			API:  "https://api.test/v1",
			NPM:  "@ai-sdk/openai-compatible",
			Env:  []string{"TESTAI_API_KEY"},
			Models: map[string]modelsdev.Model{
				"alpha": {
					ID:          "alpha",
					Name:        "Alpha",
					ReleaseDate: "2025-01-01",
					Modalities:  modelsdev.Modalities{Input: []string{"text", "image"}},
					Limit:       modelsdev.Limit{Context: 123000, Output: 12000},
					Cost:        llm.Price{Input: 2, Output: 4, CacheRead: 0.5, CacheWrite: 1},
				},
			},
		},
	}}
	env := environment{
		stdin:  strings.NewReader("testai\n\nall\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return priced, nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}

	dir := filepath.Join(home, ".config", "harness-model-proxy")
	providerData, err := os.ReadFile(filepath.Join(dir, "testai.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	if bytes.Contains(providerData, []byte("\"price\"")) {
		t.Fatalf("managed provider config should omit per-model prices: %s", providerData)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if !provider.Managed {
		t.Fatalf("provider config managed = false, want true: %s", providerData)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "alpha" {
		t.Fatalf("provider models = %+v, want only alpha", provider.Models)
	}
	if provider.Models[0].Price != nil {
		t.Fatalf("managed model price = %+v, want nil (resolved from models.dev cache)", provider.Models[0].Price)
	}
	if provider.Models[0].ContextWindow != 123000 {
		t.Fatalf("managed model context window = %d, want 123000", provider.Models[0].ContextWindow)
	}
	if provider.Models[0].OutputLimit != 12000 {
		t.Fatalf("managed model output limit = %d, want 12000", provider.Models[0].OutputLimit)
	}
	if !slices.Equal(provider.Models[0].InputModalities, []string{"text", "image"}) {
		t.Fatalf("managed model input modalities = %+v, want text,image", provider.Models[0].InputModalities)
	}
}

func TestRunSetupSIGINTCancelsCatalogFetch(t *testing.T) {
	home := t.TempDir()
	catalogStarted := make(chan struct{})
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"setup"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		sigCh: make(chan os.Signal, 1),
		modelsDevCatalog: func(ctx context.Context) (*modelsdev.Catalog, error) {
			close(catalogStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		terminalRows: func() int { return 12 },
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case <-catalogStarted:
	case <-time.After(time.Second):
		t.Fatal("setup did not start catalog fetch")
	}
	env.sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != exitInterrupt {
			t.Fatalf("setup SIGINT exit = %d, want %d; stderr=%q", code, exitInterrupt, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("setup did not exit after SIGINT")
	}
	if out.Len() != 0 {
		t.Fatalf("interrupted setup should not prompt; stdout=%q", out.String())
	}
}

func TestRunSetupWritesOpenAICodexProvider(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("openai-codex\ngpt-5.5\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalogWithOpenAI(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	if strings.Contains(out.String(), "API key") {
		t.Fatalf("openai-codex setup should not prompt for an API key, output=%q", out.String())
	}

	dir := filepath.Join(home, ".config", "harness-model-proxy")
	configData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read proxy config: %v", err)
	}
	var mainConfig setupMainConfig
	if err := json.Unmarshal(configData, &mainConfig); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	if len(mainConfig.ProviderConfigs) != 1 || mainConfig.ProviderConfigs[0] != "openai-codex.json" {
		t.Fatalf("provider configs = %+v, want openai-codex.json", mainConfig.ProviderConfigs)
	}

	providerData, err := os.ReadFile(filepath.Join(dir, "openai-codex.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if provider.Name != openAICodexProviderID ||
		provider.APIType != "responses" ||
		provider.BaseURL != openAICodexProviderBaseURL ||
		provider.APIKey != "" ||
		len(provider.APIKeyEnv) != 0 ||
		provider.Auth == nil ||
		provider.Auth.Type != auth.TypeCodexOAuth {
		t.Fatalf("provider config = %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "gpt-5.5" || provider.Models[0].ContextWindow != 272000 {
		t.Fatalf("provider models = %+v, want Codex gpt-5.5 with 272000 context", provider.Models)
	}
	if provider.Models[0].OutputLimit != 0 {
		t.Fatalf("provider output limit = %d, want omitted", provider.Models[0].OutputLimit)
	}
	if !slices.Contains(provider.Models[0].InputModalities, "image") {
		t.Fatalf("provider input modalities = %+v, want image support", provider.Models[0].InputModalities)
	}
	if len(provider.Models[0].ReasoningOptions) != 1 || !slices.Contains(provider.Models[0].ReasoningOptions[0].Values, "xhigh") {
		t.Fatalf("provider reasoning options = %+v, want Codex effort values", provider.Models[0].ReasoningOptions)
	}
	if !provider.Managed {
		t.Fatalf("codex provider should be managed: %+v", provider)
	}
	if provider.PriceSource != "" {
		t.Fatalf("codex price_source = %q, want omitted for subscription provider", provider.PriceSource)
	}
	if !provider.OmitMaxOutputTokens {
		t.Fatalf("codex omit_max_output_tokens = false, want true")
	}
	if provider.ResponsesStateful != nil {
		t.Fatalf("codex responses_stateful = %v, want omitted default", provider.ResponsesStateful)
	}
	if provider.ResponsesWebSocket != nil {
		t.Fatalf("codex responses_websocket = %v, want omitted runtime default", provider.ResponsesWebSocket)
	}
}

func TestRunSetupWritesSakanaProvider(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("sakana\n\nfugu-ultra\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalog(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(out.String(), "SAKANA_API_KEY") {
		t.Fatalf("sakana setup should mention SAKANA_API_KEY, output=%q", out.String())
	}

	dir := filepath.Join(home, ".config", "harness-model-proxy")
	configData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read proxy config: %v", err)
	}
	var mainConfig setupMainConfig
	if err := json.Unmarshal(configData, &mainConfig); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	if len(mainConfig.ProviderConfigs) != 1 || mainConfig.ProviderConfigs[0] != "sakana.json" {
		t.Fatalf("provider configs = %+v, want sakana.json", mainConfig.ProviderConfigs)
	}

	providerData, err := os.ReadFile(filepath.Join(dir, "sakana.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	if bytes.Contains(providerData, []byte("\"price\"")) {
		t.Fatalf("sakana managed config should omit flat per-model prices: %s", providerData)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if provider.Name != sakanaProviderID ||
		provider.APIType != "responses" ||
		provider.BaseURL != sakanaProviderBaseURL ||
		provider.APIKey != "" ||
		provider.Auth != nil {
		t.Fatalf("provider config = %+v", provider)
	}
	if !provider.Managed {
		t.Fatalf("sakana provider should be managed: %+v", provider)
	}
	if provider.ResponsesStateful == nil || *provider.ResponsesStateful {
		t.Fatalf("sakana responses_stateful = %v, want false", provider.ResponsesStateful)
	}
	if !slices.Equal(provider.APIKeyEnv, []string{"SAKANA_API_KEY"}) {
		t.Fatalf("sakana API key env = %+v, want SAKANA_API_KEY", provider.APIKeyEnv)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "fugu-ultra" {
		t.Fatalf("provider models = %+v, want fugu-ultra", provider.Models)
	}
	model := provider.Models[0]
	if model.ContextWindow != 1_000_000 {
		t.Fatalf("fugu-ultra context = %d, want 1000000", model.ContextWindow)
	}
	if !slices.Equal(model.InputModalities, []string{"text", "image"}) {
		t.Fatalf("fugu-ultra input modalities = %+v, want text,image", model.InputModalities)
	}
	if model.Reasoning == nil || !*model.Reasoning {
		t.Fatalf("fugu-ultra reasoning = %v, want true", model.Reasoning)
	}
	if len(model.ReasoningOptions) != 1 || !slices.Equal(model.ReasoningOptions[0].Values, []string{"high", "xhigh"}) {
		t.Fatalf("fugu-ultra reasoning options = %+v, want high,xhigh", model.ReasoningOptions)
	}
}

func TestRunSetupWritesGoogleOpenAICompatibleProvider(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("google\n\n1\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalogWithGoogle(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}

	dir := filepath.Join(home, ".config", "harness-model-proxy")
	providerData, err := os.ReadFile(filepath.Join(dir, "google.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if provider.Name != "google" ||
		provider.APIType != "openai" ||
		provider.BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" ||
		provider.APIKey != "" ||
		provider.Auth != nil {
		t.Fatalf("provider config = %+v", provider)
	}
	if len(provider.APIKeyEnv) != 3 ||
		provider.APIKeyEnv[0] != "GOOGLE_API_KEY" ||
		provider.APIKeyEnv[1] != "GOOGLE_GENERATIVE_AI_API_KEY" ||
		provider.APIKeyEnv[2] != "GEMINI_API_KEY" {
		t.Fatalf("provider API key env = %+v", provider.APIKeyEnv)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "gemini-test" || provider.Models[0].ContextWindow != 1000000 {
		t.Fatalf("provider models = %+v, want gemini-test", provider.Models)
	}
}

func TestRunAuthStatusForProvider(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "harness-model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"provider_configs":["p.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.json"), []byte(`{
  "name": "p",
  "api_type": "openai",
  "base_url": "https://api.example/v1",
  "auth": {
    "type": "oauth2",
    "flow": "device_code",
    "client_id": "client",
    "token_url": "https://auth.example/token",
    "device_url": "https://auth.example/device"
  },
  "models": [{"name":"m","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"auth", "status", "p"},
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
	}
	if code := run(env); code != exitOK {
		t.Fatalf("auth status exit = %d, want 0; stderr=%q", code, errw.String())
	}
	if got := out.String(); !strings.Contains(got, "p: not logged in") {
		t.Fatalf("auth status output = %q", got)
	}
}

func TestRunSetupModelSelectorCancelDoesNotWriteConfig(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("testai\n\ncancel\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalog(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err == nil || err.Error() != "setup cancelled" {
		t.Fatalf("runSetup error = %v, want setup cancelled; stderr=%q", err, errw.String())
	}
	dir := filepath.Join(home, ".config", "harness-model-proxy")
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "testai.json")); !os.IsNotExist(err) {
		t.Fatalf("testai.json stat error = %v, want not exist", err)
	}
}

func TestRunSetupUpdatesExistingProviderConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "harness-model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"provider_configs":["testai.json"],"default_context_window":256000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "api_key": "sk-existing",
  "models": [{"name":"alpha","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("testai\n\n1\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalog(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	output := out.String()
	if !strings.Contains(output, "*   1.") || !strings.Contains(output, "\x1b[1mtestai\x1b[0m") || !strings.Contains(output, "\x1b[1mTestAI\x1b[0m") {
		t.Fatalf("provider selector should mark existing provider with star and bold, output=%q", output)
	}
	if !strings.Contains(output, "(1 enabled)") {
		t.Fatalf("model selector should start from existing allowlist, output=%q", output)
	}
	if !strings.Contains(output, "Beta") {
		t.Fatalf("model selector should show disabled catalog models for existing providers, output=%q", output)
	}
	if !strings.Contains(output, "Updated "+filepath.Join(dir, "testai.json")) {
		t.Fatalf("setup should report provider update, output=%q", output)
	}

	providerData, err := os.ReadFile(filepath.Join(dir, "testai.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if provider.APIKey != "sk-existing" {
		t.Fatalf("provider API key = %q, want preserved existing key", provider.APIKey)
	}
	if len(provider.Models) != 2 || provider.Models[0].Name != "alpha" || provider.Models[1].Name != "beta" {
		t.Fatalf("provider models = %+v, want alpha and beta", provider.Models)
	}
}

func TestRunSetupUsesCachedCatalogWhenFetchFails(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "harness-model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestModelsDevCache(t, dir, testSetupCatalog())
	fetches := 0
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("testai\n\n1\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			fetches++
			return nil, errors.New("network down")
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	if fetches != 0 {
		t.Fatalf("setup should use fresh cache without fetching, fetches=%d", fetches)
	}
	if strings.Contains(errw.String(), "vendored fallback") {
		t.Fatalf("setup should not use vendored fallback when cache is valid, stderr=%q", errw.String())
	}
}

func TestSetupCatalogUsesFallbackOnlyAfterBadCacheAndFetchFailure(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "harness-model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsDevCachePath(dir), []byte(`{bad json`), 0o600); err != nil {
		t.Fatal(err)
	}
	var errw bytes.Buffer
	env := environment{
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return nil, errors.New("network down")
		},
	}

	catalog, err := setupCatalog(context.Background(), env)
	if err != nil {
		t.Fatalf("setupCatalog: %v", err)
	}
	if len(catalog.Providers) == 0 {
		t.Fatalf("fallback catalog has no providers")
	}
	if !strings.Contains(errw.String(), "cached models.dev catalog failed") ||
		!strings.Contains(errw.String(), "using vendored fallback") {
		t.Fatalf("stderr should explain bad cache/fallback, got %q", errw.String())
	}
}

func TestRunRefreshModelsPreservesConfiguredModelAllowlist(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["testai.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "api_key": "sk-test",
  "models": [{"name":"alpha","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &bytes.Buffer{},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "testai.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatal(err)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "alpha" || provider.Models[0].ContextWindow != 123000 {
		t.Fatalf("provider models after refresh = %+v, want refreshed alpha only", provider.Models)
	}
	if !provider.Managed {
		t.Fatalf("refreshed provider config managed = false, want true: %s", data)
	}
	if provider.Models[0].Price != nil || bytes.Contains(data, []byte("\"price\"")) {
		t.Fatalf("refreshed managed config should omit per-model prices: %s", data)
	}
	cacheData, err := os.ReadFile(modelsDevCachePath(dir))
	if err != nil {
		t.Fatalf("read models.dev cache: %v", err)
	}
	cache, err := modelsdev.Decode(bytes.NewReader(cacheData))
	if err != nil {
		t.Fatalf("decode models.dev cache: %v", err)
	}
	if _, ok := cache.Provider("testai"); !ok {
		t.Fatalf("models.dev cache missing refreshed testai provider")
	}
}

func TestRunRefreshModelsFallsBackToCachedCatalog(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["testai.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "api_key": "sk-test",
  "models": [{"name":"alpha","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestModelsDevCache(t, dir, testSetupCatalog())
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return nil, errors.New("network down")
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "testai.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatal(err)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "alpha" || provider.Models[0].ContextWindow != 123000 {
		t.Fatalf("provider models after refresh = %+v, want cached alpha metadata", provider.Models)
	}
	if !strings.Contains(errw.String(), "using cached catalog") {
		t.Fatalf("stderr should mention cached catalog, got %q", errw.String())
	}
}

func TestRefreshModelsDevCacheIfStaleUpdatesOldCache(t *testing.T) {
	dir := t.TempDir()
	writeTestModelsDevCache(t, dir, &modelsdev.Catalog{Providers: map[string]modelsdev.Provider{
		"oldai": {
			ID:   "oldai",
			Name: "OldAI",
			API:  "https://old.example/v1",
			NPM:  "@ai-sdk/openai-compatible",
			Models: map[string]modelsdev.Model{
				"old": {ID: "old", Name: "Old", Limit: modelsdev.Limit{Context: 1}},
			},
		},
	}})
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	old := base.Add(-2 * time.Hour)
	if err := os.Chtimes(modelsDevCachePath(dir), old, old); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	env := environment{
		stderr: &bytes.Buffer{},
		now:    func() time.Time { return base },
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			fetches++
			return testSetupCatalog(), nil
		},
	}

	refreshModelsDevCacheIfStale(context.Background(), env, dir, time.Hour, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	cacheData, err := os.ReadFile(modelsDevCachePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cache, err := modelsdev.Decode(bytes.NewReader(cacheData))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Provider("testai"); !ok {
		t.Fatalf("cache was not refreshed with testai provider")
	}
}

func TestWriteModelsDevCacheRejectsInvalidCandidate(t *testing.T) {
	dir := t.TempDir()
	writeTestModelsDevCache(t, dir, testSetupCatalog())

	err := writeModelsDevCache(dir, []byte(`{bad json`))
	if err == nil || !strings.Contains(err.Error(), "did not parse") {
		t.Fatalf("writeModelsDevCache error = %v, want parse failure", err)
	}
	cache := readTestModelsDevCache(t, dir)
	if stats := modelsDevCatalogStats(cache); stats.providers != 1 || stats.models != 2 {
		t.Fatalf("cache stats after rejected write = %+v, want original 1 provider/2 models", stats)
	}
}

func TestWriteModelsDevCacheRejectsHugeCountSwing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		current   int
		candidate int
	}{
		{name: "shrink", current: 200, candidate: 40},
		{name: "growth", current: 40, candidate: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestModelsDevCache(t, dir, testSetupCatalogWithModelCount(tc.current))
			data, err := modelsdev.Encode(testSetupCatalogWithModelCount(tc.candidate))
			if err != nil {
				t.Fatal(err)
			}

			err = writeModelsDevCache(dir, append(data, '\n'))
			if err == nil || !strings.Contains(err.Error(), "count changed too much") {
				t.Fatalf("writeModelsDevCache error = %v, want count swing failure", err)
			}
			cache := readTestModelsDevCache(t, dir)
			if stats := modelsDevCatalogStats(cache); stats.providers != 1 || stats.models != tc.current {
				t.Fatalf("cache stats after rejected write = %+v, want original 1 provider/%d models", stats, tc.current)
			}
		})
	}
}

func TestWriteModelsDevCacheBacksUpPreviousCache(t *testing.T) {
	dir := t.TempDir()
	writeTestModelsDevCache(t, dir, testSetupCatalogWithModelCount(2))

	data, err := modelsdev.Encode(testSetupCatalogWithModelCount(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeModelsDevCache(dir, append(data, '\n')); err != nil {
		t.Fatalf("writeModelsDevCache first update: %v", err)
	}
	if stats := modelsDevCatalogStats(readTestModelsDevCachePath(t, modelsDevCacheBackupPath(dir))); stats.models != 2 {
		t.Fatalf("backup stats after first update = %+v, want 2 models", stats)
	}

	data, err = modelsdev.Encode(testSetupCatalogWithModelCount(4))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeModelsDevCache(dir, append(data, '\n')); err != nil {
		t.Fatalf("writeModelsDevCache second update: %v", err)
	}
	if stats := modelsDevCatalogStats(readTestModelsDevCache(t, dir)); stats.models != 4 {
		t.Fatalf("live cache stats after second update = %+v, want 4 models", stats)
	}
	if stats := modelsDevCatalogStats(readTestModelsDevCachePath(t, modelsDevCacheBackupPath(dir))); stats.models != 3 {
		t.Fatalf("backup stats after second update = %+v, want previous 3 models", stats)
	}
}

func TestRunRefreshModelsHandlesOpenAICodexProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["openai-codex.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "openai-codex.json"), []byte(`{
  "name": "openai-codex",
  "api_type": "responses",
  "base_url": "https://chatgpt.com/backend-api/codex",
	  "auth": {"type":"codex_oauth","token_file":"tokens/custom-codex.json"},
	  "price_source": "openai",
	  "models": [{"name":"gpt-5.5","context_window":1000,"output_limit":64000}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &bytes.Buffer{},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalogWithOpenAI(), nil
		},
		codexModelsData: func(context.Context) ([]byte, error) {
			return []byte(testCodexModelsCatalogJSON()), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "openai-codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Name != openAICodexProviderID ||
		provider.APIType != "responses" ||
		provider.BaseURL != openAICodexProviderBaseURL ||
		provider.Auth == nil ||
		provider.Auth.Type != auth.TypeCodexOAuth ||
		provider.Auth.TokenFile != "tokens/custom-codex.json" {
		t.Fatalf("provider after refresh = %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "gpt-5.5" || provider.Models[0].ContextWindow != 272000 {
		t.Fatalf("provider models after refresh = %+v", provider.Models)
	}
	if provider.Models[0].OutputLimit != 0 {
		t.Fatalf("provider output limit after refresh = %d, want omitted", provider.Models[0].OutputLimit)
	}
	if provider.PriceSource != "" {
		t.Fatalf("codex price_source after refresh = %q, want omitted for subscription provider", provider.PriceSource)
	}
	if !provider.OmitMaxOutputTokens {
		t.Fatalf("codex omit_max_output_tokens after refresh = false, want true")
	}
	if provider.ResponsesStateful != nil {
		t.Fatalf("codex responses_stateful after refresh = %v, want omitted default", provider.ResponsesStateful)
	}
	if provider.ResponsesWebSocket != nil {
		t.Fatalf("codex responses_websocket after refresh = %v, want omitted runtime default", provider.ResponsesWebSocket)
	}
}

func TestRunRefreshModelsHandlesSakanaProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["sakana.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sakana.json"), []byte(`{
  "name": "sakana",
  "api_type": "responses",
  "base_url": "https://api.sakana.ai/v1",
  "api_key": "sk-existing",
  "responses_stateful": true,
  "models": [{"name":"fugu","context_window":1000,"price":{"input":99,"output":99}}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &bytes.Buffer{},
		modelsDevCatalog: func(context.Context) (*modelsdev.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sakana.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("\"price\"")) {
		t.Fatalf("sakana refreshed config should omit flat per-model prices: %s", data)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Name != sakanaProviderID ||
		provider.APIType != "responses" ||
		provider.BaseURL != sakanaProviderBaseURL ||
		provider.APIKey != "sk-existing" {
		t.Fatalf("provider after refresh = %+v", provider)
	}
	if !provider.Managed {
		t.Fatalf("sakana provider should be managed after refresh: %+v", provider)
	}
	if provider.ResponsesStateful == nil || *provider.ResponsesStateful {
		t.Fatalf("sakana responses_stateful after refresh = %v, want false", provider.ResponsesStateful)
	}
	if !slices.Equal(provider.APIKeyEnv, []string{"SAKANA_API_KEY"}) {
		t.Fatalf("sakana API key env after refresh = %+v, want SAKANA_API_KEY", provider.APIKeyEnv)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "fugu" {
		t.Fatalf("provider models after refresh = %+v, want fugu", provider.Models)
	}
	model := provider.Models[0]
	if model.ContextWindow != 1_000_000 {
		t.Fatalf("fugu context after refresh = %d, want 1000000", model.ContextWindow)
	}
	if !slices.Equal(model.InputModalities, []string{"text", "image"}) {
		t.Fatalf("fugu input modalities after refresh = %+v, want text,image", model.InputModalities)
	}
	if len(model.ReasoningOptions) != 1 || !slices.Equal(model.ReasoningOptions[0].Values, []string{"high", "xhigh"}) {
		t.Fatalf("fugu reasoning options after refresh = %+v, want high,xhigh", model.ReasoningOptions)
	}
}

func TestOpenAICodexProviderUsesListVisibleCodexModels(t *testing.T) {
	catalog, err := decodeCodexModels([]byte(testCodexModelsCatalogJSON()))
	if err != nil {
		t.Fatalf("decodeCodexModels: %v", err)
	}
	provider, ok := openAICodexProvider(catalog)
	if !ok {
		t.Fatal("openAICodexProvider = false, want provider")
	}
	if len(provider.Models) != 1 {
		t.Fatalf("provider models = %+v, want only one list-visible supported model", provider.Models)
	}
	model, ok := provider.Models["gpt-5.5"]
	if !ok {
		t.Fatalf("provider models = %+v, want gpt-5.5", provider.Models)
	}
	if model.Limit.Context != 272000 {
		t.Fatalf("gpt-5.5 context = %d, want 272000", model.Limit.Context)
	}
	if model.Limit.Output != 0 {
		t.Fatalf("gpt-5.5 output limit = %d, want omitted", model.Limit.Output)
	}
	if !model.Reasoning || len(model.ReasoningOptions) != 1 || !slices.Contains(model.ReasoningOptions[0].Values, "xhigh") {
		t.Fatalf("gpt-5.5 reasoning = %v options=%+v, want Codex effort options", model.Reasoning, model.ReasoningOptions)
	}
	if _, ok := provider.Models["codex-auto-review"]; ok {
		t.Fatalf("hidden codex-auto-review should not be exposed: %+v", provider.Models)
	}
	if _, ok := provider.Models["unsupported"]; ok {
		t.Fatalf("unsupported model should not be exposed: %+v", provider.Models)
	}
}

func TestRunRefreshModelsSIGINTCancelsCatalogFetch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["testai.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalogStarted := make(chan struct{})
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"refresh-models", "-config", cfgPath},
		stdout: &out,
		stderr: &errw,
		sigCh:  make(chan os.Signal, 1),
		modelsDevCatalog: func(ctx context.Context) (*modelsdev.Catalog, error) {
			close(catalogStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case <-catalogStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start catalog fetch")
	}
	env.sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != exitInterrupt {
			t.Fatalf("refresh SIGINT exit = %d, want %d; stderr=%q", code, exitInterrupt, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("refresh did not exit after SIGINT")
	}
	if out.Len() != 0 {
		t.Fatalf("interrupted refresh should not print updates; stdout=%q", out.String())
	}
}

func testSetupCatalog() *modelsdev.Catalog {
	return &modelsdev.Catalog{Providers: map[string]modelsdev.Provider{
		"testai": {
			ID:   "testai",
			Name: "TestAI",
			API:  "https://api.test/v1",
			NPM:  "@ai-sdk/openai-compatible",
			Env:  []string{"TESTAI_API_KEY"},
			Models: map[string]modelsdev.Model{
				"alpha": {
					ID:          "alpha",
					Name:        "Alpha",
					ReleaseDate: "2025-01-01",
					Modalities:  modelsdev.Modalities{Input: []string{"text", "image"}},
					Limit:       modelsdev.Limit{Context: 123000},
				},
				"beta": {
					ID:          "beta",
					Name:        "Beta",
					ReleaseDate: "2026-01-01",
					Modalities:  modelsdev.Modalities{Input: []string{"text"}},
					Limit:       modelsdev.Limit{Context: 456000},
				},
			},
		},
	}}
}

func testSetupCatalogWithOpenAI() *modelsdev.Catalog {
	catalog := testSetupCatalog()
	catalog.Providers["openai"] = modelsdev.Provider{
		ID:   "openai",
		Name: "OpenAI",
		API:  "https://api.openai.com/v1",
		NPM:  "@ai-sdk/openai",
		Env:  []string{"OPENAI_API_KEY"},
		Models: map[string]modelsdev.Model{
			"gpt-test": {
				ID:          "gpt-test",
				Name:        "GPT Test",
				ReleaseDate: "2026-02-01",
				Modalities:  modelsdev.Modalities{Input: []string{"text", "image"}},
				Reasoning:   true,
				Limit:       modelsdev.Limit{Context: 999000, Output: 64000},
			},
		},
	}
	return catalog
}

func testCodexModelsCatalogJSON() string {
	return `{
  "models": [
    {
      "slug": "gpt-5.5",
      "display_name": "GPT-5.5",
      "context_window": 272000,
      "max_context_window": 272000,
      "input_modalities": ["text", "image"],
      "supported_reasoning_levels": [
        {"effort": "low"},
        {"effort": "medium"},
        {"effort": "high"},
        {"effort": "xhigh"}
      ],
      "visibility": "list",
      "supported_in_api": true
    },
    {
      "slug": "codex-auto-review",
      "display_name": "Codex Auto Review",
      "context_window": 272000,
      "input_modalities": ["text", "image"],
      "visibility": "hide",
      "supported_in_api": true
    },
    {
      "slug": "unsupported",
      "display_name": "Unsupported",
      "context_window": 128000,
      "input_modalities": ["text"],
      "visibility": "list",
      "supported_in_api": false
    }
  ]
}`
}

func testSetupCatalogWithGoogle() *modelsdev.Catalog {
	return &modelsdev.Catalog{Providers: map[string]modelsdev.Provider{
		"google": {
			ID:   "google",
			Name: "Google",
			NPM:  "@ai-sdk/google",
			Env:  []string{"GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY"},
			Models: map[string]modelsdev.Model{
				"gemini-test": {
					ID:          "gemini-test",
					Name:        "Gemini Test",
					ReleaseDate: "2026-03-01",
					Limit:       modelsdev.Limit{Context: 1000000},
				},
			},
		},
	}}
}

func testSetupCatalogWithModelCount(count int) *modelsdev.Catalog {
	models := make(map[string]modelsdev.Model, count)
	for i := range count {
		id := fmt.Sprintf("model-%02d", i+1)
		models[id] = modelsdev.Model{
			ID:    id,
			Name:  "Model " + id,
			Limit: modelsdev.Limit{Context: 1000 + i},
		}
	}
	return &modelsdev.Catalog{Providers: map[string]modelsdev.Provider{
		"testai": {
			ID:     "testai",
			Name:   "TestAI",
			API:    "https://api.test/v1",
			NPM:    "@ai-sdk/openai-compatible",
			Models: models,
		},
	}}
}

func writeTestModelsDevCache(t *testing.T, dir string, catalog *modelsdev.Catalog) {
	t.Helper()
	data, err := modelsdev.Encode(catalog)
	if err != nil {
		t.Fatalf("encode models.dev cache: %v", err)
	}
	if err := writeModelsDevCache(dir, append(data, '\n')); err != nil {
		t.Fatalf("write models.dev cache: %v", err)
	}
}

func readTestModelsDevCache(t *testing.T, dir string) *modelsdev.Catalog {
	t.Helper()
	return readTestModelsDevCachePath(t, modelsDevCachePath(dir))
}

func readTestModelsDevCachePath(t *testing.T, path string) *modelsdev.Catalog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read models.dev cache: %v", err)
	}
	catalog, err := modelsdev.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode models.dev cache: %v", err)
	}
	return catalog
}
