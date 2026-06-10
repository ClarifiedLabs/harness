// Package modelsdev reads the public models.dev catalog and reduces it to the
// provider, endpoint, model context, and pricing fields harness needs.
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"harness/internal/llm"
)

const DefaultURL = "https://models.dev/api.json"

// Catalog is the provider-keyed models.dev API response.
type Catalog struct {
	Providers map[string]Provider
}

// Provider is one provider entry in the models.dev API response.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	API    string           `json:"api"`
	Doc    string           `json:"doc"`
	NPM    string           `json:"npm"`
	Env    []string         `json:"env"`
	Models map[string]Model `json:"models"`
}

// Model is the subset of one models.dev model entry used by harness.
type Model struct {
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	ReleaseDate      string                `json:"release_date"`
	LastUpdated      string                `json:"last_updated"`
	Reasoning        bool                  `json:"reasoning"`
	ReasoningOptions []llm.ReasoningOption `json:"reasoning_options"`
	Limit            Limit                 `json:"limit"`
	Cost             llm.Price             `json:"cost"`
}

// Limit carries token limits from models.dev.
type Limit struct {
	Context int `json:"context"`
}

// Fetch downloads and decodes a models.dev API catalog. A nil client uses the
// default HTTP client.
func Fetch(ctx context.Context, client *http.Client, url string) (*Catalog, error) {
	if url == "" {
		url = DefaultURL
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev: GET %s: %s", url, resp.Status)
	}
	return Decode(resp.Body)
}

// Decode parses a models.dev API catalog.
func Decode(r io.Reader) (*Catalog, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, err
	}

	var wrapper struct {
		Providers map[string]Provider `json:"providers"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Providers != nil {
		return normalizeProviders(wrapper.Providers), nil
	}

	var providers map[string]Provider
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, err
	}
	return normalizeProviders(providers), nil
}

func normalizeProviders(providers map[string]Provider) *Catalog {
	if providers == nil {
		providers = map[string]Provider{}
	}
	for key, p := range providers {
		if p.ID == "" {
			p.ID = key
		}
		if p.Models == nil {
			p.Models = map[string]Model{}
		}
		for modelKey, m := range p.Models {
			if m.ID == "" {
				m.ID = modelKey
			}
			p.Models[modelKey] = m
		}
		providers[key] = p
	}
	return &Catalog{Providers: providers}
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

// ProviderByAPI returns the provider whose models.dev api field matches baseURL
// after trimming trailing slashes.
func (c *Catalog) ProviderByAPI(baseURL string) (Provider, bool) {
	if c == nil {
		return Provider{}, false
	}
	baseURL = normalizeURL(baseURL)
	if baseURL == "" {
		return Provider{}, false
	}
	for _, p := range c.Providers {
		if normalizeURL(p.API) == baseURL {
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

// BaseURL returns the provider API base URL known to models.dev, falling back to
// harness's built-in defaults for first-party providers.
func (p Provider) BaseURL() string {
	if p.API != "" {
		return p.API
	}
	switch p.ID {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com"
	default:
		return ""
	}
}

// APIType returns the harness dialect to use for this provider when it is known.
func (p Provider) APIType() string {
	if p.ID == "anthropic" || strings.Contains(strings.ToLower(p.NPM), "anthropic") || slices.Contains(p.Env, "ANTHROPIC_API_KEY") {
		return "anthropic"
	}
	if p.ID == "openai" {
		return "responses"
	}
	if p.API != "" || strings.Contains(strings.ToLower(p.NPM), "openai") {
		return "openai"
	}
	return ""
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

// ProviderConfig converts this models.dev provider entry into a harness
// provider config. The supplied apiKey is the only user-specific field; all
// other connection and model metadata comes from models.dev plus harness's
// first-party URL defaults.
func (p Provider) ProviderConfig(apiKey string) llm.ProviderConfig {
	models := p.ModelsByID()
	entries := make([]llm.ModelEntry, 0, len(models))
	for _, m := range models {
		entry := llm.ModelEntry{
			Name:             m.ID,
			ContextWindow:    m.Limit.Context,
			Price:            m.Cost,
			ReasoningOptions: append([]llm.ReasoningOption(nil), m.ReasoningOptions...),
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

// ModelInfo converts one models.dev model into a harness registry entry.
func (m Model) ModelInfo() llm.ModelInfo {
	return llm.ModelInfo{
		ContextWindow: m.Limit.Context,
		Price:         m.Cost,
		Reasoning: &llm.ReasoningInfo{
			Supported: m.Reasoning,
			Options:   append([]llm.ReasoningOption(nil), m.ReasoningOptions...),
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
