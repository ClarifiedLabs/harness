package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProviderConfigsWarnsAndSkipsMissingFile(t *testing.T) {
	var warnings []string
	r, providers, err := LoadProviderConfigs(t.TempDir(), []string{"missing.json"}, func(msg string) {
		warnings = append(warnings, msg)
	})
	if err != nil {
		t.Fatalf("LoadProviderConfigs: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("providers = %d, want 0", len(providers))
	}
	if _, known := r.Cost("anything", Usage{InputTokens: 1}); known {
		t.Fatalf("missing file should not register models")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "missing.json") {
		t.Fatalf("warning = %v, want one warning naming missing.json", warnings)
	}
}

func TestLoadProviderConfigsReadsProviderWrapper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	body := `{
  "providers": [
    {
      "name": "openrouter",
      "api_type": "openai",
      "base_url": "https://openrouter.ai/api/v1",
      "models": [
        {"name":"openai/gpt-5.1","context_window":1000000,"output_limit":96000,"price":{"input":2,"output":8},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]}
      ]
    },
    {
      "name": "anthropic",
      "api_type": "anthropic",
      "models": [
        {"name":"claude-sonnet-4-5","context_window":1000000,"price":{"input":3,"output":15}}
      ]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write provider config: %v", err)
	}

	r, providers, err := LoadProviderConfigs(dir, []string{"providers.json"}, nil)
	if err != nil {
		t.Fatalf("LoadProviderConfigs: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(providers))
	}
	if got := r.ContextWindow("openai/gpt-5.1"); got != 1_000_000 {
		t.Fatalf("openai/gpt-5.1 context window = %d, want 1000000", got)
	}
	if got := r.OutputLimit("openai/gpt-5.1"); got != 96_000 {
		t.Fatalf("openai/gpt-5.1 output limit = %d, want 96000", got)
	}
	// A model with no configured output limit reports 0 so the dialect falls back.
	if got := r.OutputLimit("claude-sonnet-4-5"); got != 0 {
		t.Fatalf("claude-sonnet-4-5 output limit = %d, want 0", got)
	}
	// The raw provider config entry must also carry the output limit; the proxy
	// resolves it from there at dispatch.
	if got := providers[0].Models[0].OutputLimit; got != 96_000 {
		t.Fatalf("provider entry output limit = %d, want 96000", got)
	}
	info, ok := r.Lookup("openai/gpt-5.1")
	if !ok || info.Reasoning == nil || !info.Reasoning.SupportsEffort("medium") {
		t.Fatalf("openai/gpt-5.1 reasoning info = %+v, ok=%v", info.Reasoning, ok)
	}
	cost, known := r.Cost("claude-sonnet-4-5", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !known || cost != 18 {
		t.Fatalf("claude cost known=%v cost=%v, want true and 18", known, cost)
	}
}

func TestRegistryFromProviderConfigsBuildsLookups(t *testing.T) {
	providers := []ProviderConfig{{
		Name: "openrouter",
		Models: []ModelEntry{{
			Name:          "openai/gpt-5.5",
			ContextWindow: 1_000_000,
			OutputLimit:   64_000,
			Price:         Price{Input: 5, Output: 30},
		}},
	}}
	r := RegistryFromProviderConfigs(providers)

	for _, name := range []string{"openai/gpt-5.5", "openrouter:openai/gpt-5.5"} {
		info, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) not found", name)
		}
		if info.ContextWindow != 1_000_000 || info.OutputLimit != 64_000 {
			t.Fatalf("Lookup(%q) = %+v, want window 1M / output 64k", name, info)
		}
		if info.Price != (Price{Input: 5, Output: 30}) {
			t.Fatalf("Lookup(%q) price = %+v, want {5,30}", name, info.Price)
		}
	}

	cost, known := r.Cost("openrouter:openai/gpt-5.5", Usage{InputTokens: 1_000_000})
	if !known || cost != 5 {
		t.Fatalf("Cost = %v (known=%v), want 5", cost, known)
	}
}

func TestMergeModelFillsOutputLimit(t *testing.T) {
	r := NewRegistry(map[string]ModelInfo{
		"gpt-x": {ContextWindow: 200_000},
	})
	// Discovered metadata fills the missing output limit.
	r.MergeModel("gpt-x", ModelInfo{OutputLimit: 48_000})
	if got := r.OutputLimit("gpt-x"); got != 48_000 {
		t.Fatalf("output limit after merge = %d, want 48000", got)
	}
	// An explicit registry value is not overwritten by later discovery.
	r.MergeModel("gpt-x", ModelInfo{OutputLimit: 1_000})
	if got := r.OutputLimit("gpt-x"); got != 48_000 {
		t.Fatalf("output limit = %d, want 48000 (explicit value preserved)", got)
	}
}
