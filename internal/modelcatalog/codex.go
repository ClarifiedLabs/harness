package modelcatalog

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"harness/internal/llm"
)

const (
	// CodexModelsURL is OpenAI Codex's model catalog endpoint.
	CodexModelsURL = "https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json"
	// OpenAICodexProviderID identifies the synthetic ChatGPT subscription provider.
	OpenAICodexProviderID = "openai-codex"
	// OpenAICodexProviderName is the synthetic provider's display name.
	OpenAICodexProviderName = "OpenAI Codex (ChatGPT subscription)"
	// OpenAICodexProviderBaseURL is the ChatGPT Codex backend endpoint.
	OpenAICodexProviderBaseURL = "https://chatgpt.com/backend-api/codex"
)

//go:embed codex_fallback.json
var codexModelsFallbackJSON []byte

type codexModelsCatalog struct {
	Models []codexModel `json:"models"`
}

type codexModel struct {
	Slug                              string                 `json:"slug"`
	DisplayName                       string                 `json:"display_name,omitempty"`
	ContextWindow                     int                    `json:"context_window,omitempty"`
	MaxContextWindow                  int                    `json:"max_context_window,omitempty"`
	InputModalities                   []string               `json:"input_modalities,omitempty"`
	SupportedReasoningLevels          []codexReasoningPreset `json:"supported_reasoning_levels,omitempty"`
	SupportsReasoningSummaryParameter *bool                  `json:"supports_reasoning_summary_parameter,omitempty"`
	SupportsReasoningSummaries        *bool                  `json:"supports_reasoning_summaries,omitempty"`
	Visibility                        string                 `json:"visibility,omitempty"`
	SupportedInAPI                    *bool                  `json:"supported_in_api,omitempty"`
	ServiceTiers                      []codexServiceTier     `json:"service_tiers,omitempty"`
	DefaultServiceTier                *string                `json:"default_service_tier"`
	AdditionalSpeedTiers              []string               `json:"additional_speed_tiers,omitempty"`
}

type codexReasoningPreset struct {
	Effort string `json:"effort,omitempty"`
}

type codexServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// FetchCodexModelsData downloads the OpenAI Codex model catalog and returns its
// raw JSON body. A nil client uses the default HTTP client.
func FetchCodexModelsData(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	if url == "" {
		url = CodexModelsURL
	}
	return fetchCatalogData(ctx, client, url, "OpenAI Codex model catalog")
}

// DecodeCodexModels parses an OpenAI Codex model catalog and converts its
// list-visible API models into the synthetic openai-codex provider.
func DecodeCodexModels(data []byte) (Provider, error) {
	var catalog codexModelsCatalog
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&catalog); err != nil {
		return Provider{}, err
	}
	provider, ok := codexProviderFromCatalog(&catalog)
	if !ok {
		return Provider{}, fmt.Errorf("OpenAI Codex model catalog has no list-visible models")
	}
	return provider, nil
}

// PruneCodexModelsData renders an OpenAI Codex model catalog with only the
// fields consumed by DecodeCodexModels.
func PruneCodexModelsData(data []byte) ([]byte, error) {
	var catalog codexModelsCatalog
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&catalog); err != nil {
		return nil, err
	}
	return json.MarshalIndent(catalog, "", "  ")
}

// CodexModelsFallback decodes the vendored OpenAI Codex model snapshot.
func CodexModelsFallback() (Provider, error) {
	return DecodeCodexModels(codexModelsFallbackJSON)
}

func codexProviderFromCatalog(catalog *codexModelsCatalog) (Provider, bool) {
	if catalog == nil {
		return Provider{}, false
	}
	models := make(map[string]Model)
	for _, model := range catalog.Models {
		entry, ok := codexModelToCatalog(model)
		if !ok {
			continue
		}
		models[entry.ID] = entry
	}
	if len(models) == 0 {
		return Provider{}, false
	}
	return Provider{
		ID:     OpenAICodexProviderID,
		Name:   OpenAICodexProviderName,
		API:    OpenAICodexProviderBaseURL,
		Models: models,
	}, true
}

func codexModelToCatalog(model codexModel) (Model, bool) {
	id := strings.TrimSpace(model.Slug)
	if id == "" || !codexModelVisible(model) {
		return Model{}, false
	}
	contextWindow := model.ContextWindow
	if contextWindow <= 0 {
		contextWindow = model.MaxContextWindow
	}
	if contextWindow <= 0 {
		return Model{}, false
	}
	name := strings.TrimSpace(model.DisplayName)
	if name == "" {
		name = id
	}
	reasoningValues := codexReasoningValues(model.SupportedReasoningLevels)
	summarySupported := true
	if model.SupportsReasoningSummaryParameter != nil {
		summarySupported = *model.SupportsReasoningSummaryParameter
	} else if model.SupportsReasoningSummaries != nil {
		summarySupported = *model.SupportsReasoningSummaries
	}
	entry := Model{
		ID:                        id,
		Name:                      name,
		Modalities:                Modalities{Input: append([]string(nil), model.InputModalities...)},
		Reasoning:                 len(reasoningValues) > 0,
		ReasoningSummarySupported: &summarySupported,
		Limit:                     Limit{Context: contextWindow},
		ServiceTiers:              codexServiceTiers(model),
	}
	if len(reasoningValues) > 0 {
		entry.ReasoningOptions = []llm.ReasoningOption{{Type: "effort", Values: reasoningValues}}
	}
	return entry, true
}

func codexServiceTiers(model codexModel) []llm.ServiceTier {
	tiers := make([]llm.ServiceTier, 0, len(model.ServiceTiers)+len(model.AdditionalSpeedTiers))
	for _, raw := range model.ServiceTiers {
		id := raw.ID
		if strings.EqualFold(strings.TrimSpace(raw.Name), "fast") {
			id = "fast"
		}
		tier := llm.ServiceTier{
			ID:          id,
			Name:        raw.Name,
			Description: raw.Description,
			Request:     llm.ServiceTierRequest{ServiceTier: raw.ID},
		}
		tiers = append(tiers, tier)
	}
	for _, additional := range model.AdditionalSpeedTiers {
		additional = strings.ToLower(strings.TrimSpace(additional))
		if additional == "" {
			continue
		}
		id := additional
		requestValue := additional
		name := catalogModeName(additional)
		if additional == "fast" {
			requestValue = "priority"
			name = "Fast"
		}
		tiers = append(tiers, llm.ServiceTier{
			ID:      id,
			Name:    name,
			Request: llm.ServiceTierRequest{ServiceTier: requestValue},
		})
	}
	return llm.NormalizeServiceTiers(tiers)
}

func codexModelVisible(model codexModel) bool {
	if model.SupportedInAPI != nil && !*model.SupportedInAPI {
		return false
	}
	visibility := strings.ToLower(strings.TrimSpace(model.Visibility))
	return visibility == "" || visibility == "list"
}

func codexReasoningValues(presets []codexReasoningPreset) []string {
	values := make([]string, 0, len(presets))
	for _, preset := range presets {
		effort := strings.TrimSpace(preset.Effort)
		if effort != "" {
			values = append(values, effort)
		}
	}
	return values
}
