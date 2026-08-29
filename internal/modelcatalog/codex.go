package modelcatalog

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/llm"
)

const (
	// codexRepositoryRawURL is the raw-content root for the official OpenAI
	// Codex repository. Catalog refresh pins files to one stable release tag.
	codexRepositoryRawURL = "https://raw.githubusercontent.com/openai/codex"
	codexModelsPath       = "codex-rs/models-manager/models.json"
	// OpenAICodexProviderID identifies the synthetic ChatGPT subscription provider.
	OpenAICodexProviderID = "openai-codex"
	// OpenAICodexProviderName is the synthetic provider's display name.
	OpenAICodexProviderName = "OpenAI Codex (ChatGPT subscription)"
	// OpenAICodexProviderBaseURL is the ChatGPT Codex backend endpoint.
	OpenAICodexProviderBaseURL = "https://chatgpt.com/backend-api/codex"
)

//go:embed codex_fallback.json
var codexModelsFallbackJSON []byte

//go:embed codex_client_version.txt
var codexClientVersionText string

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

// CodexClientVersion returns the official stable Codex CLI version captured
// with the vendored Codex catalog by make refresh-model-catalogs. It is a
// provider protocol compatibility value, not the harness build version.
func CodexClientVersion() string {
	return strings.TrimSpace(codexClientVersionText)
}

// ValidateCodexClientVersion accepts the numeric major.minor.patch shape
// required by the authenticated ChatGPT Codex model endpoint.
func ValidateCodexClientVersion(version string) error {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid Codex client version %q: want major.minor.patch", version)
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return fmt.Errorf("invalid Codex client version %q: want major.minor.patch", version)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return fmt.Errorf("invalid Codex client version %q: want major.minor.patch", version)
			}
		}
	}
	return nil
}

// DecodeCodexReleaseVersion extracts a stable Codex CLI version from GitHub's
// latest-release response. Stable Codex release tags use rust-vX.Y.Z.
func DecodeCodexReleaseVersion(data []byte) (string, error) {
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return "", fmt.Errorf("decode OpenAI Codex release: %w", err)
	}
	version, ok := strings.CutPrefix(strings.TrimSpace(release.TagName), "rust-v")
	if !ok {
		return "", fmt.Errorf("OpenAI Codex release tag %q does not start with rust-v", release.TagName)
	}
	if err := ValidateCodexClientVersion(version); err != nil {
		return "", err
	}
	return version, nil
}

// CodexModelsURL returns the public model catalog pinned to the same official
// Codex release as CodexClientVersion.
func CodexModelsURL() string {
	return codexRepositoryRawURL + "/rust-v" + CodexClientVersion() + "/" + codexModelsPath
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
		tier := llm.ServiceTier{
			ID:          raw.ID,
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
		name := catalogModeName(additional)
		if additional == "fast" {
			name = "Fast"
		}
		tiers = append(tiers, llm.ServiceTier{
			ID:      additional,
			Name:    name,
			Request: llm.ServiceTierRequest{ServiceTier: additional},
		})
	}
	return NormalizeCodexFastServiceTiers(tiers)
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
