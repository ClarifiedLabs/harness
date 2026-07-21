package modelcatalog

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
)

const testCatalog = `{
  "openrouter": {
    "id": "openrouter",
    "name": "OpenRouter",
    "api": "https://openrouter.ai/api/v1",
    "env": ["OPENROUTER_API_KEY"],
    "npm": "@openrouter/ai-sdk-provider",
    "models": {
      "openai/gpt-5.5": {
        "id": "openai/gpt-5.5",
        "name": "GPT-5.5",
        "release_date": "2026-06-01",
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "reasoning": true,
        "reasoning_options": [{"type":"effort","values":["low","medium","high"]}],
        "limit": {"context": 1050000, "output": 64000},
        "cost": {"input": 5, "output": 30, "cache_read": 0.5}
      }
    }
  },
  "openai": {
    "id": "openai",
    "name": "OpenAI",
    "env": ["OPENAI_API_KEY"],
    "npm": "@ai-sdk/openai",
    "models": {
      "gpt-5.5": {
        "id": "gpt-5.5",
        "name": "GPT-5.5",
        "release_date": "2026-06-01",
        "reasoning": true,
        "reasoning_options": [{"type":"effort","values":["none","low","medium","high","xhigh"]}],
        "limit": {"context": 1050000},
        "cost": {"input": 5, "output": 30, "cache_read": 0.5},
        "experimental": {"modes": {"fast": {
          "cost": {"input": 10, "output": 60, "cache_read": 1},
          "provider": {"body": {"service_tier": "priority"}}
        }}}
      }
    }
  },
  "anthropic": {
    "id": "anthropic",
    "name": "Anthropic",
    "env": ["ANTHROPIC_API_KEY"],
    "npm": "@ai-sdk/anthropic",
    "models": {
      "claude-opus-4-8": {
        "id": "claude-opus-4-8",
        "name": "Claude Opus 4.8",
        "release_date": "2026-05-01",
        "reasoning": true,
        "reasoning_options": [{"type":"effort","values":["low","medium","high","xhigh","max"]}],
        "limit": {"context": 1000000},
        "cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25},
        "experimental": {"modes": {"fast": {
          "cost": {"input": 30, "output": 150, "cache_read": 3, "cache_write": 37.5},
          "provider": {
            "body": {"speed": "fast"},
            "headers": {"anthropic-beta": "fast-mode-2026-02-01"}
          }
        }}}
      }
    }
  }
}`

func TestDecodeProviderBaseURLAndModelPricing(t *testing.T) {
	c, err := DecodeModelsDev(strings.NewReader(testCatalog))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	p, ok := c.Provider("openrouter")
	if !ok {
		t.Fatal("openrouter provider not found")
	}
	if got := p.BaseURL(); got != "https://openrouter.ai/api/v1" {
		t.Fatalf("BaseURL = %q", got)
	}
	if got := p.APIType(); got != "openai" {
		t.Fatalf("APIType = %q, want openai", got)
	}
	info, ok := p.ModelInfo("openai/gpt-5.5")
	if !ok {
		t.Fatal("model not found")
	}
	if info.ContextWindow != 1_050_000 || info.OutputLimit != 64_000 || info.Price.Input != 5 || info.Price.Output != 30 || info.Price.CacheRead != 0.5 {
		t.Fatalf("model info = %+v", info)
	}
	if !slices.Equal(info.InputModalities, []string{"text", "image"}) {
		t.Fatalf("input modalities = %+v, want text,image", info.InputModalities)
	}
	if info.Reasoning == nil || !info.Reasoning.SupportsEffort("high") {
		t.Fatalf("reasoning info = %+v, want high effort support", info.Reasoning)
	}

	// The output limit must also reach the synthesized provider config's ModelEntry,
	// which is the field the proxy resolves at dispatch.
	pc := p.ProviderConfig("sk-test")
	var entry llm.ModelEntry
	for _, e := range pc.Models {
		if e.Name == "openai/gpt-5.5" {
			entry = e
		}
	}
	if entry.ContextWindow != 1_050_000 || entry.OutputLimit != 64_000 || !slices.Equal(entry.InputModalities, []string{"text", "image"}) {
		t.Fatalf("provider config entry = %+v, want context 1050000 / output 64000", entry)
	}
}

func TestFirstPartyProviderFallbacks(t *testing.T) {
	c, err := DecodeModelsDev(strings.NewReader(testCatalog))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	openai, _ := c.Provider("openai")
	if got := openai.BaseURL(); got != "https://api.openai.com/v1" {
		t.Fatalf("openai BaseURL = %q", got)
	}
	if got := openai.APIType(); got != "responses" {
		t.Fatalf("openai APIType = %q", got)
	}
	anthropic, _ := c.Provider("anthropic")
	if got := anthropic.BaseURL(); got != "https://api.anthropic.com" {
		t.Fatalf("anthropic BaseURL = %q", got)
	}
	if got := anthropic.APIType(); got != "anthropic" {
		t.Fatalf("anthropic APIType = %q", got)
	}
}

func TestDecodeModelsDevServiceTierModes(t *testing.T) {
	c, err := DecodeModelsDev(strings.NewReader(testCatalog))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	openai, _ := c.Provider("openai")
	openaiInfo, _ := openai.ModelInfo("gpt-5.5")
	fast, ok := llm.ResolveServiceTier("fast", openaiInfo.ServiceTiers)
	if !ok || fast.ID != "fast" || fast.Request.ServiceTier != "priority" || fast.Price.Input != 10 || fast.Price.Output != 60 {
		t.Fatalf("OpenAI fast tier = %+v, %v", fast, ok)
	}
	anthropic, _ := c.Provider("anthropic")
	anthropicInfo, _ := anthropic.ModelInfo("claude-opus-4-8")
	fast, ok = llm.ResolveServiceTier("fast", anthropicInfo.ServiceTiers)
	if !ok || fast.ID != "fast" || fast.Request.Speed != "fast" || !slices.Equal(fast.Request.Betas, []string{"fast-mode-2026-02-01"}) || fast.Price.Input != 30 {
		t.Fatalf("Anthropic fast tier = %+v, %v", fast, ok)
	}
}

func TestPruneModelsDevDataPreservesNormalizedServiceTierModes(t *testing.T) {
	data, err := PruneModelsDevData([]byte(testCatalog))
	if err != nil {
		t.Fatalf("PruneModelsDevData: %v", err)
	}
	c, err := DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode pruned catalog: %v", err)
	}
	openai, _ := c.Provider("openai")
	info, _ := openai.ModelInfo("gpt-5.5")
	fast, ok := llm.ResolveServiceTier("fast", info.ServiceTiers)
	if !ok || fast.Request.ServiceTier != "priority" || fast.Price.Output != 60 {
		t.Fatalf("pruned OpenAI fast tier = %+v, %v", fast, ok)
	}
}

func TestProviderFallbacksFromNPM(t *testing.T) {
	tests := []struct {
		name        string
		provider    Provider
		wantBaseURL string
		wantAPIType string
	}{
		{
			name:        "openai sdk",
			provider:    Provider{ID: "custom-openai", NPM: "@ai-sdk/openai"},
			wantBaseURL: "https://api.openai.com/v1",
			wantAPIType: "openai",
		},
		{
			name:        "anthropic sdk",
			provider:    Provider{ID: "custom-anthropic", NPM: "@ai-sdk/anthropic"},
			wantBaseURL: "https://api.anthropic.com",
			wantAPIType: "anthropic",
		},
		{
			name:        "google sdk",
			provider:    Provider{ID: "google", NPM: "@ai-sdk/google"},
			wantBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			wantAPIType: "openai",
		},
		{
			name:        "google vertex sdk unsupported",
			provider:    Provider{ID: "google-vertex", NPM: "@ai-sdk/google-vertex"},
			wantBaseURL: "",
			wantAPIType: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.provider.BaseURL(); got != tt.wantBaseURL {
				t.Fatalf("BaseURL = %q, want %q", got, tt.wantBaseURL)
			}
			if got := tt.provider.APIType(); got != tt.wantAPIType {
				t.Fatalf("APIType = %q, want %q", got, tt.wantAPIType)
			}
		})
	}
}

func TestDecodeCatalogWrapper(t *testing.T) {
	c, err := DecodeModelsDev(strings.NewReader(`{"providers":` + testCatalog + `,"models":{}}`))
	if err != nil {
		t.Fatalf("Decode wrapper: %v", err)
	}
	if _, ok := c.Provider("openai"); !ok {
		t.Fatal("openai provider not found in wrapper catalog")
	}
}

func TestModelsByReleaseDateNewestFirst(t *testing.T) {
	c, err := DecodeModelsDev(strings.NewReader(`{
  "openai": {
    "id": "openai",
    "name": "OpenAI",
    "models": {
      "old": {"id":"old","name":"Old","release_date":"2024-01-01"},
      "new": {"id":"new","name":"New","release_date":"2026-01-01"},
      "updated": {"id":"updated","name":"Updated","last_updated":"2025-01-01"}
    }
  }
}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	p, _ := c.Provider("openai")
	models := p.ModelsByReleaseDate()
	if got := []string{models[0].ID, models[1].ID, models[2].ID}; got[0] != "new" || got[1] != "updated" || got[2] != "old" {
		t.Fatalf("release sort = %v", got)
	}
}

func TestModelsDevFallbackSnapshotDecodes(t *testing.T) {
	assertModelsDevFallbackJSON(t, modelsDevFallbackJSON)
}

func TestModelsDevFallbackCandidateDecodes(t *testing.T) {
	path := os.Getenv("MODELSDEV_FALLBACK_CANDIDATE")
	if path == "" {
		t.Skip("MODELSDEV_FALLBACK_CANDIDATE is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Read candidate: %v", err)
	}
	assertModelsDevFallbackJSON(t, data)
}

func TestAPITypeFromModelShape(t *testing.T) {
	responses := Provider{
		ID:  "sakana",
		NPM: "@ai-sdk/openai-compatible",
		API: "https://api.sakana.ai/v1",
		Env: []string{"SAKANA_API_KEY"},
		Models: map[string]Model{
			"fugu": {ID: "fugu", Provider: ModelProvider{Shape: "responses"}},
		},
	}
	if got := responses.APIType(); got != "responses" {
		t.Fatalf("shape-responses APIType = %q, want responses", got)
	}

	completions := Provider{
		ID:  "azure",
		NPM: "@ai-sdk/azure",
		Models: map[string]Model{
			"kimi": {ID: "kimi", Provider: ModelProvider{Shape: "completions"}},
		},
	}
	if got := completions.APIType(); got != "openai" {
		t.Fatalf("shape-completions APIType = %q, want openai", got)
	}
}

func TestDecodeTieredPrice(t *testing.T) {
	catalog := `{
  "testai": {
    "id": "testai",
    "name": "TestAI",
    "env": ["TESTAI_API_KEY"],
    "npm": "@ai-sdk/openai-compatible",
    "api": "https://api.test/v1",
    "models": {
      "alpha": {
        "id": "alpha",
        "name": "Alpha",
        "release_date": "2026-01-01",
        "modalities": {"input": ["text"], "output": ["text"]},
        "limit": {"context": 1050000},
        "provider": {"shape": "responses"},
        "cost": {
          "input": 5,
          "output": 30,
          "cache_read": 0.5,
          "tiers": [
            {"input": 10, "output": 45, "cache_read": 1.0, "tier": {"type": "context", "size": 272000}}
          ]
        }
      }
    }
  }
}`
	c, err := DecodeModelsDev(strings.NewReader(catalog))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	p, ok := c.Provider("testai")
	if !ok {
		t.Fatal("testai provider not found")
	}
	if got := p.APIType(); got != "responses" {
		t.Fatalf("APIType = %q, want responses", got)
	}
	m, ok := p.Models["alpha"]
	if !ok {
		t.Fatal("alpha model not found")
	}
	if m.Provider.Shape != "responses" {
		t.Fatalf("model shape = %q, want responses", m.Provider.Shape)
	}
	if len(m.Cost.Tiers) != 1 {
		t.Fatalf("tiers = %+v, want one tier", m.Cost.Tiers)
	}
	if m.Cost.Tiers[0].Threshold != 272_000 {
		t.Fatalf("tier threshold = %d, want 272000", m.Cost.Tiers[0].Threshold)
	}
	if m.Cost.Tiers[0].Input != 10 || m.Cost.Tiers[0].Output != 45 {
		t.Fatalf("tier price = %+v, want input=10 output=45", m.Cost.Tiers[0])
	}
	info, ok := p.ModelInfo("alpha")
	if !ok {
		t.Fatal("ModelInfo not found")
	}
	if info.Shape != "responses" {
		t.Fatalf("ModelInfo.Shape = %q, want responses", info.Shape)
	}
}

func assertModelsDevFallbackJSON(t *testing.T, data []byte) {
	t.Helper()
	c, err := DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Providers) == 0 {
		t.Fatal("decoded no providers")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if _, ok := raw["providers"]; ok {
		t.Fatal("expected models.dev api.json provider map, got catalog wrapper with providers key")
	}
	if _, ok := raw["models"]; ok {
		t.Fatal("expected models.dev api.json provider map, got model-only or catalog data with models key")
	}

	for _, providerData := range raw {
		var provider struct {
			Models map[string]json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(providerData, &provider); err == nil && len(provider.Models) > 0 {
			return
		}
	}
	t.Fatal("expected at least one provider entry with models")
}
