package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/modelsdev"
)

func TestRunSetupWritesOnlySelectedModelsAndNoProxyDefault(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("1\n\nsave\n2\nsave\n"),
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
}

func TestRunSetupSIGINTCancelsCatalogFetch(t *testing.T) {
	home := t.TempDir()
	catalogStarted := make(chan struct{})
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--setup"},
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
		stdin:  strings.NewReader("openai-codex\n1\nsave\n"),
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
	if len(provider.Models) != 1 || provider.Models[0].Name != "gpt-test" || provider.Models[0].ContextWindow != 999000 {
		t.Fatalf("provider models = %+v, want gpt-test", provider.Models)
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
		stdin:  strings.NewReader("1\n\ncancel\n"),
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
		stdin:  strings.NewReader("1\n\n1\nsave\n"),
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
  "models": [{"name":"gpt-test","context_window":1000}]
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
	if len(provider.Models) != 1 || provider.Models[0].Name != "gpt-test" || provider.Models[0].ContextWindow != 999000 {
		t.Fatalf("provider models after refresh = %+v", provider.Models)
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
		args:   []string{"--refresh-models", "-config", cfgPath},
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
					Limit:       modelsdev.Limit{Context: 123000},
				},
				"beta": {
					ID:          "beta",
					Name:        "Beta",
					ReleaseDate: "2026-01-01",
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
				Reasoning:   true,
				Limit:       modelsdev.Limit{Context: 999000},
			},
		},
	}
	return catalog
}
