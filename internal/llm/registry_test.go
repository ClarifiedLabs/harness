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
        {"name":"openai/gpt-5.1","context_window":1000000,"price":{"input":2,"output":8},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]}
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
	info, ok := r.Lookup("openai/gpt-5.1")
	if !ok || info.Reasoning == nil || !info.Reasoning.SupportsEffort("medium") {
		t.Fatalf("openai/gpt-5.1 reasoning info = %+v, ok=%v", info.Reasoning, ok)
	}
	cost, known := r.Cost("claude-sonnet-4-5", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !known || cost != 18 {
		t.Fatalf("claude cost known=%v cost=%v, want true and 18", known, cost)
	}
}
