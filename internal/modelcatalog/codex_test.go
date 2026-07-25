package modelcatalog

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"harness/internal/llm"
)

func TestDecodeCodexModelsUsesListVisibleModels(t *testing.T) {
	provider, err := DecodeCodexModels([]byte(testCodexModelsCatalogJSON()))
	if err != nil {
		t.Fatalf("DecodeCodexModels: %v", err)
	}
	if provider.ID != OpenAICodexProviderID || provider.Name != OpenAICodexProviderName || provider.API != OpenAICodexProviderBaseURL {
		t.Fatalf("provider identity = %+v", provider)
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
	if model.ReasoningSummarySupported == nil || !*model.ReasoningSummarySupported {
		t.Fatalf("gpt-5.5 reasoning summary support = %v, want true", model.ReasoningSummarySupported)
	}
	fast, ok := llm.ResolveServiceTier("fast", model.ServiceTiers)
	if !ok || fast.ID != "fast" || fast.Request.ServiceTier != "priority" {
		t.Fatalf("gpt-5.5 fast tier = %+v, %v", fast, ok)
	}
	if _, ok := provider.Models["codex-auto-review"]; ok {
		t.Fatalf("hidden codex-auto-review should not be exposed: %+v", provider.Models)
	}
	if _, ok := provider.Models["unsupported"]; ok {
		t.Fatalf("unsupported model should not be exposed: %+v", provider.Models)
	}
}

func TestCodexFallbackSnapshotDecodes(t *testing.T) {
	if _, err := DecodeCodexModels(codexModelsFallbackJSON); err != nil {
		t.Fatalf("DecodeCodexModels fallback: %v", err)
	}
}

func TestPruneCodexModelsDataPreservesReasoningSummarySupport(t *testing.T) {
	data, err := PruneCodexModelsData([]byte(testCodexModelsCatalogJSON()))
	if err != nil {
		t.Fatalf("PruneCodexModelsData: %v", err)
	}
	var catalog codexModelsCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("decode pruned catalog: %v", err)
	}
	if len(catalog.Models) == 0 ||
		catalog.Models[0].SupportsReasoningSummaries == nil ||
		!*catalog.Models[0].SupportsReasoningSummaries {
		t.Fatalf("pruned reasoning summary support = %+v, want true", catalog.Models)
	}
}

func TestCodexModelUsesRequestBuilderReasoningSummaryCapability(t *testing.T) {
	supported, legacySupported := false, true
	model, ok := codexModelToCatalog(codexModel{
		Slug:                              "gpt-test",
		ContextWindow:                     128_000,
		SupportedReasoningLevels:          []codexReasoningPreset{{Effort: "medium"}},
		SupportsReasoningSummaryParameter: &supported,
		SupportsReasoningSummaries:        &legacySupported,
		Visibility:                        "list",
	})
	if !ok {
		t.Fatal("codexModelToCatalog rejected visible model")
	}
	if model.ReasoningSummarySupported == nil || *model.ReasoningSummarySupported {
		t.Fatalf("reasoning summary support = %v, want false", model.ReasoningSummarySupported)
	}
}

func TestCodexFallbackCandidateDecodes(t *testing.T) {
	path := os.Getenv("CODEX_MODELS_FALLBACK_CANDIDATE")
	if path == "" {
		t.Skip("CODEX_MODELS_FALLBACK_CANDIDATE is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCodexModels(data); err != nil {
		t.Fatalf("DecodeCodexModels candidate: %v", err)
	}
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
      "supports_reasoning_summaries": true,
      "visibility": "list",
      "supported_in_api": true,
      "service_tiers": [{"id":"priority","name":"Fast","description":"Lower latency"}],
      "default_service_tier": null
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
