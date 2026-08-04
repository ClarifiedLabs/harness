package modelcatalog

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"harness/internal/llm"
)

// ModelsDevURL is the public models.dev catalog endpoint.
const ModelsDevURL = "https://models.dev/api.json"

//go:embed modelsdev_fallback.json
var modelsDevFallbackJSON []byte

// FetchModelsDev downloads and decodes a models.dev API catalog. A nil client
// uses the default HTTP client.
func FetchModelsDev(ctx context.Context, client *http.Client, url string) (*Catalog, error) {
	data, err := FetchModelsDevData(ctx, client, url)
	if err != nil {
		return nil, err
	}
	return DecodeModelsDev(bytes.NewReader(data))
}

// FetchModelsDevData downloads a models.dev API catalog and returns the raw JSON
// body. A nil client uses the default HTTP client.
func FetchModelsDevData(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	if url == "" {
		url = ModelsDevURL
	}
	return fetchCatalogData(ctx, client, url, "models.dev")
}

// DecodeModelsDev parses a models.dev API catalog.
func DecodeModelsDev(r io.Reader) (*Catalog, error) {
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

// EncodeModelsDev renders a catalog in a models.dev cache-compatible JSON shape.
func EncodeModelsDev(c *Catalog) ([]byte, error) {
	providers := map[string]Provider{}
	if c != nil && c.Providers != nil {
		providers = c.Providers
	}
	return json.MarshalIndent(struct {
		Providers map[string]Provider `json:"providers"`
	}{Providers: providers}, "", "  ")
}

// PruneModelsDevData renders a models.dev catalog with only the metadata used
// by harness. The result keeps the upstream provider-map shape expected by the
// vendored fallback snapshot.
func PruneModelsDevData(data []byte) ([]byte, error) {
	catalog, err := DecodeModelsDev(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	providers := make(map[string]modelsDevSnapshotProvider, len(catalog.Providers))
	for key, provider := range catalog.Providers {
		models := make(map[string]modelsDevSnapshotModel, len(provider.Models))
		for modelKey, model := range provider.Models {
			models[modelKey] = modelsDevSnapshotModel{
				ID:               model.ID,
				Name:             model.Name,
				ReleaseDate:      model.ReleaseDate,
				LastUpdated:      model.LastUpdated,
				Modalities:       modelsDevSnapshotModalities{Input: model.Modalities.Input},
				Reasoning:        model.Reasoning,
				ReasoningOptions: model.ReasoningOptions,
				Limit:            model.Limit,
				Provider:         model.Provider,
				Cost:             model.Cost,
				ServiceTiers:     llm.NormalizeServiceTiers(model.ServiceTiers),
			}
		}
		providers[key] = modelsDevSnapshotProvider{
			ID:     provider.ID,
			Name:   provider.Name,
			API:    provider.API,
			NPM:    provider.NPM,
			Env:    provider.Env,
			Models: models,
		}
	}
	return json.MarshalIndent(providers, "", "  ")
}

type modelsDevSnapshotProvider struct {
	ID     string                            `json:"id"`
	Name   string                            `json:"name,omitempty"`
	API    string                            `json:"api,omitempty"`
	NPM    string                            `json:"npm,omitempty"`
	Env    []string                          `json:"env,omitempty"`
	Models map[string]modelsDevSnapshotModel `json:"models"`
}

type modelsDevSnapshotModel struct {
	ID               string                      `json:"id"`
	Name             string                      `json:"name,omitempty"`
	ReleaseDate      string                      `json:"release_date,omitempty"`
	LastUpdated      string                      `json:"last_updated,omitempty"`
	Modalities       modelsDevSnapshotModalities `json:"modalities,omitzero"`
	Reasoning        bool                        `json:"reasoning,omitempty"`
	ReasoningOptions []llm.ReasoningOption       `json:"reasoning_options,omitempty"`
	Limit            Limit                       `json:"limit,omitzero"`
	Provider         ModelProvider               `json:"provider,omitzero"`
	Cost             llm.Price                   `json:"cost,omitzero"`
	ServiceTiers     []llm.ServiceTier           `json:"service_tiers,omitempty"`
}

type modelsDevSnapshotModalities struct {
	Input []string `json:"input,omitempty"`
}

// ModelsDevFallback decodes the vendored models.dev api.json snapshot.
func ModelsDevFallback() (*Catalog, error) {
	return DecodeModelsDev(bytes.NewReader(modelsDevFallbackJSON))
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
			m.ServiceTiers = modelsDevServiceTiers(p.ID, m)
			m.Experimental = modelExperimental{}
			p.Models[modelKey] = m
		}
		providers[key] = p
	}
	return &Catalog{Providers: providers}
}

func modelsDevServiceTiers(providerID string, model Model) []llm.ServiceTier {
	tiers := append([]llm.ServiceTier(nil), model.ServiceTiers...)
	modeIDs := make([]string, 0, len(model.Experimental.Modes))
	for id := range model.Experimental.Modes {
		modeIDs = append(modeIDs, id)
	}
	sort.Strings(modeIDs)
	for _, modeID := range modeIDs {
		mode := model.Experimental.Modes[modeID]
		request := llm.ServiceTierRequest{
			ServiceTier: mode.Provider.Body.ServiceTier,
			Speed:       mode.Provider.Body.Speed,
		}
		if request.ServiceTier == "" && request.Speed == "" {
			continue
		}
		if beta := mode.Provider.Headers["anthropic-beta"]; beta != "" {
			request.Betas = splitCatalogBetas(beta)
		}
		tiers = append(tiers, llm.ServiceTier{
			ID:      modeID,
			Name:    catalogModeName(modeID),
			Request: request,
			Price:   mode.Cost,
		})
	}
	if strings.EqualFold(strings.TrimSpace(providerID), "openai") {
		return NormalizeOpenAIFastServiceTiers(tiers)
	}
	return llm.NormalizeServiceTiers(tiers)
}

func catalogModeName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

func splitCatalogBetas(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' '
	})
}
