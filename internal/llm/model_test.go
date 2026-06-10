package llm

import (
	"os"
	"path/filepath"
	"testing"
)

func testRegistry() *Registry {
	return NewRegistry(map[string]ModelInfo{
		"claude-opus-4-8": {
			ContextWindow: 1_000_000,
			Price:         Price{Input: 5.0, Output: 25.0, CacheRead: 0.5, CacheWrite: 6.25},
		},
	})
}

func TestCostKnownModel(t *testing.T) {
	r := testRegistry()
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	cost, known := r.Cost("claude-opus-4-8", u)
	if !known {
		t.Fatalf("Cost(known model) known = false, want true")
	}
	if cost <= 0 {
		t.Fatalf("Cost(known model) = %v, want > 0", cost)
	}
}

func TestCostUnknownModel(t *testing.T) {
	r := testRegistry()
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	cost, known := r.Cost("totally-made-up-model", u)
	if known {
		t.Fatalf("Cost(unknown model) known = true, want false")
	}
	if cost != 0 {
		t.Fatalf("Cost(unknown model) = %v, want 0", cost)
	}
}

func TestCostComponents(t *testing.T) {
	r := testRegistry()
	base := Usage{InputTokens: 1_000_000}
	withCache := Usage{InputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000}

	baseCost, ok1 := r.Cost("claude-opus-4-8", base)
	cacheCost, ok2 := r.Cost("claude-opus-4-8", withCache)
	if !ok1 || !ok2 {
		t.Fatalf("expected known model")
	}
	if cacheCost <= baseCost {
		t.Fatalf("cache usage cost %v should exceed base cost %v", cacheCost, baseCost)
	}
}

func TestContextWindowDefault(t *testing.T) {
	r := testRegistry()
	if got := r.ContextWindow("unknown-model"); got != DefaultContextWindow {
		t.Fatalf("ContextWindow(unknown) = %d, want %d", got, DefaultContextWindow)
	}
}

func TestContextWindowDefaultCanBeConfigured(t *testing.T) {
	r := testRegistry()
	r.SetDefaultContextWindow(512_000)
	if got := r.ContextWindow("unknown-model"); got != 512_000 {
		t.Fatalf("ContextWindow(unknown) = %d, want 512000", got)
	}
	if got := r.ContextWindow("claude-opus-4-8"); got != 1_000_000 {
		t.Fatalf("configured default should not replace known model window, got %d", got)
	}
}

func TestContextWindowKnown(t *testing.T) {
	r := testRegistry()
	got := r.ContextWindow("claude-opus-4-8")
	if got != 1_000_000 {
		t.Fatalf("ContextWindow(claude-opus-4-8) = %d, want 1000000", got)
	}
}

func TestModelsSorted(t *testing.T) {
	r := NewRegistry(map[string]ModelInfo{
		"z-model": {},
		"a-model": {},
	})
	got := r.Models()
	want := []string{"a-model", "z-model"}
	if len(got) != len(want) {
		t.Fatalf("Models length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Models = %v, want %v", got, want)
		}
	}
}

func TestLoadProviderConfigsAddsQualifiedLookupWithoutListingDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(`[
  {"name":"openai","models":[{"name":"gpt-5.5","context_window":400000}]},
  {"name":"openrouter","models":[{"name":"gpt-5.5","context_window":1050000}]}
]`), 0o644); err != nil {
		t.Fatalf("write providers: %v", err)
	}
	r, _, err := LoadProviderConfigs(dir, []string{"providers.json"}, nil)
	if err != nil {
		t.Fatalf("LoadProviderConfigs: %v", err)
	}
	if got := r.ContextWindow("openrouter:gpt-5.5"); got != 1_050_000 {
		t.Fatalf("qualified context = %d, want 1050000", got)
	}
	models := r.Models()
	if len(models) != 1 || models[0] != "gpt-5.5" {
		t.Fatalf("listed models = %v, want only unqualified gpt-5.5", models)
	}
}

func TestRegistryEntriesWellFormed(t *testing.T) {
	r := testRegistry()
	for name, info := range r.models {
		if info.ContextWindow <= 0 {
			t.Errorf("model %q has non-positive context window %d", name, info.ContextWindow)
		}
		if info.Price.Input <= 0 || info.Price.Output <= 0 {
			t.Errorf("model %q has non-positive input/output price %+v", name, info.Price)
		}
	}
}
