package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	if !strings.Contains(out.String(), setupSaveEmptySelectionPrompt) {
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
	cache, err := modelcatalog.DecodeModelsDev(bytes.NewReader(cacheData))
	if err != nil {
		t.Fatalf("decode models.dev cache: %v", err)
	}
	if _, ok := cache.Provider("testai"); !ok {
		t.Fatalf("models.dev cache missing testai provider")
	}
}

func TestSetupSelectionPagesAlignHighlightedRows(t *testing.T) {
	stripANSI := strings.NewReplacer("\x1b[1m", "", "\x1b[0m", "").Replace
	assertAligned := func(t *testing.T, output string, firstRow, secondRow int, firstText, secondText string) {
		t.Helper()
		lines := strings.Split(strings.TrimSuffix(stripANSI(output), "\n"), "\n")
		if len(lines) <= secondRow {
			t.Fatalf("selection output has %d lines, want row %d:\n%s", len(lines), secondRow, output)
		}
		firstColumn := strings.Index(lines[firstRow], firstText)
		secondColumn := strings.Index(lines[secondRow], secondText)
		if firstColumn < 0 || secondColumn < 0 {
			t.Fatalf("selection rows missing text %q or %q:\n%s", firstText, secondText, output)
		}
		if firstColumn != secondColumn {
			t.Fatalf("highlighted row text starts in column %d, want %d:\n%s", secondColumn, firstColumn, output)
		}
	}

	t.Run("providers", func(t *testing.T) {
		providers := []setupProviderPick{
			{Provider: modelcatalog.Provider{ID: "plain", Name: "Plain"}},
			{Provider: modelcatalog.Provider{ID: "selected-provider", Name: "Selected"}, Configured: true},
		}
		var out bytes.Buffer
		printSetupProviderSelectionPage(&out, providers, 0, 20, "")
		assertAligned(t, out.String(), 1, 2, "0 models", "0 models")
		assertAligned(t, out.String(), 1, 2, "Plain", "Selected")
	})

	t.Run("models", func(t *testing.T) {
		models := []setupModelPick{
			{Model: modelcatalog.Model{ID: "plain", Name: "Plain"}},
			{Model: modelcatalog.Model{ID: "selected-model", Name: "Selected"}, Enabled: true},
		}
		var out bytes.Buffer
		printSetupModelSelectionPage(&out, "provider", models, []int{0, 1}, 0, 20, "")
		assertAligned(t, out.String(), 2, 3, "Plain", "Selected")
	})
}

// TestRunSetupWritesManagedConfigWithoutPrices verifies that setup marks the
// written provider config managed and omits per-model prices, even when
// models.dev has prices for the selected model. The proxy resolves those prices
// live from the cache instead, so refreshes reach the server without a re-setup.
func TestRunSetupWritesManagedConfigWithoutPrices(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	priced := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"testai": {
			ID:   "testai",
			Name: "TestAI",
			API:  "https://api.test/v1",
			NPM:  "@ai-sdk/openai-compatible",
			Env:  []string{"TESTAI_API_KEY"},
			Models: map[string]modelcatalog.Model{
				"alpha": {
					ID:          "alpha",
					Name:        "Alpha",
					ReleaseDate: "2025-01-01",
					Modalities:  modelcatalog.Modalities{Input: []string{"text", "image"}},
					Limit:       modelcatalog.Limit{Context: 123000, Output: 12000},
					Cost:        llm.Price{Input: 2, Output: 4, CacheRead: 0.5, CacheWrite: 1},
					ServiceTiers: []llm.ServiceTier{{
						ID:      "fast",
						Name:    "Fast",
						Request: llm.ServiceTierRequest{ServiceTier: "priority"},
						Price:   llm.Price{Input: 4, Output: 8},
					}},
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	fast, ok := llm.ResolveServiceTier("fast", provider.Models[0].ServiceTiers)
	if !ok || fast.ID != "fast" || fast.Request.ServiceTier != "priority" || !fast.Price.IsZero() {
		t.Fatalf("managed model fast tier = %+v, %v", fast, ok)
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
		modelsDevCatalog: func(ctx context.Context) (*modelcatalog.Catalog, error) {
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	if provider.Name != modelcatalog.OpenAICodexProviderID ||
		provider.APIType != "responses" ||
		provider.BaseURL != modelcatalog.OpenAICodexProviderBaseURL ||
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
	if provider.PromptCache.KeyField != llm.PromptCacheKeyFieldPromptCacheKey {
		t.Fatalf("provider prompt cache key field = %q, want prompt_cache_key", provider.PromptCache.KeyField)
	}
	if !slices.Contains(provider.Models[0].InputModalities, "image") {
		t.Fatalf("provider input modalities = %+v, want image support", provider.Models[0].InputModalities)
	}
	if len(provider.Models[0].ReasoningOptions) != 1 || !slices.Contains(provider.Models[0].ReasoningOptions[0].Values, "xhigh") {
		t.Fatalf("provider reasoning options = %+v, want Codex effort values", provider.Models[0].ReasoningOptions)
	}
	if fast, ok := llm.ResolveServiceTier("fast", provider.Models[0].ServiceTiers); !ok || fast.ID != "fast" || fast.Request.ServiceTier != "priority" {
		t.Fatalf("provider fast tier = %+v, %v", fast, ok)
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
	if provider.ResponsesCompaction == nil || !*provider.ResponsesCompaction {
		t.Fatalf("codex responses_compaction = %v, want enabled", provider.ResponsesCompaction)
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalogWithSakana(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(out.String(), "SAKANA_API_KEY") {
		t.Fatalf("sakana setup should mention SAKANA_API_KEY, output=%q", out.String())
	}
	if !strings.Contains(out.String(), "$5/$30 ≤272k · $10/$45 >272k") {
		t.Fatalf("sakana setup should show every pricing tier, output=%q", out.String())
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
	if provider.Name != "sakana" ||
		provider.APIType != "responses" ||
		provider.BaseURL != "https://api.sakana.ai/v1" ||
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

func TestRunSetupOffersAuthenticatedLiveOnlyProviderModels(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"live-only"}]}`))
	}))
	defer server.Close()
	catalog := testSetupCatalogWithSakana()
	provider := catalog.Providers["sakana"]
	provider.API = server.URL
	catalog.Providers["sakana"] = provider
	var out, errw bytes.Buffer
	env := environment{
		stdin: strings.NewReader("sakana\nsecret\nlive-only\nsave\n"), stdout: &out, stderr: &errw,
		getenv: func(key string) string {
			if key == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog:     func(context.Context) (*modelcatalog.Catalog, error) { return catalog, nil },
		providerModelsClient: server.Client(), terminalRows: func() int { return 12 },
	}
	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "harness-model-proxy", "sakana.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].Models) != 1 || providers[0].Models[0].Name != "live-only" {
		t.Fatalf("providers = %+v", providers)
	}
}

func TestRunSetupWritesMetaResponsesProviderFromDirectCatalog(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
		  {"id":"muse-spark-1.3","object":"model","created":0,"owned_by":"meta"},
		  {"id":"muse-image-1.0","object":"model","created":0,"owned_by":"meta"},
		  {"id":"muse-voice-transcribe-1.0","object":"model","created":0,"owned_by":"meta"}
		]}`))
	}))
	defer server.Close()

	catalog := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"meta": {
			ID: "meta", Name: "Meta", API: server.URL + "/v1", NPM: "@ai-sdk/openai", Env: []string{"META_MODEL_API_KEY"},
			Models: map[string]modelcatalog.Model{
				"muse-spark-1.2": {ID: "muse-spark-1.2", Name: "Muse Spark 1.2"},
			},
		},
	}}
	var out, errw bytes.Buffer
	env := environment{
		stdin: strings.NewReader("meta\nsecret\nmuse-spark-1.3\nsave\n"), stdout: &out, stderr: &errw,
		getenv: func(key string) string {
			if key == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog:     func(context.Context) (*modelcatalog.Catalog, error) { return catalog, nil },
		providerModelsClient: server.Client(), terminalRows: func() int { return 12 },
	}
	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}
	if strings.Contains(out.String(), "muse-image") || strings.Contains(out.String(), "muse-voice") {
		t.Fatalf("setup offered non-Responses Meta models: %q", out.String())
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "harness-model-proxy", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].APIType != "responses" || len(providers[0].Models) != 1 || providers[0].Models[0].Name != "muse-spark-1.3" {
		t.Fatalf("Meta provider = %+v; config=%s", providers, data)
	}
}

func TestRunSetupWritesSakanaProviderWithoutModelShape(t *testing.T) {
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalogWithSakanaNoShape(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "harness-model-proxy", "sakana.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if provider.APIType != "responses" {
		t.Fatalf("sakana api_type = %q, want responses; config=%s", provider.APIType, data)
	}
}

func TestRunSetupWritesGoogleInteractionsProvider(t *testing.T) {
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
		provider.APIType != "interactions" ||
		provider.BaseURL != "https://generativelanguage.googleapis.com/v1beta" ||
		provider.APIKey != "" ||
		provider.Auth != nil {
		t.Fatalf("provider config = %+v", provider)
	}
	if provider.InteractionsStateful == nil || !*provider.InteractionsStateful {
		t.Fatalf("interactions_stateful = %v, want true", provider.InteractionsStateful)
	}
	if !slices.Equal(provider.ServerTools, []string{llm.ServerToolWebSearch}) {
		t.Fatalf("Google server tools = %v, want web_search", provider.ServerTools)
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

func TestGoogleSetupPreservesExplicitStatelessOverride(t *testing.T) {
	disabled := false
	cfg := setupProviderConfig{InteractionsStateful: &disabled}
	applySyntheticProviderDefaults(modelcatalog.Provider{ID: "google"}, &cfg)
	if cfg.InteractionsStateful == nil || *cfg.InteractionsStateful {
		t.Fatalf("interactions_stateful = %v, want preserved false", cfg.InteractionsStateful)
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"provider_configs":["testai.json"],"default_context_window":256000,"x-extension":{"nested":[1,true,"keep"]}}`), 0o600); err != nil {
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	configData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(configData, &raw); err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw["x-extension"]); err != nil {
		t.Fatal(err)
	}
	if got := compact.String(); got != `{"nested":[1,true,"keep"]}` {
		t.Fatalf("setup did not preserve unknown config field: %s", got)
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	cache, err := modelcatalog.DecodeModelsDev(bytes.NewReader(cacheData))
	if err != nil {
		t.Fatalf("decode models.dev cache: %v", err)
	}
	if _, ok := cache.Provider("testai"); !ok {
		t.Fatalf("models.dev cache missing refreshed testai provider")
	}
}

func TestRunRefreshModelsQueriesMetaAndMigratesToResponses(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
		  {"id":"muse-spark-1.2","object":"model","created":0,"owned_by":"meta"},
		  {"id":"muse-spark-1.3","object":"model","created":0,"owned_by":"meta"}
		]}`))
	}))
	defer server.Close()

	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["meta.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providerJSON := fmt.Sprintf(`{
	  "name":"meta","api_type":"openai","base_url":%q,"api_key":"secret","managed":true,
	  "models":[{"name":"muse-spark-1.2","context_window":1000}]
	}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(providerJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"meta": {
			ID: "meta", Name: "Meta", API: server.URL + "/v1", NPM: "@ai-sdk/openai", Env: []string{"META_MODEL_API_KEY"},
			Models: map[string]modelcatalog.Model{
				"muse-spark-1.2": {ID: "muse-spark-1.2", Name: "Muse Spark 1.2", Limit: modelcatalog.Limit{Context: 2000}},
			},
		},
	}}
	env := environment{
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, providerModelsClient: server.Client(),
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) { return catalog, nil },
	}
	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].APIType != "responses" || len(providers[0].Models) != 1 || providers[0].Models[0].Name != "muse-spark-1.2" {
		t.Fatalf("refreshed Meta provider = %+v; config=%s", providers, data)
	}
	snapshot, err := modeldiscovery.ReadProviderCache(dir, "meta", server.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Format != modeldiscovery.FormatMeta || !snapshot.Models["muse-spark-1.3"].Eligible {
		t.Fatalf("Meta direct snapshot = %+v", snapshot)
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	writeTestModelsDevCache(t, dir, &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"oldai": {
			ID:   "oldai",
			Name: "OldAI",
			API:  "https://old.example/v1",
			NPM:  "@ai-sdk/openai-compatible",
			Models: map[string]modelcatalog.Model{
				"old": {ID: "old", Name: "Old", Limit: modelcatalog.Limit{Context: 1}},
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	cache, err := modelcatalog.DecodeModelsDev(bytes.NewReader(cacheData))
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
			data, err := modelcatalog.EncodeModelsDev(testSetupCatalogWithModelCount(tc.candidate))
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

	data, err := modelcatalog.EncodeModelsDev(testSetupCatalogWithModelCount(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeModelsDevCache(dir, append(data, '\n')); err != nil {
		t.Fatalf("writeModelsDevCache first update: %v", err)
	}
	if stats := modelsDevCatalogStats(readTestModelsDevCachePath(t, modelsDevCacheBackupPath(dir))); stats.models != 2 {
		t.Fatalf("backup stats after first update = %+v, want 2 models", stats)
	}

	data, err = modelcatalog.EncodeModelsDev(testSetupCatalogWithModelCount(4))
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
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
	if provider.Name != modelcatalog.OpenAICodexProviderID ||
		provider.APIType != "responses" ||
		provider.BaseURL != modelcatalog.OpenAICodexProviderBaseURL ||
		provider.Auth == nil ||
		provider.Auth.Type != auth.TypeCodexOAuth ||
		provider.Auth.TokenFile != "tokens/custom-codex.json" {
		t.Fatalf("provider after refresh = %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "gpt-5.5" || provider.Models[0].ContextWindow != 272000 {
		t.Fatalf("provider models after refresh = %+v", provider.Models)
	}
	if fast, ok := llm.ResolveServiceTier("fast", provider.Models[0].ServiceTiers); !ok || fast.ID != "fast" || fast.Request.ServiceTier != "priority" {
		t.Fatalf("provider fast tier after refresh = %+v, %v", fast, ok)
	}
	if provider.Models[0].OutputLimit != 64000 {
		t.Fatalf("provider output limit after failed direct refresh = %d, want configured fallback 64000", provider.Models[0].OutputLimit)
	}
	if provider.PromptCache.KeyField != llm.PromptCacheKeyFieldPromptCacheKey {
		t.Fatalf("provider prompt cache key field after refresh = %q, want prompt_cache_key", provider.PromptCache.KeyField)
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
	if provider.ResponsesCompaction == nil || !*provider.ResponsesCompaction {
		t.Fatalf("codex responses_compaction after refresh = %v, want enabled", provider.ResponsesCompaction)
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
  "api_type": "openai",
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
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalogWithSakanaNoShape(), nil
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
	if provider.Name != "sakana" ||
		provider.APIType != "responses" ||
		provider.BaseURL != "https://api.sakana.ai/v1" ||
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

func TestRunRefreshModelsUsesAuthenticatedProviderAvailabilityWithoutAddingModels(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"fugu"},{"id":"new-model"}]}`))
	}))
	defer server.Close()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["sakana.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providerData := fmt.Sprintf(`{
  "name":"sakana","api_type":"openai","base_url":%q,"api_key":"sk-test","managed":true,
  "models":[{"name":"fugu","context_window":1000},{"name":"retired","context_window":1000}]
}`, server.URL)
	if err := os.WriteFile(filepath.Join(dir, "sakana.json"), []byte(providerData), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"sakana": {ID: "sakana", Name: "Sakana", API: server.URL, NPM: "@ai-sdk/openai-compatible", Models: map[string]modelcatalog.Model{
			"fugu":    {ID: "fugu", Limit: modelcatalog.Limit{Context: 2000}},
			"retired": {ID: "retired", Limit: modelcatalog.Limit{Context: 2000}},
		}},
	}}
	var out, errw bytes.Buffer
	env := environment{stdout: &out, stderr: &errw, providerModelsClient: server.Client(), modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) { return catalog, nil }}
	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "sakana.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].Models) != 1 || providers[0].Models[0].Name != "fugu" {
		t.Fatalf("providers = %+v, want only configured live fugu", providers)
	}
	if _, err := os.Stat(modeldiscovery.CachePath(dir, "sakana")); err != nil {
		t.Fatalf("provider cache: %v", err)
	}
}

func TestRunRefreshModelsPreservesConfiguredModelsOnProviderFailure(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["sakana.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providerData := fmt.Sprintf(`{
  "name":"sakana","api_type":"openai","base_url":%q,"api_key":"bad","managed":true,
  "models":[{"name":"fugu","context_window":1000},{"name":"configured-only","context_window":1000}]
}`, server.URL)
	if err := os.WriteFile(filepath.Join(dir, "sakana.json"), []byte(providerData), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"sakana": {ID: "sakana", Name: "Sakana", API: server.URL, NPM: "@ai-sdk/openai-compatible", Models: map[string]modelcatalog.Model{"fugu": {ID: "fugu", Limit: modelcatalog.Limit{Context: 2000}}}},
	}}
	var out, errw bytes.Buffer
	env := environment{stdout: &out, stderr: &errw, providerModelsClient: server.Client(), modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) { return catalog, nil }}
	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sakana.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].Models) != 2 {
		t.Fatalf("providers = %+v, want configured allowlist preserved", providers)
	}
	if !strings.Contains(errw.String(), "preserving configured models") {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestRunRefreshModelsReportsProgressAndTimesOutStalledProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["sakana.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sakana.json"), []byte(`{
  "name":"sakana","api_type":"openai","base_url":"https://provider.test/v1","api_key":"sk-test","managed":true,
  "models":[{"name":"fugu","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"sakana": {ID: "sakana", API: "https://provider.test/v1", NPM: "@ai-sdk/openai-compatible", Models: map[string]modelcatalog.Model{
			"fugu": {ID: "fugu", Limit: modelcatalog.Limit{Context: 2000}},
		}},
	}}
	timeout := time.Nanosecond
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		providerModelsClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
		providerModelsTimeout: &timeout,
		modelsDevCatalog:      func(context.Context) (*modelcatalog.Catalog, error) { return catalog, nil },
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	for _, want := range []string{
		"refreshing models.dev catalog",
		"models.dev catalog ready",
		`querying provider "sakana"`,
		"preserving configured models",
	} {
		if !strings.Contains(errw.String(), want) {
			t.Errorf("stderr missing %q: %q", want, errw.String())
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "sakana.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].Models) != 1 || providers[0].Models[0].Name != "fugu" {
		t.Fatalf("providers after timed-out refresh = %+v, want configured allowlist preserved", providers)
	}
}

func TestRefreshProviderAfterLoginUpdatesAllowlistAndPreservesDiscoveryOverride(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"fugu"},{"id":"new-model"}]}`))
	}))
	defer server.Close()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["sakana.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	includeUnknown := true
	current := llm.ProviderConfig{
		Name: "sakana", APIType: "openai", BaseURL: server.URL, APIKey: "secret", Managed: true,
		ModelDiscovery: &llm.ModelDiscoveryConfig{Enabled: &enabled, IncludeUnknownModels: &includeUnknown},
		Models:         []llm.ModelEntry{{Name: "fugu", ContextWindow: 1000}, {Name: "retired", ContextWindow: 1000}},
	}
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sakana.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	env := environment{stdout: &out, stderr: &errw, providerModelsClient: server.Client()}
	if err := refreshProviderAfterLogin(context.Background(), env, cfgPath, current); err != nil {
		t.Fatal(err)
	}
	updatedData, err := os.ReadFile(filepath.Join(dir, "sakana.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := llm.DecodeProviderConfigs(updatedData)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].Models) != 1 || providers[0].Models[0].Name != "fugu" {
		t.Fatalf("providers = %+v", providers)
	}
	if providers[0].ModelDiscovery == nil || providers[0].ModelDiscovery.IncludeUnknownModels == nil || !*providers[0].ModelDiscovery.IncludeUnknownModels {
		t.Fatalf("model discovery override was not preserved: %+v", providers[0].ModelDiscovery)
	}
	if !strings.Contains(out.String(), "rerun setup") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestRunRefreshModelsRemovesProviderMissingFromCatalog(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["testai.json","goneai.json"],"default_context_window":222000,"x-extension":{"nested":[1,true,"keep"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "api_key": "sk-test",
  "model_discovery": {"enabled":false},
  "models": [{"name":"alpha","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "goneai.json"), []byte(`{
  "name": "goneai",
  "api_type": "openai",
  "base_url": "https://api.gone/v1",
  "api_key": "sk-gone",
  "model_discovery": {"enabled":false},
  "models": [{"name":"gone-1","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(errw.String(), "provider \"goneai\"") || !strings.Contains(errw.String(), "no longer in the model catalog") {
		t.Fatalf("stderr should warn about the removed provider, got %q", errw.String())
	}
	// The surviving provider file is refreshed and kept.
	if _, err := os.Stat(filepath.Join(dir, "testai.json")); err != nil {
		t.Fatalf("testai.json should be kept: %v", err)
	}
	// The gone provider's file is deleted.
	if _, err := os.Stat(filepath.Join(dir, "goneai.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("goneai.json should be removed, stat err = %v", err)
	}
	// The main config no longer references the removed provider file, and other
	// keys are preserved.
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		ProviderConfigs      []string `json:"provider_configs"`
		DefaultContextWindow int      `json:"default_context_window"`
	}
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ProviderConfigs, []string{"testai.json"}) {
		t.Fatalf("provider_configs after refresh = %+v, want [testai.json]", cfg.ProviderConfigs)
	}
	if cfg.DefaultContextWindow != 222000 {
		t.Fatalf("default_context_window after refresh = %d, want 222000 preserved", cfg.DefaultContextWindow)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(cfgData, &raw); err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw["x-extension"]); err != nil {
		t.Fatal(err)
	}
	if got := compact.String(); got != `{"nested":[1,true,"keep"]}` {
		t.Fatalf("refresh did not preserve unknown config field: %s", got)
	}
}

func TestRunRefreshModelsRemovesMissingProviderConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["testai.json","missing.json"]}`), 0o600); err != nil {
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
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(errw.String(), "missing.json") || !strings.Contains(errw.String(), "no longer exists") {
		t.Fatalf("stderr should warn about the missing provider file, got %q", errw.String())
	}
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		ProviderConfigs []string `json:"provider_configs"`
	}
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ProviderConfigs, []string{"testai.json"}) {
		t.Fatalf("provider_configs after refresh = %+v, want [testai.json]", cfg.ProviderConfigs)
	}
}

func TestRunRefreshModelsDropsMissingModelKeepsOthers(t *testing.T) {
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
  "model_discovery": {"enabled":false},
  "models": [{"name":"alpha","context_window":1000},{"name":"retired","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(errw.String(), "model \"retired\"") || !strings.Contains(errw.String(), "no longer in the model catalog") {
		t.Fatalf("stderr should warn about the removed model, got %q", errw.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "testai.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatal(err)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "alpha" {
		t.Fatalf("provider models after refresh = %+v, want alpha only", provider.Models)
	}
}

func TestRunRefreshModelsRemovesProviderWithNoModelsRemaining(t *testing.T) {
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
  "model_discovery": {"enabled":false},
  "models": [{"name":"retired","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(errw.String(), "no models remaining") {
		t.Fatalf("stderr should warn about the emptied provider, got %q", errw.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "testai.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("testai.json should be removed, stat err = %v", err)
	}
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		ProviderConfigs []string `json:"provider_configs"`
	}
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProviderConfigs) != 0 {
		t.Fatalf("provider_configs after refresh = %+v, want empty", cfg.ProviderConfigs)
	}
}

func TestRunRefreshModelsDropsMissingProviderFromMultiProviderFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["providers.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(`[
  {
    "name": "testai",
    "api_type": "openai",
    "base_url": "https://api.test/v1",
    "api_key": "sk-test",
    "model_discovery": {"enabled":false},
    "models": [{"name":"alpha","context_window":1000}]
  },
  {
    "name": "goneai",
    "api_type": "openai",
    "base_url": "https://api.gone/v1",
    "api_key": "sk-gone",
    "model_discovery": {"enabled":false},
    "models": [{"name":"gone-1","context_window":1000}]
  }
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalog(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(errw.String(), "provider \"goneai\"") {
		t.Fatalf("stderr should warn about the removed provider, got %q", errw.String())
	}
	// The file survives (testai still present) and now holds only the kept provider.
	data, err := os.ReadFile(filepath.Join(dir, "providers.json"))
	if err != nil {
		t.Fatalf("providers.json should be kept: %v", err)
	}
	providers, err := llm.DecodeProviderConfigs(data)
	if err != nil {
		t.Fatalf("decode refreshed providers.json: %v", err)
	}
	if len(providers) != 1 || providers[0].Name != "testai" {
		t.Fatalf("providers after refresh = %+v, want testai only", providers)
	}
	// The config file still references the surviving provider file.
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfgData), "providers.json") {
		t.Fatalf("config should still reference providers.json, got %q", cfgData)
	}
}

func TestRunRefreshModelsRemovesUnsupportedProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["testai.json","legacy.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testai.json"), []byte(`{
  "name": "testai",
  "api_type": "openai",
  "base_url": "https://api.test/v1",
  "api_key": "sk-test",
  "model_discovery": {"enabled":false},
  "models": [{"name":"alpha","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), []byte(`{
  "name": "legacy",
  "api_type": "openai",
  "base_url": "https://api.legacy/v1",
  "api_key": "sk-legacy",
  "model_discovery": {"enabled":false},
  "models": [{"name":"legacy-1","context_window":1000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Catalog lists "legacy" but with no api/npm, so harness can't resolve a wire
	// shape for it: api_type and base_url both come back empty (unsupported).
	catalog := testSetupCatalog()
	catalog.Providers["legacy"] = modelcatalog.Provider{
		ID:   "legacy",
		Name: "Legacy",
		Models: map[string]modelcatalog.Model{
			"legacy-1": {ID: "legacy-1", Name: "Legacy 1", Limit: modelcatalog.Limit{Context: 1000}},
		},
	}
	var out, errw bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &errw,
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return catalog, nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v; stderr=%q", err, errw.String())
	}
	if !strings.Contains(errw.String(), "provider \"legacy\"") || !strings.Contains(errw.String(), "no longer supported by harness") {
		t.Fatalf("stderr should warn about the unsupported provider, got %q", errw.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "legacy.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy.json should be removed, stat err = %v", err)
	}
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		ProviderConfigs []string `json:"provider_configs"`
	}
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ProviderConfigs, []string{"testai.json"}) {
		t.Fatalf("provider_configs after refresh = %+v, want [testai.json]", cfg.ProviderConfigs)
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
		modelsDevCatalog: func(ctx context.Context) (*modelcatalog.Catalog, error) {
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

func TestPreserveReasoningReplayDomains(t *testing.T) {
	next := []setupModelConfig{{Name: "k3"}, {Name: "new"}}
	preserveReasoningReplayDomains([]llm.ModelEntry{
		{Name: "k3", ReasoningReplayDomain: "k3-family"},
		{Name: "removed", ReasoningReplayDomain: "old-family"},
	}, next)

	if next[0].ReasoningReplayDomain != "k3-family" {
		t.Fatalf("preserved domain = %q, want k3-family", next[0].ReasoningReplayDomain)
	}
	if next[1].ReasoningReplayDomain != "" {
		t.Fatalf("new model domain = %q, want empty default", next[1].ReasoningReplayDomain)
	}
}

func testSetupCatalog() *modelcatalog.Catalog {
	return &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"testai": {
			ID:   "testai",
			Name: "TestAI",
			API:  "https://api.test/v1",
			NPM:  "@ai-sdk/openai-compatible",
			Env:  []string{"TESTAI_API_KEY"},
			Models: map[string]modelcatalog.Model{
				"alpha": {
					ID:          "alpha",
					Name:        "Alpha",
					ReleaseDate: "2025-01-01",
					Modalities:  modelcatalog.Modalities{Input: []string{"text", "image"}},
					Limit:       modelcatalog.Limit{Context: 123000},
				},
				"beta": {
					ID:          "beta",
					Name:        "Beta",
					ReleaseDate: "2026-01-01",
					Modalities:  modelcatalog.Modalities{Input: []string{"text"}},
					Limit:       modelcatalog.Limit{Context: 456000},
				},
			},
		},
	}}
}

func testSetupCatalogWithOpenAI() *modelcatalog.Catalog {
	catalog := testSetupCatalog()
	catalog.Providers["openai"] = modelcatalog.Provider{
		ID:   "openai",
		Name: "OpenAI",
		API:  "https://api.openai.com/v1",
		NPM:  "@ai-sdk/openai",
		Env:  []string{"OPENAI_API_KEY"},
		Models: map[string]modelcatalog.Model{
			"gpt-test": {
				ID:          "gpt-test",
				Name:        "GPT Test",
				ReleaseDate: "2026-02-01",
				Modalities:  modelcatalog.Modalities{Input: []string{"text", "image"}},
				Reasoning:   true,
				Limit:       modelcatalog.Limit{Context: 999000, Output: 64000},
			},
		},
	}
	return catalog
}

func TestSetupProviderEnablesCompactionForFirstPartyOpenAIResponses(t *testing.T) {
	openAI := testSetupCatalogWithOpenAI().Providers["openai"]
	openAICfg := setupProviderFromCatalog(openAI, "sk-test", nil, []modelcatalog.Model{openAI.Models["gpt-test"]})
	if openAICfg.APIType != "responses" || openAICfg.ResponsesCompaction == nil || !*openAICfg.ResponsesCompaction {
		t.Fatalf("OpenAI setup config = %+v, want Responses compaction enabled", openAICfg)
	}

	codex := openAI
	codex.ID = modelcatalog.OpenAICodexProviderID
	codex.API = modelcatalog.OpenAICodexProviderBaseURL
	codexCfg := setupProviderFromCatalog(codex, "", &auth.Config{Type: auth.TypeCodexOAuth}, []modelcatalog.Model{openAI.Models["gpt-test"]})
	if codexCfg.ResponsesCompaction == nil || !*codexCfg.ResponsesCompaction {
		t.Fatalf("Codex setup config responses_compaction = %v, want enabled", codexCfg.ResponsesCompaction)
	}
}

func TestSetupProviderPreservesExplicitCodexCompactionOptOut(t *testing.T) {
	disabled := false
	current := llm.ProviderConfig{
		Name:                modelcatalog.OpenAICodexProviderID,
		APIType:             "responses",
		BaseURL:             modelcatalog.OpenAICodexProviderBaseURL,
		Managed:             true,
		ResponsesCompaction: &disabled,
		ResponsesToolSearch: &disabled,
		AnthropicToolSearch: llm.AnthropicToolSearchRegex,
	}
	meta := modelcatalog.Provider{
		ID:  modelcatalog.OpenAICodexProviderID,
		API: modelcatalog.OpenAICodexProviderBaseURL,
	}
	next := setupProviderFromCurrent(current, &meta, nil)
	if next.ResponsesCompaction == nil || *next.ResponsesCompaction {
		t.Fatalf("Codex responses_compaction = %v, want preserved false", next.ResponsesCompaction)
	}
	if next.ResponsesToolSearch == nil || *next.ResponsesToolSearch {
		t.Fatalf("Codex responses_tool_search = %v, want preserved false", next.ResponsesToolSearch)
	}
	if next.AnthropicToolSearch != llm.AnthropicToolSearchRegex {
		t.Fatalf("anthropic_tool_search = %q, want preserved regex", next.AnthropicToolSearch)
	}
}

func testSetupCatalogWithSakana() *modelcatalog.Catalog {
	sakanaReasoning := []llm.ReasoningOption{{Type: "effort", Values: []string{"high", "xhigh"}}}
	return &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"sakana": {
			ID:   "sakana",
			Name: "Sakana AI",
			API:  "https://api.sakana.ai/v1",
			Env:  []string{"SAKANA_API_KEY"},
			NPM:  "@ai-sdk/openai-compatible",
			Models: map[string]modelcatalog.Model{
				"fugu": {
					ID:               "fugu",
					Name:             "Fugu",
					ReleaseDate:      "2026-06-15",
					Modalities:       modelcatalog.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
					Reasoning:        true,
					ReasoningOptions: append([]llm.ReasoningOption(nil), sakanaReasoning...),
					Limit:            modelcatalog.Limit{Context: 1_000_000, Output: 1_000_000},
					Provider:         modelcatalog.ModelProvider{Shape: "responses"},
				},
				"fugu-ultra": {
					ID:               "fugu-ultra",
					Name:             "Fugu Ultra",
					ReleaseDate:      "2026-06-15",
					Modalities:       modelcatalog.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
					Reasoning:        true,
					ReasoningOptions: append([]llm.ReasoningOption(nil), sakanaReasoning...),
					Limit:            modelcatalog.Limit{Context: 1_000_000, Output: 1_000_000},
					Provider:         modelcatalog.ModelProvider{Shape: "responses"},
					Cost:             llm.Price{Input: 5, Output: 30, CacheRead: 0.5, Tiers: []llm.PriceTier{{Threshold: 272_000, Input: 10, Output: 45, CacheRead: 1.0}}},
				},
				"fugu-ultra-20260615": {
					ID:               "fugu-ultra-20260615",
					Name:             "Fugu Ultra 20260615",
					ReleaseDate:      "2026-06-15",
					Modalities:       modelcatalog.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
					Reasoning:        true,
					ReasoningOptions: append([]llm.ReasoningOption(nil), sakanaReasoning...),
					Limit:            modelcatalog.Limit{Context: 1_000_000, Output: 1_000_000},
					Provider:         modelcatalog.ModelProvider{Shape: "responses"},
					Cost:             llm.Price{Input: 5, Output: 30, CacheRead: 0.5, Tiers: []llm.PriceTier{{Threshold: 272_000, Input: 10, Output: 45, CacheRead: 1.0}}},
				},
			},
		},
	}}
}

func testSetupCatalogWithSakanaNoShape() *modelcatalog.Catalog {
	catalog := testSetupCatalogWithSakana()
	provider := catalog.Providers["sakana"]
	for id, model := range provider.Models {
		model.Provider.Shape = ""
		provider.Models[id] = model
	}
	catalog.Providers["sakana"] = provider
	return catalog
}

func testSetupCatalogWithGoogle() *modelcatalog.Catalog {
	return &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"google": {
			ID:   "google",
			Name: "Google",
			NPM:  "@ai-sdk/google",
			Env:  []string{"GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY"},
			Models: map[string]modelcatalog.Model{
				"gemini-test": {
					ID:          "gemini-test",
					Name:        "Gemini Test",
					ReleaseDate: "2026-03-01",
					Limit:       modelcatalog.Limit{Context: 1000000},
				},
			},
		},
	}}
}

func testSetupCatalogWithModelCount(count int) *modelcatalog.Catalog {
	models := make(map[string]modelcatalog.Model, count)
	for i := range count {
		id := fmt.Sprintf("model-%02d", i+1)
		models[id] = modelcatalog.Model{
			ID:    id,
			Name:  "Model " + id,
			Limit: modelcatalog.Limit{Context: 1000 + i},
		}
	}
	return &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"testai": {
			ID:     "testai",
			Name:   "TestAI",
			API:    "https://api.test/v1",
			NPM:    "@ai-sdk/openai-compatible",
			Models: models,
		},
	}}
}

func writeTestModelsDevCache(t *testing.T, dir string, catalog *modelcatalog.Catalog) {
	t.Helper()
	data, err := modelcatalog.EncodeModelsDev(catalog)
	if err != nil {
		t.Fatalf("encode models.dev cache: %v", err)
	}
	if err := writeModelsDevCache(dir, append(data, '\n')); err != nil {
		t.Fatalf("write models.dev cache: %v", err)
	}
}

func readTestModelsDevCache(t *testing.T, dir string) *modelcatalog.Catalog {
	t.Helper()
	return readTestModelsDevCachePath(t, modelsDevCachePath(dir))
}

func readTestModelsDevCachePath(t *testing.T, path string) *modelcatalog.Catalog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read models.dev cache: %v", err)
	}
	catalog, err := modelcatalog.DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode models.dev cache: %v", err)
	}
	return catalog
}

// testSetupCatalogWithKimiForCoding mirrors the models.dev shape for Kimi for
// Coding: the Anthropic SDK package and the shared /coding/v1 base URL.
func testSetupCatalogWithKimiForCoding() *modelcatalog.Catalog {
	return &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
		"kimi-for-coding": {
			ID:   "kimi-for-coding",
			Name: "Kimi For Coding",
			API:  "https://api.kimi.com/coding/v1",
			NPM:  "@ai-sdk/anthropic",
			Env:  []string{"KIMI_API_KEY"},
			Models: map[string]modelcatalog.Model{
				"kimi-k3": {
					ID:          "kimi-k3",
					Name:        "Kimi K3",
					ReleaseDate: "2026-07-01",
					Modalities:  modelcatalog.Modalities{Input: []string{"text"}},
					Reasoning:   true,
					Limit:       modelcatalog.Limit{Context: 1000000, Output: 64000},
				},
			},
		},
	}}
}

// TestRunSetupWritesKimiForCodingOpenAIDialect verifies setup resolves
// kimi-for-coding to the OpenAI chat-completions dialect with reasoning replay
// enabled, despite models.dev listing the Anthropic SDK package.
func TestRunSetupWritesKimiForCodingOpenAIDialect(t *testing.T) {
	home := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		stdin:  strings.NewReader("kimi-for-coding\n\nall\nsave\n"),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalogWithKimiForCoding(), nil
		},
		terminalRows: func() int { return 12 },
	}

	if err := runSetup(context.Background(), env, false); err != nil {
		t.Fatalf("runSetup: %v; stderr=%q", err, errw.String())
	}

	providerData, err := os.ReadFile(filepath.Join(home, ".config", "harness-model-proxy", "kimi-for-coding.json"))
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	if !strings.Contains(string(providerData), `"reasoning_replay": true`) {
		t.Fatalf("provider config = %s, want reasoning_replay:true", providerData)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(providerData, &provider); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if provider.APIType != "openai" {
		t.Fatalf("api_type = %q, want openai (dual-protocol override)", provider.APIType)
	}
	if provider.ReasoningReplay != llm.ReasoningReplayFull {
		t.Fatalf("reasoning_replay = %q, want full", provider.ReasoningReplay)
	}
	if provider.BaseURL != "https://api.kimi.com/coding/v1" {
		t.Fatalf("base_url = %q, want the shared /coding/v1 base", provider.BaseURL)
	}
}

// TestRunRefreshModelsRewritesKimiForCodingToOpenAI verifies refresh-models
// upgrades an existing Anthropic-dialect kimi-for-coding config to the OpenAI
// dialect with reasoning replay while refreshing model metadata.
func TestRunRefreshModelsRewritesKimiForCodingToOpenAI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["kimi-for-coding.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kimi-for-coding.json"), []byte(`{
  "name": "kimi-for-coding",
  "api_type": "anthropic",
  "base_url": "https://api.kimi.com/coding/v1",
  "managed": true,
  "models": [{"name":"kimi-k3","context_window":256000}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := environment{
		stdout: &out,
		stderr: &bytes.Buffer{},
		modelsDevCatalog: func(context.Context) (*modelcatalog.Catalog, error) {
			return testSetupCatalogWithKimiForCoding(), nil
		},
	}

	if err := runRefreshModels(context.Background(), env, cfgPath); err != nil {
		t.Fatalf("runRefreshModels: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "kimi-for-coding.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reasoning_replay": true`) {
		t.Fatalf("provider config after refresh = %s, want reasoning_replay:true", data)
	}
	var provider setupProviderConfig
	if err := json.Unmarshal(data, &provider); err != nil {
		t.Fatal(err)
	}
	if provider.APIType != "openai" {
		t.Fatalf("api_type after refresh = %q, want openai", provider.APIType)
	}
	if provider.ReasoningReplay != llm.ReasoningReplayFull {
		t.Fatalf("reasoning_replay after refresh = %q, want full", provider.ReasoningReplay)
	}
	if len(provider.Models) != 1 || provider.Models[0].Name != "kimi-k3" || provider.Models[0].ContextWindow != 1000000 {
		t.Fatalf("models after refresh = %+v, want kimi-k3 metadata refreshed", provider.Models)
	}
}
