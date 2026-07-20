package modelcatalog

import (
	"os"
	"slices"
	"testing"
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
