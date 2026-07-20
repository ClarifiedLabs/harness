package modelcatalog

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
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
			p.Models[modelKey] = m
		}
		providers[key] = p
	}
	return &Catalog{Providers: providers}
}
