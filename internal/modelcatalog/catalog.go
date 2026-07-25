// Package modelcatalog defines the provider and model metadata harness uses and
// adapts upstream model catalogs into that shared representation.
package modelcatalog

import (
	"slices"
	"sort"
	"strings"

	"harness/internal/llm"
)

// Catalog is a provider-keyed collection of model metadata.
type Catalog struct {
	Providers map[string]Provider
}

// Provider describes one model provider and its available models.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	API    string           `json:"api"`
	Doc    string           `json:"doc"`
	NPM    string           `json:"npm"`
	Env    []string         `json:"env"`
	Models map[string]Model `json:"models"`
}

// Model is the provider-local model metadata used by harness.
type Model struct {
	ID                        string                `json:"id"`
	Name                      string                `json:"name"`
	ReleaseDate               string                `json:"release_date"`
	LastUpdated               string                `json:"last_updated"`
	Modalities                Modalities            `json:"modalities"`
	Reasoning                 bool                  `json:"reasoning"`
	ReasoningSummarySupported *bool                 `json:"reasoning_summary_supported,omitempty"`
	ReasoningOptions          []llm.ReasoningOption `json:"reasoning_options"`
	Limit                     Limit                 `json:"limit"`
	Provider                  ModelProvider         `json:"provider"`
	Cost                      llm.Price             `json:"cost"`
	ServiceTiers              []llm.ServiceTier     `json:"service_tiers,omitempty"`
	Experimental              modelExperimental     `json:"experimental,omitzero"`
}

type modelExperimental struct {
	Modes map[string]modelMode `json:"modes,omitempty"`
}

type modelMode struct {
	Cost     llm.Price         `json:"cost,omitzero"`
	Provider modelModeProvider `json:"provider,omitzero"`
}

type modelModeProvider struct {
	Body    modelModeBody     `json:"body,omitzero"`
	Headers map[string]string `json:"headers,omitempty"`
}

type modelModeBody struct {
	ServiceTier string `json:"service_tier,omitempty"`
	Speed       string `json:"speed,omitempty"`
}

// ModelProvider captures provider-specific wire-shape hints attached to a model.
type ModelProvider struct {
	Shape string `json:"shape"`
}

// Modalities carries supported input and output modalities.
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Limit carries token limits.
type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// Provider returns the provider with id, matching case-insensitively against the
// provider key and the provider's id field.
func (c *Catalog) Provider(id string) (Provider, bool) {
	if c == nil {
		return Provider{}, false
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return Provider{}, false
	}
	if p, ok := c.Providers[id]; ok {
		return p, true
	}
	for key, p := range c.Providers {
		if strings.ToLower(key) == id || strings.ToLower(p.ID) == id {
			return p, true
		}
	}
	return Provider{}, false
}

// ProvidersList returns provider entries sorted by id.
func (c *Catalog) ProvidersList() []Provider {
	if c == nil {
		return nil
	}
	providers := make([]Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		providers = append(providers, p)
	}
	sortProviders(providers)
	return providers
}

const (
	npmAnthropic = "@ai-sdk/anthropic"
	npmGoogle    = "@ai-sdk/google"
	npmOpenAI    = "@ai-sdk/openai"
)

// BaseURL returns the provider API base URL known to models.dev, falling back to
// harness's built-in defaults for first-party providers and exact SDK package
// matches whose HTTP wire format is known.
func (p Provider) BaseURL() string {
	npm := strings.ToLower(strings.TrimSpace(p.NPM))
	// Managed Google uses the native Interactions endpoint even if models.dev
	// still publishes an SDK-oriented or OpenAI-compatibility API URL.
	if p.ID == "google" || npm == npmGoogle {
		return "https://generativelanguage.googleapis.com/v1beta"
	}
	if p.API != "" {
		return p.API
	}
	switch {
	case p.ID == "openai" || npm == npmOpenAI:
		return "https://api.openai.com/v1"
	case p.ID == "anthropic" || npm == npmAnthropic:
		return "https://api.anthropic.com"
	default:
		return ""
	}
}

// APIType returns the harness dialect to use for this provider when it is known.
// It first honors well-known first-party providers, then the per-model
// provider.shape field (responses/completions), then OpenAI-compatible heuristics.
func (p Provider) APIType() string {
	npm := strings.ToLower(strings.TrimSpace(p.NPM))
	if p.ID == "anthropic" || strings.Contains(npm, "anthropic") || slices.Contains(p.Env, "ANTHROPIC_API_KEY") {
		return "anthropic"
	}
	if p.ID == "openai" {
		return "responses"
	}
	if p.ID == "google" || npm == npmGoogle {
		return "interactions"
	}
	switch p.dominantShape() {
	case "responses":
		return "responses"
	case "completions":
		return "openai"
	}
	if p.API != "" || strings.Contains(npm, "openai") {
		return "openai"
	}
	return ""
}

// dominantShape returns the provider.shape value shared by every model that has
// one, or "" when models disagree or none specify a shape.
func (p Provider) dominantShape() string {
	var shape string
	for _, m := range p.Models {
		s := strings.ToLower(strings.TrimSpace(m.Provider.Shape))
		if s == "" {
			continue
		}
		if shape == "" {
			shape = s
			continue
		}
		if shape != s {
			return ""
		}
	}
	return shape
}

// ModelsByID returns model entries sorted by provider-local model id.
func (p Provider) ModelsByID() []Model {
	models := make([]Model, 0, len(p.Models))
	for _, m := range p.Models {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

// ModelsByReleaseDate returns model entries sorted newest first, falling back
// to last_updated and then id for stable ordering.
func (p Provider) ModelsByReleaseDate() []Model {
	models := p.ModelsByID()
	sort.SliceStable(models, func(i, j int) bool {
		di := modelSortDate(models[i])
		dj := modelSortDate(models[j])
		if di != dj {
			return di > dj
		}
		return models[i].ID < models[j].ID
	})
	return models
}

// ModelInfo returns harness registry metadata for modelID.
func (p Provider) ModelInfo(modelID string) (llm.ModelInfo, bool) {
	if p.Models == nil {
		return llm.ModelInfo{}, false
	}
	if m, ok := p.Models[modelID]; ok {
		return m.ModelInfo(), true
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m.ModelInfo(), true
		}
	}
	return llm.ModelInfo{}, false
}

// ProviderConfig converts this catalog provider into a harness provider config.
// The supplied apiKey is the only user-specific field.
func (p Provider) ProviderConfig(apiKey string) llm.ProviderConfig {
	models := p.ModelsByID()
	entries := make([]llm.ModelEntry, 0, len(models))
	for _, m := range models {
		entry := llm.ModelEntry{
			Name:                      m.ID,
			ContextWindow:             m.Limit.Context,
			OutputLimit:               m.Limit.Output,
			InputModalities:           append([]string(nil), m.Modalities.Input...),
			Price:                     m.Cost,
			Shape:                     m.Provider.Shape,
			ReasoningSummarySupported: m.ReasoningSummarySupported,
			ReasoningOptions:          append([]llm.ReasoningOption(nil), m.ReasoningOptions...),
			ServiceTiers:              llm.NormalizeServiceTiers(m.ServiceTiers),
		}
		reasoning := m.Reasoning
		entry.Reasoning = &reasoning
		entries = append(entries, entry)
	}
	return llm.ProviderConfig{
		Name:      p.ID,
		APIType:   p.APIType(),
		BaseURL:   p.BaseURL(),
		APIKey:    apiKey,
		APIKeyEnv: append([]string(nil), p.Env...),
		Models:    entries,
	}
}

// ModelInfo converts one catalog model into a harness registry entry.
func (m Model) ModelInfo() llm.ModelInfo {
	return llm.ModelInfo{
		ContextWindow:   m.Limit.Context,
		OutputLimit:     m.Limit.Output,
		InputModalities: append([]string(nil), m.Modalities.Input...),
		ServiceTiers:    llm.NormalizeServiceTiers(m.ServiceTiers),
		Price:           m.Cost,
		Shape:           m.Provider.Shape,
		Reasoning: &llm.ReasoningInfo{
			Supported:        m.Reasoning,
			SummarySupported: m.ReasoningSummarySupported,
			Options:          append([]llm.ReasoningOption(nil), m.ReasoningOptions...),
		},
	}
}

func modelSortDate(m Model) string {
	if m.ReleaseDate != "" {
		return m.ReleaseDate
	}
	return m.LastUpdated
}

func sortProviders(providers []Provider) {
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].ID < providers[j].ID
	})
}

func normalizeURL(s string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(s)), "/")
}
